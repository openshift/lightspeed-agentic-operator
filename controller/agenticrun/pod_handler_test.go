package agenticrun

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"
)

// makeTokenUsage is a helper to build a TokenUsage value concisely.
func makeTokenUsage(in, out int64) agenticv1alpha1.TokenUsage {
	return agenticv1alpha1.TokenUsage{InputTokens: ptr.To(in), OutputTokens: ptr.To(out)}
}

// assertOptionalTokens compares one optional token count, treating nil as
// "not reported" rather than zero.
func assertOptionalTokens(t *testing.T, name string, got, want *int64) {
	t.Helper()
	switch {
	case want == nil && got != nil:
		t.Errorf("%s = %d, want absent", name, *got)
	case want != nil && got == nil:
		t.Errorf("%s absent, want %d", name, *want)
	case want != nil && got != nil && *got != *want:
		t.Errorf("%s = %d, want %d", name, *got, *want)
	}
}

// analysisResultWithTokenUsage wraps a TokenUsage in an AnalysisResult so the
// table-driven aggregation tests can hand it to aggregateTokenUsage.
func analysisResultWithTokenUsage(tu agenticv1alpha1.TokenUsage) client.Object {
	return &agenticv1alpha1.AnalysisResult{
		Status: agenticv1alpha1.AnalysisResultStatus{TokenUsage: tu},
	}
}

// assertTokenUsage is a test helper that checks a TokenUsage value is present
// (non-zero) and carries the expected input/output counts.
func assertTokenUsage(t *testing.T, tu agenticv1alpha1.TokenUsage, wantInput, wantOutput int64) {
	t.Helper()
	if tu.IsZero() {
		t.Fatal("expected non-zero tokenUsage")
	}
	if tu.InputTokens == nil || *tu.InputTokens != wantInput {
		t.Errorf("inputTokens = %v, want %d", tu.InputTokens, wantInput)
	}
	if tu.OutputTokens == nil || *tu.OutputTokens != wantOutput {
		t.Errorf("outputTokens = %v, want %d", tu.OutputTokens, wantOutput)
	}
}

// newVerifyingReconciler drives a run through analysis and execution and parks it
// in the Verifying phase with Verified=Unknown (verification sandbox launched but
// not yet completed). It returns the reconciler, its fake client, and the run so a
// test can create the VerificationResult CR itself and invoke the REAL pod-handler
// completion path (completeStep) directly — the paths TestReconcile_*Escalates
// exercise only through the testAgentCaller double.
func newVerifyingReconciler(t *testing.T) (*AgenticRunReconciler, *agenticv1alpha1.AgenticRun) {
	t.Helper()
	agent := newTestAgentCaller()
	scheme := testScheme()
	run := testAgenticRun()
	objs := append([]client.Object{run}, defaultObjects()...)
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
		WithStatusSubresource(run, &agenticv1alpha1.AnalysisResult{}, &agenticv1alpha1.ExecutionResult{}, &agenticv1alpha1.VerificationResult{}, &agenticv1alpha1.EscalationResult{}).Build()

	r := &AgenticRunReconciler{Client: fc, APIReader: fc, Agent: agent.withClient(t, fc, "default"), Namespace: "default"}

	mustReconcile(t, r, "fix-crash")      // analysis → proposed
	approveAgenticRun(t, fc, "fix-crash") // approve execution
	mustReconcile(t, r, "fix-crash")      // execution → Executed=True

	// Suppress the double's inline verification completion so handleVerification
	// only launches the sandbox (Verified=Unknown) and leaves the completion to us.
	agent.verifyResult = nil
	mustReconcile(t, r, "fix-crash") // verification launched → Verified=Unknown

	run, err := getAgenticRun(r, "fix-crash")
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if c := meta.FindStatusCondition(run.Status.Conditions, agenticv1alpha1.AgenticRunConditionVerified); c == nil || c.Status != metav1.ConditionUnknown {
		t.Fatalf("expected Verified=Unknown (Verifying) before completeStep, got %+v", c)
	}
	return r, run
}

func TestLateResultPreservesAcceptedSuspension(t *testing.T) {
	ctx := context.Background()
	r, stale := newVerifyingReconciler(t)
	current := stale.DeepCopy()
	meta.SetStatusCondition(&current.Status.Conditions, metav1.Condition{
		Type:               agenticv1alpha1.AgenticRunConditionStopping,
		Status:             metav1.ConditionTrue,
		Reason:             reasonSystemSuspended,
		LastTransitionTime: metav1.NewTime(time.Unix(100, 0).UTC()),
		ObservedGeneration: current.Generation,
	})
	if err := r.Status().Patch(ctx, current, client.MergeFromWithOptions(stale.DeepCopy(), client.MergeFromWithOptimisticLock{})); err != nil {
		t.Fatalf("persist accepted stop: %v", err)
	}

	late := &agenticv1alpha1.VerificationResult{
		ObjectMeta: metav1.ObjectMeta{Name: "late-verification", Namespace: "default"},
	}
	err := r.patchStepResult(ctx, stale, "verification",
		agenticv1alpha1.AgenticRunConditionVerified, late.Name, late,
		metav1.ConditionTrue, "Passed", "late result")
	if !errors.Is(err, errStopAccepted) {
		t.Fatalf("late result error = %v, want %v", err, errStopAccepted)
	}
	if err := r.patchStepCondition(ctx, stale, agenticv1alpha1.AgenticRunConditionVerified,
		metav1.ConditionFalse, ReasonSandboxTimeout, "late timeout"); !errors.Is(err, errStopAccepted) {
		t.Fatalf("late timeout condition error = %v, want %v", err, errStopAccepted)
	}
	if err := r.completeStep(ctx, stale, &corev1.Pod{}, "verification",
		agenticv1alpha1.AgenticRunConditionVerified, "late timeout", ReasonSandboxTimeout); !errors.Is(err, errStopAccepted) {
		t.Fatalf("timeout completion error = %v, want %v", err, errStopAccepted)
	}
	got, err := getAgenticRun(r, stale.Name)
	if err != nil {
		t.Fatal(err)
	}
	stopping := meta.FindStatusCondition(got.Status.Conditions, agenticv1alpha1.AgenticRunConditionStopping)
	verified := meta.FindStatusCondition(got.Status.Conditions, agenticv1alpha1.AgenticRunConditionVerified)
	if stopping == nil || stopping.Status != metav1.ConditionTrue ||
		verified == nil || verified.Status != metav1.ConditionUnknown {
		t.Fatalf("late result changed stopped workflow: %+v", got.Status.Conditions)
	}
	if len(got.Status.Steps.Verification.Results) != 0 {
		t.Fatal("late result was linked into the stopped run")
	}
}

func TestStoppedPodEventResolvesByUIDWithoutOwnerChain(t *testing.T) {
	ctx := context.Background()
	run := testAgenticRun()
	run.UID = "stopped-run-uid"
	run.Status.Conditions = []metav1.Condition{
		{
			Type:               agenticv1alpha1.AgenticRunConditionStopping,
			Status:             metav1.ConditionFalse,
			Reason:             reasonTeardownFailed,
			ObservedGeneration: run.Generation,
		},
		{
			Type:               agenticv1alpha1.AgenticRunConditionAnalyzed,
			Status:             metav1.ConditionFalse,
			Reason:             reasonEmergencyStopFailed,
			ObservedGeneration: run.Generation,
		},
	}
	fc := fake.NewClientBuilder().WithScheme(testScheme()).
		WithObjects(run).
		WithStatusSubresource(run).
		Build()
	r := &AgenticRunReconciler{
		Client:    fc,
		APIReader: fc,
		Config:    testCache(t, "sandbox-claim"),
		Namespace: "default",
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "late-claim-pod",
			Namespace: "default",
			Labels:    map[string]string{LabelRun: string(run.UID), LabelStep: "analysis"},
		},
		Status: corev1.PodStatus{Phase: corev1.PodSucceeded},
	}
	requests := r.handlePodEvent(ctx, pod)
	if len(requests) != 1 || requests[0].NamespacedName != client.ObjectKeyFromObject(run) {
		t.Fatalf("late stopped Pod requests = %+v, want run UID fallback request", requests)
	}
	got, err := getAgenticRun(r, run.Name)
	if err != nil {
		t.Fatal(err)
	}
	if verified := meta.FindStatusCondition(got.Status.Conditions, agenticv1alpha1.AgenticRunConditionVerified); verified != nil {
		t.Fatalf("late Pod event advanced a stopped run: %+v", verified)
	}
}

func TestPodNameAnnotationCannotSelectReplacementRun(t *testing.T) {
	ctx := context.Background()
	run := testAgenticRun()
	run.UID = "replacement-run-uid"
	fc := fake.NewClientBuilder().WithScheme(testScheme()).
		WithObjects(run).
		WithStatusSubresource(run).
		Build()
	r := &AgenticRunReconciler{
		Client:    fc,
		APIReader: fc,
		Config:    testCache(t, "sandbox-claim"),
		Namespace: "default",
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "stale-pod",
			Namespace:   "default",
			Labels:      map[string]string{LabelRun: "deleted-run-uid", LabelStep: "analysis"},
			Annotations: map[string]string{AnnotationRunName: run.Name},
		},
	}
	if requests := r.handlePodEvent(ctx, pod); len(requests) != 0 {
		t.Fatalf("stale Pod identity enqueued replacement run: %+v", requests)
	}
}

// createVerificationResult creates a VerificationResult CR named exactly as the
// pod handler's validateResultCR expects (resultCRName + nextResultIndex), with a
// Completed condition whose reason reflects outcome.
func createVerificationResult(t *testing.T, r *AgenticRunReconciler, run *agenticv1alpha1.AgenticRun, outcome agenticv1alpha1.ActionOutcome, checks []agenticv1alpha1.VerifyCheck, summary string) {
	t.Helper()
	ctx := context.Background()
	now := metav1.Now()
	crName := resultCRName(run.Name, "verification", nextResultIndex(run, "verification"))
	cr := &agenticv1alpha1.VerificationResult{
		ObjectMeta: metav1.ObjectMeta{
			Name:            crName,
			Namespace:       "default",
			Labels:          resultLabels(string(run.UID), "verification"),
			OwnerReferences: []metav1.OwnerReference{agenticRunOwnerRef(run)},
		},
	}
	if err := r.Create(ctx, cr); err != nil {
		t.Fatalf("create VerificationResult: %v", err)
	}
	cr.Status = agenticv1alpha1.VerificationResultStatus{
		Checks:     checks,
		Summary:    summary,
		Conditions: resultConditions(&now, now, outcome),
	}
	if err := r.Status().Update(ctx, cr); err != nil {
		t.Fatalf("update VerificationResult status: %v", err)
	}
}

// TestCompleteStep_VerificationObjectiveFailure_Escalates exercises the REAL
// pod-handler path (completeStep → patchVerificationFailedEscalating) for an
// objective verification failure: a valid VerificationResult that reports the
// remediation did not work. A regression here fails `make test` even though the
// double-driven TestReconcile_VerificationObjectiveFailure_Escalates would not.
func TestCompleteStep_VerificationObjectiveFailure_Escalates(t *testing.T) {
	ctx := context.Background()
	r, run := newVerifyingReconciler(t)

	createVerificationResult(t, r, run, agenticv1alpha1.ActionOutcomeFailed,
		[]agenticv1alpha1.VerifyCheck{{Name: "pod-running", Source: "oc", Value: "CrashLoopBackOff", Result: agenticv1alpha1.CheckResultFailed}},
		"Pod still crashing")

	pod := &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodSucceeded}}
	if err := r.completeStep(ctx, run, pod, "verification", stepConditionType("verification"), "", ""); err != nil {
		t.Fatalf("completeStep: %v", err)
	}

	got, err := getAgenticRun(r, "fix-crash")
	if err != nil {
		t.Fatalf("get run: %v", err)
	}

	if phase := agenticv1alpha1.DerivePhase(got.Status.Conditions); phase != agenticv1alpha1.AgenticRunPhaseEscalating {
		t.Fatalf("expected Escalating, got %s", phase)
	}
	verified := meta.FindStatusCondition(got.Status.Conditions, agenticv1alpha1.AgenticRunConditionVerified)
	if verified == nil || verified.Status != metav1.ConditionFalse || verified.Reason != agenticv1alpha1.ReasonVerificationFailed {
		t.Fatalf("expected Verified=False/VerificationFailed, got %+v", verified)
	}
	escalated := meta.FindStatusCondition(got.Status.Conditions, agenticv1alpha1.AgenticRunConditionEscalated)
	if escalated == nil || escalated.Status != metav1.ConditionUnknown || escalated.Reason != agenticv1alpha1.ReasonVerificationFailed {
		t.Fatalf("expected Escalated=Unknown/VerificationFailed, got %+v", escalated)
	}
	// Exactly one verification result ref, recorded as a failure — proves the run
	// did NOT re-run verification and did not lose the objective-failure outcome.
	refs := got.Status.Steps.Verification.Results
	if len(refs) != 1 {
		t.Fatalf("expected exactly 1 verification result ref, got %d", len(refs))
	}
	if refs[0].Outcome != agenticv1alpha1.ActionOutcomeFailed {
		t.Errorf("expected verification result outcome Failed, got %s", refs[0].Outcome)
	}
}

// TestCompleteStep_VerificationSuccess_Completed exercises the REAL pod-handler
// path for a passing verification (completeStep → patchStepResult): Verified=True
// and no Escalated condition, so DerivePhase yields Completed.
func TestCompleteStep_VerificationAgentTimeout_Escalates(t *testing.T) {
	ctx := context.Background()
	r, run := newVerifyingReconciler(t)

	name := resultCRName(run.Name, "verification", nextResultIndex(run, "verification"))
	createVerificationResult(t, r, run, agenticv1alpha1.ActionOutcomeFailed, nil, "Agent invocation timed out")
	result := &agenticv1alpha1.VerificationResult{}
	if err := r.Get(ctx, client.ObjectKey{Name: name, Namespace: "default"}, result); err != nil {
		t.Fatalf("get VerificationResult: %v", err)
	}
	completed := meta.FindStatusCondition(result.Status.Conditions, agenticv1alpha1.ResultConditionCompleted)
	completed.Reason = agenticv1alpha1.ResultReasonAgentTimeout
	completed.Message = "Agent invocation timeout"
	if err := r.Status().Update(ctx, result); err != nil {
		t.Fatalf("update VerificationResult timeout status: %v", err)
	}

	pod := &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodSucceeded}}
	if err := r.completeStep(ctx, run, pod, "verification", stepConditionType("verification"), "", ""); err != nil {
		t.Fatalf("completeStep: %v", err)
	}

	got, err := getAgenticRun(r, "fix-crash")
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if phase := agenticv1alpha1.DerivePhase(got.Status.Conditions); phase != agenticv1alpha1.AgenticRunPhaseEscalating {
		t.Fatalf("expected Escalating, got %s", phase)
	}
	verified := meta.FindStatusCondition(got.Status.Conditions, agenticv1alpha1.AgenticRunConditionVerified)
	if verified == nil || verified.Status != metav1.ConditionFalse {
		t.Fatalf("expected Verified=False, got %+v", verified)
	}
	escalated := meta.FindStatusCondition(got.Status.Conditions, agenticv1alpha1.AgenticRunConditionEscalated)
	if escalated == nil || escalated.Status != metav1.ConditionUnknown {
		t.Fatalf("expected Escalated=Unknown, got %+v", escalated)
	}
}

func TestCompleteStep_VerificationSuccess_Completed(t *testing.T) {
	ctx := context.Background()
	r, run := newVerifyingReconciler(t)

	createVerificationResult(t, r, run, agenticv1alpha1.ActionOutcomeSucceeded,
		[]agenticv1alpha1.VerifyCheck{{Name: "pod-running", Source: "oc", Value: "Running", Result: agenticv1alpha1.CheckResultPassed}},
		"Pod healthy")

	pod := &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodSucceeded}}
	if err := r.completeStep(ctx, run, pod, "verification", stepConditionType("verification"), "", ""); err != nil {
		t.Fatalf("completeStep: %v", err)
	}

	got, err := getAgenticRun(r, "fix-crash")
	if err != nil {
		t.Fatalf("get run: %v", err)
	}

	if phase := agenticv1alpha1.DerivePhase(got.Status.Conditions); phase != agenticv1alpha1.AgenticRunPhaseCompleted {
		t.Fatalf("expected Completed, got %s", phase)
	}
	verified := meta.FindStatusCondition(got.Status.Conditions, agenticv1alpha1.AgenticRunConditionVerified)
	if verified == nil || verified.Status != metav1.ConditionTrue {
		t.Fatalf("expected Verified=True, got %+v", verified)
	}
	if escalated := meta.FindStatusCondition(got.Status.Conditions, agenticv1alpha1.AgenticRunConditionEscalated); escalated != nil {
		t.Fatalf("expected no Escalated condition on success, got %+v", escalated)
	}
}

// TestCompleteStep_VerificationSystemFailure_Terminal exercises the REAL
// pod-handler path when the sandbox exits WITHOUT a VerificationResult CR (crash /
// no status): completeStep records Verified=False/SandboxFailed with NO Escalated
// condition, so the run is terminally Failed — it does NOT escalate. This locks
// the objective-vs-system split at the real code path.
func TestCompleteStep_VerificationSystemFailure_Terminal(t *testing.T) {
	ctx := context.Background()
	r, run := newVerifyingReconciler(t)

	// No VerificationResult CR is created — validateResultCR returns msgSandboxNoResult.
	pod := &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodSucceeded}}
	if err := r.completeStep(ctx, run, pod, "verification", stepConditionType("verification"), "", ""); err != nil {
		t.Fatalf("completeStep: %v", err)
	}

	got, err := getAgenticRun(r, "fix-crash")
	if err != nil {
		t.Fatalf("get run: %v", err)
	}

	if phase := agenticv1alpha1.DerivePhase(got.Status.Conditions); phase != agenticv1alpha1.AgenticRunPhaseFailed {
		t.Fatalf("expected Failed, got %s", phase)
	}
	verified := meta.FindStatusCondition(got.Status.Conditions, agenticv1alpha1.AgenticRunConditionVerified)
	if verified == nil || verified.Status != metav1.ConditionFalse || verified.Reason != ReasonSandboxFailed {
		t.Fatalf("expected Verified=False/SandboxFailed, got %+v", verified)
	}
	if escalated := meta.FindStatusCondition(got.Status.Conditions, agenticv1alpha1.AgenticRunConditionEscalated); escalated != nil {
		t.Fatalf("expected NO Escalated condition on system failure, got %+v", escalated)
	}
}

func TestCompleteStep_ToolResultSafetyInspectionFailure(t *testing.T) {
	ctx := context.Background()
	r, run := newVerifyingReconciler(t)

	pod := &corev1.Pod{Status: corev1.PodStatus{
		Phase: corev1.PodFailed,
		ContainerStatuses: []corev1.ContainerStatus{{
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
				ExitCode: 1,
				Message:  ReasonToolResultSafetyInspectionFailed,
			}},
		}},
	}}
	if err := r.completeStep(ctx, run, pod, "verification", stepConditionType("verification"), "", ""); err != nil {
		t.Fatalf("completeStep: %v", err)
	}

	got, err := getAgenticRun(r, "fix-crash")
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	verified := meta.FindStatusCondition(got.Status.Conditions, agenticv1alpha1.AgenticRunConditionVerified)
	if verified == nil || verified.Status != metav1.ConditionFalse {
		t.Fatalf("expected Verified=False, got %+v", verified)
	}
	if verified.Reason != "ToolResultSafetyInspectionFailed" {
		t.Errorf("Verified reason = %q, want ToolResultSafetyInspectionFailed", verified.Reason)
	}
	if verified.Message != "A possible security issue was detected in the tool output. The agentic run was stopped for safety." {
		t.Errorf("Verified message = %q", verified.Message)
	}
	if phase := agenticv1alpha1.DerivePhase(got.Status.Conditions); phase != agenticv1alpha1.AgenticRunPhaseFailed {
		t.Errorf("phase = %s, want Failed", phase)
	}
}

func TestAggregateTokenUsage(t *testing.T) {
	tests := []struct {
		name       string
		resultCR   client.Object
		initial    agenticv1alpha1.TokenUsage
		wantInput  int64
		wantOutput int64
		wantZero   bool
	}{
		{
			name: "first contribution",
			resultCR: &agenticv1alpha1.AnalysisResult{
				Status: agenticv1alpha1.AnalysisResultStatus{TokenUsage: makeTokenUsage(100, 50)},
			},
			wantInput:  100,
			wantOutput: 50,
		},
		{
			name: "adds to the total carried so far",
			resultCR: &agenticv1alpha1.ExecutionResult{
				Status: agenticv1alpha1.ExecutionResultStatus{TokenUsage: makeTokenUsage(200, 80)},
			},
			initial:    makeTokenUsage(100, 50),
			wantInput:  300,
			wantOutput: 130,
		},
		{
			name: "a step reporting nothing leaves the total alone",
			resultCR: &agenticv1alpha1.VerificationResult{
				Status: agenticv1alpha1.VerificationResultStatus{},
			},
			wantZero: true,
		},
		{
			name: "a step reporting nothing preserves the existing total",
			resultCR: &agenticv1alpha1.EscalationResult{
				Status: agenticv1alpha1.EscalationResultStatus{},
			},
			initial:    makeTokenUsage(100, 50),
			wantInput:  100,
			wantOutput: 50,
		},
		{
			name: "explicit zeros are values",
			resultCR: &agenticv1alpha1.AnalysisResult{
				Status: agenticv1alpha1.AnalysisResultStatus{TokenUsage: makeTokenUsage(0, 0)},
			},
			wantInput:  0,
			wantOutput: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			run := &agenticv1alpha1.AgenticRun{
				Status: agenticv1alpha1.AgenticRunStatus{
					TokenUsage: *tt.initial.DeepCopy(),
				},
			}

			aggregateTokenUsage(run, extractTokenUsage(tt.resultCR))

			if tt.wantZero {
				if !run.Status.TokenUsage.IsZero() && tt.initial.IsZero() {
					t.Fatalf("expected zero tokenUsage, got %+v", run.Status.TokenUsage)
				}
				return
			}
			assertTokenUsage(t, run.Status.TokenUsage, tt.wantInput, tt.wantOutput)
		})
	}
}

// TestAggregateTokenUsage_OptionalFields pins the rules for reasoningTokens and
// toolTokens (sum), peakInputTokens (maximum), model (never aggregated), and that
// an unreported count adds nothing instead of clearing the total.
func TestAggregateTokenUsage_OptionalFields(t *testing.T) {
	i := func(v int64) *int64 { return ptr.To(v) }

	tests := []struct {
		name          string
		initial       agenticv1alpha1.TokenUsage
		result        agenticv1alpha1.TokenUsage
		wantReasoning *int64
		wantTool      *int64
		wantPeak      *int64
		wantInput     *int64
		wantOutput    *int64
	}{
		{
			name:          "first contribution carries every reported field",
			result:        agenticv1alpha1.TokenUsage{InputTokens: i(100), OutputTokens: i(50), ReasoningTokens: i(20), ToolTokens: i(30), PeakInputTokens: i(100), Model: "claude-x"},
			wantReasoning: i(20),
			wantTool:      i(30),
			wantPeak:      i(100),
			wantInput:     i(100),
			wantOutput:    i(50),
		},
		{
			name:       "unreported optional fields stay absent",
			result:     agenticv1alpha1.TokenUsage{InputTokens: i(100), OutputTokens: i(50)},
			wantInput:  i(100),
			wantOutput: i(50),
		},
		{
			name:          "later contribution sums reported values",
			initial:       agenticv1alpha1.TokenUsage{InputTokens: i(100), OutputTokens: i(50), ReasoningTokens: i(20), ToolTokens: i(30), PeakInputTokens: i(100)},
			result:        agenticv1alpha1.TokenUsage{InputTokens: i(200), OutputTokens: i(80), ReasoningTokens: i(5), ToolTokens: i(7), PeakInputTokens: i(125)},
			wantReasoning: i(25),
			wantTool:      i(37),
			wantPeak:      i(125),
			wantInput:     i(300),
			wantOutput:    i(130),
		},
		{
			name:          "an unreported optional field does not clear the run value",
			initial:       agenticv1alpha1.TokenUsage{InputTokens: i(100), OutputTokens: i(50), ReasoningTokens: i(20), ToolTokens: i(30), PeakInputTokens: i(100)},
			result:        agenticv1alpha1.TokenUsage{InputTokens: i(200), OutputTokens: i(80)},
			wantReasoning: i(20),
			wantTool:      i(30),
			wantPeak:      i(100),
			wantInput:     i(300),
			wantOutput:    i(130),
		},
		{
			name:          "peak keeps the larger value in either direction",
			initial:       agenticv1alpha1.TokenUsage{InputTokens: i(100), OutputTokens: i(50), PeakInputTokens: i(100)},
			result:        agenticv1alpha1.TokenUsage{InputTokens: i(200), OutputTokens: i(80), PeakInputTokens: i(75)},
			wantPeak:      i(100),
			wantInput:     i(300),
			wantOutput:    i(130),
			wantReasoning: nil,
		},
		{
			name:          "explicit zero is a reported value, not absence",
			initial:       agenticv1alpha1.TokenUsage{InputTokens: i(100), OutputTokens: i(50), ReasoningTokens: i(0), PeakInputTokens: i(0)},
			result:        agenticv1alpha1.TokenUsage{InputTokens: i(200), OutputTokens: i(80), ReasoningTokens: i(5), PeakInputTokens: i(125)},
			wantReasoning: i(5),
			wantPeak:      i(125),
			wantInput:     i(300),
			wantOutput:    i(130),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			run := &agenticv1alpha1.AgenticRun{
				Status: agenticv1alpha1.AgenticRunStatus{TokenUsage: *tt.initial.DeepCopy()},
			}

			aggregateTokenUsage(run, tt.result)

			got := run.Status.TokenUsage
			assertOptionalTokens(t, "inputTokens", got.InputTokens, tt.wantInput)
			assertOptionalTokens(t, "outputTokens", got.OutputTokens, tt.wantOutput)
			assertOptionalTokens(t, "reasoningTokens", got.ReasoningTokens, tt.wantReasoning)
			assertOptionalTokens(t, "toolTokens", got.ToolTokens, tt.wantTool)
			assertOptionalTokens(t, "peakInputTokens", got.PeakInputTokens, tt.wantPeak)
			if got.Model != "" {
				t.Errorf("model must not be aggregated onto AgenticRun, got %q", got.Model)
			}
		})
	}
}

// TestAggregateTokenUsage_NoReseed pins the two sequences the old
// "is the aggregate zero?" seeding rule got wrong: it re-copied the later
// contribution instead of adding to it, publishing a total that was neither the
// run's sum nor marked as partial.
func TestAggregateTokenUsage_NoReseed(t *testing.T) {
	i := func(v int64) *int64 { return ptr.To(v) }

	t.Run("disjoint partial contributions add up", func(t *testing.T) {
		run := &agenticv1alpha1.AgenticRun{}
		aggregateTokenUsage(run, agenticv1alpha1.TokenUsage{InputTokens: i(100)})
		aggregateTokenUsage(run, agenticv1alpha1.TokenUsage{OutputTokens: i(50)})
		aggregateTokenUsage(run, agenticv1alpha1.TokenUsage{InputTokens: i(200), OutputTokens: i(20), PeakInputTokens: i(300)})

		got := run.Status.TokenUsage
		assertOptionalTokens(t, "inputTokens", got.InputTokens, i(300))
		assertOptionalTokens(t, "outputTokens", got.OutputTokens, i(70))
		assertOptionalTokens(t, "peakInputTokens", got.PeakInputTokens, i(300))
	})

	t.Run("a step that reports nothing does not let the next one overwrite", func(t *testing.T) {
		run := &agenticv1alpha1.AgenticRun{}
		aggregateTokenUsage(run, agenticv1alpha1.TokenUsage{}) // e.g. a pre-OLS-4921 sandbox
		aggregateTokenUsage(run, agenticv1alpha1.TokenUsage{InputTokens: i(400), OutputTokens: i(150)})
		aggregateTokenUsage(run, agenticv1alpha1.TokenUsage{InputTokens: i(100), OutputTokens: i(50)})

		got := run.Status.TokenUsage
		assertOptionalTokens(t, "inputTokens", got.InputTokens, i(500))
		assertOptionalTokens(t, "outputTokens", got.OutputTokens, i(200))
	})
}

// TestTokenUsage_IsZero pins presence across every field: IsZero means "nothing
// was reported", so a record carrying only an optional field must survive
// serialization rather than being dropped as empty.
func TestTokenUsage_IsZero(t *testing.T) {
	i := func(v int64) *int64 { return ptr.To(v) }

	tests := []struct {
		name string
		tu   agenticv1alpha1.TokenUsage
		want bool
	}{
		{name: "empty", tu: agenticv1alpha1.TokenUsage{}, want: true},
		{name: "input only", tu: agenticv1alpha1.TokenUsage{InputTokens: i(1)}},
		{name: "output only", tu: agenticv1alpha1.TokenUsage{OutputTokens: i(1)}},
		{name: "reasoning only", tu: agenticv1alpha1.TokenUsage{ReasoningTokens: i(1)}},
		{name: "tool only", tu: agenticv1alpha1.TokenUsage{ToolTokens: i(1)}},
		{name: "peak only", tu: agenticv1alpha1.TokenUsage{PeakInputTokens: i(1)}},
		{name: "model only", tu: agenticv1alpha1.TokenUsage{Model: "claude-x"}},
		{name: "explicit zeros", tu: agenticv1alpha1.TokenUsage{InputTokens: i(0), OutputTokens: i(0)}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.tu.IsZero(); got != tt.want {
				t.Errorf("IsZero() = %v, want %v", got, tt.want)
			}
		})
	}
}

// setupVerificationResultCR creates a VerificationResult CR with tokenUsage,
// named exactly as validateResultCR expects. Shared fixture for the table-driven
// TestCompleteStep_AggregatesTokenUsage.
func setupVerificationResultCR(t *testing.T, r *AgenticRunReconciler, run *agenticv1alpha1.AgenticRun, outcome agenticv1alpha1.ActionOutcome, checks []agenticv1alpha1.VerifyCheck, summary string, tu agenticv1alpha1.TokenUsage) {
	t.Helper()
	ctx := context.Background()
	now := metav1.Now()
	crName := resultCRName(run.Name, "verification", nextResultIndex(run, "verification"))
	cr := &agenticv1alpha1.VerificationResult{
		ObjectMeta: metav1.ObjectMeta{
			Name:            crName,
			Namespace:       "default",
			Labels:          resultLabels(string(run.UID), "verification"),
			OwnerReferences: []metav1.OwnerReference{agenticRunOwnerRef(run)},
		},
	}
	if err := r.Create(ctx, cr); err != nil {
		t.Fatalf("create VerificationResult: %v", err)
	}
	cr.Status = agenticv1alpha1.VerificationResultStatus{
		Checks:     checks,
		Summary:    summary,
		Conditions: resultConditions(&now, now, outcome),
		TokenUsage: tu,
	}
	if err := r.Status().Update(ctx, cr); err != nil {
		t.Fatalf("update VerificationResult status: %v", err)
	}
}

// TestCompleteStep_AggregatesTokenUsage exercises the integration path for
// tokenUsage aggregation through completeStep on both the success and
// verification-failed → escalating paths.
func TestCompleteStep_AggregatesTokenUsage(t *testing.T) {
	tests := []struct {
		name       string
		outcome    agenticv1alpha1.ActionOutcome
		checks     []agenticv1alpha1.VerifyCheck
		summary    string
		tu         agenticv1alpha1.TokenUsage
		wantPhase  agenticv1alpha1.AgenticRunPhase
		wantInput  int64
		wantOutput int64
	}{
		{
			name:       "success path",
			outcome:    agenticv1alpha1.ActionOutcomeSucceeded,
			checks:     []agenticv1alpha1.VerifyCheck{{Name: "pod-running", Source: "oc", Value: "Running", Result: agenticv1alpha1.CheckResultPassed}},
			summary:    "Pod healthy",
			tu:         makeTokenUsage(500, 200),
			wantPhase:  agenticv1alpha1.AgenticRunPhaseCompleted,
			wantInput:  500,
			wantOutput: 200,
		},
		{
			name:       "verification-failed escalating path",
			outcome:    agenticv1alpha1.ActionOutcomeFailed,
			checks:     []agenticv1alpha1.VerifyCheck{{Name: "pod-running", Source: "oc", Value: "CrashLoopBackOff", Result: agenticv1alpha1.CheckResultFailed}},
			summary:    "Pod still crashing",
			tu:         makeTokenUsage(300, 120),
			wantPhase:  agenticv1alpha1.AgenticRunPhaseEscalating,
			wantInput:  300,
			wantOutput: 120,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			r, run := newVerifyingReconciler(t)

			setupVerificationResultCR(t, r, run, tt.outcome, tt.checks, tt.summary, tt.tu)

			pod := &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodSucceeded}}
			if err := r.completeStep(ctx, run, pod, "verification", stepConditionType("verification"), "", ""); err != nil {
				t.Fatalf("completeStep: %v", err)
			}

			got, err := getAgenticRun(r, "fix-crash")
			if err != nil {
				t.Fatalf("get run: %v", err)
			}

			if phase := agenticv1alpha1.DerivePhase(got.Status.Conditions); phase != tt.wantPhase {
				t.Fatalf("expected %s, got %s", tt.wantPhase, phase)
			}
			assertTokenUsage(t, got.Status.TokenUsage, tt.wantInput, tt.wantOutput)
		})
	}
}

// TestCompleteStep_TwiceAggregatesUsageOnce pins the guard that makes step
// completion safe under concurrent callers: completeStep re-reads the run and
// returns early once the step condition is terminal, and status patches carry an
// optimistic lock, so a duplicate completion (pod event and timeout ticker can
// both reach it) cannot aggregate the same Result CR twice. This is what a
// package-level mutex used to be credited with.
func TestCompleteStep_TwiceAggregatesUsageOnce(t *testing.T) {
	ctx := context.Background()
	r, run := newVerifyingReconciler(t)

	setupVerificationResultCR(t, r, run, agenticv1alpha1.ActionOutcomeSucceeded,
		[]agenticv1alpha1.VerifyCheck{{Name: "pod-running", Source: "oc", Value: "Running", Result: agenticv1alpha1.CheckResultPassed}},
		"Pod healthy", makeTokenUsage(500, 200))

	pod := &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodSucceeded}}
	for i := 0; i < 2; i++ {
		if err := r.completeStep(ctx, run, pod, "verification", stepConditionType("verification"), "", ""); err != nil {
			t.Fatalf("completeStep attempt %d: %v", i+1, err)
		}
	}

	got, err := getAgenticRun(r, "fix-crash")
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	assertTokenUsage(t, got.Status.TokenUsage, 500, 200)
}

// TestPatchStepResult_SkipsWhenStepAlreadyTerminal covers the window
// TestCompleteStep_TwiceAggregatesUsageOnce cannot: completeStep's own terminal
// check has already passed on a stale read when a second caller reaches
// patchStepResult, whose re-read then returns the state the first caller
// committed. The fresh read carries the terminal condition and the aggregated
// usage, so aggregating again would double the totals — the optimistic lock
// cannot help, because the second write's base is that same fresh version.
func TestPatchStepResult_SkipsWhenStepAlreadyTerminal(t *testing.T) {
	ctx := context.Background()
	r, run := newVerifyingReconciler(t)

	setupVerificationResultCR(t, r, run, agenticv1alpha1.ActionOutcomeSucceeded,
		[]agenticv1alpha1.VerifyCheck{{Name: "pod-running", Source: "oc", Value: "Running", Result: agenticv1alpha1.CheckResultPassed}},
		"Pod healthy", makeTokenUsage(500, 200))

	crName := resultCRName(run.Name, "verification", nextResultIndex(run, "verification"))
	cr := &agenticv1alpha1.VerificationResult{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: "default", Name: crName}, cr); err != nil {
		t.Fatalf("get VerificationResult: %v", err)
	}

	pod := &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodSucceeded}}
	if err := r.completeStep(ctx, run, pod, "verification", stepConditionType("verification"), "", ""); err != nil {
		t.Fatalf("first completion: %v", err)
	}

	// The loser of the race still holds the pre-completion run, so it gets here.
	if err := r.patchStepResult(ctx, run, "verification", stepConditionType("verification"), crName, cr,
		metav1.ConditionTrue, ReasonSucceeded, "Sandbox completed"); err != nil {
		t.Fatalf("late patchStepResult: %v", err)
	}

	got, err := getAgenticRun(r, "fix-crash")
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	assertTokenUsage(t, got.Status.TokenUsage, 500, 200)
	if refs := got.Status.Steps.Verification.Results; len(refs) != 1 {
		t.Errorf("verification result refs = %d, want 1 (a late caller must not append a duplicate)", len(refs))
	}
}

// TestPatchVerificationFailedEscalating_SkipsWhenAlreadyTerminal is the same
// guard for the escalating path, which writes Verified=False instead of a
// success condition.
func TestPatchVerificationFailedEscalating_SkipsWhenAlreadyTerminal(t *testing.T) {
	ctx := context.Background()
	r, run := newVerifyingReconciler(t)

	setupVerificationResultCR(t, r, run, agenticv1alpha1.ActionOutcomeFailed,
		[]agenticv1alpha1.VerifyCheck{{Name: "pod-running", Source: "oc", Value: "CrashLoopBackOff", Result: agenticv1alpha1.CheckResultFailed}},
		"Pod still crashing", makeTokenUsage(300, 120))

	crName := resultCRName(run.Name, "verification", nextResultIndex(run, "verification"))
	cr := &agenticv1alpha1.VerificationResult{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: "default", Name: crName}, cr); err != nil {
		t.Fatalf("get VerificationResult: %v", err)
	}

	for i := 0; i < 2; i++ {
		if err := r.patchVerificationFailedEscalating(ctx, run, crName, cr, agenticv1alpha1.ReasonVerificationFailed); err != nil {
			t.Fatalf("patch attempt %d: %v", i+1, err)
		}
	}

	got, err := getAgenticRun(r, "fix-crash")
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	assertTokenUsage(t, got.Status.TokenUsage, 300, 120)
	if refs := got.Status.Steps.Verification.Results; len(refs) != 1 {
		t.Errorf("verification result refs = %d, want 1", len(refs))
	}
}

func TestPodFailMessage(t *testing.T) {
	exit := int32(1)

	tests := []struct {
		name string
		pod  *corev1.Pod
		want string
	}{
		{
			name: "succeeded without result",
			pod:  &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodSucceeded}},
			want: msgSandboxNoResult,
		},
		{
			name: "failed with termination message",
			pod: &corev1.Pod{Status: corev1.PodStatus{
				Phase: corev1.PodFailed,
				ContainerStatuses: []corev1.ContainerStatus{{
					State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
						Message: "OOMKilled",
					}},
				}},
			}},
			want: "OOMKilled",
		},
		{
			name: "tool result safety inspection failure",
			pod: &corev1.Pod{Status: corev1.PodStatus{
				Phase: corev1.PodFailed,
				ContainerStatuses: []corev1.ContainerStatus{{
					State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
						Message: ReasonToolResultSafetyInspectionFailed,
					}},
				}},
			}},
			want: "A possible security issue was detected in the tool output. The agentic run was stopped for safety.",
		},
		{
			name: "failed with exit code",
			pod: &corev1.Pod{Status: corev1.PodStatus{
				Phase: corev1.PodFailed,
				ContainerStatuses: []corev1.ContainerStatus{{
					State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
						ExitCode: exit,
					}},
				}},
			}},
			want: "sandbox pod failed (exit 1)",
		},
		{
			name: "failed without details",
			pod:  &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodFailed}},
			want: "sandbox pod failed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := podFailMessage(tt.pod)
			if got != tt.want {
				t.Errorf("podFailMessage = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestStartTimedOut(t *testing.T) {
	now := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name    string
		phase   corev1.PodPhase
		created time.Time
		timeout time.Duration
		want    bool
	}{
		{"pending past deadline", corev1.PodPending, now.Add(-6 * time.Minute), 5 * time.Minute, true},
		{"pending within deadline", corev1.PodPending, now.Add(-2 * time.Minute), 5 * time.Minute, false},
		{"unknown past deadline", corev1.PodUnknown, now.Add(-6 * time.Minute), 5 * time.Minute, true},
		{"running ignores start timeout", corev1.PodRunning, now.Add(-6 * time.Minute), 5 * time.Minute, false},
		{"succeeded ignores start timeout", corev1.PodSucceeded, now.Add(-6 * time.Minute), 5 * time.Minute, false},
		{"failed ignores start timeout", corev1.PodFailed, now.Add(-6 * time.Minute), 5 * time.Minute, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := startTimedOut(tt.phase, tt.created, now, tt.timeout)
			if got != tt.want {
				t.Errorf("startTimedOut = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestMainContainerStartedAt(t *testing.T) {
	started := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	pod := &corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{
		{Name: "sidecar", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(started.Add(-time.Minute))}}},
		{Name: "agent", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(started)}}},
	}}}
	if got := mainContainerStartedAt(pod); !got.Equal(started) {
		t.Fatalf("mainContainerStartedAt = %v, want %v", got, started)
	}
	if got := mainContainerStartedAt(&corev1.Pod{}); !got.IsZero() {
		t.Fatalf("mainContainerStartedAt without running container = %v, want zero", got)
	}
}

func TestOverallTimedOut(t *testing.T) {
	now := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name    string
		created time.Time
		timeout time.Duration
		want    bool
	}{
		{"past deadline", now.Add(-15 * time.Minute), 10 * time.Minute, true},
		{"within deadline", now.Add(-5 * time.Minute), 10 * time.Minute, false},
		{"exactly at deadline", now.Add(-10 * time.Minute), 10 * time.Minute, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := overallTimedOut(tt.created, now, tt.timeout)
			if got != tt.want {
				t.Errorf("overallTimedOut = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestPodTerminatedInfo(t *testing.T) {
	msg, code := podTerminatedInfo(nil)
	if msg != "" || code != nil {
		t.Fatalf("nil pod: %q %v", msg, code)
	}
	pod := &corev1.Pod{Status: corev1.PodStatus{
		ContainerStatuses: []corev1.ContainerStatus{{
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
				ExitCode: 2,
				Message:  "boom",
			}},
		}},
	}}
	msg, code = podTerminatedInfo(pod)
	if msg != "boom" || code == nil || *code != 2 {
		t.Fatalf("got msg=%q code=%v", msg, code)
	}
}

// fullTokenUsage returns a TokenUsage with every field reported.
func fullTokenUsage() agenticv1alpha1.TokenUsage {
	return agenticv1alpha1.TokenUsage{
		InputTokens:     ptr.To[int64](100),
		OutputTokens:    ptr.To[int64](50),
		ReasoningTokens: ptr.To[int64](20),
		ToolTokens:      ptr.To[int64](30),
		PeakInputTokens: ptr.To[int64](100),
	}
}

// bedrockTokenUsage is the shape Anthropic on Bedrock/Vertex produces today: the
// provider reports input and output but no reasoning details, so the sandbox MUST
// omit reasoningTokens rather than report 0.
func bedrockTokenUsage() agenticv1alpha1.TokenUsage {
	tu := fullTokenUsage()
	tu.ReasoningTokens = nil
	return tu
}

// reportStepUsage mirrors the production call order: the run aggregate is updated
// first, then completeness of the step's token usage is recorded.
func reportStepUsage(r *AgenticRunReconciler, run *agenticv1alpha1.AgenticRun, step string, incoming agenticv1alpha1.TokenUsage, stepSucceeded bool) {
	aggregateTokenUsage(run, incoming)
	r.reportTokenUsage(context.Background(), run, step, incoming, stepSucceeded)
}

// TestAssessTokenUsageReporting pins the severity split: only missing required
// fields (or an absent struct) are negative; omitted optional fields keep the
// assessment positive.
func TestAssessTokenUsageReporting(t *testing.T) {
	tests := []struct {
		name            string
		usage           agenticv1alpha1.TokenUsage
		wantStatus      metav1.ConditionStatus
		wantReason      string
		wantMessage     string
		wantRequired    []string
		wantOptionalGap []string
	}{
		{
			name:        "all fields reported",
			usage:       fullTokenUsage(),
			wantStatus:  metav1.ConditionTrue,
			wantReason:  agenticv1alpha1.ReasonTokenUsageReportedComplete,
			wantMessage: "All required token usage fields were reported",
		},
		{
			name: "reasoning reported as an explicit zero counts as reported",
			usage: func() agenticv1alpha1.TokenUsage {
				tu := fullTokenUsage()
				tu.ReasoningTokens = ptr.To[int64](0)
				return tu
			}(),
			wantStatus: metav1.ConditionTrue,
			wantReason: agenticv1alpha1.ReasonTokenUsageReportedComplete,
		},
		{
			name:            "reasoning omitted stays positive (Bedrock and Vertex today)",
			usage:           bedrockTokenUsage(),
			wantStatus:      metav1.ConditionTrue,
			wantReason:      agenticv1alpha1.ReasonTokenUsageReportedComplete,
			wantOptionalGap: []string{"reasoningTokens"},
		},
		{
			name: "reasoning and tool omitted stay positive",
			usage: agenticv1alpha1.TokenUsage{
				InputTokens:     ptr.To[int64](100),
				OutputTokens:    ptr.To[int64](50),
				PeakInputTokens: ptr.To[int64](100),
			},
			wantStatus:      metav1.ConditionTrue,
			wantReason:      agenticv1alpha1.ReasonTokenUsageReportedComplete,
			wantOptionalGap: []string{"reasoningTokens", "toolTokens"},
		},
		{
			name: "peak absent is an alarm: it is required context occupancy",
			usage: agenticv1alpha1.TokenUsage{
				InputTokens:     ptr.To[int64](100),
				OutputTokens:    ptr.To[int64](50),
				ReasoningTokens: ptr.To[int64](20),
				ToolTokens:      ptr.To[int64](30),
			},
			wantStatus:   metav1.ConditionFalse,
			wantReason:   agenticv1alpha1.ReasonTokenUsageReportedIncomplete,
			wantRequired: []string{"peakInputTokens"},
		},
		{
			name:         "entire struct absent is bad",
			usage:        agenticv1alpha1.TokenUsage{},
			wantStatus:   metav1.ConditionFalse,
			wantReason:   agenticv1alpha1.ReasonTokenUsageReportedNone,
			wantRequired: []string{"inputTokens", "outputTokens", "peakInputTokens"},
		},
		{
			name: "output absent is bad",
			usage: agenticv1alpha1.TokenUsage{
				InputTokens: ptr.To[int64](100),
			},
			wantStatus:      metav1.ConditionFalse,
			wantReason:      agenticv1alpha1.ReasonTokenUsageReportedIncomplete,
			wantRequired:    []string{"outputTokens", "peakInputTokens"},
			wantOptionalGap: []string{"reasoningTokens", "toolTokens"},
		},
		{
			name: "input absent is bad",
			usage: agenticv1alpha1.TokenUsage{
				OutputTokens: ptr.To[int64](50),
			},
			wantStatus:      metav1.ConditionFalse,
			wantReason:      agenticv1alpha1.ReasonTokenUsageReportedIncomplete,
			wantRequired:    []string{"inputTokens", "peakInputTokens"},
			wantOptionalGap: []string{"reasoningTokens", "toolTokens"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := assessTokenUsageReporting(tt.usage)
			if got.Status != tt.wantStatus {
				t.Errorf("status = %q, want %q", got.Status, tt.wantStatus)
			}
			if got.Reason != tt.wantReason {
				t.Errorf("reason = %q, want %q", got.Reason, tt.wantReason)
			}
			if strings.Join(got.RequiredMissing, ",") != strings.Join(tt.wantRequired, ",") {
				t.Errorf("required missing = %v, want %v", got.RequiredMissing, tt.wantRequired)
			}
			if strings.Join(got.OptionalMissing, ",") != strings.Join(tt.wantOptionalGap, ",") {
				t.Errorf("optional missing = %v, want %v", got.OptionalMissing, tt.wantOptionalGap)
			}
			if got.Message == "" {
				t.Error("message must never be empty")
			}
			if tt.wantMessage != "" && got.Message != tt.wantMessage {
				t.Errorf("message = %q, want %q", got.Message, tt.wantMessage)
			}
		})
	}
}

// TestTokenUsageJSON_HasNoSkillTokens guards the decision that skill tokens are
// part of toolTokens: the field must not exist on the API.
func TestTokenUsageJSON_HasNoSkillTokens(t *testing.T) {
	tu := agenticv1alpha1.TokenUsage{
		InputTokens:     ptr.To[int64](100),
		OutputTokens:    ptr.To[int64](50),
		ReasoningTokens: ptr.To[int64](20),
		ToolTokens:      ptr.To[int64](30),
		PeakInputTokens: ptr.To[int64](100),
		Model:           "claude-sonnet-4-5-20250929",
	}
	raw, err := json.Marshal(tu)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(raw), "skill") {
		t.Errorf("tokenUsage JSON must not expose skill tokens, got %s", raw)
	}
}

// TestCRDTokenUsageSchema pins the generated CRD contract for tokenUsage. Unit
// tests use a fake client that ignores schemas entirely, so without this guard a
// regenerated CRD could quietly reintroduce `required` fields and every real
// sandbox that under-reports would have its whole status patch rejected — losing
// the failure reason, diagnosis and options along with the token counts.
//
// The contract (spec: token-usage.md rules 3a-3b, crd-api.md rule 6c.1): no field
// is required, minProperties keeps a present-but-empty record invalid, and every
// counter is individually expressible.
func TestCRDTokenUsageSchema(t *testing.T) {
	crdsWithTokenUsage := []string{
		"agenticruns",
		"analysisresults",
		"executionresults",
		"verificationresults",
		"escalationresults",
	}
	fields := []string{"inputTokens", "outputTokens", "reasoningTokens", "toolTokens", "peakInputTokens", "model"}

	for _, name := range crdsWithTokenUsage {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join("..", "..", "config", "crd", "bases", "agentic.openshift.io_"+name+".yaml")
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read %s: %v", path, err)
			}

			var crd apiextensionsv1.CustomResourceDefinition
			if err := yaml.Unmarshal(raw, &crd); err != nil {
				t.Fatalf("unmarshal %s: %v", path, err)
			}

			schema := findPropertySchema(&crd, "tokenUsage")
			if schema == nil {
				t.Fatalf("%s: no tokenUsage schema found", name)
			}

			if len(schema.Required) != 0 {
				t.Errorf("tokenUsage.required = %v, want none: a missing count must never reject a status patch",
					schema.Required)
			}
			if schema.MinProperties == nil || *schema.MinProperties != 1 {
				t.Errorf("tokenUsage.minProperties = %v, want 1 (a present-but-empty record is meaningless)",
					schema.MinProperties)
			}
			for _, field := range fields {
				if _, ok := schema.Properties[field]; !ok {
					t.Errorf("tokenUsage has no %q property", field)
				}
			}
			if _, ok := schema.Properties["skillTokens"]; ok {
				t.Error("tokenUsage has a skillTokens property: skill content is read through tools and is part of toolTokens")
			}
		})
	}
}

// findPropertySchema returns the first schema for the named property in any
// version of the CRD, searching nested properties.
func findPropertySchema(crd *apiextensionsv1.CustomResourceDefinition, property string) *apiextensionsv1.JSONSchemaProps {
	var walk func(*apiextensionsv1.JSONSchemaProps) *apiextensionsv1.JSONSchemaProps
	walk = func(schema *apiextensionsv1.JSONSchemaProps) *apiextensionsv1.JSONSchemaProps {
		if schema == nil {
			return nil
		}
		if found, ok := schema.Properties[property]; ok {
			return &found
		}
		for name := range schema.Properties {
			child := schema.Properties[name]
			if found := walk(&child); found != nil {
				return found
			}
		}
		if schema.Items != nil && schema.Items.Schema != nil {
			return walk(schema.Items.Schema)
		}
		return nil
	}

	for i := range crd.Spec.Versions {
		if found := walk(crd.Spec.Versions[i].Schema.OpenAPIV3Schema); found != nil {
			return found
		}
	}
	return nil
}

// TestReportTokenUsage_BedrockOmissionStaysPositive is the contract test for the
// current provider deficiency: an omitted reasoningTokens must not warn, must not
// flip the condition, and must leave the run aggregate without reasoning.
func TestReportTokenUsage_BedrockOmissionStaysPositive(t *testing.T) {
	recorder := record.NewFakeRecorder(20)
	r := &AgenticRunReconciler{EventRecorder: recorder}
	run := &agenticv1alpha1.AgenticRun{}

	before := testutil.ToFloat64(tokenUsageMissingTotal.WithLabelValues("reasoningTokens"))
	reportStepUsage(r, run, "analysis", bedrockTokenUsage(), true)

	if got := testutil.ToFloat64(tokenUsageMissingTotal.WithLabelValues("reasoningTokens")) - before; got != 1 {
		t.Errorf("reasoningTokens omission counter delta = %v, want 1", got)
	}
	select {
	case event := <-recorder.Events:
		t.Fatalf("omitting reasoningTokens must not warn, got %q", event)
	default:
	}

	c := meta.FindStatusCondition(run.Status.Conditions, agenticv1alpha1.AgenticRunConditionTokenUsageReported)
	if c == nil || c.Status != metav1.ConditionTrue || c.Reason != agenticv1alpha1.ReasonTokenUsageReportedComplete {
		t.Fatalf("condition = %+v, want True/%s", c, agenticv1alpha1.ReasonTokenUsageReportedComplete)
	}

	if run.Status.TokenUsage.ReasoningTokens != nil {
		t.Errorf("run reasoningTokens = %d, want absent (provider could not report it)",
			*run.Status.TokenUsage.ReasoningTokens)
	}
	if run.Status.TokenUsage.InputTokens == nil || *run.Status.TokenUsage.InputTokens != 100 {
		t.Errorf("run inputTokens = %v, want 100", run.Status.TokenUsage.InputTokens)
	}
}

// TestReportTokenUsage_MissingRequiredIsLoud verifies a dropped required field is
// visible through the counter and negative condition without creating a Kubernetes event.
func TestReportTokenUsage_MissingRequiredIsLoud(t *testing.T) {
	recorder := record.NewFakeRecorder(20)
	r := &AgenticRunReconciler{EventRecorder: recorder}
	run := &agenticv1alpha1.AgenticRun{ObjectMeta: metav1.ObjectMeta{Name: "fix-crash", Namespace: "default"}}

	before := testutil.ToFloat64(tokenUsageMissingTotal.WithLabelValues("outputTokens"))
	reportStepUsage(r, run, "analysis", agenticv1alpha1.TokenUsage{InputTokens: ptr.To[int64](100)}, true)

	if got := testutil.ToFloat64(tokenUsageMissingTotal.WithLabelValues("outputTokens")) - before; got != 1 {
		t.Errorf("outputTokens missing counter delta = %v, want 1", got)
	}

	select {
	case event := <-recorder.Events:
		t.Fatalf("token usage reporting must not create Kubernetes events, got %q", event)
	default:
	}

	c := meta.FindStatusCondition(run.Status.Conditions, agenticv1alpha1.AgenticRunConditionTokenUsageReported)
	if c == nil || c.Status != metav1.ConditionFalse || c.Reason != agenticv1alpha1.ReasonTokenUsageReportedIncomplete {
		t.Fatalf("condition = %+v, want False/%s", c, agenticv1alpha1.ReasonTokenUsageReportedIncomplete)
	}
}

// TestReportTokenUsage_NegativeConditionIsSticky verifies a run that once lost
// required data stays flagged: later steps cannot repair an already-summed total.
func TestReportTokenUsage_NegativeConditionIsSticky(t *testing.T) {
	r := &AgenticRunReconciler{EventRecorder: record.NewFakeRecorder(20)}
	run := &agenticv1alpha1.AgenticRun{}

	reportStepUsage(r, run, "analysis", agenticv1alpha1.TokenUsage{InputTokens: ptr.To[int64](100)}, true)
	reportStepUsage(r, run, "execution", fullTokenUsage(), true)

	c := meta.FindStatusCondition(run.Status.Conditions, agenticv1alpha1.AgenticRunConditionTokenUsageReported)
	if c == nil || c.Status != metav1.ConditionFalse {
		t.Fatalf("condition = %+v, want it to stay False", c)
	}
}

// TestReportTokenUsage_FailedStepWithNoUsageIsSilent covers the failure contract: a
// failed step that published no token usage is expected, not a defect. Nothing is
// recorded: no event, no counter, and no condition.
func TestReportTokenUsage_FailedStepWithNoUsageIsSilent(t *testing.T) {
	recorder := record.NewFakeRecorder(20)
	r := &AgenticRunReconciler{EventRecorder: recorder}
	run := &agenticv1alpha1.AgenticRun{}

	beforeIn := testutil.ToFloat64(tokenUsageMissingTotal.WithLabelValues("inputTokens"))
	beforeOut := testutil.ToFloat64(tokenUsageMissingTotal.WithLabelValues("outputTokens"))

	reportStepUsage(r, run, "verification", agenticv1alpha1.TokenUsage{}, false)

	select {
	case event := <-recorder.Events:
		t.Fatalf("a failed step without token usage must not warn, got %q", event)
	default:
	}
	if got := testutil.ToFloat64(tokenUsageMissingTotal.WithLabelValues("inputTokens")) - beforeIn; got != 0 {
		t.Errorf("inputTokens counter delta = %v, want 0 for a failed step", got)
	}
	if got := testutil.ToFloat64(tokenUsageMissingTotal.WithLabelValues("outputTokens")) - beforeOut; got != 0 {
		t.Errorf("outputTokens counter delta = %v, want 0 for a failed step", got)
	}
	if c := meta.FindStatusCondition(run.Status.Conditions, agenticv1alpha1.AgenticRunConditionTokenUsageReported); c != nil {
		t.Errorf("condition = %+v, want unset for a failed step without usage", c)
	}
}

// TestReportTokenUsage_FailedStepWithPartialUsageIsSilent verifies failures do not
// carry the completeness contract even when the sandbox reported something before
// dying: the usage is still aggregated, but nothing is flagged.
func TestReportTokenUsage_FailedStepWithPartialUsageIsSilent(t *testing.T) {
	recorder := record.NewFakeRecorder(20)
	r := &AgenticRunReconciler{EventRecorder: recorder}
	run := &agenticv1alpha1.AgenticRun{}

	beforeOut := testutil.ToFloat64(tokenUsageMissingTotal.WithLabelValues("outputTokens"))
	reportStepUsage(r, run, "execution", agenticv1alpha1.TokenUsage{InputTokens: ptr.To[int64](100)}, false)

	select {
	case event := <-recorder.Events:
		t.Fatalf("a failed step must not warn about token usage, got %q", event)
	default:
	}
	if got := testutil.ToFloat64(tokenUsageMissingTotal.WithLabelValues("outputTokens")) - beforeOut; got != 0 {
		t.Errorf("outputTokens counter delta = %v, want 0 for a failed step", got)
	}
	if c := meta.FindStatusCondition(run.Status.Conditions, agenticv1alpha1.AgenticRunConditionTokenUsageReported); c != nil {
		t.Errorf("condition = %+v, want unset for a failed step", c)
	}
	if run.Status.TokenUsage.InputTokens == nil || *run.Status.TokenUsage.InputTokens != 100 {
		t.Errorf("failed step usage must still be aggregated, inputTokens = %v", run.Status.TokenUsage.InputTokens)
	}
}

// sandboxChainPod builds a Pod owned by a Sandbox that is owned by a
// SandboxClaim, following the agents.x-k8s.io ownership chain the resolvers walk.
func sandboxChainPod(podLabels, podAnnotations, sandboxLabels, sandboxAnnotations, claimLabels, claimAnnotations map[string]string) []client.Object {
	sandbox := &unstructured.Unstructured{}
	sandbox.SetGroupVersionKind(smSandboxGVK)
	sandbox.SetName("sandbox-analysis")
	sandbox.SetNamespace("test-ns")
	sandbox.SetLabels(sandboxLabels)
	sandbox.SetAnnotations(sandboxAnnotations)

	claim := &unstructured.Unstructured{}
	claim.SetGroupVersionKind(smClaimGVK)
	claim.SetName("claim-analysis")
	claim.SetNamespace("test-ns")
	claim.SetLabels(claimLabels)
	claim.SetAnnotations(claimAnnotations)
	claim.SetCreationTimestamp(metav1.NewTime(time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)))

	sandbox.SetOwnerReferences([]metav1.OwnerReference{{
		APIVersion: smClaimGVK.GroupVersion().String(),
		Kind:       smClaimGVK.Kind,
		Name:       claim.GetName(),
	}})

	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name:            "sandbox-analysis-pod",
		Namespace:       "test-ns",
		Labels:          podLabels,
		Annotations:     podAnnotations,
		OwnerReferences: []metav1.OwnerReference{{APIVersion: smSandboxGVK.GroupVersion().String(), Kind: smSandboxGVK.Kind, Name: sandbox.GetName()}},
	}}
	return []client.Object{pod, sandbox, claim}
}

// TestResolveSandboxPodMetadata pins which object wins when identity is spread
// across the Pod → Sandbox → SandboxClaim chain: the Pod's own copy first, then
// the Sandbox, then the Claim.
func TestResolveSandboxPodMetadata(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name     string
		objects  []client.Object
		wantStep string
		wantName string
		wantUID  types.UID
		wantErr  bool
	}{
		{
			name: "pod copy wins over the chain",
			objects: sandboxChainPod(
				map[string]string{LabelStep: "analysis", LabelRun: "run-uid"},
				map[string]string{AnnotationRunName: "fix-crash"},
				map[string]string{LabelStep: "execution", LabelRun: "other-uid"},
				map[string]string{AnnotationRunName: "other-run"},
				map[string]string{LabelStep: "verification", LabelRun: "claim-uid"},
				map[string]string{AnnotationRunName: "claim-run"},
			),
			wantStep: "analysis",
			wantName: "fix-crash",
			wantUID:  "run-uid",
		},
		{
			name: "sandbox supplies what the pod lacks",
			objects: sandboxChainPod(
				nil, nil,
				map[string]string{LabelStep: "analysis", LabelRun: "run-uid"},
				map[string]string{AnnotationRunName: "fix-crash"},
				map[string]string{LabelStep: "verification", LabelRun: "claim-uid"},
				map[string]string{AnnotationRunName: "claim-run"},
			),
			wantStep: "analysis",
			wantName: "fix-crash",
			wantUID:  "run-uid",
		},
		{
			name: "claim supplies what the sandbox lacks",
			objects: sandboxChainPod(
				nil, nil, nil, nil,
				map[string]string{LabelStep: "analysis", LabelRun: "claim-uid"},
				map[string]string{AnnotationRunName: "claim-run"},
			),
			wantStep: "analysis",
			wantName: "claim-run",
			wantUID:  "claim-uid",
		},
		{
			name: "pod without a sandbox owner",
			objects: []client.Object{&corev1.Pod{ObjectMeta: metav1.ObjectMeta{
				Name: "orphan", Namespace: "test-ns",
			}}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := fake.NewClientBuilder().WithObjects(tt.objects...).Build()
			pod := &corev1.Pod{}
			for _, obj := range tt.objects {
				if p, ok := obj.(*corev1.Pod); ok {
					pod = p
				}
			}

			step, runName, runUID, err := resolveSandboxPodMetadata(ctx, c, pod)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected an error")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if step != tt.wantStep || runName != tt.wantName || runUID != tt.wantUID {
				t.Errorf("got step=%q runName=%q runUID=%q, want step=%q runName=%q runUID=%q",
					step, runName, runUID, tt.wantStep, tt.wantName, tt.wantUID)
			}
		})
	}

	t.Run("sandbox fetch failure surfaces", func(t *testing.T) {
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
			Name: "sandbox-analysis-pod", Namespace: "test-ns",
			OwnerReferences: []metav1.OwnerReference{{Kind: smSandboxGVK.Kind, Name: "gone-sandbox"}},
		}}
		c := fake.NewClientBuilder().WithObjects(pod).Build()
		if _, _, _, err := resolveSandboxPodMetadata(ctx, c, pod); err == nil {
			t.Fatal("expected an error when the Sandbox is missing")
		}
	})
}

// TestSandboxClaimCreationTime pins the startup clock used in claim mode: the
// claim's timestamp, not the pod's.
func TestSandboxClaimCreationTime(t *testing.T) {
	ctx := context.Background()

	t.Run("claim timestamp", func(t *testing.T) {
		objects := sandboxChainPod(nil, nil, nil, nil, nil, nil)
		c := fake.NewClientBuilder().WithObjects(objects...).Build()
		pod := objects[0].(*corev1.Pod)

		got, err := sandboxClaimCreationTime(ctx, c, pod)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
		if !got.Equal(want) {
			t.Errorf("claim creation time = %s, want %s", got, want)
		}
	})

	t.Run("no claim owner yields zero", func(t *testing.T) {
		sandbox := &unstructured.Unstructured{}
		sandbox.SetGroupVersionKind(smSandboxGVK)
		sandbox.SetName("sandbox-analysis")
		sandbox.SetNamespace("test-ns")
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
			Name: "pod", Namespace: "test-ns",
			OwnerReferences: []metav1.OwnerReference{{Kind: smSandboxGVK.Kind, Name: sandbox.GetName()}},
		}}
		c := fake.NewClientBuilder().WithObjects(pod, sandbox).Build()

		got, err := sandboxClaimCreationTime(ctx, c, pod)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !got.IsZero() {
			t.Errorf("claim creation time = %s, want zero", got)
		}
	})

	t.Run("sandbox fetch failure surfaces", func(t *testing.T) {
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
			Name: "pod", Namespace: "test-ns",
			OwnerReferences: []metav1.OwnerReference{{Kind: smSandboxGVK.Kind, Name: "gone-sandbox"}},
		}}
		c := fake.NewClientBuilder().WithObjects(pod).Build()
		if _, err := sandboxClaimCreationTime(ctx, c, pod); err == nil {
			t.Fatal("expected an error when the Sandbox is missing")
		}
	})
}
