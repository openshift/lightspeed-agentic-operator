/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package agenticolsconfig

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
)

const (
	requeueDelay = 5 * time.Second

	reasonDraining                   = "Draining"
	reasonAdminActivated             = "AdminActivated"
	reasonAdminDeactivated           = "AdminDeactivated"
	eventReasonSuspensionActivated   = "SuspensionActivated"
	eventReasonSuspensionDeactivated = "SuspensionDeactivated"
)

// Reconciler watches AgenticOLSConfig and Proposal resources to maintain
// the Suspended status condition and emit lifecycle Events on the config CR.
type Reconciler struct {
	client.Client
	EventRecorder record.EventRecorder
}

// +kubebuilder:rbac:groups=agentic.openshift.io,resources=agenticolsconfigs,verbs=get;list;watch
// +kubebuilder:rbac:groups=agentic.openshift.io,resources=agenticolsconfigs/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=agentic.openshift.io,resources=proposals,verbs=list

func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var config agenticv1alpha1.AgenticOLSConfig
	if err := r.Get(ctx, req.NamespacedName, &config); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !config.Spec.Suspended {
		return r.handleDeactivation(ctx, &config)
	}
	return r.handleActivation(ctx, &config)
}

func (r *Reconciler) handleDeactivation(ctx context.Context, config *agenticv1alpha1.AgenticOLSConfig) (ctrl.Result, error) {
	existing := meta.FindStatusCondition(config.Status.Conditions, agenticv1alpha1.AgenticOLSConfigConditionSuspended)
	if existing == nil || existing.Status != metav1.ConditionTrue {
		return ctrl.Result{}, nil
	}

	base := config.DeepCopy()
	meta.SetStatusCondition(&config.Status.Conditions, metav1.Condition{
		Type:   agenticv1alpha1.AgenticOLSConfigConditionSuspended,
		Status: metav1.ConditionFalse,
		Reason: reasonAdminDeactivated,
	})
	if err := r.Status().Patch(ctx, config, client.MergeFrom(base)); err != nil {
		return ctrl.Result{}, fmt.Errorf("patch Suspended=False: %w", err)
	}
	r.EventRecorder.Event(config, corev1.EventTypeNormal, eventReasonSuspensionDeactivated, "System resumed; agentic operations re-enabled")
	return ctrl.Result{}, nil
}

func (r *Reconciler) handleActivation(ctx context.Context, config *agenticv1alpha1.AgenticOLSConfig) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var proposals agenticv1alpha1.ProposalList
	if err := r.List(ctx, &proposals); err != nil {
		return ctrl.Result{}, fmt.Errorf("list proposals: %w", err)
	}

	var nonTerminal, emergencyStopped int
	for i := range proposals.Items {
		phase := agenticv1alpha1.DerivePhase(proposals.Items[i].Status.Conditions)
		if phase == agenticv1alpha1.ProposalPhaseEmergencyStopped {
			emergencyStopped++
			continue
		}
		if !isTerminal(phase) {
			nonTerminal++
		}
	}

	if nonTerminal > 0 {
		log.V(1).Info("waiting for proposals to terminate", "nonTerminal", nonTerminal)
		base := config.DeepCopy()
		meta.SetStatusCondition(&config.Status.Conditions, metav1.Condition{
			Type:    agenticv1alpha1.AgenticOLSConfigConditionSuspended,
			Status:  metav1.ConditionTrue,
			Reason:  reasonDraining,
			Message: fmt.Sprintf("Waiting for %d proposals to terminate", nonTerminal),
		})
		if err := r.Status().Patch(ctx, config, client.MergeFrom(base)); err != nil {
			return ctrl.Result{}, fmt.Errorf("patch Suspended=True/Draining: %w", err)
		}
		return ctrl.Result{RequeueAfter: requeueDelay}, nil
	}

	msg := fmt.Sprintf("System suspended; %d proposals emergency-stopped", emergencyStopped)
	existing := meta.FindStatusCondition(config.Status.Conditions, agenticv1alpha1.AgenticOLSConfigConditionSuspended)
	if existing != nil && existing.Status == metav1.ConditionTrue && existing.Reason == reasonAdminActivated && existing.Message == msg {
		return ctrl.Result{}, nil
	}

	base := config.DeepCopy()
	meta.SetStatusCondition(&config.Status.Conditions, metav1.Condition{
		Type:    agenticv1alpha1.AgenticOLSConfigConditionSuspended,
		Status:  metav1.ConditionTrue,
		Reason:  reasonAdminActivated,
		Message: msg,
	})
	if err := r.Status().Patch(ctx, config, client.MergeFrom(base)); err != nil {
		return ctrl.Result{}, fmt.Errorf("patch Suspended=True: %w", err)
	}
	r.EventRecorder.Eventf(config, corev1.EventTypeWarning, eventReasonSuspensionActivated, msg)
	return ctrl.Result{}, nil
}

func isTerminal(phase agenticv1alpha1.ProposalPhase) bool {
	switch phase {
	case agenticv1alpha1.ProposalPhaseCompleted,
		agenticv1alpha1.ProposalPhaseDenied,
		agenticv1alpha1.ProposalPhaseEscalated,
		agenticv1alpha1.ProposalPhaseEmergencyStopped,
		agenticv1alpha1.ProposalPhaseFailed:
		return true
	}
	return false
}

// SetupWithManager registers the controller and its watches.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&agenticv1alpha1.AgenticOLSConfig{}).
		Watches(
			&agenticv1alpha1.Proposal{},
			handler.EnqueueRequestsFromMapFunc(func(_ context.Context, _ client.Object) []reconcile.Request {
				return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: "cluster"}}}
			}),
		).
		Named("agenticolsconfig").
		Complete(r)
}
