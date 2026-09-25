package agenticrun

import (
	"context"
	"testing"
	"time"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// makeTokenUsage is a helper to build a TokenUsage value concisely.
func makeTokenUsage(in, out int64) agenticv1alpha1.TokenUsage {
	return agenticv1alpha1.TokenUsage{InputTokens: ptr.To(in), OutputTokens: ptr.To(out)}
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

	r := &AgenticRunReconciler{Client: fc, Agent: agent.withClient(t, fc, "default"), Namespace: "default"}

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
			name: "init from zero — analysis",
			resultCR: &agenticv1alpha1.AnalysisResult{
				Status: agenticv1alpha1.AnalysisResultStatus{TokenUsage: makeTokenUsage(100, 50)},
			},
			wantInput:  100,
			wantOutput: 50,
		},
		{
			name: "sum — execution",
			resultCR: &agenticv1alpha1.ExecutionResult{
				Status: agenticv1alpha1.ExecutionResultStatus{TokenUsage: makeTokenUsage(200, 80)},
			},
			initial:    makeTokenUsage(100, 50),
			wantInput:  300,
			wantOutput: 130,
		},
		{
			name: "skip when absent — verification",
			resultCR: &agenticv1alpha1.VerificationResult{
				Status: agenticv1alpha1.VerificationResultStatus{},
			},
			wantZero: true,
		},
		{
			name: "skip when absent preserves existing — escalation",
			resultCR: &agenticv1alpha1.EscalationResult{
				Status: agenticv1alpha1.EscalationResultStatus{},
			},
			initial:    makeTokenUsage(100, 50),
			wantInput:  100,
			wantOutput: 50,
		},
		{
			name: "zero values treated as present",
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

			aggregateTokenUsage(run, tt.resultCR)

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
						Message: toolResultSafetyInspectionFailed,
					}},
				}},
			}},
			want: msgToolResultSafetyInspectionFailed,
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
