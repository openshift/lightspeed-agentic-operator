package proposal

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
)

func buildFakeClient(objs ...client.Object) client.Client {
	return fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(objs...).Build()
}

func TestResolveProposal_Inline_AnalysisOnly(t *testing.T) {
	proposal := &agenticv1alpha1.Proposal{
		ObjectMeta: metav1.ObjectMeta{Name: "p1", Namespace: "default"},
		Spec: agenticv1alpha1.ProposalSpec{
			Request: "investigate this",
			Tools:   agenticv1alpha1.ToolsSpec{Skills: []agenticv1alpha1.SkillsSource{{Image: "s:v1"}}},
			Analysis: agenticv1alpha1.ProposalStep{
				Agent: "smart",
			},
		},
	}
	smart := &agenticv1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "smart"}, Spec: agenticv1alpha1.AgentSpec{LLMProvider: agenticv1alpha1.LLMProviderReference{Name: "opus"}}}

	fc := buildFakeClient(smart, testLLM("opus"), proposal)
	resolved, err := resolveProposal(context.Background(), fc, proposal, nil)
	if err != nil {
		t.Fatalf("resolveProposal: %v", err)
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

func TestResolveProposal_Inline_WithExecAndVerify(t *testing.T) {
	proposal := &agenticv1alpha1.Proposal{
		ObjectMeta: metav1.ObjectMeta{Name: "p1", Namespace: "default"},
		Spec: agenticv1alpha1.ProposalSpec{
			Request:      "full inline",
			Tools:        agenticv1alpha1.ToolsSpec{Skills: []agenticv1alpha1.SkillsSource{{Image: "s:v1"}}},
			Analysis:     agenticv1alpha1.ProposalStep{Agent: "smart"},
			Execution:    agenticv1alpha1.ProposalStep{Agent: "default"},
			Verification: agenticv1alpha1.ProposalStep{Agent: "fast"},
		},
	}
	smart := &agenticv1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "smart"}, Spec: agenticv1alpha1.AgentSpec{LLMProvider: agenticv1alpha1.LLMProviderReference{Name: "opus"}}}
	def := &agenticv1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "default"}, Spec: agenticv1alpha1.AgentSpec{LLMProvider: agenticv1alpha1.LLMProviderReference{Name: "opus"}}}
	fast := &agenticv1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "fast"}, Spec: agenticv1alpha1.AgentSpec{LLMProvider: agenticv1alpha1.LLMProviderReference{Name: "haiku"}}}

	fc := buildFakeClient(smart, def, fast, testLLM("opus"), testLLM("haiku"), proposal)
	resolved, err := resolveProposal(context.Background(), fc, proposal, nil)
	if err != nil {
		t.Fatalf("resolveProposal: %v", err)
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

func TestResolveProposal_Inline_DefaultAgent(t *testing.T) {
	proposal := &agenticv1alpha1.Proposal{
		ObjectMeta: metav1.ObjectMeta{Name: "p1", Namespace: "default"},
		Spec: agenticv1alpha1.ProposalSpec{
			Request:  "no agent specified",
			Tools:    agenticv1alpha1.ToolsSpec{Skills: []agenticv1alpha1.SkillsSource{{Image: "s:v1"}}},
			Analysis: agenticv1alpha1.ProposalStep{},
		},
	}

	fc := buildFakeClient(testDefaultAgent(), testLLM("smart"), proposal)
	resolved, err := resolveProposal(context.Background(), fc, proposal, nil)
	if err != nil {
		t.Fatalf("resolveProposal: %v", err)
	}

	if resolved.Analysis.Agent.Name != "default" {
		t.Errorf("analysis agent = %s, want default (implicit)", resolved.Analysis.Agent.Name)
	}
}

func TestResolveProposal_PerStepTools(t *testing.T) {
	proposal := &agenticv1alpha1.Proposal{
		ObjectMeta: metav1.ObjectMeta{Name: "p1", Namespace: "default"},
		Spec: agenticv1alpha1.ProposalSpec{
			Request: "fix it",
			Tools: agenticv1alpha1.ToolsSpec{
				Skills: []agenticv1alpha1.SkillsSource{{Image: "shared:latest"}},
			},
			Analysis: agenticv1alpha1.ProposalStep{
				Agent: "default",
				Tools: agenticv1alpha1.ToolsSpec{
					Skills: []agenticv1alpha1.SkillsSource{{Image: "analysis-specific:v1", Paths: []string{"/skills/remediation"}}},
				},
			},
			Execution: agenticv1alpha1.ProposalStep{Agent: "default"},
			Verification: agenticv1alpha1.ProposalStep{
				Agent: "default",
				Tools: agenticv1alpha1.ToolsSpec{
					Skills: []agenticv1alpha1.SkillsSource{{Image: "verify-specific:v2", Paths: []string{"/skills/compliance"}}},
				},
			},
		},
	}

	fc := buildFakeClient(testDefaultAgent(), testLLM("smart"), proposal)
	resolved, err := resolveProposal(context.Background(), fc, proposal, nil)
	if err != nil {
		t.Fatalf("resolveProposal: %v", err)
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

func TestResolveProposal_MissingAgent(t *testing.T) {
	proposal := &agenticv1alpha1.Proposal{
		ObjectMeta: metav1.ObjectMeta{Name: "p1", Namespace: "default"},
		Spec: agenticv1alpha1.ProposalSpec{
			Request:  "fix it",
			Analysis: agenticv1alpha1.ProposalStep{Agent: "nonexistent"},
		},
	}

	fc := buildFakeClient(proposal)
	_, err := resolveProposal(context.Background(), fc, proposal, nil)
	if err == nil {
		t.Fatal("expected error for missing agent")
	}
}

func TestResolveProposal_AgentCaching(t *testing.T) {
	proposal := &agenticv1alpha1.Proposal{
		ObjectMeta: metav1.ObjectMeta{Name: "p1", Namespace: "default"},
		Spec: agenticv1alpha1.ProposalSpec{
			Request:      "fix it",
			Tools:        agenticv1alpha1.ToolsSpec{Skills: []agenticv1alpha1.SkillsSource{{Image: "s:v1"}}},
			Analysis:     agenticv1alpha1.ProposalStep{Agent: "default"},
			Execution:    agenticv1alpha1.ProposalStep{Agent: "default"},
			Verification: agenticv1alpha1.ProposalStep{Agent: "default"},
		},
	}

	fc := buildFakeClient(testDefaultAgent(), testLLM("smart"), proposal)
	resolved, err := resolveProposal(context.Background(), fc, proposal, nil)
	if err != nil {
		t.Fatalf("resolveProposal: %v", err)
	}

	if resolved.Analysis.Agent != resolved.Execution.Agent {
		t.Error("same agent name should resolve to the same Agent pointer (cached)")
	}
	if resolved.Analysis.LLM != resolved.Execution.LLM {
		t.Error("same LLM should resolve to the same LLMProvider pointer (cached)")
	}
}
