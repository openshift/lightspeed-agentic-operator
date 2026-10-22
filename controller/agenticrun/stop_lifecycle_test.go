package agenticrun

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func newStopLifecycleReconciler(t *testing.T, run *agenticv1alpha1.AgenticRun, agent *testAgentCaller, extra ...client.Object) (*AgenticRunReconciler, client.Client) {
	t.Helper()
	objects := append([]client.Object{run}, extra...)
	fc := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(objects...).
		WithStatusSubresource(&agenticv1alpha1.AgenticRun{}).Build()
	return &AgenticRunReconciler{Client: fc, Agent: agent, Namespace: run.Namespace}, fc
}

func stopLifecycleCondition(run *agenticv1alpha1.AgenticRun, conditionType string, status metav1.ConditionStatus, reason, message string) metav1.Condition {
	return metav1.Condition{
		Type:               conditionType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: run.Generation,
	}
}

func findStopLifecycleCondition(conditions []metav1.Condition, conditionType string) *metav1.Condition {
	for i := range conditions {
		if conditions[i].Type == conditionType {
			return &conditions[i]
		}
	}
	return nil
}

func getStopLifecycleRun(t *testing.T, c client.Client, name string) *agenticv1alpha1.AgenticRun {
	t.Helper()
	var run agenticv1alpha1.AgenticRun
	if err := c.Get(context.Background(), client.ObjectKey{Name: name, Namespace: "default"}, &run); err != nil {
		t.Fatalf("get AgenticRun: %v", err)
	}
	return &run
}

func stopLifecycleRequest(run *agenticv1alpha1.AgenticRun) ctrl.Request {
	return ctrl.Request{NamespacedName: client.ObjectKeyFromObject(run)}
}

func hasFinalizer(finalizers []string, wanted string) bool {
	for _, finalizer := range finalizers {
		if finalizer == wanted {
			return true
		}
	}
	return false
}

func assertStopLifecycleCondition(t *testing.T, run *agenticv1alpha1.AgenticRun, conditionType string, status metav1.ConditionStatus, reason, message string) {
	t.Helper()
	condition := findStopLifecycleCondition(run.Status.Conditions, conditionType)
	if condition == nil {
		t.Fatalf("condition %q not found in %+v", conditionType, run.Status.Conditions)
	}
	if condition.Status != status || condition.Reason != reason || condition.Message != message {
		t.Fatalf("condition %q = status %q reason %q message %q, want %q/%q/%q", conditionType, condition.Status, condition.Reason, condition.Message, status, reason, message)
	}
}

func TestReconcileCancellationPhaseMapping(t *testing.T) {
	tests := []struct {
		name       string
		phase      agenticv1alpha1.AgenticRunPhase
		conditions func(*agenticv1alpha1.AgenticRun) []metav1.Condition
		wantType   string
	}{
		{
			name:     "Pending maps to Analyzed",
			phase:    agenticv1alpha1.AgenticRunPhasePending,
			wantType: agenticv1alpha1.AgenticRunConditionAnalyzed,
		},
		{
			name:  "Analyzing maps to Analyzed",
			phase: agenticv1alpha1.AgenticRunPhaseAnalyzing,
			conditions: func(run *agenticv1alpha1.AgenticRun) []metav1.Condition {
				return []metav1.Condition{stopLifecycleCondition(run, agenticv1alpha1.AgenticRunConditionAnalyzed, metav1.ConditionUnknown, reasonInProgress, "analysis running")}
			},
			wantType: agenticv1alpha1.AgenticRunConditionAnalyzed,
		},
		{
			name:  "Proposed maps to Executed",
			phase: agenticv1alpha1.AgenticRunPhaseProposed,
			conditions: func(run *agenticv1alpha1.AgenticRun) []metav1.Condition {
				return []metav1.Condition{stopLifecycleCondition(run, agenticv1alpha1.AgenticRunConditionAnalyzed, metav1.ConditionTrue, reasonComplete, "analysis complete")}
			},
			wantType: agenticv1alpha1.AgenticRunConditionExecuted,
		},
		{
			name:  "Executing maps to Executed",
			phase: agenticv1alpha1.AgenticRunPhaseExecuting,
			conditions: func(run *agenticv1alpha1.AgenticRun) []metav1.Condition {
				return []metav1.Condition{
					stopLifecycleCondition(run, agenticv1alpha1.AgenticRunConditionAnalyzed, metav1.ConditionTrue, reasonComplete, "analysis complete"),
					stopLifecycleCondition(run, agenticv1alpha1.AgenticRunConditionExecuted, metav1.ConditionUnknown, reasonInProgress, "execution running"),
				}
			},
			wantType: agenticv1alpha1.AgenticRunConditionExecuted,
		},
		{
			name:  "Verifying maps to Verified",
			phase: agenticv1alpha1.AgenticRunPhaseVerifying,
			conditions: func(run *agenticv1alpha1.AgenticRun) []metav1.Condition {
				return []metav1.Condition{
					stopLifecycleCondition(run, agenticv1alpha1.AgenticRunConditionAnalyzed, metav1.ConditionTrue, reasonComplete, "analysis complete"),
					stopLifecycleCondition(run, agenticv1alpha1.AgenticRunConditionExecuted, metav1.ConditionTrue, reasonComplete, "execution complete"),
					stopLifecycleCondition(run, agenticv1alpha1.AgenticRunConditionVerified, metav1.ConditionUnknown, reasonInProgress, "verification running"),
				}
			},
			wantType: agenticv1alpha1.AgenticRunConditionVerified,
		},
		{
			name:  "Escalating maps to Escalated",
			phase: agenticv1alpha1.AgenticRunPhaseEscalating,
			conditions: func(run *agenticv1alpha1.AgenticRun) []metav1.Condition {
				return []metav1.Condition{
					stopLifecycleCondition(run, agenticv1alpha1.AgenticRunConditionAnalyzed, metav1.ConditionTrue, reasonComplete, "analysis complete"),
					stopLifecycleCondition(run, agenticv1alpha1.AgenticRunConditionExecuted, metav1.ConditionTrue, reasonComplete, "execution complete"),
					stopLifecycleCondition(run, agenticv1alpha1.AgenticRunConditionVerified, metav1.ConditionFalse, agenticv1alpha1.ReasonVerificationFailed, "verification failed"),
					stopLifecycleCondition(run, agenticv1alpha1.AgenticRunConditionEscalated, metav1.ConditionUnknown, reasonInProgress, "escalation running"),
				}
			},
			wantType: agenticv1alpha1.AgenticRunConditionEscalated,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			run := testAgenticRun()
			run.Generation = 1
			run.Spec.Cancelled = true
			if tt.conditions != nil {
				run.Status.Conditions = tt.conditions(run)
			}
			if phase := agenticv1alpha1.DerivePhase(run.Status.Conditions); phase != tt.phase {
				t.Fatalf("fixture phase = %s, want %s", phase, tt.phase)
			}

			agent := newTestAgentCaller()
			r, fc := newStopLifecycleReconciler(t, run, agent)
			result, err := r.Reconcile(context.Background(), stopLifecycleRequest(run))
			if err != nil {
				t.Fatalf("Reconcile: %v", err)
			}
			if result.Requeue || result.RequeueAfter != 0 {
				t.Fatalf("confirmed stop unexpectedly requeued: %+v", result)
			}

			got := getStopLifecycleRun(t, fc, run.Name)
			if phase := agenticv1alpha1.DerivePhase(got.Status.Conditions); phase != agenticv1alpha1.AgenticRunPhaseFailed {
				t.Fatalf("phase = %s, want Failed", phase)
			}
			assertStopLifecycleCondition(t, got, tt.wantType, metav1.ConditionFalse, reasonCancelledByUser, "Run stopped by user")
			assertStopLifecycleCondition(t, got, agenticv1alpha1.AgenticRunConditionStopping, metav1.ConditionFalse, reasonTeardownComplete, messageTeardownComplete)
			if agent.stopSandboxesCalls != 1 {
				t.Fatalf("StopSandboxes called %d times, want 1", agent.stopSandboxesCalls)
			}
		})
	}
}

func TestCancellationRetainsReferencesUntilConfirmedAndRetries(t *testing.T) {
	run := testAgenticRun()
	run.UID = types.UID("run-uid")
	run.Spec.Cancelled = true
	run.Status.Conditions = []metav1.Condition{
		stopLifecycleCondition(run, agenticv1alpha1.AgenticRunConditionAnalyzed, metav1.ConditionTrue, reasonComplete, "analysis complete"),
		stopLifecycleCondition(run, agenticv1alpha1.AgenticRunConditionExecuted, metav1.ConditionUnknown, reasonInProgress, "execution running"),
	}
	analysisRef := agenticv1alpha1.SandboxInfo{ClaimName: "analysis-claim", Namespace: "default"}
	executionRef := agenticv1alpha1.SandboxInfo{ClaimName: "execution-claim", Namespace: "default"}
	verificationRef := agenticv1alpha1.SandboxInfo{ClaimName: "verification-claim", Namespace: "default"}
	escalationRef := agenticv1alpha1.SandboxInfo{ClaimName: "escalation-claim", Namespace: "default"}
	run.Status.Steps.Analysis.Sandbox = analysisRef
	run.Status.Steps.Execution.Sandbox = executionRef
	run.Status.Steps.Verification.Sandbox = verificationRef
	run.Status.Steps.Escalation.Sandbox = escalationRef

	confirmed := false
	agent := newTestAgentCaller()
	agent.stopSandboxesConfirmed = &confirmed
	r, fc := newStopLifecycleReconciler(t, run, agent)
	ctx := context.Background()

	result, err := r.Reconcile(ctx, stopLifecycleRequest(run))
	if err != nil {
		t.Fatalf("first Reconcile: %v", err)
	}
	if result.RequeueAfter != stopSandboxesRequeueAfter {
		t.Fatalf("RequeueAfter = %s, want %s", result.RequeueAfter, stopSandboxesRequeueAfter)
	}
	got := getStopLifecycleRun(t, fc, run.Name)
	assertStopLifecycleCondition(t, got, agenticv1alpha1.AgenticRunConditionStopping, metav1.ConditionTrue, reasonCancelledByUser, messageStopping)
	if c := findStopLifecycleCondition(got.Status.Conditions, agenticv1alpha1.AgenticRunConditionExecuted); c == nil || c.Status != metav1.ConditionUnknown {
		t.Fatalf("Executed changed before confirmed teardown: %+v", c)
	}
	if phase := agenticv1alpha1.DerivePhase(got.Status.Conditions); phase != agenticv1alpha1.AgenticRunPhaseExecuting {
		t.Fatalf("phase while teardown is pending = %s, want Executing", phase)
	}
	if got.Status.Steps.Analysis.Sandbox != analysisRef || got.Status.Steps.Execution.Sandbox != executionRef ||
		got.Status.Steps.Verification.Sandbox != verificationRef || got.Status.Steps.Escalation.Sandbox != escalationRef {
		t.Fatalf("sandbox refs cleared before confirmation: analysis=%+v execution=%+v verification=%+v escalation=%+v",
			got.Status.Steps.Analysis.Sandbox, got.Status.Steps.Execution.Sandbox, got.Status.Steps.Verification.Sandbox, got.Status.Steps.Escalation.Sandbox)
	}
	if !hasFinalizer(got.Finalizers, rbacCleanupFinalizer) || !hasFinalizer(got.Finalizers, templogCleanupFinalizer) {
		t.Fatalf("stop-pending run missing cleanup finalizers: %v", got.Finalizers)
	}

	agent.stopSandboxesErr = errors.New("cleanup incomplete")
	if _, err := r.Reconcile(ctx, stopLifecycleRequest(got)); err == nil {
		t.Fatal("expected teardown error")
	}
	got = getStopLifecycleRun(t, fc, run.Name)
	assertStopLifecycleCondition(t, got, agenticv1alpha1.AgenticRunConditionStopping, metav1.ConditionTrue, reasonCancelledByUser, messageStopping)
	if got.Status.Steps.Analysis.Sandbox != analysisRef || got.Status.Steps.Execution.Sandbox != executionRef ||
		got.Status.Steps.Verification.Sandbox != verificationRef || got.Status.Steps.Escalation.Sandbox != escalationRef {
		t.Fatalf("sandbox refs cleared after teardown error: analysis=%+v execution=%+v verification=%+v escalation=%+v",
			got.Status.Steps.Analysis.Sandbox, got.Status.Steps.Execution.Sandbox, got.Status.Steps.Verification.Sandbox, got.Status.Steps.Escalation.Sandbox)
	}
	if findStopLifecycleCondition(got.Status.Conditions, agenticv1alpha1.AgenticRunConditionExecuted).Status != metav1.ConditionUnknown {
		t.Fatal("cancellation outcome recorded after teardown error")
	}
	if phase := agenticv1alpha1.DerivePhase(got.Status.Conditions); phase != agenticv1alpha1.AgenticRunPhaseExecuting {
		t.Fatalf("phase after teardown error = %s, want Executing", phase)
	}

	agent.stopSandboxesErr = nil
	confirmed = true
	if _, err := r.Reconcile(ctx, stopLifecycleRequest(got)); err != nil {
		t.Fatalf("confirmed Reconcile: %v", err)
	}
	got = getStopLifecycleRun(t, fc, run.Name)
	assertStopLifecycleCondition(t, got, agenticv1alpha1.AgenticRunConditionStopping, metav1.ConditionFalse, reasonTeardownComplete, messageTeardownComplete)
	assertStopLifecycleCondition(t, got, agenticv1alpha1.AgenticRunConditionExecuted, metav1.ConditionFalse, reasonCancelledByUser, "Run stopped by user")
	if got.Status.Steps.Analysis.Sandbox != (agenticv1alpha1.SandboxInfo{}) ||
		got.Status.Steps.Execution.Sandbox != (agenticv1alpha1.SandboxInfo{}) ||
		got.Status.Steps.Verification.Sandbox != (agenticv1alpha1.SandboxInfo{}) ||
		got.Status.Steps.Escalation.Sandbox != (agenticv1alpha1.SandboxInfo{}) {
		t.Fatalf("sandbox refs remain after confirmed teardown: analysis=%+v execution=%+v verification=%+v escalation=%+v",
			got.Status.Steps.Analysis.Sandbox, got.Status.Steps.Execution.Sandbox, got.Status.Steps.Verification.Sandbox, got.Status.Steps.Escalation.Sandbox)
	}
	if agent.stopSandboxesCalls != 3 {
		t.Fatalf("StopSandboxes called %d times, want 3", agent.stopSandboxesCalls)
	}
	if agent.releaseAllCount != 0 || len(agent.releasedSteps) != 0 {
		t.Fatalf("stop path used Release fallback: releaseAll=%d releasedSteps=%v", agent.releaseAllCount, agent.releasedSteps)
	}
}

func TestSuspensionPrecedesCancellation(t *testing.T) {
	run := testAgenticRun()
	run.UID = types.UID("run-uid")
	run.Spec.Cancelled = true
	run.Status.Conditions = []metav1.Condition{
		stopLifecycleCondition(run, agenticv1alpha1.AgenticRunConditionAnalyzed, metav1.ConditionTrue, reasonComplete, "analysis complete"),
		stopLifecycleCondition(run, agenticv1alpha1.AgenticRunConditionExecuted, metav1.ConditionUnknown, reasonInProgress, "execution running"),
	}
	suspended := &agenticv1alpha1.AgenticOLSConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster"},
		Spec:       agenticv1alpha1.AgenticOLSConfigSpec{Suspended: true},
	}
	confirmed := false
	agent := newTestAgentCaller()
	agent.stopSandboxesConfirmed = &confirmed
	r, fc := newStopLifecycleReconciler(t, run, agent, suspended)
	ctx := context.Background()

	result, err := r.Reconcile(ctx, stopLifecycleRequest(run))
	if err != nil {
		t.Fatalf("pending Reconcile: %v", err)
	}
	if result.RequeueAfter != stopSandboxesRequeueAfter {
		t.Fatalf("RequeueAfter = %s, want %s", result.RequeueAfter, stopSandboxesRequeueAfter)
	}
	got := getStopLifecycleRun(t, fc, run.Name)
	assertStopLifecycleCondition(t, got, agenticv1alpha1.AgenticRunConditionStopping, metav1.ConditionTrue, reasonSystemSuspended, messageStopping)
	if findStopLifecycleCondition(got.Status.Conditions, agenticv1alpha1.AgenticRunConditionExecuted).Status != metav1.ConditionUnknown {
		t.Fatal("cancellation outcome was recorded before suspended teardown completed")
	}

	confirmed = true
	if _, err := r.Reconcile(ctx, stopLifecycleRequest(got)); err != nil {
		t.Fatalf("confirmed Reconcile: %v", err)
	}
	got = getStopLifecycleRun(t, fc, run.Name)
	if phase := agenticv1alpha1.DerivePhase(got.Status.Conditions); phase != agenticv1alpha1.AgenticRunPhaseEmergencyStopped {
		t.Fatalf("phase = %s, want EmergencyStopped", phase)
	}
	assertStopLifecycleCondition(t, got, agenticv1alpha1.AgenticRunConditionStopping, metav1.ConditionFalse, reasonTeardownComplete, messageTeardownComplete)
	assertStopLifecycleCondition(t, got, agenticv1alpha1.AgenticRunConditionEmergencyStopped, metav1.ConditionTrue, reasonSystemSuspended, "Terminated by system kill switch (AgenticOLSConfig.spec.suspended=true)")
	if c := findStopLifecycleCondition(got.Status.Conditions, agenticv1alpha1.AgenticRunConditionExecuted); c == nil || c.Status != metav1.ConditionUnknown {
		t.Fatalf("cancellation outcome overrode suspension: %+v", c)
	}
	for _, condition := range got.Status.Conditions {
		if condition.Reason == reasonCancelledByUser {
			t.Fatalf("cancellation condition written despite suspension precedence: %+v", condition)
		}
	}
}

func TestCancelledTerminalRunPreservesOutcomeAndStillCleans(t *testing.T) {
	tests := []struct {
		name     string
		preserve bool
		labeled  bool
	}{
		{name: "TTL-labeled terminal", labeled: true},
		{name: "preserved failed terminal", preserve: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			run := testAgenticRun()
			run.UID = types.UID("run-uid")
			run.Spec.Cancelled = true
			run.Status.Conditions = []metav1.Condition{
				stopLifecycleCondition(run, agenticv1alpha1.AgenticRunConditionAnalyzed, metav1.ConditionFalse, reasonWorkflowFailed, "preexisting terminal failure"),
			}
			run.Status.Steps.Analysis.Sandbox = agenticv1alpha1.SandboxInfo{ClaimName: "analysis-claim", Namespace: "default"}
			run.Status.Steps.Execution.Sandbox = agenticv1alpha1.SandboxInfo{ClaimName: "execution-claim", Namespace: "default"}
			if tt.labeled {
				now := metav1.NewTime(time.Now())
				deadline := metav1.NewTime(now.Add(24 * time.Hour))
				run.Labels = map[string]string{terminalTTLLabel: "true"}
				run.Status.TerminalTime = &now
				run.Status.DeleteAfter = &deadline
			}
			if tt.preserve {
				run.Annotations = map[string]string{preserveSandboxAnnotation: "true"}
			}
			analysisRef := run.Status.Steps.Analysis.Sandbox
			executionRef := run.Status.Steps.Execution.Sandbox

			confirmed := false
			agent := newTestAgentCaller()
			agent.stopSandboxesConfirmed = &confirmed
			r, fc := newStopLifecycleReconciler(t, run, agent)
			ctx := context.Background()
			result, err := r.Reconcile(ctx, stopLifecycleRequest(run))
			if err != nil {
				t.Fatalf("pending Reconcile: %v", err)
			}
			if result.RequeueAfter != stopSandboxesRequeueAfter {
				t.Fatalf("RequeueAfter = %s, want %s", result.RequeueAfter, stopSandboxesRequeueAfter)
			}
			got := getStopLifecycleRun(t, fc, run.Name)
			if got.Status.Steps.Analysis.Sandbox != analysisRef || got.Status.Steps.Execution.Sandbox != executionRef {
				t.Fatalf("terminal refs cleared before confirmation: analysis=%+v execution=%+v", got.Status.Steps.Analysis.Sandbox, got.Status.Steps.Execution.Sandbox)
			}
			assertStopLifecycleCondition(t, got, agenticv1alpha1.AgenticRunConditionAnalyzed, metav1.ConditionFalse, reasonWorkflowFailed, "preexisting terminal failure")

			confirmed = true
			if _, err := r.Reconcile(ctx, stopLifecycleRequest(got)); err != nil {
				t.Fatalf("confirmed Reconcile: %v", err)
			}
			got = getStopLifecycleRun(t, fc, run.Name)
			if phase := agenticv1alpha1.DerivePhase(got.Status.Conditions); phase != agenticv1alpha1.AgenticRunPhaseFailed {
				t.Fatalf("phase = %s, want preserved Failed", phase)
			}
			assertStopLifecycleCondition(t, got, agenticv1alpha1.AgenticRunConditionAnalyzed, metav1.ConditionFalse, reasonWorkflowFailed, "preexisting terminal failure")
			if findStopLifecycleCondition(got.Status.Conditions, agenticv1alpha1.AgenticRunConditionExecuted) != nil {
				t.Fatal("cancellation overwrote preexisting terminal history")
			}
			if got.Status.Steps.Analysis.Sandbox != (agenticv1alpha1.SandboxInfo{}) || got.Status.Steps.Execution.Sandbox != (agenticv1alpha1.SandboxInfo{}) {
				t.Fatalf("confirmed terminal refs not cleared: analysis=%+v execution=%+v", got.Status.Steps.Analysis.Sandbox, got.Status.Steps.Execution.Sandbox)
			}
			if tt.preserve && got.Status.TerminalTime != nil {
				t.Fatal("preserved failed run received a terminal timestamp")
			}
			if agent.stopSandboxesCalls != 2 || agent.releaseAllCount != 0 {
				t.Fatalf("terminal stop cleanup calls: StopSandboxes=%d ReleaseSandboxes=%d", agent.stopSandboxesCalls, agent.releaseAllCount)
			}
		})
	}
}

func TestStoppedOutcomeBlocksRevisionAndLateResult(t *testing.T) {
	run := testAgenticRun()
	run.UID = types.UID("run-uid")
	run.Generation = 2
	run.Spec.Cancelled = true
	run.Spec.Execution = agenticv1alpha1.AgenticRunStep{}
	run.Spec.RevisionFeedback = "retry this run"
	now := metav1.NewTime(time.Now())
	deadline := metav1.NewTime(now.Add(24 * time.Hour))
	run.Labels = map[string]string{terminalTTLLabel: "true"}
	run.Status.TerminalTime = &now
	run.Status.DeleteAfter = &deadline
	run.Status.Conditions = []metav1.Condition{
		stopLifecycleCondition(run, agenticv1alpha1.AgenticRunConditionAnalyzed, metav1.ConditionFalse, reasonCancelledByUser, "Run stopped by user"),
		stopLifecycleCondition(run, agenticv1alpha1.AgenticRunConditionStopping, metav1.ConditionFalse, reasonTeardownComplete, messageTeardownComplete),
	}
	run.Status.Conditions[0].ObservedGeneration = 1
	run.Status.Steps.Analysis.Sandbox = agenticv1alpha1.SandboxInfo{ClaimName: "late-analysis-claim", Namespace: "default"}

	stale := run.DeepCopy()
	stale.Spec.Cancelled = false
	stale.Status.Conditions = []metav1.Condition{
		stopLifecycleCondition(stale, agenticv1alpha1.AgenticRunConditionAnalyzed, metav1.ConditionUnknown, reasonInProgress, "analysis running"),
	}
	agent := newTestAgentCaller()
	r, fc := newStopLifecycleReconciler(t, run, agent, defaultObjects()...)
	r.Config = ttlTestCache(t, "14")
	ctx := context.Background()

	lateAnalysis := &agenticv1alpha1.AnalysisResult{}
	err := r.patchStepResult(ctx, stale, "analysis", agenticv1alpha1.AgenticRunConditionAnalyzed, "late-analysis-result", lateAnalysis, metav1.ConditionTrue, reasonComplete, "late result")
	if err != errAsyncUpdateSuperseded {
		t.Fatalf("late result patch error = %v, want superseded sentinel", err)
	}
	got := getStopLifecycleRun(t, fc, run.Name)
	assertStopLifecycleCondition(t, got, agenticv1alpha1.AgenticRunConditionAnalyzed, metav1.ConditionFalse, reasonCancelledByUser, "Run stopped by user")
	if len(got.Status.Steps.Analysis.Results) != 0 {
		t.Fatalf("late Result CR ref was appended: %+v", got.Status.Steps.Analysis.Results)
	}

	lateResultEvent := &agenticv1alpha1.AnalysisResult{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "late-analysis-result",
			Namespace:       run.Namespace,
			Labels:          resultLabels(string(run.UID), "analysis"),
			OwnerReferences: []metav1.OwnerReference{agenticRunOwnerRef(run)},
		},
	}
	if err := r.Create(ctx, lateResultEvent); err != nil {
		t.Fatalf("create late Result CR: %v", err)
	}
	if _, err := r.Reconcile(ctx, stopLifecycleRequest(run)); err != nil {
		t.Fatalf("reconcile after owned Result event: %v", err)
	}
	got = getStopLifecycleRun(t, fc, run.Name)
	assertStopLifecycleCondition(t, got, agenticv1alpha1.AgenticRunConditionAnalyzed, metav1.ConditionFalse, reasonCancelledByUser, "Run stopped by user")
	if len(got.Status.Steps.Analysis.Results) != 0 || agent.stopSandboxesCalls != 1 {
		t.Fatalf("late Result event changed stopped run: results=%+v stopCalls=%d", got.Status.Steps.Analysis.Results, agent.stopSandboxesCalls)
	}
	if got.Status.Steps.Analysis.Sandbox != (agenticv1alpha1.SandboxInfo{}) {
		t.Fatalf("late-resource cleanup did not clear confirmed sandbox ref: %+v", got.Status.Steps.Analysis.Sandbox)
	}
}

func TestAsyncCompletionCannotWriteAfterCancellationIsPersisted(t *testing.T) {
	run := testAgenticRun()
	run.UID = types.UID("run-uid")
	run.Spec.Cancelled = true
	run.Status.Conditions = []metav1.Condition{
		stopLifecycleCondition(run, agenticv1alpha1.AgenticRunConditionAnalyzed, metav1.ConditionTrue, reasonComplete, "analysis complete"),
		stopLifecycleCondition(run, agenticv1alpha1.AgenticRunConditionExecuted, metav1.ConditionUnknown, reasonInProgress, "execution running"),
	}
	run.Status.Steps.Execution.Sandbox = agenticv1alpha1.SandboxInfo{ClaimName: "execution-claim", Namespace: "default"}
	stale := run.DeepCopy()
	stale.Spec.Cancelled = false

	agent := newTestAgentCaller()
	r, fc := newStopLifecycleReconciler(t, run, agent)
	pod := &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodFailed}}
	if err := r.completeStep(context.Background(), stale, pod, "execution", agenticv1alpha1.AgenticRunConditionExecuted, "", ""); err != nil {
		t.Fatalf("late completeStep: %v", err)
	}

	got := getStopLifecycleRun(t, fc, run.Name)
	if c := findStopLifecycleCondition(got.Status.Conditions, agenticv1alpha1.AgenticRunConditionExecuted); c == nil || c.Status != metav1.ConditionUnknown {
		t.Fatalf("late completion overwrote current execution status: %+v", c)
	}
	if got.Status.Steps.Execution.Sandbox.ClaimName != "execution-claim" || len(got.Status.Steps.Execution.Results) != 0 {
		t.Fatalf("late completion changed execution refs/results: %+v", got.Status.Steps.Execution)
	}
	if len(agent.releasedSteps) != 0 || agent.releaseAllCount != 0 {
		t.Fatalf("late completion released sandbox: releasedSteps=%v releaseAll=%d", agent.releasedSteps, agent.releaseAllCount)
	}
}
func TestLatePodEventRequeuesStoppedRun(t *testing.T) {
	run := testAgenticRun()
	run.UID = types.UID("run-uid")
	run.Spec.Cancelled = true
	now := metav1.NewTime(time.Now())
	run.Labels = map[string]string{terminalTTLLabel: "true"}
	run.Status.TerminalTime = &now
	run.Status.DeleteAfter = &metav1.Time{Time: now.Add(24 * time.Hour)}
	run.Status.Conditions = []metav1.Condition{
		stopLifecycleCondition(run, agenticv1alpha1.AgenticRunConditionAnalyzed, metav1.ConditionFalse, reasonCancelledByUser, "Run stopped by user"),
		stopLifecycleCondition(run, agenticv1alpha1.AgenticRunConditionStopping, metav1.ConditionFalse, reasonTeardownComplete, messageTeardownComplete),
	}
	run.Status.Steps.Analysis.Sandbox = agenticv1alpha1.SandboxInfo{ClaimName: "late-analysis-claim", Namespace: "default"}
	agent := newTestAgentCaller()
	r, fc := newStopLifecycleReconciler(t, run, agent)

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "late-analysis-pod",
			Namespace: run.Namespace,
			Labels:    map[string]string{LabelRun: string(run.UID), LabelStep: "analysis"},
			Annotations: map[string]string{
				AnnotationRunName: run.Name,
			},
		},
		Status: corev1.PodStatus{Phase: corev1.PodSucceeded},
	}
	requests := r.handlePodEvent(context.Background(), pod)
	if len(requests) != 1 || requests[0].NamespacedName != client.ObjectKeyFromObject(run) {
		t.Fatalf("late managed Pod requests = %v, want owning run", requests)
	}
	assertStopLifecycleCondition(t, getStopLifecycleRun(t, fc, run.Name), agenticv1alpha1.AgenticRunConditionAnalyzed, metav1.ConditionFalse, reasonCancelledByUser, "Run stopped by user")
	if _, err := r.Reconcile(context.Background(), requests[0]); err != nil {
		t.Fatalf("reconcile queued by late Pod: %v", err)
	}
	got := getStopLifecycleRun(t, fc, run.Name)
	assertStopLifecycleCondition(t, got, agenticv1alpha1.AgenticRunConditionAnalyzed, metav1.ConditionFalse, reasonCancelledByUser, "Run stopped by user")
	if got.Status.Steps.Analysis.Sandbox != (agenticv1alpha1.SandboxInfo{}) || agent.stopSandboxesCalls != 1 {
		t.Fatalf("late Pod cleanup refs=%+v stopCalls=%d", got.Status.Steps.Analysis.Sandbox, agent.stopSandboxesCalls)
	}
}

func TestManagedSandboxEventsEnqueueOwningRunSafely(t *testing.T) {
	run := testAgenticRun()
	run.UID = types.UID("run-uid")
	run.Status.Steps.Execution.Sandbox = agenticv1alpha1.SandboxInfo{ClaimName: "execution-claim", Namespace: "default"}
	baseClient := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(run).
		WithStatusSubresource(&agenticv1alpha1.AgenticRun{}).Build()

	claim := &unstructured.Unstructured{}
	claim.SetGroupVersionKind(smClaimGVK)
	claim.SetName("execution-claim")
	claim.SetNamespace(run.Namespace)
	claim.SetUID(types.UID("claim-uid"))
	claim.SetLabels(map[string]string{LabelRun: string(run.UID), LabelStep: "execution"})
	claim.SetAnnotations(map[string]string{AnnotationRunName: run.Name})
	claim.SetOwnerReferences([]metav1.OwnerReference{agenticRunOwnerRef(run)})
	r := &AgenticRunReconciler{Client: baseClient, Agent: newTestAgentCaller(), Namespace: run.Namespace}

	requests := r.handleManagedSandboxEvent(context.Background(), claim)
	if len(requests) != 1 || requests[0].NamespacedName != client.ObjectKeyFromObject(run) {
		t.Fatalf("SandboxClaim requests = %v, want owning run", requests)
	}

	staleClaim := claim.DeepCopy()
	staleClaim.SetLabels(map[string]string{LabelRun: "different-run-uid", LabelStep: "execution"})
	if requests := r.handleManagedSandboxEvent(context.Background(), staleClaim); len(requests) != 0 {
		t.Fatalf("stale SandboxClaim UID enqueued unrelated run: %v", requests)
	}

	sandbox := &unstructured.Unstructured{}
	sandbox.SetGroupVersionKind(smSandboxGVK)
	sandbox.SetName("execution-sandbox")
	sandbox.SetNamespace(run.Namespace)
	sandbox.SetOwnerReferences([]metav1.OwnerReference{{
		APIVersion: smClaimGVK.GroupVersion().String(),
		Kind:       smClaimGVK.Kind,
		Name:       claim.GetName(),
		UID:        claim.GetUID(),
	}})
	r.Client = &sandboxClaimLookupClient{Client: baseClient, claim: claim}
	requests = r.handleManagedSandboxEvent(context.Background(), sandbox)
	if len(requests) != 1 || requests[0].NamespacedName != client.ObjectKeyFromObject(run) {
		t.Fatalf("Sandbox without a Pod requests = %v, want owning run", requests)
	}

	r.Client = baseClient
	requests = r.handleManagedSandboxEvent(context.Background(), sandbox)
	if len(requests) != 1 || requests[0].NamespacedName != client.ObjectKeyFromObject(run) {
		t.Fatalf("Sandbox with missing Claim requests = %v, want run found from retained ref", requests)
	}
}

type sandboxClaimLookupClient struct {
	client.Client
	claim *unstructured.Unstructured
}

func (c *sandboxClaimLookupClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if claim, ok := obj.(*unstructured.Unstructured); ok && key.Name == c.claim.GetName() && key.Namespace == c.claim.GetNamespace() && claim.GetObjectKind().GroupVersionKind() == smClaimGVK {
		*claim = *c.claim.DeepCopy()
		return nil
	}
	return c.Client.Get(ctx, key, obj, opts...)
}

func TestRunStopLaunchSentinelDoesNotFailStep(t *testing.T) {
	run := testAgenticRun()
	agent := newTestAgentCaller()
	agent.analyzeErr = ErrRunStopAccepted
	objects := append([]client.Object{run}, defaultObjects()...)
	fc := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(objects...).
		WithStatusSubresource(run, &agenticv1alpha1.AnalysisResult{}).Build()
	r := &AgenticRunReconciler{Client: fc, Agent: agent, Namespace: run.Namespace}

	result, err := reconcileOnce(r, run.Name)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !result.Requeue {
		t.Fatalf("result = %+v, want requeue after stop sentinel", result)
	}
	got := getStopLifecycleRun(t, fc, run.Name)
	if c := findStopLifecycleCondition(got.Status.Conditions, agenticv1alpha1.AgenticRunConditionAnalyzed); c == nil || c.Status != metav1.ConditionUnknown {
		t.Fatalf("stop sentinel converted the step into failure: %+v", c)
	}
	if len(got.Status.Steps.Analysis.Results) != 0 {
		t.Fatalf("stop sentinel created a failure result: %+v", got.Status.Steps.Analysis.Results)
	}
	var results agenticv1alpha1.AnalysisResultList
	if err := fc.List(context.Background(), &results, client.InNamespace(run.Namespace)); err != nil {
		t.Fatalf("list AnalysisResults: %v", err)
	}
	if len(results.Items) != 0 {
		t.Fatalf("stop sentinel created %d AnalysisResults", len(results.Items))
	}
}

func TestStatusPatchUsesOptimisticResourceVersionAndDoesNotRetryConflict(t *testing.T) {
	baseClient := fake.NewClientBuilder().WithScheme(testScheme()).Build()
	wrapped := &staleStatusClient{Client: baseClient}
	r := &AgenticRunReconciler{Client: wrapped}
	run := testAgenticRun()
	run.ResourceVersion = "17"
	base := run.DeepCopy()
	run.Status.Conditions = []metav1.Condition{stopLifecycleCondition(run, agenticv1alpha1.AgenticRunConditionAnalyzed, metav1.ConditionFalse, reasonFailed, "stale update")}

	err := r.statusPatch(context.Background(), run, base)
	if !apierrors.IsConflict(err) {
		t.Fatalf("statusPatch error = %v, want conflict", err)
	}
	if wrapped.patchCalls != 1 {
		t.Fatalf("status patch attempted %d times, want one attempt", wrapped.patchCalls)
	}
	if !bytes.Contains(wrapped.patchData, []byte(`"resourceVersion":"17"`)) {
		t.Fatalf("status patch omitted optimistic resourceVersion: %s", wrapped.patchData)
	}
}

type staleStatusClient struct {
	client.Client
	patchCalls int
	patchData  []byte
}

func (c *staleStatusClient) Status() client.SubResourceWriter {
	return staleStatusWriter{SubResourceWriter: c.Client.Status(), client: c}
}

type staleStatusWriter struct {
	client.SubResourceWriter
	client *staleStatusClient
}

func (w staleStatusWriter) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
	w.client.patchCalls++
	w.client.patchData, _ = patch.Data(obj)
	return apierrors.NewConflict(schema.GroupResource{Group: agenticv1alpha1.GroupVersion.Group, Resource: "agenticruns"}, obj.GetName(), errors.New("stale resourceVersion"))
}

func TestPodStatusConflictEnqueuesRunForReevaluation(t *testing.T) {
	run := testAgenticRun()
	run.UID = types.UID("run-uid")
	run.Status.Conditions = []metav1.Condition{
		stopLifecycleCondition(run, agenticv1alpha1.AgenticRunConditionAnalyzed, metav1.ConditionUnknown, reasonInProgress, "analysis running"),
	}
	baseClient := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(run).
		WithStatusSubresource(&agenticv1alpha1.AgenticRun{}).Build()
	wrapped := &staleStatusClient{Client: baseClient}
	r := &AgenticRunReconciler{Client: wrapped, Agent: newTestAgentCaller(), Namespace: run.Namespace}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "analysis-pod",
			Namespace: run.Namespace,
			Labels:    map[string]string{LabelRun: string(run.UID), LabelStep: "analysis"},
			Annotations: map[string]string{
				AnnotationRunName: run.Name,
			},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}

	requests := r.handlePodEvent(context.Background(), pod)
	if len(requests) != 1 || requests[0].NamespacedName != client.ObjectKeyFromObject(run) {
		t.Fatalf("conflicted Pod status patch requests = %v, want owning run requeued", requests)
	}
	if wrapped.patchCalls != 1 {
		t.Fatalf("conflicted Pod status patch attempted %d times, want one attempt", wrapped.patchCalls)
	}
	got := getStopLifecycleRun(t, baseClient, run.Name)
	if c := findStopLifecycleCondition(got.Status.Conditions, agenticv1alpha1.AgenticRunConditionAnalyzed); c == nil || c.Status != metav1.ConditionUnknown || c.Reason != reasonInProgress {
		t.Fatalf("conflicting async patch changed current condition: %+v", c)
	}
}

func TestCancelledTerminalPhasesPreserveTheirExistingOutcome(t *testing.T) {
	tests := []struct {
		name       string
		phase      agenticv1alpha1.AgenticRunPhase
		conditions func(*agenticv1alpha1.AgenticRun) []metav1.Condition
	}{
		{
			name:  "Completed",
			phase: agenticv1alpha1.AgenticRunPhaseCompleted,
			conditions: func(run *agenticv1alpha1.AgenticRun) []metav1.Condition {
				return []metav1.Condition{
					stopLifecycleCondition(run, agenticv1alpha1.AgenticRunConditionAnalyzed, metav1.ConditionTrue, reasonComplete, "analysis complete"),
					stopLifecycleCondition(run, agenticv1alpha1.AgenticRunConditionExecuted, metav1.ConditionTrue, reasonComplete, "execution complete"),
					stopLifecycleCondition(run, agenticv1alpha1.AgenticRunConditionVerified, metav1.ConditionTrue, reasonComplete, "verification complete"),
				}
			},
		},
		{
			name:  "Denied",
			phase: agenticv1alpha1.AgenticRunPhaseDenied,
			conditions: func(run *agenticv1alpha1.AgenticRun) []metav1.Condition {
				return []metav1.Condition{
					stopLifecycleCondition(run, agenticv1alpha1.AgenticRunConditionAnalyzed, metav1.ConditionTrue, reasonComplete, "analysis complete"),
					stopLifecycleCondition(run, agenticv1alpha1.AgenticRunConditionDenied, metav1.ConditionTrue, reasonUserDenied, "denied before execution"),
				}
			},
		},
		{
			name:  "Escalated",
			phase: agenticv1alpha1.AgenticRunPhaseEscalated,
			conditions: func(run *agenticv1alpha1.AgenticRun) []metav1.Condition {
				return []metav1.Condition{
					stopLifecycleCondition(run, agenticv1alpha1.AgenticRunConditionAnalyzed, metav1.ConditionTrue, reasonComplete, "analysis complete"),
					stopLifecycleCondition(run, agenticv1alpha1.AgenticRunConditionExecuted, metav1.ConditionTrue, reasonComplete, "execution complete"),
					stopLifecycleCondition(run, agenticv1alpha1.AgenticRunConditionVerified, metav1.ConditionFalse, agenticv1alpha1.ReasonVerificationFailed, "verification failed"),
					stopLifecycleCondition(run, agenticv1alpha1.AgenticRunConditionEscalated, metav1.ConditionTrue, reasonComplete, "escalation complete"),
				}
			},
		},
		{
			name:  "Failed",
			phase: agenticv1alpha1.AgenticRunPhaseFailed,
			conditions: func(run *agenticv1alpha1.AgenticRun) []metav1.Condition {
				return []metav1.Condition{stopLifecycleCondition(run, agenticv1alpha1.AgenticRunConditionAnalyzed, metav1.ConditionFalse, reasonWorkflowFailed, "preexisting failure")}
			},
		},
		{
			name:  "EmergencyStopped",
			phase: agenticv1alpha1.AgenticRunPhaseEmergencyStopped,
			conditions: func(run *agenticv1alpha1.AgenticRun) []metav1.Condition {
				return []metav1.Condition{stopLifecycleCondition(run, agenticv1alpha1.AgenticRunConditionEmergencyStopped, metav1.ConditionTrue, reasonSystemSuspended, "preexisting emergency stop")}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			run := testAgenticRun()
			run.UID = types.UID("run-uid")
			run.Spec.Cancelled = true
			now := metav1.NewTime(time.Now())
			run.Status.TerminalTime = &now
			run.Status.DeleteAfter = &metav1.Time{Time: now.Add(24 * time.Hour)}
			run.Labels = map[string]string{terminalTTLLabel: "true"}
			run.Status.Conditions = tt.conditions(run)
			if phase := agenticv1alpha1.DerivePhase(run.Status.Conditions); phase != tt.phase {
				t.Fatalf("fixture phase = %s, want %s", phase, tt.phase)
			}
			wantConditions := append([]metav1.Condition(nil), run.Status.Conditions...)
			run.Status.Steps.Analysis.Sandbox = agenticv1alpha1.SandboxInfo{ClaimName: "late-analysis-claim", Namespace: "default"}

			agent := newTestAgentCaller()
			r, fc := newStopLifecycleReconciler(t, run, agent)
			if _, err := r.Reconcile(context.Background(), stopLifecycleRequest(run)); err != nil {
				t.Fatalf("Reconcile: %v", err)
			}
			got := getStopLifecycleRun(t, fc, run.Name)
			if phase := agenticv1alpha1.DerivePhase(got.Status.Conditions); phase != tt.phase {
				t.Fatalf("phase after cancellation = %s, want %s", phase, tt.phase)
			}
			if len(got.Status.Conditions) != len(wantConditions) {
				t.Fatalf("terminal conditions changed: got %+v, want %+v", got.Status.Conditions, wantConditions)
			}
			for _, want := range wantConditions {
				condition := findStopLifecycleCondition(got.Status.Conditions, want.Type)
				if condition == nil || condition.Status != want.Status || condition.Reason != want.Reason || condition.Message != want.Message {
					t.Fatalf("terminal condition %q changed: got %+v, want %+v", want.Type, condition, want)
				}
			}
			if got.Status.Steps.Analysis.Sandbox != (agenticv1alpha1.SandboxInfo{}) {
				t.Fatalf("confirmed terminal cleanup retained sandbox ref: %+v", got.Status.Steps.Analysis.Sandbox)
			}
			if agent.stopSandboxesCalls != 1 {
				t.Fatalf("StopSandboxes called %d times, want 1", agent.stopSandboxesCalls)
			}
		})
	}
}
