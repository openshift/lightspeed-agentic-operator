package proposal

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
)

// envToMap converts []corev1.EnvVar to map[string]string for easy assertion
func envToMap(envVars []corev1.EnvVar) map[string]string {
	result := make(map[string]string)
	for _, ev := range envVars {
		result[ev.Name] = ev.Value
	}
	return result
}

func intstr8080() intstr.IntOrString {
	return intstr.FromInt32(8080)
}

func TestPodSpecBuilder_Anthropic(t *testing.T) {
	builder := PodSpecBuilder{Image: "quay.io/lightspeed/agent:latest"}
	agent := &agenticv1alpha1.Agent{
		Spec: agenticv1alpha1.AgentSpec{
			Model: "claude-opus-4-6",
		},
	}
	llm := testLLMProviderWithURL(agenticv1alpha1.LLMProviderAnthropic, "https://custom.api")

	podSpec, err := builder.Build(agent, llm, nil, "analysis", defaultSandboxSA)
	if err != nil {
		t.Fatalf("Build failed: %v", err)
	}

	// Verify container basics
	if len(podSpec.Containers) != 1 {
		t.Fatalf("expected 1 container, got %d", len(podSpec.Containers))
	}
	container := podSpec.Containers[0]
	if container.Name != "agent" {
		t.Errorf("container name = %q, want agent", container.Name)
	}
	if container.Image != "quay.io/lightspeed/agent:latest" {
		t.Errorf("container image = %q", container.Image)
	}

	// Verify port
	if len(container.Ports) != 1 {
		t.Fatalf("expected 1 port, got %d", len(container.Ports))
	}
	if container.Ports[0].Name != "http" {
		t.Errorf("port name = %q, want http", container.Ports[0].Name)
	}
	if container.Ports[0].ContainerPort != 8080 {
		t.Errorf("port = %d, want 8080", container.Ports[0].ContainerPort)
	}
	if container.Ports[0].Protocol != corev1.ProtocolTCP {
		t.Errorf("protocol = %q, want TCP", container.Ports[0].Protocol)
	}

	// Verify env vars
	envMap := envToMap(container.Env)
	if envMap["LIGHTSPEED_PROVIDER"] != "anthropic" {
		t.Errorf("LIGHTSPEED_PROVIDER = %q, want anthropic", envMap["LIGHTSPEED_PROVIDER"])
	}
	if envMap["LIGHTSPEED_MODEL"] != "claude-opus-4-6" {
		t.Errorf("LIGHTSPEED_MODEL = %q", envMap["LIGHTSPEED_MODEL"])
	}
	if envMap["LIGHTSPEED_PROVIDER_URL"] != "https://custom.api" {
		t.Errorf("LIGHTSPEED_PROVIDER_URL = %q", envMap["LIGHTSPEED_PROVIDER_URL"])
	}

	// Verify envFrom
	if len(container.EnvFrom) != 1 {
		t.Fatalf("expected 1 envFrom, got %d", len(container.EnvFrom))
	}
	if container.EnvFrom[0].SecretRef == nil {
		t.Fatal("envFrom secretRef is nil")
	}
	if container.EnvFrom[0].SecretRef.Name != "my-llm-secret" {
		t.Errorf("secretRef name = %q, want my-llm-secret", container.EnvFrom[0].SecretRef.Name)
	}

	// Verify volume mount
	foundMount := false
	for _, m := range container.VolumeMounts {
		if m.Name == llmCredsVolumeName && m.MountPath == llmCredsMountPath {
			foundMount = true
			if !m.ReadOnly {
				t.Error("credential volume mount should be readOnly")
			}
			break
		}
	}
	if !foundMount {
		t.Errorf("missing credential volume mount at %s", llmCredsMountPath)
	}

	// Verify volume
	foundVolume := false
	for _, v := range podSpec.Volumes {
		if v.Name == llmCredsVolumeName {
			foundVolume = true
			if v.Secret == nil {
				t.Fatal("volume secret source is nil")
			}
			if v.Secret.SecretName != "my-llm-secret" {
				t.Errorf("secret name = %q, want my-llm-secret", v.Secret.SecretName)
			}
			break
		}
	}
	if !foundVolume {
		t.Error("missing llm-credentials volume")
	}

	// Verify readiness probe
	if container.ReadinessProbe == nil {
		t.Fatal("readinessProbe is nil")
	}
	if container.ReadinessProbe.HTTPGet == nil {
		t.Fatal("readinessProbe.HTTPGet is nil")
	}
	if container.ReadinessProbe.HTTPGet.Path != "/ready" {
		t.Errorf("readinessProbe path = %q, want /ready", container.ReadinessProbe.HTTPGet.Path)
	}
	if container.ReadinessProbe.HTTPGet.Port != intstr8080() {
		t.Errorf("readinessProbe port = %v, want 8080", container.ReadinessProbe.HTTPGet.Port)
	}
	if container.ReadinessProbe.InitialDelaySeconds != 3 {
		t.Errorf("readinessProbe initialDelay = %d, want 3", container.ReadinessProbe.InitialDelaySeconds)
	}
	if container.ReadinessProbe.PeriodSeconds != 10 {
		t.Errorf("readinessProbe period = %d, want 10", container.ReadinessProbe.PeriodSeconds)
	}
	if container.ReadinessProbe.FailureThreshold != 3 {
		t.Errorf("readinessProbe failure = %d, want 3", container.ReadinessProbe.FailureThreshold)
	}

	// Verify liveness probe
	if container.LivenessProbe == nil {
		t.Fatal("livenessProbe is nil")
	}
	if container.LivenessProbe.HTTPGet == nil {
		t.Fatal("livenessProbe.HTTPGet is nil")
	}
	if container.LivenessProbe.HTTPGet.Path != "/health" {
		t.Errorf("livenessProbe path = %q, want /health", container.LivenessProbe.HTTPGet.Path)
	}
	if container.LivenessProbe.HTTPGet.Port != intstr8080() {
		t.Errorf("livenessProbe port = %v, want 8080", container.LivenessProbe.HTTPGet.Port)
	}
	if container.LivenessProbe.InitialDelaySeconds != 10 {
		t.Errorf("livenessProbe initialDelay = %d, want 10", container.LivenessProbe.InitialDelaySeconds)
	}
	if container.LivenessProbe.PeriodSeconds != 30 {
		t.Errorf("livenessProbe period = %d, want 30", container.LivenessProbe.PeriodSeconds)
	}
	if container.LivenessProbe.FailureThreshold != 3 {
		t.Errorf("livenessProbe failure = %d, want 3", container.LivenessProbe.FailureThreshold)
	}

	// Verify security context
	if container.SecurityContext == nil {
		t.Fatal("securityContext is nil")
	}
	if container.SecurityContext.AllowPrivilegeEscalation == nil || *container.SecurityContext.AllowPrivilegeEscalation {
		t.Error("allowPrivilegeEscalation should be false")
	}
	if container.SecurityContext.Capabilities == nil {
		t.Fatal("capabilities is nil")
	}
	if len(container.SecurityContext.Capabilities.Drop) != 1 || container.SecurityContext.Capabilities.Drop[0] != "ALL" {
		t.Errorf("capabilities.drop = %v, want [ALL]", container.SecurityContext.Capabilities.Drop)
	}

	// Verify service account
	if podSpec.ServiceAccountName != defaultSandboxSA {
		t.Errorf("serviceAccountName = %q, want %q", podSpec.ServiceAccountName, defaultSandboxSA)
	}
	if podSpec.AutomountServiceAccountToken == nil || *podSpec.AutomountServiceAccountToken {
		t.Error("automountServiceAccountToken should be false")
	}
}

func TestPodSpecBuilder_Vertex(t *testing.T) {
	builder := PodSpecBuilder{Image: "quay.io/lightspeed/agent:latest"}
	agent := &agenticv1alpha1.Agent{Spec: agenticv1alpha1.AgentSpec{Model: "gemini-2.0-flash-exp"}}
	llm := testLLMProvider(agenticv1alpha1.LLMProviderGoogleCloudVertex)

	podSpec, err := builder.Build(agent, llm, nil, "analysis", defaultSandboxSA)
	if err != nil {
		t.Fatalf("Build failed: %v", err)
	}

	container := podSpec.Containers[0]
	envMap := envToMap(container.Env)
	if envMap["LIGHTSPEED_PROVIDER"] != "vertex" {
		t.Errorf("LIGHTSPEED_PROVIDER = %q, want vertex", envMap["LIGHTSPEED_PROVIDER"])
	}
	if envMap["LIGHTSPEED_MODEL"] != "gemini-2.0-flash-exp" {
		t.Errorf("LIGHTSPEED_MODEL = %q", envMap["LIGHTSPEED_MODEL"])
	}
	if envMap["LIGHTSPEED_MODEL_PROVIDER"] != "anthropic" {
		t.Errorf("LIGHTSPEED_MODEL_PROVIDER = %q, want anthropic", envMap["LIGHTSPEED_MODEL_PROVIDER"])
	}
	if envMap["LIGHTSPEED_PROVIDER_PROJECT"] != "test-project" {
		t.Errorf("LIGHTSPEED_PROVIDER_PROJECT = %q, want test-project", envMap["LIGHTSPEED_PROVIDER_PROJECT"])
	}
	if envMap["LIGHTSPEED_PROVIDER_REGION"] != "us-central1" {
		t.Errorf("LIGHTSPEED_PROVIDER_REGION = %q, want us-central1", envMap["LIGHTSPEED_PROVIDER_REGION"])
	}
}

func TestPodSpecBuilder_Azure(t *testing.T) {
	builder := PodSpecBuilder{Image: "quay.io/lightspeed/agent:latest"}
	agent := &agenticv1alpha1.Agent{Spec: agenticv1alpha1.AgentSpec{Model: "gpt-4o"}}
	llm := testLLMProvider(agenticv1alpha1.LLMProviderAzureOpenAI)

	podSpec, err := builder.Build(agent, llm, nil, "analysis", defaultSandboxSA)
	if err != nil {
		t.Fatalf("Build failed: %v", err)
	}

	container := podSpec.Containers[0]
	envMap := envToMap(container.Env)
	if envMap["LIGHTSPEED_PROVIDER"] != "azure" {
		t.Errorf("LIGHTSPEED_PROVIDER = %q, want azure", envMap["LIGHTSPEED_PROVIDER"])
	}
	if envMap["LIGHTSPEED_PROVIDER_URL"] != "https://test.openai.azure.com" {
		t.Errorf("LIGHTSPEED_PROVIDER_URL = %q", envMap["LIGHTSPEED_PROVIDER_URL"])
	}
	if envMap["LIGHTSPEED_PROVIDER_API_VERSION"] != "2024-02-01" {
		t.Errorf("LIGHTSPEED_PROVIDER_API_VERSION = %q", envMap["LIGHTSPEED_PROVIDER_API_VERSION"])
	}
}

func TestPodSpecBuilder_Bedrock(t *testing.T) {
	builder := PodSpecBuilder{Image: "quay.io/lightspeed/agent:latest"}
	agent := &agenticv1alpha1.Agent{Spec: agenticv1alpha1.AgentSpec{Model: "anthropic.claude-3-opus-20240229-v1:0"}}
	llm := testLLMProvider(agenticv1alpha1.LLMProviderAWSBedrock)

	podSpec, err := builder.Build(agent, llm, nil, "analysis", defaultSandboxSA)
	if err != nil {
		t.Fatalf("Build failed: %v", err)
	}

	container := podSpec.Containers[0]
	envMap := envToMap(container.Env)
	if envMap["LIGHTSPEED_PROVIDER"] != "bedrock" {
		t.Errorf("LIGHTSPEED_PROVIDER = %q, want bedrock", envMap["LIGHTSPEED_PROVIDER"])
	}
	if envMap["LIGHTSPEED_PROVIDER_REGION"] != "us-east-1" {
		t.Errorf("LIGHTSPEED_PROVIDER_REGION = %q, want us-east-1", envMap["LIGHTSPEED_PROVIDER_REGION"])
	}
}

func TestPodSpecBuilder_OpenAI(t *testing.T) {
	builder := PodSpecBuilder{Image: "quay.io/lightspeed/agent:latest"}
	agent := &agenticv1alpha1.Agent{Spec: agenticv1alpha1.AgentSpec{Model: "gpt-4o"}}
	llm := testLLMProviderWithURL(agenticv1alpha1.LLMProviderOpenAI, "https://api.example.com")

	podSpec, err := builder.Build(agent, llm, nil, "analysis", defaultSandboxSA)
	if err != nil {
		t.Fatalf("Build failed: %v", err)
	}

	container := podSpec.Containers[0]
	envMap := envToMap(container.Env)
	if envMap["LIGHTSPEED_PROVIDER"] != "openai" {
		t.Errorf("LIGHTSPEED_PROVIDER = %q, want openai", envMap["LIGHTSPEED_PROVIDER"])
	}
	if envMap["LIGHTSPEED_PROVIDER_URL"] != "https://api.example.com" {
		t.Errorf("LIGHTSPEED_PROVIDER_URL = %q", envMap["LIGHTSPEED_PROVIDER_URL"])
	}
}

func TestPodSpecBuilder_NilAgent(t *testing.T) {
	builder := PodSpecBuilder{Image: "quay.io/lightspeed/agent:latest"}
	llm := testLLMProvider(agenticv1alpha1.LLMProviderAnthropic)

	_, err := builder.Build(nil, llm, nil, "analysis", defaultSandboxSA)
	if err == nil {
		t.Fatal("expected error when agent is nil")
	}
	if err.Error() != "agent is required" {
		t.Errorf("error = %q, want 'agent is required'", err.Error())
	}
}

func TestPodSpecBuilder_NilLLM(t *testing.T) {
	builder := PodSpecBuilder{Image: "quay.io/lightspeed/agent:latest"}
	agent := &agenticv1alpha1.Agent{Spec: agenticv1alpha1.AgentSpec{Model: "claude-opus-4-6"}}

	_, err := builder.Build(agent, nil, nil, "analysis", defaultSandboxSA)
	if err == nil {
		t.Fatal("expected error when LLM is nil")
	}
	if err.Error() != "LLMProvider is required" {
		t.Errorf("error = %q, want 'LLMProvider is required'", err.Error())
	}
}

func TestPodSpecBuilder_Skills(t *testing.T) {
	builder := PodSpecBuilder{Image: "quay.io/lightspeed/agent:latest"}
	agent := &agenticv1alpha1.Agent{Spec: agenticv1alpha1.AgentSpec{Model: "claude-opus-4-6"}}
	llm := testLLMProvider(agenticv1alpha1.LLMProviderAnthropic)
	tools := &agenticv1alpha1.ToolsSpec{
		Skills: []agenticv1alpha1.SkillsSource{
			{Image: "quay.io/lightspeed/skills:latest"},
		},
	}

	podSpec, err := builder.Build(agent, llm, tools, "analysis", defaultSandboxSA)
	if err != nil {
		t.Fatalf("Build failed: %v", err)
	}

	foundVolume := false
	for _, v := range podSpec.Volumes {
		if v.Name == "skills" {
			foundVolume = true
			if v.Image == nil {
				t.Fatal("skills volume image source is nil")
			}
			if v.Image.Reference != "quay.io/lightspeed/skills:latest" {
				t.Errorf("skills image = %q, want quay.io/lightspeed/skills:latest", v.Image.Reference)
			}
			if v.Image.PullPolicy != corev1.PullAlways {
				t.Errorf("skills pullPolicy = %v, want PullAlways", v.Image.PullPolicy)
			}
			break
		}
	}
	if !foundVolume {
		t.Error("missing skills volume")
	}
}

func TestPodSpecBuilder_SkillsWithPaths(t *testing.T) {
	builder := PodSpecBuilder{Image: "quay.io/lightspeed/agent:latest"}
	agent := &agenticv1alpha1.Agent{Spec: agenticv1alpha1.AgentSpec{Model: "claude-opus-4-6"}}
	llm := testLLMProvider(agenticv1alpha1.LLMProviderAnthropic)
	tools := &agenticv1alpha1.ToolsSpec{
		Skills: []agenticv1alpha1.SkillsSource{
			{
				Image: "quay.io/lightspeed/skills:latest",
				Paths: []string{"/skills/search.md", "/skills/analyze.md"},
			},
		},
	}

	podSpec, err := builder.Build(agent, llm, tools, "analysis", defaultSandboxSA)
	if err != nil {
		t.Fatalf("Build failed: %v", err)
	}

	container := podSpec.Containers[0]
	if len(container.VolumeMounts) < 2 {
		t.Fatalf("expected at least 2 volume mounts, got %d", len(container.VolumeMounts))
	}

	foundSearch := false
	foundAnalyze := false
	for _, m := range container.VolumeMounts {
		if m.Name == "skills" {
			if m.MountPath == "/app/skills/search.md" && m.SubPath == "skills/search.md" {
				foundSearch = true
			}
			if m.MountPath == "/app/skills/analyze.md" && m.SubPath == "skills/analyze.md" {
				foundAnalyze = true
			}
			if !m.ReadOnly {
				t.Error("skills mount should be readOnly")
			}
		}
	}
	if !foundSearch {
		t.Error("missing search.md skill mount")
	}
	if !foundAnalyze {
		t.Error("missing analyze.md skill mount")
	}
}

func TestPodSpecBuilder_RequiredSecrets_EnvVar(t *testing.T) {
	builder := PodSpecBuilder{Image: "quay.io/lightspeed/agent:latest"}
	agent := &agenticv1alpha1.Agent{Spec: agenticv1alpha1.AgentSpec{Model: "claude-opus-4-6"}}
	llm := testLLMProvider(agenticv1alpha1.LLMProviderAnthropic)
	tools := &agenticv1alpha1.ToolsSpec{
		RequiredSecrets: []agenticv1alpha1.SecretRequirement{
			{
				Name: "github-token",
				MountAs: agenticv1alpha1.SecretMountSpec{
					Type: agenticv1alpha1.SecretMountEnvVar,
					EnvVar: agenticv1alpha1.SecretMountEnvVarConfig{
						Name: "GITHUB_TOKEN",
					},
				},
			},
		},
	}

	podSpec, err := builder.Build(agent, llm, tools, "analysis", defaultSandboxSA)
	if err != nil {
		t.Fatalf("Build failed: %v", err)
	}

	container := podSpec.Containers[0]
	foundEnv := false
	for _, e := range container.Env {
		if e.Name == "GITHUB_TOKEN" {
			foundEnv = true
			if e.ValueFrom == nil {
				t.Fatal("env var valueFrom is nil")
			}
			if e.ValueFrom.SecretKeyRef == nil {
				t.Fatal("env var secretKeyRef is nil")
			}
			if e.ValueFrom.SecretKeyRef.Name != "github-token" {
				t.Errorf("secretKeyRef name = %q, want github-token", e.ValueFrom.SecretKeyRef.Name)
			}
			if e.ValueFrom.SecretKeyRef.Key != "token" {
				t.Errorf("secretKeyRef key = %q, want token", e.ValueFrom.SecretKeyRef.Key)
			}
			break
		}
	}
	if !foundEnv {
		t.Error("missing GITHUB_TOKEN env var")
	}
}

func TestPodSpecBuilder_RequiredSecrets_FileMount(t *testing.T) {
	builder := PodSpecBuilder{Image: "quay.io/lightspeed/agent:latest"}
	agent := &agenticv1alpha1.Agent{Spec: agenticv1alpha1.AgentSpec{Model: "claude-opus-4-6"}}
	llm := testLLMProvider(agenticv1alpha1.LLMProviderAnthropic)
	tools := &agenticv1alpha1.ToolsSpec{
		RequiredSecrets: []agenticv1alpha1.SecretRequirement{
			{
				Name: "kubeconfig",
				MountAs: agenticv1alpha1.SecretMountSpec{
					Type: agenticv1alpha1.SecretMountFilePath,
					FilePath: agenticv1alpha1.SecretMountFilePathConfig{
						Path: "/etc/kubeconfig",
					},
				},
			},
		},
	}

	podSpec, err := builder.Build(agent, llm, tools, "analysis", defaultSandboxSA)
	if err != nil {
		t.Fatalf("Build failed: %v", err)
	}

	foundVolume := false
	for _, v := range podSpec.Volumes {
		if v.Name == "req-kubeconfig" {
			foundVolume = true
			if v.Secret == nil {
				t.Fatal("volume secret source is nil")
			}
			if v.Secret.SecretName != "kubeconfig" {
				t.Errorf("secret name = %q, want kubeconfig", v.Secret.SecretName)
			}
			break
		}
	}
	if !foundVolume {
		t.Error("missing req-kubeconfig volume")
	}

	container := podSpec.Containers[0]
	foundMount := false
	for _, m := range container.VolumeMounts {
		if m.Name == "req-kubeconfig" && m.MountPath == "/etc/kubeconfig" {
			foundMount = true
			if !m.ReadOnly {
				t.Error("required secret mount should be readOnly")
			}
			break
		}
	}
	if !foundMount {
		t.Error("missing /etc/kubeconfig mount")
	}
}
