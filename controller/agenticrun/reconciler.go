package agenticrun

import (
	"context"
	"fmt"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	toolscache "k8s.io/client-go/tools/cache"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
	"github.com/openshift/lightspeed-agentic-operator/pkg/configuration"
)

const (
	ErrRemoveFinalizer             = "remove finalizer"
	ErrAddFinalizer                = "add finalizer"
	ErrPatchTemplogCleanupAttempts = "patch templog cleanup attempts"
	ErrPatchRBACCleanupAttempts    = "patch rbac cleanup attempts"
	ErrStampTerminalTTL            = "stamp terminal TTL"
	ErrConfigNotReady              = "operator configuration not yet available"
	ErrDeleteExpiredRun            = "delete expired run"
	ErrLabelTerminalRun            = "label terminal run"
	terminalTTLLabel               = "agentic.openshift.io/ttl-managed"
	suspensionRequeueAfter         = 5 * time.Second
)

// TempLogCleaner is the interface for deleting templog records on CR deletion.
type TempLogCleaner interface {
	DeleteLogs(ctx context.Context, traceID string) error
}

// AgenticRunReconciler reconciles AgenticRun objects.
//
// Agent, APIReader, and Sandbox must be set before calling SetupWithManager.
type AgenticRunReconciler struct {
	client.Client
	APIReader client.Reader
	Agent     AgentCaller
	Sandbox   *SandboxManager
	Config    *configuration.Cache
	Namespace string
	Audit     AuditLogger
	TempLog   TempLogCleaner
	Now       func() time.Time
}

func (r *AgenticRunReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// +kubebuilder:rbac:groups=apiextensions.k8s.io,resources=customresourcedefinitions,verbs=get
// +kubebuilder:rbac:groups=agentic.openshift.io,resources=agenticruns,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=agentic.openshift.io,resources=agenticruns/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=agentic.openshift.io,resources=agenticruns/finalizers,verbs=update
// +kubebuilder:rbac:groups=agentic.openshift.io,resources=agents,verbs=get;list;watch
// +kubebuilder:rbac:groups=agentic.openshift.io,resources=llmproviders,verbs=get;list;watch
// +kubebuilder:rbac:groups=agentic.openshift.io,resources=agenticrunapprovals,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=agentic.openshift.io,resources=agenticrunapprovals/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=agentic.openshift.io,resources=approvalpolicies,verbs=get;list;watch
// +kubebuilder:rbac:groups=agentic.openshift.io,resources=analysisresults,verbs=get;list;watch;create;patch;update
// +kubebuilder:rbac:groups=agentic.openshift.io,resources=executionresults,verbs=get;list;watch;create;patch;update
// +kubebuilder:rbac:groups=agentic.openshift.io,resources=verificationresults,verbs=get;list;watch;create;patch;update
// +kubebuilder:rbac:groups=agentic.openshift.io,resources=escalationresults,verbs=get;list;watch;create;patch;update
// +kubebuilder:rbac:groups=agentic.openshift.io,resources=analysisresults/status;executionresults/status;verificationresults/status;escalationresults/status,verbs=get;patch;update
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=roles;rolebindings,verbs=get;list;watch;create;delete;patch;update
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterroles;clusterrolebindings,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups=agentic.openshift.io,resources=agenticolsconfigs,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=create;delete;get;list;patch;update;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=create;delete;get;list;update;watch

func (r *AgenticRunReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	if r.APIReader == nil {
		return ctrl.Result{}, fmt.Errorf("uncached API reader is required by the run controller")
	}

	var run agenticv1alpha1.AgenticRun
	if err := r.APIReader.Get(ctx, req.NamespacedName, &run); err != nil {
		if client.IgnoreNotFound(err) == nil && r.Audit != nil {
			r.Audit.CleanupDeleted(req.NamespacedName)
		}
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !run.DeletionTimestamp.IsZero() {
		if r.Audit != nil {
			r.Audit.Cleanup(&run)
		}
		accepted := hasAcceptedStop(&run)
		suspended := false
		var start metav1.Time
		currentActivation := false
		if !accepted {
			var err error
			suspended, start, currentActivation, err = r.readSuspensionActivation(ctx)
			if err != nil {
				return ctrl.Result{}, err
			}
		}
		if accepted || suspended {
			if suspended && !currentActivation && !accepted {
				return ctrl.Result{RequeueAfter: suspensionRequeueAfter}, nil
			}
			result, err := r.handleSuspensionAt(ctx, &run, start)
			if err != nil {
				return result, err
			}
			if result.Requeue || result.RequeueAfter > 0 {
				return result, nil
			}
			if controllerutil.ContainsFinalizer(&run, rbacCleanupFinalizer) {
				base := run.DeepCopy()
				controllerutil.RemoveFinalizer(&run, rbacCleanupFinalizer)
				if err := r.Patch(ctx, &run, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); client.IgnoreNotFound(err) != nil {
					return ctrl.Result{}, fmt.Errorf("%s: %w", ErrRemoveFinalizer, err)
				}
			}
			if controllerutil.ContainsFinalizer(&run, templogCleanupFinalizer) {
				return r.handleTemplogCleanup(ctx, &run)
			}
			return ctrl.Result{}, nil
		}

		if controllerutil.ContainsFinalizer(&run, rbacCleanupFinalizer) {
			if err := r.Agent.ReleaseSandboxes(ctx, &run, false); err != nil {
				return ctrl.Result{}, fmt.Errorf("release sandboxes during deletion: %w", err)
			}
			original := run.DeepCopy()
			controllerutil.RemoveFinalizer(&run, rbacCleanupFinalizer)
			if err := r.Patch(ctx, &run, client.MergeFrom(original)); client.IgnoreNotFound(err) != nil {
				return ctrl.Result{}, fmt.Errorf("%s: %w", ErrRemoveFinalizer, err)
			}
		}
		if controllerutil.ContainsFinalizer(&run, templogCleanupFinalizer) {
			return r.handleTemplogCleanup(ctx, &run)
		}
		return ctrl.Result{}, nil
	}

	// --- Finalizers (first sight of any non-deleting run, including terminal) ---
	if !controllerutil.ContainsFinalizer(&run, rbacCleanupFinalizer) || !controllerutil.ContainsFinalizer(&run, templogCleanupFinalizer) {
		base := run.DeepCopy()
		controllerutil.AddFinalizer(&run, rbacCleanupFinalizer)
		controllerutil.AddFinalizer(&run, templogCleanupFinalizer)
		if err := r.Patch(ctx, &run, client.MergeFrom(base)); err != nil {
			return ctrl.Result{}, fmt.Errorf("%s: %w", ErrAddFinalizer, err)
		}
		if err := r.APIReader.Get(ctx, req.NamespacedName, &run); err != nil {
			return ctrl.Result{}, client.IgnoreNotFound(err)
		}
	}

	accepted := hasAcceptedStop(&run)
	suspended := false
	var start metav1.Time
	currentActivation := false
	if !accepted {
		var err error
		suspended, start, currentActivation, err = r.readSuspensionActivation(ctx)
		if err != nil {
			return ctrl.Result{}, err
		}
	}
	if accepted || suspended {
		if suspended && !currentActivation && !accepted {
			return ctrl.Result{RequeueAfter: suspensionRequeueAfter}, nil
		}
		result, err := r.handleSuspensionAt(ctx, &run, start)
		if err != nil {
			return result, err
		}
		phase := agenticv1alpha1.DerivePhase(run.Status.Conditions)
		if isTerminal(phase) {
			var cfg *configuration.Config
			if r.Config != nil {
				cfg = r.Config.Get()
			}
			if cfg == nil {
				if result.RequeueAfter == 0 && !result.Requeue {
					result = ctrl.Result{RequeueAfter: suspensionRequeueAfter}
				}
				return result, nil
			}
			terminalResult, err := r.handleSuspendedTerminalCleanup(ctx, &run, phase, cfg.TerminalTTLDays)
			if err != nil {
				return terminalResult, err
			}
			if result.RequeueAfter == 0 && !result.Requeue {
				result = terminalResult
			}
		}
		return result, nil
	}

	// --- Configuration guard (wait for lightspeed-agentic-configuration ConfigMap) ---
	var cfg *configuration.Config
	if r.Config != nil {
		cfg = r.Config.Get()
	}
	if cfg == nil {
		return ctrl.Result{}, fmt.Errorf("%s", ErrConfigNotReady)
	}

	phase := agenticv1alpha1.DerivePhase(run.Status.Conditions)
	skipTerminalCleanup := (run.Labels[terminalTTLLabel] == "true" && run.Status.DeleteAfter != nil) || (phase == agenticv1alpha1.AgenticRunPhaseFailed && preserveFailedSandbox(&run))

	// --- Terminal phases ---
	switch phase {
	case agenticv1alpha1.AgenticRunPhaseCompleted:
		revisable := isNoActionRequired(&run) || run.Spec.Execution.IsZero()
		if !(revisable && needsRevision(&run)) {
			if skipTerminalCleanup {
				return ctrl.Result{}, nil
			}
			return r.handleTerminalCleanup(ctx, &run, phase, cfg.TerminalTTLDays)
		}

	case agenticv1alpha1.AgenticRunPhaseDenied,
		agenticv1alpha1.AgenticRunPhaseEscalated,
		agenticv1alpha1.AgenticRunPhaseEmergencyStopped:
		if skipTerminalCleanup {
			return ctrl.Result{}, nil
		}
		return r.handleTerminalCleanup(ctx, &run, phase, cfg.TerminalTTLDays)

	case agenticv1alpha1.AgenticRunPhaseFailed:
		if !(run.Spec.Execution.IsZero() && needsRevision(&run)) {
			if skipTerminalCleanup {
				return ctrl.Result{}, nil
			}
			if result, err := r.handleFailed(ctx, &run); err != nil {
				return result, err
			}
			return r.handleTerminalCleanup(ctx, &run, phase, cfg.TerminalTTLDays)
		}
	}

	// --- Ensure AgenticRunApproval exists ---
	policy, err := getApprovalPolicy(ctx, r.Client)
	if err != nil {
		log.Error(err, "failed to get ApprovalPolicy")
	}

	approval, err := ensureAgenticRunApproval(ctx, r.Client, &run, policy, r.Namespace)
	if err != nil {
		log.Error(err, "failed to ensure AgenticRunApproval")
		return ctrl.Result{Requeue: true}, nil
	}

	// --- Resolve agents/LLMs ---
	resolved, err := resolveAgenticRun(ctx, r.Client, &run, approval, r.Namespace)
	if err != nil {
		log.Error(err, "workflow resolution failed")
		current, guardErr := readRunnableRun(ctx, r.APIReader, client.ObjectKeyFromObject(&run), run.UID)
		if guardErr != nil {
			if guardErr == errStopAccepted {
				return ctrl.Result{Requeue: true}, nil
			}
			return ctrl.Result{}, guardErr
		}
		run = *current
		phase = agenticv1alpha1.DerivePhase(run.Status.Conditions)
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

	case agenticv1alpha1.AgenticRunPhaseCompleted,
		agenticv1alpha1.AgenticRunPhaseFailed:
		revisable := isNoActionRequired(&run) || run.Spec.Execution.IsZero()
		if revisable && needsRevision(&run) {
			return r.handleRevision(ctx, &run, resolved)
		}
		return ctrl.Result{}, nil

	default:
		log.V(1).Info("unhandled phase, no-op", LogKeyPhase, phase)
		return ctrl.Result{}, nil
	}
}

func (r *AgenticRunReconciler) readSuspensionActivation(ctx context.Context) (bool, metav1.Time, bool, error) {
	if r.APIReader == nil {
		return false, metav1.Time{}, false, fmt.Errorf("uncached API reader is required by the run controller")
	}
	var config agenticv1alpha1.AgenticOLSConfig
	err := r.APIReader.Get(ctx, client.ObjectKey{Name: "cluster"}, &config)
	if err != nil {
		if client.IgnoreNotFound(err) == nil {
			return false, metav1.Time{}, false, nil
		}
		return false, metav1.Time{}, false, fmt.Errorf("read AgenticOLSConfig through uncached API reader: %w", err)
	}
	if !config.Spec.Suspended {
		return false, metav1.Time{}, false, nil
	}
	condition := meta.FindStatusCondition(config.Status.Conditions, agenticv1alpha1.AgenticOLSConfigConditionSuspended)
	current := condition != nil &&
		condition.Status == metav1.ConditionTrue &&
		condition.ObservedGeneration == config.Generation &&
		!condition.LastTransitionTime.IsZero()
	if !current {
		return true, metav1.Time{}, false, nil
	}
	return true, condition.LastTransitionTime, true, nil
}

// SetupWithManager sets up the controller with the Manager.
func readerBindingEventHandlers(namespace string) toolscache.ResourceEventHandlerFuncs {
	return toolscache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			if readerBindingReferencesServiceAccount(obj, namespace) {
				invalidateReaderBindings()
			}
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			if readerBindingReferencesServiceAccount(oldObj, namespace) || readerBindingReferencesServiceAccount(newObj, namespace) {
				invalidateReaderBindings()
			}
		},
		DeleteFunc: func(obj interface{}) {
			if readerBindingReferencesServiceAccount(obj, namespace) {
				invalidateReaderBindings()
			}
		},
	}
}

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
		var reqs []ctrl.Request
		for _, p := range runs.Items {
			phase := agenticv1alpha1.DerivePhase(p.Status.Conditions)
			// Runs without a terminal timestamp still need a first deadline.
			if !isTerminal(phase) || p.Status.TerminalTime == nil {
				reqs = append(reqs, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(&p)})
			}
		}
		return reqs
	}

	if err := mgr.Add(manager.RunnableFunc(r.runTimeoutLoop)); err != nil {
		return err
	}

	readerBindingInformer, err := mgr.GetCache().GetInformer(context.Background(), &rbacv1.ClusterRoleBinding{})
	if err != nil {
		return fmt.Errorf("get ClusterRoleBinding informer: %w", err)
	}
	if _, err := readerBindingInformer.AddEventHandler(readerBindingEventHandlers(r.Namespace)); err != nil {
		return fmt.Errorf("watch ClusterRoleBindings: %w", err)
	}

	builder := ctrl.NewControllerManagedBy(mgr).
		For(&agenticv1alpha1.AgenticRun{}).
		Owns(&agenticv1alpha1.AgenticRunApproval{}).
		Watches(&corev1.Pod{}, handler.EnqueueRequestsFromMapFunc(r.handlePodEvent),
			builder.WithPredicates(predicate.NewPredicateFuncs(func(obj client.Object) bool {
				return obj.GetNamespace() == r.Namespace
			}))).
		Watches(&agenticv1alpha1.ApprovalPolicy{}, handler.EnqueueRequestsFromMapFunc(fanOutToActiveRuns)).
		Watches(&agenticv1alpha1.AgenticOLSConfig{}, handler.EnqueueRequestsFromMapFunc(fanOutToActiveRuns)).
		Watches(&corev1.ConfigMap{}, handler.EnqueueRequestsFromMapFunc(
			func(ctx context.Context, obj client.Object) []ctrl.Request {
				if obj.GetNamespace() != r.Namespace || obj.GetName() != configuration.ConfigMapName {
					return nil
				}
				return fanOutToActiveRuns(ctx, obj)
			},
		))

	return builder.
		Named("agenticrun").
		WithOptions(controller.Options{MaxConcurrentReconciles: maxConcurrent}).
		Complete(r)
}

// handleTerminalCleanup releases sandboxes, emits audit spans, and records the
// fixed terminal deadline and label used by the hourly expiry sweep.
func (r *AgenticRunReconciler) handleTerminalCleanup(ctx context.Context, run *agenticv1alpha1.AgenticRun, phase agenticv1alpha1.AgenticRunPhase, ceilingDays int32) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	if hasSandboxClaims(run) {
		if err := r.Agent.ReleaseSandboxes(ctx, run, false); err != nil {
			log.Error(err, "sandbox cleanup failed at terminal phase")
		}
	}
	return r.handleTerminalRecord(ctx, run, phase, ceilingDays)
}

func (r *AgenticRunReconciler) handleSuspendedTerminalCleanup(ctx context.Context, run *agenticv1alpha1.AgenticRun, phase agenticv1alpha1.AgenticRunPhase, ceilingDays int32) (ctrl.Result, error) {
	if run.Labels[terminalTTLLabel] == "true" && run.Status.DeleteAfter != nil {
		return ctrl.Result{}, nil
	}
	return r.handleTerminalRecord(ctx, run, phase, ceilingDays)
}

func (r *AgenticRunReconciler) handleTerminalRecord(ctx context.Context, run *agenticv1alpha1.AgenticRun, phase agenticv1alpha1.AgenticRunPhase, ceilingDays int32) (ctrl.Result, error) {
	if r.APIReader == nil {
		return ctrl.Result{}, fmt.Errorf("uncached API reader is required by terminal run handling")
	}
	current := &agenticv1alpha1.AgenticRun{}
	if err := r.APIReader.Get(ctx, client.ObjectKeyFromObject(run), current); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if current.UID != run.UID {
		return ctrl.Result{}, nil
	}
	*run = *current

	if r.Audit != nil {
		r.Audit.EmitTerminalSpan(ctx, run, string(phase), terminalReason(run))
		r.Audit.Cleanup(run)
	}

	if run.Status.TerminalTime == nil {
		days := ceilingDays
		if run.Spec.TerminalTTL != nil && *run.Spec.TerminalTTL < days {
			days = *run.Spec.TerminalTTL
		}
		now := metav1.Now()
		deadline := metav1.NewTime(now.Time.UTC().AddDate(0, 0, int(days)))
		base := run.DeepCopy()
		run.Status.TerminalTime = &now
		run.Status.DeleteAfter = &deadline
		if err := r.statusPatch(ctx, run, base); err != nil {
			return ctrl.Result{}, fmt.Errorf("%s: %w", ErrStampTerminalTTL, err)
		}
		if err := r.APIReader.Get(ctx, client.ObjectKeyFromObject(run), run); err != nil {
			return ctrl.Result{}, client.IgnoreNotFound(err)
		}
		if run.UID != current.UID {
			return ctrl.Result{}, nil
		}
	}

	if run.Labels[terminalTTLLabel] != "true" {
		base := run.DeepCopy()
		if run.Labels == nil {
			run.Labels = make(map[string]string)
		}
		run.Labels[terminalTTLLabel] = "true"
		if err := r.Patch(ctx, run, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
			return ctrl.Result{}, fmt.Errorf("%s: %w", ErrLabelTerminalRun, err)
		}
	}
	return ctrl.Result{}, nil
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
			if patchErr := r.Patch(ctx, run, client.MergeFrom(original)); client.IgnoreNotFound(patchErr) != nil {
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
	if err := r.Patch(ctx, run, client.MergeFrom(original)); client.IgnoreNotFound(err) != nil {
		return ctrl.Result{}, fmt.Errorf("%s: %w", ErrRemoveFinalizer, err)
	}
	return ctrl.Result{}, nil
}
