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

func TestAgentDependencyValidator_VerifiesAgentDependencies(t *testing.T) {
	agent := testDefaultAgent()
	provider := testLLM("smart")
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "provider-secret", Namespace: "default"}}
	agent.Spec.LLMProvider.Name = provider.Name
	provider.Spec.GoogleCloudVertex.CredentialsSecret.Name = secret.Name

	_, _, errs := validateAgentDependencies(context.Background(), buildFakeClient(agent, provider, secret), "default", "")
	if len(errs) != 0 {
		t.Fatalf("validate returned errors: %v", errs)
	}
}

func TestAgentDependencyValidator_ReportsMissingDependencies(t *testing.T) {
	tests := []struct {
		name       string
		objects    []client.Object
		agentName  string
		wantErrors []string
	}{
		{
			name:       "agent",
			wantErrors: []string{`Agent "missing" not found`},
			agentName:  "missing",
		},
		{
			name:       "LLMProvider",
			objects:    []client.Object{testDefaultAgent()},
			wantErrors: []string{`Agent "default" references missing LLMProvider "smart"`},
		},
		{
			name: "credentials Secret",
			objects: func() []client.Object {
				agent := testDefaultAgent()
				provider := testLLM("smart")
				agent.Spec.LLMProvider.Name = provider.Name
				provider.Spec.GoogleCloudVertex.CredentialsSecret.Name = "missing-secret"
				return []client.Object{agent, provider}
			}(),
			wantErrors: []string{`LLMProvider "smart" references missing Secret "missing-secret"`},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, errs := validateAgentDependencies(context.Background(), buildFakeClient(tt.objects...), "default", tt.agentName)
			if len(errs) != len(tt.wantErrors) {
				t.Fatalf("validate returned %d errors %v, want %d errors %v", len(errs), errs, len(tt.wantErrors), tt.wantErrors)
			}
			for i, want := range tt.wantErrors {
				if !strings.Contains(errs[i].Error(), want) {
					t.Errorf("error[%d] = %q, want substring %q", i, errs[i], want)
				}
			}
		})
	}
}

func TestValidateAgenticRun_ReturnsValidatedDependencies(t *testing.T) {
	run := &agenticv1alpha1.AgenticRun{
		ObjectMeta: metav1.ObjectMeta{Name: "p1", Namespace: "default"},
		Spec: agenticv1alpha1.AgenticRunSpec{
			Analysis: agenticv1alpha1.AgenticRunStep{Agent: "default"},
		},
	}
	agent := testDefaultAgent()
	provider := testLLM("smart")
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "llm-secret", Namespace: "default"}}

	errs, agents, providers := validateAgenticRun(context.Background(), buildFakeClient(agent, provider, secret, run), run, nil, "default")
	if len(errs) != 0 {
		t.Fatalf("validateAgenticRun returned errors: %v", errs)
	}
	if got := agents[agent.Name]; got == nil || got.Name != agent.Name || got.Spec.LLMProvider.Name != provider.Name {
		t.Fatalf("validated Agent = %#v, want Agent %q referencing %q", got, agent.Name, provider.Name)
	}
	if got := providers[provider.Name]; got == nil || got.Name != provider.Name {
		t.Fatalf("validated LLMProvider = %#v, want %q", got, provider.Name)
	}
}

func TestValidateAgenticRun_UsesOperatorNamespaceForCredentials(t *testing.T) {
	run := &agenticv1alpha1.AgenticRun{
		ObjectMeta: metav1.ObjectMeta{Name: "p1", Namespace: "workload"},
		Spec: agenticv1alpha1.AgenticRunSpec{
			Analysis: agenticv1alpha1.AgenticRunStep{Agent: "default"},
		},
	}
	agent := testDefaultAgent()
	provider := testLLM("smart")
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "llm-secret", Namespace: "operator"}}

	errs, agents, providers := validateAgenticRun(context.Background(), buildFakeClient(agent, provider, secret, run), run, nil, "operator")
	if len(errs) != 0 {
		t.Fatalf("validateAgenticRun returned errors: %v", errs)
	}
	if agents[agent.Name] == nil || providers[provider.Name] == nil {
		t.Fatalf("expected validated dependencies, got agents=%v providers=%v", agents, providers)
	}
}

func TestValidateAgenticRun_DeduplicatesInvalidAgent(t *testing.T) {
	run := &agenticv1alpha1.AgenticRun{
		ObjectMeta: metav1.ObjectMeta{Name: "p1", Namespace: "default"},
		Spec: agenticv1alpha1.AgenticRunSpec{
			Analysis:     agenticv1alpha1.AgenticRunStep{Agent: "broken"},
			Execution:    agenticv1alpha1.AgenticRunStep{Agent: "broken"},
			Verification: agenticv1alpha1.AgenticRunStep{Agent: "broken"},
		},
	}
	broken := &agenticv1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "broken"},
		Spec: agenticv1alpha1.AgentSpec{
			LLMProvider: agenticv1alpha1.LLMProviderReference{Name: "missing"},
		},
	}

	errs, agents, providers := validateAgenticRun(context.Background(), buildFakeClient(broken, run), run, nil, "default")
	if len(agents) != 0 || len(providers) != 0 {
		t.Fatalf("invalid Agent should not be returned in dependency maps: agents=%v providers=%v", agents, providers)
	}
	if len(errs) != 1 {
		t.Fatalf("validateAgenticRun returned %d errors, want 1: %v", len(errs), errs)
	}
	if !strings.Contains(errs[0].Error(), `Agent "broken" references missing LLMProvider "missing"`) {
		t.Fatalf("error = %q, want missing LLMProvider error", errs[0])
	}
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

	fc := buildFakeClient(smart, testLLM("opus"), &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "llm-secret", Namespace: "default"}}, run)
	resolved, err := resolveAgenticRun(context.Background(), fc, run, nil, "default")
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

	fc := buildFakeClient(smart, def, fast, testLLM("opus"), testLLM("haiku"), &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "llm-secret", Namespace: "default"}}, run)
	resolved, err := resolveAgenticRun(context.Background(), fc, run, nil, "default")
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

	fc := buildFakeClient(testDefaultAgent(), testLLM("smart"), &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "llm-secret", Namespace: "default"}}, run)
	resolved, err := resolveAgenticRun(context.Background(), fc, run, nil, "default")
	if err != nil {
		t.Fatalf("resolveAgenticRun: %v", err)
	}

	if resolved.Analysis.Agent.Name != "default" {
		t.Errorf("analysis agent = %s, want default (implicit)", resolved.Analysis.Agent.Name)
	}
}

func TestResolveAgenticRun_RunLevelTools(t *testing.T) {
	run := &agenticv1alpha1.AgenticRun{
		ObjectMeta: metav1.ObjectMeta{Name: "p1", Namespace: "default"},
		Spec: agenticv1alpha1.AgenticRunSpec{
			Request: "fix it",
			Tools: agenticv1alpha1.ToolsSpec{
				Skills: []agenticv1alpha1.SkillsSource{{Image: "shared:latest", Paths: []string{"/skills/remediation"}}},
			},
			Analysis:     agenticv1alpha1.AgenticRunStep{Agent: "default"},
			Execution:    agenticv1alpha1.AgenticRunStep{Agent: "default"},
			Verification: agenticv1alpha1.AgenticRunStep{Agent: "default"},
		},
	}

	fc := buildFakeClient(testDefaultAgent(), testLLM("smart"), &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "llm-secret", Namespace: "default"}}, run)
	resolved, err := resolveAgenticRun(context.Background(), fc, run, nil, "default")
	if err != nil {
		t.Fatalf("resolveAgenticRun: %v", err)
	}

	if resolved.Analysis.Tools != &run.Spec.Tools ||
		resolved.Execution.Tools != &run.Spec.Tools ||
		resolved.Verification.Tools != &run.Spec.Tools {
		t.Fatal("analysis, execution, and verification must all use run-level tools")
	}
	if got := resolved.Analysis.Tools.Skills[0].Image; got != "shared:latest" {
		t.Errorf("run-level tools skill image = %s, want shared:latest", got)
	}
	if got := resolved.Analysis.Tools.Skills[0].Paths; len(got) != 1 || got[0] != "/skills/remediation" {
		t.Errorf("run-level tools skill paths = %v, want [/skills/remediation]", got)
	}
}

func TestResolveAgenticRun_MissingCredentialsSecret(t *testing.T) {
	run := &agenticv1alpha1.AgenticRun{
		ObjectMeta: metav1.ObjectMeta{Name: "p1", Namespace: "default"},
		Spec: agenticv1alpha1.AgenticRunSpec{
			Request:  "fix it",
			Analysis: agenticv1alpha1.AgenticRunStep{Agent: "default"},
		},
	}

	fc := buildFakeClient(testDefaultAgent(), testLLM("smart"), run)
	_, err := resolveAgenticRun(context.Background(), fc, run, nil, "default")
	if err == nil {
		t.Fatal("expected error for missing credentials Secret")
	}
	if !strings.Contains(err.Error(), `LLMProvider "smart" references missing Secret "llm-secret"`) {
		t.Fatalf("error = %q, want missing credentials Secret", err)
	}
}

func TestResolveAgenticRun_ValidatesWorkflowAgents(t *testing.T) {
	run := &agenticv1alpha1.AgenticRun{
		ObjectMeta: metav1.ObjectMeta{Name: "p1", Namespace: "default"},
		Spec: agenticv1alpha1.AgenticRunSpec{
			Request:   "fix it",
			Analysis:  agenticv1alpha1.AgenticRunStep{Agent: "default"},
			Execution: agenticv1alpha1.AgenticRunStep{Agent: "broken"},
		},
	}
	broken := &agenticv1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "broken"},
		Spec:       agenticv1alpha1.AgentSpec{LLMProvider: agenticv1alpha1.LLMProviderReference{Name: "missing"}},
	}

	fc := buildFakeClient(testDefaultAgent(), testLLM("smart"), &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "llm-secret", Namespace: "default"}}, broken, run)
	_, err := resolveAgenticRun(context.Background(), fc, run, nil, "default")
	if err == nil {
		t.Fatal("expected error for invalid workflow Agent")
	}
	if !strings.Contains(err.Error(), `Agent "broken" references missing LLMProvider "missing"`) {
		t.Fatalf("error = %q, want invalid workflow Agent", err)
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
	_, err := resolveAgenticRun(context.Background(), fc, run, nil, "default")
	if err == nil {
		t.Fatal("expected error for missing agent")
	}
}

func TestResolveAgenticRun_RepeatedAgentReferences(t *testing.T) {
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

	fc := buildFakeClient(testDefaultAgent(), testLLM("smart"), &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "llm-secret", Namespace: "default"}}, run)
	resolved, err := resolveAgenticRun(context.Background(), fc, run, nil, "default")
	if err != nil {
		t.Fatalf("resolveAgenticRun: %v", err)
	}

	if resolved.Analysis.Agent.Name != "default" || resolved.Execution.Agent.Name != "default" || resolved.Verification.Agent.Name != "default" {
		t.Error("all stages should resolve the referenced default Agent")
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

func TestValidateAgentDependencies_AzureCredentials(t *testing.T) {
	tests := []struct {
		name      string
		data      map[string][]byte
		wantError string
	}{
		{"api key", map[string][]byte{"apitoken": []byte("my-key")}, ""},
		{"service principal", map[string][]byte{"client_id": []byte("cid"), "tenant_id": []byte("tid"), "client_secret": []byte("csec")}, ""},
		{"incomplete service principal", map[string][]byte{"client_id": []byte("cid"), "tenant_id": []byte("tid")}, "client_secret"},
		{"unrelated keys", map[string][]byte{"unrelated": []byte("value")}, "client_id"},
		{"empty API key", map[string][]byte{"apitoken": []byte{}}, "client_id"},
		{"empty service principal value", map[string][]byte{"client_id": []byte("cid"), "tenant_id": []byte{}, "client_secret": []byte("csec")}, "tenant_id"},
		{"empty API key with valid service principal", map[string][]byte{"apitoken": []byte{}, "client_id": []byte("cid"), "tenant_id": []byte("tid"), "client_secret": []byte("csec")}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			agent := testDefaultAgent()
			agent.Spec.LLMProvider.Name = "azure-llm"
			secret := azureSecret("creds", tt.data)
			_, _, issues := validateAgentDependencies(context.Background(), buildFakeClient(agent, azureLLM("creds"), secret), "openshift-lightspeed", agent.Name)
			if tt.wantError == "" {
				if len(issues) != 0 {
					t.Fatalf("unexpected validation issues: %v", issues)
				}
			} else if len(issues) != 1 || !strings.Contains(issues[0].Error(), tt.wantError) {
				t.Fatalf("validation issues = %v, want one mentioning %q", issues, tt.wantError)
			}
		})
	}
}
