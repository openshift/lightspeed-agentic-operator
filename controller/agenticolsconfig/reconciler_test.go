package agenticolsconfig

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
	"github.com/openshift/lightspeed-agentic-operator/controller/agenticrun"
	"github.com/openshift/lightspeed-agentic-operator/pkg/configuration"
)

func testScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	utilruntime.Must(agenticv1alpha1.AddToScheme(s))
	utilruntime.Must(corev1.AddToScheme(s))
	utilruntime.Must(rbacv1.AddToScheme(s))
	return s
}

func testConfig(suspended bool) *agenticv1alpha1.AgenticOLSConfig {
	return &agenticv1alpha1.AgenticOLSConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster", UID: types.UID("config-uid"), Generation: 1},
		Spec:       agenticv1alpha1.AgenticOLSConfigSpec{Suspended: suspended},
	}
}

func emergencyStoppedAgenticRun(name string) *agenticv1alpha1.AgenticRun {
	return &agenticv1alpha1.AgenticRun{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "default", UID: types.UID(name + "-uid"), Generation: 1,
		},
		Spec: agenticv1alpha1.AgenticRunSpec{Request: "test"},
		Status: agenticv1alpha1.AgenticRunStatus{
			Conditions: []metav1.Condition{{
				Type:               agenticv1alpha1.AgenticRunConditionEmergencyStopped,
				Status:             metav1.ConditionTrue,
				Reason:             reasonSystemSuspended,
				ObservedGeneration: 1,
			}},
		},
	}
}

func pendingAgenticRun(name string) *agenticv1alpha1.AgenticRun {
	return &agenticv1alpha1.AgenticRun{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "default", UID: types.UID(name + "-uid"), Generation: 1,
		},
		Spec: agenticv1alpha1.AgenticRunSpec{Request: "test"},
	}
}

func completedAgenticRun(name string, target string) *agenticv1alpha1.AgenticRun {
	terminalTime := metav1.NewTime(time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC))
	deleteAfter := metav1.NewTime(time.Date(2026, 10, 26, 13, 0, 0, 0, time.UTC))
	return &agenticv1alpha1.AgenticRun{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "default", UID: types.UID(name + "-uid"), Generation: 1,
		},
		Spec: agenticv1alpha1.AgenticRunSpec{Request: "test", TargetCluster: target},
		Status: agenticv1alpha1.AgenticRunStatus{
			Conditions: []metav1.Condition{{
				Type:               agenticv1alpha1.AgenticRunConditionVerified,
				Status:             metav1.ConditionTrue,
				Reason:             "Verified",
				ObservedGeneration: 1,
			}},
			TerminalTime: &terminalTime,
			DeleteAfter:  &deleteAfter,
		},
	}
}

type testStatusClient struct {
	client.Client

	failRunStatusPatch            bool
	stallRunStatusPatch           bool
	stalledRunStatusPatch         bool
	runStatusPatchHadDeadline     bool
	runStatusPatchReachedDeadline bool
	advanceRunMarkerClock         func()
	conflictRunStatusPatch        bool
	conflictedRunStatus           bool
	naturalTerminalTime           time.Time
	requireLiveConfigStatus       bool
	sawCanceledConfigStatusPatch  bool
	holdFinalizedPods             bool
}

func (c *testStatusClient) Status() client.SubResourceWriter {
	return &testStatusWriter{SubResourceWriter: c.Client.Status(), owner: c}
}

func (c *testStatusClient) Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
	if c.holdFinalizedPods {
		if pod, ok := obj.(*corev1.Pod); ok && len(pod.Finalizers) != 0 {
			return nil
		}
	}
	return c.Client.Delete(ctx, obj, opts...)
}

type testStatusWriter struct {
	client.SubResourceWriter
	owner *testStatusClient
}

func (w *testStatusWriter) Patch(
	ctx context.Context,
	obj client.Object,
	patch client.Patch,
	opts ...client.SubResourcePatchOption,
) error {
	switch run := obj.(type) {
	case *agenticv1alpha1.AgenticRun:
		if w.owner.stallRunStatusPatch && !w.owner.stalledRunStatusPatch {
			w.owner.stalledRunStatusPatch = true
			if _, ok := ctx.Deadline(); !ok {
				return errors.New("run marker status patch has no bounded pass deadline")
			}
			w.owner.runStatusPatchHadDeadline = true
			if w.owner.advanceRunMarkerClock != nil {
				w.owner.advanceRunMarkerClock()
				w.owner.runStatusPatchReachedDeadline = true
			}
			return errors.New("simulated run marker write timeout at activation deadline")
		}
		if w.owner.conflictRunStatusPatch && !w.owner.conflictedRunStatus {
			w.owner.conflictedRunStatus = true
			current := &agenticv1alpha1.AgenticRun{}
			if err := w.owner.Client.Get(ctx, client.ObjectKeyFromObject(run), current); err != nil {
				return err
			}
			base := current.DeepCopy()
			if w.owner.naturalTerminalTime.IsZero() {
				return errors.New("natural terminal time is required for the conflict fixture")
			}
			terminalTime := metav1.NewTime(w.owner.naturalTerminalTime)
			current.Status.TerminalTime = &terminalTime
			deleteAfter := metav1.NewTime(w.owner.naturalTerminalTime.Add(24 * time.Hour))
			current.Status.DeleteAfter = &deleteAfter
			meta.SetStatusCondition(&current.Status.Conditions, metav1.Condition{
				Type:               agenticv1alpha1.AgenticRunConditionVerified,
				Status:             metav1.ConditionTrue,
				Reason:             "CompletedNaturally",
				ObservedGeneration: current.Generation,
			})
			if err := w.owner.Client.Status().Patch(ctx, current, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
				return err
			}
			return apierrors.NewConflict(
				schema.GroupResource{Group: "agentic.openshift.io", Resource: "agenticruns"},
				run.Name,
				errors.New("simulated natural-completion conflict"),
			)
		}
		if w.owner.failRunStatusPatch {
			return errors.New("simulated run status patch failure")
		}
	case *agenticv1alpha1.AgenticOLSConfig:
		if w.owner.requireLiveConfigStatus && ctx.Err() != nil {
			w.owner.sawCanceledConfigStatusPatch = true
			return ctx.Err()
		}
	}
	return w.SubResourceWriter.Patch(ctx, obj, patch, opts...)
}

type testAPIReader struct {
	client.Reader
	listHook           func(context.Context, client.ObjectList) (bool, error)
	getHook            func(context.Context, client.ObjectKey, client.Object) (bool, error)
	optionalListErrors map[schema.GroupVersionKind]error
	existingCRDs       map[string]bool
}

func testOptionalSandboxListErrors() map[schema.GroupVersionKind]error {
	claimList := schema.GroupVersionKind{
		Group: "extensions.agents.x-k8s.io", Version: "v1beta1", Kind: "SandboxClaimList",
	}
	sandboxList := schema.GroupVersionKind{
		Group: "agents.x-k8s.io", Version: "v1beta1", Kind: "SandboxList",
	}
	warmPoolList := schema.GroupVersionKind{
		Group: "extensions.agents.x-k8s.io", Version: "v1beta1", Kind: "SandboxWarmPoolList",
	}
	return map[schema.GroupVersionKind]error{
		claimList: &meta.NoKindMatchError{
			GroupKind:        schema.GroupKind{Group: claimList.Group, Kind: claimList.Kind},
			SearchedVersions: []string{claimList.Version},
		},
		sandboxList: &meta.NoResourceMatchError{
			PartialResource: schema.GroupVersionResource{
				Group: sandboxList.Group, Version: sandboxList.Version, Resource: "sandboxes",
			},
		},
		warmPoolList: &meta.NoKindMatchError{
			GroupKind:        schema.GroupKind{Group: warmPoolList.Group, Kind: warmPoolList.Kind},
			SearchedVersions: []string{warmPoolList.Version},
		},
	}
}

func (r *testAPIReader) Get(
	ctx context.Context,
	key client.ObjectKey,
	obj client.Object,
	opts ...client.GetOption,
) error {
	if r.getHook != nil {
		if handled, err := r.getHook(ctx, key, obj); handled {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if object, ok := obj.(*unstructured.Unstructured); ok {
		gvk := object.GroupVersionKind()
		if gvk.Group == "apiextensions.k8s.io" && gvk.Kind == "CustomResourceDefinition" &&
			!r.existingCRDs[key.Name] {
			return apierrors.NewNotFound(
				schema.GroupResource{Group: "apiextensions.k8s.io", Resource: "customresourcedefinitions"},
				key.Name,
			)
		}
	}
	return r.Reader.Get(ctx, key, obj, opts...)
}

func (r *testAPIReader) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if r.listHook != nil {
		if handled, err := r.listHook(ctx, list); handled {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, ok := list.(*unstructured.UnstructuredList); ok {
		gvk := list.GetObjectKind().GroupVersionKind()
		if err, exists := r.optionalListErrors[gvk]; exists {
			return err
		}
		return errors.New("unexpected unstructured inventory list " + gvk.String())
	}
	return r.Reader.List(ctx, list, opts...)
}

func newTestReconciler(
	t *testing.T,
	now *time.Time,
	objects ...client.Object,
) (*Reconciler, client.Client, *record.FakeRecorder, *testStatusClient, *testAPIReader) {
	return newTestReconcilerWithHooks(t, now, nil, nil, objects...)
}

func newTestReconcilerWithHooks(
	t *testing.T,
	now *time.Time,
	statusClient *testStatusClient,
	listHook func(context.Context, client.ObjectList) (bool, error),
	objects ...client.Object,
) (*Reconciler, client.Client, *record.FakeRecorder, *testStatusClient, *testAPIReader) {
	t.Helper()
	fc := fake.NewClientBuilder().
		WithScheme(testScheme()).
		WithObjects(objects...).
		WithStatusSubresource(&agenticv1alpha1.AgenticOLSConfig{}, &agenticv1alpha1.AgenticRun{}).
		Build()
	if statusClient == nil {
		statusClient = &testStatusClient{}
	}
	statusClient.Client = fc
	reader := &testAPIReader{
		Reader:             fc,
		listHook:           listHook,
		optionalListErrors: testOptionalSandboxListErrors(),
	}
	recorder := record.NewFakeRecorder(8)
	r := &Reconciler{
		Client:        statusClient,
		APIReader:     reader,
		Sandbox:       agenticrun.NewSandboxManager(statusClient, reader, &configuration.Cache{ForceBareMode: true}, "default", nil),
		EventRecorder: recorder,
	}
	if now != nil {
		r.Now = func() time.Time { return *now }
	}
	return r, fc, recorder, statusClient, reader
}

func reconcileOnce(r *Reconciler) (ctrl.Result, error) {
	return r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "cluster"},
	})
}

func getConfig(t *testing.T, c client.Client) *agenticv1alpha1.AgenticOLSConfig {
	t.Helper()
	cfg := &agenticv1alpha1.AgenticOLSConfig{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "cluster"}, cfg); err != nil {
		t.Fatalf("get config: %v", err)
	}
	return cfg
}

func getRun(t *testing.T, c client.Client, name string) *agenticv1alpha1.AgenticRun {
	t.Helper()
	run := &agenticv1alpha1.AgenticRun{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: name}, run); err != nil {
		t.Fatalf("get run %q: %v", name, err)
	}
	return run
}
func conditionHistoryEqual(left, right []metav1.Condition) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i].Type != right[i].Type ||
			left[i].Status != right[i].Status ||
			left[i].ObservedGeneration != right[i].ObservedGeneration ||
			left[i].Reason != right[i].Reason ||
			left[i].Message != right[i].Message ||
			!left[i].LastTransitionTime.Time.Equal(right[i].LastTransitionTime.Time) {
			return false
		}
	}
	return true
}

func optionalTimeHistoryEqual(left, right *metav1.Time) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return left.Time.Equal(right.Time)
}

func stepHistoryEqual(
	leftConditions, rightConditions []metav1.Condition,
	leftSandbox, rightSandbox agenticv1alpha1.SandboxInfo,
	leftResults, rightResults []agenticv1alpha1.StepResultRef,
) bool {
	return conditionHistoryEqual(leftConditions, rightConditions) &&
		reflect.DeepEqual(leftSandbox, rightSandbox) &&
		reflect.DeepEqual(leftResults, rightResults)
}

func runStatusHistoryEqual(left, right agenticv1alpha1.AgenticRunStatus) bool {
	return conditionHistoryEqual(left.Conditions, right.Conditions) &&
		stepHistoryEqual(left.Steps.Analysis.Conditions, right.Steps.Analysis.Conditions,
			left.Steps.Analysis.Sandbox, right.Steps.Analysis.Sandbox,
			left.Steps.Analysis.Results, right.Steps.Analysis.Results) &&
		stepHistoryEqual(left.Steps.Execution.Conditions, right.Steps.Execution.Conditions,
			left.Steps.Execution.Sandbox, right.Steps.Execution.Sandbox,
			left.Steps.Execution.Results, right.Steps.Execution.Results) &&
		stepHistoryEqual(left.Steps.Verification.Conditions, right.Steps.Verification.Conditions,
			left.Steps.Verification.Sandbox, right.Steps.Verification.Sandbox,
			left.Steps.Verification.Results, right.Steps.Verification.Results) &&
		stepHistoryEqual(left.Steps.Escalation.Conditions, right.Steps.Escalation.Conditions,
			left.Steps.Escalation.Sandbox, right.Steps.Escalation.Sandbox,
			left.Steps.Escalation.Results, right.Steps.Escalation.Results) &&
		reflect.DeepEqual(left.TokenUsage, right.TokenUsage) &&
		optionalTimeHistoryEqual(left.TerminalTime, right.TerminalTime) &&
		optionalTimeHistoryEqual(left.DeleteAfter, right.DeleteAfter)
}

func waitForEvent(t *testing.T, recorder *record.FakeRecorder, wantReason string) string {
	t.Helper()
	select {
	case event := <-recorder.Events:
		if !strings.Contains(event, wantReason) {
			t.Fatalf("event = %q, want reason %q", event, wantReason)
		}
		return event
	default:
		t.Fatalf("no event with reason %q was emitted", wantReason)
		return ""
	}
}

func suspendedConfigAt(generation int64, start time.Time, reason string, safeStatus metav1.ConditionStatus) *agenticv1alpha1.AgenticOLSConfig {
	config := testConfig(true)
	config.Generation = generation
	config.Status.Conditions = []metav1.Condition{
		{
			Type:               agenticv1alpha1.AgenticOLSConfigConditionSuspended,
			Status:             metav1.ConditionTrue,
			Reason:             reason,
			Message:            "existing activation",
			ObservedGeneration: generation,
			LastTransitionTime: metav1.NewTime(start),
		},
		{
			Type:               agenticv1alpha1.AgenticOLSConfigConditionSafeToResume,
			Status:             safeStatus,
			Reason:             reasonStopping,
			Message:            "existing safety state",
			ObservedGeneration: generation,
			LastTransitionTime: metav1.NewTime(start),
		},
	}
	return config
}

func heldManagedPod(runUID types.UID) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "held-sandbox",
			Namespace:  "default",
			UID:        types.UID("held-pod-uid"),
			Finalizers: []string{"test.agentic.openshift.io/hold"},
			Labels: map[string]string{
				agenticrun.LabelRun:  string(runUID),
				agenticrun.LabelStep: "analysis",
			},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

func requireCondition(
	t *testing.T,
	conditions []metav1.Condition,
	conditionType string,
) metav1.Condition {
	t.Helper()
	condition := findCondition(conditions, conditionType)
	if condition == nil {
		t.Fatalf("condition %q not set", conditionType)
	}
	return *condition
}

func removePodFinalizer(t *testing.T, c client.Client, name string) {
	t.Helper()
	pod := &corev1.Pod{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: name}, pod); err != nil {
		t.Fatalf("get held Pod: %v", err)
	}
	pod.Finalizers = nil
	if err := c.Update(context.Background(), pod); err != nil {
		t.Fatalf("remove held Pod finalizer: %v", err)
	}
}

func completeAcceptedStop(t *testing.T, c client.Client, name string) {
	t.Helper()
	run := getRun(t, c, name)
	base := run.DeepCopy()
	meta.SetStatusCondition(&run.Status.Conditions, metav1.Condition{
		Type:               agenticv1alpha1.AgenticRunConditionStopping,
		Status:             metav1.ConditionFalse,
		Reason:             reasonTeardownComplete,
		Message:            "Sandbox workload and access cleanup confirmed",
		ObservedGeneration: run.Generation,
	})
	meta.SetStatusCondition(&run.Status.Conditions, metav1.Condition{
		Type:               agenticv1alpha1.AgenticRunConditionEmergencyStopped,
		Status:             metav1.ConditionTrue,
		Reason:             reasonSystemSuspended,
		Message:            "Terminated by global suspension",
		ObservedGeneration: run.Generation,
	})
	if err := c.Status().Patch(context.Background(), run, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
		t.Fatalf("persist terminal run outcome: %v", err)
	}
}

func TestReconcile_NotFound(t *testing.T) {
	r, _, _, _, _ := newTestReconciler(t, nil)
	_, err := reconcileOnce(r)
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
}

func TestReconcile_FreshActivationNormalizesTimeAndAcceptsStopsBeforeCleanup(t *testing.T) {
	now := time.Date(2026, 10, 26, 11, 59, 30, 987654321, time.UTC)
	wantStart := now.UTC().Truncate(time.Second)
	config := testConfig(true)
	run := pendingAgenticRun("precleanup-run")
	var statusStore client.Client
	observedBeforeCleanup := false
	listHook := func(_ context.Context, list client.ObjectList) (bool, error) {
		if _, ok := list.(*corev1.PodList); !ok || observedBeforeCleanup {
			return false, nil
		}
		observedBeforeCleanup = true
		current := &agenticv1alpha1.AgenticOLSConfig{}
		if err := statusStore.Get(context.Background(), types.NamespacedName{Name: "cluster"}, current); err != nil {
			return true, err
		}
		suspended := findCondition(current.Status.Conditions, agenticv1alpha1.AgenticOLSConfigConditionSuspended)
		safe := findCondition(current.Status.Conditions, agenticv1alpha1.AgenticOLSConfigConditionSafeToResume)
		if suspended == nil || suspended.Status != metav1.ConditionTrue ||
			suspended.Reason != reasonStopping || suspended.ObservedGeneration != current.Generation ||
			!suspended.LastTransitionTime.Time.Equal(wantStart) {
			return true, errors.New("current-generation Stopping was not durable before cleanup")
		}
		if safe == nil || safe.Status != metav1.ConditionFalse || safe.ObservedGeneration != current.Generation {
			return true, errors.New("SafeToResume=False was not durable before cleanup")
		}
		currentRun := &agenticv1alpha1.AgenticRun{}
		if err := statusStore.Get(context.Background(), types.NamespacedName{Namespace: run.Namespace, Name: run.Name}, currentRun); err != nil {
			return true, err
		}
		marker := findCondition(currentRun.Status.Conditions, agenticv1alpha1.AgenticRunConditionStopping)
		if marker == nil || marker.Status != metav1.ConditionTrue ||
			marker.Reason != reasonSystemSuspended || marker.ObservedGeneration != currentRun.Generation ||
			!marker.LastTransitionTime.Equal(&suspended.LastTransitionTime) {
			return true, errors.New("UID-bound run stop acceptance was not durable before cleanup")
		}
		return false, nil
	}
	r, fc, _, _, _ := newTestReconcilerWithHooks(t, &now, nil, listHook, config, run)
	statusStore = fc

	if _, err := reconcileOnce(r); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if !observedBeforeCleanup {
		t.Fatal("global cleanup inventory was not observed")
	}
}

func TestReconcile_Activation(t *testing.T) {
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	config := testConfig(true)
	stopped := emergencyStoppedAgenticRun("stopped-1")
	r, fc, recorder, _, _ := newTestReconciler(t, &now, config, stopped)

	result, err := reconcileOnce(r)
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if result.RequeueAfter != requeueDelay {
		t.Fatalf("requeue after = %v, want %v to discover late resources", result.RequeueAfter, requeueDelay)
	}

	got := getConfig(t, fc)
	suspended := requireCondition(t, got.Status.Conditions, agenticv1alpha1.AgenticOLSConfigConditionSuspended)
	if suspended.Status != metav1.ConditionTrue || suspended.Reason != reasonAdminActivated {
		t.Fatalf("Suspended = %s/%s, want True/%s", suspended.Status, suspended.Reason, reasonAdminActivated)
	}
	if suspended.ObservedGeneration != got.Generation || !suspended.LastTransitionTime.Time.Equal(now) {
		t.Fatalf("Suspended activation identity = generation %d at %v, want %d at %v",
			suspended.ObservedGeneration, suspended.LastTransitionTime, got.Generation, now)
	}
	safe := requireCondition(t, got.Status.Conditions, agenticv1alpha1.AgenticOLSConfigConditionSafeToResume)
	if safe.Status != metav1.ConditionTrue || safe.ObservedGeneration != got.Generation ||
		!safe.LastTransitionTime.Time.Equal(now) {
		t.Fatalf("SafeToResume = %s generation %d at %v, want True generation %d at %v",
			safe.Status, safe.ObservedGeneration, safe.LastTransitionTime, got.Generation, now)
	}

	waitForEvent(t, recorder, eventReasonSuspensionActivated)
}

func TestReconcile_ActivationPendingAgenticRuns(t *testing.T) {
	now := time.Date(2026, 10, 26, 12, 1, 0, 0, time.UTC)
	config := testConfig(true)
	run := pendingAgenticRun("pending-1")
	r, fc, recorder, _, _ := newTestReconciler(t, &now, config, run)

	result, err := reconcileOnce(r)
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if result.RequeueAfter == 0 {
		t.Fatal("expected requeue while run outcomes remain pending")
	}

	waitForEvent(t, recorder, eventReasonSuspensionActivated)

	gotConfig := getConfig(t, fc)
	suspended := requireCondition(t, gotConfig.Status.Conditions, agenticv1alpha1.AgenticOLSConfigConditionSuspended)
	if suspended.Status != metav1.ConditionTrue || suspended.Reason != reasonStopping {
		t.Fatalf("Suspended = %s/%s, want True/Stopping", suspended.Status, suspended.Reason)
	}
	if suspended.ObservedGeneration != gotConfig.Generation {
		t.Fatalf("Suspended observed generation = %d, want %d", suspended.ObservedGeneration, gotConfig.Generation)
	}
	safe := requireCondition(t, gotConfig.Status.Conditions, agenticv1alpha1.AgenticOLSConfigConditionSafeToResume)
	if safe.Status != metav1.ConditionTrue || safe.ObservedGeneration != gotConfig.Generation {
		t.Fatalf("SafeToResume = %s generation %d, want True generation %d after cleanup and marker acceptance",
			safe.Status, safe.ObservedGeneration, gotConfig.Generation)
	}

	gotRun := getRun(t, fc, run.Name)
	marker := requireCondition(t, gotRun.Status.Conditions, agenticv1alpha1.AgenticRunConditionStopping)
	if marker.Status != metav1.ConditionTrue || marker.Reason != reasonSystemSuspended {
		t.Fatalf("run Stopping = %s/%s, want True/%s", marker.Status, marker.Reason, reasonSystemSuspended)
	}
	if marker.ObservedGeneration != gotRun.Generation || !marker.LastTransitionTime.Equal(&suspended.LastTransitionTime) {
		t.Fatalf("run stop acceptance did not retain activation identity: %+v", marker)
	}
}

func TestReconcile_SameGenerationRestartPreservesDeadline(t *testing.T) {
	start := time.Date(2026, 10, 26, 12, 2, 0, 0, time.UTC)
	now := start.Add(9 * time.Second)
	config := suspendedConfigAt(4, start, reasonAdminActivated, metav1.ConditionFalse)
	pod := heldManagedPod(types.UID("old-run-uid"))
	r, fc, _, statusClient, _ := newTestReconciler(t, &now, config, pod)
	statusClient.holdFinalizedPods = true

	result, err := reconcileOnce(r)
	if err != nil {
		t.Fatalf("first Reconcile() error = %v", err)
	}
	first := getConfig(t, fc)
	firstSuspended := requireCondition(t, first.Status.Conditions, agenticv1alpha1.AgenticOLSConfigConditionSuspended)
	if firstSuspended.Reason != reasonStopping || !firstSuspended.LastTransitionTime.Time.Equal(start) {
		t.Fatalf("same-generation reason transition changed activation time: %+v", firstSuspended)
	}
	if result.RequeueAfter <= 0 || result.RequeueAfter > time.Second {
		t.Fatalf("requeue after = %v, want a deadline-bounded delay no greater than one second", result.RequeueAfter)
	}

	now = start.Add(agenticrun.SuspensionStopWindow)
	restarted := &Reconciler{
		Client:        r.Client,
		APIReader:     r.APIReader,
		Sandbox:       agenticrun.NewSandboxManager(r.Client, r.APIReader, &configuration.Cache{ForceBareMode: true}, "default", nil),
		EventRecorder: record.NewFakeRecorder(2),
		Now:           func() time.Time { return now },
	}
	_, err = reconcileOnce(restarted)
	if err != nil {
		t.Fatalf("restarted Reconcile() error = %v", err)
	}
	got := getConfig(t, fc)
	suspended := requireCondition(t, got.Status.Conditions, agenticv1alpha1.AgenticOLSConfigConditionSuspended)
	if suspended.Reason != reasonAdminActivatedWithErrors || !suspended.LastTransitionTime.Time.Equal(start) {
		t.Fatalf("restart moved the original deadline or hid its error: %+v", suspended)
	}
	if !strings.Contains(suspended.Message, "held-sandbox") {
		t.Fatalf("deadline diagnostic does not identify the held resource: %q", suspended.Message)
	}
	safe := requireCondition(t, got.Status.Conditions, agenticv1alpha1.AgenticOLSConfigConditionSafeToResume)
	if safe.Status != metav1.ConditionFalse || safe.ObservedGeneration != got.Generation {
		t.Fatalf("SafeToResume = %s generation %d, want False generation %d", safe.Status, safe.ObservedGeneration, got.Generation)
	}
}

func TestReconcile_StaleGenerationDoesNotAuthorizeSafety(t *testing.T) {
	oldStart := time.Date(2026, 10, 26, 11, 59, 0, 0, time.UTC)
	now := time.Date(2026, 10, 26, 12, 3, 0, 0, time.UTC)
	config := suspendedConfigAt(3, oldStart, reasonAdminActivated, metav1.ConditionTrue)
	config.Status.Conditions[0].ObservedGeneration = 2
	config.Status.Conditions[1].ObservedGeneration = 2
	pod := heldManagedPod(types.UID("rapid-run-uid"))
	r, fc, _, statusClient, _ := newTestReconciler(t, &now, config, pod)
	statusClient.holdFinalizedPods = true

	_, err := reconcileOnce(r)
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	got := getConfig(t, fc)
	suspended := requireCondition(t, got.Status.Conditions, agenticv1alpha1.AgenticOLSConfigConditionSuspended)
	safe := requireCondition(t, got.Status.Conditions, agenticv1alpha1.AgenticOLSConfigConditionSafeToResume)
	if suspended.Reason != reasonStopping || suspended.ObservedGeneration != got.Generation ||
		!suspended.LastTransitionTime.Time.Equal(now) {
		t.Fatalf("new generation did not establish a fresh activation: %+v", suspended)
	}
	if safe.Status != metav1.ConditionFalse || safe.ObservedGeneration != got.Generation ||
		!safe.LastTransitionTime.Time.Equal(now) {
		t.Fatalf("new generation inherited prior safety: %+v", safe)
	}
}

func TestReconcile_AmbiguousTerminalRunGetsAuxiliaryStopMarker(t *testing.T) {
	now := time.Date(2026, 10, 26, 12, 3, 30, 0, time.UTC)
	config := testConfig(true)
	run := completedAgenticRun("ambiguous-terminal", "")
	run.Status.TerminalTime = nil
	originalDeleteAfter := *run.Status.DeleteAfter
	r, fc, _, _, _ := newTestReconciler(t, &now, config, run)

	if _, err := reconcileOnce(r); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	gotConfig := getConfig(t, fc)
	suspended := requireCondition(t, gotConfig.Status.Conditions, agenticv1alpha1.AgenticOLSConfigConditionSuspended)
	safe := requireCondition(t, gotConfig.Status.Conditions, agenticv1alpha1.AgenticOLSConfigConditionSafeToResume)
	gotRun := getRun(t, fc, run.Name)
	marker := requireCondition(t, gotRun.Status.Conditions, agenticv1alpha1.AgenticRunConditionStopping)
	verified := requireCondition(t, gotRun.Status.Conditions, agenticv1alpha1.AgenticRunConditionVerified)
	if marker.Status != metav1.ConditionTrue || marker.Reason != reasonSystemSuspended ||
		marker.ObservedGeneration != gotRun.Generation ||
		!marker.LastTransitionTime.Equal(&suspended.LastTransitionTime) {
		t.Fatalf("ambiguous terminal run did not receive activation-bound auxiliary stop intent: %+v", marker)
	}
	if verified.Status != metav1.ConditionTrue ||
		agenticv1alpha1.DerivePhase(gotRun.Status.Conditions) != agenticv1alpha1.AgenticRunPhaseCompleted ||
		gotRun.Status.TerminalTime != nil ||
		gotRun.Status.DeleteAfter == nil || !gotRun.Status.DeleteAfter.Equal(&originalDeleteAfter) ||
		suspended.Reason != reasonAdminActivated ||
		safe.Status != metav1.ConditionTrue {
		t.Fatalf("auxiliary stop marker changed terminal history or blocked confirmed safety: %+v / %+v / %+v",
			gotRun.Status, suspended, safe)
	}
}

func TestReconcile_RecoveryPreservesTerminalErrorAndUpdatesSafety(t *testing.T) {
	start := time.Date(2026, 10, 26, 12, 4, 0, 0, time.UTC)
	now := start.Add(agenticrun.SuspensionStopWindow)
	config := suspendedConfigAt(5, start, reasonAdminActivatedWithErrors, metav1.ConditionFalse)
	run := pendingAgenticRun("recover-me")
	pod := heldManagedPod(run.UID)
	r, fc, _, statusClient, _ := newTestReconciler(t, &now, config, run, pod)
	statusClient.holdFinalizedPods = true

	_, err := reconcileOnce(r)
	if err != nil {
		t.Fatalf("initial error-state Reconcile() error = %v", err)
	}
	first := getConfig(t, fc)
	firstSuspended := requireCondition(t, first.Status.Conditions, agenticv1alpha1.AgenticOLSConfigConditionSuspended)
	firstSafe := requireCondition(t, first.Status.Conditions, agenticv1alpha1.AgenticOLSConfigConditionSafeToResume)
	if firstSuspended.Reason != reasonAdminActivatedWithErrors || firstSafe.Status != metav1.ConditionFalse {
		t.Fatalf("unresolved cleanup did not retain unsafe terminal error: %+v / %+v", firstSuspended, firstSafe)
	}

	statusClient.holdFinalizedPods = false
	removePodFinalizer(t, fc, pod.Name)
	completeAcceptedStop(t, fc, run.Name)
	gotRun := getRun(t, fc, run.Name)
	if !isTerminal(agenticv1alpha1.DerivePhase(gotRun.Status.Conditions)) {
		t.Fatal("fixture run did not reach its terminal stop outcome")
	}

	_, err = reconcileOnce(r)
	if err != nil {
		t.Fatalf("recovery Reconcile() error = %v", err)
	}
	recovered := getConfig(t, fc)
	recoveredSuspended := requireCondition(t, recovered.Status.Conditions, agenticv1alpha1.AgenticOLSConfigConditionSuspended)
	recoveredSafe := requireCondition(t, recovered.Status.Conditions, agenticv1alpha1.AgenticOLSConfigConditionSafeToResume)
	if recoveredSuspended.Reason != reasonAdminActivatedWithErrors {
		t.Fatalf("recovery changed terminal global reason to %q", recoveredSuspended.Reason)
	}
	if recoveredSafe.Status != metav1.ConditionTrue || recoveredSafe.ObservedGeneration != recovered.Generation ||
		!recoveredSafe.LastTransitionTime.Time.Equal(now) {
		t.Fatalf("recovery did not open safety at the injected time after cleanup confirmation: %+v", recoveredSafe)
	}
}

func TestReconcile_UnsavedMarkerAtDeadlineDoesNotStayStopping(t *testing.T) {
	start := time.Date(2026, 10, 26, 12, 5, 0, 0, time.UTC)
	now := start.Add(agenticrun.SuspensionStopWindow)
	config := suspendedConfigAt(7, start, reasonStopping, metav1.ConditionFalse)
	run := pendingAgenticRun("marker-write-fails")
	statusClient := &testStatusClient{failRunStatusPatch: true}
	r, fc, _, _, _ := newTestReconcilerWithHooks(t, &now, statusClient, nil, config, run)

	result, err := reconcileOnce(r)
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	got := getConfig(t, fc)
	suspended := requireCondition(t, got.Status.Conditions, agenticv1alpha1.AgenticOLSConfigConditionSuspended)
	safe := requireCondition(t, got.Status.Conditions, agenticv1alpha1.AgenticOLSConfigConditionSafeToResume)
	if suspended.Reason != reasonAdminActivatedWithErrors {
		t.Fatalf("unsaved marker left global status at %q past deadline", suspended.Reason)
	}
	if !strings.Contains(suspended.Message, "marker-write-fails") || !strings.Contains(suspended.Message, "unsaved stop markers") {
		t.Fatalf("deadline diagnostic omitted the unsaved run marker: %q", suspended.Message)
	}
	if safe.Status != metav1.ConditionFalse || safe.ObservedGeneration != got.Generation {
		t.Fatalf("unsaved marker incorrectly authorized resumption: %+v", safe)
	}
	if result.RequeueAfter != requeueDelay {
		t.Fatalf("terminal error requeue = %v, want %v for background cleanup", result.RequeueAfter, requeueDelay)
	}

	_, err = reconcileOnce(r)
	if err != nil {
		t.Fatalf("second Reconcile() error = %v", err)
	}
	again := getConfig(t, fc)
	if gotCondition := requireCondition(t, again.Status.Conditions, agenticv1alpha1.AgenticOLSConfigConditionSuspended); gotCondition.Reason != reasonAdminActivatedWithErrors {
		t.Fatalf("repeated failure reverted terminal error to %q", gotCondition.Reason)
	}
}

func TestReconcile_RunMarkerConflictPreservesNaturalCompletionAndRetriesTerminalAcceptance(t *testing.T) {
	start := time.Date(2026, 10, 26, 12, 5, 30, 0, time.UTC)
	now := start.Add(agenticrun.SuspensionStopWindow)
	naturalTerminalTime := start.Add(time.Second)
	naturalDeleteAfter := metav1.NewTime(naturalTerminalTime.Add(24 * time.Hour))
	config := suspendedConfigAt(7, start, reasonStopping, metav1.ConditionFalse)
	run := pendingAgenticRun("natural-completion-race")
	statusClient := &testStatusClient{
		conflictRunStatusPatch: true,
		failRunStatusPatch:     true,
		naturalTerminalTime:    naturalTerminalTime,
	}
	r, fc, _, statusClient, _ := newTestReconcilerWithHooks(t, &now, statusClient, nil, config, run)

	_, err := reconcileOnce(r)
	if err != nil {
		t.Fatalf("first Reconcile() error = %v", err)
	}
	if !statusClient.conflictedRunStatus {
		t.Fatal("fixture did not create the natural-completion status conflict")
	}
	firstConfig := getConfig(t, fc)
	firstSuspended := requireCondition(t, firstConfig.Status.Conditions, agenticv1alpha1.AgenticOLSConfigConditionSuspended)
	firstSafe := requireCondition(t, firstConfig.Status.Conditions, agenticv1alpha1.AgenticOLSConfigConditionSafeToResume)
	if firstSuspended.Reason != reasonAdminActivatedWithErrors ||
		!strings.Contains(firstSuspended.Message, run.Name) ||
		!strings.Contains(firstSuspended.Message, "unsaved stop markers") ||
		firstSafe.Status != metav1.ConditionFalse {
		t.Fatalf("failed terminal acceptance did not remain a deadline safety blocker: %+v / %+v", firstSuspended, firstSafe)
	}
	firstRun := getRun(t, fc, run.Name)
	if !isTerminal(agenticv1alpha1.DerivePhase(firstRun.Status.Conditions)) ||
		findCondition(firstRun.Status.Conditions, agenticv1alpha1.AgenticRunConditionStopping) != nil ||
		firstRun.Status.TerminalTime == nil ||
		!firstRun.Status.TerminalTime.Time.Equal(naturalTerminalTime) ||
		firstRun.Status.DeleteAfter == nil ||
		!firstRun.Status.DeleteAfter.Equal(&naturalDeleteAfter) {
		t.Fatalf("natural terminal history was overwritten or the failed marker was falsely claimed: %+v", firstRun.Status)
	}

	statusClient.failRunStatusPatch = false
	restarted := &Reconciler{
		Client:        r.Client,
		APIReader:     r.APIReader,
		Sandbox:       agenticrun.NewSandboxManager(r.Client, r.APIReader, &configuration.Cache{ForceBareMode: true}, "default", nil),
		EventRecorder: record.NewFakeRecorder(2),
		Now:           func() time.Time { return now },
	}
	if _, err := reconcileOnce(restarted); err != nil {
		t.Fatalf("terminal-cohort retry Reconcile() error = %v", err)
	}
	recoveredConfig := getConfig(t, fc)
	recoveredSuspended := requireCondition(t, recoveredConfig.Status.Conditions, agenticv1alpha1.AgenticOLSConfigConditionSuspended)
	recoveredSafe := requireCondition(t, recoveredConfig.Status.Conditions, agenticv1alpha1.AgenticOLSConfigConditionSafeToResume)
	if recoveredSuspended.Reason != reasonAdminActivatedWithErrors ||
		recoveredSafe.Status != metav1.ConditionTrue {
		t.Fatalf("terminal retry changed error outcome or failed to open safety after durable acceptance: %+v / %+v",
			recoveredSuspended, recoveredSafe)
	}
	recoveredRun := getRun(t, fc, run.Name)
	marker := requireCondition(t, recoveredRun.Status.Conditions, agenticv1alpha1.AgenticRunConditionStopping)
	verified := requireCondition(t, recoveredRun.Status.Conditions, agenticv1alpha1.AgenticRunConditionVerified)
	if marker.Status != metav1.ConditionTrue || marker.Reason != reasonSystemSuspended ||
		!marker.LastTransitionTime.Time.Equal(start) ||
		verified.Status != metav1.ConditionTrue || verified.Reason != "CompletedNaturally" ||
		recoveredRun.Status.TerminalTime == nil ||
		!recoveredRun.Status.TerminalTime.Time.Equal(naturalTerminalTime) ||
		recoveredRun.Status.DeleteAfter == nil ||
		!recoveredRun.Status.DeleteAfter.Equal(&naturalDeleteAfter) {
		t.Fatalf("auxiliary stop acceptance did not preserve natural terminal history: %+v", recoveredRun.Status)
	}
}

func TestReconcile_MissingSuspensionDependenciesAreErrors(t *testing.T) {
	now := time.Date(2026, 10, 26, 12, 5, 45, 0, time.UTC)
	r, _, _, _, _ := newTestReconciler(t, &now, testConfig(true))

	r.Sandbox = nil
	if _, err := reconcileOnce(r); err == nil {
		t.Fatal("missing sandbox manager silently accepted global cleanup")
	}
	r.Sandbox = agenticrun.NewSandboxManager(r.Client, r.APIReader, &configuration.Cache{ForceBareMode: true}, "default", nil)
	r.APIReader = nil
	if _, err := reconcileOnce(r); err == nil {
		t.Fatal("missing uncached API reader silently accepted global cleanup")
	}
}

func TestReconcile_RapidGenerationResetsDeadlineAndSafety(t *testing.T) {
	oldStart := time.Date(2026, 10, 26, 12, 3, 0, 0, time.UTC)
	now := oldStart.Add(time.Second)
	config := suspendedConfigAt(1, oldStart, reasonAdminActivated, metav1.ConditionTrue)
	pod := heldManagedPod(types.UID("rapid-run-uid"))
	r, fc, _, statusClient, _ := newTestReconciler(t, &now, config, pod)
	statusClient.holdFinalizedPods = true

	stored := getConfig(t, fc)
	stored.Spec.Suspended = false
	stored.Generation = 2
	if err := fc.Update(context.Background(), stored); err != nil {
		t.Fatalf("persist rapid deactivation generation: %v", err)
	}
	if _, err := reconcileOnce(r); err != nil {
		t.Fatalf("deactivation Reconcile() error = %v", err)
	}

	now = oldStart.Add(2 * time.Second)
	stored = getConfig(t, fc)
	stored.Spec.Suspended = true
	stored.Generation = 3
	if err := fc.Update(context.Background(), stored); err != nil {
		t.Fatalf("persist rapid activation generation: %v", err)
	}
	if _, err := reconcileOnce(r); err != nil {
		t.Fatalf("new activation Reconcile() error = %v", err)
	}
	got := getConfig(t, fc)
	suspended := requireCondition(t, got.Status.Conditions, agenticv1alpha1.AgenticOLSConfigConditionSuspended)
	safe := requireCondition(t, got.Status.Conditions, agenticv1alpha1.AgenticOLSConfigConditionSafeToResume)
	if suspended.Reason != reasonStopping || suspended.ObservedGeneration != got.Generation ||
		!suspended.LastTransitionTime.Time.Equal(now) {
		t.Fatalf("rapid activation did not establish a fresh deadline: %+v", suspended)
	}
	if safe.Status != metav1.ConditionFalse || safe.ObservedGeneration != got.Generation ||
		!safe.LastTransitionTime.Time.Equal(now) {
		t.Fatalf("rapid activation inherited prior safety: %+v", safe)
	}
}

func TestReconcile_StalledCleanupUsesLiveStatusContextAtDeadline(t *testing.T) {
	now := time.Date(2026, 10, 26, 12, 7, 0, 0, time.UTC)
	start := now.Add(-agenticrun.SuspensionStopWindow + time.Nanosecond)
	config := suspendedConfigAt(1, start, reasonStopping, metav1.ConditionFalse)
	waitedForDeadline := false
	listHook := func(ctx context.Context, list client.ObjectList) (bool, error) {
		if _, ok := list.(*corev1.PodList); !ok || waitedForDeadline {
			return false, nil
		}
		waitedForDeadline = true
		<-ctx.Done()
		return true, ctx.Err()
	}
	statusClient := &testStatusClient{requireLiveConfigStatus: true}
	r, fc, _, statusClient, _ := newTestReconcilerWithHooks(t, &now, statusClient, listHook, config)

	_, err := reconcileOnce(r)
	if err != nil {
		t.Fatalf("deadline status write failed after bounded cleanup: %v", err)
	}
	if !waitedForDeadline {
		t.Fatal("cleanup fixture did not hold the Pod inventory request to its deadline")
	}
	if statusClient.sawCanceledConfigStatusPatch {
		t.Fatal("deadline outcome status used the expired cleanup context")
	}
	got := getConfig(t, fc)
	suspended := requireCondition(t, got.Status.Conditions, agenticv1alpha1.AgenticOLSConfigConditionSuspended)
	if suspended.Reason != reasonAdminActivatedWithErrors || suspended.ObservedGeneration != got.Generation {
		t.Fatalf("stalled request did not persist current-generation deadline outcome: %+v", suspended)
	}
}

func TestReconcile_StalledRunMarkerUsesDeadlineAndLiveStatusContext(t *testing.T) {
	start := time.Date(2026, 10, 26, 12, 7, 30, 0, time.UTC)
	now := start
	config := suspendedConfigAt(4, start, reasonStopping, metav1.ConditionFalse)
	run := pendingAgenticRun("stalled-run-marker")
	statusClient := &testStatusClient{
		requireLiveConfigStatus: true,
		stallRunStatusPatch:     true,
	}
	statusClient.advanceRunMarkerClock = func() {
		now = start.Add(agenticrun.SuspensionStopWindow)
	}
	var reader *testAPIReader
	runListHadDeadline, runGetHadDeadline := false, false
	listHook := func(ctx context.Context, list client.ObjectList) (bool, error) {
		if _, ok := list.(*agenticv1alpha1.AgenticRunList); !ok {
			return false, nil
		}
		_, runListHadDeadline = ctx.Deadline()
		if reader == nil {
			return true, errors.New("test API reader is not initialized")
		}
		return true, reader.Reader.List(context.Background(), list)
	}
	r, fc, _, statusClient, apiReader := newTestReconcilerWithHooks(t, &now, statusClient, listHook, config, run)
	reader = apiReader
	reader.getHook = func(ctx context.Context, key client.ObjectKey, obj client.Object) (bool, error) {
		if _, ok := obj.(*agenticv1alpha1.AgenticRun); !ok || key.Name != run.Name {
			return false, nil
		}
		_, runGetHadDeadline = ctx.Deadline()
		return true, reader.Reader.Get(context.Background(), key, obj)
	}

	_, err := reconcileOnce(r)
	if err != nil {
		t.Fatalf("run-marker timeout prevented deadline status: %v", err)
	}
	if !runListHadDeadline || !runGetHadDeadline ||
		!statusClient.stalledRunStatusPatch ||
		!statusClient.runStatusPatchHadDeadline ||
		!statusClient.runStatusPatchReachedDeadline {
		t.Fatal("run marker inventory/read/write did not use the bounded pass context")
	}
	if statusClient.sawCanceledConfigStatusPatch {
		t.Fatal("global outcome patch used the expired run-marker context")
	}
	gotConfig := getConfig(t, fc)
	suspended := requireCondition(t, gotConfig.Status.Conditions, agenticv1alpha1.AgenticOLSConfigConditionSuspended)
	safe := requireCondition(t, gotConfig.Status.Conditions, agenticv1alpha1.AgenticOLSConfigConditionSafeToResume)
	if suspended.Reason != reasonAdminActivatedWithErrors ||
		!strings.Contains(suspended.Message, run.Name) ||
		safe.Status != metav1.ConditionFalse {
		t.Fatalf("stalled marker did not produce a deadline error and safety block: %+v / %+v", suspended, safe)
	}
	if marker := findCondition(getRun(t, fc, run.Name).Status.Conditions, agenticv1alpha1.AgenticRunConditionStopping); marker != nil {
		t.Fatalf("timed-out marker patch unexpectedly wrote run stop state: %+v", marker)
	}
}

func TestReconcile_RunListFailureAtDeadlinePersistsError(t *testing.T) {
	start := time.Date(2026, 10, 26, 12, 6, 30, 0, time.UTC)
	now := start.Add(agenticrun.SuspensionStopWindow)
	config := suspendedConfigAt(3, start, reasonStopping, metav1.ConditionFalse)
	listHook := func(_ context.Context, list client.ObjectList) (bool, error) {
		if _, ok := list.(*agenticv1alpha1.AgenticRunList); ok {
			return true, errors.New("run inventory denied")
		}
		return false, nil
	}
	statusClient := &testStatusClient{requireLiveConfigStatus: true}
	r, fc, _, statusClient, _ := newTestReconcilerWithHooks(t, &now, statusClient, listHook, config)

	_, err := reconcileOnce(r)
	if err != nil {
		t.Fatalf("run-list failure prevented deadline outcome: %v", err)
	}
	if statusClient.sawCanceledConfigStatusPatch {
		t.Fatal("run-list failure caused outcome status to use a canceled context")
	}
	got := getConfig(t, fc)
	suspended := requireCondition(t, got.Status.Conditions, agenticv1alpha1.AgenticOLSConfigConditionSuspended)
	safe := requireCondition(t, got.Status.Conditions, agenticv1alpha1.AgenticOLSConfigConditionSafeToResume)
	if suspended.Reason != reasonAdminActivatedWithErrors ||
		!strings.Contains(suspended.Message, "run inventory unavailable") ||
		safe.Status != metav1.ConditionFalse {
		t.Fatalf("run-list failure was not reported conservatively at deadline: %+v / %+v", suspended, safe)
	}
}

func TestReconcile_PreservesTerminalHistoryAndDistinguishesRemoteEvidence(t *testing.T) {
	base := time.Date(2026, 10, 26, 12, 8, 0, 0, time.UTC)

	t.Run("spoke target alone is not evidence", func(t *testing.T) {
		now := base
		config := testConfig(true)
		run := emergencyStoppedAgenticRun("target-only")
		run.Spec.TargetCluster = "spoke-a"
		original := run.DeepCopy()
		r, fc, _, _, _ := newTestReconciler(t, &now, config, run)

		_, err := reconcileOnce(r)
		if err != nil {
			t.Fatalf("Reconcile() error = %v", err)
		}
		gotConfig := getConfig(t, fc)
		suspended := requireCondition(t, gotConfig.Status.Conditions, agenticv1alpha1.AgenticOLSConfigConditionSuspended)
		safe := requireCondition(t, gotConfig.Status.Conditions, agenticv1alpha1.AgenticOLSConfigConditionSafeToResume)
		if suspended.Reason != reasonAdminActivated || safe.Status != metav1.ConditionTrue {
			t.Fatalf("spoke target alone blocked confirmed hub cleanup: %+v / %+v", suspended, safe)
		}
		if got := getRun(t, fc, run.Name); !runStatusHistoryEqual(got.Status, original.Status) {
			t.Fatalf("global cleanup changed terminal run history: got %+v, want %+v", got.Status, original.Status)
		}
	})

	t.Run("recorded remote sandbox remains unverified", func(t *testing.T) {
		now := base
		config := testConfig(true)
		run := completedAgenticRun("known-remote", "spoke-b")
		run.Status.Steps.Analysis.Sandbox = agenticv1alpha1.SandboxInfo{ClaimName: "known-claim", Namespace: "default"}
		original := run.DeepCopy()
		r, fc, _, _, _ := newTestReconciler(t, &now, config, run)

		_, err := reconcileOnce(r)
		if err != nil {
			t.Fatalf("first Reconcile() error = %v", err)
		}
		first := getConfig(t, fc)
		firstSuspended := requireCondition(t, first.Status.Conditions, agenticv1alpha1.AgenticOLSConfigConditionSuspended)
		firstSafe := requireCondition(t, first.Status.Conditions, agenticv1alpha1.AgenticOLSConfigConditionSafeToResume)
		if firstSuspended.Reason != reasonStopping || firstSafe.Status != metav1.ConditionFalse {
			t.Fatalf("known remote sandbox was treated as confirmed: %+v / %+v", firstSuspended, firstSafe)
		}

		now = base.Add(agenticrun.SuspensionStopWindow)
		_, err = reconcileOnce(r)
		if err != nil {
			t.Fatalf("deadline Reconcile() error = %v", err)
		}
		got := getConfig(t, fc)
		suspended := requireCondition(t, got.Status.Conditions, agenticv1alpha1.AgenticOLSConfigConditionSuspended)
		safe := requireCondition(t, got.Status.Conditions, agenticv1alpha1.AgenticOLSConfigConditionSafeToResume)
		if suspended.Reason != reasonAdminActivatedWithErrors ||
			!strings.Contains(suspended.Message, "known-claim") ||
			safe.Status != metav1.ConditionFalse {
			t.Fatalf("deadline failed to report unverified remote evidence: %+v / %+v", suspended, safe)
		}
		if gotRun := getRun(t, fc, run.Name); !runStatusHistoryEqual(gotRun.Status, original.Status) {
			t.Fatalf("global cleanup changed terminal run history: got %+v, want %+v", gotRun.Status, original.Status)
		}
	})
}

func TestReconcile_Deactivation(t *testing.T) {
	now := time.Date(2026, 10, 26, 12, 9, 0, 0, time.UTC)
	config := testConfig(false)
	config.Status.Conditions = []metav1.Condition{{
		Type:               agenticv1alpha1.AgenticOLSConfigConditionSuspended,
		Status:             metav1.ConditionTrue,
		Reason:             reasonAdminActivated,
		ObservedGeneration: config.Generation,
		LastTransitionTime: metav1.NewTime(now.Add(-time.Minute)),
	}}
	r, fc, recorder, _, _ := newTestReconciler(t, &now, config)

	_, err := reconcileOnce(r)
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	got := getConfig(t, fc)
	condition := requireCondition(t, got.Status.Conditions, agenticv1alpha1.AgenticOLSConfigConditionSuspended)
	if condition.Status != metav1.ConditionFalse || condition.Reason != reasonAdminDeactivated {
		t.Fatalf("Suspended = %s/%s, want False/%s", condition.Status, condition.Reason, reasonAdminDeactivated)
	}
	if condition.ObservedGeneration != got.Generation {
		t.Fatalf("deactivation observed generation = %d, want %d", condition.ObservedGeneration, got.Generation)
	}

	waitForEvent(t, recorder, eventReasonSuspensionDeactivated)
}

func TestReconcile_NoOpWhenActiveWithoutCondition(t *testing.T) {
	now := time.Date(2026, 10, 26, 12, 10, 0, 0, time.UTC)
	config := testConfig(false)
	r, fc, _, _, _ := newTestReconciler(t, &now, config)

	_, err := reconcileOnce(r)
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	got := getConfig(t, fc)
	if len(got.Status.Conditions) != 0 {
		t.Fatalf("expected no status change, got %v", got.Status.Conditions)
	}
}

func findCondition(conditions []metav1.Condition, condType string) *metav1.Condition {
	for i := range conditions {
		if conditions[i].Type == condType {
			return &conditions[i]
		}
	}
	return nil
}
