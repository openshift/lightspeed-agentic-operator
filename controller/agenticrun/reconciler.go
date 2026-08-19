package agenticrun

import (
	"context"
	"fmt"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
	"github.com/openshift/lightspeed-agentic-operator/pkg/configuration"
)

const (
	ErrRemoveFinalizer             = "remove finalizer"
	ErrAddFinalizer                = "add finalizer"
	ErrPatchTemplogCleanupAttempts = "patch templog cleanup attempts"
	ErrPatchRBACCleanupAttempts    = "patch rbac cleanup attempts"
	ErrMigrateLeftoverToEscalation = "migrate leftover verification-failed run to escalation"
	ErrStampTerminalTTL            = "stamp terminal TTL"
	ErrDeleteExpiredRun            = "delete expired run"
)

// TempLogCleaner is the interface for deleting templog records on CR deletion.
type TempLogCleaner interface {
	DeleteLogs(ctx context.Context, traceID string) error
}

// AgenticRunReconciler reconciles AgenticRun objects.
//
// Agent must be set before calling SetupWithManager.
type AgenticRunReconciler struct {
	client.Client
	Agent     AgentCaller
	Config    *configuration.Cache
	Namespace string
	Audit     AuditLogger
	TempLog   TempLogCleaner
}

// +kubebuilder:rbac:groups=agentic.openshift.io,resources=agenticruns,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=agentic.openshift.io,resources=agenticruns/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=agentic.openshift.io,resources=agenticruns/finalizers,verbs=update
// +kubebuilder:rbac:groups=agentic.openshift.io,resources=agents,verbs=get;list;watch
// +kubebuilder:rbac:groups=agentic.openshift.io,resources=llmproviders,verbs=get;list;watch
// +kubebuilder:rbac:groups=agentic.openshift.io,resources=agenticrunapprovals,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=agentic.openshift.io,resources=agenticrunapprovals/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=agentic.openshift.io,resources=approvalpolicies,verbs=get;list;watch
// +kubebuilder:rbac:groups=agentic.openshift.io,resources=analysisresults,verbs=get;list;watch;create
// +kubebuilder:rbac:groups=agentic.openshift.io,resources=executionresults,verbs=get;list;watch;create
// +kubebuilder:rbac:groups=agentic.openshift.io,resources=verificationresults,verbs=get;list;watch;create
// +kubebuilder:rbac:groups=agentic.openshift.io,resources=escalationresults,verbs=get;list;watch;create
// +kubebuilder:rbac:groups=agentic.openshift.io,resources=analysisresults/status;executionresults/status;verificationresults/status;escalationresults/status,verbs=get;patch;update
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=roles;rolebindings,verbs=get;create;delete
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterroles;clusterrolebindings,verbs=get;list;create;update;delete
// +kubebuilder:rbac:groups=agentic.openshift.io,resources=agenticolsconfigs,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch

func (r *AgenticRunReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var run agenticv1alpha1.AgenticRun
	if err := r.Get(ctx, req.NamespacedName, &run); err != nil {
		if client.IgnoreNotFound(err) == nil && r.Audit != nil {
			r.Audit.CleanupDeleted(req.NamespacedName)
		}
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// --- Deletion ---
	if !run.DeletionTimestamp.IsZero() {
		if r.Audit != nil {
			r.Audit.Cleanup(&run)
		}
		if controllerutil.ContainsFinalizer(&run, rbacCleanupFinalizer) {
			return r.handleRBACCleanup(ctx, &run)
		}
		if controllerutil.ContainsFinalizer(&run, templogCleanupFinalizer) {
			return r.handleTemplogCleanup(ctx, &run)
		}
		return ctrl.Result{}, nil
	}

	// --- Finalizers (first sight of any non-deleting run, including terminal) ---
	if !controllerutil.ContainsFinalizer(&run, rbacCleanupFinalizer) || !controllerutil.ContainsFinalizer(&run, templogCleanupFinalizer) {
		original := run.DeepCopy()
		controllerutil.AddFinalizer(&run, rbacCleanupFinalizer)
		controllerutil.AddFinalizer(&run, templogCleanupFinalizer)
		if err := r.Patch(ctx, &run, client.MergeFrom(original)); err != nil {
			return ctrl.Result{}, fmt.Errorf("%s: %w", ErrAddFinalizer, err)
		}
		if err := r.Get(ctx, req.NamespacedName, &run); err != nil {
			return ctrl.Result{}, client.IgnoreNotFound(err)
		}
	}

	// --- Upgrade shim (OLS-3817): rescue runs stranded mid-retry ---
	// A previous operator version marked a failed verification pending a retry
	// with Verified=False, reason "RetryingExecution", and no Escalated
	// condition. Retries are gone, and DerivePhase now maps that condition set
	// to Failed (terminal) -- which would strand such a leftover run without
	// ever escalating. Convert it onto the escalation path by writing the same
	// Escalated=Unknown/VerificationFailed condition a fresh verification
	// failure now sets. The reason match is deliberately narrow: a genuine
	// system failure during verification uses reason "Failed" and must stay
	// terminal. "RetryingExecution" is a historical value no current code
	// writes, so this only ever matches pre-upgrade leftovers.
	const leftoverRetryReason = "RetryingExecution"
	if verified := meta.FindStatusCondition(run.Status.Conditions, agenticv1alpha1.AgenticRunConditionVerified); verified != nil &&
		verified.Status == metav1.ConditionFalse &&
		verified.Reason == leftoverRetryReason &&
		meta.FindStatusCondition(run.Status.Conditions, agenticv1alpha1.AgenticRunConditionEscalated) == nil {
		base := run.DeepCopy()
		meta.SetStatusCondition(&run.Status.Conditions, metav1.Condition{
			Type:               agenticv1alpha1.AgenticRunConditionEscalated,
			Status:             metav1.ConditionUnknown,
			Reason:             agenticv1alpha1.ReasonVerificationFailed,
			Message:            "Verification failed under a previous operator version, escalating",
			ObservedGeneration: run.Generation,
		})
		if err := r.statusPatch(ctx, &run, base); err != nil {
			return ctrl.Result{}, fmt.Errorf("%s: %w", ErrMigrateLeftoverToEscalation, err)
		}
		return ctrl.Result{Requeue: true}, nil
	}

	phase := agenticv1alpha1.DerivePhase(run.Status.Conditions)

	// --- Terminal phases (before suspension guard so audit cleanup always runs) ---
	switch phase {
	case agenticv1alpha1.AgenticRunPhaseNoActionRequired:
		if !needsRevision(&run) {
			return r.handleTerminalCleanup(ctx, &run, phase)
		}

	case agenticv1alpha1.AgenticRunPhaseCompleted:
		if !(run.Spec.Execution.IsZero() && needsRevision(&run)) {
			return r.handleTerminalCleanup(ctx, &run, phase)
		}

	case agenticv1alpha1.AgenticRunPhaseDenied,
		agenticv1alpha1.AgenticRunPhaseEscalated,
		agenticv1alpha1.AgenticRunPhaseEmergencyStopped:
		return r.handleTerminalCleanup(ctx, &run, phase)

	case agenticv1alpha1.AgenticRunPhaseFailed:
		if !(run.Spec.Execution.IsZero() && needsRevision(&run)) {
			if result, err := r.handleFailed(ctx, &run); err != nil {
				return result, err
			}
			return r.handleTerminalCleanup(ctx, &run, phase)
		}
	}

	// --- Configuration guard (wait for lightspeed-agentic-configuration ConfigMap) ---
	if r.Config != nil && !r.Config.Available() {
		log.Info("operator configuration not yet available, skipping")
		return ctrl.Result{}, nil
	}

	// --- Suspension guard (non-terminal runs and advisory-only Completed runs needing revision reach here) ---
	suspended, err := isSuspended(ctx, r.Client)
	if err != nil {
		return ctrl.Result{}, err
	}
	if suspended {
		return r.handleSuspension(ctx, &run)
	}

	// --- Ensure AgenticRunApproval exists ---
	policy, err := getApprovalPolicy(ctx, r.Client)
	if err != nil {
		log.Error(err, "failed to get ApprovalPolicy")
	}

	approval, err := ensureAgenticRunApproval(ctx, r.Client, &run, policy)
	if err != nil {
		log.Error(err, "failed to ensure AgenticRunApproval")
		return ctrl.Result{Requeue: true}, nil
	}

	// --- Resolve agents/LLMs ---
	resolved, err := resolveAgenticRun(ctx, r.Client, &run, approval)
	if err != nil {
		log.Error(err, "workflow resolution failed")
		base := run.DeepCopy()
		meta.SetStatusCondition(&run.Status.Conditions, metav1.Condition{
			Type:               agenticv1alpha1.AgenticRunConditionAnalyzed,
			Status:             metav1.ConditionFalse,
			Reason:             reasonWorkflowFailed,
			Message:            err.Error(),
			ObservedGeneration: run.Generation,
		})
		if statusErr := r.statusPatch(ctx, &run, base); statusErr != nil {
			log.Error(statusErr, "failed to patch status after workflow resolution failure")
		}
		return ctrl.Result{}, nil
	}

	log.V(1).Info("reconciling", LogKeyPhase, phase)

	// --- Phase routing ---
	switch phase {
	case agenticv1alpha1.AgenticRunPhasePending, agenticv1alpha1.AgenticRunPhaseAnalyzing:
		if needsRevision(&run) {
			return r.handleRevision(ctx, &run, resolved)
		}
		return r.handleAnalysis(ctx, &run, resolved, approval, policy)

	case agenticv1alpha1.AgenticRunPhaseProposed, agenticv1alpha1.AgenticRunPhaseExecuting:
		if needsRevision(&run) {
			return r.handleRevision(ctx, &run, resolved)
		}
		return r.handleExecution(ctx, &run, resolved, approval, policy)

	case agenticv1alpha1.AgenticRunPhaseVerifying:
		return r.handleVerification(ctx, &run, resolved, approval, policy)

	case agenticv1alpha1.AgenticRunPhaseEscalating:
		if needsRevision(&run) {
			return r.handleRevision(ctx, &run, resolved)
		}
		return r.handleEscalation(ctx, &run, resolved, approval, policy)

	case agenticv1alpha1.AgenticRunPhaseNoActionRequired:
		if needsRevision(&run) {
			return r.handleRevision(ctx, &run, resolved)
		}
		return ctrl.Result{}, nil

	case agenticv1alpha1.AgenticRunPhaseCompleted,
		agenticv1alpha1.AgenticRunPhaseFailed:
		if run.Spec.Execution.IsZero() && needsRevision(&run) {
			return r.handleRevision(ctx, &run, resolved)
		}
		return ctrl.Result{}, nil

	default:
		log.V(1).Info("unhandled phase, no-op", LogKeyPhase, phase)
		return ctrl.Result{}, nil
	}
}

// SetupWithManager sets up the controller with the Manager.
func (r *AgenticRunReconciler) SetupWithManager(mgr ctrl.Manager) error {
	maxConcurrent := int(agenticv1alpha1.DefaultMaxConcurrentRuns)
	var ap agenticv1alpha1.ApprovalPolicy
	if err := mgr.GetAPIReader().Get(context.Background(), client.ObjectKey{Name: "cluster"}, &ap); err == nil {
		if ap.Spec.MaxConcurrentRuns > 0 {
			maxConcurrent = int(ap.Spec.MaxConcurrentRuns)
		}
	}
	fanOutToActiveRuns := func(ctx context.Context, _ client.Object) []ctrl.Request {
		var runs agenticv1alpha1.AgenticRunList
		if err := r.List(ctx, &runs); err != nil {
			return nil
		}
		// Only re-enqueue terminal runs for a missing ttlAfterTerminal stamp
		// when there's actually a cluster default to stamp -- otherwise
		// every terminal run with no cluster TTL configured would be
		// re-enqueued on every ApprovalPolicy/AgenticOLSConfig/ConfigMap
		// change forever, for no effect.
		clusterTTL, err := getTerminalTTL(ctx, r.Client)
		if err != nil {
			clusterTTL = nil
		}
		var reqs []ctrl.Request
		for _, p := range runs.Items {
			phase := agenticv1alpha1.DerivePhase(p.Status.Conditions)
			// Enqueue non-terminal runs (normal workflow), terminal runs
			// still missing terminalTime (stamped unconditionally), and
			// terminal runs missing ttlAfterTerminal only when a cluster
			// default currently exists to stamp.
			needsTTLStamp := clusterTTL != nil && p.Spec.TTLAfterTerminal == nil
			if !isTerminal(phase) || p.Status.TerminalTime == nil || needsTTLStamp {
				reqs = append(reqs, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(&p)})
			}
		}
		return reqs
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&agenticv1alpha1.AgenticRun{}).
		Owns(&agenticv1alpha1.AgenticRunApproval{}).
		Owns(&agenticv1alpha1.AnalysisResult{}).
		Owns(&agenticv1alpha1.ExecutionResult{}).
		Owns(&agenticv1alpha1.VerificationResult{}).
		Owns(&agenticv1alpha1.EscalationResult{}).
		Watches(&agenticv1alpha1.ApprovalPolicy{}, handler.EnqueueRequestsFromMapFunc(fanOutToActiveRuns)).
		Watches(&agenticv1alpha1.AgenticOLSConfig{}, handler.EnqueueRequestsFromMapFunc(fanOutToActiveRuns)).
		Watches(&corev1.ConfigMap{}, handler.EnqueueRequestsFromMapFunc(
			func(ctx context.Context, obj client.Object) []ctrl.Request {
				if obj.GetNamespace() != r.Namespace || obj.GetName() != configuration.ConfigMapName {
					return nil
				}
				return fanOutToActiveRuns(ctx, obj)
			},
		)).
		Named("agenticrun").
		WithOptions(controller.Options{MaxConcurrentReconciles: maxConcurrent}).
		Complete(r)
}

// handleTerminalCleanup performs common cleanup for all terminal phases: releases
// sandbox claims, emits audit spans, and delegates to TTL handling.
func (r *AgenticRunReconciler) handleTerminalCleanup(ctx context.Context, run *agenticv1alpha1.AgenticRun, phase agenticv1alpha1.AgenticRunPhase) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	if hasSandboxClaims(run) {
		if err := r.Agent.ReleaseSandboxes(ctx, run); err != nil {
			log.Error(err, "sandbox cleanup failed at terminal phase")
		}
	}
	if r.Audit != nil {
		r.Audit.EmitTerminalSpan(ctx, run, string(phase), terminalReason(run))
		r.Audit.Cleanup(run)
	}
	if result, requeue, err := r.handleTerminalTTL(ctx, run); requeue || err != nil {
		return result, err
	}
	return ctrl.Result{}, nil
}

// handleTerminalTTL stamps terminalTime and ttlAfterTerminal on a terminal run,
// then checks whether the TTL has expired. If expired, it deletes the AgenticRun
// CR (Kubernetes GC cascades to owned resources). If not expired, it returns a
// RequeueAfter for the remaining TTL. Returns (result, requeue, error) where
// requeue=true means the caller should return the result instead of continuing.
func (r *AgenticRunReconciler) handleTerminalTTL(ctx context.Context, run *agenticv1alpha1.AgenticRun) (ctrl.Result, bool, error) {
	log := logf.FromContext(ctx)
	now := metav1.Now()

	// --- Stamp terminalTime if not yet set ---
	if run.Status.TerminalTime == nil {
		base := run.DeepCopy()
		run.Status.TerminalTime = &now
		if err := r.statusPatch(ctx, run, base); err != nil {
			log.Error(err, "failed to stamp terminalTime")
			return ctrl.Result{}, false, fmt.Errorf("%s: %w", ErrStampTerminalTTL, err)
		}
	}

	// --- Stamp ttlAfterTerminal from cluster config if not already set ---
	if run.Spec.TTLAfterTerminal == nil {
		clusterTTL, err := getTerminalTTL(ctx, r.Client)
		if err != nil {
			return ctrl.Result{}, false, fmt.Errorf("%s: %w", ErrStampTerminalTTL, err)
		}
		if clusterTTL != nil {
			// run already reflects the latest server state: Patch (like the
			// statusPatch above) decodes the API server's response back into
			// run, so there's no need to re-Get here. Re-Getting through the
			// manager's cached client can race the informer cache and clobber
			// the terminalTime just stamped above with a stale, pre-patch
			// copy, leading to a nil Status.TerminalTime below.
			original := run.DeepCopy()
			run.Spec.TTLAfterTerminal = clusterTTL
			if err := r.Patch(ctx, run, client.MergeFrom(original)); err != nil {
				log.Error(err, "failed to stamp ttlAfterTerminal")
				return ctrl.Result{}, false, fmt.Errorf("%s: %w", ErrStampTerminalTTL, err)
			}
		}
	}

	// --- Idempotent observedGeneration repair ---
	// Patching spec.ttlAfterTerminal bumps metadata.generation. Advance
	// the Analyzed condition's ObservedGeneration in lockstep so this
	// operator-driven mutation isn't mistaken for a user-initiated
	// revision request by needsRevision(). This runs unconditionally
	// (outside the TTLAfterTerminal == nil block) so that a crash between
	// the spec patch and this status patch is self-healing on the next
	// reconcile.
	if analyzed := meta.FindStatusCondition(run.Status.Conditions, agenticv1alpha1.AgenticRunConditionAnalyzed); analyzed != nil && analyzed.ObservedGeneration != run.Generation {
		base := run.DeepCopy()
		analyzed.ObservedGeneration = run.Generation
		if err := r.statusPatch(ctx, run, base); err != nil {
			log.Error(err, "failed to advance observedGeneration after ttlAfterTerminal stamp")
			return ctrl.Result{}, false, fmt.Errorf("%s: %w", ErrStampTerminalTTL, err)
		}
	}

	// --- Evaluate TTL ---
	if run.Spec.TTLAfterTerminal == nil {
		// No TTL configured — no auto-deletion.
		return ctrl.Result{}, false, nil
	}

	ttlSeconds := *run.Spec.TTLAfterTerminal
	if ttlSeconds == 0 {
		// TTL=0 explicitly disables auto-deletion for this run.
		return ctrl.Result{}, false, nil
	}

	terminalTime := run.Status.TerminalTime.Time
	expiry := terminalTime.Add(time.Duration(ttlSeconds) * time.Second)
	remaining := time.Until(expiry)

	if remaining <= 0 {
		log.Info("TTL expired, deleting AgenticRun", LogKeyName, run.Name)
		if err := r.Delete(ctx, run); err != nil {
			if client.IgnoreNotFound(err) == nil {
				return ctrl.Result{}, true, nil
			}
			return ctrl.Result{}, false, fmt.Errorf("%s: %w", ErrDeleteExpiredRun, err)
		}
		return ctrl.Result{}, true, nil
	}

	log.V(1).Info("TTL not yet expired, requeueing", LogKeyName, run.Name, "remaining", remaining)
	return ctrl.Result{RequeueAfter: remaining}, true, nil
}

// handleTemplogCleanup deletes audit logs from the Collector's Postgres store
// for this AgenticRun. Retries up to templogMaxCleanupAttempts, then removes
// the finalizer regardless to unblock CR deletion.
func (r *AgenticRunReconciler) handleTemplogCleanup(ctx context.Context, run *agenticv1alpha1.AgenticRun) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	attempts := 0
	if v, ok := run.Annotations[templogCleanupAttemptsAnnotation]; ok {
		parsed, err := strconv.Atoi(v)
		if err != nil || parsed < 0 {
			// Malformed annotation must not skip cleanup — reset to zero and retry delete.
			log.Info("ignoring invalid templog cleanup attempts annotation", "value", v)
			attempts = 0
		} else {
			attempts = parsed
		}
	}

	if r.TempLog != nil && attempts < templogMaxCleanupAttempts {
		if err := r.TempLog.DeleteLogs(ctx, string(run.UID)); err != nil {
			log.Error(err, "templog cleanup failed, will retry", "attempt", attempts+1, "max", templogMaxCleanupAttempts)
			original := run.DeepCopy()
			if run.Annotations == nil {
				run.Annotations = make(map[string]string)
			}
			run.Annotations[templogCleanupAttemptsAnnotation] = fmt.Sprintf("%d", attempts+1)
			if patchErr := r.Patch(ctx, run, client.MergeFrom(original)); patchErr != nil {
				return ctrl.Result{}, fmt.Errorf("%s: %w", ErrPatchTemplogCleanupAttempts, patchErr)
			}
			return ctrl.Result{RequeueAfter: templogCleanupRequeueAfter}, nil
		}
	} else if attempts >= templogMaxCleanupAttempts {
		log.Info("templog cleanup exhausted retries, removing finalizer with orphaned logs",
			"agenticRunID", string(run.UID))
	}

	original := run.DeepCopy()
	controllerutil.RemoveFinalizer(run, templogCleanupFinalizer)
	if err := r.Patch(ctx, run, client.MergeFrom(original)); err != nil {
		return ctrl.Result{}, fmt.Errorf("%s: %w", ErrRemoveFinalizer, err)
	}
	return ctrl.Result{}, nil
}

// handleRBACCleanup releases sandboxes and deletes execution RBAC resources
// for a deleting AgenticRun. Retries up to rbacMaxCleanupAttempts, then
// removes the finalizer regardless to avoid leaving the CR stuck in Terminating.
func (r *AgenticRunReconciler) handleRBACCleanup(ctx context.Context, run *agenticv1alpha1.AgenticRun) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	attempts := 0
	if v, ok := run.Annotations[rbacCleanupAttemptsAnnotation]; ok {
		parsed, err := strconv.Atoi(v)
		if err != nil || parsed < 0 {
			log.Info("ignoring invalid rbac cleanup attempts annotation", "value", v)
			attempts = 0
		} else {
			attempts = parsed
		}
	}

	if attempts < rbacMaxCleanupAttempts {
		sandboxErr := r.Agent.ReleaseSandboxes(ctx, run)
		rbacErr := cleanupExecutionRBAC(ctx, r.Client, run, r.Namespace)
		if sandboxErr != nil || rbacErr != nil {
			if sandboxErr != nil {
				log.Error(sandboxErr, "sandbox release failed, will retry", "attempt", attempts+1, "max", rbacMaxCleanupAttempts)
			}
			if rbacErr != nil {
				log.Error(rbacErr, "RBAC cleanup failed, will retry", "attempt", attempts+1, "max", rbacMaxCleanupAttempts)
			}
			original := run.DeepCopy()
			if run.Annotations == nil {
				run.Annotations = make(map[string]string)
			}
			run.Annotations[rbacCleanupAttemptsAnnotation] = fmt.Sprintf("%d", attempts+1)
			if patchErr := r.Patch(ctx, run, client.MergeFrom(original)); patchErr != nil {
				return ctrl.Result{}, fmt.Errorf("%s: %w", ErrPatchRBACCleanupAttempts, patchErr)
			}
			return ctrl.Result{RequeueAfter: rbacCleanupRequeueAfter}, nil
		}
	} else {
		log.Info("RBAC cleanup exhausted retries, removing finalizer with orphaned resources", "attempts", attempts)
	}

	original := run.DeepCopy()
	controllerutil.RemoveFinalizer(run, rbacCleanupFinalizer)
	if err := r.Patch(ctx, run, client.MergeFrom(original)); err != nil {
		return ctrl.Result{}, fmt.Errorf("%s: %w", ErrRemoveFinalizer, err)
	}
	// Requeue so templog cleanup Patches a fresh object (avoids conflict
	// from two sequential Patches in one reconcile).
	return ctrl.Result{Requeue: true}, nil
}
