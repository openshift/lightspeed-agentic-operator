package agenticrun

import (
	"context"
	"encoding/json"
	"testing"

	"go.opentelemetry.io/otel/trace"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
	"github.com/openshift/lightspeed-agentic-operator/pkg/configuration"
)

func testBasePodSpec() *corev1.PodSpec {
	return &corev1.PodSpec{
		Containers: []corev1.Container{{Name: "agent", Image: "quay.io/test/agent:latest"}},
	}
}

func testLLMProvider(providerType agenticv1alpha1.LLMProviderType) *agenticv1alpha1.LLMProvider {
	creds := agenticv1alpha1.SecretReference{Name: "my-llm-secret"}
	spec := agenticv1alpha1.LLMProviderSpec{Type: providerType}
	switch providerType {
	case agenticv1alpha1.LLMProviderAnthropic:
		spec.Anthropic = agenticv1alpha1.AnthropicConfig{CredentialsSecret: creds}
	case agenticv1alpha1.LLMProviderGoogleCloudVertex:
		spec.GoogleCloudVertex = agenticv1alpha1.GoogleCloudVertexConfig{
			CredentialsSecret: creds,
			ProjectID:         "test-project",
			Region:            "us-central1",
			ModelProvider:     agenticv1alpha1.GoogleCloudVertexModelProviderAnthropic,
		}
	case agenticv1alpha1.LLMProviderOpenAI:
		spec.OpenAI = agenticv1alpha1.OpenAIConfig{CredentialsSecret: creds}
	case agenticv1alpha1.LLMProviderAzureOpenAI:
		spec.AzureOpenAI = agenticv1alpha1.AzureOpenAIConfig{
			CredentialsSecret: creds,
			Endpoint:          "https://test.openai.azure.com",
			APIVersion:        "2024-02-01",
		}
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

func envToMap(envVars []corev1.EnvVar) map[string]string {
	result := make(map[string]string)
	for _, ev := range envVars {
		result[ev.Name] = ev.Value
	}
	return result
}

func TestBuildSkills_Empty(t *testing.T) {
	b := &PodSpecBuilder{}
	vols, mounts := b.buildSkills(nil)
	if vols != nil || mounts != nil {
		t.Fatal("nil skills should return nil")
	}
	vols, mounts = b.buildSkills([]agenticv1alpha1.SkillsSource{})
	if vols != nil || mounts != nil {
		t.Fatal("empty skills should return nil")
	}
	vols, mounts = b.buildSkills([]agenticv1alpha1.SkillsSource{{Image: ""}})
	if vols != nil || mounts != nil {
		t.Fatal("skills with empty image should return nil")
	}
}

func TestBuildSkills_WithPaths(t *testing.T) {
	b := &PodSpecBuilder{}
	skills := []agenticv1alpha1.SkillsSource{{
		Image: "registry.example.com/skills:latest",
		Paths: []string{"/troubleshooting/network", "/analysis/logs"},
	}}

	vols, mounts := b.buildSkills(skills)
	if len(vols) != 2 {
		t.Fatalf("expected 2 volumes (image + workdir), got %d", len(vols))
	}
	if vols[0].Image == nil || vols[0].Image.Reference != "registry.example.com/skills:latest" {
		t.Errorf("first volume should be image volume, got %+v", vols[0])
	}
	if vols[1].EmptyDir == nil {
		t.Error("second volume should be emptyDir workdir")
	}

	// workdir mount + 2 path mounts
	if len(mounts) != 3 {
		t.Fatalf("expected 3 mounts, got %d", len(mounts))
	}
	if mounts[0].MountPath != "/app/skills/.agents" {
		t.Errorf("workdir mount path = %q", mounts[0].MountPath)
	}
	if mounts[1].MountPath != "/app/skills/network" || mounts[1].SubPath != "troubleshooting/network" {
		t.Errorf("skill mount[1] = %q subpath=%q", mounts[1].MountPath, mounts[1].SubPath)
	}
	if mounts[2].MountPath != "/app/skills/logs" || mounts[2].SubPath != "analysis/logs" {
		t.Errorf("skill mount[2] = %q subpath=%q", mounts[2].MountPath, mounts[2].SubPath)
	}
}

func TestBuildSkills_NoPaths(t *testing.T) {
	b := &PodSpecBuilder{}
	skills := []agenticv1alpha1.SkillsSource{{
		Image: "registry.example.com/skills:latest",
	}}

	vols, mounts := b.buildSkills(skills)
	if len(vols) != 2 {
		t.Fatalf("expected 2 volumes, got %d", len(vols))
	}
	if len(mounts) != 1 {
		t.Fatalf("expected 1 mount (workdir only), got %d", len(mounts))
	}
}

func TestBuildMCPServers_Empty(t *testing.T) {
	b := &PodSpecBuilder{}
	vols, mounts, envs, err := b.buildMCPServers(nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Even nil input produces the env var with an empty JSON array
	if len(envs) != 1 || envs[0].Name != mcpServersEnvVar {
		t.Fatal("expected LIGHTSPEED_MCP_SERVERS env var")
	}
	if len(vols) != 0 || len(mounts) != 0 {
		t.Error("no volumes or mounts expected for empty servers")
	}
}

func TestBuildMCPServers_WithSecretHeaders(t *testing.T) {
	b := &PodSpecBuilder{}
	servers := []agenticv1alpha1.MCPServerConfig{
		{
			Name:           "github",
			URL:            "https://mcp.github.com",
			TimeoutSeconds: 30,
			Headers: []agenticv1alpha1.MCPHeader{
				{
					Name: "Authorization",
					ValueFrom: agenticv1alpha1.MCPHeaderValueSource{
						Type:   agenticv1alpha1.MCPHeaderSourceTypeSecret,
						Secret: agenticv1alpha1.SecretReference{Name: "gh-token"},
					},
				},
			},
		},
		{
			Name: "internal",
			URL:  "https://internal.svc:8443",
			Headers: []agenticv1alpha1.MCPHeader{
				{
					Name: "X-Auth",
					ValueFrom: agenticv1alpha1.MCPHeaderValueSource{
						Type: agenticv1alpha1.MCPHeaderSourceTypeServiceAccountToken,
					},
				},
			},
		},
	}

	vols, mounts, envs, err := b.buildMCPServers(servers)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Only the Secret header produces a volume
	if len(vols) != 1 {
		t.Fatalf("expected 1 volume for secret header, got %d", len(vols))
	}
	if vols[0].Secret == nil || vols[0].Secret.SecretName != "gh-token" {
		t.Errorf("volume secret = %+v", vols[0])
	}
	if len(mounts) != 1 || mounts[0].MountPath != mcpHeadersMountRoot+"/gh-token" {
		t.Errorf("mount = %+v", mounts)
	}

	if len(envs) != 1 || envs[0].Name != mcpServersEnvVar {
		t.Fatal("expected LIGHTSPEED_MCP_SERVERS env")
	}

	var entries []mcpServerEnvEntry
	if err := json.Unmarshal([]byte(envs[0].Value), &entries); err != nil {
		t.Fatalf("unmarshal env: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}

	gh := entries[0]
	if gh.Name != "github" || gh.URL != "https://mcp.github.com" || gh.Timeout != 30 {
		t.Errorf("github entry = %+v", gh)
	}
	if len(gh.Headers) != 1 || gh.Headers[0].Source != "Secret" || gh.Headers[0].SecretName != "gh-token" {
		t.Errorf("github headers = %+v", gh.Headers)
	}

	internal := entries[1]
	if internal.Name != "internal" || internal.Timeout != 0 {
		t.Errorf("internal entry = %+v", internal)
	}
	if len(internal.Headers) != 1 || internal.Headers[0].Source != "ServiceAccountToken" {
		t.Errorf("internal headers = %+v", internal.Headers)
	}
}

func TestBuildRequiredSecrets_FilePath(t *testing.T) {
	b := &PodSpecBuilder{}
	secrets := []agenticv1alpha1.SecretRequirement{
		{
			Name: "tls-cert",
			MountAs: agenticv1alpha1.SecretMountSpec{
				Type: agenticv1alpha1.SecretMountFilePath,
				FilePath: agenticv1alpha1.SecretMountFilePathConfig{
					Path: "/etc/tls/server.crt",
				},
			},
		},
	}

	vols, mounts, envs := b.buildRequiredSecrets(secrets)
	if len(vols) != 1 || vols[0].Name != "req-tls-cert" {
		t.Fatalf("expected 1 volume named req-tls-cert, got %+v", vols)
	}
	if vols[0].Secret == nil || vols[0].Secret.SecretName != "tls-cert" {
		t.Errorf("volume secret = %+v", vols[0])
	}
	if len(mounts) != 1 || mounts[0].MountPath != "/etc/tls/server.crt" || !mounts[0].ReadOnly {
		t.Errorf("mount = %+v", mounts)
	}
	if len(envs) != 0 {
		t.Error("file path mount should produce no env vars")
	}
}

func TestBuildRequiredSecrets_EnvVar(t *testing.T) {
	b := &PodSpecBuilder{}
	secrets := []agenticv1alpha1.SecretRequirement{
		{
			Name: "api-key",
			MountAs: agenticv1alpha1.SecretMountSpec{
				Type: agenticv1alpha1.SecretMountEnvVar,
				EnvVar: agenticv1alpha1.SecretMountEnvVarConfig{
					Name: "API_KEY",
				},
			},
		},
	}

	vols, mounts, envs := b.buildRequiredSecrets(secrets)
	if len(vols) != 0 || len(mounts) != 0 {
		t.Error("env var mount should produce no volumes/mounts")
	}
	if len(envs) != 1 {
		t.Fatalf("expected 1 env var, got %d", len(envs))
	}
	if envs[0].Name != "API_KEY" {
		t.Errorf("env name = %q", envs[0].Name)
	}
	ref := envs[0].ValueFrom.SecretKeyRef
	if ref == nil || ref.Name != "api-key" || ref.Key != "token" {
		t.Errorf("env ref = %+v", ref)
	}
	if ref.Optional == nil || !*ref.Optional {
		t.Error("secret key ref should be optional")
	}
}

func TestBuildRequiredSecrets_Mixed(t *testing.T) {
	b := &PodSpecBuilder{}
	secrets := []agenticv1alpha1.SecretRequirement{
		{
			Name: "cert",
			MountAs: agenticv1alpha1.SecretMountSpec{
				Type:     agenticv1alpha1.SecretMountFilePath,
				FilePath: agenticv1alpha1.SecretMountFilePathConfig{Path: "/certs/ca.pem"},
			},
		},
		{
			Name: "token",
			MountAs: agenticv1alpha1.SecretMountSpec{
				Type:   agenticv1alpha1.SecretMountEnvVar,
				EnvVar: agenticv1alpha1.SecretMountEnvVarConfig{Name: "MY_TOKEN"},
			},
		},
	}

	vols, mounts, envs := b.buildRequiredSecrets(secrets)
	if len(vols) != 1 {
		t.Fatalf("expected 1 volume (file path only), got %d", len(vols))
	}
	if len(mounts) != 1 {
		t.Fatalf("expected 1 mount, got %d", len(mounts))
	}
	if len(envs) != 1 {
		t.Fatalf("expected 1 env, got %d", len(envs))
	}
}

func TestBuildRequiredSecrets_Empty(t *testing.T) {
	b := &PodSpecBuilder{}
	vols, mounts, envs := b.buildRequiredSecrets(nil)
	if len(vols) != 0 || len(mounts) != 0 || len(envs) != 0 {
		t.Error("nil secrets should return empty slices")
	}
}

// --- Build integration tests ---

func TestBuild_NilBase(t *testing.T) {
	b := &PodSpecBuilder{}
	agent := &agenticv1alpha1.Agent{Spec: agenticv1alpha1.AgentSpec{Model: "m"}}
	llm := testLLMProvider(agenticv1alpha1.LLMProviderAnthropic)
	_, err := b.Build(nil, agent, llm, nil, nil, nil, "analysis", "uid", "sa", "uid-test-run", "", 0)
	if err == nil {
		t.Fatal("expected error for nil base")
	}
}

func TestBuild_NilAgent(t *testing.T) {
	b := &PodSpecBuilder{}
	llm := testLLMProvider(agenticv1alpha1.LLMProviderAnthropic)
	_, err := b.Build(testBasePodSpec(), nil, llm, nil, nil, nil, "analysis", "uid", "sa", "uid-test-run", "", 0)
	if err == nil {
		t.Fatal("expected error for nil agent")
	}
}

func TestBuild_NilLLM(t *testing.T) {
	b := &PodSpecBuilder{}
	agent := &agenticv1alpha1.Agent{Spec: agenticv1alpha1.AgentSpec{Model: "m"}}
	_, err := b.Build(testBasePodSpec(), agent, nil, nil, nil, nil, "analysis", "uid", "sa", "uid-test-run", "", 0)
	if err == nil {
		t.Fatal("expected error for nil LLM")
	}
}

func TestBuild_EmptySA(t *testing.T) {
	b := &PodSpecBuilder{}
	agent := &agenticv1alpha1.Agent{Spec: agenticv1alpha1.AgentSpec{Model: "m"}}
	llm := testLLMProvider(agenticv1alpha1.LLMProviderAnthropic)
	_, err := b.Build(testBasePodSpec(), agent, llm, nil, nil, nil, "analysis", "uid", "", "uid-test-run", "", 0)
	if err == nil {
		t.Fatal("expected error for empty serviceAccount")
	}
}

func TestBuild_Anthropic(t *testing.T) {
	b := &PodSpecBuilder{}
	agent := &agenticv1alpha1.Agent{Spec: agenticv1alpha1.AgentSpec{Model: "claude-opus-4-6"}}
	llm := testLLMProviderWithURL(agenticv1alpha1.LLMProviderAnthropic, "https://custom.api")

	ps, err := b.Build(testBasePodSpec(), agent, llm, nil, nil, nil, "analysis", "uid-123", "my-sa", "uid-test-run", "", 0)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if ps.ServiceAccountName != "my-sa" {
		t.Errorf("SA = %q", ps.ServiceAccountName)
	}
	env := envToMap(ps.Containers[0].Env)
	if env["LIGHTSPEED_PROVIDER"] != "anthropic" {
		t.Errorf("PROVIDER = %q", env["LIGHTSPEED_PROVIDER"])
	}
	if env["LIGHTSPEED_MODEL"] != "claude-opus-4-6" {
		t.Errorf("MODEL = %q", env["LIGHTSPEED_MODEL"])
	}
	if env["LIGHTSPEED_PROVIDER_URL"] != "https://custom.api" {
		t.Errorf("URL = %q", env["LIGHTSPEED_PROVIDER_URL"])
	}
	if ps.Containers[0].ReadinessProbe != nil || ps.Containers[0].LivenessProbe != nil {
		t.Error("HTTP probes must not be set for batch sandboxes")
	}
	assertInputConfigMapMount(t, ps, "uid-test-run")
}

func TestBuild_RequiresInputConfigMapName(t *testing.T) {
	b := &PodSpecBuilder{}
	agent := &agenticv1alpha1.Agent{Spec: agenticv1alpha1.AgentSpec{Model: "m"}}
	llm := testLLMProvider(agenticv1alpha1.LLMProviderAnthropic)
	_, err := b.Build(testBasePodSpec(), agent, llm, nil, nil, nil, "analysis", "uid", "sa", "", "", 0)
	if err == nil {
		t.Fatal("expected error for empty inputConfigMapName")
	}
}

func TestBuild_InputConfigMapMount(t *testing.T) {
	b := &PodSpecBuilder{}
	agent := &agenticv1alpha1.Agent{Spec: agenticv1alpha1.AgentSpec{Model: "m"}}
	llm := testLLMProvider(agenticv1alpha1.LLMProviderAnthropic)
	const cmName = "uid-my-run"

	ps, err := b.Build(testBasePodSpec(), agent, llm, nil, nil, nil, "analysis", "uid", "sa", cmName, "", 0)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	assertInputConfigMapMount(t, ps, cmName)
	if ps.Containers[0].ReadinessProbe != nil || ps.Containers[0].LivenessProbe != nil {
		t.Error("HTTP probes must not be set")
	}
}

func TestBuild_Traceparent(t *testing.T) {
	b := &PodSpecBuilder{}
	agent := &agenticv1alpha1.Agent{Spec: agenticv1alpha1.AgentSpec{Model: "m"}}
	llm := testLLMProvider(agenticv1alpha1.LLMProviderAnthropic)
	const tp = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"

	ps, err := b.Build(testBasePodSpec(), agent, llm, nil, nil, nil, "analysis", "uid", "sa", "uid-test-run", tp, 0)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	env := envToMap(ps.Containers[0].Env)
	if env["TRACEPARENT"] != tp {
		t.Fatalf("TRACEPARENT = %q, want %q", env["TRACEPARENT"], tp)
	}

	ps, err = b.Build(testBasePodSpec(), agent, llm, nil, nil, nil, "analysis", "uid", "sa", "uid-test-run", "", 0)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	env = envToMap(ps.Containers[0].Env)
	if _, ok := env["TRACEPARENT"]; ok {
		t.Fatalf("TRACEPARENT should be omitted when empty, got %q", env["TRACEPARENT"])
	}
}

func assertInputConfigMapMount(t *testing.T, ps *corev1.PodSpec, cmName string) {
	t.Helper()
	var foundVol bool
	for _, v := range ps.Volumes {
		if v.Name != inputConfigMapVolumeName {
			continue
		}
		foundVol = true
		if v.ConfigMap == nil || v.ConfigMap.Name != cmName {
			t.Errorf("input volume ConfigMap = %+v, want name %q", v.ConfigMap, cmName)
		}
	}
	if !foundVol {
		t.Errorf("missing volume %q", inputConfigMapVolumeName)
	}
	var foundMount bool
	for _, m := range ps.Containers[0].VolumeMounts {
		if m.Name != inputConfigMapVolumeName {
			continue
		}
		foundMount = true
		if m.MountPath != inputConfigMapMountPath {
			t.Errorf("input mount path = %q, want %q", m.MountPath, inputConfigMapMountPath)
		}
		if !m.ReadOnly {
			t.Error("input mount must be read-only")
		}
	}
	if !foundMount {
		t.Errorf("missing volume mount %q", inputConfigMapVolumeName)
	}
}

func TestBuild_Vertex(t *testing.T) {
	b := &PodSpecBuilder{}
	agent := &agenticv1alpha1.Agent{Spec: agenticv1alpha1.AgentSpec{Model: "gemini-2.0"}}
	llm := testLLMProvider(agenticv1alpha1.LLMProviderGoogleCloudVertex)

	ps, err := b.Build(testBasePodSpec(), agent, llm, nil, nil, nil, "analysis", "uid", "sa", "uid-test-run", "", 0)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	env := envToMap(ps.Containers[0].Env)
	if env["LIGHTSPEED_PROVIDER"] != "vertex" {
		t.Errorf("PROVIDER = %q", env["LIGHTSPEED_PROVIDER"])
	}
	if env["LIGHTSPEED_MODEL_PROVIDER"] != "anthropic" {
		t.Errorf("MODEL_PROVIDER = %q", env["LIGHTSPEED_MODEL_PROVIDER"])
	}
	if env["LIGHTSPEED_PROVIDER_PROJECT"] != "test-project" {
		t.Errorf("PROJECT = %q", env["LIGHTSPEED_PROVIDER_PROJECT"])
	}
	if env["LIGHTSPEED_PROVIDER_REGION"] != "us-central1" {
		t.Errorf("REGION = %q", env["LIGHTSPEED_PROVIDER_REGION"])
	}
}

func TestBuild_Azure(t *testing.T) {
	b := &PodSpecBuilder{}
	agent := &agenticv1alpha1.Agent{Spec: agenticv1alpha1.AgentSpec{Model: "gpt-4o"}}
	llm := testLLMProvider(agenticv1alpha1.LLMProviderAzureOpenAI)

	ps, err := b.Build(testBasePodSpec(), agent, llm, nil, nil, nil, "analysis", "uid", "sa", "uid-test-run", "", 0)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	env := envToMap(ps.Containers[0].Env)
	if env["LIGHTSPEED_PROVIDER"] != "azure" {
		t.Errorf("PROVIDER = %q", env["LIGHTSPEED_PROVIDER"])
	}
	if env["LIGHTSPEED_PROVIDER_URL"] != "https://test.openai.azure.com" {
		t.Errorf("URL = %q", env["LIGHTSPEED_PROVIDER_URL"])
	}
	if env["LIGHTSPEED_PROVIDER_API_VERSION"] != "2024-02-01" {
		t.Errorf("API_VERSION = %q", env["LIGHTSPEED_PROVIDER_API_VERSION"])
	}
}

func TestBuild_Bedrock(t *testing.T) {
	b := &PodSpecBuilder{}
	agent := &agenticv1alpha1.Agent{Spec: agenticv1alpha1.AgentSpec{Model: "claude-v3"}}
	llm := testLLMProvider(agenticv1alpha1.LLMProviderAWSBedrock)

	ps, err := b.Build(testBasePodSpec(), agent, llm, nil, nil, nil, "analysis", "uid", "sa", "uid-test-run", "", 0)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	env := envToMap(ps.Containers[0].Env)
	if env["LIGHTSPEED_PROVIDER"] != "bedrock" {
		t.Errorf("PROVIDER = %q", env["LIGHTSPEED_PROVIDER"])
	}
	if env["LIGHTSPEED_PROVIDER_REGION"] != "us-east-1" {
		t.Errorf("REGION = %q", env["LIGHTSPEED_PROVIDER_REGION"])
	}
}

func TestBuild_OpenAI(t *testing.T) {
	b := &PodSpecBuilder{}
	agent := &agenticv1alpha1.Agent{Spec: agenticv1alpha1.AgentSpec{Model: "gpt-4o"}}
	llm := testLLMProviderWithURL(agenticv1alpha1.LLMProviderOpenAI, "https://api.example.com")

	ps, err := b.Build(testBasePodSpec(), agent, llm, nil, nil, nil, "analysis", "uid", "sa", "uid-test-run", "", 0)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	env := envToMap(ps.Containers[0].Env)
	if env["LIGHTSPEED_PROVIDER"] != "openai" {
		t.Errorf("PROVIDER = %q", env["LIGHTSPEED_PROVIDER"])
	}
	if env["LIGHTSPEED_PROVIDER_URL"] != "https://api.example.com" {
		t.Errorf("URL = %q", env["LIGHTSPEED_PROVIDER_URL"])
	}
}

func TestBuild_ReasoningConfig(t *testing.T) {
	b := &PodSpecBuilder{}
	agent := &agenticv1alpha1.Agent{
		Spec: agenticv1alpha1.AgentSpec{
			Model: "claude-opus-4-6",
			ReasoningConfig: map[string]apiextensionsv1.JSON{
				"thinking": {Raw: []byte(`"enabled"`)},
				"budget":   {Raw: []byte(`4096`)},
			},
		},
	}
	llm := testLLMProvider(agenticv1alpha1.LLMProviderAnthropic)

	ps, err := b.Build(testBasePodSpec(), agent, llm, nil, nil, nil, "analysis", "uid", "sa", "uid-test-run", "", 0)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	env := envToMap(ps.Containers[0].Env)
	raw, ok := env["LIGHTSPEED_REASONING_CONFIG"]
	if !ok {
		t.Fatal("LIGHTSPEED_REASONING_CONFIG not set")
	}
	var parsed map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		t.Fatalf("not valid JSON: %v", err)
	}
	if string(parsed["thinking"]) != `"enabled"` {
		t.Errorf("thinking = %s", parsed["thinking"])
	}
}

func TestBuild_NoReasoningConfig(t *testing.T) {
	b := &PodSpecBuilder{}
	agent := &agenticv1alpha1.Agent{Spec: agenticv1alpha1.AgentSpec{Model: "m"}}
	llm := testLLMProvider(agenticv1alpha1.LLMProviderAnthropic)

	ps, err := b.Build(testBasePodSpec(), agent, llm, nil, nil, nil, "analysis", "uid", "sa", "uid-test-run", "", 0)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	env := envToMap(ps.Containers[0].Env)
	if _, ok := env["LIGHTSPEED_REASONING_CONFIG"]; ok {
		t.Error("LIGHTSPEED_REASONING_CONFIG should not be set when absent")
	}
}

func TestBuild_CredentialsSecretMounted(t *testing.T) {
	b := &PodSpecBuilder{}
	agent := &agenticv1alpha1.Agent{Spec: agenticv1alpha1.AgentSpec{Model: "m"}}
	llm := testLLMProvider(agenticv1alpha1.LLMProviderAnthropic)

	ps, err := b.Build(testBasePodSpec(), agent, llm, nil, nil, nil, "analysis", "uid", "sa", "uid-test-run", "", 0)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	// EnvFrom should reference the credentials secret
	found := false
	for _, ef := range ps.Containers[0].EnvFrom {
		if ef.SecretRef != nil && ef.SecretRef.Name == "my-llm-secret" {
			found = true
		}
	}
	if !found {
		t.Error("credentials secret not in envFrom")
	}

	// Volume mount at llmCredsMountPath
	foundMount := false
	for _, m := range ps.Containers[0].VolumeMounts {
		if m.Name == llmCredsVolumeName && m.MountPath == llmCredsMountPath && m.ReadOnly {
			foundMount = true
		}
	}
	if !foundMount {
		t.Errorf("credentials volume not mounted at %s", llmCredsMountPath)
	}
}

func TestBuild_RHOKPEndpointAndCA(t *testing.T) {
	b := &PodSpecBuilder{}
	agent := &agenticv1alpha1.Agent{Spec: agenticv1alpha1.AgentSpec{Model: "m"}}
	llm := testLLMProvider(agenticv1alpha1.LLMProviderAnthropic)
	rhokp := &configuration.RHOKPConfig{
		Endpoint:     "https://lightspeed-rhokp.ns.svc:8443",
		CASecretName: "lightspeed-agentic-rhokp-ca",
	}

	ps, err := b.Build(testBasePodSpec(), agent, llm, nil, nil, rhokp, "analysis", "uid", "sa", "uid-test-run", "", 0)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	foundEndpoint := false
	foundCA := false
	for _, e := range ps.Containers[0].Env {
		if e.Name == "LIGHTSPEED_RHOKP_ENDPOINT" && e.Value == rhokp.Endpoint {
			foundEndpoint = true
		}
		if e.Name == "LIGHTSPEED_RHOKP_CA_CERTIFICATE" && e.Value == rhokpCAMountPath+"/"+rhokpCASecretKey {
			foundCA = true
		}
	}
	if !foundEndpoint {
		t.Error("LIGHTSPEED_RHOKP_ENDPOINT env var not set")
	}
	if !foundCA {
		t.Error("LIGHTSPEED_RHOKP_CA_CERTIFICATE env var not set")
	}

	foundVolume := false
	for _, v := range ps.Volumes {
		if v.Name == rhokpCAVolumeName && v.Secret != nil && v.Secret.SecretName == rhokp.CASecretName {
			foundVolume = true
		}
	}
	if !foundVolume {
		t.Error("RHOKP CA volume not added")
	}

	foundMount := false
	for _, m := range ps.Containers[0].VolumeMounts {
		if m.Name == rhokpCAVolumeName && m.MountPath == rhokpCAMountPath && m.ReadOnly {
			foundMount = true
		}
	}
	if !foundMount {
		t.Errorf("RHOKP CA volume not mounted at %s", rhokpCAMountPath)
	}
}

func TestBuild_RHOKPOmitted(t *testing.T) {
	b := &PodSpecBuilder{}
	agent := &agenticv1alpha1.Agent{Spec: agenticv1alpha1.AgentSpec{Model: "m"}}
	llm := testLLMProvider(agenticv1alpha1.LLMProviderAnthropic)

	ps, err := b.Build(testBasePodSpec(), agent, llm, nil, nil, nil, "analysis", "uid", "sa", "uid-test-run", "", 0)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	for _, e := range ps.Containers[0].Env {
		if e.Name == "LIGHTSPEED_RHOKP_ENDPOINT" {
			t.Error("LIGHTSPEED_RHOKP_ENDPOINT should not be set when RHOKP is nil")
		}
	}
	for _, v := range ps.Volumes {
		if v.Name == rhokpCAVolumeName {
			t.Error("RHOKP CA volume should not be present when RHOKP is nil")
		}
	}
}

func TestBuild_DeduplicatesVolumes(t *testing.T) {
	base := &corev1.PodSpec{
		Containers: []corev1.Container{{
			Name:  "agent",
			Image: "quay.io/test/agent:latest",
			VolumeMounts: []corev1.VolumeMount{
				{Name: "home", MountPath: "/home/agent"},
				{Name: "skills-workdir", MountPath: "/app/skills/.agents"},
			},
		}},
		Volumes: []corev1.Volume{
			{Name: "home", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
			{Name: "skills-workdir", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		},
	}

	b := &PodSpecBuilder{}
	agent := &agenticv1alpha1.Agent{Spec: agenticv1alpha1.AgentSpec{Model: "m"}}
	llm := testLLMProvider(agenticv1alpha1.LLMProviderOpenAI)
	tools := &agenticv1alpha1.ToolsSpec{
		Skills: []agenticv1alpha1.SkillsSource{{
			Image: "quay.io/test/skills:latest",
			Paths: []string{"/network-diagnostics"},
		}},
	}

	ps, err := b.Build(base, agent, llm, tools, nil, nil, "analysis", "uid", "sa", "uid-test-run", "", 0)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	// No duplicate volumes by name.
	volCounts := map[string]int{}
	for _, v := range ps.Volumes {
		volCounts[v.Name]++
	}
	for name, count := range volCounts {
		if count > 1 {
			t.Errorf("duplicate volume %q (count=%d)", name, count)
		}
	}

	// No duplicate mounts by mountPath.
	mountCounts := map[string]int{}
	for _, m := range ps.Containers[0].VolumeMounts {
		mountCounts[m.MountPath]++
	}
	for mp, count := range mountCounts {
		if count > 1 {
			t.Errorf("duplicate mount path %q (count=%d)", mp, count)
		}
	}

	// Exactly one home and skills-workdir volume, both emptyDir.
	for _, name := range []string{"home", "skills-workdir"} {
		if volCounts[name] != 1 {
			t.Errorf("expected exactly 1 %q volume, got %d", name, volCounts[name])
		}
	}

	// Generated skills image volume present with correct image.
	if volCounts["skills"] != 1 {
		t.Fatalf("expected exactly 1 skills volume, got %d", volCounts["skills"])
	}
	for _, v := range ps.Volumes {
		if v.Name == "skills" {
			if v.VolumeSource.Image == nil || v.VolumeSource.Image.Reference != "quay.io/test/skills:latest" {
				t.Errorf("skills volume has wrong image: %+v", v.VolumeSource)
			}
		}
	}

	// Generated skill mount with SubPath present.
	foundSkillMount := false
	for _, m := range ps.Containers[0].VolumeMounts {
		if m.Name == "skills" && m.SubPath == "network-diagnostics" {
			foundSkillMount = true
		}
	}
	if !foundSkillMount {
		t.Error("expected skills mount with SubPath network-diagnostics")
	}
}

func TestBuild_GeneratedVolumeOverridesBase(t *testing.T) {
	base := &corev1.PodSpec{
		Containers: []corev1.Container{{
			Name:  "agent",
			Image: "quay.io/test/agent:latest",
			VolumeMounts: []corev1.VolumeMount{
				{Name: "skills-workdir", MountPath: "/app/skills/.agents"},
			},
		}},
		Volumes: []corev1.Volume{
			{Name: "skills", VolumeSource: corev1.VolumeSource{
				Image: &corev1.ImageVolumeSource{Reference: "placeholder:latest"},
			}},
			{Name: "skills-workdir", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		},
	}

	b := &PodSpecBuilder{}
	agent := &agenticv1alpha1.Agent{Spec: agenticv1alpha1.AgentSpec{Model: "m"}}
	llm := testLLMProvider(agenticv1alpha1.LLMProviderOpenAI)
	tools := &agenticv1alpha1.ToolsSpec{
		Skills: []agenticv1alpha1.SkillsSource{{Image: "quay.io/real/skills:v1"}},
	}

	ps, err := b.Build(base, agent, llm, tools, nil, nil, "analysis", "uid", "sa", "uid-test-run", "", 0)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	// Exactly one skills volume, with the generated image (not placeholder).
	skillsCount := 0
	for _, v := range ps.Volumes {
		if v.Name == "skills" {
			skillsCount++
			if v.VolumeSource.Image == nil || v.VolumeSource.Image.Reference != "quay.io/real/skills:v1" {
				t.Errorf("skills volume should be overridden by generated entry, got: %+v", v.VolumeSource)
			}
		}
	}
	if skillsCount != 1 {
		t.Errorf("expected exactly 1 skills volume, got %d", skillsCount)
	}
}

func TestTraceparentFromContext_NoSpan(t *testing.T) {
	if got := traceparentFromContext(context.Background()); got != "" {
		t.Fatalf("expected empty traceparent, got %q", got)
	}
}

func TestTraceparentFromContext_WithSpan(t *testing.T) {
	traceID := trace.TraceID{0x4b, 0xf9, 0x2f, 0x35, 0x77, 0xb3, 0x4d, 0xa6, 0xa3, 0xce, 0x92, 0x9d, 0x0e, 0x0e, 0x47, 0x36}
	spanID := trace.SpanID{0x00, 0xf0, 0x67, 0xaa, 0x0b, 0xa9, 0x02, 0xb7}
	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    traceID,
		SpanID:     spanID,
		TraceFlags: trace.FlagsSampled,
	})
	ctx := trace.ContextWithSpanContext(context.Background(), sc)

	want := "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	if got := traceparentFromContext(ctx); got != want {
		t.Fatalf("traceparent = %q, want %q", got, want)
	}
}

func TestBuild_AgentTimeoutSeconds(t *testing.T) {
	b := &PodSpecBuilder{}
	agent := &agenticv1alpha1.Agent{Spec: agenticv1alpha1.AgentSpec{Model: "m"}}
	llm := testLLMProvider(agenticv1alpha1.LLMProviderAnthropic)

	ps, err := b.Build(testBasePodSpec(), agent, llm, nil, nil, nil, "analysis", "uid", "sa", "uid-test-run", "", 1800)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	env := envToMap(ps.Containers[0].Env)
	if env["LIGHTSPEED_AGENT_TIMEOUT_SECONDS"] != "1800" {
		t.Errorf("LIGHTSPEED_AGENT_TIMEOUT_SECONDS = %q, want %q", env["LIGHTSPEED_AGENT_TIMEOUT_SECONDS"], "1800")
	}
}

func TestBuild_AgentTimeoutSecondsOmittedWhenZero(t *testing.T) {
	b := &PodSpecBuilder{}
	agent := &agenticv1alpha1.Agent{Spec: agenticv1alpha1.AgentSpec{Model: "m"}}
	llm := testLLMProvider(agenticv1alpha1.LLMProviderAnthropic)

	ps, err := b.Build(testBasePodSpec(), agent, llm, nil, nil, nil, "analysis", "uid", "sa", "uid-test-run", "", 0)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	env := envToMap(ps.Containers[0].Env)
	if _, ok := env["LIGHTSPEED_AGENT_TIMEOUT_SECONDS"]; ok {
		t.Error("LIGHTSPEED_AGENT_TIMEOUT_SECONDS should be omitted when agentTimeoutSeconds is 0")
	}
}

func TestBuild_MaxTurns(t *testing.T) {
	b := &PodSpecBuilder{}
	agent := &agenticv1alpha1.Agent{Spec: agenticv1alpha1.AgentSpec{Model: "m", MaxTurns: 200}}
	llm := testLLMProvider(agenticv1alpha1.LLMProviderAnthropic)

	ps, err := b.Build(testBasePodSpec(), agent, llm, nil, nil, nil, "analysis", "uid", "sa", "uid-test-run", "", 600)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	env := envToMap(ps.Containers[0].Env)
	if env["LIGHTSPEED_AGENT_MAX_TURNS"] != "200" {
		t.Errorf("LIGHTSPEED_AGENT_MAX_TURNS = %q, want %q", env["LIGHTSPEED_AGENT_MAX_TURNS"], "200")
	}
}

func TestBuild_MaxTurnsOmittedWhenZero(t *testing.T) {
	b := &PodSpecBuilder{}
	agent := &agenticv1alpha1.Agent{Spec: agenticv1alpha1.AgentSpec{Model: "m"}}
	llm := testLLMProvider(agenticv1alpha1.LLMProviderAnthropic)

	ps, err := b.Build(testBasePodSpec(), agent, llm, nil, nil, nil, "analysis", "uid", "sa", "uid-test-run", "", 0)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	env := envToMap(ps.Containers[0].Env)
	if _, ok := env["LIGHTSPEED_AGENT_MAX_TURNS"]; ok {
		t.Error("LIGHTSPEED_AGENT_MAX_TURNS should be omitted when maxTurns is 0")
	}
}
