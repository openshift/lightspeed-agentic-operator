package agenticrun

import (
	"context"
	"fmt"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
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

func newReconcilerWithPolicy(t *testing.T, run *agenticv1alpha1.AgenticRun, agent *testAgentCaller, policy *agenticv1alpha1.ApprovalPolicy, extraObjs ...client.Object) (*AgenticRunReconciler, client.WithWatch) {
	t.Helper()
	scheme := testScheme()
	objs := []client.Object{run, testDefaultAgent(), testLLM("smart")}
	if policy != nil {
		objs = append(objs, policy)
	}
	objs = append(objs, extraObjs...)
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
		WithStatusSubresource(run, &agenticv1alpha1.AnalysisResult{}, &agenticv1alpha1.ExecutionResult{}, &agenticv1alpha1.VerificationResult{}, &agenticv1alpha1.EscalationResult{}).Build()
	r := &AgenticRunReconciler{Client: fc, Agent: agent, Namespace: "default"}
	// Initial reconcile creates AgenticRunApproval (auto-approved stages based on policy).
	reconcileOnce(r, run.Name)
	return r, fc
}

func newManualReconciler(t *testing.T, run *agenticv1alpha1.AgenticRun, agent *testAgentCaller, extraObjs ...client.Object) (*AgenticRunReconciler, client.WithWatch) {
	t.Helper()
	return newReconcilerWithPolicy(t, run, agent, testManualPolicy(), extraObjs...)
}

func mustGetAgenticRun(t *testing.T, r *AgenticRunReconciler, name string) *agenticv1alpha1.AgenticRun {
	t.Helper()
	p, err := getAgenticRun(r, name)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	return p
}

func assertPhase(t *testing.T, r *AgenticRunReconciler, name string, want agenticv1alpha1.AgenticRunPhase) *agenticv1alpha1.AgenticRun {
	t.Helper()
	p, err := getAgenticRun(r, name)
	if err != nil {
		t.Fatalf("get run: %v", err)
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
	var approval agenticv1alpha1.AgenticRunApproval
	if err := fc.Get(context.Background(), types.NamespacedName{Name: name, Namespace: "default"}, &approval); err != nil {
		t.Fatalf("get AgenticRunApproval: %v", err)
	}
	var o approveOpts
	if len(opts) > 0 {
		o = opts[0]
	}
	stage := agenticv1alpha1.ApprovalStage{Type: stageType}
	switch stageType {
	case agenticv1alpha1.ApprovalStageAnalysis:
		stage.Analysis = &agenticv1alpha1.AnalysisApproval{Agent: o.agent}
	case agenticv1alpha1.ApprovalStageExecution:
		stage.Execution = &agenticv1alpha1.ExecutionApproval{Option: o.option, Agent: o.agent}
	case agenticv1alpha1.ApprovalStageVerification:
		stage.Verification = &agenticv1alpha1.VerificationApproval{Agent: o.agent}
	case agenticv1alpha1.ApprovalStageEscalation:
		stage.Escalation = &agenticv1alpha1.EscalationApproval{Agent: o.agent}
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
	var approval agenticv1alpha1.AgenticRunApproval
	if err := fc.Get(context.Background(), types.NamespacedName{Name: name, Namespace: "default"}, &approval); err != nil {
		t.Fatalf("get AgenticRunApproval: %v", err)
	}
	base := approval.DeepCopy()
	stage := agenticv1alpha1.ApprovalStage{Type: stageType, Decision: agenticv1alpha1.ApprovalDecisionDenied}
	switch stageType {
	case agenticv1alpha1.ApprovalStageAnalysis:
		stage.Analysis = &agenticv1alpha1.AnalysisApproval{}
	case agenticv1alpha1.ApprovalStageExecution:
		stage.Execution = &agenticv1alpha1.ExecutionApproval{}
	case agenticv1alpha1.ApprovalStageVerification:
		stage.Verification = &agenticv1alpha1.VerificationApproval{}
	case agenticv1alpha1.ApprovalStageEscalation:
		stage.Escalation = &agenticv1alpha1.EscalationApproval{}
	}
	approval.Spec.Stages = append(approval.Spec.Stages, stage)
	if err := fc.Patch(context.Background(), &approval, client.MergeFrom(base)); err != nil {
		t.Fatalf("deny %s: %v", stageType, err)
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
	run := testAgenticRun()
	agent := newTestAgentCaller()
	r, fc := newManualReconciler(t, run, agent)

	// After initial reconcile: Pending, analysis needs approval
	assertPhase(t, r, "fix-crash", agenticv1alpha1.AgenticRunPhasePending)

	// Approve analysis
	approveAnalysis(t, fc, "fix-crash")

	// Analysis runs → Proposed
	result, err := reconcileOnce(r, "fix-crash")
	mustNotRequeue(t, result, err, "after analysis approval")
	assertPhase(t, r, "fix-crash", agenticv1alpha1.AgenticRunPhaseProposed)

	// Reconcile 3: Proposed, execution needs approval — should not requeue
	result, err = reconcileOnce(r, "fix-crash")
	mustNotRequeue(t, result, err, "proposed waiting for execution approval")
	assertPhase(t, r, "fix-crash", agenticv1alpha1.AgenticRunPhaseProposed)

	// Approve execution
	approveExecution(t, fc, "fix-crash", 0)

	// Reconcile 4: execution runs → Verifying
	result, err = reconcileOnce(r, "fix-crash")
	mustNotRequeue(t, result, err, "after execution approval")
	p := assertPhase(t, r, "fix-crash", agenticv1alpha1.AgenticRunPhaseVerifying)
	executed := meta.FindStatusCondition(p.Status.Conditions, agenticv1alpha1.AgenticRunConditionExecuted)
	if executed == nil || executed.Status != metav1.ConditionTrue {
		t.Fatal("execution should have completed")
	}

	// Reconcile 5: Verifying, verification needs approval — should not requeue
	result, err = reconcileOnce(r, "fix-crash")
	mustNotRequeue(t, result, err, "verifying waiting for verification approval")
	assertPhase(t, r, "fix-crash", agenticv1alpha1.AgenticRunPhaseVerifying)

	// Approve verification
	approveVerification(t, fc, "fix-crash")

	// Reconcile 6: verification runs → Completed
	result, err = reconcileOnce(r, "fix-crash")
	if err != nil {
		t.Fatalf("verification reconcile: %v", err)
	}
	assertPhase(t, r, "fix-crash", agenticv1alpha1.AgenticRunPhaseCompleted)
}

// ---------------------------------------------------------------------------
// Proposed phase holds until execution approval
// ---------------------------------------------------------------------------

func TestManualApproval_ProposedWaitsForExecution(t *testing.T) {
	run := testAgenticRun()
	agent := newTestAgentCaller()
	r, fc := newManualReconciler(t, run, agent)

	approveAnalysis(t, fc, "fix-crash")
	reconcileOnce(r, "fix-crash")
	assertPhase(t, r, "fix-crash", agenticv1alpha1.AgenticRunPhaseProposed)

	// Multiple reconciles without approving execution — should stay Proposed
	for i := 0; i < 3; i++ {
		result, err := reconcileOnce(r, "fix-crash")
		mustNotRequeue(t, result, err, fmt.Sprintf("proposed idle reconcile %d", i))
		assertPhase(t, r, "fix-crash", agenticv1alpha1.AgenticRunPhaseProposed)
	}

	// Approve execution → should progress
	approveExecution(t, fc, "fix-crash", 0)
	result, err := reconcileOnce(r, "fix-crash")
	mustNotRequeue(t, result, err, "execution after approval")
	assertPhase(t, r, "fix-crash", agenticv1alpha1.AgenticRunPhaseVerifying)
}

// ---------------------------------------------------------------------------
// Denial at each stage
// ---------------------------------------------------------------------------

func TestManualApproval_DenyAnalysis(t *testing.T) {
	run := testAgenticRun()
	agent := newTestAgentCaller()
	r, fc := newManualReconciler(t, run, agent)

	// Deny analysis
	denyStage(t, fc, "fix-crash", agenticv1alpha1.ApprovalStageAnalysis)

	reconcileOnce(r, "fix-crash")
	assertPhase(t, r, "fix-crash", agenticv1alpha1.AgenticRunPhaseDenied)
}

func TestManualApproval_DenyExecution(t *testing.T) {
	run := testAgenticRun()
	agent := newTestAgentCaller()
	r, fc := newManualReconciler(t, run, agent)

	// Approve analysis, run it
	approveAnalysis(t, fc, "fix-crash")
	reconcileOnce(r, "fix-crash")
	assertPhase(t, r, "fix-crash", agenticv1alpha1.AgenticRunPhaseProposed)

	// Deny execution
	denyStage(t, fc, "fix-crash", agenticv1alpha1.ApprovalStageExecution)

	reconcileOnce(r, "fix-crash")
	assertPhase(t, r, "fix-crash", agenticv1alpha1.AgenticRunPhaseDenied)
}

func TestManualApproval_DenyVerification(t *testing.T) {
	run := testAgenticRun()
	agent := newTestAgentCaller()
	r, fc := newManualReconciler(t, run, agent)

	// Run through analysis and execution
	approveAnalysis(t, fc, "fix-crash")
	reconcileOnce(r, "fix-crash")
	approveExecution(t, fc, "fix-crash", 0)
	reconcileOnce(r, "fix-crash")
	assertPhase(t, r, "fix-crash", agenticv1alpha1.AgenticRunPhaseVerifying)

	// Deny verification
	denyStage(t, fc, "fix-crash", agenticv1alpha1.ApprovalStageVerification)

	reconcileOnce(r, "fix-crash")
	assertPhase(t, r, "fix-crash", agenticv1alpha1.AgenticRunPhaseDenied)
}

// ---------------------------------------------------------------------------
// Failures at each stage
// ---------------------------------------------------------------------------

func TestManualApproval_AnalysisFails(t *testing.T) {
	run := testAgenticRun()
	agent := newTestAgentCaller()
	agent.analyzeErr = fmt.Errorf("LLM timeout")
	r, fc := newManualReconciler(t, run, agent)

	approveAnalysis(t, fc, "fix-crash")
	reconcileOnce(r, "fix-crash")
	assertPhase(t, r, "fix-crash", agenticv1alpha1.AgenticRunPhaseFailed)
}

func TestManualApproval_ExecutionFails(t *testing.T) {
	run := testAgenticRun()
	agent := newTestAgentCaller()
	agent.executeErr = fmt.Errorf("sandbox crashed")
	r, fc := newManualReconciler(t, run, agent)

	approveAnalysis(t, fc, "fix-crash")
	reconcileOnce(r, "fix-crash")
	approveExecution(t, fc, "fix-crash", 0)
	reconcileOnce(r, "fix-crash")
	assertPhase(t, r, "fix-crash", agenticv1alpha1.AgenticRunPhaseFailed)
}

func TestManualApproval_VerificationFails(t *testing.T) {
	run := testAgenticRun()
	agent := newTestAgentCaller()
	agent.verifyErr = fmt.Errorf("verification timed out")
	r, fc := newManualReconciler(t, run, agent)

	approveAnalysis(t, fc, "fix-crash")
	reconcileOnce(r, "fix-crash")
	approveExecution(t, fc, "fix-crash", 0)
	reconcileOnce(r, "fix-crash")
	approveVerification(t, fc, "fix-crash")
	reconcileOnce(r, "fix-crash")
	assertPhase(t, r, "fix-crash", agenticv1alpha1.AgenticRunPhaseFailed)
}

// ---------------------------------------------------------------------------
// Verification failure triggers retry back to Executing
// ---------------------------------------------------------------------------

func TestManualApproval_VerificationFailRetry(t *testing.T) {
	run := testAgenticRun()
	agent := newTestAgentCaller()
	policy := testPolicyWithMaxAttempts(agenticv1alpha1.ApprovalModeManual, agenticv1alpha1.ApprovalModeManual, agenticv1alpha1.ApprovalModeManual, 3)
	r, fc := newReconcilerWithPolicy(t, run, agent, policy)

	// Analysis → Proposed → Executing → Verifying
	approveAnalysis(t, fc, "fix-crash")
	reconcileOnce(r, "fix-crash")
	approveExecution(t, fc, "fix-crash", 0)
	reconcileOnce(r, "fix-crash")
	assertPhase(t, r, "fix-crash", agenticv1alpha1.AgenticRunPhaseVerifying)

	// Make verification fail (not a system error — objective failure)
	agent.verifyResult = &VerificationOutput{
		Success: false,
		Summary: "Pod still crashing",
		Checks:  []agenticv1alpha1.VerifyCheck{{Name: "pod-running", Result: agenticv1alpha1.CheckResultFailed}},
	}
	approveVerification(t, fc, "fix-crash")
	reconcileOnce(r, "fix-crash")

	// Should retry → Executing (Verified=False/RetryingExecution)
	assertPhase(t, r, "fix-crash", agenticv1alpha1.AgenticRunPhaseExecuting)
	p, _ := getAgenticRun(r, "fix-crash")
	if p.Status.Steps.Execution.RetryCount == nil || *p.Status.Steps.Execution.RetryCount != 1 {
		t.Fatalf("expected retryCount=1, got %v", p.Status.Steps.Execution.RetryCount)
	}
}

func TestManualApproval_FullRetryExhaustion(t *testing.T) {
	run := testAgenticRun()
	agent := newTestAgentCaller()
	policy := testPolicyWithMaxAttempts(agenticv1alpha1.ApprovalModeManual, agenticv1alpha1.ApprovalModeManual, agenticv1alpha1.ApprovalModeManual, 3)
	r, fc := newReconcilerWithPolicy(t, run, agent, policy)

	// Run through to Verifying
	approveAnalysis(t, fc, "fix-crash")
	reconcileOnce(r, "fix-crash")
	approveExecution(t, fc, "fix-crash", 0)
	reconcileOnce(r, "fix-crash")
	assertPhase(t, r, "fix-crash", agenticv1alpha1.AgenticRunPhaseVerifying)

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
	assertPhase(t, r, "fix-crash", agenticv1alpha1.AgenticRunPhaseExecuting)
	p, _ := getAgenticRun(r, "fix-crash")
	if *p.Status.Steps.Execution.RetryCount != 1 {
		t.Fatalf("expected retryCount=1, got %d", *p.Status.Steps.Execution.RetryCount)
	}

	// Re-execute → Verifying
	reconcileOnce(r, "fix-crash")
	assertPhase(t, r, "fix-crash", agenticv1alpha1.AgenticRunPhaseVerifying)

	// Attempt 2 (of 3): verify fails again → Executing (retryCount=2)
	reconcileOnce(r, "fix-crash")
	assertPhase(t, r, "fix-crash", agenticv1alpha1.AgenticRunPhaseExecuting)
	p, _ = getAgenticRun(r, "fix-crash")
	if *p.Status.Steps.Execution.RetryCount != 2 {
		t.Fatalf("expected retryCount=2, got %d", *p.Status.Steps.Execution.RetryCount)
	}

	// Re-execute → Verifying
	reconcileOnce(r, "fix-crash")
	assertPhase(t, r, "fix-crash", agenticv1alpha1.AgenticRunPhaseVerifying)

	// Attempt 3 (of 3): verify fails → retries exhausted (retryCount=2 == maxAttempts-1)
	// → Escalating (escalation step injected)
	reconcileOnce(r, "fix-crash")
	assertPhase(t, r, "fix-crash", agenticv1alpha1.AgenticRunPhaseEscalating)
}

func TestManualApproval_RetryThenSucceed(t *testing.T) {
	run := testAgenticRun()
	agent := newTestAgentCaller()
	policy := testPolicyWithMaxAttempts(agenticv1alpha1.ApprovalModeManual, agenticv1alpha1.ApprovalModeManual, agenticv1alpha1.ApprovalModeManual, 3)
	r, fc := newReconcilerWithPolicy(t, run, agent, policy)

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
	assertPhase(t, r, "fix-crash", agenticv1alpha1.AgenticRunPhaseExecuting)

	// Re-execute succeeds, now make verification pass
	agent.verifyResult = &VerificationOutput{
		Success: true,
		Summary: "All checks passed",
		Checks:  []agenticv1alpha1.VerifyCheck{{Name: "pod-running", Result: agenticv1alpha1.CheckResultPassed}},
	}
	reconcileOnce(r, "fix-crash") // re-execute → Verifying
	reconcileOnce(r, "fix-crash") // verify → Completed
	assertPhase(t, r, "fix-crash", agenticv1alpha1.AgenticRunPhaseCompleted)

	p, _ := getAgenticRun(r, "fix-crash")
	if *p.Status.Steps.Execution.RetryCount != 1 {
		t.Fatalf("expected retryCount=1, got %d", *p.Status.Steps.Execution.RetryCount)
	}
}

// ---------------------------------------------------------------------------
// No approval policy → all stages default to Manual
// ---------------------------------------------------------------------------

func TestNoPolicy_DefaultsToManual(t *testing.T) {
	run := testAgenticRun()
	agent := newTestAgentCaller()

	scheme := testScheme()
	// No ApprovalPolicy object at all
	objs := []client.Object{run, testDefaultAgent(), testLLM("smart")}
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
		WithStatusSubresource(run, &agenticv1alpha1.AnalysisResult{}, &agenticv1alpha1.ExecutionResult{}, &agenticv1alpha1.VerificationResult{}, &agenticv1alpha1.EscalationResult{}).Build()
	r := &AgenticRunReconciler{Client: fc, Agent: agent, Namespace: "default"}

	// Initial reconcile creates AgenticRunApproval; analysis should wait for approval
	reconcileOnce(r, "fix-crash")
	assertPhase(t, r, "fix-crash", agenticv1alpha1.AgenticRunPhasePending)

	// Approve analysis manually
	approveAnalysis(t, fc, "fix-crash")
	reconcileOnce(r, "fix-crash")
	assertPhase(t, r, "fix-crash", agenticv1alpha1.AgenticRunPhaseProposed)

	// Execution should also wait
	result, err := reconcileOnce(r, "fix-crash")
	mustNotRequeue(t, result, err, "no policy, execution pending")
	assertPhase(t, r, "fix-crash", agenticv1alpha1.AgenticRunPhaseProposed)
}

// ---------------------------------------------------------------------------
// Advisory-only (no execution, no verification) still needs analysis approval
// ---------------------------------------------------------------------------

func TestManualApproval_AdvisoryOnly(t *testing.T) {
	run := &agenticv1alpha1.AgenticRun{
		ObjectMeta: metav1.ObjectMeta{Name: "fix-crash", Namespace: "default"},
		Spec: agenticv1alpha1.AgenticRunSpec{
			Request:          "Investigate issue",
			Tools:            testTools(),
			TargetNamespaces: []string{"production"},
			Analysis:         agenticv1alpha1.AgenticRunStep{Agent: "default"},
		},
	}
	agent := newTestAgentCaller()
	r, fc := newManualReconciler(t, run, agent)
	assertPhase(t, r, "fix-crash", agenticv1alpha1.AgenticRunPhasePending)

	// Approve analysis
	approveAnalysis(t, fc, "fix-crash")
	reconcileOnce(r, "fix-crash")
	assertPhase(t, r, "fix-crash", agenticv1alpha1.AgenticRunPhaseProposed)

	// Proposed → execution step is nil → skips to Completed
	reconcileOnce(r, "fix-crash")
	assertPhase(t, r, "fix-crash", agenticv1alpha1.AgenticRunPhaseCompleted)
}

// ---------------------------------------------------------------------------
// Advisory-only with auto-approve policy that includes Verification —
// verifies that absent steps are skipped in AgenticRunApproval seeding (OLS-3223)
// ---------------------------------------------------------------------------

func TestAutoApproval_AdvisoryOnly_SkipsAbsentStages(t *testing.T) {
	run := &agenticv1alpha1.AgenticRun{
		ObjectMeta: metav1.ObjectMeta{Name: "advisory", Namespace: "default"},
		Spec: agenticv1alpha1.AgenticRunSpec{
			Request:          "Investigate issue",
			Tools:            testTools(),
			TargetNamespaces: []string{"production"},
			Analysis:         agenticv1alpha1.AgenticRunStep{Agent: "default"},
		},
	}
	agent := newTestAgentCaller()
	policy := testAutoApprovePolicy() // auto-approves Analysis + Verification
	r, fc := newReconcilerWithPolicy(t, run, agent, policy)

	// First reconcile creates AgenticRunApproval — must not fail despite
	// Verification being in the policy but absent from the run.
	// Analysis is auto-approved and completes immediately (mock agent),
	// then no execution/verification → straight to Completed.
	reconcileOnce(r, "advisory")

	var approval agenticv1alpha1.AgenticRunApproval
	if err := fc.Get(context.Background(), types.NamespacedName{Name: "advisory", Namespace: "default"}, &approval); err != nil {
		t.Fatalf("get AgenticRunApproval: %v", err)
	}
	for _, s := range approval.Spec.Stages {
		if s.Type == agenticv1alpha1.ApprovalStageVerification {
			t.Fatal("Verification stage should not be seeded for analysis-only run")
		}
	}

	assertPhase(t, r, "advisory", agenticv1alpha1.AgenticRunPhaseCompleted)
}

// ---------------------------------------------------------------------------
// Trust mode (no verification) needs manual analysis + execution approval
// ---------------------------------------------------------------------------

func TestManualApproval_TrustMode(t *testing.T) {
	run := &agenticv1alpha1.AgenticRun{
		ObjectMeta: metav1.ObjectMeta{Name: "fix-crash", Namespace: "default"},
		Spec: agenticv1alpha1.AgenticRunSpec{
			Request:          "Fix with trust",
			Tools:            testTools(),
			TargetNamespaces: []string{"production"},
			Analysis:         agenticv1alpha1.AgenticRunStep{Agent: "default"},
			Execution:        agenticv1alpha1.AgenticRunStep{Agent: "default"},
		},
	}
	agent := newTestAgentCaller()
	r, fc := newManualReconciler(t, run, agent)

	approveAnalysis(t, fc, "fix-crash")
	reconcileOnce(r, "fix-crash")
	assertPhase(t, r, "fix-crash", agenticv1alpha1.AgenticRunPhaseProposed)

	approveExecution(t, fc, "fix-crash", 0)
	reconcileOnce(r, "fix-crash")

	// No verification step → Completed
	assertPhase(t, r, "fix-crash", agenticv1alpha1.AgenticRunPhaseCompleted)
}

// ---------------------------------------------------------------------------
// Manual policy: AgenticRunApproval starts with no auto-approved stages
// ---------------------------------------------------------------------------

func TestManualApproval_NoAutoApprovedStages(t *testing.T) {
	run := testAgenticRun()
	agent := newTestAgentCaller()
	_, fc := newManualReconciler(t, run, agent)

	var approval agenticv1alpha1.AgenticRunApproval
	if err := fc.Get(context.Background(), types.NamespacedName{Name: "fix-crash", Namespace: "default"}, &approval); err != nil {
		t.Fatalf("get AgenticRunApproval: %v", err)
	}

	if len(approval.Spec.Stages) != 0 {
		t.Fatalf("all-manual policy should create 0 auto-approved stages, got %d", len(approval.Spec.Stages))
	}
}

// ---------------------------------------------------------------------------
// Execution not reported as success → fails
// ---------------------------------------------------------------------------

func TestManualApproval_ExecutionReportsFailure(t *testing.T) {
	run := testAgenticRun()
	agent := newTestAgentCaller()
	agent.executeResult = &ExecutionOutput{Success: false}
	r, fc := newManualReconciler(t, run, agent)

	approveAnalysis(t, fc, "fix-crash")
	reconcileOnce(r, "fix-crash")
	approveExecution(t, fc, "fix-crash", 0)
	reconcileOnce(r, "fix-crash")
	assertPhase(t, r, "fix-crash", agenticv1alpha1.AgenticRunPhaseFailed)
}

// ---------------------------------------------------------------------------
// Verification objective failure without maxAttempts → terminal Failed
// ---------------------------------------------------------------------------

func TestManualApproval_VerificationFailDefaultOneAttempt(t *testing.T) {
	run := testAgenticRun()
	// No maxAttempts on policy → defaults to 1 (one attempt, no retries)
	agent := newTestAgentCaller()
	agent.verifyResult = &VerificationOutput{
		Success: false,
		Summary: "Pod still crashing",
	}
	r, fc := newManualReconciler(t, run, agent)

	approveAnalysis(t, fc, "fix-crash")
	reconcileOnce(r, "fix-crash")
	approveExecution(t, fc, "fix-crash", 0)
	reconcileOnce(r, "fix-crash")
	approveVerification(t, fc, "fix-crash")
	reconcileOnce(r, "fix-crash")

	// maxAttempts=1 → 1 total attempt, no retries → escalate immediately
	assertPhase(t, r, "fix-crash", agenticv1alpha1.AgenticRunPhaseEscalating)
}

// ---------------------------------------------------------------------------
// Agent override from approval stage is respected
// ---------------------------------------------------------------------------

func TestManualApproval_AgentOverride(t *testing.T) {
	run := testAgenticRun()
	agent := newTestAgentCaller()
	fastAgent := &agenticv1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "fast"},
		Spec:       agenticv1alpha1.AgentSpec{LLMProvider: agenticv1alpha1.LLMProviderReference{Name: "smart"}, Model: "claude-haiku-4-5"},
	}
	r, fc := newManualReconciler(t, run, agent, fastAgent)

	// Approve analysis with "fast" agent override
	approveAnalysis(t, fc, "fix-crash", "fast")

	result, err := reconcileOnce(r, "fix-crash")
	mustNotRequeue(t, result, err, "analysis with override")
	assertPhase(t, r, "fix-crash", agenticv1alpha1.AgenticRunPhaseProposed)
}

// ---------------------------------------------------------------------------
// DerivePhase truth table — comprehensive phase derivation coverage
// ---------------------------------------------------------------------------

func TestDerivePhase_ProposedVsExecuting(t *testing.T) {
	tests := []struct {
		name       string
		conditions []metav1.Condition
		want       agenticv1alpha1.AgenticRunPhase
	}{
		{
			name:       "no conditions → Pending",
			conditions: nil,
			want:       agenticv1alpha1.AgenticRunPhasePending,
		},
		{
			name: "Analyzed=Unknown → Analyzing",
			conditions: []metav1.Condition{
				{Type: agenticv1alpha1.AgenticRunConditionAnalyzed, Status: metav1.ConditionUnknown},
			},
			want: agenticv1alpha1.AgenticRunPhaseAnalyzing,
		},
		{
			name: "Analyzed=True only → Proposed (not Executing)",
			conditions: []metav1.Condition{
				{Type: agenticv1alpha1.AgenticRunConditionAnalyzed, Status: metav1.ConditionTrue},
			},
			want: agenticv1alpha1.AgenticRunPhaseProposed,
		},
		{
			name: "Analyzed=True + Executed=Unknown → Executing",
			conditions: []metav1.Condition{
				{Type: agenticv1alpha1.AgenticRunConditionAnalyzed, Status: metav1.ConditionTrue},
				{Type: agenticv1alpha1.AgenticRunConditionExecuted, Status: metav1.ConditionUnknown},
			},
			want: agenticv1alpha1.AgenticRunPhaseExecuting,
		},
		{
			name: "Analyzed=True + Executed=True → Verifying",
			conditions: []metav1.Condition{
				{Type: agenticv1alpha1.AgenticRunConditionAnalyzed, Status: metav1.ConditionTrue},
				{Type: agenticv1alpha1.AgenticRunConditionExecuted, Status: metav1.ConditionTrue},
			},
			want: agenticv1alpha1.AgenticRunPhaseVerifying,
		},
		{
			name: "Analyzed=True + Verified=False/RetryingExecution → Executing (retry)",
			conditions: []metav1.Condition{
				{Type: agenticv1alpha1.AgenticRunConditionAnalyzed, Status: metav1.ConditionTrue},
				{Type: agenticv1alpha1.AgenticRunConditionVerified, Status: metav1.ConditionFalse, Reason: agenticv1alpha1.ReasonRetryingExecution},
			},
			want: agenticv1alpha1.AgenticRunPhaseExecuting,
		},
		{
			name: "Analyzed=False → Failed",
			conditions: []metav1.Condition{
				{Type: agenticv1alpha1.AgenticRunConditionAnalyzed, Status: metav1.ConditionFalse},
			},
			want: agenticv1alpha1.AgenticRunPhaseFailed,
		},
		{
			name: "Denied=True → Denied (regardless of other conditions)",
			conditions: []metav1.Condition{
				{Type: agenticv1alpha1.AgenticRunConditionAnalyzed, Status: metav1.ConditionTrue},
				{Type: agenticv1alpha1.AgenticRunConditionDenied, Status: metav1.ConditionTrue},
			},
			want: agenticv1alpha1.AgenticRunPhaseDenied,
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
		name      string
		analysis  agenticv1alpha1.ApprovalMode
		execution agenticv1alpha1.ApprovalMode
		verify    agenticv1alpha1.ApprovalMode
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
			run := testAgenticRun()
			agent := newTestAgentCaller()
			policy := testPolicy(tt.analysis, tt.execution, tt.verify)
			r, fc := newReconcilerWithPolicy(t, run, agent, policy)

			// After initial reconcile: auto stages should already be in the approval
			var approval agenticv1alpha1.AgenticRunApproval
			if err := fc.Get(context.Background(), types.NamespacedName{Name: "fix-crash", Namespace: "default"}, &approval); err != nil {
				t.Fatalf("get AgenticRunApproval: %v", err)
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
				assertPhase(t, r, "fix-crash", agenticv1alpha1.AgenticRunPhasePending)
				approveAnalysis(t, fc, "fix-crash")
			}
			reconcileOnce(r, "fix-crash")

			if tt.execution == A {
				// Auto-approved execution runs in same reconcile cycle after analysis
				// — skips Proposed, goes straight to Verifying (or needs another reconcile)
				phase := agenticv1alpha1.DerivePhase(mustGetAgenticRun(t, r, "fix-crash").Status.Conditions)
				if phase == agenticv1alpha1.AgenticRunPhaseProposed {
					reconcileOnce(r, "fix-crash")
				}
			} else {
				assertPhase(t, r, "fix-crash", agenticv1alpha1.AgenticRunPhaseProposed)
				result, err := reconcileOnce(r, "fix-crash")
				mustNotRequeue(t, result, err, "execution pending")
				approveExecution(t, fc, "fix-crash", 0)
				reconcileOnce(r, "fix-crash")
			}
			assertPhase(t, r, "fix-crash", agenticv1alpha1.AgenticRunPhaseVerifying)

			// Verification: approve manually if needed, then reconcile
			if tt.verify == M {
				result, err := reconcileOnce(r, "fix-crash")
				mustNotRequeue(t, result, err, "verification pending")
				assertPhase(t, r, "fix-crash", agenticv1alpha1.AgenticRunPhaseVerifying)
				approveVerification(t, fc, "fix-crash")
			}
			reconcileOnce(r, "fix-crash")
			assertPhase(t, r, "fix-crash", agenticv1alpha1.AgenticRunPhaseCompleted)
		})
	}
}

// ---------------------------------------------------------------------------
// Policy change after AgenticRunApproval creation (the fallback path)
// ---------------------------------------------------------------------------

func approveEscalation(t *testing.T, fc client.WithWatch, name string) {
	t.Helper()
	approveStage(t, fc, name, agenticv1alpha1.ApprovalStageEscalation)
}

// ---------------------------------------------------------------------------
// Escalation: approve and complete
// ---------------------------------------------------------------------------

func TestEscalation_ApproveAndComplete(t *testing.T) {
	run := testAgenticRun()
	agent := newTestAgentCaller()
	agent.verifyResult = &VerificationOutput{
		Success: false,
		Summary: "Pod still crashing",
		Checks:  []agenticv1alpha1.VerifyCheck{{Name: "pod-running", Result: agenticv1alpha1.CheckResultFailed}},
	}
	policy := testPolicyWithMaxAttempts(agenticv1alpha1.ApprovalModeManual, agenticv1alpha1.ApprovalModeManual, agenticv1alpha1.ApprovalModeManual, 1)
	r, fc := newReconcilerWithPolicy(t, run, agent, policy)

	// Run through to verification failure → retry → exhaustion → Escalating
	approveAnalysis(t, fc, "fix-crash")
	reconcileOnce(r, "fix-crash")
	approveExecution(t, fc, "fix-crash", 0)
	reconcileOnce(r, "fix-crash")
	approveVerification(t, fc, "fix-crash")
	reconcileOnce(r, "fix-crash") // verify fails, maxAttempts=1 → escalate immediately
	assertPhase(t, r, "fix-crash", agenticv1alpha1.AgenticRunPhaseEscalating)

	// Approve escalation
	approveEscalation(t, fc, "fix-crash")
	reconcileOnce(r, "fix-crash")

	// Should be terminal Escalated
	p := assertPhase(t, r, "fix-crash", agenticv1alpha1.AgenticRunPhaseEscalated)
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
	run := testAgenticRun()
	agent := newTestAgentCaller()
	agent.verifyResult = &VerificationOutput{Success: false, Summary: "fail"}
	r, fc := newManualReconciler(t, run, agent)

	approveAnalysis(t, fc, "fix-crash")
	reconcileOnce(r, "fix-crash")
	approveExecution(t, fc, "fix-crash", 0)
	reconcileOnce(r, "fix-crash")
	approveVerification(t, fc, "fix-crash")
	reconcileOnce(r, "fix-crash")
	assertPhase(t, r, "fix-crash", agenticv1alpha1.AgenticRunPhaseEscalating)

	denyStage(t, fc, "fix-crash", agenticv1alpha1.ApprovalStageEscalation)
	reconcileOnce(r, "fix-crash")
	assertPhase(t, r, "fix-crash", agenticv1alpha1.AgenticRunPhaseDenied)
}

// ---------------------------------------------------------------------------
// Escalation: agent failure
// ---------------------------------------------------------------------------

func TestEscalation_AgentFailure(t *testing.T) {
	run := testAgenticRun()
	agent := newTestAgentCaller()
	agent.verifyResult = &VerificationOutput{Success: false, Summary: "fail"}
	agent.escalateErr = fmt.Errorf("escalation agent crashed")
	r, fc := newManualReconciler(t, run, agent)

	approveAnalysis(t, fc, "fix-crash")
	reconcileOnce(r, "fix-crash")
	approveExecution(t, fc, "fix-crash", 0)
	reconcileOnce(r, "fix-crash")
	approveVerification(t, fc, "fix-crash")
	reconcileOnce(r, "fix-crash")
	assertPhase(t, r, "fix-crash", agenticv1alpha1.AgenticRunPhaseEscalating)

	approveEscalation(t, fc, "fix-crash")
	reconcileOnce(r, "fix-crash")
	assertPhase(t, r, "fix-crash", agenticv1alpha1.AgenticRunPhaseFailed)
}

// ---------------------------------------------------------------------------
// Escalation: auto-approve via policy
// ---------------------------------------------------------------------------

func TestEscalation_AutoApprove(t *testing.T) {
	run := testAgenticRun()
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
	r, fc := newReconcilerWithPolicy(t, run, agent, policy)

	// Analysis auto-approved → Proposed
	reconcileOnce(r, "fix-crash")
	assertPhase(t, r, "fix-crash", agenticv1alpha1.AgenticRunPhaseProposed)

	approveExecution(t, fc, "fix-crash", 0)
	reconcileOnce(r, "fix-crash") // execute → Verifying
	reconcileOnce(r, "fix-crash") // verify fails → Escalating
	assertPhase(t, r, "fix-crash", agenticv1alpha1.AgenticRunPhaseEscalating)

	// Escalation is auto-approved, should run and complete
	reconcileOnce(r, "fix-crash")
	assertPhase(t, r, "fix-crash", agenticv1alpha1.AgenticRunPhaseEscalated)
}

// ---------------------------------------------------------------------------
// Escalation: re-reconcile while in progress is a no-op
// ---------------------------------------------------------------------------

func TestEscalation_InProgressIsIdempotent(t *testing.T) {
	run := testAgenticRun()
	agent := newTestAgentCaller()
	agent.verifyResult = &VerificationOutput{
		Success: false,
		Summary: "Pod still crashing",
		Checks:  []agenticv1alpha1.VerifyCheck{{Name: "pod-running", Result: agenticv1alpha1.CheckResultFailed}},
	}
	policy := testPolicyWithMaxAttempts(agenticv1alpha1.ApprovalModeManual, agenticv1alpha1.ApprovalModeManual, agenticv1alpha1.ApprovalModeManual, 1)
	r, fc := newReconcilerWithPolicy(t, run, agent, policy)

	// Drive to Escalating phase
	approveAnalysis(t, fc, "fix-crash")
	reconcileOnce(r, "fix-crash")
	approveExecution(t, fc, "fix-crash", 0)
	reconcileOnce(r, "fix-crash")
	approveVerification(t, fc, "fix-crash")
	reconcileOnce(r, "fix-crash") // verify fails, retry
	reconcileOnce(r, "fix-crash") // re-execute
	reconcileOnce(r, "fix-crash") // verify fails again, retries exhausted → Escalating
	assertPhase(t, r, "fix-crash", agenticv1alpha1.AgenticRunPhaseEscalating)

	// Approve escalation and run it
	approveEscalation(t, fc, "fix-crash")
	reconcileOnce(r, "fix-crash")
	p := assertPhase(t, r, "fix-crash", agenticv1alpha1.AgenticRunPhaseEscalated)

	// Record how many EscalationResult refs exist
	resultCount := len(p.Status.Steps.Escalation.Results)
	if resultCount != 1 {
		t.Fatalf("expected exactly 1 escalation result, got %d", resultCount)
	}

	// Re-reconcile: should be a no-op, no additional results
	reconcileOnce(r, "fix-crash")
	p = assertPhase(t, r, "fix-crash", agenticv1alpha1.AgenticRunPhaseEscalated)
	if len(p.Status.Steps.Escalation.Results) != resultCount {
		t.Fatalf("re-reconcile created duplicate results: got %d, want %d",
			len(p.Status.Steps.Escalation.Results), resultCount)
	}
}

// ---------------------------------------------------------------------------
// Policy change after AgenticRunApproval creation (the fallback path)
// ---------------------------------------------------------------------------

func TestPolicyChange_ManualToAutomatic(t *testing.T) {
	run := testAgenticRun()
	agent := newTestAgentCaller()

	// Start with all-manual policy
	r, fc := newManualReconciler(t, run, agent)
	assertPhase(t, r, "fix-crash", agenticv1alpha1.AgenticRunPhasePending)

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
	assertPhase(t, r, "fix-crash", agenticv1alpha1.AgenticRunPhaseProposed)
}

// ---------------------------------------------------------------------------
// NoActionRequired: analysis terminates without execution
// ---------------------------------------------------------------------------

func TestNoActionRequired_TerminalWithoutExecution(t *testing.T) {
	run := testAgenticRun()
	agent := newTestAgentCaller()
	agent.analyzeResult = &AnalysisOutput{
		Success:        true,
		ActionRequired: ptr.To(false),
		Diagnosis: &agenticv1alpha1.DiagnosisResult{
			Summary:    "Alert is a false alarm — pod restarted once due to a transient OOM but has been stable since",
			Confidence: "High",
			RootCause:  "Transient OOM, already self-healed",
		},
	}
	r, fc := newReconcilerWithPolicy(t, run, agent, testAutoApprovePolicy())

	// Analysis auto-approved → NoActionRequired (terminal)
	reconcileOnce(r, "fix-crash")
	p := assertPhase(t, r, "fix-crash", agenticv1alpha1.AgenticRunPhaseNoActionRequired)

	// Verify AnalysisResult CR was created with ActionRequired=False and Diagnosis
	if len(p.Status.Steps.Analysis.Results) == 0 {
		t.Fatal("expected analysis result ref")
	}
	var ar agenticv1alpha1.AnalysisResult
	if err := fc.Get(context.Background(), types.NamespacedName{Name: p.Status.Steps.Analysis.Results[0].Name, Namespace: "default"}, &ar); err != nil {
		t.Fatalf("get AnalysisResult: %v", err)
	}
	if ar.Status.ActionRequired != agenticv1alpha1.ActionRequiredFalse {
		t.Errorf("ActionRequired = %q, want %q", ar.Status.ActionRequired, agenticv1alpha1.ActionRequiredFalse)
	}
	if ar.Status.Diagnosis.Summary == "" {
		t.Fatal("expected Diagnosis to be set")
	}
	if ar.Status.Diagnosis.Confidence != "High" {
		t.Errorf("Diagnosis.Confidence = %q, want High", ar.Status.Diagnosis.Confidence)
	}

	// Execution and verification should never have run
	if len(p.Status.Steps.Execution.Results) != 0 {
		t.Error("execution should not have run")
	}
	if len(p.Status.Steps.Verification.Results) != 0 {
		t.Error("verification should not have run")
	}

	// Re-reconcile is a no-op (terminal)
	result, err := reconcileOnce(r, "fix-crash")
	mustNotRequeue(t, result, err, "terminal no-op re-reconcile")
	assertPhase(t, r, "fix-crash", agenticv1alpha1.AgenticRunPhaseNoActionRequired)
}

func TestNoActionRequired_RevisionTriggersReanalysis(t *testing.T) {
	run := testAgenticRun()
	agent := newTestAgentCaller()
	agent.analyzeResult = &AnalysisOutput{
		Success:        true,
		ActionRequired: ptr.To(false),
		Diagnosis: &agenticv1alpha1.DiagnosisResult{
			Summary:    "False alarm",
			Confidence: "Medium",
			RootCause:  "Transient issue",
		},
	}
	r, fc := newReconcilerWithPolicy(t, run, agent, testAutoApprovePolicy())

	// Analysis → NoActionRequired
	result, err := reconcileOnce(r, "fix-crash")
	mustNotRequeue(t, result, err, "initial no-action-required analysis")
	assertPhase(t, r, "fix-crash", agenticv1alpha1.AgenticRunPhaseNoActionRequired)

	// Admin disagrees — submits revision feedback to re-analyze
	agent.analyzeResult = &AnalysisOutput{
		Success:        true,
		ActionRequired: ptr.To(true),
		Options: []agenticv1alpha1.RemediationOption{{
			Title: "Increase memory",
			Diagnosis: agenticv1alpha1.DiagnosisResult{
				Summary: "OOM", Confidence: "High", RootCause: "Low limit",
			},
			RemediationPlan: agenticv1alpha1.RemediationPlan{
				Description: "Increase to 512Mi",
				Actions:     []agenticv1alpha1.ProposedAction{{Command: "kubectl patch deploy/web -n production -p '{}'", Type: "mutation", Description: "Patch"}},
				Risk:        "Low",
				Reversible:  agenticv1alpha1.ReversibilityReversible,
			},
		}},
	}
	reviseAgenticRun(t, fc, "fix-crash", "This is NOT a false alarm, the pod keeps crashing")

	// Revision triggers re-analysis → should now be Proposed
	result, err = reconcileOnce(r, "fix-crash")
	mustNotRequeue(t, result, err, "revision-triggered reanalysis")
	assertPhase(t, r, "fix-crash", agenticv1alpha1.AgenticRunPhaseProposed)

	p := mustGetAgenticRun(t, r, "fix-crash")
	if len(p.Status.Steps.Analysis.Results) < 2 {
		t.Fatalf("expected at least 2 analysis results (initial + revision), got %d", len(p.Status.Steps.Analysis.Results))
	}
}

func TestNoActionRequired_NilActionRequiredDefaultsToTrue(t *testing.T) {
	run := testAgenticRun()
	agent := newTestAgentCaller()
	agent.analyzeResult = &AnalysisOutput{
		Success:        true,
		ActionRequired: nil,
		Options: []agenticv1alpha1.RemediationOption{{
			Title: "Increase memory",
			Diagnosis: agenticv1alpha1.DiagnosisResult{
				Summary: "OOM", Confidence: "High", RootCause: "Low limit",
			},
			RemediationPlan: agenticv1alpha1.RemediationPlan{
				Description: "Increase to 512Mi",
				Actions:     []agenticv1alpha1.ProposedAction{{Command: "kubectl patch deploy/web -n production -p '{}'", Type: "mutation", Description: "Patch"}},
				Risk:        "Low",
				Reversible:  agenticv1alpha1.ReversibilityReversible,
			},
		}},
	}
	r, _ := newReconcilerWithPolicy(t, run, agent, testAutoApprovePolicy())

	reconcileOnce(r, "fix-crash")
	assertPhase(t, r, "fix-crash", agenticv1alpha1.AgenticRunPhaseProposed)
}
