// Package webhookpolicy manages ingress for the always-installed admission webhook.
// It is infrastructure, not an agentic operand: it must work on both 4.x and 5.x.
package webhookpolicy

import (
	"context"
	"fmt"
	"reflect"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	ServiceName = "agentic-operator-webhook-service"
	PolicyName  = "allow-webhook-ingress"
	managedBy   = "app.kubernetes.io/managed-by"
	manager     = "lightspeed-agentic-operator"
)

type Reconciler struct {
	client.Client
	Namespace string
}

// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;patch;update
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch

func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	if req.Namespace != r.Namespace || req.Name != PolicyName {
		return ctrl.Result{}, nil
	}
	var service corev1.Service
	if err := r.Get(ctx, types.NamespacedName{Namespace: r.Namespace, Name: ServiceName}, &service); err != nil {
		// When OLM removes the Service, Kubernetes garbage-collects its policy.
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	selector := service.Spec.Selector
	if len(selector) != 1 || (selector["control-plane"] != "controller-manager" && selector["control-plane"] != "agentic-controller-manager") {
		return ctrl.Result{}, fmt.Errorf("webhook Service has an unexpected pod selector: %v", selector)
	}

	want := policy(r.Namespace, selector["control-plane"])
	if err := controllerutil.SetOwnerReference(&service, want, r.Scheme()); err != nil {
		return ctrl.Result{}, fmt.Errorf("set webhook policy owner: %w", err)
	}
	var current networkingv1.NetworkPolicy
	if err := r.Get(ctx, req.NamespacedName, &current); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		if err := r.Create(ctx, want); err != nil {
			if apierrors.IsAlreadyExists(err) {
				return ctrl.Result{Requeue: true}, nil
			}
			return ctrl.Result{}, fmt.Errorf("create webhook policy: %w", err)
		}
		return ctrl.Result{}, nil
	}

	// Adopt only the exact old policy installed by the standalone kustomization.
	// Never overwrite an unrelated policy with the same name.
	if current.Labels[managedBy] != manager && !legacyPolicy(&current) {
		return ctrl.Result{}, fmt.Errorf("webhook policy %s/%s is not managed by this operator", r.Namespace, PolicyName)
	}
	// Service recreation changes its UID; adopt the replacement, but never
	// steal a policy owned by another resource even if it carries our label.
	for _, owner := range current.OwnerReferences {
		if owner.APIVersion != "v1" || owner.Kind != "Service" || owner.Name != ServiceName {
			return ctrl.Result{}, fmt.Errorf("webhook policy %s/%s has an unrelated owner", r.Namespace, PolicyName)
		}
	}
	before := current.DeepCopy()
	current.Spec = want.Spec
	if current.Labels == nil {
		current.Labels = map[string]string{}
	}
	current.Labels[managedBy] = manager
	current.OwnerReferences = want.OwnerReferences
	if !reflect.DeepEqual(before.Spec, current.Spec) || !reflect.DeepEqual(before.Labels, current.Labels) || !reflect.DeepEqual(before.OwnerReferences, current.OwnerReferences) {
		if err := r.Patch(ctx, &current, client.MergeFrom(before)); err != nil {
			return ctrl.Result{}, fmt.Errorf("update webhook policy: %w", err)
		}
	}
	return ctrl.Result{}, nil
}

func policy(namespace, selector string) *networkingv1.NetworkPolicy {
	tcp := corev1.ProtocolTCP
	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: PolicyName, Namespace: namespace, Labels: map[string]string{managedBy: manager}},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"control-plane": selector}},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
			Ingress:     []networkingv1.NetworkPolicyIngressRule{{Ports: []networkingv1.NetworkPolicyPort{{Port: ptr(intstr.FromInt32(9443)), Protocol: &tcp}}}},
		},
	}
}

func ptr[T any](v T) *T { return &v }

func legacyPolicy(p *networkingv1.NetworkPolicy) bool {
	return len(p.OwnerReferences) == 0 && len(p.Labels) == 0 &&
		reflect.DeepEqual(p.Spec, policy(p.Namespace, "controller-manager").Spec)
}

func (r *Reconciler) serviceRequests(_ context.Context, obj client.Object) []reconcile.Request {
	if obj.GetNamespace() != r.Namespace || obj.GetName() != ServiceName {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: PolicyName, Namespace: r.Namespace}}}
}

func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&networkingv1.NetworkPolicy{}, builder.WithPredicates(predicate.NewPredicateFuncs(func(obj client.Object) bool {
			return obj.GetNamespace() == r.Namespace && obj.GetName() == PolicyName
		}))).
		Watches(&corev1.Service{}, handler.EnqueueRequestsFromMapFunc(r.serviceRequests)).
		Named("webhook-network-policy").
		Complete(r)
}
