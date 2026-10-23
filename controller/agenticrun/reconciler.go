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
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	toolscache "k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
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
	ErrRemoveFinalizer              = "remove finalizer"
	ErrAddFinalizer                 = "add finalizer"
	ErrPatchTemplogCleanupAttempts  = "patch templog cleanup attempts"
	ErrPatchRBACCleanupAttempts     = "patch rbac cleanup attempts"
	ErrStampTerminalTTL             = "stamp terminal TTL"
	ErrConfigNotReady               = "operator configuration not yet available"
	ErrDeleteExpiredRun             = "delete expired run"
	ErrLabelTerminalRun             = "label terminal run"
	terminalTTLLabel                = "agentic.openshift.io/ttl-managed"
	eventReasonRBACFinalizerRelease = "RBACFinalizerReleasedWithUnverifiedSpoke"
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
	// APIReader is an authoritative, uncached reader. When nil it falls back
	// to Client.
	APIReader     client.Reader
	Agent         AgentCaller
	Config        *configuration.Cache
	Namespace     string
	Audit         AuditLogger
	TempLog       TempLogCleaner
	EventRecorder record.EventRecorder
}

func (r *AgenticRunReconciler) apiReader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
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
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterroles,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterrolebindings,verbs=get;list;watch;create;delete;patch
// +kubebuilder:rbac:groups=agentic.openshift.io,resources=agenticolsconfigs,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=create;delete;get;list;patch;update;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

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
				if !IsSpokeStopDegradeable(err) || run.DeletionTimestamp == nil || time.Since(run.DeletionTimestamp.Time) < SpokeRetryWindow {
					return ctrl.Result{}, fmt.Errorf("release sandboxes during deletion: %w", err)
				}
				if r.EventRecorder != nil {
					r.EventRecorder.Eventf(&run, corev1.EventTypeWarning, eventReasonRBACFinalizerRelease,
						"Released RBAC cleanup finalizer after %s; spoke cleanup is unverified: %v", SpokeRetryWindow, err)
				}
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

	if result, handled, err := r.stopGuard(ctx, &run); handled || err != nil {
		return result, err
	}

	// --- Finalizers (first sight of any non-stopping run) ---
	if finalized, err := r.ensureRunFinalizers(ctx, &run); err != nil {
		return ctrl.Result{}, err
	} else if !finalized {
		return ctrl.Result{}, nil
	}
	if err := r.Get(ctx, req.NamespacedName, &run); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if result, handled, err := r.stopGuard(ctx, &run); handled || err != nil {
		return result, err
	}
	phase := agenticv1alpha1.DerivePhase(run.Status.Conditions)

	// --- Configuration guard (wait for lightspeed-agentic-configuration ConfigMap) ---
	var cfg *configuration.Config
	if r.Config != nil {
		cfg = r.Config.Get()
	}
	if cfg == nil {
		return ctrl.Result{}, fmt.Errorf("%s", ErrConfigNotReady)
	}

	skipTerminalCleanup := (run.Labels[terminalTTLLabel] == "true" && run.Status.DeleteAfter != nil) || (phase == agenticv1alpha1.AgenticRunPhaseFailed && preserveFailedSandbox(&run))

	// --- Terminal phases ---
	switch phase {
	case agenticv1alpha1.AgenticRunPhaseCompleted:
		if !needsTerminalRevision(&run, phase) {
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
		if !needsTerminalRevision(&run, phase) {
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
		base := run.DeepCopy()
		meta.SetStatusCondition(&run.Status.Conditions, metav1.Condition{
			Type:               agenticv1alpha1.AgenticRunConditionAnalyzed,
			Status:             metav1.ConditionFalse,
			Reason:             reasonWorkflowFailed,
			Message:            err.Error(),
			ObservedGeneration: run.Generation,
		})
		if statusErr := r.statusPatch(ctx, &run, base); statusErr != nil {
			return ctrl.Result{}, fmt.Errorf("patch status after workflow resolution failure: %w", statusErr)
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

func (r *AgenticRunReconciler) stopGuard(ctx context.Context, run *agenticv1alpha1.AgenticRun) (ctrl.Result, bool, error) {
	phase := agenticv1alpha1.DerivePhase(run.Status.Conditions)
	suspended, err := isSuspended(ctx, r.Client)
	if err != nil {
		return ctrl.Result{}, false, err
	}
	if isStopping(run) {
		result, err := r.handleStopping(ctx, run, reasonSystemSuspended)
		return result, true, err
	}
	if suspended && !isTerminal(phase) {
		result, err := r.handleStopping(ctx, run, reasonSystemSuspended)
		return result, true, err
	}
	if isTerminal(phase) && (isStoppedOutcome(run) || isStopCleanupComplete(run) || (suspended && needsTerminalRevision(run, phase))) {
		result, err := r.handleStoppedTerminalCleanup(ctx, run, phase)
		return result, true, err
	}
	return ctrl.Result{}, false, nil
}
func (r *AgenticRunReconciler) ensureRunFinalizers(ctx context.Context, run *agenticv1alpha1.AgenticRun) (bool, error) {
	if controllerutil.ContainsFinalizer(run, rbacCleanupFinalizer) && controllerutil.ContainsFinalizer(run, templogCleanupFinalizer) {
		return true, nil
	}
	base := run.DeepCopy()
	controllerutil.AddFinalizer(run, rbacCleanupFinalizer)
	controllerutil.AddFinalizer(run, templogCleanupFinalizer)
	if err := r.Patch(ctx, run, client.MergeFrom(base)); err != nil {
		if client.IgnoreNotFound(err) == nil {
			return false, nil
		}
		return false, fmt.Errorf("%s: %w", ErrAddFinalizer, err)
	}
	if err := r.Get(ctx, client.ObjectKeyFromObject(run), run); err != nil {
		if client.IgnoreNotFound(err) == nil {
			return false, nil
		}
		return false, fmt.Errorf("refresh AgenticRun after finalizer patch: %w", err)
	}
	return true, nil
}

func (r *AgenticRunReconciler) handleStoppedTerminalCleanup(ctx context.Context, run *agenticv1alpha1.AgenticRun, phase agenticv1alpha1.AgenticRunPhase) (ctrl.Result, error) {
	finalized, err := r.ensureRunFinalizers(ctx, run)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !finalized {
		return ctrl.Result{}, nil
	}
	if err := r.Get(ctx, client.ObjectKeyFromObject(run), run); err != nil {
		if client.IgnoreNotFound(err) == nil {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	if !run.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}
	phase = agenticv1alpha1.DerivePhase(run.Status.Conditions)
	if !isTerminal(phase) {
		return ctrl.Result{Requeue: true}, nil
	}

	confirmed, err := r.Agent.StopSandboxes(ctx, run)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("stop sandboxes for terminal run: %w", err)
	}
	if !confirmed {
		return ctrl.Result{RequeueAfter: stopSandboxesRequeueAfter}, nil
	}

	base := run.DeepCopy()
	changed := clearSandboxReferences(run)
	if isStopping(run) {
		meta.SetStatusCondition(&run.Status.Conditions, metav1.Condition{
			Type:               agenticv1alpha1.AgenticRunConditionStopping,
			Status:             metav1.ConditionFalse,
			Reason:             reasonTeardownComplete,
			Message:            messageTeardownComplete,
			ObservedGeneration: run.Generation,
		})
		changed = true
	}
	if changed {
		if err := r.statusPatch(ctx, run, base); err != nil {
			return ctrl.Result{}, fmt.Errorf("complete terminal run teardown: %w", err)
		}
		if err := r.Get(ctx, client.ObjectKeyFromObject(run), run); err != nil {
			if client.IgnoreNotFound(err) == nil {
				return ctrl.Result{}, nil
			}
			return ctrl.Result{}, err
		}
		phase = agenticv1alpha1.DerivePhase(run.Status.Conditions)
	}
	if !isTerminal(phase) {
		return ctrl.Result{Requeue: true}, nil
	}

	if phase == agenticv1alpha1.AgenticRunPhaseFailed && preserveFailedSandbox(run) {
		return ctrl.Result{}, nil
	}
	if run.Status.TerminalTime != nil && run.Status.DeleteAfter != nil && run.Labels[terminalTTLLabel] == "true" {
		return ctrl.Result{}, nil
	}

	var ceilingDays int32
	if run.Status.TerminalTime == nil && !(phase == agenticv1alpha1.AgenticRunPhaseFailed && preserveFailedSandbox(run)) {
		var cfg *configuration.Config
		if r.Config != nil {
			cfg = r.Config.Get()
		}
		if cfg == nil {
			return ctrl.Result{}, fmt.Errorf("%s", ErrConfigNotReady)
		}
		ceilingDays = cfg.TerminalTTLDays
	}
	return r.recordTerminalCleanup(ctx, run, phase, ceilingDays)
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

	controllerBuilder := ctrl.NewControllerManagedBy(mgr).
		For(&agenticv1alpha1.AgenticRun{}).
		Owns(&agenticv1alpha1.AgenticRunApproval{}).
		Owns(&agenticv1alpha1.AnalysisResult{}).
		Owns(&agenticv1alpha1.ExecutionResult{}).
		Owns(&agenticv1alpha1.VerificationResult{}).
		Owns(&agenticv1alpha1.EscalationResult{}).
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

	if _, err := mgr.GetRESTMapper().RESTMapping(smClaimGVK.GroupKind(), smClaimGVK.Version); err == nil {
		claim := &unstructured.Unstructured{}
		claim.SetGroupVersionKind(smClaimGVK)
		controllerBuilder = controllerBuilder.Watches(claim, handler.EnqueueRequestsFromMapFunc(r.handleManagedSandboxEvent),
			builder.WithPredicates(predicate.NewPredicateFuncs(func(obj client.Object) bool {
				return obj.GetNamespace() == r.Namespace
			})))
	} else if !meta.IsNoMatchError(err) {
		return fmt.Errorf("resolve SandboxClaim watch: %w", err)
	}

	if _, err := mgr.GetRESTMapper().RESTMapping(smSandboxGVK.GroupKind(), smSandboxGVK.Version); err == nil {
		sandbox := &unstructured.Unstructured{}
		sandbox.SetGroupVersionKind(smSandboxGVK)
		controllerBuilder = controllerBuilder.Watches(sandbox, handler.EnqueueRequestsFromMapFunc(r.handleManagedSandboxEvent),
			builder.WithPredicates(predicate.NewPredicateFuncs(func(obj client.Object) bool {
				return obj.GetNamespace() == r.Namespace
			})))
	} else if !meta.IsNoMatchError(err) {
		return fmt.Errorf("resolve Sandbox watch: %w", err)
	}
	return controllerBuilder.
		Named("agenticrun").
		WithOptions(controller.Options{MaxConcurrentReconciles: maxConcurrent}).
		Complete(r)
}

// handleTerminalCleanup releases sandboxes, emits audit spans, and records the
// fixed terminal deadline and label used by the hourly expiry sweep.
func (r *AgenticRunReconciler) handleTerminalCleanup(ctx context.Context, run *agenticv1alpha1.AgenticRun, phase agenticv1alpha1.AgenticRunPhase, ceilingDays int32) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	if hasSandboxClaims(run) {
		if err := r.Agent.ReleaseSandboxes(ctx, run); err != nil {
			log.Error(err, "sandbox cleanup failed at terminal phase")
		}
	}
	return r.recordTerminalCleanup(ctx, run, phase, ceilingDays)
}

func (r *AgenticRunReconciler) recordTerminalCleanup(ctx context.Context, run *agenticv1alpha1.AgenticRun, phase agenticv1alpha1.AgenticRunPhase, ceilingDays int32) (ctrl.Result, error) {
	if r.Audit != nil {
		r.Audit.EmitTerminalSpan(ctx, run, string(phase), terminalReason(run))
		r.Audit.Cleanup(run)
	}
	if phase == agenticv1alpha1.AgenticRunPhaseFailed && preserveFailedSandbox(run) {
		return ctrl.Result{}, nil
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
	}

	if run.Labels[terminalTTLLabel] == "true" {
		return ctrl.Result{}, nil
	}
	base := run.DeepCopy()
	if run.Labels == nil {
		run.Labels = make(map[string]string)
	}
	run.Labels[terminalTTLLabel] = "true"
	if err := r.Patch(ctx, run, client.MergeFrom(base)); err != nil {
		return ctrl.Result{}, fmt.Errorf("%s: %w", ErrLabelTerminalRun, err)
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
