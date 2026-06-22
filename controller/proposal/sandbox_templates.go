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

const (
	ErrMarshalTemplateHash      = "marshal template hash input"
	ErrReadBaseTemplate         = "failed to read base sandbox template"
	ErrComputeTemplateHash      = "compute template hash"
	ErrCheckTemplate            = "failed to check template"
	ErrPatchSkillsImage         = "patch skills image"
	ErrPatchSkillsPaths         = "patch skills paths"
	ErrPatchLLMCredentials      = "patch LLM credentials"
	ErrPatchMCPServers          = "patch MCP servers"
	ErrPatchRequiredSecrets     = "patch required secrets"
	ErrPatchProbes              = "patch probes"
	ErrSetServiceAccountName    = "set serviceAccountName on template"
	ErrCreateTemplate           = "failed to create template"
	ErrSetProvider              = "set LIGHTSPEED_PROVIDER"
	ErrSetModel                 = "set LIGHTSPEED_MODEL"
	ErrAddCredentialsEnvFrom    = "add credentials envFrom"
	ErrAddCredentialsVolume     = "add credentials volume"
	ErrMountCredentials         = "mount credentials"
	ErrSetProviderURL           = "set LIGHTSPEED_PROVIDER_URL"
	ErrSetModelProvider         = "set LIGHTSPEED_MODEL_PROVIDER"
	ErrSetProviderProject       = "set LIGHTSPEED_PROVIDER_PROJECT"
	ErrSetProviderRegion        = "set LIGHTSPEED_PROVIDER_REGION"
	ErrSetProviderAPIVersion    = "set LIGHTSPEED_PROVIDER_API_VERSION"
	ErrAddSecretVolume          = "add secret volume"
	ErrAddVolumeMountForSecret  = "add volume mount"
	ErrAddEnvVarFromSecret      = "add env var from secret"
	ErrPatchProbesFn            = "patchProbes"
	ErrListOldTemplates         = "failed to list old templates"
	ErrExtractSAName            = "extract serviceAccountName from template"
	ErrReadContainers           = "read containers"
	ErrAddEnvFromSecretFn       = "addEnvFromSecret"
	ErrSetEnvFrom               = "set envFrom"
	ErrUpsertEnv                = "upsertEnv"
	ErrSetEnv                   = "set env"
	ErrAddVolumeMountFn         = "addVolumeMount"
	ErrSetVolumeMountsUpdate    = "set volumeMounts (update)"
	ErrSetVolumeMountsAppend    = "set volumeMounts (append)"
	ErrReadVolumes              = "read volumes"
	ErrSetSkillsImageRef        = "set skills image reference"
	ErrSetSkillsImagePullPolicy = "set skills image pullPolicy"
	ErrPatchSkillsPathsFn       = "patchSkillsPaths"
	ErrSetVolumeMounts          = "set volumeMounts"
	ErrAddMCPHeaderVolume       = "add MCP header secret volume"
	ErrAddMCPHeaderMount        = "add MCP header volume mount"
	ErrMarshalMCPConfig         = "marshal MCP server config"
)

var sandboxTemplateGVK = schema.GroupVersionKind{
	Group: "extensions.agents.x-k8s.io", Version: "v1alpha1", Kind: "SandboxTemplate",
}

const (
	llmCredsVolumeName   = "llm-credentials"
	llmCredsMountPath    = "/var/run/secrets/llm-credentials"
	mcpHeadersMountRoot  = "/var/secrets/mcp"
	mcpServersEnvVar     = "LIGHTSPEED_MCP_SERVERS"
	dataSourceMountPath  = "/data/input"
	dataSourceVolumeName = "lightspeed-data-source"

	LabelManaged      = "agentic.openshift.io/managed"
	LabelBaseTemplate = "agentic.openshift.io/base-template"
	LabelStep         = "agentic.openshift.io/step"
	LabelAgent        = "agentic.openshift.io/agent"
	LabelProposal     = "agentic.openshift.io/proposal"
	LabelComponent    = "agentic.openshift.io/component"
)

type templateHashInput struct {
	LLM                 agenticv1alpha1.LLMProviderSpec     `json:"llm"`
	Model               string                              `json:"model"`
	Skills              []agenticv1alpha1.SkillsSource      `json:"skills"`
	MCPServers          []agenticv1alpha1.MCPServerConfig   `json:"mcpServers,omitempty"`
	RequiredSecrets     []agenticv1alpha1.SecretRequirement `json:"requiredSecrets,omitempty"`
	DataSource          *agenticv1alpha1.DataSource         `json:"dataSource,omitempty"`
	Step                string                              `json:"step"`
	BaseResourceVersion string                              `json:"baseRV"`
	ServiceAccount      string                              `json:"serviceAccount"`
}

func computeTemplateHash(
	llm *agenticv1alpha1.LLMProvider,
	model string,
	skills []agenticv1alpha1.SkillsSource,
	mcpServers []agenticv1alpha1.MCPServerConfig,
	requiredSecrets []agenticv1alpha1.SecretRequirement,
	dataSource *agenticv1alpha1.DataSource,
	step string,
	baseResourceVersion string,
	serviceAccount string,
) (string, error) {
	input := templateHashInput{
		LLM:                 llm.Spec,
		Model:               model,
		Skills:              skills,
		MCPServers:          mcpServers,
		RequiredSecrets:     requiredSecrets,
		DataSource:          dataSource,
		Step:                step,
		BaseResourceVersion: baseResourceVersion,
		ServiceAccount:      serviceAccount,
	}
	data, err := json.Marshal(input)
	if err != nil {
		return "", fmt.Errorf("%s: %w", ErrMarshalTemplateHash, err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])[:10], nil
}

func agentTemplateName(step, agentName, hash string) string {
	return truncateK8sName(fmt.Sprintf("ls-%s-%s-%s", step, agentName, hash))
}

// EnsureAgentTemplate creates a SandboxTemplate derived from the base template
// with skills, LLM credentials, MCP servers, required secrets, and an optional
// dataSource PVC from the CRD chain.
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
	serviceAccount string,
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
		return "", fmt.Errorf("%s %q: %w", ErrReadBaseTemplate, baseTemplateName, err)
	}

	var skills []agenticv1alpha1.SkillsSource
	var mcpServers []agenticv1alpha1.MCPServerConfig
	var requiredSecrets []agenticv1alpha1.SecretRequirement
	var dataSource *agenticv1alpha1.DataSource
	if tools != nil {
		skills = tools.Skills
		mcpServers = tools.MCPServers
		requiredSecrets = tools.RequiredSecrets
		if !tools.DataSource.IsZero() {
			dataSource = &tools.DataSource
		}
	}

	hash, err := computeTemplateHash(llm, agent.Spec.Model, skills, mcpServers, requiredSecrets, dataSource, step, base.GetResourceVersion(), serviceAccount)
	if err != nil {
		return "", fmt.Errorf("%s: %w", ErrComputeTemplateHash, err)
	}
	name := agentTemplateName(step, agent.Name, hash)

	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(sandboxTemplateGVK)
	err = c.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, existing)
	if err == nil {
		return name, nil
	}
	if !apierrors.IsNotFound(err) {
		return "", fmt.Errorf("%s %q: %w", ErrCheckTemplate, name, err)
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
			return "", fmt.Errorf("%s: %w", ErrPatchSkillsImage, err)
		}
		if len(skills[0].Paths) > 0 {
			if err := patchSkillsPaths(derived, skills[0].Paths); err != nil {
				return "", fmt.Errorf("%s: %w", ErrPatchSkillsPaths, err)
			}
		}
	}

	if err := patchLLMCredentials(derived, llm, agent.Spec.Model); err != nil {
		return "", fmt.Errorf("%s: %w", ErrPatchLLMCredentials, err)
	}

	if len(mcpServers) > 0 {
		if err := patchMCPServers(derived, mcpServers); err != nil {
			return "", fmt.Errorf("%s: %w", ErrPatchMCPServers, err)
		}
	}

	if len(requiredSecrets) > 0 {
		if err := patchRequiredSecrets(derived, requiredSecrets); err != nil {
			return "", fmt.Errorf("%s: %w", ErrPatchRequiredSecrets, err)
		}
	}

	if dataSource != nil {
		if err := patchDataSource(derived, dataSource); err != nil {
			return "", fmt.Errorf("patch data source: %w", err)
		}
	}

	if err := patchProbes(derived); err != nil {
		return "", fmt.Errorf("%s: %w", ErrPatchProbes, err)
	}

	if serviceAccount != "" {
		if err := unstructured.SetNestedField(derived.Object, serviceAccount, "spec", "podTemplate", "spec", "serviceAccountName"); err != nil {
			return "", fmt.Errorf("%s: %w", ErrSetServiceAccountName, err)
		}
	}

	if err := c.Create(ctx, derived); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return name, nil
		}
		return "", fmt.Errorf("%s %q: %w", ErrCreateTemplate, name, err)
	}

	log.Info("Created agent SandboxTemplate",
		LogKeyName, name,
		"base", baseTemplateName,
		LogKeyStep, step,
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

func providerTypeString(t agenticv1alpha1.LLMProviderType) string {
	switch t {
	case agenticv1alpha1.LLMProviderAnthropic:
		return "anthropic"
	case agenticv1alpha1.LLMProviderGoogleCloudVertex:
		return "vertex"
	case agenticv1alpha1.LLMProviderOpenAI:
		return "openai"
	case agenticv1alpha1.LLMProviderAzureOpenAI:
		return "azure"
	case agenticv1alpha1.LLMProviderAWSBedrock:
		return "bedrock"
	default:
		return strings.ToLower(string(t))
	}
}

func patchLLMCredentials(tmpl *unstructured.Unstructured, llm *agenticv1alpha1.LLMProvider, model string) error {
	secretName := credentialsSecretName(llm)

	if err := setEnvVar(tmpl, "LIGHTSPEED_PROVIDER", providerTypeString(llm.Spec.Type)); err != nil {
		return fmt.Errorf("%s: %w", ErrSetProvider, err)
	}
	if err := setEnvVar(tmpl, "LIGHTSPEED_MODEL", model); err != nil {
		return fmt.Errorf("%s: %w", ErrSetModel, err)
	}
	if err := addEnvFromSecret(tmpl, secretName); err != nil {
		return fmt.Errorf("%s: %w", ErrAddCredentialsEnvFrom, err)
	}
	if err := addSecretVolume(tmpl, llmCredsVolumeName, secretName); err != nil {
		return fmt.Errorf("%s: %w", ErrAddCredentialsVolume, err)
	}
	if err := addVolumeMount(tmpl, llmCredsVolumeName, llmCredsMountPath, true); err != nil {
		return fmt.Errorf("%s: %w", ErrMountCredentials, err)
	}

	switch llm.Spec.Type {
	case agenticv1alpha1.LLMProviderAnthropic:
		if u := providerURL(llm); u != "" {
			if err := setEnvVar(tmpl, "LIGHTSPEED_PROVIDER_URL", u); err != nil {
				return fmt.Errorf("%s: %w", ErrSetProviderURL, err)
			}
		}
	case agenticv1alpha1.LLMProviderGoogleCloudVertex:
		cfg := llm.Spec.GoogleCloudVertex
		if err := setEnvVar(tmpl, "LIGHTSPEED_MODEL_PROVIDER", strings.ToLower(string(cfg.ModelProvider))); err != nil {
			return fmt.Errorf("%s: %w", ErrSetModelProvider, err)
		}
		if err := setEnvVar(tmpl, "LIGHTSPEED_PROVIDER_PROJECT", cfg.ProjectID); err != nil {
			return fmt.Errorf("%s: %w", ErrSetProviderProject, err)
		}
		if err := setEnvVar(tmpl, "LIGHTSPEED_PROVIDER_REGION", cfg.Region); err != nil {
			return fmt.Errorf("%s: %w", ErrSetProviderRegion, err)
		}
		if u := providerURL(llm); u != "" {
			if err := setEnvVar(tmpl, "LIGHTSPEED_PROVIDER_URL", u); err != nil {
				return fmt.Errorf("%s: %w", ErrSetProviderURL, err)
			}
		}
	case agenticv1alpha1.LLMProviderOpenAI:
		if u := providerURL(llm); u != "" {
			if err := setEnvVar(tmpl, "LIGHTSPEED_PROVIDER_URL", u); err != nil {
				return fmt.Errorf("%s: %w", ErrSetProviderURL, err)
			}
		}
	case agenticv1alpha1.LLMProviderAzureOpenAI:
		cfg := llm.Spec.AzureOpenAI
		providerURLValue := cfg.Endpoint
		if u := cfg.URL; u != "" {
			providerURLValue = u
		}
		if err := setEnvVar(tmpl, "LIGHTSPEED_PROVIDER_URL", providerURLValue); err != nil {
			return fmt.Errorf("%s: %w", ErrSetProviderURL, err)
		}
		if cfg.APIVersion != "" {
			if err := setEnvVar(tmpl, "LIGHTSPEED_PROVIDER_API_VERSION", cfg.APIVersion); err != nil {
				return fmt.Errorf("%s: %w", ErrSetProviderAPIVersion, err)
			}
		}
	case agenticv1alpha1.LLMProviderAWSBedrock:
		cfg := llm.Spec.AWSBedrock
		if err := setEnvVar(tmpl, "LIGHTSPEED_PROVIDER_REGION", cfg.Region); err != nil {
			return fmt.Errorf("%s: %w", ErrSetProviderRegion, err)
		}
		if u := providerURL(llm); u != "" {
			if err := setEnvVar(tmpl, "LIGHTSPEED_PROVIDER_URL", u); err != nil {
				return fmt.Errorf("%s: %w", ErrSetProviderURL, err)
			}
		}
	}
	return nil
}

func patchRequiredSecrets(tmpl *unstructured.Unstructured, secrets []agenticv1alpha1.SecretRequirement) error {
	for _, s := range secrets {
		switch s.MountAs.Type {
		case agenticv1alpha1.SecretMountFilePath:
			volName := "req-" + s.Name
			if err := addSecretVolume(tmpl, volName, s.Name); err != nil {
				return fmt.Errorf("%s %q: %w", ErrAddSecretVolume, s.Name, err)
			}
			if err := addVolumeMount(tmpl, volName, s.MountAs.FilePath.Path, true); err != nil {
				return fmt.Errorf("%s %q: %w", ErrAddVolumeMountForSecret, s.MountAs.FilePath.Path, err)
			}
		case agenticv1alpha1.SecretMountEnvVar:
			if err := addEnvVarFromSecret(tmpl, s.MountAs.EnvVar.Name, s.Name, "token"); err != nil {
				return fmt.Errorf("%s %q: %w", ErrAddEnvVarFromSecret, s.Name, err)
			}
		}
	}
	return nil
}

func patchProbes(tmpl *unstructured.Unstructured) error {
	container, containers, err := firstContainer(tmpl)
	if err != nil {
		return fmt.Errorf("%s: %w", ErrPatchProbesFn, err)
	}

	container["readinessProbe"] = map[string]any{
		"httpGet": map[string]any{
			"path": "/ready",
			"port": int64(8080),
		},
		"initialDelaySeconds": int64(3),
		"periodSeconds":       int64(10),
		"failureThreshold":    int64(3),
	}

	container["livenessProbe"] = map[string]any{
		"httpGet": map[string]any{
			"path": "/health",
			"port": int64(8080),
		},
		"initialDelaySeconds": int64(10),
		"periodSeconds":       int64(30),
		"failureThreshold":    int64(3),
	}

	return writeContainers(tmpl, container, containers)
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
		LabelStep:    step,
	})

	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(sandboxTemplateGVK)
	if err := c.List(ctx, list, &client.ListOptions{
		Namespace:     namespace,
		LabelSelector: sel,
	}); err != nil {
		return fmt.Errorf("%s: %w", ErrListOldTemplates, err)
	}

	log := logf.FromContext(ctx).WithName("sandbox-templates")
	for i := range list.Items {
		item := &list.Items[i]
		if item.GetName() == currentName {
			continue
		}
		if err := c.Delete(ctx, item); err != nil && !apierrors.IsNotFound(err) {
			log.Error(err, "failed to delete old template", LogKeyName, item.GetName())
			continue
		}
		log.Info("Garbage-collected old SandboxTemplate", LogKeyName, item.GetName())
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
		return "", fmt.Errorf("%s %q: %w", ErrExtractSAName, templateName, err)
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
		return nil, nil, fmt.Errorf("%s: %w", ErrReadContainers, err)
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
		return fmt.Errorf("%s: %w", ErrAddEnvFromSecretFn, err)
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
		return fmt.Errorf("%s: %w", ErrSetEnvFrom, err)
	}
	return writeContainers(tmpl, container, containers)
}

func upsertEnv(tmpl *unstructured.Unstructured, name string, entry map[string]any) error {
	container, containers, err := firstContainer(tmpl)
	if err != nil {
		return fmt.Errorf("%s(%s): %w", ErrUpsertEnv, name, err)
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
		return fmt.Errorf("%s: %w", ErrSetEnv, err)
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

func addPVCVolume(tmpl *unstructured.Unstructured, volumeName, claimName string) error {
	volumes, _, _ := unstructured.NestedSlice(tmpl.Object, "spec", "podTemplate", "spec", "volumes")
	vol := map[string]any{
		"name": volumeName,
		"persistentVolumeClaim": map[string]any{
			"claimName": claimName,
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

func patchDataSource(tmpl *unstructured.Unstructured, ds *agenticv1alpha1.DataSource) error {
	volName := dataSourceVolumeName
	if err := addPVCVolume(tmpl, volName, ds.ClaimName); err != nil {
		return fmt.Errorf("add data source PVC volume: %w", err)
	}
	return addVolumeMount(tmpl, volName, dataSourceMountPath, true)
}

func addVolumeMount(tmpl *unstructured.Unstructured, name, mountPath string, readOnly bool) error {
	container, containers, err := firstContainer(tmpl)
	if err != nil {
		return fmt.Errorf("%s: %w", ErrAddVolumeMountFn, err)
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
				return fmt.Errorf("%s: %w", ErrSetVolumeMountsUpdate, err)
			}
			return writeContainers(tmpl, container, containers)
		}
	}
	mounts = append(mounts, mount)
	if err := unstructured.SetNestedSlice(container, mounts, "volumeMounts"); err != nil {
		return fmt.Errorf("%s: %w", ErrSetVolumeMountsAppend, err)
	}
	return writeContainers(tmpl, container, containers)
}

func patchSkillsImage(tmpl *unstructured.Unstructured, image string) error {
	volumes, found, err := unstructured.NestedSlice(tmpl.Object, "spec", "podTemplate", "spec", "volumes")
	if err != nil {
		return fmt.Errorf("%s: %w", ErrReadVolumes, err)
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
			return fmt.Errorf("%s: %w", ErrSetSkillsImageRef, err)
		}
		if err := unstructured.SetNestedField(vol, "Always", "image", "pullPolicy"); err != nil {
			return fmt.Errorf("%s: %w", ErrSetSkillsImagePullPolicy, err)
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
		return fmt.Errorf("%s: %w", ErrPatchSkillsPathsFn, err)
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
		return fmt.Errorf("%s: %w", ErrSetVolumeMounts, err)
	}
	return writeContainers(tmpl, container, containers)
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
					return fmt.Errorf("%s: %w", ErrAddMCPHeaderVolume, err)
				}
				if err := addVolumeMount(tmpl, "mcp-header-"+h.ValueFrom.Secret.Name, mcpHeadersMountRoot+"/"+h.ValueFrom.Secret.Name, true); err != nil {
					return fmt.Errorf("%s: %w", ErrAddMCPHeaderMount, err)
				}
			}
			entry.Headers = append(entry.Headers, he)
		}
		entries = append(entries, entry)
	}

	data, err := json.Marshal(entries)
	if err != nil {
		return fmt.Errorf("%s: %w", ErrMarshalMCPConfig, err)
	}
	return setEnvVar(tmpl, mcpServersEnvVar, string(data))
}
