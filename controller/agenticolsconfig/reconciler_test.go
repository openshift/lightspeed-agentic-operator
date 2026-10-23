package agenticolsconfig

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
	"github.com/openshift/lightspeed-agentic-operator/controller/agenticrun"
)

type fakeSweeper struct {
	confirmed bool
	err       error
	calls     int
	observe   func()
}

func (f *fakeSweeper) SweepCluster(_ context.Context, _ []agenticv1alpha1.AgenticRun) (bool, error) {
	f.calls++
	if f.observe != nil {
		f.observe()
	}
	return f.confirmed, f.err
}

func testScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	utilruntime.Must(agenticv1alpha1.AddToScheme(s))
	utilruntime.Must(corev1.AddToScheme(s))
	return s
}

func testConfig(suspended bool) *agenticv1alpha1.AgenticOLSConfig {
	return &agenticv1alpha1.AgenticOLSConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster"},
		Spec:       agenticv1alpha1.AgenticOLSConfigSpec{Suspended: suspended},
	}
}

func emergencyStoppedAgenticRun(name string) *agenticv1alpha1.AgenticRun {
	return &agenticv1alpha1.AgenticRun{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: agenticv1alpha1.AgenticRunSpec{
			Request: "test",
		},
		Status: agenticv1alpha1.AgenticRunStatus{
			Conditions: []metav1.Condition{{
				Type:   agenticv1alpha1.AgenticRunConditionEmergencyStopped,
				Status: metav1.ConditionTrue,
				Reason: "SystemSuspended",
			}},
		},
	}
}

func pendingAgenticRun(name string) *agenticv1alpha1.AgenticRun {
	return &agenticv1alpha1.AgenticRun{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: agenticv1alpha1.AgenticRunSpec{
			Request: "test",
		},
	}
}

func stoppingAgenticRun(name, targetCluster string) *agenticv1alpha1.AgenticRun {
	run := pendingAgenticRun(name)
	run.Spec.TargetCluster = targetCluster
	run.Status.Conditions = []metav1.Condition{{
		Type:   agenticv1alpha1.AgenticRunConditionStopping,
		Status: metav1.ConditionTrue,
		Reason: "SystemSuspended",
	}}
	return run
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

func waitForEvent(t *testing.T, recorder *record.FakeRecorder, wantReason string) string {
	t.Helper()
	select {
	case event := <-recorder.Events:
		if !strings.Contains(event, wantReason) {
			t.Fatalf("event = %q, want reason %q", event, wantReason)
		}
		return event
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for event with reason %q", wantReason)
		return ""
	}
}

func TestReconcile_NotFound(t *testing.T) {
	fc := fake.NewClientBuilder().WithScheme(testScheme()).Build()
	recorder := record.NewFakeRecorder(1)
	r := &Reconciler{Client: fc, EventRecorder: recorder}

	_, err := reconcileOnce(r)
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
}

func TestReconcile_Activation(t *testing.T) {
	config := testConfig(true)
	run := emergencyStoppedAgenticRun("stopped-1")
	fc := fake.NewClientBuilder().
		WithScheme(testScheme()).
		WithObjects(config, run).
		WithStatusSubresource(&agenticv1alpha1.AgenticOLSConfig{}).
		Build()
	recorder := record.NewFakeRecorder(2)
	sweeper := &fakeSweeper{confirmed: true}
	r := &Reconciler{Client: fc, Sandbox: sweeper, EventRecorder: recorder}

	result, err := reconcileOnce(r)
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if result.Requeue || result.RequeueAfter != 0 {
		t.Fatalf("expected no requeue when all runs are terminal, got %+v", result)
	}
	if sweeper.calls != 1 {
		t.Fatalf("sweep calls = %d, want 1", sweeper.calls)
	}

	got := getConfig(t, fc)
	cond := findCondition(got.Status.Conditions, agenticv1alpha1.AgenticOLSConfigConditionSuspended)
	if cond == nil {
		t.Fatal("Suspended condition not set")
	}
	if cond.Status != metav1.ConditionTrue {
		t.Fatalf("condition status = %q, want True", cond.Status)
	}
	if cond.Reason != reasonAdminActivated {
		t.Fatalf("condition reason = %q, want %q", cond.Reason, reasonAdminActivated)
	}
	wantMsg := "System suspended; 1 runs emergency-stopped"
	if cond.Message != wantMsg {
		t.Fatalf("condition message = %q, want %q", cond.Message, wantMsg)
	}

	event := waitForEvent(t, recorder, eventReasonSuspensionActivated)
	if !strings.Contains(event, "stopping 0 active sandbox workloads") {
		t.Fatalf("event = %q, want activation message", event)
	}
}

func TestReconcile_ActivationPendingAgenticRuns(t *testing.T) {
	config := testConfig(true)
	run := pendingAgenticRun("pending-1")
	fc := fake.NewClientBuilder().
		WithScheme(testScheme()).
		WithObjects(config, run).
		WithStatusSubresource(&agenticv1alpha1.AgenticOLSConfig{}).
		Build()
	recorder := record.NewFakeRecorder(2)
	r := &Reconciler{Client: fc, Sandbox: &fakeSweeper{confirmed: true}, EventRecorder: recorder}

	result, err := reconcileOnce(r)
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if result.RequeueAfter != requeueDelay {
		t.Fatalf("RequeueAfter = %s, want %s", result.RequeueAfter, requeueDelay)
	}

	got := getConfig(t, fc)
	cond := findCondition(got.Status.Conditions, agenticv1alpha1.AgenticOLSConfigConditionSuspended)
	if cond == nil {
		t.Fatal("Suspended condition not set during stopping")
	}
	if cond.Status != metav1.ConditionTrue {
		t.Fatalf("condition status = %q, want True", cond.Status)
	}
	if cond.Reason != reasonStopping {
		t.Fatalf("condition reason = %q, want %q", cond.Reason, reasonStopping)
	}
	wantMsg := "Stopping system; 1 runs still stopping"
	if cond.Message != wantMsg {
		t.Fatalf("condition message = %q, want %q", cond.Message, wantMsg)
	}
}

func TestReconcile_SetsStoppingBeforeSweep(t *testing.T) {
	config := testConfig(true)
	run := pendingAgenticRun("pending-1")
	fc := fake.NewClientBuilder().
		WithScheme(testScheme()).
		WithObjects(config, run).
		WithStatusSubresource(&agenticv1alpha1.AgenticOLSConfig{}).
		Build()
	recorder := record.NewFakeRecorder(2)
	var observedReason string
	sweeper := &fakeSweeper{
		confirmed: true,
		observe: func() {
			observedReason = findCondition(getConfig(t, fc).Status.Conditions, agenticv1alpha1.AgenticOLSConfigConditionSuspended).Reason
		},
	}
	r := &Reconciler{Client: fc, Sandbox: sweeper, EventRecorder: recorder}

	if _, err := reconcileOnce(r); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if observedReason != reasonStopping {
		t.Fatalf("condition during sweep = %q, want %q", observedReason, reasonStopping)
	}
}

func TestReconcile_ActivationSweepPending(t *testing.T) {
	config := testConfig(true)
	run := emergencyStoppedAgenticRun("stopped-1")
	fc := fake.NewClientBuilder().
		WithScheme(testScheme()).
		WithObjects(config, run).
		WithStatusSubresource(&agenticv1alpha1.AgenticOLSConfig{}).
		Build()
	recorder := record.NewFakeRecorder(2)
	r := &Reconciler{Client: fc, Sandbox: &fakeSweeper{confirmed: false}, EventRecorder: recorder}

	result, err := reconcileOnce(r)
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if result.RequeueAfter != requeueDelay {
		t.Fatalf("RequeueAfter = %s, want %s", result.RequeueAfter, requeueDelay)
	}
	cond := findCondition(getConfig(t, fc).Status.Conditions, agenticv1alpha1.AgenticOLSConfigConditionSuspended)
	if cond == nil || cond.Reason != reasonStopping {
		t.Fatalf("condition = %+v, want Stopping", cond)
	}
}

func TestReconcile_DegradesWithStoppedRunOnFailedSpoke(t *testing.T) {
	config := testConfig(true)
	run := stoppingAgenticRun("stopped-1", "spoke-a")
	fc := fake.NewClientBuilder().
		WithScheme(testScheme()).
		WithObjects(config, run).
		WithStatusSubresource(&agenticv1alpha1.AgenticOLSConfig{}).
		Build()
	recorder := record.NewFakeRecorder(3)
	spokeErr := &agenticrun.SpokeStopError{Cluster: "spoke-a", Kind: agenticrun.SpokeStopGone, Err: errors.New("missing kubeconfig")}
	r := &Reconciler{Client: fc, Sandbox: &fakeSweeper{confirmed: false, err: spokeErr}, EventRecorder: recorder}

	result, err := reconcileOnce(r)
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if result.RequeueAfter != degradedRequeueWait {
		t.Fatalf("RequeueAfter = %s, want %s", result.RequeueAfter, degradedRequeueWait)
	}
	cond := findCondition(getConfig(t, fc).Status.Conditions, agenticv1alpha1.AgenticOLSConfigConditionSuspended)
	if cond == nil || cond.Reason != reasonAdminActivatedWithErrors {
		t.Fatalf("condition = %+v, want %s", cond, reasonAdminActivatedWithErrors)
	}
	if !strings.Contains(cond.Message, "stopped-1") || !strings.Contains(cond.Message, "spoke-a") {
		t.Fatalf("condition message = %q, want run and spoke names", cond.Message)
	}
	waitForEvent(t, recorder, eventReasonSuspensionActivated)
	waitForEvent(t, recorder, eventReasonSuspensionCompletedWithErrors)
}

func TestReconcile_StoppedRunOnHealthySpokeKeepsStopping(t *testing.T) {
	config := testConfig(true)
	run := stoppingAgenticRun("stopped-1", "spoke-a")
	fc := fake.NewClientBuilder().
		WithScheme(testScheme()).
		WithObjects(config, run).
		WithStatusSubresource(&agenticv1alpha1.AgenticOLSConfig{}).
		Build()
	spokeErr := &agenticrun.SpokeStopError{Cluster: "spoke-b", Kind: agenticrun.SpokeStopGone, Err: errors.New("missing kubeconfig")}
	r := &Reconciler{Client: fc, Sandbox: &fakeSweeper{confirmed: false, err: spokeErr}, EventRecorder: record.NewFakeRecorder(2)}

	result, err := reconcileOnce(r)
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if result.RequeueAfter != requeueDelay {
		t.Fatalf("RequeueAfter = %s, want %s", result.RequeueAfter, requeueDelay)
	}
	cond := findCondition(getConfig(t, fc).Status.Conditions, agenticv1alpha1.AgenticOLSConfigConditionSuspended)
	if cond == nil || cond.Reason != reasonStopping {
		t.Fatalf("condition = %+v, want %s", cond, reasonStopping)
	}
}

func TestReconcile_NonStoppingRunOnFailedSpokeKeepsStopping(t *testing.T) {
	config := testConfig(true)
	run := pendingAgenticRun("pending-1")
	run.Spec.TargetCluster = "spoke-a"
	fc := fake.NewClientBuilder().
		WithScheme(testScheme()).
		WithObjects(config, run).
		WithStatusSubresource(&agenticv1alpha1.AgenticOLSConfig{}).
		Build()
	spokeErr := &agenticrun.SpokeStopError{Cluster: "spoke-a", Kind: agenticrun.SpokeStopGone, Err: errors.New("missing kubeconfig")}
	r := &Reconciler{Client: fc, Sandbox: &fakeSweeper{confirmed: false, err: spokeErr}, EventRecorder: record.NewFakeRecorder(2)}

	result, err := reconcileOnce(r)
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if result.RequeueAfter != requeueDelay {
		t.Fatalf("RequeueAfter = %s, want %s", result.RequeueAfter, requeueDelay)
	}
	cond := findCondition(getConfig(t, fc).Status.Conditions, agenticv1alpha1.AgenticOLSConfigConditionSuspended)
	if cond == nil || cond.Reason != reasonStopping {
		t.Fatalf("condition = %+v, want %s", cond, reasonStopping)
	}
}

func TestReconcile_FailedSpokeWithoutRunDegrades(t *testing.T) {
	config := testConfig(true)
	fc := fake.NewClientBuilder().
		WithScheme(testScheme()).
		WithObjects(config).
		WithStatusSubresource(&agenticv1alpha1.AgenticOLSConfig{}).
		Build()
	spokeErr := &agenticrun.SpokeStopError{Cluster: "spoke-a", Kind: agenticrun.SpokeStopGone, Err: errors.New("missing kubeconfig")}
	r := &Reconciler{Client: fc, Sandbox: &fakeSweeper{confirmed: false, err: spokeErr}, EventRecorder: record.NewFakeRecorder(2)}

	result, err := reconcileOnce(r)
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if result.RequeueAfter != degradedRequeueWait {
		t.Fatalf("RequeueAfter = %s, want %s", result.RequeueAfter, degradedRequeueWait)
	}
	cond := findCondition(getConfig(t, fc).Status.Conditions, agenticv1alpha1.AgenticOLSConfigConditionSuspended)
	if cond == nil || cond.Reason != reasonAdminActivatedWithErrors {
		t.Fatalf("condition = %+v, want %s", cond, reasonAdminActivatedWithErrors)
	}
}

func TestReconcile_SpokeFailureDegradesAfterWindow(t *testing.T) {
	config := testConfig(true)
	config.Status.Conditions = []metav1.Condition{{
		Type:               agenticv1alpha1.AgenticOLSConfigConditionSuspended,
		Status:             metav1.ConditionTrue,
		Reason:             reasonStopping,
		Message:            "Stopping system; 0 runs still stopping",
		LastTransitionTime: metav1.NewTime(time.Now().Add(-2 * agenticrun.SpokeRetryWindow)),
	}}
	fc := fake.NewClientBuilder().
		WithScheme(testScheme()).
		WithObjects(config).
		WithStatusSubresource(&agenticv1alpha1.AgenticOLSConfig{}).
		Build()
	recorder := record.NewFakeRecorder(3)
	spokeErr := &agenticrun.SpokeStopError{Cluster: "spoke-a", Kind: agenticrun.SpokeStopUnreachable, Err: errors.New("dial tcp: i/o timeout")}
	r := &Reconciler{Client: fc, Sandbox: &fakeSweeper{confirmed: false, err: spokeErr}, EventRecorder: recorder}

	result, err := reconcileOnce(r)
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if result.RequeueAfter != degradedRequeueWait {
		t.Fatalf("RequeueAfter = %s, want %s", result.RequeueAfter, degradedRequeueWait)
	}
	cond := findCondition(getConfig(t, fc).Status.Conditions, agenticv1alpha1.AgenticOLSConfigConditionSuspended)
	if cond == nil || cond.Reason != reasonAdminActivatedWithErrors {
		t.Fatalf("condition = %+v, want %s", cond, reasonAdminActivatedWithErrors)
	}
	if !strings.Contains(cond.Message, "spoke-a") {
		t.Fatalf("condition message = %q, want spoke name", cond.Message)
	}
	event := waitForEvent(t, recorder, eventReasonSuspensionCompletedWithErrors)
	if !strings.Contains(event, "spoke-a") {
		t.Fatalf("event = %q, want spoke name", event)
	}
}

func TestReconcile_SpokeGoneDegradesImmediately(t *testing.T) {
	config := testConfig(true)
	config.Status.Conditions = []metav1.Condition{{
		Type:               agenticv1alpha1.AgenticOLSConfigConditionSuspended,
		Status:             metav1.ConditionTrue,
		Reason:             reasonStopping,
		LastTransitionTime: metav1.NewTime(time.Now()),
	}}
	fc := fake.NewClientBuilder().
		WithScheme(testScheme()).
		WithObjects(config).
		WithStatusSubresource(&agenticv1alpha1.AgenticOLSConfig{}).
		Build()
	recorder := record.NewFakeRecorder(3)
	spokeErr := &agenticrun.SpokeStopError{Cluster: "spoke-a", Kind: agenticrun.SpokeStopGone, Err: errors.New("missing kubeconfig")}
	r := &Reconciler{Client: fc, Sandbox: &fakeSweeper{confirmed: false, err: spokeErr}, EventRecorder: recorder}

	result, err := reconcileOnce(r)
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if result.RequeueAfter != degradedRequeueWait {
		t.Fatalf("RequeueAfter = %s, want %s", result.RequeueAfter, degradedRequeueWait)
	}
	cond := findCondition(getConfig(t, fc).Status.Conditions, agenticv1alpha1.AgenticOLSConfigConditionSuspended)
	if cond == nil || cond.Reason != reasonAdminActivatedWithErrors {
		t.Fatalf("condition = %+v, want %s", cond, reasonAdminActivatedWithErrors)
	}
}

func TestReconcile_UnreachableSpokeWindowStartsAtStopping(t *testing.T) {
	config := testConfig(true)
	fc := fake.NewClientBuilder().
		WithScheme(testScheme()).
		WithObjects(config).
		WithStatusSubresource(&agenticv1alpha1.AgenticOLSConfig{}).
		Build()
	start := time.Now().Truncate(time.Second)
	recorder := record.NewFakeRecorder(4)
	spokeErr := &agenticrun.SpokeStopError{Cluster: "spoke-a", Kind: agenticrun.SpokeStopUnreachable, Err: errors.New("dial tcp: i/o timeout")}
	r := &Reconciler{
		Client:        fc,
		Sandbox:       &fakeSweeper{confirmed: false, err: spokeErr},
		EventRecorder: recorder,
		Now:           func() time.Time { return start },
	}

	if _, err := reconcileOnce(r); err != nil {
		t.Fatalf("first Reconcile() error = %v", err)
	}
	cond := findCondition(getConfig(t, fc).Status.Conditions, agenticv1alpha1.AgenticOLSConfigConditionSuspended)
	if cond == nil || cond.Reason != reasonStopping {
		t.Fatalf("condition = %+v, want Stopping", cond)
	}
	if !cond.LastTransitionTime.Time.Equal(start) {
		t.Fatalf("Stopping transition time = %s, want %s", cond.LastTransitionTime.Time, start)
	}

	r.Now = func() time.Time { return start.Add(5 * time.Second) }
	result, err := reconcileOnce(r)
	if err != nil {
		t.Fatalf("second Reconcile() error = %v", err)
	}
	if result.RequeueAfter != requeueDelay {
		t.Fatalf("RequeueAfter after 5s = %s, want %s", result.RequeueAfter, requeueDelay)
	}

	r.Now = func() time.Time { return start.Add(agenticrun.SpokeRetryWindow + time.Second) }
	result, err = reconcileOnce(r)
	if err != nil {
		t.Fatalf("third Reconcile() error = %v", err)
	}
	if result.RequeueAfter != degradedRequeueWait {
		t.Fatalf("RequeueAfter after window = %s, want %s", result.RequeueAfter, degradedRequeueWait)
	}
	cond = findCondition(getConfig(t, fc).Status.Conditions, agenticv1alpha1.AgenticOLSConfigConditionSuspended)
	if cond == nil || cond.Reason != reasonAdminActivatedWithErrors {
		t.Fatalf("condition = %+v, want %s", cond, reasonAdminActivatedWithErrors)
	}
}

func TestReconcile_SpokeFailureWaitsForWindow(t *testing.T) {
	config := testConfig(true)
	config.Status.Conditions = []metav1.Condition{{
		Type:               agenticv1alpha1.AgenticOLSConfigConditionSuspended,
		Status:             metav1.ConditionTrue,
		Reason:             reasonStopping,
		LastTransitionTime: metav1.NewTime(time.Now()),
	}}
	fc := fake.NewClientBuilder().
		WithScheme(testScheme()).
		WithObjects(config).
		WithStatusSubresource(&agenticv1alpha1.AgenticOLSConfig{}).
		Build()
	recorder := record.NewFakeRecorder(3)
	spokeErr := &agenticrun.SpokeStopError{Cluster: "spoke-a", Kind: agenticrun.SpokeStopUnreachable, Err: errors.New("dial tcp: i/o timeout")}
	r := &Reconciler{Client: fc, Sandbox: &fakeSweeper{confirmed: false, err: spokeErr}, EventRecorder: recorder}

	result, err := reconcileOnce(r)
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if result.RequeueAfter != requeueDelay {
		t.Fatalf("RequeueAfter = %s, want %s", result.RequeueAfter, requeueDelay)
	}
	cond := findCondition(getConfig(t, fc).Status.Conditions, agenticv1alpha1.AgenticOLSConfigConditionSuspended)
	if cond == nil || cond.Reason != reasonStopping {
		t.Fatalf("condition = %+v, want Stopping", cond)
	}
}

func TestReconcile_NonDegradeableErrorStaysStopping(t *testing.T) {
	config := testConfig(true)
	config.Status.Conditions = []metav1.Condition{{
		Type:               agenticv1alpha1.AgenticOLSConfigConditionSuspended,
		Status:             metav1.ConditionTrue,
		Reason:             reasonStopping,
		LastTransitionTime: metav1.NewTime(time.Now().Add(-2 * agenticrun.SpokeRetryWindow)),
	}}
	fc := fake.NewClientBuilder().
		WithScheme(testScheme()).
		WithObjects(config).
		WithStatusSubresource(&agenticv1alpha1.AgenticOLSConfig{}).
		Build()
	recorder := record.NewFakeRecorder(3)
	r := &Reconciler{Client: fc, Sandbox: &fakeSweeper{confirmed: false, err: errors.New("forbidden")}, EventRecorder: recorder}

	if _, err := reconcileOnce(r); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	cond := findCondition(getConfig(t, fc).Status.Conditions, agenticv1alpha1.AgenticOLSConfigConditionSuspended)
	if cond == nil || cond.Reason != reasonStopping {
		t.Fatalf("condition = %+v, want Stopping", cond)
	}
}

func TestReconcile_DegradedUpgradesWhenClean(t *testing.T) {
	config := testConfig(true)
	config.Status.Conditions = []metav1.Condition{{
		Type:   agenticv1alpha1.AgenticOLSConfigConditionSuspended,
		Status: metav1.ConditionTrue,
		Reason: reasonAdminActivatedWithErrors,
	}}
	fc := fake.NewClientBuilder().
		WithScheme(testScheme()).
		WithObjects(config).
		WithStatusSubresource(&agenticv1alpha1.AgenticOLSConfig{}).
		Build()
	recorder := record.NewFakeRecorder(3)
	r := &Reconciler{Client: fc, Sandbox: &fakeSweeper{confirmed: true}, EventRecorder: recorder}

	if _, err := reconcileOnce(r); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	cond := findCondition(getConfig(t, fc).Status.Conditions, agenticv1alpha1.AgenticOLSConfigConditionSuspended)
	if cond == nil || cond.Reason != reasonAdminActivated {
		t.Fatalf("condition = %+v, want %s", cond, reasonAdminActivated)
	}
}

func TestReconcile_Deactivation(t *testing.T) {
	config := testConfig(false)
	config.Status.Conditions = []metav1.Condition{{
		Type:   agenticv1alpha1.AgenticOLSConfigConditionSuspended,
		Status: metav1.ConditionTrue,
		Reason: reasonAdminActivated,
	}}
	fc := fake.NewClientBuilder().
		WithScheme(testScheme()).
		WithObjects(config).
		WithStatusSubresource(&agenticv1alpha1.AgenticOLSConfig{}).
		Build()
	recorder := record.NewFakeRecorder(2)
	r := &Reconciler{Client: fc, EventRecorder: recorder}

	_, err := reconcileOnce(r)
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	got := getConfig(t, fc)
	cond := findCondition(got.Status.Conditions, agenticv1alpha1.AgenticOLSConfigConditionSuspended)
	if cond == nil {
		t.Fatal("Suspended condition missing")
	}
	if cond.Status != metav1.ConditionFalse {
		t.Fatalf("condition status = %q, want False", cond.Status)
	}
	if cond.Reason != reasonAdminDeactivated {
		t.Fatalf("condition reason = %q, want %q", cond.Reason, reasonAdminDeactivated)
	}

	waitForEvent(t, recorder, eventReasonSuspensionDeactivated)
}

func TestReconcile_DeactivationFromDegraded(t *testing.T) {
	config := testConfig(false)
	config.Status.Conditions = []metav1.Condition{{
		Type:   agenticv1alpha1.AgenticOLSConfigConditionSuspended,
		Status: metav1.ConditionTrue,
		Reason: reasonAdminActivatedWithErrors,
	}}
	fc := fake.NewClientBuilder().
		WithScheme(testScheme()).
		WithObjects(config).
		WithStatusSubresource(&agenticv1alpha1.AgenticOLSConfig{}).
		Build()
	recorder := record.NewFakeRecorder(2)
	r := &Reconciler{Client: fc, EventRecorder: recorder}

	if _, err := reconcileOnce(r); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	cond := findCondition(getConfig(t, fc).Status.Conditions, agenticv1alpha1.AgenticOLSConfigConditionSuspended)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != reasonAdminDeactivated {
		t.Fatalf("condition = %+v, want AdminDeactivated", cond)
	}
}

func TestReconcile_DeactivationDeniedWhileStopping(t *testing.T) {
	config := testConfig(false)
	config.Status.Conditions = []metav1.Condition{{
		Type:   agenticv1alpha1.AgenticOLSConfigConditionSuspended,
		Status: metav1.ConditionTrue,
		Reason: reasonStopping,
	}}
	fc := fake.NewClientBuilder().
		WithScheme(testScheme()).
		WithObjects(config).
		WithStatusSubresource(&agenticv1alpha1.AgenticOLSConfig{}).
		Build()
	recorder := record.NewFakeRecorder(2)
	r := &Reconciler{Client: fc, EventRecorder: recorder}

	if _, err := reconcileOnce(r); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	cond := findCondition(getConfig(t, fc).Status.Conditions, agenticv1alpha1.AgenticOLSConfigConditionSuspended)
	if cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != reasonStopping {
		t.Fatalf("condition = %+v, want still Stopping", cond)
	}
	waitForEvent(t, recorder, eventReasonSuspensionDeactivationDenied)
}

func TestReconcile_NoOpWhenActiveWithoutCondition(t *testing.T) {
	config := testConfig(false)
	fc := fake.NewClientBuilder().
		WithScheme(testScheme()).
		WithObjects(config).
		WithStatusSubresource(&agenticv1alpha1.AgenticOLSConfig{}).
		Build()
	recorder := record.NewFakeRecorder(1)
	r := &Reconciler{Client: fc, EventRecorder: recorder}

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
