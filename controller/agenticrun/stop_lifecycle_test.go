package agenticrun

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
)

func newStopLifecycleReconciler(t *testing.T, run *agenticv1alpha1.AgenticRun, agent *testAgentCaller, extra ...client.Object) (*AgenticRunReconciler, client.Client) {
	t.Helper()
	objects := append([]client.Object{run}, extra...)
	fc := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(objects...).
		WithStatusSubresource(&agenticv1alpha1.AgenticRun{}).Build()
	return &AgenticRunReconciler{Client: fc, Agent: agent, Namespace: run.Namespace}, fc
}

func newSuspensionReconciler(t *testing.T, run *agenticv1alpha1.AgenticRun, agent *testAgentCaller, extra ...client.Object) (*AgenticRunReconciler, client.Client) {
	t.Helper()
	suspended := &agenticv1alpha1.AgenticOLSConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster"},
		Spec:       agenticv1alpha1.AgenticOLSConfigSpec{Suspended: true},
	}
	objects := append([]client.Object{run, suspended}, extra...)
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

func TestSuspensionStopsRunAndRetriesUntilConfirmed(t *testing.T) {
	run := testAgenticRun()
	run.UID = types.UID("run-uid")
	run.Status.Conditions = []metav1.Condition{
		stopLifecycleCondition(run, agenticv1alpha1.AgenticRunConditionAnalyzed, metav1.ConditionTrue, reasonComplete, "analysis complete"),
		stopLifecycleCondition(run, agenticv1alpha1.AgenticRunConditionExecuted, metav1.ConditionUnknown, reasonInProgress, "execution running"),
	}
	run.Status.Steps.Execution.Sandbox = agenticv1alpha1.SandboxInfo{ClaimName: "execution-claim", Namespace: "default"}

	confirmed := false
	agent := newTestAgentCaller()
	agent.stopSandboxesConfirmed = &confirmed
	r, fc := newSuspensionReconciler(t, run, agent)
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
		t.Fatal("outcome was recorded before teardown completed")
	}
	if got.Status.Steps.Execution.Sandbox.ClaimName != "execution-claim" {
		t.Fatal("sandbox reference cleared before teardown confirmed")
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
	if got.Status.Steps.Execution.Sandbox != (agenticv1alpha1.SandboxInfo{}) {
		t.Fatalf("confirmed sandbox reference not cleared: %+v", got.Status.Steps.Execution.Sandbox)
	}
	if agent.stopSandboxesCalls != 2 {
		t.Fatalf("StopSandboxes calls = %d, want 2", agent.stopSandboxesCalls)
	}
}

func TestSuspensionPendingRunIsStopped(t *testing.T) {
	run := testAgenticRun()
	run.UID = types.UID("run-uid")
	agent := newTestAgentCaller()
	r, fc := newSuspensionReconciler(t, run, agent)

	if _, err := r.Reconcile(context.Background(), stopLifecycleRequest(run)); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	got := getStopLifecycleRun(t, fc, run.Name)
	if phase := agenticv1alpha1.DerivePhase(got.Status.Conditions); phase != agenticv1alpha1.AgenticRunPhaseEmergencyStopped {
		t.Fatalf("phase = %s, want EmergencyStopped", phase)
	}
	assertStopLifecycleCondition(t, got, agenticv1alpha1.AgenticRunConditionStopping, metav1.ConditionFalse, reasonTeardownComplete, messageTeardownComplete)
	assertStopLifecycleCondition(t, got, agenticv1alpha1.AgenticRunConditionEmergencyStopped, metav1.ConditionTrue, reasonSystemSuspended, "Terminated by system kill switch (AgenticOLSConfig.spec.suspended=true)")
}

func TestSuspensionStopErrorKeepsStopping(t *testing.T) {
	run := testAgenticRun()
	run.UID = types.UID("run-uid")
	run.Status.Conditions = []metav1.Condition{
		stopLifecycleCondition(run, agenticv1alpha1.AgenticRunConditionAnalyzed, metav1.ConditionTrue, reasonComplete, "analysis complete"),
		stopLifecycleCondition(run, agenticv1alpha1.AgenticRunConditionExecuted, metav1.ConditionUnknown, reasonInProgress, "execution running"),
	}
	agent := newTestAgentCaller()
	agent.stopSandboxesErr = errors.New("cleanup incomplete")
	r, fc := newSuspensionReconciler(t, run, agent)

	if _, err := r.Reconcile(context.Background(), stopLifecycleRequest(run)); err == nil {
		t.Fatal("expected teardown error")
	}
	got := getStopLifecycleRun(t, fc, run.Name)
	assertStopLifecycleCondition(t, got, agenticv1alpha1.AgenticRunConditionStopping, metav1.ConditionTrue, reasonSystemSuspended, messageStopping)
	if phase := agenticv1alpha1.DerivePhase(got.Status.Conditions); phase == agenticv1alpha1.AgenticRunPhaseEmergencyStopped {
		t.Fatal("stopped outcome recorded despite teardown error")
	}
}

func TestStoppedOutcomeBlocksRevisionAndLateResult(t *testing.T) {
	run := testAgenticRun()
	run.UID = types.UID("run-uid")
	run.Generation = 2
	run.Spec.Execution = agenticv1alpha1.AgenticRunStep{}
	run.Spec.RevisionFeedback = "retry this run"
	now := metav1.NewTime(time.Now())
	deadline := metav1.NewTime(now.Add(24 * time.Hour))
	run.Labels = map[string]string{terminalTTLLabel: "true"}
	run.Status.TerminalTime = &now
	run.Status.DeleteAfter = &deadline
	run.Status.Conditions = []metav1.Condition{
		stopLifecycleCondition(run, agenticv1alpha1.AgenticRunConditionEmergencyStopped, metav1.ConditionTrue, reasonSystemSuspended, "Terminated by system kill switch (AgenticOLSConfig.spec.suspended=true)"),
		stopLifecycleCondition(run, agenticv1alpha1.AgenticRunConditionStopping, metav1.ConditionFalse, reasonTeardownComplete, messageTeardownComplete),
	}
	run.Status.Conditions[0].ObservedGeneration = 1
	run.Status.Steps.Analysis.Sandbox = agenticv1alpha1.SandboxInfo{ClaimName: "late-analysis-claim", Namespace: "default"}

	stale := run.DeepCopy()
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
	assertStopLifecycleCondition(t, got, agenticv1alpha1.AgenticRunConditionEmergencyStopped, metav1.ConditionTrue, reasonSystemSuspended, "Terminated by system kill switch (AgenticOLSConfig.spec.suspended=true)")
	if len(got.Status.Steps.Analysis.Results) != 0 {
		t.Fatalf("late Result CR ref was appended: %+v", got.Status.Steps.Analysis.Results)
	}
}

func TestAsyncCompletionCannotWriteAfterStoppingIsPersisted(t *testing.T) {
	run := testAgenticRun()
	run.UID = types.UID("run-uid")
	run.Status.Conditions = []metav1.Condition{
		stopLifecycleCondition(run, agenticv1alpha1.AgenticRunConditionAnalyzed, metav1.ConditionTrue, reasonComplete, "analysis complete"),
		stopLifecycleCondition(run, agenticv1alpha1.AgenticRunConditionExecuted, metav1.ConditionUnknown, reasonInProgress, "execution running"),
		stopLifecycleCondition(run, agenticv1alpha1.AgenticRunConditionStopping, metav1.ConditionTrue, reasonSystemSuspended, messageStopping),
	}
	run.Status.Steps.Execution.Sandbox = agenticv1alpha1.SandboxInfo{ClaimName: "execution-claim", Namespace: "default"}
	stale := run.DeepCopy()
	stale.Status.Conditions = []metav1.Condition{
		stopLifecycleCondition(stale, agenticv1alpha1.AgenticRunConditionAnalyzed, metav1.ConditionTrue, reasonComplete, "analysis complete"),
		stopLifecycleCondition(stale, agenticv1alpha1.AgenticRunConditionExecuted, metav1.ConditionUnknown, reasonInProgress, "execution running"),
	}

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
	now := metav1.NewTime(time.Now())
	run.Labels = map[string]string{terminalTTLLabel: "true"}
	run.Status.TerminalTime = &now
	run.Status.DeleteAfter = &metav1.Time{Time: now.Add(24 * time.Hour)}
	run.Status.Conditions = []metav1.Condition{
		stopLifecycleCondition(run, agenticv1alpha1.AgenticRunConditionEmergencyStopped, metav1.ConditionTrue, reasonSystemSuspended, "Terminated by system kill switch (AgenticOLSConfig.spec.suspended=true)"),
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
	if _, err := r.Reconcile(context.Background(), requests[0]); err != nil {
		t.Fatalf("reconcile queued by late Pod: %v", err)
	}
	got := getStopLifecycleRun(t, fc, run.Name)
	if got.Status.Steps.Analysis.Sandbox != (agenticv1alpha1.SandboxInfo{}) || agent.stopSandboxesCalls != 1 {
		t.Fatalf("late Pod cleanup refs=%+v stopCalls=%d", got.Status.Steps.Analysis.Sandbox, agent.stopSandboxesCalls)
	}
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
