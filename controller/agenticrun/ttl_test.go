package agenticrun

import (
	"context"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
)

// ptr32 is defined in helpers_test.go

func TestGetTerminalTTL(t *testing.T) {
	tests := []struct {
		name    string
		objects []client.Object
		want    *int32
	}{
		{
			name:    "no config CR returns nil",
			objects: nil,
			want:    nil,
		},
		{
			name: "config without lifecycle returns nil",
			objects: []client.Object{&agenticv1alpha1.AgenticOLSConfig{
				ObjectMeta: metav1.ObjectMeta{Name: "cluster"},
				Spec:       agenticv1alpha1.AgenticOLSConfigSpec{},
			}},
			want: nil,
		},
		{
			name: "config with terminalTTL returns value",
			objects: []client.Object{&agenticv1alpha1.AgenticOLSConfig{
				ObjectMeta: metav1.ObjectMeta{Name: "cluster"},
				Spec: agenticv1alpha1.AgenticOLSConfigSpec{
					Lifecycle: agenticv1alpha1.LifecycleConfig{TerminalTTL: ptr32(3600)},
				},
			}},
			want: ptr32(3600),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			objects := tt.objects
			if objects == nil {
				objects = []client.Object{}
			}
			fc := fake.NewClientBuilder().
				WithScheme(testScheme()).
				WithObjects(objects...).
				Build()
			got, err := getTerminalTTL(context.Background(), fc)
			if err != nil {
				t.Fatalf("getTerminalTTL() error = %v", err)
			}
			if (got == nil) != (tt.want == nil) {
				t.Fatalf("getTerminalTTL() = %v, want %v", got, tt.want)
			}
			if got != nil && *got != *tt.want {
				t.Fatalf("getTerminalTTL() = %d, want %d", *got, *tt.want)
			}
		})
	}
}

func TestHandleTerminalTTL_StampsTerminalTimeAndTTL(t *testing.T) {
	config := &agenticv1alpha1.AgenticOLSConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster"},
		Spec: agenticv1alpha1.AgenticOLSConfigSpec{
			Lifecycle: agenticv1alpha1.LifecycleConfig{TerminalTTL: ptr32(3600)},
		},
	}

	run := testAgenticRun()
	run.Status.Conditions = []metav1.Condition{{
		Type:   agenticv1alpha1.AgenticRunConditionVerified,
		Status: metav1.ConditionTrue,
		Reason: "Complete",
	}}

	objs := append([]client.Object{run, config}, defaultObjects()...)
	fc := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(objs...).
		WithStatusSubresource(run).Build()

	r := &AgenticRunReconciler{Client: fc, Agent: newTestAgentCaller(), Namespace: "default"}

	result, err := reconcileOnce(r, "fix-crash")
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	got, getErr := getAgenticRun(r, "fix-crash")
	if getErr != nil {
		t.Fatalf("getAgenticRun: %v", getErr)
	}

	if got.Status.TerminalTime == nil {
		t.Fatal("terminalTime should be stamped")
	}
	if got.Spec.TTLAfterTerminal == nil {
		t.Fatal("ttlAfterTerminal should be stamped from config")
	}
	if *got.Spec.TTLAfterTerminal != 3600 {
		t.Errorf("ttlAfterTerminal = %d, want 3600", *got.Spec.TTLAfterTerminal)
	}
	if result.RequeueAfter <= 0 {
		t.Error("expected RequeueAfter > 0 for non-expired TTL")
	}

	stampedTerminalTime := got.Status.TerminalTime.DeepCopy()

	// A second reconcile of the same still-terminal run must not refresh
	// terminalTime -- otherwise expiry would perpetually postpone itself.
	if _, err := reconcileOnce(r, "fix-crash"); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	after, getErr := getAgenticRun(r, "fix-crash")
	if getErr != nil {
		t.Fatalf("getAgenticRun after second reconcile: %v", getErr)
	}
	if after.Status.TerminalTime == nil {
		t.Fatal("terminalTime should still be set after second reconcile")
	}
	if !after.Status.TerminalTime.Equal(stampedTerminalTime) {
		t.Errorf("terminalTime changed on second reconcile: got %v, want unchanged %v", after.Status.TerminalTime, stampedTerminalTime)
	}
}

func TestHandleTerminalTTL_PresetTTLNotOverwritten(t *testing.T) {
	config := &agenticv1alpha1.AgenticOLSConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster"},
		Spec: agenticv1alpha1.AgenticOLSConfigSpec{
			Lifecycle: agenticv1alpha1.LifecycleConfig{TerminalTTL: ptr32(3600)},
		},
	}

	run := testAgenticRun()
	run.Spec.TTLAfterTerminal = ptr32(7200) // pre-set by adapter
	run.Status.Conditions = []metav1.Condition{{
		Type:   agenticv1alpha1.AgenticRunConditionVerified,
		Status: metav1.ConditionTrue,
		Reason: "Complete",
	}}

	objs := append([]client.Object{run, config}, defaultObjects()...)
	fc := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(objs...).
		WithStatusSubresource(run).Build()

	r := &AgenticRunReconciler{Client: fc, Agent: newTestAgentCaller(), Namespace: "default"}

	_, err := reconcileOnce(r, "fix-crash")
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	got, getErr := getAgenticRun(r, "fix-crash")
	if getErr != nil {
		t.Fatalf("getAgenticRun: %v", getErr)
	}
	if got.Spec.TTLAfterTerminal == nil || *got.Spec.TTLAfterTerminal != 7200 {
		t.Errorf("ttlAfterTerminal = %v, want 7200 (pre-set should not be overwritten)", got.Spec.TTLAfterTerminal)
	}
}

func TestHandleTerminalTTL_ZeroDisablesAutoDeletion(t *testing.T) {
	run := testAgenticRun()
	run.Spec.TTLAfterTerminal = ptr32(0) // explicitly disable
	now := metav1.NewTime(time.Now().Add(-1 * time.Hour))
	run.Status.TerminalTime = &now
	run.Status.Conditions = []metav1.Condition{{
		Type:   agenticv1alpha1.AgenticRunConditionVerified,
		Status: metav1.ConditionTrue,
		Reason: "Complete",
	}}

	objs := append([]client.Object{run}, defaultObjects()...)
	fc := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(objs...).
		WithStatusSubresource(run).Build()

	r := &AgenticRunReconciler{Client: fc, Agent: newTestAgentCaller(), Namespace: "default"}

	result, err := reconcileOnce(r, "fix-crash")
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if result.RequeueAfter != 0 {
		t.Error("ttl=0 should not requeue")
	}

	// Run should still exist.
	got, getErr := getAgenticRun(r, "fix-crash")
	if getErr != nil {
		t.Fatalf("run should not be deleted when ttl=0: %v", getErr)
	}
	if got == nil {
		t.Fatal("run should still exist when ttl=0")
	}
}

func TestHandleTerminalTTL_ExpiredRunDeleted(t *testing.T) {
	run := testAgenticRun()
	run.Spec.TTLAfterTerminal = ptr32(60) // 60 seconds TTL
	pastTime := metav1.NewTime(time.Now().Add(-2 * time.Minute))
	run.Status.TerminalTime = &pastTime
	run.Status.Conditions = []metav1.Condition{{
		Type:   agenticv1alpha1.AgenticRunConditionVerified,
		Status: metav1.ConditionTrue,
		Reason: "Complete",
	}}

	objs := append([]client.Object{run}, defaultObjects()...)
	fc := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(objs...).
		WithStatusSubresource(run).Build()

	r := &AgenticRunReconciler{Client: fc, Agent: newTestAgentCaller(), Namespace: "default"}

	_, err := reconcileOnce(r, "fix-crash")
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	// Every non-deleting reconcile adds rbacCleanupFinalizer and
	// templogCleanupFinalizer before reaching the terminal-phase handling
	// (see the "Finalizers" block near the top of Reconcile), so by the
	// time handleTerminalTTL calls Delete, the run always has finalizers.
	// The fake client mirrors real Kubernetes semantics here: it keeps the
	// object with DeletionTimestamp set rather than removing it outright.
	// Assert that concrete outcome rather than accepting either, so a real
	// regression (e.g. Delete not called at all) can't slip through as
	// "acceptable."
	var updated agenticv1alpha1.AgenticRun
	getErr := fc.Get(context.Background(), types.NamespacedName{Name: "fix-crash", Namespace: "default"}, &updated)
	if getErr != nil {
		t.Fatalf("expired run should still exist with DeletionTimestamp set (finalizers present): %v", getErr)
	}
	if updated.DeletionTimestamp.IsZero() {
		t.Fatal("expired run should have DeletionTimestamp set")
	}
}

func TestHandleTerminalTTL_NotExpiredRequeues(t *testing.T) {
	run := testAgenticRun()
	run.Spec.TTLAfterTerminal = ptr32(3600) // 1 hour TTL
	now := metav1.NewTime(time.Now())
	run.Status.TerminalTime = &now
	run.Status.Conditions = []metav1.Condition{{
		Type:   agenticv1alpha1.AgenticRunConditionVerified,
		Status: metav1.ConditionTrue,
		Reason: "Complete",
	}}

	objs := append([]client.Object{run}, defaultObjects()...)
	fc := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(objs...).
		WithStatusSubresource(run).Build()

	r := &AgenticRunReconciler{Client: fc, Agent: newTestAgentCaller(), Namespace: "default"}

	result, err := reconcileOnce(r, "fix-crash")
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if result.RequeueAfter <= 0 {
		t.Error("non-expired TTL should requeue with remaining time")
	}
	if result.RequeueAfter > 1*time.Hour {
		t.Errorf("RequeueAfter = %v, should be <= 1h", result.RequeueAfter)
	}

	// Run should still exist.
	_, getErr := getAgenticRun(r, "fix-crash")
	if getErr != nil {
		t.Fatalf("non-expired run should still exist: %v", getErr)
	}
}

func TestHandleTerminalTTL_NoConfigNoAutoDeletion(t *testing.T) {
	// No AgenticOLSConfig CR exists — backwards-compatible, no auto-deletion.
	run := testAgenticRun()
	run.Status.Conditions = []metav1.Condition{{
		Type:   agenticv1alpha1.AgenticRunConditionVerified,
		Status: metav1.ConditionTrue,
		Reason: "Complete",
	}}

	objs := append([]client.Object{run}, defaultObjects()...)
	fc := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(objs...).
		WithStatusSubresource(run).Build()

	r := &AgenticRunReconciler{Client: fc, Agent: newTestAgentCaller(), Namespace: "default"}

	result, err := reconcileOnce(r, "fix-crash")
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if result.RequeueAfter != 0 || result.Requeue {
		t.Error("no config should not cause requeue for TTL")
	}

	// Run should still exist and have terminalTime stamped but no ttlAfterTerminal.
	got, getErr := getAgenticRun(r, "fix-crash")
	if getErr != nil {
		t.Fatalf("getAgenticRun: %v", getErr)
	}
	if got.Status.TerminalTime == nil {
		t.Error("terminalTime should still be stamped")
	}
	if got.Spec.TTLAfterTerminal != nil {
		t.Errorf("ttlAfterTerminal should be nil when no config, got %d", *got.Spec.TTLAfterTerminal)
	}
}

func TestHandleTerminalTTL_DeniedPhase(t *testing.T) {
	config := &agenticv1alpha1.AgenticOLSConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster"},
		Spec: agenticv1alpha1.AgenticOLSConfigSpec{
			Lifecycle: agenticv1alpha1.LifecycleConfig{TerminalTTL: ptr32(60)},
		},
	}

	run := testAgenticRun()
	run.Status.Conditions = []metav1.Condition{
		{Type: agenticv1alpha1.AgenticRunConditionAnalyzed, Status: metav1.ConditionTrue, Reason: "AnalysisComplete"},
		{Type: agenticv1alpha1.AgenticRunConditionDenied, Status: metav1.ConditionTrue, Reason: "UserDenied"},
	}

	objs := append([]client.Object{run, config}, defaultObjects()...)
	fc := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(objs...).
		WithStatusSubresource(run).Build()

	r := &AgenticRunReconciler{Client: fc, Agent: newTestAgentCaller(), Namespace: "default"}

	result, err := reconcileOnce(r, "fix-crash")
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	got, getErr := getAgenticRun(r, "fix-crash")
	if getErr != nil {
		t.Fatalf("getAgenticRun: %v", getErr)
	}
	if got.Status.TerminalTime == nil {
		t.Fatal("Denied run should have terminalTime stamped")
	}
	if got.Spec.TTLAfterTerminal == nil || *got.Spec.TTLAfterTerminal != 60 {
		t.Errorf("ttlAfterTerminal should be 60 for Denied run, got %v", got.Spec.TTLAfterTerminal)
	}
	if result.RequeueAfter <= 0 {
		t.Error("expected RequeueAfter > 0 for non-expired Denied run")
	}
}

func TestHandleTerminalTTL_FailedPhase(t *testing.T) {
	config := &agenticv1alpha1.AgenticOLSConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster"},
		Spec: agenticv1alpha1.AgenticOLSConfigSpec{
			Lifecycle: agenticv1alpha1.LifecycleConfig{TerminalTTL: ptr32(120)},
		},
	}

	run := testAgenticRun()
	run.Status.Conditions = []metav1.Condition{
		{Type: agenticv1alpha1.AgenticRunConditionAnalyzed, Status: metav1.ConditionFalse, Reason: "Failed", Message: "analysis error"},
	}

	objs := append([]client.Object{run, config}, defaultObjects()...)
	fc := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(objs...).
		WithStatusSubresource(run).Build()

	r := &AgenticRunReconciler{Client: fc, Agent: newTestAgentCaller(), Namespace: "default"}

	result, err := reconcileOnce(r, "fix-crash")
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	got, getErr := getAgenticRun(r, "fix-crash")
	if getErr != nil {
		t.Fatalf("getAgenticRun: %v", getErr)
	}
	if got.Status.TerminalTime == nil {
		t.Fatal("Failed run should have terminalTime stamped")
	}
	if result.RequeueAfter <= 0 {
		t.Error("expected RequeueAfter > 0 for non-expired Failed run")
	}
}

// TestHandleTerminalTTL_StampSyncsObservedGeneration guards against a
// regression where stamping ttlAfterTerminal (a spec write) bumps
// metadata.generation — on a real cluster, every accepted spec Patch does
// this — while needsRevision() treats any generation > Analyzed.
// observedGeneration as "revision requested." Since spec.revisionFeedback is
// never cleared once processed, an internal TTL-stamping generation bump
// left unsynced would spuriously re-arm the revision workflow later.
//
// The fake client used here (controller-runtime v0.23) does not simulate
// apiserver-side generation incrementing on Patch, so this test seeds an
// already-elevated Generation directly (as if a prior spec write, including
// a pre-fix ttlAfterTerminal stamp, had already bumped it) to exercise the
// sync logic in handleTerminalTTL deterministically.
func TestHandleTerminalTTL_StampSyncsObservedGeneration(t *testing.T) {
	config := &agenticv1alpha1.AgenticOLSConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster"},
		Spec: agenticv1alpha1.AgenticOLSConfigSpec{
			Lifecycle: agenticv1alpha1.LifecycleConfig{TerminalTTL: ptr32(3600)},
		},
	}

	run := testAgenticRun()
	run.Generation = 5
	// Stale feedback from a revision that was already fully processed;
	// RevisionFeedback is never cleared by the controller.
	run.Spec.RevisionFeedback = "please double check the fix"
	run.Status.Conditions = []metav1.Condition{
		{
			Type:               agenticv1alpha1.AgenticRunConditionAnalyzed,
			Status:             metav1.ConditionTrue,
			Reason:             "Complete",
			ObservedGeneration: 3, // stale relative to Generation=5
		},
		{Type: agenticv1alpha1.AgenticRunConditionVerified, Status: metav1.ConditionTrue, Reason: "Complete"},
	}

	objs := append([]client.Object{run, config}, defaultObjects()...)
	fc := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(objs...).
		WithStatusSubresource(run).Build()

	r := &AgenticRunReconciler{Client: fc, Agent: newTestAgentCaller(), Namespace: "default"}

	before, err := getAgenticRun(r, "fix-crash")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !needsRevision(before) {
		t.Fatal("test setup invalid: expected needsRevision() true before the TTL stamp corrects observedGeneration")
	}

	if _, _, err := r.handleTerminalTTL(context.Background(), before); err != nil {
		t.Fatalf("handleTerminalTTL: %v", err)
	}

	after, err := getAgenticRun(r, "fix-crash")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if after.Spec.TTLAfterTerminal == nil || *after.Spec.TTLAfterTerminal != 3600 {
		t.Fatalf("ttlAfterTerminal not stamped: %v", after.Spec.TTLAfterTerminal)
	}
	analyzed := meta.FindStatusCondition(after.Status.Conditions, agenticv1alpha1.AgenticRunConditionAnalyzed)
	if analyzed == nil || analyzed.ObservedGeneration != after.Generation {
		t.Fatalf("Analyzed.observedGeneration = %v, want %d (current generation)", analyzed, after.Generation)
	}
	if needsRevision(after) {
		t.Error("stamping ttlAfterTerminal must not leave stale revisionFeedback able to spuriously re-trigger needsRevision()")
	}
}
