package agenticrun

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
	"github.com/openshift/lightspeed-agentic-operator/pkg/configuration"
)

func ttlTestCache(t *testing.T, days string) *configuration.Cache {
	t.Helper()
	cache := &configuration.Cache{}
	if err := cache.OnConfigMapChange(context.Background(), &corev1.ConfigMap{
		Data: map[string]string{configuration.KeyTerminalTTLDays: days},
	}); err != nil {
		t.Fatal(err)
	}
	return cache
}

func terminalTestRun() *agenticv1alpha1.AgenticRun {
	run := testAgenticRun()
	run.Status.Conditions = []metav1.Condition{{
		Type: agenticv1alpha1.AgenticRunConditionVerified, Status: metav1.ConditionTrue, Reason: "Complete",
	}}
	return run
}

func ttlTestReconciler(t *testing.T, run *agenticv1alpha1.AgenticRun, cache *configuration.Cache) *AgenticRunReconciler {
	t.Helper()
	objects := append([]client.Object{run}, defaultObjects()...)
	fc := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(objects...).WithStatusSubresource(run).Build()
	return &AgenticRunReconciler{Client: fc, Config: cache, Agent: newTestAgentCaller(), Namespace: "default"}
}

func TestHandleTerminalTTL_RecordsCappedDeadlineWithoutChangingSpec(t *testing.T) {
	for _, tc := range []struct {
		name     string
		request  *int32
		ceiling  string
		wantDays int
	}{
		{name: "omitted request", ceiling: "4", wantDays: 4},
		{name: "shorter request", request: ptr32(2), ceiling: "4", wantDays: 2},
		{name: "equal request", request: ptr32(4), ceiling: "4", wantDays: 4},
		{name: "longer request", request: ptr32(6), ceiling: "4", wantDays: 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := terminalTestRun()
			run.Spec.TerminalTTL = tc.request
			run.Generation = 5
			r := ttlTestReconciler(t, run, ttlTestCache(t, tc.ceiling))
			result, err := reconcileOnce(r, run.Name)
			if err != nil {
				t.Fatal(err)
			}
			got, err := getAgenticRun(r, run.Name)
			if err != nil {
				t.Fatal(err)
			}
			if got.Status.TerminalTime == nil || got.Status.DeleteAfter == nil {
				t.Fatalf("both terminal timestamps must be recorded: %#v", got.Status)
			}
			if want := time.Duration(tc.wantDays) * 24 * time.Hour; got.Status.DeleteAfter.Sub(got.Status.TerminalTime.Time) != want {
				t.Errorf("deadline interval = %v, want %v", got.Status.DeleteAfter.Sub(got.Status.TerminalTime.Time), want)
			}
			if got.Generation != 5 || (got.Spec.TerminalTTL == nil) != (tc.request == nil) || (tc.request != nil && *got.Spec.TerminalTTL != *tc.request) {
				t.Errorf("TTL processing changed spec/generation: spec=%+v generation=%d", got.Spec, got.Generation)
			}
			if got.Labels[terminalTTLLabel] != "true" {
				t.Errorf("terminal TTL label = %q, want true", got.Labels[terminalTTLLabel])
			}
			if result.RequeueAfter != 0 {
				t.Errorf("terminal run must not schedule its own expiry check: %+v", result)
			}
		})
	}
}

func TestReconcile_LabeledTerminalRunSkipsCleanup(t *testing.T) {
	run := terminalTestRun()
	run.Labels = map[string]string{terminalTTLLabel: "true"}
	run.Status.TerminalTime = ptrTime(time.Now())
	run.Status.DeleteAfter = ptrTime(time.Now().Add(24 * time.Hour))
	run.Status.Steps.Analysis.Sandbox.ClaimName = "analysis-sandbox"
	caller := newTestAgentCaller()
	r := ttlTestReconciler(t, run, nil)
	r.Agent = caller

	if _, err := reconcileOnce(r, run.Name); err != nil {
		t.Fatal(err)
	}
	if caller.releaseAllCount != 0 {
		t.Errorf("terminal cleanup was repeated %d times", caller.releaseAllCount)
	}
}

func TestReconcile_PreLabeledRunReceivesTerminalDeadline(t *testing.T) {
	run := terminalTestRun()
	run.Labels = map[string]string{terminalTTLLabel: "true"}
	r := ttlTestReconciler(t, run, ttlTestCache(t, "14"))

	if _, err := reconcileOnce(r, run.Name); err != nil {
		t.Fatal(err)
	}
	got, err := getAgenticRun(r, run.Name)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status.TerminalTime == nil || got.Status.DeleteAfter == nil {
		t.Errorf("pre-labeled run should receive a terminal deadline: status=%+v", got.Status)
	}
}

func TestHandleTerminalTTL_UsesRecordedDeadlineAfterConfigChanges(t *testing.T) {
	run := terminalTestRun()
	cache := ttlTestCache(t, "3")
	r := ttlTestReconciler(t, run, cache)
	if _, err := reconcileOnce(r, run.Name); err != nil {
		t.Fatal(err)
	}
	first, err := getAgenticRun(r, run.Name)
	if err != nil {
		t.Fatal(err)
	}
	deadline, terminalTime := *first.Status.DeleteAfter, *first.Status.TerminalTime
	if err := cache.OnConfigMapChange(context.Background(), &corev1.ConfigMap{Data: map[string]string{configuration.KeyTerminalTTLDays: "1"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := reconcileOnce(r, run.Name); err != nil {
		t.Fatal(err)
	}
	after, err := getAgenticRun(r, run.Name)
	if err != nil {
		t.Fatal(err)
	}
	if !after.Status.DeleteAfter.Equal(&deadline) || !after.Status.TerminalTime.Equal(&terminalTime) {
		t.Error("later configuration change moved recorded deadline")
	}
}

func TestSweepExpiredRuns_OnlyDeletesLabeledExpiredRuns(t *testing.T) {
	expired := terminalTestRun()
	expired.Status.DeleteAfter = ptrTime(time.Now().Add(-time.Hour))
	expired.Labels = map[string]string{terminalTTLLabel: "true"}

	future := terminalTestRun()
	future.Name = "future"
	future.Status.DeleteAfter = ptrTime(time.Now().Add(time.Hour))
	future.Labels = map[string]string{terminalTTLLabel: "true"}

	unlabeled := terminalTestRun()
	unlabeled.Name = "unlabeled"
	unlabeled.Status.DeleteAfter = ptrTime(time.Now().Add(-time.Hour))

	r := ttlTestReconciler(t, expired, nil)
	if err := r.Create(context.Background(), future); err != nil {
		t.Fatal(err)
	}
	if err := r.Create(context.Background(), unlabeled); err != nil {
		t.Fatal(err)
	}
	if err := r.sweepExpiredRuns(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name       string
		wantDelete bool
	}{
		{expired.Name, true}, {future.Name, false}, {unlabeled.Name, false},
	} {
		var got agenticv1alpha1.AgenticRun
		err := r.Get(context.Background(), types.NamespacedName{Name: tc.name, Namespace: expired.Namespace}, &got)
		if tc.wantDelete {
			if err == nil && got.DeletionTimestamp.IsZero() {
				t.Errorf("%s should be deleted", tc.name)
			}
		} else if err != nil || !got.DeletionTimestamp.IsZero() {
			t.Errorf("%s should remain, err=%v", tc.name, err)
		}
	}
}

func ptrTime(t time.Time) *metav1.Time {
	v := metav1.NewTime(t)
	return &v
}

func TestReconcile_MissingConfigReturnsRetryableError(t *testing.T) {
	for _, tc := range []struct {
		name  string
		cache *configuration.Cache
		run   func() *agenticv1alpha1.AgenticRun
	}{
		{name: "pending run with nil cache", run: testAgenticRun},
		{name: "terminal run with unloaded cache", cache: &configuration.Cache{}, run: terminalTestRun},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := tc.run()
			r := ttlTestReconciler(t, run, tc.cache)
			result, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(run)})
			if err == nil || err.Error() != "operator configuration not yet available" {
				t.Fatalf("expected configuration-not-ready error, got %v", err)
			}
			got, err := getAgenticRun(r, run.Name)
			if err != nil {
				t.Fatal(err)
			}
			if got.Status.TerminalTime != nil || got.Status.DeleteAfter != nil || got.Labels[terminalTTLLabel] != "" {
				t.Error("deadline and label should wait for configuration")
			}
			if result.RequeueAfter != 0 {
				t.Errorf("missing configuration must not schedule a fixed timer: %+v", result)
			}
		})
	}
}

func TestReconcile_PreserveAnnotationDoesNotDisableCompletedTTL(t *testing.T) {
	run := terminalTestRun()
	run.Annotations = map[string]string{preserveSandboxAnnotation: "true"}
	r := ttlTestReconciler(t, run, ttlTestCache(t, "14"))

	if _, err := reconcileOnce(r, run.Name); err != nil {
		t.Fatal(err)
	}
	got, err := getAgenticRun(r, run.Name)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status.TerminalTime == nil || got.Status.DeleteAfter == nil || got.Labels[terminalTTLLabel] != "true" {
		t.Errorf("completed run should receive a deadline despite preservation annotation: status=%+v labels=%v", got.Status, got.Labels)
	}
}

func TestHandleTerminalTTL_PreservedFailedSandboxDisablesAutoDeletion(t *testing.T) {
	run := testAgenticRun()
	run.Annotations = map[string]string{preserveSandboxAnnotation: "true"}
	run.Status.Conditions = []metav1.Condition{{
		Type: agenticv1alpha1.AgenticRunConditionAnalyzed, Status: metav1.ConditionFalse, Reason: reasonFailed,
	}}
	r := ttlTestReconciler(t, run, nil)
	result, err := reconcileOnce(r, run.Name)
	if err != nil {
		t.Fatal(err)
	}
	got, err := getAgenticRun(r, run.Name)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status.TerminalTime != nil || got.Status.DeleteAfter != nil || got.Labels[terminalTTLLabel] != "" || result.RequeueAfter != 0 {
		t.Errorf("preserved run should skip terminal cleanup: %#v, %+v", got.Status, result)
	}
}
