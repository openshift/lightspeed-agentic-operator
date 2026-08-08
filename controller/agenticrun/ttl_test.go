package agenticrun

import (
	"context"
	"testing"
	"time"

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

	got, _ := getAgenticRun(r, "fix-crash")

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

	got, _ := getAgenticRun(r, "fix-crash")
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

	// Run should be marked for deletion (DeletionTimestamp set).
	// With finalizers present, Kubernetes keeps the object until finalizers
	// are removed — Delete sets DeletionTimestamp rather than removing it.
	var updated agenticv1alpha1.AgenticRun
	getErr := fc.Get(context.Background(), types.NamespacedName{Name: "fix-crash", Namespace: "default"}, &updated)
	if getErr != nil {
		if client.IgnoreNotFound(getErr) == nil {
			// Object was fully deleted (no finalizers case) — also acceptable.
			return
		}
		t.Fatalf("unexpected error: %v", getErr)
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
	got, _ := getAgenticRun(r, "fix-crash")
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

	got, _ := getAgenticRun(r, "fix-crash")
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

	got, _ := getAgenticRun(r, "fix-crash")
	if got.Status.TerminalTime == nil {
		t.Fatal("Failed run should have terminalTime stamped")
	}
	if result.RequeueAfter <= 0 {
		t.Error("expected RequeueAfter > 0 for non-expired Failed run")
	}
}
