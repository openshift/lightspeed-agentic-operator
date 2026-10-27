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
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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
	requeueDelay                 = 5 * time.Second
	backgroundCleanupPassTimeout = 5 * time.Second
	statusPatchAttempts          = 3
	maxDiagnosticItems           = 6
	maxDiagnosticLength          = 1024

	reasonStopping                   = "Stopping"
	reasonAdminActivated             = "AdminActivated"
	reasonAdminActivatedWithErrors   = "AdminActivatedWithErrors"
	reasonAdminDeactivated           = "AdminDeactivated"
	reasonSystemSuspended            = "SystemSuspended"
	reasonTeardownComplete           = "TeardownComplete"
	reasonTeardownFailed             = "TeardownFailed"
	reasonEmergencyStopFailed        = "EmergencyStopFailed"
	reasonCleanupConfirmed           = "CleanupConfirmed"
	eventReasonSuspensionActivated   = "SuspensionActivated"
	eventReasonSuspensionDeactivated = "SuspensionDeactivated"
	runRBACNamespacesAnnotation      = "agentic.openshift.io/rbac-namespaces"
)

func activationStart(config *agenticv1alpha1.AgenticOLSConfig, now time.Time) metav1.Time {
	current := meta.FindStatusCondition(config.Status.Conditions, agenticv1alpha1.AgenticOLSConfigConditionSuspended)
	if current != nil && current.Status == metav1.ConditionTrue &&
		current.ObservedGeneration == config.Generation && !current.LastTransitionTime.IsZero() {
		return current.LastTransitionTime
	}
	// Match persisted metav1.Time precision before copying this activation identity.
	return metav1.NewTime(now.UTC().Truncate(time.Second))
}

func setConditionAt(conditions *[]metav1.Condition, condition metav1.Condition, at metav1.Time) {
	meta.SetStatusCondition(conditions, condition)
	meta.FindStatusCondition(*conditions, condition.Type).LastTransitionTime = at
}

// Reconciler maintains global suspension outcome and resumption-safety status.
type Reconciler struct {
	client.Client
	EventRecorder record.EventRecorder
	APIReader     client.Reader
	Sandbox       *agenticrun.SandboxManager
	Now           func() time.Time
}

// +kubebuilder:rbac:groups=agentic.openshift.io,resources=agenticolsconfigs,verbs=get;list;watch
// +kubebuilder:rbac:groups=agentic.openshift.io,resources=agenticolsconfigs/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=agentic.openshift.io,resources=agenticruns,verbs=get;list;watch
// +kubebuilder:rbac:groups=agentic.openshift.io,resources=agenticruns/status,verbs=get;patch;update

func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	if r.APIReader == nil {
		return ctrl.Result{}, errors.New("uncached API reader is required by the global suspension controller")
	}
	if r.Client == nil {
		return ctrl.Result{}, errors.New("client is required by the global suspension controller")
	}

	var config agenticv1alpha1.AgenticOLSConfig
	if err := r.APIReader.Get(ctx, req.NamespacedName, &config); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !config.Spec.Suspended {
		return r.handleDeactivation(ctx, req.NamespacedName)
	}

	if r.Sandbox == nil {
		return ctrl.Result{}, errors.New("sandbox manager is required by the global suspension controller")
	}

	activeConfig, start, freshActivation, active, err := r.ensureActivationStatus(ctx, req.NamespacedName)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !active {
		if activeConfig != nil && !activeConfig.Spec.Suspended {
			return r.handleDeactivation(ctx, req.NamespacedName)
		}
		return ctrl.Result{RequeueAfter: requeueDelay}, nil
	}
	if freshActivation && r.EventRecorder != nil {
		r.EventRecorder.Event(
			activeConfig,
			corev1.EventTypeWarning,
			eventReasonSuspensionActivated,
			"System suspension accepted; workload and access cleanup is in progress",
		)
	}

	return r.reconcileSuspension(ctx, activeConfig, start)
}

func (r *Reconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Reconciler) ensureActivationStatus(
	ctx context.Context,
	key types.NamespacedName,
) (*agenticv1alpha1.AgenticOLSConfig, metav1.Time, bool, bool, error) {
	for range statusPatchAttempts {
		current := &agenticv1alpha1.AgenticOLSConfig{}
		if err := r.APIReader.Get(ctx, key, current); err != nil {
			if apierrors.IsNotFound(err) {
				return nil, metav1.Time{}, false, false, nil
			}
			return nil, metav1.Time{}, false, false, fmt.Errorf("read AgenticOLSConfig through uncached API reader: %w", err)
		}
		if !current.Spec.Suspended {
			return current, metav1.Time{}, false, false, nil
		}

		suspended := meta.FindStatusCondition(current.Status.Conditions, agenticv1alpha1.AgenticOLSConfigConditionSuspended)
		currentActivation := suspended != nil &&
			suspended.Status == metav1.ConditionTrue &&
			suspended.ObservedGeneration == current.Generation &&
			!suspended.LastTransitionTime.IsZero()
		start := activationStart(current, r.now())
		safe := meta.FindStatusCondition(current.Status.Conditions, agenticv1alpha1.AgenticOLSConfigConditionSafeToResume)
		needsSafetyCondition := safe == nil || safe.ObservedGeneration != current.Generation
		if currentActivation && !needsSafetyCondition {
			return current, start, false, true, nil
		}

		base := current.DeepCopy()
		freshActivation := !currentActivation
		if freshActivation {
			setConditionAt(&current.Status.Conditions, metav1.Condition{
				Type:               agenticv1alpha1.AgenticOLSConfigConditionSuspended,
				Status:             metav1.ConditionTrue,
				Reason:             reasonStopping,
				Message:            "Suspension accepted; global workload and access cleanup is in progress",
				ObservedGeneration: current.Generation,
			}, start)
			setConditionAt(&current.Status.Conditions, metav1.Condition{
				Type:               agenticv1alpha1.AgenticOLSConfigConditionSafeToResume,
				Status:             metav1.ConditionFalse,
				Reason:             reasonStopping,
				Message:            "Cleanup and durable run-stop acceptance are not yet confirmed",
				ObservedGeneration: current.Generation,
			}, start)
		} else {
			meta.SetStatusCondition(&current.Status.Conditions, metav1.Condition{
				Type:               agenticv1alpha1.AgenticOLSConfigConditionSafeToResume,
				Status:             metav1.ConditionFalse,
				Reason:             reasonStopping,
				Message:            "Current-generation resumption safety has not been confirmed",
				ObservedGeneration: current.Generation,
			})
			meta.FindStatusCondition(current.Status.Conditions, agenticv1alpha1.AgenticOLSConfigConditionSafeToResume).LastTransitionTime = start
		}

		err := r.Status().Patch(ctx, current, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
		if apierrors.IsConflict(err) {
			continue
		}
		if err != nil {
			return nil, metav1.Time{}, false, false, fmt.Errorf("persist current-generation suspension acceptance: %w", err)
		}
		return current, start, freshActivation, true, nil
	}
	return nil, metav1.Time{}, false, false, errors.New("persist current-generation suspension acceptance: status conflict retry limit reached")
}

func (r *Reconciler) handleDeactivation(ctx context.Context, key types.NamespacedName) (ctrl.Result, error) {
	for range statusPatchAttempts {
		current := &agenticv1alpha1.AgenticOLSConfig{}
		if err := r.APIReader.Get(ctx, key, current); err != nil {
			if apierrors.IsNotFound(err) {
				return ctrl.Result{}, nil
			}
			return ctrl.Result{}, fmt.Errorf("read AgenticOLSConfig through uncached API reader: %w", err)
		}
		if current.Spec.Suspended {
			return ctrl.Result{Requeue: true}, nil
		}

		existing := meta.FindStatusCondition(current.Status.Conditions, agenticv1alpha1.AgenticOLSConfigConditionSuspended)
		if existing == nil {
			return ctrl.Result{}, nil
		}
		wasSuspended := existing.Status == metav1.ConditionTrue
		if !wasSuspended && existing.ObservedGeneration == current.Generation {
			return ctrl.Result{}, nil
		}

		base := current.DeepCopy()
		setConditionAt(&current.Status.Conditions, metav1.Condition{
			Type:               agenticv1alpha1.AgenticOLSConfigConditionSuspended,
			Status:             metav1.ConditionFalse,
			Reason:             reasonAdminDeactivated,
			Message:            "System resumed; agentic operations re-enabled",
			ObservedGeneration: current.Generation,
		}, metav1.NewTime(r.now()))
		err := r.Status().Patch(ctx, current, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
		if apierrors.IsConflict(err) {
			continue
		}
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("patch Suspended=False: %w", err)
		}
		if wasSuspended && r.EventRecorder != nil {
			r.EventRecorder.Event(current, corev1.EventTypeNormal, eventReasonSuspensionDeactivated, "System resumed; agentic operations re-enabled")
		}
		return ctrl.Result{}, nil
	}
	return ctrl.Result{}, errors.New("patch Suspended=False: status conflict retry limit reached")
}

type activationEvidence struct {
	cleanup            agenticrun.SuspensionCleanup
	cleanupErr         error
	runInventoryErr    error
	unsavedMarkers     []string
	pendingRunOutcomes []string
	unverifiedRemote   []string
}

func (e activationEvidence) cleanupConfirmed() bool {
	return e.cleanupErr == nil && e.cleanup.Confirmed()
}

func (e activationEvidence) runAcceptanceConfirmed() bool {
	return e.runInventoryErr == nil && len(e.unsavedMarkers) == 0
}

func (e activationEvidence) safeToResume() bool {
	return e.cleanupConfirmed() &&
		e.runAcceptanceConfirmed() &&
		len(e.unverifiedRemote) == 0
}

func (e activationEvidence) unresolved() bool {
	return !e.cleanupConfirmed() ||
		!e.runAcceptanceConfirmed() ||
		len(e.pendingRunOutcomes) > 0 ||
		len(e.unverifiedRemote) > 0
}

func (e activationEvidence) diagnostic(prefix string) string {
	items := make([]string, 0,
		len(e.cleanup.Pending)+len(e.cleanup.Failures)+len(e.cleanup.Unverified)+
			len(e.unsavedMarkers)+len(e.pendingRunOutcomes)+len(e.unverifiedRemote)+2,
	)
	for _, item := range e.cleanup.Pending {
		items = append(items, "cleanup pending: "+item)
	}
	for _, item := range e.cleanup.Failures {
		items = append(items, "cleanup failed: "+item)
	}
	for _, item := range e.cleanup.Unverified {
		items = append(items, "cleanup unverified: "+item)
	}
	items = append(items, e.unsavedMarkers...)
	items = append(items, e.pendingRunOutcomes...)
	items = append(items, e.unverifiedRemote...)
	if e.cleanupErr != nil {
		items = append(items, "cleanup pass returned an error")
	}
	if e.runInventoryErr != nil {
		items = append(items, "run inventory unavailable")
	}
	items = sortedUnique(items)

	counts := fmt.Sprintf(
		"%d cleanup pending, %d cleanup failures, %d cleanup unverified, %d unsaved stop markers, %d run outcomes pending, %d remote executions/access unverified",
		len(e.cleanup.Pending),
		len(e.cleanup.Failures),
		len(e.cleanup.Unverified),
		len(e.unsavedMarkers),
		len(e.pendingRunOutcomes),
		len(e.unverifiedRemote),
	)
	if e.cleanupErr != nil {
		counts += ", cleanup pass error"
	}
	if e.runInventoryErr != nil {
		counts += ", run inventory unavailable"
	}
	message := prefix + ": " + counts
	if len(items) != 0 {
		selected := items
		if len(selected) > maxDiagnosticItems {
			selected = append(append([]string(nil), selected[:maxDiagnosticItems]...), fmt.Sprintf("%d additional unresolved items", len(items)-maxDiagnosticItems))
		}
		message += "; unresolved: " + strings.Join(selected, "; ")
	}
	return boundedDiagnostic(message)
}

func sortedUnique(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	sort.Strings(values)
	write := 0
	for _, value := range values {
		if write != 0 && values[write-1] == value {
			continue
		}
		values[write] = value
		write++
	}
	return values[:write]
}

func boundedDiagnostic(message string) string {
	if len(message) <= maxDiagnosticLength {
		return message
	}
	runes := []rune(message)
	if len(runes) <= maxDiagnosticLength {
		return message
	}
	return string(runes[:maxDiagnosticLength-3]) + "..."
}

func (r *Reconciler) reconcileSuspension(
	ctx context.Context,
	config *agenticv1alpha1.AgenticOLSConfig,
	start metav1.Time,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	now := r.now()
	passCtx, cancelPass, initialWindowPass := boundedCleanupContext(ctx, start, now)
	var evidence activationEvidence

	var runs agenticv1alpha1.AgenticRunList
	if err := r.APIReader.List(passCtx, &runs); err != nil {
		evidence.runInventoryErr = err
		log.Error(err, "cannot read run inventory during global suspension")
	} else {
		for i := range runs.Items {
			run := &runs.Items[i]
			terminalSnapshot := isTerminal(agenticv1alpha1.DerivePhase(run.Status.Conditions))
			initiallyNonTerminal := !terminalSnapshot
			acceptStop := initiallyNonTerminal || terminalRunMayBeAffected(run, start)
			current := run
			markerSaved := false
			if acceptStop {
				if run.UID == "" {
					evidence.unsavedMarkers = append(evidence.unsavedMarkers,
						fmt.Sprintf("stop marker not persisted for %s/%s: run UID is missing", run.Namespace, run.Name))
				} else {
					var disappeared bool
					var err error
					current, markerSaved, disappeared, err = r.persistRunStop(passCtx, run, start)
					if err != nil {
						log.Error(
							err,
							"cannot persist run stop acceptance",
							"run", types.NamespacedName{Namespace: run.Namespace, Name: run.Name},
							"uid", run.UID,
						)
						evidence.unsavedMarkers = append(evidence.unsavedMarkers,
							fmt.Sprintf("stop marker not persisted for %s/%s uid=%s", run.Namespace, run.Name, run.UID))
						current = run
					} else if disappeared {
						continue
					} else if !markerSaved {
						evidence.unsavedMarkers = append(evidence.unsavedMarkers,
							fmt.Sprintf("stop marker not persisted for %s/%s uid=%s", run.Namespace, run.Name, run.UID))
					}
				}
			}

			if remoteEvidence := knownRemoteEvidence(current); remoteEvidence != "" {
				evidence.unverifiedRemote = append(evidence.unverifiedRemote, remoteEvidence)
			}
			if initiallyNonTerminal && markerSaved && current != nil &&
				!isTerminal(agenticv1alpha1.DerivePhase(current.Status.Conditions)) &&
				!hasFinalRunStopOutcome(current) {
				evidence.pendingRunOutcomes = append(evidence.pendingRunOutcomes,
					fmt.Sprintf("run outcome pending: %s/%s uid=%s", current.Namespace, current.Name, current.UID))
			}
		}
	}

	report, cleanupErr := r.Sandbox.CleanupSuspension(passCtx, "")
	evidence.cleanup = report
	evidence.cleanupErr = cleanupErr
	passErr := passCtx.Err()
	cancelPass()

	now = r.now()
	deadlineReached := !now.Before(start.Add(agenticrun.SuspensionStopWindow))
	if initialWindowPass &&
		ctx.Err() == nil &&
		(errors.Is(passErr, context.DeadlineExceeded) ||
			errors.Is(evidence.runInventoryErr, context.DeadlineExceeded) ||
			errors.Is(evidence.cleanupErr, context.DeadlineExceeded)) {
		deadlineReached = true
	}
	if deadlineReached && now.Before(start.Add(agenticrun.SuspensionStopWindow)) {
		now = start.Add(agenticrun.SuspensionStopWindow)
	}
	if err := r.patchActivationOutcome(ctx, config.UID, config.Generation, start, evidence, deadlineReached); err != nil {
		return ctrl.Result{}, err
	}
	log.V(1).Info(
		"global suspension pass completed",
		"cleanupConfirmed", evidence.cleanupConfirmed(),
		"runAcceptanceConfirmed", evidence.runAcceptanceConfirmed(),
		"safeToResume", evidence.safeToResume(),
		"unresolved", evidence.unresolved(),
		"deadlineReached", deadlineReached,
	)
	return ctrl.Result{RequeueAfter: suspensionRequeueDelay(start, now)}, nil
}

func boundedCleanupContext(ctx context.Context, start metav1.Time, now time.Time) (context.Context, context.CancelFunc, bool) {
	remaining := start.Add(agenticrun.SuspensionStopWindow).Sub(now)
	if remaining > 0 {
		passCtx, cancel := context.WithTimeout(ctx, remaining)
		return passCtx, cancel, true
	}
	passCtx, cancel := context.WithTimeout(ctx, backgroundCleanupPassTimeout)
	return passCtx, cancel, false
}

func suspensionRequeueDelay(start metav1.Time, now time.Time) time.Duration {
	remaining := start.Add(agenticrun.SuspensionStopWindow).Sub(now)
	if remaining > 0 && remaining < requeueDelay {
		return remaining
	}
	return requeueDelay
}

func (r *Reconciler) persistRunStop(
	passCtx context.Context,
	snapshot *agenticv1alpha1.AgenticRun,
	start metav1.Time,
) (*agenticv1alpha1.AgenticRun, bool, bool, error) {
	key := client.ObjectKeyFromObject(snapshot)
	for range statusPatchAttempts {
		current := &agenticv1alpha1.AgenticRun{}
		if err := r.APIReader.Get(passCtx, key, current); err != nil {
			if apierrors.IsNotFound(err) {
				return nil, false, true, nil
			}
			return nil, false, false, fmt.Errorf("read run through uncached API reader: %w", err)
		}
		if current.UID != snapshot.UID {
			return nil, false, true, nil
		}
		if hasAcceptedRunStop(current) {
			return current, true, false, nil
		}

		// The caller only invokes this for a run known to be affected by the
		// activation. Adding this non-phase marker to a newly terminal run
		// preserves its workflow outcome while preventing later revision.

		base := current.DeepCopy()
		setConditionAt(&current.Status.Conditions, metav1.Condition{
			Type:               agenticv1alpha1.AgenticRunConditionStopping,
			Status:             metav1.ConditionTrue,
			Reason:             reasonSystemSuspended,
			Message:            "Stop accepted by global system suspension",
			ObservedGeneration: current.Generation,
		}, start)
		err := r.Status().Patch(passCtx, current, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
		if apierrors.IsConflict(err) {
			continue
		}
		if err != nil {
			return nil, false, false, fmt.Errorf("patch stop acceptance: %w", err)
		}
		return current, true, false, nil
	}
	return nil, false, false, errors.New("status conflict retry limit reached")
}

// terminalRunMayBeAffected preserves definitively pre-activation outcomes;
// missing or current-activation TerminalTime remains an affected cohort.
func terminalRunMayBeAffected(run *agenticv1alpha1.AgenticRun, start metav1.Time) bool {
	terminalTime := run.Status.TerminalTime
	return terminalTime == nil || terminalTime.IsZero() || !terminalTime.Time.Before(start.Time)
}

func hasAcceptedRunStop(run *agenticv1alpha1.AgenticRun) bool {
	stopping := meta.FindStatusCondition(run.Status.Conditions, agenticv1alpha1.AgenticRunConditionStopping)
	if stopping != nil {
		if stopping.Status == metav1.ConditionTrue && stopping.Reason == reasonSystemSuspended {
			return true
		}
		if stopping.Status == metav1.ConditionFalse &&
			(stopping.Reason == reasonTeardownComplete || stopping.Reason == reasonTeardownFailed) {
			return true
		}
	}
	emergency := meta.FindStatusCondition(run.Status.Conditions, agenticv1alpha1.AgenticRunConditionEmergencyStopped)
	if emergency != nil && emergency.Status == metav1.ConditionTrue {
		return true
	}
	for i := range run.Status.Conditions {
		condition := &run.Status.Conditions[i]
		if condition.Status == metav1.ConditionFalse && condition.Reason == reasonEmergencyStopFailed {
			return true
		}
	}
	return false
}

func hasFinalRunStopOutcome(run *agenticv1alpha1.AgenticRun) bool {
	stopping := meta.FindStatusCondition(run.Status.Conditions, agenticv1alpha1.AgenticRunConditionStopping)
	if stopping != nil && stopping.Status == metav1.ConditionFalse &&
		(stopping.Reason == reasonTeardownComplete || stopping.Reason == reasonTeardownFailed) {
		return true
	}
	emergency := meta.FindStatusCondition(run.Status.Conditions, agenticv1alpha1.AgenticRunConditionEmergencyStopped)
	if emergency != nil && emergency.Status == metav1.ConditionTrue {
		return true
	}
	for i := range run.Status.Conditions {
		condition := &run.Status.Conditions[i]
		if condition.Status == metav1.ConditionFalse && condition.Reason == reasonEmergencyStopFailed {
			return true
		}
	}
	return false
}

func knownRemoteEvidence(run *agenticv1alpha1.AgenticRun) string {
	if run == nil || run.Spec.TargetCluster == "" {
		return ""
	}
	evidence := make([]string, 0, 5)
	for _, step := range []struct {
		name string
		info agenticv1alpha1.SandboxInfo
	}{
		{name: "analysis", info: run.Status.Steps.Analysis.Sandbox},
		{name: "execution", info: run.Status.Steps.Execution.Sandbox},
		{name: "verification", info: run.Status.Steps.Verification.Sandbox},
		{name: "escalation", info: run.Status.Steps.Escalation.Sandbox},
	} {
		if step.info.ClaimName != "" {
			evidence = append(evidence, fmt.Sprintf("%s sandbox %s/%s", step.name, step.info.Namespace, step.info.ClaimName))
		}
	}
	if run.Annotations[runRBACNamespacesAnnotation] != "" {
		evidence = append(evidence, "remote execution RBAC annotation")
	}
	for _, condition := range run.Status.Conditions {
		if condition.Status == metav1.ConditionUnknown {
			evidence = append(evidence, "active workflow condition "+condition.Type)
			break
		}
	}
	if len(evidence) == 0 {
		return ""
	}
	sort.Strings(evidence)
	return boundedDiagnostic(fmt.Sprintf(
		"remote execution/access unverified for %s/%s uid=%s target=%s: %s",
		run.Namespace, run.Name, run.UID, run.Spec.TargetCluster, strings.Join(evidence, ", "),
	))
}

func (r *Reconciler) patchActivationOutcome(
	ctx context.Context,
	configUID types.UID,
	generation int64,
	start metav1.Time,
	evidence activationEvidence,
	deadlineReached bool,
) error {
	key := types.NamespacedName{Name: "cluster"}
	for range statusPatchAttempts {
		current := &agenticv1alpha1.AgenticOLSConfig{}
		if err := r.APIReader.Get(ctx, key, current); err != nil {
			if apierrors.IsNotFound(err) {
				return nil
			}
			return fmt.Errorf("read AgenticOLSConfig before outcome patch: %w", err)
		}
		if (configUID != "" && current.UID != configUID) ||
			!current.Spec.Suspended ||
			current.Generation != generation {
			return nil
		}
		suspended := meta.FindStatusCondition(current.Status.Conditions, agenticv1alpha1.AgenticOLSConfigConditionSuspended)
		if suspended == nil || suspended.Status != metav1.ConditionTrue ||
			suspended.ObservedGeneration != generation ||
			!suspended.LastTransitionTime.Equal(&start) {
			return nil
		}

		reason, message := outcomeForEvidence(evidence, suspended.Reason, deadlineReached)
		safeStatus := metav1.ConditionFalse
		safeReason := reasonStopping
		safeMessage := evidence.diagnostic("Resumption remains blocked")
		if evidence.safeToResume() {
			safeStatus = metav1.ConditionTrue
			safeReason = reasonCleanupConfirmed
			safeMessage = "Observed workload/access cleanup and durable run-stop acceptance are confirmed"
		}
		base := current.DeepCopy()
		setConditionAt(&current.Status.Conditions, metav1.Condition{
			Type:               agenticv1alpha1.AgenticOLSConfigConditionSuspended,
			Status:             metav1.ConditionTrue,
			Reason:             reason,
			Message:            message,
			ObservedGeneration: generation,
		}, start)
		previousSafe := meta.FindStatusCondition(base.Status.Conditions, agenticv1alpha1.AgenticOLSConfigConditionSafeToResume)
		meta.SetStatusCondition(&current.Status.Conditions, metav1.Condition{
			Type:               agenticv1alpha1.AgenticOLSConfigConditionSafeToResume,
			Status:             safeStatus,
			Reason:             safeReason,
			Message:            safeMessage,
			ObservedGeneration: generation,
		})
		if previousSafe == nil || previousSafe.Status != safeStatus {
			meta.FindStatusCondition(current.Status.Conditions, agenticv1alpha1.AgenticOLSConfigConditionSafeToResume).LastTransitionTime = metav1.NewTime(r.now())
		}
		if reflect.DeepEqual(base.Status.Conditions, current.Status.Conditions) {
			return nil
		}
		err := r.Status().Patch(ctx, current, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
		if apierrors.IsConflict(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("patch global suspension outcome and safety: %w", err)
		}
		return nil
	}
	return errors.New("patch global suspension outcome and safety: status conflict retry limit reached")
}

func outcomeForEvidence(evidence activationEvidence, currentReason string, deadlineReached bool) (string, string) {
	if currentReason == reasonAdminActivatedWithErrors {
		if evidence.unresolved() {
			return reasonAdminActivatedWithErrors, evidence.diagnostic("Suspension completed with errors")
		}
		return reasonAdminActivatedWithErrors, "Suspension completed with errors; current cleanup and stop-acceptance safety are confirmed"
	}
	if evidence.unresolved() {
		if deadlineReached {
			return reasonAdminActivatedWithErrors, evidence.diagnostic("Suspension completed with errors")
		}
		return reasonStopping, evidence.diagnostic("Suspension stopping")
	}
	return reasonAdminActivated, "System suspended; cleanup and affected run outcomes are confirmed"
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
