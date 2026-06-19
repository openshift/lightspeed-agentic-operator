package agenticolsconfig

import (
	"context"
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
)

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

func emergencyStoppedProposal(name string) *agenticv1alpha1.Proposal {
	return &agenticv1alpha1.Proposal{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: agenticv1alpha1.ProposalSpec{
			Request: "test",
		},
		Status: agenticv1alpha1.ProposalStatus{
			Conditions: []metav1.Condition{{
				Type:   agenticv1alpha1.ProposalConditionEmergencyStopped,
				Status: metav1.ConditionTrue,
				Reason: "SystemSuspended",
			}},
		},
	}
}

func pendingProposal(name string) *agenticv1alpha1.Proposal {
	return &agenticv1alpha1.Proposal{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: agenticv1alpha1.ProposalSpec{
			Request: "test",
		},
	}
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
	prop := emergencyStoppedProposal("stopped-1")
	fc := fake.NewClientBuilder().
		WithScheme(testScheme()).
		WithObjects(config, prop).
		WithStatusSubresource(&agenticv1alpha1.AgenticOLSConfig{}).
		Build()
	recorder := record.NewFakeRecorder(1)
	r := &Reconciler{Client: fc, EventRecorder: recorder}

	result, err := reconcileOnce(r)
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if result.Requeue {
		t.Fatal("expected no requeue when all proposals are terminal")
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
	wantMsg := "System suspended; 1 proposals emergency-stopped"
	if cond.Message != wantMsg {
		t.Fatalf("condition message = %q, want %q", cond.Message, wantMsg)
	}

	event := waitForEvent(t, recorder, eventReasonSuspensionActivated)
	if !strings.Contains(event, wantMsg) {
		t.Fatalf("event = %q, want message %q", event, wantMsg)
	}
}

func TestReconcile_ActivationPendingProposals(t *testing.T) {
	config := testConfig(true)
	prop := pendingProposal("pending-1")
	fc := fake.NewClientBuilder().
		WithScheme(testScheme()).
		WithObjects(config, prop).
		WithStatusSubresource(&agenticv1alpha1.AgenticOLSConfig{}).
		Build()
	recorder := record.NewFakeRecorder(1)
	r := &Reconciler{Client: fc, EventRecorder: recorder}

	result, err := reconcileOnce(r)
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if result.RequeueAfter == 0 {
		t.Fatal("expected requeue while non-terminal proposals remain")
	}

	got := getConfig(t, fc)
	if len(got.Status.Conditions) != 0 {
		t.Fatalf("expected no status conditions yet, got %v", got.Status.Conditions)
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
	recorder := record.NewFakeRecorder(1)
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
