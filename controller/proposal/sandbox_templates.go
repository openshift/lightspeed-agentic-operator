package proposal

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
)

var sandboxTemplateGVK = schema.GroupVersionKind{
	Group: "extensions.agents.x-k8s.io", Version: "v1alpha1", Kind: "SandboxTemplate",
}

const (
	agentModeEnvVar = "LIGHTSPEED_MODE"

	vertexCredsMountPath = "/var/secrets/google"
	vertexCredsFileName  = "credentials.json"
	llmCredsVolumeName   = "llm-credentials"
	mcpHeadersMountRoot  = "/var/secrets/mcp"
	mcpServersEnvVar     = "LIGHTSPEED_MCP_SERVERS"

	LabelManaged      = "agentic.openshift.io/managed"
	LabelBaseTemplate = "agentic.openshift.io/base-template"
	LabelStep        = "agentic.openshift.io/step"
	LabelAgent        = "agentic.openshift.io/agent"
	LabelProposal     = "agentic.openshift.io/proposal"
	LabelComponent    = "agentic.openshift.io/component"
)

type templateHashInput struct {
	LLM                    agenticv1alpha1.LLMProviderSpec     `json:"llm"`
	Model                  string                              `json:"model"`
	Skills                 []agenticv1alpha1.SkillsSource      `json:"skills"`
	MCPServers             []agenticv1alpha1.MCPServerConfig   `json:"mcpServers,omitempty"`
	RequiredSecrets        []agenticv1alpha1.SecretRequirement `json:"requiredSecrets,omitempty"`
	Step                   string                              `json:"step"`
	BaseResourceVersion    string                              `json:"baseRV"`
}

func computeTemplateHash(
	llm *agenticv1alpha1.LLMProvider,
	model string,
	skills []agenticv1alpha1.SkillsSource,
	mcpServers []agenticv1alpha1.MCPServerConfig,
	requiredSecrets []agenticv1alpha1.SecretRequirement,
	step string,
	baseResourceVersion string,
) (string, error) {
	input := templateHashInput{
		LLM:                 llm.Spec,
		Model:               model,
		Skills:              skills,
		MCPServers:          mcpServers,
		RequiredSecrets:     requiredSecrets,
		Step: step,
		BaseResourceVersion: baseResourceVersion,
	}
	data, err := json.Marshal(input)
	if err != nil {
		return "", fmt.Errorf("marshal template hash input: %w", err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])[:10], nil
}

func agentTemplateName(step, agentName, hash string) string {
	return truncateK8sName(fmt.Sprintf("ls-%s-%s-%s", step, agentName, hash))
}

// EnsureAgentTemplate creates a SandboxTemplate derived from the base template
// with skills, LLM credentials, MCP servers, and required secrets from the CRD chain.
// Template name includes a config hash — same input = same template = no-op.
// Old templates for the same agent+phase are garbage-collected.
func EnsureAgentTemplate(
	ctx context.Context,
	c client.Client,
	baseTemplateName string,
	namespace string,
	step string,
	agent *agenticv1alpha1.Agent,
	llm *agenticv1alpha1.LLMProvider,
	tools *agenticv1alpha1.ToolsSpec,
) (string, error) {
	log := logf.FromContext(ctx).WithName("sandbox-templates")

	if agent == nil {
		return "", fmt.Errorf("agent is required for template generation")
	}
	if llm == nil {
		return "", fmt.Errorf("LLMProvider is required for template generation")
	}

	base := &unstructured.Unstructured{}
	base.SetGroupVersionKind(sandboxTemplateGVK)
	if err := c.Get(ctx, types.NamespacedName{Name: baseTemplateName, Namespace: namespace}, base); err != nil {
		return "", fmt.Errorf("failed to read base sandbox template %q: %w", baseTemplateName, err)
	}

	var skills []agenticv1alpha1.SkillsSource
	var mcpServers []agenticv1alpha1.MCPServerConfig
	var requiredSecrets []agenticv1alpha1.SecretRequirement
	if tools != nil {
		skills = tools.Skills
		mcpServers = tools.MCPServers
		requiredSecrets = tools.RequiredSecrets
	}

	hash, err := computeTemplateHash(llm, agent.Spec.Model, skills, mcpServers, requiredSecrets, step, base.GetResourceVersion())
	if err != nil {
		return "", fmt.Errorf("compute template hash: %w", err)
	}
	name := agentTemplateName(step, agent.Name, hash)

	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(sandboxTemplateGVK)
	err = c.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, existing)
	if err == nil {
		return name, nil
	}
	if !apierrors.IsNotFound(err) {
		return "", fmt.Errorf("failed to check template %q: %w", name, err)
	}

	derived := base.DeepCopy()
	derived.SetName(name)
	derived.SetResourceVersion("")
	derived.SetUID("")
	derived.SetGeneration(0)
	derived.SetCreationTimestamp(metav1.Time{})

	annotations := derived.GetAnnotations()
	if annotations != nil {
		delete(annotations, "kubectl.kubernetes.io/last-applied-configuration")
		derived.SetAnnotations(annotations)
	}

	lbls := derived.GetLabels()
	if lbls == nil {
		lbls = map[string]string{}
	}
	lbls[LabelManaged] = "true"
	lbls[LabelBaseTemplate] = baseTemplateName
	lbls[LabelStep] = step
	lbls[LabelAgent] = agent.Name
	derived.SetLabels(lbls)

	if len(skills) > 0 && skills[0].Image != "" {
		if err := patchSkillsImage(derived, skills[0].Image); err != nil {
			return "", fmt.Errorf("patch skills image: %w", err)
		}
		if len(skills[0].Paths) > 0 {
			if err := patchSkillsPaths(derived, skills[0].Paths); err != nil {
				return "", fmt.Errorf("patch skills paths: %w", err)
			}
		}
	}

	if err := patchAgentMode(derived, step); err != nil {
		return "", fmt.Errorf("patch agent mode: %w", err)
	}
	if err := patchLLMCredentials(derived, llm, agent.Spec.Model); err != nil {
		return "", fmt.Errorf("patch LLM credentials: %w", err)
	}

	if len(mcpServers) > 0 {
		if err := patchMCPServers(derived, mcpServers); err != nil {
			return "", fmt.Errorf("patch MCP servers: %w", err)
		}
	}

	if len(requiredSecrets) > 0 {
		if err := patchRequiredSecrets(derived, requiredSecrets); err != nil {
			return "", fmt.Errorf("patch required secrets: %w", err)
		}
	}

	if err := c.Create(ctx, derived); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return name, nil
		}
		return "", fmt.Errorf("failed to create template %q: %w", name, err)
	}

	log.Info("Created agent SandboxTemplate",
		"name", name,
		"base", baseTemplateName,
		"step", step,
		"agent", agent.Name,
		"llmProvider", llm.Name,
		"hash", hash)

	if err := gcOldTemplates(ctx, c, namespace, agent.Name, step, name); err != nil {
		log.Error(err, "failed to garbage-collect old templates")
	}

	return name, nil
}

func credentialsSecretName(llm *agenticv1alpha1.LLMProvider) string {
	switch llm.Spec.Type {
	case agenticv1alpha1.LLMProviderAnthropic:
		return llm.Spec.Anthropic.CredentialsSecret.Name
	case agenticv1alpha1.LLMProviderGoogleCloudVertex:
		return llm.Spec.GoogleCloudVertex.CredentialsSecret.Name
	case agenticv1alpha1.LLMProviderOpenAI:
		return llm.Spec.OpenAI.CredentialsSecret.Name
	case agenticv1alpha1.LLMProviderAzureOpenAI:
		return llm.Spec.AzureOpenAI.CredentialsSecret.Name
	case agenticv1alpha1.LLMProviderAWSBedrock:
		return llm.Spec.AWSBedrock.CredentialsSecret.Name
	default:
		return ""
	}
}

func providerURL(llm *agenticv1alpha1.LLMProvider) string {
	switch llm.Spec.Type {
	case agenticv1alpha1.LLMProviderAnthropic:
		return llm.Spec.Anthropic.URL
	case agenticv1alpha1.LLMProviderGoogleCloudVertex:
		return llm.Spec.GoogleCloudVertex.URL
	case agenticv1alpha1.LLMProviderOpenAI:
		return llm.Spec.OpenAI.URL
	case agenticv1alpha1.LLMProviderAzureOpenAI:
		return llm.Spec.AzureOpenAI.URL
	case agenticv1alpha1.LLMProviderAWSBedrock:
		return llm.Spec.AWSBedrock.URL
	default:
		return ""
	}
}

func patchLLMCredentials(tmpl *unstructured.Unstructured, llm *agenticv1alpha1.LLMProvider, model string) error {
	secretName := credentialsSecretName(llm)

	if err := addEnvFromSecret(tmpl, secretName); err != nil {
		return fmt.Errorf("add credentials envFrom: %w", err)
	}
	if err := setEnvVar(tmpl, "ANTHROPIC_MODEL", model); err != nil {
		return fmt.Errorf("set ANTHROPIC_MODEL: %w", err)
	}

	if u := providerURL(llm); u != "" {
		if err := setEnvVar(tmpl, providerURLEnvVar(llm.Spec.Type), u); err != nil {
			return fmt.Errorf("set provider URL: %w", err)
		}
	}

	switch llm.Spec.Type {
	case agenticv1alpha1.LLMProviderGoogleCloudVertex:
		cfg := llm.Spec.GoogleCloudVertex
		if err := setEnvVar(tmpl, "CLAUDE_CODE_USE_VERTEX", "1"); err != nil {
			return fmt.Errorf("set CLAUDE_CODE_USE_VERTEX: %w", err)
		}
		if err := setEnvVar(tmpl, "GCP_PROJECT", cfg.ProjectID); err != nil {
			return fmt.Errorf("set GCP_PROJECT: %w", err)
		}
		if err := setEnvVar(tmpl, "GCP_REGION", cfg.Region); err != nil {
			return fmt.Errorf("set GCP_REGION: %w", err)
		}
		if err := setEnvVar(tmpl, "GOOGLE_APPLICATION_CREDENTIALS", vertexCredsMountPath+"/"+vertexCredsFileName); err != nil {
			return fmt.Errorf("set GOOGLE_APPLICATION_CREDENTIALS: %w", err)
		}
		if err := addSecretVolume(tmpl, llmCredsVolumeName, secretName); err != nil {
			return fmt.Errorf("add Vertex credentials volume: %w", err)
		}
		if err := addVolumeMount(tmpl, llmCredsVolumeName, vertexCredsMountPath, true); err != nil {
			return fmt.Errorf("mount Vertex credentials: %w", err)
		}
	case agenticv1alpha1.LLMProviderAzureOpenAI:
		cfg := llm.Spec.AzureOpenAI
		if err := setEnvVar(tmpl, "AZURE_OPENAI_ENDPOINT", cfg.Endpoint); err != nil {
			return fmt.Errorf("set AZURE_OPENAI_ENDPOINT: %w", err)
		}
		if cfg.APIVersion != "" {
			if err := setEnvVar(tmpl, "AZURE_OPENAI_API_VERSION", cfg.APIVersion); err != nil {
				return fmt.Errorf("set AZURE_OPENAI_API_VERSION: %w", err)
			}
		}
	case agenticv1alpha1.LLMProviderAWSBedrock:
		cfg := llm.Spec.AWSBedrock
		if err := setEnvVar(tmpl, "CLAUDE_CODE_USE_BEDROCK", "1"); err != nil {
			return fmt.Errorf("set CLAUDE_CODE_USE_BEDROCK: %w", err)
		}
		if err := setEnvVar(tmpl, "AWS_REGION", cfg.Region); err != nil {
			return fmt.Errorf("set AWS_REGION: %w", err)
		}
	}
	return nil
}

func providerURLEnvVar(t agenticv1alpha1.LLMProviderType) string {
	switch t {
	case agenticv1alpha1.LLMProviderOpenAI:
		return "OPENAI_BASE_URL"
	case agenticv1alpha1.LLMProviderAzureOpenAI:
		return "AZURE_OPENAI_ENDPOINT"
	default:
		return "ANTHROPIC_BASE_URL"
	}
}

func patchRequiredSecrets(tmpl *unstructured.Unstructured, secrets []agenticv1alpha1.SecretRequirement) error {
	for _, s := range secrets {
		switch s.MountAs.Type {
		case agenticv1alpha1.SecretMountFilePath:
			volName := "req-" + s.Name
			if err := addSecretVolume(tmpl, volName, s.Name); err != nil {
				return fmt.Errorf("add secret volume %q: %w", s.Name, err)
			}
			if err := addVolumeMount(tmpl, volName, s.MountAs.FilePath.Path, true); err != nil {
				return fmt.Errorf("add volume mount %q: %w", s.MountAs.FilePath.Path, err)
			}
		case agenticv1alpha1.SecretMountEnvVar:
			if err := addEnvVarFromSecret(tmpl, s.MountAs.EnvVar.Name, s.Name, "token"); err != nil {
				return fmt.Errorf("add env var from secret %q: %w", s.Name, err)
			}
		}
	}
	return nil
}

func gcOldTemplates(
	ctx context.Context,
	c client.Client,
	namespace string,
	agentName string,
	step string,
	currentName string,
) error {
	sel := labels.SelectorFromSet(labels.Set{
		LabelManaged: "true",
		LabelAgent:   agentName,
		LabelStep: step,
	})

	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(sandboxTemplateGVK)
	if err := c.List(ctx, list, &client.ListOptions{
		Namespace:     namespace,
		LabelSelector: sel,
	}); err != nil {
		return fmt.Errorf("failed to list old templates: %w", err)
	}

	log := logf.FromContext(ctx).WithName("sandbox-templates")
	for i := range list.Items {
		item := &list.Items[i]
		if item.GetName() == currentName {
			continue
		}
		if err := c.Delete(ctx, item); err != nil && !apierrors.IsNotFound(err) {
			log.Error(err, "failed to delete old template", "name", item.GetName())
			continue
		}
		log.Info("Garbage-collected old SandboxTemplate", "name", item.GetName())
	}
	return nil
}

// SandboxTemplateServiceAccount reads the service account name from a SandboxTemplate.
func SandboxTemplateServiceAccount(ctx context.Context, c client.Client, templateName, namespace string) (string, error) {
	tmpl := &unstructured.Unstructured{}
	tmpl.SetGroupVersionKind(sandboxTemplateGVK)
	if err := c.Get(ctx, types.NamespacedName{Name: templateName, Namespace: namespace}, tmpl); err != nil {
		return "", err
	}
	sa, found, err := unstructured.NestedString(tmpl.Object, "spec", "podTemplate", "spec", "serviceAccountName")
	if err != nil {
		return "", fmt.Errorf("extract serviceAccountName from template %q: %w", templateName, err)
	}
	if !found || sa == "" {
		return "", fmt.Errorf("template %q has no serviceAccountName", templateName)
	}
	return sa, nil
}

// --- Unstructured patch helpers ---
func firstContainer(tmpl *unstructured.Unstructured) (map[string]any, []any, error) {
	containers, found, err := unstructured.NestedSlice(tmpl.Object, "spec", "podTemplate", "spec", "containers")
	if err != nil {
		return nil, nil, fmt.Errorf("read containers: %w", err)
	}
	if !found || len(containers) == 0 {
		return nil, nil, fmt.Errorf("template has no containers")
	}
	container, ok := containers[0].(map[string]any)
	if !ok {
		return nil, nil, fmt.Errorf("container[0] is not a map")
	}
	return container, containers, nil
}

func writeContainers(tmpl *unstructured.Unstructured, container map[string]any, containers []any) error {
	containers[0] = container
	return unstructured.SetNestedSlice(tmpl.Object, containers, "spec", "podTemplate", "spec", "containers")
}

func setEnvVar(tmpl *unstructured.Unstructured, name, value string) error {
	return upsertEnv(tmpl, name, map[string]any{
		"name":  name,
		"value": value,
	})
}

func addEnvVarFromSecret(tmpl *unstructured.Unstructured, envName, secretName, key string) error {
	return upsertEnv(tmpl, envName, map[string]any{
		"name": envName,
		"valueFrom": map[string]any{
			"secretKeyRef": map[string]any{
				"name":     secretName,
				"key":      key,
				"optional": true,
			},
		},
	})
}

func addEnvFromSecret(tmpl *unstructured.Unstructured, secretName string) error {
	container, containers, err := firstContainer(tmpl)
	if err != nil {
		return fmt.Errorf("addEnvFromSecret: %w", err)
	}
	envFromList, _, _ := unstructured.NestedSlice(container, "envFrom")
	for _, e := range envFromList {
		entry, eOK := e.(map[string]any)
		if !eOK {
			continue
		}
		ref, _ := entry["secretRef"].(map[string]any)
		if ref != nil && ref["name"] == secretName {
			return nil
		}
	}
	envFromList = append(envFromList, map[string]any{
		"secretRef": map[string]any{
			"name": secretName,
		},
	})
	if err := unstructured.SetNestedSlice(container, envFromList, "envFrom"); err != nil {
		return fmt.Errorf("set envFrom: %w", err)
	}
	return writeContainers(tmpl, container, containers)
}

func upsertEnv(tmpl *unstructured.Unstructured, name string, entry map[string]any) error {
	container, containers, err := firstContainer(tmpl)
	if err != nil {
		return fmt.Errorf("upsertEnv(%s): %w", name, err)
	}
	envList, _, _ := unstructured.NestedSlice(container, "env")

	updated := false
	for i, e := range envList {
		env, eOK := e.(map[string]any)
		if !eOK {
			continue
		}
		if env["name"] == name {
			envList[i] = entry
			updated = true
			break
		}
	}
	if !updated {
		envList = append(envList, entry)
	}

	if err := unstructured.SetNestedSlice(container, envList, "env"); err != nil {
		return fmt.Errorf("set env: %w", err)
	}
	return writeContainers(tmpl, container, containers)
}

func addSecretVolume(tmpl *unstructured.Unstructured, volumeName, secretName string) error {
	volumes, _, _ := unstructured.NestedSlice(tmpl.Object, "spec", "podTemplate", "spec", "volumes")
	vol := map[string]any{
		"name": volumeName,
		"secret": map[string]any{
			"secretName": secretName,
		},
	}
	for i, v := range volumes {
		existing, ok := v.(map[string]any)
		if !ok {
			continue
		}
		if existing["name"] == volumeName {
			volumes[i] = vol
			return unstructured.SetNestedSlice(tmpl.Object, volumes, "spec", "podTemplate", "spec", "volumes")
		}
	}
	volumes = append(volumes, vol)
	return unstructured.SetNestedSlice(tmpl.Object, volumes, "spec", "podTemplate", "spec", "volumes")
}

func addVolumeMount(tmpl *unstructured.Unstructured, name, mountPath string, readOnly bool) error {
	container, containers, err := firstContainer(tmpl)
	if err != nil {
		return fmt.Errorf("addVolumeMount: %w", err)
	}
	mounts, _, _ := unstructured.NestedSlice(container, "volumeMounts")
	mount := map[string]any{
		"name":      name,
		"mountPath": mountPath,
		"readOnly":  readOnly,
	}
	for i, m := range mounts {
		existing, mOK := m.(map[string]any)
		if !mOK {
			continue
		}
		if existing["mountPath"] == mountPath {
			mounts[i] = mount
			if err := unstructured.SetNestedSlice(container, mounts, "volumeMounts"); err != nil {
				return fmt.Errorf("set volumeMounts (update): %w", err)
			}
			return writeContainers(tmpl, container, containers)
		}
	}
	mounts = append(mounts, mount)
	if err := unstructured.SetNestedSlice(container, mounts, "volumeMounts"); err != nil {
		return fmt.Errorf("set volumeMounts (append): %w", err)
	}
	return writeContainers(tmpl, container, containers)
}

func patchSkillsImage(tmpl *unstructured.Unstructured, image string) error {
	volumes, found, err := unstructured.NestedSlice(tmpl.Object, "spec", "podTemplate", "spec", "volumes")
	if err != nil {
		return fmt.Errorf("read volumes: %w", err)
	}
	if !found {
		return fmt.Errorf("template has no volumes")
	}
	for i, v := range volumes {
		vol, ok := v.(map[string]any)
		if !ok {
			continue
		}
		volName, _, _ := unstructured.NestedString(vol, "name")
		if volName != "skills" {
			continue
		}
		if err := unstructured.SetNestedField(vol, image, "image", "reference"); err != nil {
			return fmt.Errorf("set skills image reference: %w", err)
		}
		if err := unstructured.SetNestedField(vol, "Always", "image", "pullPolicy"); err != nil {
			return fmt.Errorf("set skills image pullPolicy: %w", err)
		}
		volumes[i] = vol
	}
	return unstructured.SetNestedSlice(tmpl.Object, volumes, "spec", "podTemplate", "spec", "volumes")
}

func patchSkillsPaths(tmpl *unstructured.Unstructured, paths []string) error {
	if len(paths) == 0 {
		return nil
	}
	container, containers, err := firstContainer(tmpl)
	if err != nil {
		return fmt.Errorf("patchSkillsPaths: %w", err)
	}
	mounts, _, _ := unstructured.NestedSlice(container, "volumeMounts")

	baseMountPath := "/app/skills"
	var filtered []any
	for _, m := range mounts {
		mount, mOK := m.(map[string]any)
		if !mOK {
			filtered = append(filtered, m)
			continue
		}
		if mount["name"] == "skills" {
			if mp, ok := mount["mountPath"].(string); ok {
				baseMountPath = mp
			}
			continue
		}
		filtered = append(filtered, m)
	}

	for _, p := range paths {
		subPath := strings.TrimPrefix(p, "/")
		skillName := path.Base(p)
		mountPath := path.Join(baseMountPath, skillName)
		filtered = append(filtered, map[string]any{
			"name":      "skills",
			"mountPath": mountPath,
			"subPath":   subPath,
			"readOnly":  true,
		})
	}

	if err := unstructured.SetNestedSlice(container, filtered, "volumeMounts"); err != nil {
		return fmt.Errorf("set volumeMounts: %w", err)
	}
	return writeContainers(tmpl, container, containers)
}

func patchAgentMode(tmpl *unstructured.Unstructured, mode string) error {
	return setEnvVar(tmpl, agentModeEnvVar, mode)
}

// --- MCP Server patching ---

type mcpServerEnvEntry struct {
	Name    string              `json:"name"`
	URL     string              `json:"url"`
	Timeout int32               `json:"timeout,omitempty"`
	Headers []mcpHeaderEnvEntry `json:"headers,omitempty"`
}

type mcpHeaderEnvEntry struct {
	Name       string `json:"name"`
	Source     string `json:"source"`
	SecretName string `json:"secretName,omitempty"`
}

func patchMCPServers(tmpl *unstructured.Unstructured, servers []agenticv1alpha1.MCPServerConfig) error {
	entries := make([]mcpServerEnvEntry, 0, len(servers))
	for _, s := range servers {
		entry := mcpServerEnvEntry{
			Name:    s.Name,
			URL:     s.URL,
			Timeout: s.TimeoutSeconds,
		}
		for _, h := range s.Headers {
			he := mcpHeaderEnvEntry{
				Name:   h.Name,
				Source: string(h.ValueFrom.Type),
			}
			if h.ValueFrom.Type == agenticv1alpha1.MCPHeaderSourceTypeSecret {
				he.SecretName = h.ValueFrom.Secret.Name
				if err := addSecretVolume(tmpl, "mcp-header-"+h.ValueFrom.Secret.Name, h.ValueFrom.Secret.Name); err != nil {
					return fmt.Errorf("add MCP header secret volume: %w", err)
				}
				if err := addVolumeMount(tmpl, "mcp-header-"+h.ValueFrom.Secret.Name, mcpHeadersMountRoot+"/"+h.ValueFrom.Secret.Name, true); err != nil {
					return fmt.Errorf("add MCP header volume mount: %w", err)
				}
			}
			entry.Headers = append(entry.Headers, he)
		}
		entries = append(entries, entry)
	}

	data, err := json.Marshal(entries)
	if err != nil {
		return fmt.Errorf("marshal MCP server config: %w", err)
	}
	return setEnvVar(tmpl, mcpServersEnvVar, string(data))
}
