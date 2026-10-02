package webhookpolicy

import (
	"context"
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const namespace = "openshift-lightspeed"

func testClient(t *testing.T, objects ...client.Object) *Reconciler {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := networkingv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return &Reconciler{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build(), Namespace: namespace}
}

func service(selector string) *corev1.Service {
	return &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: ServiceName, Namespace: namespace, UID: "webhook-service-uid"},
		Spec: corev1.ServiceSpec{Selector: map[string]string{"control-plane": selector}}}
}

func reconcilePolicy(t *testing.T, r *Reconciler) error {
	t.Helper()
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: namespace, Name: PolicyName}})
	return err
}

func TestReconcilePolicyForBothInstallLayouts(t *testing.T) {
	for _, label := range []string{"controller-manager", "agentic-controller-manager"} {
		t.Run(label, func(t *testing.T) {
			ctx := context.Background()
			svc := service(label)
			r := testClient(t, svc)
			if got := r.serviceRequests(ctx, svc); len(got) != 1 || got[0].Name != PolicyName {
				t.Fatalf("Service event did not enqueue policy: %v", got)
			}
			for i := 0; i < 2; i++ { // stable when the watch resyncs
				if err := reconcilePolicy(t, r); err != nil {
					t.Fatal(err)
				}
			}
			var got networkingv1.NetworkPolicy
			if err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: PolicyName}, &got); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got.Spec, policy(namespace, label).Spec) {
				t.Fatalf("unexpected policy: %+v", got.Spec)
			}
			if got.Labels[managedBy] != manager || len(got.OwnerReferences) != 1 || got.OwnerReferences[0].UID != svc.UID {
				t.Fatalf("policy must be managed and owned by webhook Service: %+v", got.ObjectMeta)
			}
			if err := r.Delete(ctx, &got); err != nil {
				t.Fatal(err)
			}
			if err := reconcilePolicy(t, r); err != nil {
				t.Fatal(err)
			}
			if err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: PolicyName}, &got); err != nil {
				t.Fatalf("policy not restored after deletion: %v", err)
			}
		})
	}
}

func TestReconcileAdoptsOnlyKnownLegacyPolicy(t *testing.T) {
	ctx := context.Background()
	old := policy(namespace, "controller-manager")
	old.Labels = nil // the old standalone kustomization did not label or own it
	r := testClient(t, service("agentic-controller-manager"), old)
	if err := reconcilePolicy(t, r); err != nil {
		t.Fatal(err)
	}
	var got networkingv1.NetworkPolicy
	if err := r.Get(ctx, client.ObjectKeyFromObject(old), &got); err != nil {
		t.Fatal(err)
	}
	if got.Spec.PodSelector.MatchLabels["control-plane"] != "agentic-controller-manager" || got.Labels[managedBy] != manager || len(got.OwnerReferences) != 1 {
		t.Fatalf("legacy policy was not adopted and retargeted: %+v", got)
	}
}

func TestReconcileDoesNotOverwriteUnrelatedPolicy(t *testing.T) {
	ctx := context.Background()
	other := policy(namespace, "controller-manager")
	other.Labels = map[string]string{"owner": "someone-else"}
	r := testClient(t, service("agentic-controller-manager"), other)
	if err := reconcilePolicy(t, r); err == nil {
		t.Fatal("conflicting NetworkPolicy was overwritten")
	}
	var got networkingv1.NetworkPolicy
	if err := r.Get(ctx, client.ObjectKeyFromObject(other), &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(other.Spec, got.Spec) || got.Labels[managedBy] != "" {
		t.Fatal("unrelated policy was modified")
	}
}

func TestReconcileRepairsServiceReplacementAndPodSelector(t *testing.T) {
	ctx := context.Background()
	svc := service("controller-manager")
	r := testClient(t, svc)
	if err := reconcilePolicy(t, r); err != nil {
		t.Fatal(err)
	}
	// A new OLM Service UID and the single-bundle pod selector should be
	// adopted without leaving the policy attached to the removed Service.
	var current networkingv1.NetworkPolicy
	key := types.NamespacedName{Namespace: namespace, Name: PolicyName}
	if err := r.Get(ctx, key, &current); err != nil {
		t.Fatal(err)
	}
	if err := r.Delete(ctx, svc); err != nil {
		t.Fatal(err)
	}
	svc = service("agentic-controller-manager")
	svc.UID = "replacement-service-uid"
	if err := r.Create(ctx, svc); err != nil {
		t.Fatal(err)
	}
	if err := reconcilePolicy(t, r); err != nil {
		t.Fatal(err)
	}
	if err := r.Get(ctx, key, &current); err != nil {
		t.Fatal(err)
	}
	if current.OwnerReferences[0].UID != svc.UID || current.Spec.PodSelector.MatchLabels["control-plane"] != svc.Spec.Selector["control-plane"] {
		t.Fatalf("policy did not follow replacement Service: %+v", current)
	}
}

func TestReconcileRejectsUnrelatedOwnerEvenWithManagedLabel(t *testing.T) {
	other := policy(namespace, "controller-manager")
	other.OwnerReferences = []metav1.OwnerReference{{APIVersion: "v1", Kind: "ConfigMap", Name: "someone-else", UID: "other"}}
	r := testClient(t, service("controller-manager"), other)
	if err := reconcilePolicy(t, r); err == nil {
		t.Fatal("unrelated owner was replaced")
	}
}

func TestReconcileRequiresWebhookServiceAndSafeSelector(t *testing.T) {
	ctx := context.Background()
	r := testClient(t)
	if err := reconcilePolicy(t, r); err != nil {
		t.Fatal(err)
	}
	if err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: PolicyName}, &networkingv1.NetworkPolicy{}); !apierrors.IsNotFound(err) {
		t.Fatalf("policy created without webhook Service: %v", err)
	}
	bad := service("")
	bad.Spec.Selector = nil
	if err := r.Create(ctx, bad); err != nil {
		t.Fatal(err)
	}
	if err := reconcilePolicy(t, r); err == nil {
		t.Fatal("empty selector must not create a policy selecting every pod")
	}
}
