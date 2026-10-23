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
	"sort"
	"strings"
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
	"github.com/openshift/lightspeed-agentic-operator/controller/agenticrun"
)

const (
	requeueDelay        = 5 * time.Second
	degradedRequeueWait = 30 * time.Second

	reasonStopping                 = "Stopping"
	reasonAdminActivated           = "AdminActivated"
	reasonAdminActivatedWithErrors = "AdminActivatedWithErrors"
	reasonAdminDeactivated         = "AdminDeactivated"

	eventReasonSuspensionActivated           = "SuspensionActivated"
	eventReasonSuspensionDeactivated         = "SuspensionDeactivated"
	eventReasonSuspensionCompletedWithErrors = "SuspensionCompletedWithErrors"
	eventReasonSuspensionDeactivationDenied  = "SuspensionDeactivationDenied"
)

// SandboxSweeper is the shared hard-stop implementation used by the config
// controller. It returns true only when no active managed workload or sandbox
// access remains.
type SandboxSweeper interface {
	SweepCluster(ctx context.Context, runs []agenticv1alpha1.AgenticRun) (bool, error)
}

// Reconciler watches AgenticOLSConfig and AgenticRun resources to maintain
// the Suspended status condition, emit lifecycle Events, and drive the
// cluster-wide hard stop.
type Reconciler struct {
	client.Client
	// APIReader is an authoritative, uncached reader. When nil it falls back
	// to Client.
	APIReader client.Reader
	// Sandbox sweeps managed workloads and access. It must be set in
	// production; tests inject a fake.
	Sandbox       SandboxSweeper
	EventRecorder record.EventRecorder
	// Now is injectable for retry-window tests.
	Now func() time.Time
}

// +kubebuilder:rbac:groups=agentic.openshift.io,resources=agenticolsconfigs,verbs=get;list;watch
// +kubebuilder:rbac:groups=agentic.openshift.io,resources=agenticolsconfigs/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=agentic.openshift.io,resources=agenticruns,verbs=list
// +kubebuilder:rbac:groups="",resources=pods;configmaps;secrets;serviceaccounts,verbs=get;list;watch;delete
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=roles;rolebindings,verbs=get;list;watch;delete
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterroles;clusterrolebindings,verbs=get;list;watch;delete
// +kubebuilder:rbac:groups=extensions.agents.x-k8s.io,resources=sandboxclaims;sandboxwarmpools,verbs=get;list;watch;delete
// +kubebuilder:rbac:groups=agents.x-k8s.io,resources=sandboxes,verbs=get;list;watch;delete

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

func (r *Reconciler) reader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

func (r *Reconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Reconciler) handleDeactivation(ctx context.Context, config *agenticv1alpha1.AgenticOLSConfig) (ctrl.Result, error) {
	existing := meta.FindStatusCondition(config.Status.Conditions, agenticv1alpha1.AgenticOLSConfigConditionSuspended)
	if existing == nil || existing.Status != metav1.ConditionTrue {
		return ctrl.Result{}, nil
	}

	// Admission is the primary guard (system-config.md rule 11j). Keep the
	// reconciler from silently marking a stopping system as deactivated if a
	// stale or misconfigured policy let the spec change through.
	if existing.Reason != reasonAdminActivated && existing.Reason != reasonAdminActivatedWithErrors {
		r.EventRecorder.Eventf(config, corev1.EventTypeWarning, eventReasonSuspensionDeactivationDenied,
			"System is still %s; resume requires a terminal Suspended condition", existing.Reason)
		return ctrl.Result{}, nil
	}

	if err := r.patchCondition(ctx, config, metav1.Condition{
		Type:    agenticv1alpha1.AgenticOLSConfigConditionSuspended,
		Status:  metav1.ConditionFalse,
		Reason:  reasonAdminDeactivated,
		Message: "System resumed; agentic operations re-enabled",
	}); err != nil {
		return ctrl.Result{}, fmt.Errorf("patch Suspended=False: %w", err)
	}
	r.EventRecorder.Event(config, corev1.EventTypeNormal, eventReasonSuspensionDeactivated, "System resumed; agentic operations re-enabled")
	return ctrl.Result{}, nil
}

func (r *Reconciler) handleActivation(ctx context.Context, config *agenticv1alpha1.AgenticOLSConfig) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	now := r.now()

	runs, err := r.listRuns(ctx)
	if err != nil {
		return ctrl.Result{}, err
	}
	existing := meta.FindStatusCondition(config.Status.Conditions, agenticv1alpha1.AgenticOLSConfigConditionSuspended)
	stoppingSince := now
	if existing != nil && existing.Status == metav1.ConditionTrue && existing.Reason == reasonStopping && !existing.LastTransitionTime.IsZero() {
		stoppingSince = existing.LastTransitionTime.Time
	}

	nonTerminal := nonTerminalRuns(runs.Items)
	emergencyStopped := emergencyStoppedCount(runs.Items)

	if existing == nil || existing.Status != metav1.ConditionTrue || existing.Reason == reasonAdminDeactivated {
		r.EventRecorder.Eventf(config, corev1.EventTypeWarning, eventReasonSuspensionActivated,
			"System suspension activated; stopping %d active sandbox workloads", len(nonTerminal))
	}

	// Set Stopping before the sweep whenever teardown is not yet known to be
	// complete. A terminal condition with no non-terminal runs is rechecked by
	// the sweep below without flapping through Stopping first.
	needSweep := existing == nil || existing.Status != metav1.ConditionTrue ||
		existing.Reason == reasonStopping || existing.Reason == reasonAdminDeactivated || len(nonTerminal) > 0
	if needSweep {
		if err := r.ensureStopping(ctx, config, nonTerminal, nil); err != nil {
			return ctrl.Result{}, err
		}
	}

	if r.Sandbox == nil {
		return ctrl.Result{}, fmt.Errorf("sandbox sweeper is not configured")
	}
	confirmed, sweepErr := r.Sandbox.SweepCluster(ctx, runs.Items)

	// The run reconciler records outcomes asynchronously. Re-list through the
	// authoritative reader so the config condition waits for those outcomes.
	runs, err = r.listRuns(ctx)
	if err != nil {
		return ctrl.Result{}, err
	}
	nonTerminal = nonTerminalRuns(runs.Items)
	emergencyStopped = emergencyStoppedCount(runs.Items)
	// ensureStopping may have patched the status; use the current reason for
	// the degraded/no-op decisions below.
	existing = meta.FindStatusCondition(config.Status.Conditions, agenticv1alpha1.AgenticOLSConfigConditionSuspended)

	if len(nonTerminal) > 0 && !stoppedOnlyOnFailedSpokes(runs.Items, sweepErr) {
		if err := r.ensureStopping(ctx, config, nonTerminal, sweepErr); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: requeueDelay}, nil
	}

	if sweepErr != nil {
		// Once the config is already degraded, keep retrying without
		// restarting the retry window or flapping back through Stopping.
		if existing != nil && existing.Reason == reasonAdminActivatedWithErrors && agenticrun.IsSpokeStopDegradeable(sweepErr) {
			message := degradationMessage(sweepErr, nonTerminal, emergencyStopped)
			if existing.Message != message {
				if err := r.patchCondition(ctx, config, metav1.Condition{
					Type:               agenticv1alpha1.AgenticOLSConfigConditionSuspended,
					Status:             metav1.ConditionTrue,
					Reason:             reasonAdminActivatedWithErrors,
					Message:            message,
					ObservedGeneration: config.Generation,
				}); err != nil {
					return ctrl.Result{}, fmt.Errorf("patch Suspended=True/%s: %w", reasonAdminActivatedWithErrors, err)
				}
			}
			return ctrl.Result{RequeueAfter: degradedRequeueWait}, nil
		}
		if agenticrun.IsSpokeStopDegradeable(sweepErr) &&
			(agenticrun.IsSpokeStopGone(sweepErr) || now.Sub(stoppingSince) >= agenticrun.SpokeRetryWindow) {
			message := degradationMessage(sweepErr, nonTerminal, emergencyStopped)
			if err := r.patchCondition(ctx, config, metav1.Condition{
				Type:               agenticv1alpha1.AgenticOLSConfigConditionSuspended,
				Status:             metav1.ConditionTrue,
				Reason:             reasonAdminActivatedWithErrors,
				Message:            message,
				ObservedGeneration: config.Generation,
			}); err != nil {
				return ctrl.Result{}, fmt.Errorf("patch Suspended=True/%s: %w", reasonAdminActivatedWithErrors, err)
			}
			r.EventRecorder.Event(config, corev1.EventTypeWarning, eventReasonSuspensionCompletedWithErrors, message)
			return ctrl.Result{RequeueAfter: degradedRequeueWait}, nil
		}
		if err := r.ensureStopping(ctx, config, nonTerminal, sweepErr); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: requeueDelay}, nil
	}

	if !confirmed {
		if err := r.ensureStopping(ctx, config, nonTerminal, nil); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: requeueDelay}, nil
	}

	message := fmt.Sprintf("System suspended; %d runs emergency-stopped", emergencyStopped)
	if existing != nil && existing.Status == metav1.ConditionTrue && existing.Reason == reasonAdminActivated && existing.Message == message {
		return ctrl.Result{}, nil
	}
	if err := r.patchCondition(ctx, config, metav1.Condition{
		Type:               agenticv1alpha1.AgenticOLSConfigConditionSuspended,
		Status:             metav1.ConditionTrue,
		Reason:             reasonAdminActivated,
		Message:            message,
		ObservedGeneration: config.Generation,
	}); err != nil {
		return ctrl.Result{}, fmt.Errorf("patch Suspended=True/%s: %w", reasonAdminActivated, err)
	}
	log.V(1).Info("system suspension completed", "emergencyStopped", emergencyStopped)
	return ctrl.Result{}, nil
}

func (r *Reconciler) ensureStopping(ctx context.Context, config *agenticv1alpha1.AgenticOLSConfig, nonTerminal []string, sweepErr error) error {
	message := stoppingMessage(len(nonTerminal), sweepErr)
	existing := meta.FindStatusCondition(config.Status.Conditions, agenticv1alpha1.AgenticOLSConfigConditionSuspended)
	if existing != nil && existing.Status == metav1.ConditionTrue && existing.Reason == reasonStopping && existing.Message == message {
		return nil
	}
	stoppingSince := r.now()
	if existing != nil && existing.Status == metav1.ConditionTrue && existing.Reason == reasonStopping && !existing.LastTransitionTime.IsZero() {
		stoppingSince = existing.LastTransitionTime.Time
	}
	return r.patchCondition(ctx, config, metav1.Condition{
		Type:               agenticv1alpha1.AgenticOLSConfigConditionSuspended,
		Status:             metav1.ConditionTrue,
		Reason:             reasonStopping,
		Message:            message,
		ObservedGeneration: config.Generation,
		LastTransitionTime: metav1.NewTime(stoppingSince),
	})
}

func (r *Reconciler) patchCondition(ctx context.Context, config *agenticv1alpha1.AgenticOLSConfig, condition metav1.Condition) error {
	base := config.DeepCopy()
	meta.SetStatusCondition(&config.Status.Conditions, condition)
	if !condition.LastTransitionTime.IsZero() {
		if current := meta.FindStatusCondition(config.Status.Conditions, condition.Type); current != nil {
			current.LastTransitionTime = condition.LastTransitionTime
		}
	}
	if err := r.Status().Patch(ctx, config, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
		return err
	}
	return r.reader().Get(ctx, client.ObjectKeyFromObject(config), config)
}

func (r *Reconciler) listRuns(ctx context.Context) (*agenticv1alpha1.AgenticRunList, error) {
	var runs agenticv1alpha1.AgenticRunList
	if err := r.reader().List(ctx, &runs); err != nil {
		return nil, fmt.Errorf("list runs: %w", err)
	}
	return &runs, nil
}

// stoppedOnlyOnFailedSpokes reports whether every non-terminal AgenticRun has
// recorded Stopping and targets a spoke that the sweep could not verify. This
// is the only case where an unverified spoke may unblock degraded completion
// while an AgenticRun is still non-terminal.
func stoppedOnlyOnFailedSpokes(runs []agenticv1alpha1.AgenticRun, sweepErr error) bool {
	if sweepErr == nil || !agenticrun.IsSpokeStopDegradeable(sweepErr) {
		return false
	}
	failed := make(map[string]struct{})
	for _, cluster := range agenticrun.SpokeStopClusters(sweepErr) {
		failed[cluster] = struct{}{}
	}
	if len(failed) == 0 {
		return false
	}
	for i := range runs {
		run := &runs[i]
		if isTerminal(agenticv1alpha1.DerivePhase(run.Status.Conditions)) {
			continue
		}
		stopping := meta.FindStatusCondition(run.Status.Conditions, agenticv1alpha1.AgenticRunConditionStopping)
		if stopping == nil || stopping.Status != metav1.ConditionTrue {
			return false
		}
		if _, ok := failed[run.Spec.TargetCluster]; !ok {
			return false
		}
	}
	return true
}

func nonTerminalRuns(runs []agenticv1alpha1.AgenticRun) []string {
	names := make([]string, 0)
	for i := range runs {
		if !isTerminal(agenticv1alpha1.DerivePhase(runs[i].Status.Conditions)) {
			names = append(names, runs[i].Name)
		}
	}
	sort.Strings(names)
	return names
}

func emergencyStoppedCount(runs []agenticv1alpha1.AgenticRun) int {
	count := 0
	for i := range runs {
		if agenticv1alpha1.DerivePhase(runs[i].Status.Conditions) == agenticv1alpha1.AgenticRunPhaseEmergencyStopped {
			count++
		}
	}
	return count
}

func stoppingMessage(nonTerminal int, sweepErr error) string {
	message := fmt.Sprintf("Stopping system; %d runs still stopping", nonTerminal)
	if sweepErr != nil {
		message += fmt.Sprintf("; %s", compactError(sweepErr))
	}
	return message
}

func degradationMessage(sweepErr error, nonTerminal []string, emergencyStopped int) string {
	clusters := agenticrun.SpokeStopClusters(sweepErr)
	parts := []string{fmt.Sprintf("System suspended with errors; %d runs emergency-stopped", emergencyStopped)}
	if len(clusters) > 0 {
		parts = append(parts, fmt.Sprintf("spokes not verified: %s", strings.Join(clusters, ", ")))
	}
	if len(nonTerminal) > 0 {
		parts = append(parts, fmt.Sprintf("runs still stopping: %s", strings.Join(nonTerminal, ", ")))
	}
	parts = append(parts, compactError(sweepErr))
	return strings.Join(parts, "; ")
}

func compactError(err error) string {
	if err == nil {
		return ""
	}
	text := err.Error()
	const max = 1024
	if len(text) > max {
		return text[:max] + "..."
	}
	return text
}

func isTerminal(phase agenticv1alpha1.AgenticRunPhase) bool {
	switch phase {
	case agenticv1alpha1.AgenticRunPhaseCompleted,
		agenticv1alpha1.AgenticRunPhaseDenied,
		agenticv1alpha1.AgenticRunPhaseEscalated,
		agenticv1alpha1.AgenticRunPhaseEmergencyStopped,
		agenticv1alpha1.AgenticRunPhaseFailed:
		return true
	}
	return false
}

// SetupWithManager registers the controller and its watches.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&agenticv1alpha1.AgenticOLSConfig{}).
		Watches(
			&agenticv1alpha1.AgenticRun{},
			handler.EnqueueRequestsFromMapFunc(func(_ context.Context, _ client.Object) []reconcile.Request {
				return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: "cluster"}}}
			}),
		).
		Named("agenticolsconfig").
		Complete(r)
}
