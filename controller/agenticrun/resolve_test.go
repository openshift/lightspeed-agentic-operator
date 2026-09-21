package agenticrun

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
)

func buildFakeClient(objs ...client.Object) client.Client {
	return fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(objs...).Build()
}

func TestResolveAgenticRun_Inline_AnalysisOnly(t *testing.T) {
	run := &agenticv1alpha1.AgenticRun{
		ObjectMeta: metav1.ObjectMeta{Name: "p1", Namespace: "default"},
		Spec: agenticv1alpha1.AgenticRunSpec{
			Request: "investigate this",
			Tools:   agenticv1alpha1.ToolsSpec{Skills: []agenticv1alpha1.SkillsSource{{Image: "s:v1"}}},
			Analysis: agenticv1alpha1.AgenticRunStep{
				Agent: "smart",
			},
		},
	}
	smart := &agenticv1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "smart"}, Spec: agenticv1alpha1.AgentSpec{LLMProvider: agenticv1alpha1.LLMProviderReference{Name: "opus"}}}

	fc := buildFakeClient(smart, testLLM("opus"), run)
	resolved, err := resolveAgenticRun(context.Background(), fc, run, nil)
	if err != nil {
		t.Fatalf("resolveAgenticRun: %v", err)
	}

	if resolved.Analysis.Agent.Name != "smart" {
		t.Errorf("analysis agent = %s, want smart", resolved.Analysis.Agent.Name)
	}
	if resolved.Execution != nil {
		t.Error("execution should be nil for analysis-only")
	}
	if resolved.Verification != nil {
		t.Error("verification should be nil for analysis-only")
	}
}

func TestResolveAgenticRun_Inline_WithExecAndVerify(t *testing.T) {
	run := &agenticv1alpha1.AgenticRun{
		ObjectMeta: metav1.ObjectMeta{Name: "p1", Namespace: "default"},
		Spec: agenticv1alpha1.AgenticRunSpec{
			Request:      "full inline",
			Tools:        agenticv1alpha1.ToolsSpec{Skills: []agenticv1alpha1.SkillsSource{{Image: "s:v1"}}},
			Analysis:     agenticv1alpha1.AgenticRunStep{Agent: "smart"},
			Execution:    agenticv1alpha1.AgenticRunStep{Agent: "default"},
			Verification: agenticv1alpha1.AgenticRunStep{Agent: "fast"},
		},
	}
	smart := &agenticv1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "smart"}, Spec: agenticv1alpha1.AgentSpec{LLMProvider: agenticv1alpha1.LLMProviderReference{Name: "opus"}}}
	def := &agenticv1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "default"}, Spec: agenticv1alpha1.AgentSpec{LLMProvider: agenticv1alpha1.LLMProviderReference{Name: "opus"}}}
	fast := &agenticv1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "fast"}, Spec: agenticv1alpha1.AgentSpec{LLMProvider: agenticv1alpha1.LLMProviderReference{Name: "haiku"}}}

	fc := buildFakeClient(smart, def, fast, testLLM("opus"), testLLM("haiku"), run)
	resolved, err := resolveAgenticRun(context.Background(), fc, run, nil)
	if err != nil {
		t.Fatalf("resolveAgenticRun: %v", err)
	}

	if resolved.Analysis.Agent.Name != "smart" {
		t.Errorf("analysis agent = %s, want smart", resolved.Analysis.Agent.Name)
	}
	if resolved.Execution == nil || resolved.Execution.Agent.Name != "default" {
		t.Error("execution should use default agent")
	}
	if resolved.Verification == nil || resolved.Verification.Agent.Name != "fast" {
		t.Error("verification should use fast agent")
	}
	if resolved.Verification.LLM.Name != "haiku" {
		t.Errorf("verification LLM = %s, want haiku", resolved.Verification.LLM.Name)
	}
}

func TestResolveAgenticRun_Inline_DefaultAgent(t *testing.T) {
	run := &agenticv1alpha1.AgenticRun{
		ObjectMeta: metav1.ObjectMeta{Name: "p1", Namespace: "default"},
		Spec: agenticv1alpha1.AgenticRunSpec{
			Request:  "no agent specified",
			Tools:    agenticv1alpha1.ToolsSpec{Skills: []agenticv1alpha1.SkillsSource{{Image: "s:v1"}}},
			Analysis: agenticv1alpha1.AgenticRunStep{},
		},
	}

	fc := buildFakeClient(testDefaultAgent(), testLLM("smart"), run)
	resolved, err := resolveAgenticRun(context.Background(), fc, run, nil)
	if err != nil {
		t.Fatalf("resolveAgenticRun: %v", err)
	}

	if resolved.Analysis.Agent.Name != "default" {
		t.Errorf("analysis agent = %s, want default (implicit)", resolved.Analysis.Agent.Name)
	}
}

func TestResolveAgenticRun_PerStepTools(t *testing.T) {
	run := &agenticv1alpha1.AgenticRun{
		ObjectMeta: metav1.ObjectMeta{Name: "p1", Namespace: "default"},
		Spec: agenticv1alpha1.AgenticRunSpec{
			Request: "fix it",
			Tools: agenticv1alpha1.ToolsSpec{
				Skills: []agenticv1alpha1.SkillsSource{{Image: "shared:latest"}},
			},
			Analysis: agenticv1alpha1.AgenticRunStep{
				Agent: "default",
				Tools: agenticv1alpha1.ToolsSpec{
					Skills: []agenticv1alpha1.SkillsSource{{Image: "analysis-specific:v1", Paths: []string{"/skills/remediation"}}},
				},
			},
			Execution: agenticv1alpha1.AgenticRunStep{Agent: "default"},
			Verification: agenticv1alpha1.AgenticRunStep{
				Agent: "default",
				Tools: agenticv1alpha1.ToolsSpec{
					Skills: []agenticv1alpha1.SkillsSource{{Image: "verify-specific:v2", Paths: []string{"/skills/compliance"}}},
				},
			},
		},
	}

	fc := buildFakeClient(testDefaultAgent(), testLLM("smart"), run)
	resolved, err := resolveAgenticRun(context.Background(), fc, run, nil)
	if err != nil {
		t.Fatalf("resolveAgenticRun: %v", err)
	}

	if resolved.Analysis.Tools.Skills[0].Image != "analysis-specific:v1" {
		t.Errorf("analysis should use per-step tools, got %s", resolved.Analysis.Tools.Skills[0].Image)
	}
	if len(resolved.Analysis.Tools.Skills[0].Paths) != 1 || resolved.Analysis.Tools.Skills[0].Paths[0] != "/skills/remediation" {
		t.Errorf("analysis tools should have specific paths, got %v", resolved.Analysis.Tools.Skills[0].Paths)
	}
	if resolved.Execution.Tools.Skills[0].Image != "shared:latest" {
		t.Errorf("execution should use shared tools (no per-step override), got %s", resolved.Execution.Tools.Skills[0].Image)
	}
	if resolved.Verification.Tools.Skills[0].Image != "verify-specific:v2" {
		t.Errorf("verification should use per-step tools, got %s", resolved.Verification.Tools.Skills[0].Image)
	}
}

func TestResolveAgenticRun_MissingAgent(t *testing.T) {
	run := &agenticv1alpha1.AgenticRun{
		ObjectMeta: metav1.ObjectMeta{Name: "p1", Namespace: "default"},
		Spec: agenticv1alpha1.AgenticRunSpec{
			Request:  "fix it",
			Analysis: agenticv1alpha1.AgenticRunStep{Agent: "nonexistent"},
		},
	}

	fc := buildFakeClient(run)
	_, err := resolveAgenticRun(context.Background(), fc, run, nil)
	if err == nil {
		t.Fatal("expected error for missing agent")
	}
}

func TestResolveAgenticRun_AgentCaching(t *testing.T) {
	run := &agenticv1alpha1.AgenticRun{
		ObjectMeta: metav1.ObjectMeta{Name: "p1", Namespace: "default"},
		Spec: agenticv1alpha1.AgenticRunSpec{
			Request:      "fix it",
			Tools:        agenticv1alpha1.ToolsSpec{Skills: []agenticv1alpha1.SkillsSource{{Image: "s:v1"}}},
			Analysis:     agenticv1alpha1.AgenticRunStep{Agent: "default"},
			Execution:    agenticv1alpha1.AgenticRunStep{Agent: "default"},
			Verification: agenticv1alpha1.AgenticRunStep{Agent: "default"},
		},
	}

	fc := buildFakeClient(testDefaultAgent(), testLLM("smart"), run)
	resolved, err := resolveAgenticRun(context.Background(), fc, run, nil)
	if err != nil {
		t.Fatalf("resolveAgenticRun: %v", err)
	}

	if resolved.Analysis.Agent != resolved.Execution.Agent {
		t.Error("same agent name should resolve to the same Agent pointer (cached)")
	}
	if resolved.Analysis.LLM != resolved.Execution.LLM {
		t.Error("same LLM should resolve to the same LLMProvider pointer (cached)")
	}
}

// ── Azure credential validation ──

func azureLLM(secretName string) *agenticv1alpha1.LLMProvider {
	return &agenticv1alpha1.LLMProvider{
		ObjectMeta: metav1.ObjectMeta{Name: "azure-llm"},
		Spec: agenticv1alpha1.LLMProviderSpec{
			Type: agenticv1alpha1.LLMProviderAzureOpenAI,
			AzureOpenAI: agenticv1alpha1.AzureOpenAIConfig{
				CredentialsSecret: agenticv1alpha1.SecretReference{Name: secretName},
				Endpoint:          "https://myresource.openai.azure.com",
			},
		},
	}
}

func azureSecret(name string, keys map[string][]byte) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "openshift-lightspeed"},
		Data:       keys,
	}
}

func TestValidateCredentials_AzureAPIKey(t *testing.T) {
	secret := azureSecret("creds", map[string][]byte{
		"apitoken": []byte("my-key"),
	})
	fc := buildFakeClient(azureLLM("creds"), secret)
	err := validateCredentials(context.Background(), fc, azureLLM("creds"), "openshift-lightspeed")
	if err != nil {
		t.Errorf("expected nil, got %v", err)
	}
}

func TestValidateCredentials_AzureEntraID(t *testing.T) {
	secret := azureSecret("creds", map[string][]byte{
		"client_id":     []byte("cid"),
		"tenant_id":     []byte("tid"),
		"client_secret": []byte("csec"),
	})
	fc := buildFakeClient(azureLLM("creds"), secret)
	err := validateCredentials(context.Background(), fc, azureLLM("creds"), "openshift-lightspeed")
	if err != nil {
		t.Errorf("expected nil, got %v", err)
	}
}

func TestValidateCredentials_AzureIncompleteSP(t *testing.T) {
	secret := azureSecret("creds", map[string][]byte{
		"client_id": []byte("cid"),
		"tenant_id": []byte("tid"),
		// client_secret missing, no apitoken
	})
	fc := buildFakeClient(azureLLM("creds"), secret)
	err := validateCredentials(context.Background(), fc, azureLLM("creds"), "openshift-lightspeed")
	if err == nil {
		t.Fatal("expected error for incomplete SP set")
	}
	if !strings.Contains(err.Error(), "client_secret") {
		t.Errorf("expected error to mention missing key, got: %v", err)
	}
}

func TestValidateCredentials_AzureNoCredentials(t *testing.T) {
	secret := azureSecret("creds", map[string][]byte{
		"unrelated": []byte("value"),
	})
	fc := buildFakeClient(azureLLM("creds"), secret)
	err := validateCredentials(context.Background(), fc, azureLLM("creds"), "openshift-lightspeed")
	if err == nil {
		t.Fatal("expected error when no valid credential set")
	}
}

func TestValidateCredentials_NonAzureSkips(t *testing.T) {
	llm := &agenticv1alpha1.LLMProvider{
		ObjectMeta: metav1.ObjectMeta{Name: "anthropic-llm"},
		Spec: agenticv1alpha1.LLMProviderSpec{
			Type: agenticv1alpha1.LLMProviderAnthropic,
			Anthropic: agenticv1alpha1.AnthropicConfig{
				CredentialsSecret: agenticv1alpha1.SecretReference{Name: "api-key"},
			},
		},
	}
	// No secret in cluster — validation should skip for non-Azure
	fc := buildFakeClient(llm)
	err := validateCredentials(context.Background(), fc, llm, "openshift-lightspeed")
	if err != nil {
		t.Errorf("non-Azure providers should skip validation, got: %v", err)
	}
}

func TestValidateCredentials_AzureSecretNotFound(t *testing.T) {
	// Secret doesn't exist
	fc := buildFakeClient(azureLLM("missing-secret"))
	err := validateCredentials(context.Background(), fc, azureLLM("missing-secret"), "openshift-lightspeed")
	if err == nil {
		t.Fatal("expected error when secret not found")
	}
}
