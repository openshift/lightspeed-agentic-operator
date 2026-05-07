package proposal

import (
	"context"
	"fmt"
	"testing"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
)

// testManualPolicy returns a policy with all stages set to Manual, matching the
// production default. Tests using this policy must explicitly approve every step.
func testManualPolicy() *agenticv1alpha1.ApprovalPolicy {
	return testPolicy(agenticv1alpha1.ApprovalModeManual, agenticv1alpha1.ApprovalModeManual, agenticv1alpha1.ApprovalModeManual)
}

func testPolicy(analysis, execution, verification agenticv1alpha1.ApprovalMode) *agenticv1alpha1.ApprovalPolicy {
	return testPolicyWithMaxAttempts(analysis, execution, verification, 0)
}

func testPolicyWithMaxAttempts(analysis, execution, verification agenticv1alpha1.ApprovalMode, maxAttempts int32) *agenticv1alpha1.ApprovalPolicy {
	return &agenticv1alpha1.ApprovalPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster"},
		Spec: agenticv1alpha1.ApprovalPolicySpec{
			MaxAttempts: maxAttempts,
			Stages: []agenticv1alpha1.ApprovalPolicyStage{
				{Name: agenticv1alpha1.SandboxStepAnalysis, Approval: analysis},
				{Name: agenticv1alpha1.SandboxStepExecution, Approval: execution},
				{Name: agenticv1alpha1.SandboxStepVerification, Approval: verification},
			},
		},
	}
}

func manualObjects() []client.Object {
	return []client.Object{testDefaultAgent(), testLLM("smart"), testManualPolicy()}
}

func newReconcilerWithPolicy(t *testing.T, proposal *agenticv1alpha1.Proposal, agent *testAgentCaller, policy *agenticv1alpha1.ApprovalPolicy, extraObjs ...client.Object) (*ProposalReconciler, client.WithWatch) {
	t.Helper()
	scheme := testScheme()
	objs := []client.Object{proposal, testDefaultAgent(), testLLM("smart")}
	if policy != nil {
		objs = append(objs, policy)
	}
	objs = append(objs, extraObjs...)
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
		WithStatusSubresource(proposal, &agenticv1alpha1.AnalysisResult{}, &agenticv1alpha1.ExecutionResult{}, &agenticv1alpha1.VerificationResult{}, &agenticv1alpha1.EscalationResult{}).Build()
	r := &ProposalReconciler{Client: fc, Log: logr.Discard(), Agent: agent}
	// Initial reconcile creates ProposalApproval (auto-approved stages based on policy).
	reconcileOnce(r, proposal.Name)
	return r, fc
}

func newManualReconciler(t *testing.T, proposal *agenticv1alpha1.Proposal, agent *testAgentCaller, extraObjs ...client.Object) (*ProposalReconciler, client.WithWatch) {
	t.Helper()
	return newReconcilerWithPolicy(t, proposal, agent, testManualPolicy(), extraObjs...)
}

func mustGetProposal(t *testing.T, r *ProposalReconciler, name string) *agenticv1alpha1.Proposal {
	t.Helper()
	p, err := getProposal(r, name)
	if err != nil {
		t.Fatalf("get proposal: %v", err)
	}
	return p
}

func assertPhase(t *testing.T, r *ProposalReconciler, name string, want agenticv1alpha1.ProposalPhase) *agenticv1alpha1.Proposal {
	t.Helper()
	p, err := getProposal(r, name)
	if err != nil {
		t.Fatalf("get proposal: %v", err)
	}
	got := agenticv1alpha1.DerivePhase(p.Status.Conditions)
	if got != want {
		t.Fatalf("expected phase %s, got %s (conditions: %v)", want, got, conditionSummary(p.Status.Conditions))
	}
	return p
}

func conditionSummary(conditions []metav1.Condition) string {
	s := ""
	for _, c := range conditions {
		if s != "" {
			s += ", "
		}
		s += fmt.Sprintf("%s=%s/%s", c.Type, c.Status, c.Reason)
	}
	return s
}

type approveOpts struct {
	agent  string
	option *int32
}

func approveStage(t *testing.T, fc client.WithWatch, name string, stageType agenticv1alpha1.ApprovalStageType, opts ...approveOpts) {
	t.Helper()
	var approval agenticv1alpha1.ProposalApproval
	if err := fc.Get(context.Background(), types.NamespacedName{Name: name, Namespace: "default"}, &approval); err != nil {
		t.Fatalf("get ProposalApproval: %v", err)
	}
	var o approveOpts
	if len(opts) > 0 {
		o = opts[0]
	}
	stage := agenticv1alpha1.ApprovalStage{Type: stageType}
	switch stageType {
	case agenticv1alpha1.ApprovalStageAnalysis:
		stage.Analysis = agenticv1alpha1.AnalysisApproval{Agent: o.agent}
	case agenticv1alpha1.ApprovalStageExecution:
		stage.Execution = agenticv1alpha1.ExecutionApproval{Option: o.option, Agent: o.agent}
	case agenticv1alpha1.ApprovalStageVerification:
		stage.Verification = agenticv1alpha1.VerificationApproval{Agent: o.agent}
	case agenticv1alpha1.ApprovalStageEscalation:
		stage.Escalation = agenticv1alpha1.EscalationApproval{Agent: o.agent}
	}
	base := approval.DeepCopy()
	approval.Spec.Stages = append(approval.Spec.Stages, stage)
	if err := fc.Patch(context.Background(), &approval, client.MergeFrom(base)); err != nil {
		t.Fatalf("approve %s: %v", stageType, err)
	}
}

func approveAnalysis(t *testing.T, fc client.WithWatch, name string, agent ...string) {
	t.Helper()
	var o approveOpts
	if len(agent) > 0 {
		o.agent = agent[0]
	}
	approveStage(t, fc, name, agenticv1alpha1.ApprovalStageAnalysis, o)
}

func approveExecution(t *testing.T, fc client.WithWatch, name string, option int32) {
	t.Helper()
	approveStage(t, fc, name, agenticv1alpha1.ApprovalStageExecution, approveOpts{option: &option})
}

func approveVerification(t *testing.T, fc client.WithWatch, name string) {
	t.Helper()
	approveStage(t, fc, name, agenticv1alpha1.ApprovalStageVerification)
}

func denyStage(t *testing.T, fc client.WithWatch, name string, stageType agenticv1alpha1.ApprovalStageType) {
	t.Helper()
	var approval agenticv1alpha1.ProposalApproval
	if err := fc.Get(context.Background(), types.NamespacedName{Name: name, Namespace: "default"}, &approval); err != nil {
		t.Fatalf("get ProposalApproval: %v", err)
	}
	base := approval.DeepCopy()
	stage := agenticv1alpha1.ApprovalStage{Type: stageType, Decision: agenticv1alpha1.ApprovalDecisionDenied}
	switch stageType {
	case agenticv1alpha1.ApprovalStageAnalysis:
		stage.Analysis = agenticv1alpha1.AnalysisApproval{}
	case agenticv1alpha1.ApprovalStageExecution:
		stage.Execution = agenticv1alpha1.ExecutionApproval{}
	case agenticv1alpha1.ApprovalStageVerification:
		stage.Verification = agenticv1alpha1.VerificationApproval{}
	case agenticv1alpha1.ApprovalStageEscalation:
		stage.Escalation = agenticv1alpha1.EscalationApproval{}
	}
	approval.Spec.Stages = append(approval.Spec.Stages, stage)
	if err := fc.Patch(context.Background(), &approval, client.MergeFrom(base)); err != nil {
		t.Fatalf("deny %s: %v", stageType, err)
	}
}

func mustRequeue(t *testing.T, result ctrl.Result, err error, context string) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: unexpected error: %v", context, err)
	}
	if !result.Requeue {
		t.Fatalf("%s: expected Requeue=true", context)
	}
}

func mustNotRequeue(t *testing.T, result ctrl.Result, err error, context string) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: unexpected error: %v", context, err)
	}
	if result.Requeue {
		t.Fatalf("%s: expected Requeue=false (waiting for approval)", context)
	}
}

// ---------------------------------------------------------------------------
// Happy Path: Full lifecycle with all-manual approval
// ---------------------------------------------------------------------------

func TestManualApproval_FullLifecycle(t *testing.T) {
	proposal := testProposal()
	agent := newTestAgentCaller()
	r, fc := newManualReconciler(t, proposal, agent)

	// After initial reconcile: Pending, analysis needs approval
	assertPhase(t, r, "fix-crash", agenticv1alpha1.ProposalPhasePending)

	// Approve analysis
	approveAnalysis(t, fc, "fix-crash")

	// Analysis runs → Proposed
	result, err := reconcileOnce(r, "fix-crash")
	mustNotRequeue(t, result, err, "after analysis approval")
	assertPhase(t, r, "fix-crash", agenticv1alpha1.ProposalPhaseProposed)

	// Reconcile 3: Proposed, execution needs approval — should not requeue
	result, err = reconcileOnce(r, "fix-crash")
	mustNotRequeue(t, result, err, "proposed waiting for execution approval")
	assertPhase(t, r, "fix-crash", agenticv1alpha1.ProposalPhaseProposed)

	// Approve execution
	approveExecution(t, fc, "fix-crash", 0)

	// Reconcile 4: execution runs → Verifying
	result, err = reconcileOnce(r, "fix-crash")
	mustNotRequeue(t, result, err, "after execution approval")
	p := assertPhase(t, r, "fix-crash", agenticv1alpha1.ProposalPhaseVerifying)
	executed := meta.FindStatusCondition(p.Status.Conditions, agenticv1alpha1.ProposalConditionExecuted)
	if executed == nil || executed.Status != metav1.ConditionTrue {
		t.Fatal("execution should have completed")
	}

	// Reconcile 5: Verifying, verification needs approval — should not requeue
	result, err = reconcileOnce(r, "fix-crash")
	mustNotRequeue(t, result, err, "verifying waiting for verification approval")
	assertPhase(t, r, "fix-crash", agenticv1alpha1.ProposalPhaseVerifying)

	// Approve verification
	approveVerification(t, fc, "fix-crash")

	// Reconcile 6: verification runs → Completed
	result, err = reconcileOnce(r, "fix-crash")
	if err != nil {
		t.Fatalf("verification reconcile: %v", err)
	}
	assertPhase(t, r, "fix-crash", agenticv1alpha1.ProposalPhaseCompleted)
}

// ---------------------------------------------------------------------------
// Proposed phase holds until execution approval
// ---------------------------------------------------------------------------

func TestManualApproval_ProposedWaitsForExecution(t *testing.T) {
	proposal := testProposal()
	agent := newTestAgentCaller()
	r, fc := newManualReconciler(t, proposal, agent)

	approveAnalysis(t, fc, "fix-crash")
	reconcileOnce(r, "fix-crash")
	assertPhase(t, r, "fix-crash", agenticv1alpha1.ProposalPhaseProposed)

	// Multiple reconciles without approving execution — should stay Proposed
	for i := 0; i < 3; i++ {
		result, err := reconcileOnce(r, "fix-crash")
		mustNotRequeue(t, result, err, fmt.Sprintf("proposed idle reconcile %d", i))
		assertPhase(t, r, "fix-crash", agenticv1alpha1.ProposalPhaseProposed)
	}

	// Approve execution → should progress
	approveExecution(t, fc, "fix-crash", 0)
	result, err := reconcileOnce(r, "fix-crash")
	mustNotRequeue(t, result, err, "execution after approval")
	assertPhase(t, r, "fix-crash", agenticv1alpha1.ProposalPhaseVerifying)
}

// ---------------------------------------------------------------------------
// Denial at each stage
// ---------------------------------------------------------------------------

func TestManualApproval_DenyAnalysis(t *testing.T) {
	proposal := testProposal()
	agent := newTestAgentCaller()
	r, fc := newManualReconciler(t, proposal, agent)

	// Deny analysis
	denyStage(t, fc, "fix-crash", agenticv1alpha1.ApprovalStageAnalysis)

	reconcileOnce(r, "fix-crash")
	assertPhase(t, r, "fix-crash", agenticv1alpha1.ProposalPhaseDenied)
}

func TestManualApproval_DenyExecution(t *testing.T) {
	proposal := testProposal()
	agent := newTestAgentCaller()
	r, fc := newManualReconciler(t, proposal, agent)

	// Approve analysis, run it
	approveAnalysis(t, fc, "fix-crash")
	reconcileOnce(r, "fix-crash")
	assertPhase(t, r, "fix-crash", agenticv1alpha1.ProposalPhaseProposed)

	// Deny execution
	denyStage(t, fc, "fix-crash", agenticv1alpha1.ApprovalStageExecution)

	reconcileOnce(r, "fix-crash")
	assertPhase(t, r, "fix-crash", agenticv1alpha1.ProposalPhaseDenied)
}

func TestManualApproval_DenyVerification(t *testing.T) {
	proposal := testProposal()
	agent := newTestAgentCaller()
	r, fc := newManualReconciler(t, proposal, agent)

	// Run through analysis and execution
	approveAnalysis(t, fc, "fix-crash")
	reconcileOnce(r, "fix-crash")
	approveExecution(t, fc, "fix-crash", 0)
	reconcileOnce(r, "fix-crash")
	assertPhase(t, r, "fix-crash", agenticv1alpha1.ProposalPhaseVerifying)

	// Deny verification
	denyStage(t, fc, "fix-crash", agenticv1alpha1.ApprovalStageVerification)

	reconcileOnce(r, "fix-crash")
	assertPhase(t, r, "fix-crash", agenticv1alpha1.ProposalPhaseDenied)
}

// ---------------------------------------------------------------------------
// Failures at each stage
// ---------------------------------------------------------------------------

func TestManualApproval_AnalysisFails(t *testing.T) {
	proposal := testProposal()
	agent := newTestAgentCaller()
	agent.analyzeErr = fmt.Errorf("LLM timeout")
	r, fc := newManualReconciler(t, proposal, agent)

	approveAnalysis(t, fc, "fix-crash")
	reconcileOnce(r, "fix-crash")
	assertPhase(t, r, "fix-crash", agenticv1alpha1.ProposalPhaseFailed)
}

func TestManualApproval_ExecutionFails(t *testing.T) {
	proposal := testProposal()
	agent := newTestAgentCaller()
	agent.executeErr = fmt.Errorf("sandbox crashed")
	r, fc := newManualReconciler(t, proposal, agent)

	approveAnalysis(t, fc, "fix-crash")
	reconcileOnce(r, "fix-crash")
	approveExecution(t, fc, "fix-crash", 0)
	reconcileOnce(r, "fix-crash")
	assertPhase(t, r, "fix-crash", agenticv1alpha1.ProposalPhaseFailed)
}

func TestManualApproval_VerificationFails(t *testing.T) {
	proposal := testProposal()
	agent := newTestAgentCaller()
	agent.verifyErr = fmt.Errorf("verification timed out")
	r, fc := newManualReconciler(t, proposal, agent)

	approveAnalysis(t, fc, "fix-crash")
	reconcileOnce(r, "fix-crash")
	approveExecution(t, fc, "fix-crash", 0)
	reconcileOnce(r, "fix-crash")
	approveVerification(t, fc, "fix-crash")
	reconcileOnce(r, "fix-crash")
	assertPhase(t, r, "fix-crash", agenticv1alpha1.ProposalPhaseFailed)
}

// ---------------------------------------------------------------------------
// Verification failure triggers retry back to Executing
// ---------------------------------------------------------------------------

func TestManualApproval_VerificationFailRetry(t *testing.T) {
	proposal := testProposal()
	agent := newTestAgentCaller()
	policy := testPolicyWithMaxAttempts(agenticv1alpha1.ApprovalModeManual, agenticv1alpha1.ApprovalModeManual, agenticv1alpha1.ApprovalModeManual, 3)
	r, fc := newReconcilerWithPolicy(t, proposal, agent, policy)

	// Analysis → Proposed → Executing → Verifying
	approveAnalysis(t, fc, "fix-crash")
	reconcileOnce(r, "fix-crash")
	approveExecution(t, fc, "fix-crash", 0)
	reconcileOnce(r, "fix-crash")
	assertPhase(t, r, "fix-crash", agenticv1alpha1.ProposalPhaseVerifying)

	// Make verification fail (not a system error — objective failure)
	agent.verifyResult = &VerificationOutput{
		Success: false,
		Summary: "Pod still crashing",
		Checks:  []agenticv1alpha1.VerifyCheck{{Name: "pod-running", Result: agenticv1alpha1.CheckResultFailed}},
	}
	approveVerification(t, fc, "fix-crash")
	reconcileOnce(r, "fix-crash")

	// Should retry → Executing (Verified=False/RetryingExecution)
	assertPhase(t, r, "fix-crash", agenticv1alpha1.ProposalPhaseExecuting)
	p, _ := getProposal(r, "fix-crash")
	if p.Status.Steps.Execution.RetryCount == nil || *p.Status.Steps.Execution.RetryCount != 1 {
		t.Fatalf("expected retryCount=1, got %v", p.Status.Steps.Execution.RetryCount)
	}
}

func TestManualApproval_FullRetryExhaustion(t *testing.T) {
	proposal := testProposal()
	agent := newTestAgentCaller()
	policy := testPolicyWithMaxAttempts(agenticv1alpha1.ApprovalModeManual, agenticv1alpha1.ApprovalModeManual, agenticv1alpha1.ApprovalModeManual, 3)
	r, fc := newReconcilerWithPolicy(t, proposal, agent, policy)

	// Run through to Verifying
	approveAnalysis(t, fc, "fix-crash")
	reconcileOnce(r, "fix-crash")
	approveExecution(t, fc, "fix-crash", 0)
	reconcileOnce(r, "fix-crash")
	assertPhase(t, r, "fix-crash", agenticv1alpha1.ProposalPhaseVerifying)

	// Verification keeps failing across all retries
	agent.verifyResult = &VerificationOutput{
		Success: false,
		Summary: "Pod still crashing",
		Checks:  []agenticv1alpha1.VerifyCheck{{Name: "pod-running", Result: agenticv1alpha1.CheckResultFailed}},
	}

	// Approve verification once — approval persists across retries
	approveVerification(t, fc, "fix-crash")

	// Attempt 1 (of 3): verify fails → Executing (retryCount=1)
	reconcileOnce(r, "fix-crash")
	assertPhase(t, r, "fix-crash", agenticv1alpha1.ProposalPhaseExecuting)
	p, _ := getProposal(r, "fix-crash")
	if *p.Status.Steps.Execution.RetryCount != 1 {
		t.Fatalf("expected retryCount=1, got %d", *p.Status.Steps.Execution.RetryCount)
	}

	// Re-execute → Verifying
	reconcileOnce(r, "fix-crash")
	assertPhase(t, r, "fix-crash", agenticv1alpha1.ProposalPhaseVerifying)

	// Attempt 2 (of 3): verify fails again → Executing (retryCount=2)
	reconcileOnce(r, "fix-crash")
	assertPhase(t, r, "fix-crash", agenticv1alpha1.ProposalPhaseExecuting)
	p, _ = getProposal(r, "fix-crash")
	if *p.Status.Steps.Execution.RetryCount != 2 {
		t.Fatalf("expected retryCount=2, got %d", *p.Status.Steps.Execution.RetryCount)
	}

	// Re-execute → Verifying
	reconcileOnce(r, "fix-crash")
	assertPhase(t, r, "fix-crash", agenticv1alpha1.ProposalPhaseVerifying)

	// Attempt 3 (of 3): verify fails → retries exhausted (retryCount=2 == maxAttempts-1)
	// → Escalating (escalation step injected)
	reconcileOnce(r, "fix-crash")
	assertPhase(t, r, "fix-crash", agenticv1alpha1.ProposalPhaseEscalating)
}

func TestManualApproval_RetryThenSucceed(t *testing.T) {
	proposal := testProposal()
	agent := newTestAgentCaller()
	policy := testPolicyWithMaxAttempts(agenticv1alpha1.ApprovalModeManual, agenticv1alpha1.ApprovalModeManual, agenticv1alpha1.ApprovalModeManual, 3)
	r, fc := newReconcilerWithPolicy(t, proposal, agent, policy)

	// Run through to Verifying
	approveAnalysis(t, fc, "fix-crash")
	reconcileOnce(r, "fix-crash")
	approveExecution(t, fc, "fix-crash", 0)
	reconcileOnce(r, "fix-crash")
	approveVerification(t, fc, "fix-crash")

	// First verification fails
	agent.verifyResult = &VerificationOutput{
		Success: false,
		Summary: "Pod still crashing",
		Checks:  []agenticv1alpha1.VerifyCheck{{Name: "pod-running", Result: agenticv1alpha1.CheckResultFailed}},
	}
	reconcileOnce(r, "fix-crash")
	assertPhase(t, r, "fix-crash", agenticv1alpha1.ProposalPhaseExecuting)

	// Re-execute succeeds, now make verification pass
	agent.verifyResult = &VerificationOutput{
		Success: true,
		Summary: "All checks passed",
		Checks:  []agenticv1alpha1.VerifyCheck{{Name: "pod-running", Result: agenticv1alpha1.CheckResultPassed}},
	}
	reconcileOnce(r, "fix-crash") // re-execute → Verifying
	reconcileOnce(r, "fix-crash") // verify → Completed
	assertPhase(t, r, "fix-crash", agenticv1alpha1.ProposalPhaseCompleted)

	p, _ := getProposal(r, "fix-crash")
	if *p.Status.Steps.Execution.RetryCount != 1 {
		t.Fatalf("expected retryCount=1, got %d", *p.Status.Steps.Execution.RetryCount)
	}
}

// ---------------------------------------------------------------------------
// No approval policy → all stages default to Manual
// ---------------------------------------------------------------------------

func TestNoPolicy_DefaultsToManual(t *testing.T) {
	proposal := testProposal()
	agent := newTestAgentCaller()

	scheme := testScheme()
	// No ApprovalPolicy object at all
	objs := []client.Object{proposal, testDefaultAgent(), testLLM("smart")}
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
		WithStatusSubresource(proposal, &agenticv1alpha1.AnalysisResult{}, &agenticv1alpha1.ExecutionResult{}, &agenticv1alpha1.VerificationResult{}, &agenticv1alpha1.EscalationResult{}).Build()
	r := &ProposalReconciler{Client: fc, Log: logr.Discard(), Agent: agent}

	// Initial reconcile creates ProposalApproval; analysis should wait for approval
	reconcileOnce(r, "fix-crash")
	assertPhase(t, r, "fix-crash", agenticv1alpha1.ProposalPhasePending)

	// Approve analysis manually
	approveAnalysis(t, fc, "fix-crash")
	reconcileOnce(r, "fix-crash")
	assertPhase(t, r, "fix-crash", agenticv1alpha1.ProposalPhaseProposed)

	// Execution should also wait
	result, err := reconcileOnce(r, "fix-crash")
	mustNotRequeue(t, result, err, "no policy, execution pending")
	assertPhase(t, r, "fix-crash", agenticv1alpha1.ProposalPhaseProposed)
}

// ---------------------------------------------------------------------------
// Advisory-only (no execution, no verification) still needs analysis approval
// ---------------------------------------------------------------------------

func TestManualApproval_AdvisoryOnly(t *testing.T) {
	proposal := &agenticv1alpha1.Proposal{
		ObjectMeta: metav1.ObjectMeta{Name: "fix-crash", Namespace: "default"},
		Spec: agenticv1alpha1.ProposalSpec{
			Request:          "Investigate issue",
			Tools:            testTools(),
			TargetNamespaces: []string{"production"},
			Analysis:         agenticv1alpha1.ProposalStep{Agent: "default"},
		},
	}
	agent := newTestAgentCaller()
	r, fc := newManualReconciler(t, proposal, agent)
	assertPhase(t, r, "fix-crash", agenticv1alpha1.ProposalPhasePending)

	// Approve analysis
	approveAnalysis(t, fc, "fix-crash")
	reconcileOnce(r, "fix-crash")
	assertPhase(t, r, "fix-crash", agenticv1alpha1.ProposalPhaseProposed)

	// Proposed → execution step is nil → skips to Completed
	reconcileOnce(r, "fix-crash")
	assertPhase(t, r, "fix-crash", agenticv1alpha1.ProposalPhaseCompleted)
}

// ---------------------------------------------------------------------------
// Trust mode (no verification) needs manual analysis + execution approval
// ---------------------------------------------------------------------------

func TestManualApproval_TrustMode(t *testing.T) {
	proposal := &agenticv1alpha1.Proposal{
		ObjectMeta: metav1.ObjectMeta{Name: "fix-crash", Namespace: "default"},
		Spec: agenticv1alpha1.ProposalSpec{
			Request:          "Fix with trust",
			Tools:            testTools(),
			TargetNamespaces: []string{"production"},
			Analysis:         agenticv1alpha1.ProposalStep{Agent: "default"},
			Execution:        agenticv1alpha1.ProposalStep{Agent: "default"},
		},
	}
	agent := newTestAgentCaller()
	r, fc := newManualReconciler(t, proposal, agent)

	approveAnalysis(t, fc, "fix-crash")
	reconcileOnce(r, "fix-crash")
	assertPhase(t, r, "fix-crash", agenticv1alpha1.ProposalPhaseProposed)

	approveExecution(t, fc, "fix-crash", 0)
	reconcileOnce(r, "fix-crash")

	// No verification step → Completed
	assertPhase(t, r, "fix-crash", agenticv1alpha1.ProposalPhaseCompleted)
}

// ---------------------------------------------------------------------------
// Manual policy: ProposalApproval starts with no auto-approved stages
// ---------------------------------------------------------------------------

func TestManualApproval_NoAutoApprovedStages(t *testing.T) {
	proposal := testProposal()
	agent := newTestAgentCaller()
	_, fc := newManualReconciler(t, proposal, agent)

	var approval agenticv1alpha1.ProposalApproval
	if err := fc.Get(context.Background(), types.NamespacedName{Name: "fix-crash", Namespace: "default"}, &approval); err != nil {
		t.Fatalf("get ProposalApproval: %v", err)
	}

	if len(approval.Spec.Stages) != 0 {
		t.Fatalf("all-manual policy should create 0 auto-approved stages, got %d", len(approval.Spec.Stages))
	}
}

// ---------------------------------------------------------------------------
// Execution not reported as success → fails
// ---------------------------------------------------------------------------

func TestManualApproval_ExecutionReportsFailure(t *testing.T) {
	proposal := testProposal()
	agent := newTestAgentCaller()
	agent.executeResult = &ExecutionOutput{Success: false}
	r, fc := newManualReconciler(t, proposal, agent)

	approveAnalysis(t, fc, "fix-crash")
	reconcileOnce(r, "fix-crash")
	approveExecution(t, fc, "fix-crash", 0)
	reconcileOnce(r, "fix-crash")
	assertPhase(t, r, "fix-crash", agenticv1alpha1.ProposalPhaseFailed)
}

// ---------------------------------------------------------------------------
// Verification objective failure without maxAttempts → terminal Failed
// ---------------------------------------------------------------------------

func TestManualApproval_VerificationFailDefaultOneAttempt(t *testing.T) {
	proposal := testProposal()
	// No maxAttempts on policy → defaults to 1 (one attempt, no retries)
	agent := newTestAgentCaller()
	agent.verifyResult = &VerificationOutput{
		Success: false,
		Summary: "Pod still crashing",
	}
	r, fc := newManualReconciler(t, proposal, agent)

	approveAnalysis(t, fc, "fix-crash")
	reconcileOnce(r, "fix-crash")
	approveExecution(t, fc, "fix-crash", 0)
	reconcileOnce(r, "fix-crash")
	approveVerification(t, fc, "fix-crash")
	reconcileOnce(r, "fix-crash")

	// maxAttempts=1 → 1 total attempt, no retries → escalate immediately
	assertPhase(t, r, "fix-crash", agenticv1alpha1.ProposalPhaseEscalating)
}

// ---------------------------------------------------------------------------
// Agent override from approval stage is respected
// ---------------------------------------------------------------------------

func TestManualApproval_AgentOverride(t *testing.T) {
	proposal := testProposal()
	agent := newTestAgentCaller()
	fastAgent := &agenticv1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "fast"},
		Spec:       agenticv1alpha1.AgentSpec{LLMProvider: agenticv1alpha1.LLMProviderReference{Name: "smart"}, Model: "claude-haiku-4-5"},
	}
	r, fc := newManualReconciler(t, proposal, agent, fastAgent)

	// Approve analysis with "fast" agent override
	approveAnalysis(t, fc, "fix-crash", "fast")

	result, err := reconcileOnce(r, "fix-crash")
	mustNotRequeue(t, result, err, "analysis with override")
	assertPhase(t, r, "fix-crash", agenticv1alpha1.ProposalPhaseProposed)
}

// ---------------------------------------------------------------------------
// DerivePhase truth table — comprehensive phase derivation coverage
// ---------------------------------------------------------------------------

func TestDerivePhase_ProposedVsExecuting(t *testing.T) {
	tests := []struct {
		name       string
		conditions []metav1.Condition
		want       agenticv1alpha1.ProposalPhase
	}{
		{
			name:       "no conditions → Pending",
			conditions: nil,
			want:       agenticv1alpha1.ProposalPhasePending,
		},
		{
			name: "Analyzed=Unknown → Analyzing",
			conditions: []metav1.Condition{
				{Type: agenticv1alpha1.ProposalConditionAnalyzed, Status: metav1.ConditionUnknown},
			},
			want: agenticv1alpha1.ProposalPhaseAnalyzing,
		},
		{
			name: "Analyzed=True only → Proposed (not Executing)",
			conditions: []metav1.Condition{
				{Type: agenticv1alpha1.ProposalConditionAnalyzed, Status: metav1.ConditionTrue},
			},
			want: agenticv1alpha1.ProposalPhaseProposed,
		},
		{
			name: "Analyzed=True + Executed=Unknown → Executing",
			conditions: []metav1.Condition{
				{Type: agenticv1alpha1.ProposalConditionAnalyzed, Status: metav1.ConditionTrue},
				{Type: agenticv1alpha1.ProposalConditionExecuted, Status: metav1.ConditionUnknown},
			},
			want: agenticv1alpha1.ProposalPhaseExecuting,
		},
		{
			name: "Analyzed=True + Executed=True → Verifying",
			conditions: []metav1.Condition{
				{Type: agenticv1alpha1.ProposalConditionAnalyzed, Status: metav1.ConditionTrue},
				{Type: agenticv1alpha1.ProposalConditionExecuted, Status: metav1.ConditionTrue},
			},
			want: agenticv1alpha1.ProposalPhaseVerifying,
		},
		{
			name: "Analyzed=True + Verified=False/RetryingExecution → Executing (retry)",
			conditions: []metav1.Condition{
				{Type: agenticv1alpha1.ProposalConditionAnalyzed, Status: metav1.ConditionTrue},
				{Type: agenticv1alpha1.ProposalConditionVerified, Status: metav1.ConditionFalse, Reason: agenticv1alpha1.ReasonRetryingExecution},
			},
			want: agenticv1alpha1.ProposalPhaseExecuting,
		},
		{
			name: "Analyzed=False → Failed",
			conditions: []metav1.Condition{
				{Type: agenticv1alpha1.ProposalConditionAnalyzed, Status: metav1.ConditionFalse},
			},
			want: agenticv1alpha1.ProposalPhaseFailed,
		},
		{
			name: "Denied=True → Denied (regardless of other conditions)",
			conditions: []metav1.Condition{
				{Type: agenticv1alpha1.ProposalConditionAnalyzed, Status: metav1.ConditionTrue},
				{Type: agenticv1alpha1.ProposalConditionDenied, Status: metav1.ConditionTrue},
			},
			want: agenticv1alpha1.ProposalPhaseDenied,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := agenticv1alpha1.DerivePhase(tt.conditions)
			if got != tt.want {
				t.Errorf("DerivePhase() = %q, want %q", got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Approval policy combinations: all 8 permutations of Auto/Manual x 3 stages
// ---------------------------------------------------------------------------

func TestPolicyCombinations_FullLifecycle(t *testing.T) {
	A := agenticv1alpha1.ApprovalModeAutomatic
	M := agenticv1alpha1.ApprovalModeManual

	tests := []struct {
		name       string
		analysis   agenticv1alpha1.ApprovalMode
		execution  agenticv1alpha1.ApprovalMode
		verify     agenticv1alpha1.ApprovalMode
	}{
		{"AAA", A, A, A},
		{"AAM", A, A, M},
		{"AMA", A, M, A},
		{"AMM", A, M, M},
		{"MAA", M, A, A},
		{"MAM", M, A, M},
		{"MMA", M, M, A},
		{"MMM", M, M, M},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			proposal := testProposal()
			agent := newTestAgentCaller()
			policy := testPolicy(tt.analysis, tt.execution, tt.verify)
			r, fc := newReconcilerWithPolicy(t, proposal, agent, policy)

			// After initial reconcile: auto stages should already be in the approval
			var approval agenticv1alpha1.ProposalApproval
			if err := fc.Get(context.Background(), types.NamespacedName{Name: "fix-crash", Namespace: "default"}, &approval); err != nil {
				t.Fatalf("get ProposalApproval: %v", err)
			}
			autoCount := 0
			if tt.analysis == A {
				autoCount++
			}
			if tt.execution == A {
				autoCount++
			}
			if tt.verify == A {
				autoCount++
			}
			if len(approval.Spec.Stages) != autoCount {
				t.Fatalf("expected %d auto-approved stages, got %d", autoCount, len(approval.Spec.Stages))
			}

			// Analysis: approve manually if needed, then reconcile
			if tt.analysis == M {
				assertPhase(t, r, "fix-crash", agenticv1alpha1.ProposalPhasePending)
				approveAnalysis(t, fc, "fix-crash")
			}
			reconcileOnce(r, "fix-crash")

			if tt.execution == A {
				// Auto-approved execution runs in same reconcile cycle after analysis
				// — skips Proposed, goes straight to Verifying (or needs another reconcile)
				phase := agenticv1alpha1.DerivePhase(mustGetProposal(t, r, "fix-crash").Status.Conditions)
				if phase == agenticv1alpha1.ProposalPhaseProposed {
					reconcileOnce(r, "fix-crash")
				}
			} else {
				assertPhase(t, r, "fix-crash", agenticv1alpha1.ProposalPhaseProposed)
				result, err := reconcileOnce(r, "fix-crash")
				mustNotRequeue(t, result, err, "execution pending")
				approveExecution(t, fc, "fix-crash", 0)
				reconcileOnce(r, "fix-crash")
			}
			assertPhase(t, r, "fix-crash", agenticv1alpha1.ProposalPhaseVerifying)

			// Verification: approve manually if needed, then reconcile
			if tt.verify == M {
				result, err := reconcileOnce(r, "fix-crash")
				mustNotRequeue(t, result, err, "verification pending")
				assertPhase(t, r, "fix-crash", agenticv1alpha1.ProposalPhaseVerifying)
				approveVerification(t, fc, "fix-crash")
			}
			reconcileOnce(r, "fix-crash")
			assertPhase(t, r, "fix-crash", agenticv1alpha1.ProposalPhaseCompleted)
		})
	}
}

// ---------------------------------------------------------------------------
// Policy change after ProposalApproval creation (the fallback path)
// ---------------------------------------------------------------------------

func approveEscalation(t *testing.T, fc client.WithWatch, name string) {
	t.Helper()
	approveStage(t, fc, name, agenticv1alpha1.ApprovalStageEscalation)
}

// ---------------------------------------------------------------------------
// Escalation: approve and complete
// ---------------------------------------------------------------------------

func TestEscalation_ApproveAndComplete(t *testing.T) {
	proposal := testProposal()
	agent := newTestAgentCaller()
	agent.verifyResult = &VerificationOutput{
		Success: false,
		Summary: "Pod still crashing",
		Checks:  []agenticv1alpha1.VerifyCheck{{Name: "pod-running", Result: agenticv1alpha1.CheckResultFailed}},
	}
	policy := testPolicyWithMaxAttempts(agenticv1alpha1.ApprovalModeManual, agenticv1alpha1.ApprovalModeManual, agenticv1alpha1.ApprovalModeManual, 1)
	r, fc := newReconcilerWithPolicy(t, proposal, agent, policy)

	// Run through to verification failure → retry → exhaustion → Escalating
	approveAnalysis(t, fc, "fix-crash")
	reconcileOnce(r, "fix-crash")
	approveExecution(t, fc, "fix-crash", 0)
	reconcileOnce(r, "fix-crash")
	approveVerification(t, fc, "fix-crash")
	reconcileOnce(r, "fix-crash") // verify fails, maxAttempts=1 → escalate immediately
	assertPhase(t, r, "fix-crash", agenticv1alpha1.ProposalPhaseEscalating)

	// Approve escalation
	approveEscalation(t, fc, "fix-crash")
	reconcileOnce(r, "fix-crash")

	// Should be terminal Escalated
	p := assertPhase(t, r, "fix-crash", agenticv1alpha1.ProposalPhaseEscalated)
	if len(p.Status.Steps.Escalation.Results) == 0 {
		t.Fatal("expected EscalationResult ref in status")
	}
	if p.Status.Steps.Escalation.Results[0].Outcome != agenticv1alpha1.ActionOutcomeSucceeded {
		t.Fatal("expected escalation result to be successful")
	}
}

// ---------------------------------------------------------------------------
// Escalation: denied
// ---------------------------------------------------------------------------

func TestEscalation_Denied(t *testing.T) {
	proposal := testProposal()
	agent := newTestAgentCaller()
	agent.verifyResult = &VerificationOutput{Success: false, Summary: "fail"}
	r, fc := newManualReconciler(t, proposal, agent)

	approveAnalysis(t, fc, "fix-crash")
	reconcileOnce(r, "fix-crash")
	approveExecution(t, fc, "fix-crash", 0)
	reconcileOnce(r, "fix-crash")
	approveVerification(t, fc, "fix-crash")
	reconcileOnce(r, "fix-crash")
	assertPhase(t, r, "fix-crash", agenticv1alpha1.ProposalPhaseEscalating)

	denyStage(t, fc, "fix-crash", agenticv1alpha1.ApprovalStageEscalation)
	reconcileOnce(r, "fix-crash")
	assertPhase(t, r, "fix-crash", agenticv1alpha1.ProposalPhaseDenied)
}

// ---------------------------------------------------------------------------
// Escalation: agent failure
// ---------------------------------------------------------------------------

func TestEscalation_AgentFailure(t *testing.T) {
	proposal := testProposal()
	agent := newTestAgentCaller()
	agent.verifyResult = &VerificationOutput{Success: false, Summary: "fail"}
	agent.escalateErr = fmt.Errorf("escalation agent crashed")
	r, fc := newManualReconciler(t, proposal, agent)

	approveAnalysis(t, fc, "fix-crash")
	reconcileOnce(r, "fix-crash")
	approveExecution(t, fc, "fix-crash", 0)
	reconcileOnce(r, "fix-crash")
	approveVerification(t, fc, "fix-crash")
	reconcileOnce(r, "fix-crash")
	assertPhase(t, r, "fix-crash", agenticv1alpha1.ProposalPhaseEscalating)

	approveEscalation(t, fc, "fix-crash")
	reconcileOnce(r, "fix-crash")
	assertPhase(t, r, "fix-crash", agenticv1alpha1.ProposalPhaseFailed)
}

// ---------------------------------------------------------------------------
// Escalation: auto-approve via policy
// ---------------------------------------------------------------------------

func TestEscalation_AutoApprove(t *testing.T) {
	proposal := testProposal()
	agent := newTestAgentCaller()
	agent.verifyResult = &VerificationOutput{Success: false, Summary: "fail"}

	policy := &agenticv1alpha1.ApprovalPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster"},
		Spec: agenticv1alpha1.ApprovalPolicySpec{
			Stages: []agenticv1alpha1.ApprovalPolicyStage{
				{Name: agenticv1alpha1.SandboxStepAnalysis, Approval: agenticv1alpha1.ApprovalModeAutomatic},
				{Name: agenticv1alpha1.SandboxStepExecution, Approval: agenticv1alpha1.ApprovalModeManual},
				{Name: agenticv1alpha1.SandboxStepVerification, Approval: agenticv1alpha1.ApprovalModeAutomatic},
				{Name: agenticv1alpha1.SandboxStepEscalation, Approval: agenticv1alpha1.ApprovalModeAutomatic},
			},
		},
	}
	r, fc := newReconcilerWithPolicy(t, proposal, agent, policy)

	// Analysis auto-approved → Proposed
	reconcileOnce(r, "fix-crash")
	assertPhase(t, r, "fix-crash", agenticv1alpha1.ProposalPhaseProposed)

	approveExecution(t, fc, "fix-crash", 0)
	reconcileOnce(r, "fix-crash") // execute → Verifying
	reconcileOnce(r, "fix-crash") // verify fails → Escalating
	assertPhase(t, r, "fix-crash", agenticv1alpha1.ProposalPhaseEscalating)

	// Escalation is auto-approved, should run and complete
	reconcileOnce(r, "fix-crash")
	assertPhase(t, r, "fix-crash", agenticv1alpha1.ProposalPhaseEscalated)
}

// ---------------------------------------------------------------------------
// Escalation: re-reconcile while in progress is a no-op
// ---------------------------------------------------------------------------

func TestEscalation_InProgressIsIdempotent(t *testing.T) {
	proposal := testProposal()
	agent := newTestAgentCaller()
	agent.verifyResult = &VerificationOutput{
		Success: false,
		Summary: "Pod still crashing",
		Checks:  []agenticv1alpha1.VerifyCheck{{Name: "pod-running", Result: agenticv1alpha1.CheckResultFailed}},
	}
	policy := testPolicyWithMaxAttempts(agenticv1alpha1.ApprovalModeManual, agenticv1alpha1.ApprovalModeManual, agenticv1alpha1.ApprovalModeManual, 1)
	r, fc := newReconcilerWithPolicy(t, proposal, agent, policy)

	// Drive to Escalating phase
	approveAnalysis(t, fc, "fix-crash")
	reconcileOnce(r, "fix-crash")
	approveExecution(t, fc, "fix-crash", 0)
	reconcileOnce(r, "fix-crash")
	approveVerification(t, fc, "fix-crash")
	reconcileOnce(r, "fix-crash") // verify fails, retry
	reconcileOnce(r, "fix-crash") // re-execute
	reconcileOnce(r, "fix-crash") // verify fails again, retries exhausted → Escalating
	assertPhase(t, r, "fix-crash", agenticv1alpha1.ProposalPhaseEscalating)

	// Approve escalation and run it
	approveEscalation(t, fc, "fix-crash")
	reconcileOnce(r, "fix-crash")
	p := assertPhase(t, r, "fix-crash", agenticv1alpha1.ProposalPhaseEscalated)

	// Record how many EscalationResult refs exist
	resultCount := len(p.Status.Steps.Escalation.Results)
	if resultCount != 1 {
		t.Fatalf("expected exactly 1 escalation result, got %d", resultCount)
	}

	// Re-reconcile: should be a no-op, no additional results
	reconcileOnce(r, "fix-crash")
	p = assertPhase(t, r, "fix-crash", agenticv1alpha1.ProposalPhaseEscalated)
	if len(p.Status.Steps.Escalation.Results) != resultCount {
		t.Fatalf("re-reconcile created duplicate results: got %d, want %d",
			len(p.Status.Steps.Escalation.Results), resultCount)
	}
}

// ---------------------------------------------------------------------------
// Policy change after ProposalApproval creation (the fallback path)
// ---------------------------------------------------------------------------

func TestPolicyChange_ManualToAutomatic(t *testing.T) {
	proposal := testProposal()
	agent := newTestAgentCaller()

	// Start with all-manual policy
	r, fc := newManualReconciler(t, proposal, agent)
	assertPhase(t, r, "fix-crash", agenticv1alpha1.ProposalPhasePending)

	// Analysis is manual — verify it blocks
	result, err := reconcileOnce(r, "fix-crash")
	mustNotRequeue(t, result, err, "analysis pending with manual policy")

	// Change the policy to auto-approve analysis
	var policy agenticv1alpha1.ApprovalPolicy
	if err := fc.Get(context.Background(), types.NamespacedName{Name: "cluster"}, &policy); err != nil {
		t.Fatalf("get policy: %v", err)
	}
	base := policy.DeepCopy()
	policy.Spec.Stages[0].Approval = agenticv1alpha1.ApprovalModeAutomatic
	if err := fc.Patch(context.Background(), &policy, client.MergeFrom(base)); err != nil {
		t.Fatalf("patch policy: %v", err)
	}

	// Reconcile — analysis should now proceed via policy fallback in isStageApproved
	reconcileOnce(r, "fix-crash")
	assertPhase(t, r, "fix-crash", agenticv1alpha1.ProposalPhaseProposed)
}
