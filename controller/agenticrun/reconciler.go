package agenticrun

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
)

const (
	ErrRemoveFinalizer             = "remove finalizer"
	ErrAddFinalizer                = "add finalizer"
	ErrPatchTemplogCleanupAttempts = "patch templog cleanup attempts"
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
			if err := r.Agent.ReleaseSandboxes(ctx, &run); err != nil {
				return ctrl.Result{}, err
			}
			if err := cleanupExecutionRBAC(ctx, r.Client, &run, r.Namespace); err != nil {
				return ctrl.Result{}, err
			}
			original := run.DeepCopy()
			controllerutil.RemoveFinalizer(&run, rbacCleanupFinalizer)
			if err := r.Patch(ctx, &run, client.MergeFrom(original)); err != nil {
				return ctrl.Result{}, fmt.Errorf("%s: %w", ErrRemoveFinalizer, err)
			}
			// Requeue so templog cleanup Patches a fresh object (avoids conflict
			// from two sequential Patches in one reconcile).
			return ctrl.Result{Requeue: true}, nil
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

	phase := agenticv1alpha1.DerivePhase(run.Status.Conditions)

	// --- Terminal phases (before suspension guard so audit cleanup always runs) ---
	switch phase {
	case agenticv1alpha1.AgenticRunPhaseNoActionRequired:
		if !needsRevision(&run) {
			if hasSandboxClaims(&run) {
				if err := r.Agent.ReleaseSandboxes(ctx, &run); err != nil {
					log.Error(err, "sandbox cleanup failed at terminal phase")
				}
			}
			if r.Audit != nil {
				r.Audit.EmitTerminalSpan(ctx, &run, string(phase), terminalReason(&run))
				r.Audit.Cleanup(&run)
			}
			return ctrl.Result{}, nil
		}

	case agenticv1alpha1.AgenticRunPhaseCompleted,
		agenticv1alpha1.AgenticRunPhaseDenied,
		agenticv1alpha1.AgenticRunPhaseEscalated,
		agenticv1alpha1.AgenticRunPhaseEmergencyStopped:
		if !needsRevision(&run) {
			if hasSandboxClaims(&run) {
				if err := r.Agent.ReleaseSandboxes(ctx, &run); err != nil {
					log.Error(err, "sandbox cleanup failed at terminal phase")
				}
			}
			if r.Audit != nil {
				r.Audit.EmitTerminalSpan(ctx, &run, string(phase), terminalReason(&run))
				r.Audit.Cleanup(&run)
			}
			return ctrl.Result{}, nil
		}

	case agenticv1alpha1.AgenticRunPhaseFailed:
		return r.handleFailed(ctx, &run)
	}

	// --- Suspension guard (only non-terminal runs reach here) ---
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

	case agenticv1alpha1.AgenticRunPhaseNoActionRequired,
		agenticv1alpha1.AgenticRunPhaseCompleted,
		agenticv1alpha1.AgenticRunPhaseDenied,
		agenticv1alpha1.AgenticRunPhaseEscalated,
		agenticv1alpha1.AgenticRunPhaseEmergencyStopped:
		if needsRevision(&run) {
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
	return ctrl.NewControllerManagedBy(mgr).
		For(&agenticv1alpha1.AgenticRun{}).
		Owns(&agenticv1alpha1.AgenticRunApproval{}).
		Owns(&agenticv1alpha1.AnalysisResult{}).
		Owns(&agenticv1alpha1.ExecutionResult{}).
		Owns(&agenticv1alpha1.VerificationResult{}).
		Owns(&agenticv1alpha1.EscalationResult{}).
		Watches(&agenticv1alpha1.ApprovalPolicy{}, handler.EnqueueRequestsFromMapFunc(
			func(ctx context.Context, obj client.Object) []ctrl.Request {
				var runs agenticv1alpha1.AgenticRunList
				if err := r.List(ctx, &runs); err != nil {
					return nil
				}
				var reqs []ctrl.Request
				for _, p := range runs.Items {
					phase := agenticv1alpha1.DerivePhase(p.Status.Conditions)
					if !isTerminal(phase) {
						reqs = append(reqs, ctrl.Request{
							NamespacedName: client.ObjectKeyFromObject(&p),
						})
					}
				}
				return reqs
			},
		)).
		Watches(&agenticv1alpha1.AgenticOLSConfig{}, handler.EnqueueRequestsFromMapFunc(
			func(ctx context.Context, obj client.Object) []ctrl.Request {
				var runs agenticv1alpha1.AgenticRunList
				if err := r.List(ctx, &runs); err != nil {
					return nil
				}
				var reqs []ctrl.Request
				for _, p := range runs.Items {
					phase := agenticv1alpha1.DerivePhase(p.Status.Conditions)
					if !isTerminal(phase) {
						reqs = append(reqs, ctrl.Request{
							NamespacedName: client.ObjectKeyFromObject(&p),
						})
					}
				}
				return reqs
			},
		)).
		Named("agenticrun").
		WithOptions(controller.Options{MaxConcurrentReconciles: maxConcurrent}).
		Complete(r)
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
		traceID := strings.ReplaceAll(string(run.UID), "-", "")
		if err := r.TempLog.DeleteLogs(ctx, traceID); err != nil {
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
			"traceID", strings.ReplaceAll(string(run.UID), "-", ""))
	}

	original := run.DeepCopy()
	controllerutil.RemoveFinalizer(run, templogCleanupFinalizer)
	if err := r.Patch(ctx, run, client.MergeFrom(original)); err != nil {
		return ctrl.Result{}, fmt.Errorf("%s: %w", ErrRemoveFinalizer, err)
	}
	return ctrl.Result{}, nil
}
