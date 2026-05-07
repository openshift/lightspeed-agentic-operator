package proposal

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
)

func testLLMProvider(providerType agenticv1alpha1.LLMProviderType) *agenticv1alpha1.LLMProvider {
	creds := agenticv1alpha1.SecretReference{Name: "my-llm-secret"}
	spec := agenticv1alpha1.LLMProviderSpec{Type: providerType}
	switch providerType {
	case agenticv1alpha1.LLMProviderAnthropic:
		spec.Anthropic = agenticv1alpha1.AnthropicConfig{CredentialsSecret: creds}
	case agenticv1alpha1.LLMProviderGoogleCloudVertex:
		spec.GoogleCloudVertex = agenticv1alpha1.GoogleCloudVertexConfig{CredentialsSecret: creds, ProjectID: "test-project", Region: "us-central1"}
	case agenticv1alpha1.LLMProviderOpenAI:
		spec.OpenAI = agenticv1alpha1.OpenAIConfig{CredentialsSecret: creds}
	case agenticv1alpha1.LLMProviderAzureOpenAI:
		spec.AzureOpenAI = agenticv1alpha1.AzureOpenAIConfig{CredentialsSecret: creds, Endpoint: "https://test.openai.azure.com"}
	case agenticv1alpha1.LLMProviderAWSBedrock:
		spec.AWSBedrock = agenticv1alpha1.AWSBedrockConfig{CredentialsSecret: creds, Region: "us-east-1"}
	}
	return &agenticv1alpha1.LLMProvider{Spec: spec}
}

func testLLMProviderWithURL(providerType agenticv1alpha1.LLMProviderType, u string) *agenticv1alpha1.LLMProvider {
	p := testLLMProvider(providerType)
	switch providerType {
	case agenticv1alpha1.LLMProviderAnthropic:
		p.Spec.Anthropic.URL = u
	case agenticv1alpha1.LLMProviderGoogleCloudVertex:
		p.Spec.GoogleCloudVertex.URL = u
	case agenticv1alpha1.LLMProviderOpenAI:
		p.Spec.OpenAI.URL = u
	case agenticv1alpha1.LLMProviderAzureOpenAI:
		p.Spec.AzureOpenAI.URL = u
	case agenticv1alpha1.LLMProviderAWSBedrock:
		p.Spec.AWSBedrock.URL = u
	}
	return p
}

func emptyTemplate() *unstructured.Unstructured {
	return &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "extensions.agents.x-k8s.io/v1alpha1",
			"kind":       "SandboxTemplate",
			"spec": map[string]any{
				"podTemplate": map[string]any{
					"spec": map[string]any{
						"containers": []any{
							map[string]any{
								"name": "agent",
								"env":  []any{},
							},
						},
						"volumes": []any{},
					},
				},
			},
		},
	}
}

func mustHash(t *testing.T, llm *agenticv1alpha1.LLMProvider, model string, skills []agenticv1alpha1.SkillsSource, requiredSecrets []agenticv1alpha1.SecretRequirement, phase string) string {
	t.Helper()
	h, err := computeTemplateHash(llm, model, skills, nil, requiredSecrets, phase, "")
	if err != nil {
		t.Fatalf("computeTemplateHash: %v", err)
	}
	return h
}

func getEnvVars(tmpl *unstructured.Unstructured) []map[string]any {
	containers, _, _ := unstructured.NestedSlice(tmpl.Object, "spec", "podTemplate", "spec", "containers")
	if len(containers) == 0 {
		return nil
	}
	container := containers[0].(map[string]any)
	envList, _, _ := unstructured.NestedSlice(container, "env")
	result := make([]map[string]any, 0, len(envList))
	for _, e := range envList {
		result = append(result, e.(map[string]any))
	}
	return result
}

func findEnv(envs []map[string]any, name string) (map[string]any, bool) {
	for _, e := range envs {
		if e["name"] == name {
			return e, true
		}
	}
	return nil, false
}

func getEnvFrom(tmpl *unstructured.Unstructured) []map[string]any {
	containers, _, _ := unstructured.NestedSlice(tmpl.Object, "spec", "podTemplate", "spec", "containers")
	if len(containers) == 0 {
		return nil
	}
	container := containers[0].(map[string]any)
	envFromList, _, _ := unstructured.NestedSlice(container, "envFrom")
	result := make([]map[string]any, 0, len(envFromList))
	for _, e := range envFromList {
		result = append(result, e.(map[string]any))
	}
	return result
}

func hasSecretEnvFrom(tmpl *unstructured.Unstructured, secretName string) bool {
	for _, e := range getEnvFrom(tmpl) {
		ref, _ := e["secretRef"].(map[string]any)
		if ref != nil && ref["name"] == secretName {
			return true
		}
	}
	return false
}

func getVolumeMounts(tmpl *unstructured.Unstructured) []map[string]any {
	containers, _, _ := unstructured.NestedSlice(tmpl.Object, "spec", "podTemplate", "spec", "containers")
	if len(containers) == 0 {
		return nil
	}
	container := containers[0].(map[string]any)
	mounts, _, _ := unstructured.NestedSlice(container, "volumeMounts")
	var result []map[string]any
	for _, m := range mounts {
		result = append(result, m.(map[string]any))
	}
	return result
}

func templateWithSkillsMount() *unstructured.Unstructured {
	return &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "extensions.agents.x-k8s.io/v1alpha1",
			"kind":       "SandboxTemplate",
			"spec": map[string]any{
				"podTemplate": map[string]any{
					"spec": map[string]any{
						"containers": []any{
							map[string]any{
								"name": "agent",
								"volumeMounts": []any{
									map[string]any{"name": "tls", "mountPath": "/etc/tls", "readOnly": true},
									map[string]any{"name": "skills", "mountPath": "/app/skills"},
									map[string]any{"name": "home", "mountPath": "/home/agent"},
								},
							},
						},
						"volumes": []any{
							map[string]any{"name": "skills", "image": map[string]any{"reference": "old:latest"}},
						},
					},
				},
			},
		},
	}
}

// --- computeTemplateHash tests ---

func TestComputeTemplateHash_Deterministic(t *testing.T) {
	llm := testLLMProvider(agenticv1alpha1.LLMProviderGoogleCloudVertex)
	skills := []agenticv1alpha1.SkillsSource{{Image: "quay.io/test/skills:latest"}}

	h1 := mustHash(t, llm, "claude-opus-4-6", skills, nil, "analysis")
	h2 := mustHash(t, llm, "claude-opus-4-6", skills, nil, "analysis")

	if h1 != h2 {
		t.Errorf("same input produced different hashes: %q vs %q", h1, h2)
	}
	if len(h1) != 10 {
		t.Errorf("hash length = %d, want 10", len(h1))
	}
}

func TestComputeTemplateHash_DifferentModel(t *testing.T) {
	llm := testLLMProvider(agenticv1alpha1.LLMProviderGoogleCloudVertex)
	skills := []agenticv1alpha1.SkillsSource{{Image: "quay.io/test/skills:latest"}}
	h1 := mustHash(t, llm, "claude-opus-4-6", skills, nil, "analysis")
	h2 := mustHash(t, llm, "claude-sonnet-4-6", skills, nil, "analysis")

	if h1 == h2 {
		t.Error("different models should produce different hashes")
	}
}

func TestComputeTemplateHash_DifferentPhase(t *testing.T) {
	llm := testLLMProvider(agenticv1alpha1.LLMProviderGoogleCloudVertex)
	skills := []agenticv1alpha1.SkillsSource{{Image: "quay.io/test/skills:latest"}}

	h1 := mustHash(t, llm, "claude-opus-4-6", skills, nil, "analysis")
	h2 := mustHash(t, llm, "claude-opus-4-6", skills, nil, "execution")

	if h1 == h2 {
		t.Error("different phases should produce different hashes")
	}
}

func TestComputeTemplateHash_DifferentSecret(t *testing.T) {
	skills := []agenticv1alpha1.SkillsSource{{Image: "quay.io/test/skills:latest"}}
	llm1 := testLLMProvider(agenticv1alpha1.LLMProviderAnthropic)
	llm2 := testLLMProvider(agenticv1alpha1.LLMProviderAnthropic)
	llm2.Spec.Anthropic.CredentialsSecret.Name = "different-secret"

	h1 := mustHash(t, llm1, "claude-opus-4-6", skills, nil, "analysis")
	h2 := mustHash(t, llm2, "claude-opus-4-6", skills, nil, "analysis")

	if h1 == h2 {
		t.Error("different secrets should produce different hashes")
	}
}

func TestComputeTemplateHash_DifferentRequiredSecrets(t *testing.T) {
	llm := testLLMProvider(agenticv1alpha1.LLMProviderGoogleCloudVertex)
	skills := []agenticv1alpha1.SkillsSource{{Image: "quay.io/test/skills:latest"}}

	h1 := mustHash(t, llm, "claude-opus-4-6", skills, nil, "analysis")
	h2 := mustHash(t, llm, "claude-opus-4-6", skills, []agenticv1alpha1.SecretRequirement{
		{Name: "my-token", MountAs: agenticv1alpha1.SecretMountSpec{Type: agenticv1alpha1.SecretMountEnvVar, EnvVar: agenticv1alpha1.SecretMountEnvVarConfig{Name: "MY_TOKEN"}}},
	}, "analysis")

	if h1 == h2 {
		t.Error("different required secrets should produce different hashes")
	}
}

// --- patchLLMCredentials tests ---

func TestPatchLLMCredentials_Anthropic(t *testing.T) {
	tmpl := emptyTemplate()
	llm := testLLMProviderWithURL(agenticv1alpha1.LLMProviderAnthropic, "https://custom.api")

	if err := patchLLMCredentials(tmpl, llm, "claude-opus-4-6"); err != nil {
		t.Fatalf("patchLLMCredentials: %v", err)
	}

	if !hasSecretEnvFrom(tmpl, "my-llm-secret") {
		t.Error("missing envFrom secretRef for my-llm-secret")
	}

	envs := getEnvVars(tmpl)
	if e, ok := findEnv(envs, "ANTHROPIC_MODEL"); !ok {
		t.Error("missing ANTHROPIC_MODEL")
	} else if e["value"] != "claude-opus-4-6" {
		t.Errorf("ANTHROPIC_MODEL = %q", e["value"])
	}

	if e, ok := findEnv(envs, "ANTHROPIC_BASE_URL"); !ok {
		t.Error("missing ANTHROPIC_BASE_URL")
	} else if e["value"] != "https://custom.api" {
		t.Errorf("ANTHROPIC_BASE_URL = %q", e["value"])
	}
}

func TestPatchLLMCredentials_Vertex(t *testing.T) {
	tmpl := emptyTemplate()
	llm := testLLMProvider(agenticv1alpha1.LLMProviderGoogleCloudVertex)

	if err := patchLLMCredentials(tmpl, llm, "claude-opus-4-6"); err != nil {
		t.Fatalf("patchLLMCredentials: %v", err)
	}

	if !hasSecretEnvFrom(tmpl, "my-llm-secret") {
		t.Error("missing envFrom secretRef for my-llm-secret")
	}

	envs := getEnvVars(tmpl)
	if e, ok := findEnv(envs, "CLAUDE_CODE_USE_VERTEX"); !ok {
		t.Error("missing CLAUDE_CODE_USE_VERTEX")
	} else if e["value"] != "1" {
		t.Errorf("CLAUDE_CODE_USE_VERTEX = %q", e["value"])
	}

	if e, ok := findEnv(envs, "GOOGLE_APPLICATION_CREDENTIALS"); !ok {
		t.Error("missing GOOGLE_APPLICATION_CREDENTIALS")
	} else if e["value"] != vertexCredsMountPath+"/"+vertexCredsFileName {
		t.Errorf("GOOGLE_APPLICATION_CREDENTIALS = %q", e["value"])
	}

	containers, _, _ := unstructured.NestedSlice(tmpl.Object, "spec", "podTemplate", "spec", "containers")
	container := containers[0].(map[string]any)
	mounts, _, _ := unstructured.NestedSlice(container, "volumeMounts")
	if len(mounts) != 1 {
		t.Fatalf("expected 1 volume mount, got %d", len(mounts))
	}
	mount := mounts[0].(map[string]any)
	if mount["name"] != llmCredsVolumeName || mount["mountPath"] != vertexCredsMountPath {
		t.Errorf("mount = %v", mount)
	}
}

func TestPatchLLMCredentials_Bedrock(t *testing.T) {
	tmpl := emptyTemplate()
	llm := testLLMProvider(agenticv1alpha1.LLMProviderAWSBedrock)

	if err := patchLLMCredentials(tmpl, llm, "claude-opus-4-6"); err != nil {
		t.Fatalf("patchLLMCredentials: %v", err)
	}

	envs := getEnvVars(tmpl)
	if e, ok := findEnv(envs, "CLAUDE_CODE_USE_BEDROCK"); !ok {
		t.Error("missing CLAUDE_CODE_USE_BEDROCK")
	} else if e["value"] != "1" {
		t.Errorf("CLAUDE_CODE_USE_BEDROCK = %q", e["value"])
	}
}

// --- patchRequiredSecrets tests ---

func TestPatchRequiredSecrets_EnvVar(t *testing.T) {
	tmpl := emptyTemplate()
	err := patchRequiredSecrets(tmpl, []agenticv1alpha1.SecretRequirement{
		{Name: "github-token", MountAs: agenticv1alpha1.SecretMountSpec{Type: agenticv1alpha1.SecretMountEnvVar, EnvVar: agenticv1alpha1.SecretMountEnvVarConfig{Name: "GH_TOKEN"}}},
	})
	if err != nil {
		t.Fatalf("patchRequiredSecrets: %v", err)
	}

	envs := getEnvVars(tmpl)
	e, ok := findEnv(envs, "GH_TOKEN")
	if !ok {
		t.Fatal("missing GH_TOKEN env var")
	}
	valueFrom, _ := e["valueFrom"].(map[string]any)
	secretRef, _ := valueFrom["secretKeyRef"].(map[string]any)
	if secretRef["name"] != "github-token" {
		t.Errorf("secretKeyRef.name = %q, want github-token", secretRef["name"])
	}
}

func TestPatchRequiredSecrets_FileMount(t *testing.T) {
	tmpl := emptyTemplate()
	err := patchRequiredSecrets(tmpl, []agenticv1alpha1.SecretRequirement{
		{Name: "tls-cert", MountAs: agenticv1alpha1.SecretMountSpec{Type: agenticv1alpha1.SecretMountFilePath, FilePath: agenticv1alpha1.SecretMountFilePathConfig{Path: "/etc/certs/tls.crt"}}},
	})
	if err != nil {
		t.Fatalf("patchRequiredSecrets: %v", err)
	}

	volumes, _, _ := unstructured.NestedSlice(tmpl.Object, "spec", "podTemplate", "spec", "volumes")
	if len(volumes) != 1 {
		t.Fatalf("expected 1 volume, got %d", len(volumes))
	}
	vol := volumes[0].(map[string]any)
	if vol["name"] != "req-tls-cert" {
		t.Errorf("volume name = %q, want req-tls-cert", vol["name"])
	}

	mounts := getVolumeMounts(tmpl)
	if len(mounts) != 1 {
		t.Fatalf("expected 1 mount, got %d", len(mounts))
	}
	if mounts[0]["mountPath"] != "/etc/certs/tls.crt" {
		t.Errorf("mountPath = %q", mounts[0]["mountPath"])
	}
}

// --- Error propagation tests ---

func TestSetEnvVar_FailsOnNoContainers(t *testing.T) {
	tmpl := &unstructured.Unstructured{
		Object: map[string]any{
			"spec": map[string]any{
				"podTemplate": map[string]any{
					"spec": map[string]any{},
				},
			},
		},
	}
	err := setEnvVar(tmpl, "FOO", "bar")
	if err == nil {
		t.Error("expected error when no containers exist")
	}
}

func TestEnsureAgentTemplate_NilAgent(t *testing.T) {
	_, err := EnsureAgentTemplate(nil, nil, "base", "ns", "analysis", nil, testLLMProvider(agenticv1alpha1.LLMProviderGoogleCloudVertex), nil)
	if err == nil {
		t.Error("expected error for nil agent")
	}
}

func TestEnsureAgentTemplate_NilLLM(t *testing.T) {
	_, err := EnsureAgentTemplate(nil, nil, "base", "ns", "analysis", testDefaultAgent(), nil, nil)
	if err == nil {
		t.Error("expected error for nil LLM")
	}
}

// --- upsertEnv tests ---

func TestUpsertEnv_UpdatesExisting(t *testing.T) {
	tmpl := emptyTemplate()
	if err := setEnvVar(tmpl, "MY_VAR", "old"); err != nil {
		t.Fatal(err)
	}
	if err := setEnvVar(tmpl, "MY_VAR", "new"); err != nil {
		t.Fatal(err)
	}

	envs := getEnvVars(tmpl)
	count := 0
	for _, e := range envs {
		if e["name"] == "MY_VAR" {
			count++
			if e["value"] != "new" {
				t.Errorf("value = %q, want new", e["value"])
			}
		}
	}
	if count != 1 {
		t.Errorf("MY_VAR appears %d times, want 1", count)
	}
}

func TestAddEnvFromSecret_Idempotent(t *testing.T) {
	tmpl := emptyTemplate()
	if err := addEnvFromSecret(tmpl, "my-secret"); err != nil {
		t.Fatal(err)
	}
	if err := addEnvFromSecret(tmpl, "my-secret"); err != nil {
		t.Fatal(err)
	}

	envFrom := getEnvFrom(tmpl)
	if len(envFrom) != 1 {
		t.Errorf("envFrom count = %d, want 1", len(envFrom))
	}
}

// --- patchSkillsPaths tests ---

func TestPatchSkillsPaths_SelectiveMounting(t *testing.T) {
	tmpl := templateWithSkillsMount()
	if err := patchSkillsPaths(tmpl, []string{
		"/skills/monitoring/prometheus",
		"/skills/cluster-update/update-advisor",
	}); err != nil {
		t.Fatal(err)
	}

	mounts := getVolumeMounts(tmpl)
	if len(mounts) != 4 {
		t.Fatalf("expected 4 volume mounts (2 non-skills + 2 subPath), got %d", len(mounts))
	}

	if mounts[0]["name"] != "tls" || mounts[1]["name"] != "home" {
		t.Errorf("non-skills mounts not preserved: %v, %v", mounts[0]["name"], mounts[1]["name"])
	}

	if mounts[2]["subPath"] != "skills/monitoring/prometheus" {
		t.Errorf("subPath = %q, want skills/monitoring/prometheus", mounts[2]["subPath"])
	}
	if mounts[2]["mountPath"] != "/app/skills/prometheus" {
		t.Errorf("mountPath = %q, want /app/skills/prometheus", mounts[2]["mountPath"])
	}
}

func TestPatchSkillsPaths_NoPaths_NoChange(t *testing.T) {
	tmpl := templateWithSkillsMount()
	before := len(getVolumeMounts(tmpl))
	if err := patchSkillsPaths(tmpl, nil); err != nil {
		t.Fatal(err)
	}
	if before != len(getVolumeMounts(tmpl)) {
		t.Error("nil paths should not change mounts")
	}
}

func TestPatchSkillsPaths_HashChangesWithPaths(t *testing.T) {
	llm := testLLMProvider(agenticv1alpha1.LLMProviderGoogleCloudVertex)
	noPaths := []agenticv1alpha1.SkillsSource{{Image: "img:latest"}}
	withPaths := []agenticv1alpha1.SkillsSource{{Image: "img:latest", Paths: []string{"/a", "/b"}}}

	h1 := mustHash(t, llm, "claude-opus-4-6", noPaths, nil, "analysis")
	h2 := mustHash(t, llm, "claude-opus-4-6", withPaths, nil, "analysis")

	if h1 == h2 {
		t.Error("hash should differ when paths are added")
	}
}

func TestComputeTemplateHash_DifferentBaseResourceVersion(t *testing.T) {
	llm := testLLMProvider(agenticv1alpha1.LLMProviderGoogleCloudVertex)
	skills := []agenticv1alpha1.SkillsSource{{Image: "quay.io/test/skills:latest"}}

	h1, err := computeTemplateHash(llm, "claude-opus-4-6", skills, nil, nil, "analysis", "1000")
	if err != nil {
		t.Fatal(err)
	}
	h2, err := computeTemplateHash(llm, "claude-opus-4-6", skills, nil, nil, "analysis", "2000")
	if err != nil {
		t.Fatal(err)
	}

	if h1 == h2 {
		t.Error("different base template resourceVersion should produce different hashes")
	}
}

func TestComputeTemplateHash_SameBaseResourceVersion(t *testing.T) {
	llm := testLLMProvider(agenticv1alpha1.LLMProviderGoogleCloudVertex)
	skills := []agenticv1alpha1.SkillsSource{{Image: "quay.io/test/skills:latest"}}

	h1, err := computeTemplateHash(llm, "claude-opus-4-6", skills, nil, nil, "analysis", "1000")
	if err != nil {
		t.Fatal(err)
	}
	h2, err := computeTemplateHash(llm, "claude-opus-4-6", skills, nil, nil, "analysis", "1000")
	if err != nil {
		t.Fatal(err)
	}

	if h1 != h2 {
		t.Error("same base template resourceVersion should produce same hash")
	}
}
