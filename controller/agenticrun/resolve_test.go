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

func TestAgentDependencyValidator_ValidatesProviderCredentialKeys(t *testing.T) {
	analysisAgent := testDefaultAgent()

	tests := []struct {
		name       string
		provider   *agenticv1alpha1.LLMProvider
		secretData map[string][]byte
		wantError  string
	}{
		{
			name: "azure service principal requires complete key set",
			provider: &agenticv1alpha1.LLMProvider{
				ObjectMeta: metav1.ObjectMeta{Name: "azure"},
				Spec: agenticv1alpha1.LLMProviderSpec{
					Type: agenticv1alpha1.LLMProviderAzureOpenAI,
					AzureOpenAI: agenticv1alpha1.AzureOpenAIConfig{
						CredentialsSecret: agenticv1alpha1.SecretReference{Name: "azure-secret"},
						Endpoint:          "https://example.openai.azure.com",
					},
				},
			},
			secretData: map[string][]byte{"client_id": []byte("client-id")},
			wantError:  `LLMProvider "azure" credentials Secret "azure-secret" must contain either apitoken or all of client_id, tenant_id, client_secret`,
		},
		{
			name: "azure api token accepted",
			provider: &agenticv1alpha1.LLMProvider{
				ObjectMeta: metav1.ObjectMeta{Name: "azure"},
				Spec: agenticv1alpha1.LLMProviderSpec{
					Type: agenticv1alpha1.LLMProviderAzureOpenAI,
					AzureOpenAI: agenticv1alpha1.AzureOpenAIConfig{
						CredentialsSecret: agenticv1alpha1.SecretReference{Name: "azure-secret"},
						Endpoint:          "https://example.openai.azure.com",
					},
				},
			},
			secretData: map[string][]byte{"apitoken": []byte("token")},
		},
		{
			name: "azure complete service principal accepted",
			provider: &agenticv1alpha1.LLMProvider{
				ObjectMeta: metav1.ObjectMeta{Name: "azure"},
				Spec: agenticv1alpha1.LLMProviderSpec{
					Type: agenticv1alpha1.LLMProviderAzureOpenAI,
					AzureOpenAI: agenticv1alpha1.AzureOpenAIConfig{
						CredentialsSecret: agenticv1alpha1.SecretReference{Name: "azure-secret"},
						Endpoint:          "https://example.openai.azure.com",
					},
				},
			},
			secretData: map[string][]byte{
				"client_id":     []byte("client-id"),
				"tenant_id":     []byte("tenant-id"),
				"client_secret": []byte("client-secret"),
			},
		},
		{
			name: "bedrock requires secret key",
			provider: &agenticv1alpha1.LLMProvider{
				ObjectMeta: metav1.ObjectMeta{Name: "bedrock"},
				Spec: agenticv1alpha1.LLMProviderSpec{
					Type: agenticv1alpha1.LLMProviderAWSBedrock,
					AWSBedrock: agenticv1alpha1.AWSBedrockConfig{
						CredentialsSecret: agenticv1alpha1.SecretReference{Name: "bedrock-secret"},
						Region:            "us-east-1",
					},
				},
			},
			secretData: map[string][]byte{"aws_access_key_id": []byte("AKIA")},
			wantError:  `LLMProvider "bedrock" credentials Secret "bedrock-secret" must contain aws_access_key_id and aws_secret_access_key`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			agent := analysisAgent.DeepCopy()
			agent.Spec.LLMProvider.Name = tt.provider.Name
			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: credentialsSecretName(tt.provider), Namespace: "default"},
				Data:       tt.secretData,
			}

			_, _, errs := validateAgentDependencies(context.Background(), buildFakeClient(agent, tt.provider, secret), "default", agent.Name)
			if tt.wantError == "" {
				if len(errs) != 0 {
					t.Fatalf("validate returned errors: %v", errs)
				}
				return
			}
			if len(errs) != 1 {
				t.Fatalf("validate returned %d errors %v, want 1", len(errs), errs)
			}
			if !strings.Contains(errs[0].Error(), tt.wantError) {
				t.Fatalf("error = %q, want substring %q", errs[0], tt.wantError)
			}
		})
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
