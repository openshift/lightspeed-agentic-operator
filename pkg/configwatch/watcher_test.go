package configwatch

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestIndependentWatcherLifecycle(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	client := fake.NewClientBuilder().WithScheme(scheme).Build()
	var current *corev1.ConfigMap
	metrics := New(client, "operator", Registration{Name: "handoff", Handler: func(_ context.Context, cm *corev1.ConfigMap) error { current = cm; return nil }})
	metrics.Name = "metrics-configmap-watcher"
	blocked, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	telemetry := New(client, "operator", Registration{Name: "handoff", Handler: func(_ context.Context, cm *corev1.ConfigMap) error { close(blocked); <-release; return nil }})
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "operator", Name: "handoff"}}
	go func() { defer close(done); _, _ = telemetry.Reconcile(ctx, req) }()
	<-blocked
	defer func() { close(release); <-done }()
	reconcile := func() {
		t.Helper()
		if _, err := metrics.Reconcile(ctx, req); err != nil {
			t.Fatal(err)
		}
	}
	reconcile()
	if current != nil {
		t.Fatal("absent handoff must notify deletion")
	}
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "handoff", Namespace: "operator"}, Data: map[string]string{"tls-min-version": "VersionTLS12"}}
	if err := client.Create(ctx, cm); err != nil {
		t.Fatal(err)
	}
	reconcile()
	if current == nil || current.Data["tls-min-version"] != "VersionTLS12" {
		t.Fatal("late handoff not delivered")
	}
	cm.Data["tls-min-version"] = "VersionTLS13"
	if err := client.Update(ctx, cm); err != nil {
		t.Fatal(err)
	}
	reconcile()
	if current.Data["tls-min-version"] != "VersionTLS13" {
		t.Fatal("update not delivered")
	}
	if err := client.Delete(ctx, cm); err != nil {
		t.Fatal(err)
	}
	reconcile()
	if current != nil {
		t.Fatal("deleted handoff must clear policy despite blocked telemetry")
	}
}
