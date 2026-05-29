package proposal

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
)

// --- Configurable agent stub for tests ---

type testAgentCaller struct {
	analyzeErr  error
	executeErr  error
	verifyErr   error
	escalateErr error

	analyzeResult  *AnalysisOutput
	executeResult  *ExecutionOutput
	verifyResult   *VerificationOutput
	escalateResult *EscalationOutput
}

func newTestAgentCaller() *testAgentCaller {
	stub := &StubAgentCaller{}
	a, _ := stub.Analyze(context.Background(), nil, resolvedStep{}, "", 0)
	e, _ := stub.Execute(context.Background(), nil, resolvedStep{}, nil, 0)
	v, _ := stub.Verify(context.Background(), nil, resolvedStep{}, nil, nil, 0)
	esc, _ := stub.Escalate(context.Background(), nil, resolvedStep{}, "", 0)
	return &testAgentCaller{analyzeResult: a, executeResult: e, verifyResult: v, escalateResult: esc}
}

func (ta *testAgentCaller) Analyze(_ context.Context, _ *agenticv1alpha1.Proposal, _ resolvedStep, _ string, _ time.Duration) (*AnalysisOutput, error) {
	if ta.analyzeErr != nil {
		return nil, ta.analyzeErr
	}
	return ta.analyzeResult, nil
}
func (ta *testAgentCaller) Execute(_ context.Context, _ *agenticv1alpha1.Proposal, _ resolvedStep, _ *agenticv1alpha1.RemediationOption, _ time.Duration) (*ExecutionOutput, error) {
	if ta.executeErr != nil {
		return nil, ta.executeErr
	}
	return ta.executeResult, nil
}
func (ta *testAgentCaller) Verify(_ context.Context, _ *agenticv1alpha1.Proposal, _ resolvedStep, _ *agenticv1alpha1.RemediationOption, _ *ExecutionOutput, _ time.Duration) (*VerificationOutput, error) {
	if ta.verifyErr != nil {
		return nil, ta.verifyErr
	}
	return ta.verifyResult, nil
}
func (ta *testAgentCaller) Escalate(_ context.Context, _ *agenticv1alpha1.Proposal, _ resolvedStep, _ string, _ time.Duration) (*EscalationOutput, error) {
	if ta.escalateErr != nil {
		return nil, ta.escalateErr
	}
	return ta.escalateResult, nil
}
func (ta *testAgentCaller) ReleaseSandboxes(_ context.Context, _ *agenticv1alpha1.Proposal) error {
	return nil
}

// --- Test fixtures ---

func testScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = agenticv1alpha1.AddToScheme(s)
	_ = corev1.AddToScheme(s)
	_ = rbacv1.AddToScheme(s)
	return s
}

func testDefaultAgent() *agenticv1alpha1.Agent {
	return &agenticv1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "default"},
		Spec: agenticv1alpha1.AgentSpec{
			LLMProvider: agenticv1alpha1.LLMProviderReference{Name: "smart"},
			Model:       "claude-opus-4-6",
		},
	}
}

func testTools() agenticv1alpha1.ToolsSpec {
	return agenticv1alpha1.ToolsSpec{
		Skills: []agenticv1alpha1.SkillsSource{{Image: "registry.example.com/skills:latest"}},
	}
}

func testLLM(name string) *agenticv1alpha1.LLMProvider {
	return &agenticv1alpha1.LLMProvider{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: agenticv1alpha1.LLMProviderSpec{
			Type: agenticv1alpha1.LLMProviderGoogleCloudVertex,
			GoogleCloudVertex: agenticv1alpha1.GoogleCloudVertexConfig{
				CredentialsSecret: agenticv1alpha1.SecretReference{Name: "llm-secret"},
				ProjectID:         "test-project",
				Region:            "us-central1",
			},
		},
	}
}

func testProposal() *agenticv1alpha1.Proposal {
	return &agenticv1alpha1.Proposal{
		ObjectMeta: metav1.ObjectMeta{Name: "fix-crash", Namespace: "default"},
		Spec: agenticv1alpha1.ProposalSpec{
			Request:          "Pod crashing in production",
			Tools:            testTools(),
			TargetNamespaces: []string{"production"},
			Analysis:         agenticv1alpha1.ProposalStep{Agent: "default"},
			Execution:        agenticv1alpha1.ProposalStep{Agent: "default"},
			Verification:     agenticv1alpha1.ProposalStep{Agent: "default"},
		},
	}
}

// testAutoApprovePolicy returns an ApprovalPolicy that auto-approves analysis
// and verification stages, so tests only need to explicitly approve execution
// (which carries the selected option).
func testAutoApprovePolicy() *agenticv1alpha1.ApprovalPolicy {
	return testAutoApprovePolicyWithMaxAttempts(0)
}

func testAutoApprovePolicyWithMaxAttempts(maxAttempts int32) *agenticv1alpha1.ApprovalPolicy {
	return &agenticv1alpha1.ApprovalPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster"},
		Spec: agenticv1alpha1.ApprovalPolicySpec{
			MaxAttempts: maxAttempts,
			Stages: []agenticv1alpha1.ApprovalPolicyStage{
				{Name: agenticv1alpha1.SandboxStepAnalysis, Approval: agenticv1alpha1.ApprovalModeAutomatic},
				{Name: agenticv1alpha1.SandboxStepVerification, Approval: agenticv1alpha1.ApprovalModeAutomatic},
			},
		},
	}
}

// defaultObjects returns the standard set of cluster-scoped and namespaced
// objects needed to resolve a full workflow.
func defaultObjects() []client.Object {
	return []client.Object{
		testDefaultAgent(), testLLM("smart"), testAutoApprovePolicy(),
	}
}

func defaultObjectsWithMaxAttempts(maxAttempts int32) []client.Object {
	return []client.Object{
		testDefaultAgent(), testLLM("smart"), testAutoApprovePolicyWithMaxAttempts(maxAttempts),
	}
}

func reconcileOnce(r *ProposalReconciler, name string) (ctrl.Result, error) {
	return r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: name, Namespace: "default"},
	})
}

func getProposal(r *ProposalReconciler, name string) (*agenticv1alpha1.Proposal, error) {
	var p agenticv1alpha1.Proposal
	err := r.Get(context.Background(), types.NamespacedName{Name: name, Namespace: "default"}, &p)
	return &p, err
}

func approveProposal(t *testing.T, fc client.WithWatch, name string) {
	t.Helper()
	approveProposalWithOption(t, fc, name, 0)
}

func approveProposalWithOption(t *testing.T, fc client.WithWatch, name string, optionIndex int32) {
	t.Helper()
	var approval agenticv1alpha1.ProposalApproval
	if err := fc.Get(context.Background(), types.NamespacedName{Name: name, Namespace: "default"}, &approval); err != nil {
		t.Fatalf("get ProposalApproval for approval: %v", err)
	}
	base := approval.DeepCopy()
	hasExecution := false
	for i, s := range approval.Spec.Stages {
		if s.Type == agenticv1alpha1.ApprovalStageExecution {
			approval.Spec.Stages[i].Execution = agenticv1alpha1.ExecutionApproval{Option: &optionIndex}
			hasExecution = true
			break
		}
	}
	if !hasExecution {
		approval.Spec.Stages = append(approval.Spec.Stages, agenticv1alpha1.ApprovalStage{
			Type:      agenticv1alpha1.ApprovalStageExecution,
			Execution: agenticv1alpha1.ExecutionApproval{Option: &optionIndex},
		})
	}
	if err := fc.Patch(context.Background(), &approval, client.MergeFrom(base)); err != nil {
		t.Fatalf("approve execution with option %d: %v", optionIndex, err)
	}
}

// --- Sandbox-based reconciler helpers ---

func fakeBaseTemplate() *unstructured.Unstructured {
	return &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "extensions.agents.x-k8s.io/v1alpha1",
			"kind":       "SandboxTemplate",
			"metadata": map[string]any{
				"name":      defaultBaseTemplateName,
				"namespace": "test-ns",
			},
			"spec": map[string]any{
				"podTemplate": map[string]any{
					"spec": map[string]any{
						"serviceAccountName": "lightspeed-agent",
						"containers": []any{
							map[string]any{
								"name":  "agent",
								"image": "test-agent:latest",
								"env":   []any{},
							},
						},
						"volumes": []any{
							map[string]any{"name": "skills", "image": map[string]any{"reference": "placeholder:latest"}},
						},
					},
				},
			},
		},
	}
}

func newMockSandboxAgent(analysisJSON, executionJSON, verificationJSON string) (*SandboxAgentCaller, *mockSandboxProvider) {
	sandbox := &mockSandboxProvider{claimName: "ls-test-claim", endpoint: "http://sandbox:8080"}

	fc := fake.NewClientBuilder().WithScheme(testScheme()).Build()
	_ = fc.Create(context.Background(), fakeBaseTemplate())

	callCount := 0
	responses := []string{analysisJSON, executionJSON, verificationJSON}

	httpClient := &mockHTTPClient{}
	caller := &SandboxAgentCaller{
		Sandbox:   sandbox,
		K8sClient: fc,
		ClientFactory: func(_ string, _ time.Duration) AgentHTTPClientInterface {
			resp := responses[callCount%len(responses)]
			callCount++
			httpClient.response = &agentRunResponse{Response: json.RawMessage(resp)}
			return httpClient
		},
		Namespace: "test-ns",
		Timeout:   5 * time.Minute,
	}
	return caller, sandbox
}

// --- Reconciler-level tests ---

func TestReconcile_StatusInitialization(t *testing.T) {
	scheme := testScheme()
	proposal := &agenticv1alpha1.Proposal{
		ObjectMeta: metav1.ObjectMeta{Name: "fresh", Namespace: "default"},
		Spec: agenticv1alpha1.ProposalSpec{
			Request:      "Pod crashing",
			Tools:        testTools(),
			Analysis:     agenticv1alpha1.ProposalStep{Agent: "default"},
			Execution:    agenticv1alpha1.ProposalStep{Agent: "default"},
			Verification: agenticv1alpha1.ProposalStep{Agent: "default"},
		},
	}

	objs := append([]client.Object{proposal}, defaultObjects()...)
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
		WithStatusSubresource(proposal, &agenticv1alpha1.AnalysisResult{}, &agenticv1alpha1.ExecutionResult{}, &agenticv1alpha1.VerificationResult{}, &agenticv1alpha1.EscalationResult{}).Build()

	r := &ProposalReconciler{Client: fc, Log: logr.Discard(), Agent: newTestAgentCaller(), Namespace: "default"}

	_, err := reconcileOnce(r, "fresh")
	if err != nil {
		t.Fatalf("reconcile on nil status: %v", err)
	}

	p, _ := getProposal(r, "fresh")
	phase := agenticv1alpha1.DerivePhase(p.Status.Conditions)
	if phase != agenticv1alpha1.ProposalPhaseProposed {
		t.Fatalf("expected Proposed (analysis complete), got %s", phase)
	}
}

func TestReconcile_Denied_Terminal(t *testing.T) {
	scheme := testScheme()

	proposal := testProposal()
	proposal.Status = agenticv1alpha1.ProposalStatus{
		Conditions: []metav1.Condition{
			{Type: agenticv1alpha1.ProposalConditionAnalyzed, Status: metav1.ConditionTrue, Reason: "AnalysisComplete"},
			{Type: agenticv1alpha1.ProposalConditionDenied, Status: metav1.ConditionTrue, Reason: "UserDenied"},
		},
	}

	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(proposal).WithStatusSubresource(proposal, &agenticv1alpha1.AnalysisResult{}, &agenticv1alpha1.ExecutionResult{}, &agenticv1alpha1.VerificationResult{}, &agenticv1alpha1.EscalationResult{}).Build()
	r := &ProposalReconciler{Client: fc, Log: logr.Discard(), Agent: newTestAgentCaller(), Namespace: "default"}

	result, err := reconcileOnce(r, "fix-crash")
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if result.Requeue {
		t.Error("terminal phase should not requeue")
	}
	p, _ := getProposal(r, "fix-crash")
	if agenticv1alpha1.DerivePhase(p.Status.Conditions) != agenticv1alpha1.ProposalPhaseDenied {
		t.Fatalf("expected Denied, got %s", agenticv1alpha1.DerivePhase(p.Status.Conditions))
	}
}
