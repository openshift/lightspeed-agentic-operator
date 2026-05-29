package proposal

import (
	"context"
	"fmt"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
)

// ProposalReconciler reconciles Proposal objects.
//
// Agent must be set before calling SetupWithManager.
type ProposalReconciler struct {
	client.Client
	Log       logr.Logger
	Agent     AgentCaller
	Namespace string
}

// +kubebuilder:rbac:groups=agentic.openshift.io,resources=proposals,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=agentic.openshift.io,resources=proposals/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=agentic.openshift.io,resources=proposals/finalizers,verbs=update
// +kubebuilder:rbac:groups=agentic.openshift.io,resources=agents,verbs=get;list;watch
// +kubebuilder:rbac:groups=agentic.openshift.io,resources=llmproviders,verbs=get;list;watch
// +kubebuilder:rbac:groups=agentic.openshift.io,resources=proposalapprovals,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=agentic.openshift.io,resources=proposalapprovals/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=agentic.openshift.io,resources=approvalpolicies,verbs=get;list;watch
// +kubebuilder:rbac:groups=agentic.openshift.io,resources=analysisresults,verbs=get;list;watch;create
// +kubebuilder:rbac:groups=agentic.openshift.io,resources=executionresults,verbs=get;list;watch;create
// +kubebuilder:rbac:groups=agentic.openshift.io,resources=verificationresults,verbs=get;list;watch;create
// +kubebuilder:rbac:groups=agentic.openshift.io,resources=escalationresults,verbs=get;list;watch;create
// +kubebuilder:rbac:groups=agentic.openshift.io,resources=analysisresults/status;executionresults/status;verificationresults/status;escalationresults/status,verbs=get;patch;update
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=roles;rolebindings,verbs=get;create;delete
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterroles;clusterrolebindings,verbs=get;create;delete
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch

func (r *ProposalReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Log.WithValues("proposal", req.NamespacedName)

	var proposal agenticv1alpha1.Proposal
	if err := r.Get(ctx, req.NamespacedName, &proposal); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// --- Deletion ---
	if !proposal.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(&proposal, rbacCleanupFinalizer) {
			if err := r.Agent.ReleaseSandboxes(ctx, &proposal); err != nil {
				log.Error(err, "sandbox cleanup failed during deletion")
			}
			if err := cleanupExecutionRBAC(ctx, r.Client, &proposal); err != nil {
				log.Error(err, "RBAC cleanup failed, retrying")
				return ctrl.Result{}, err
			}
			original := proposal.DeepCopy()
			controllerutil.RemoveFinalizer(&proposal, rbacCleanupFinalizer)
			if err := r.Patch(ctx, &proposal, client.MergeFrom(original)); err != nil {
				return ctrl.Result{}, fmt.Errorf("remove finalizer: %w", err)
			}
		}
		return ctrl.Result{}, nil
	}

	phase := agenticv1alpha1.DerivePhase(proposal.Status.Conditions)

	// --- Finalizer ---
	if !controllerutil.ContainsFinalizer(&proposal, rbacCleanupFinalizer) {
		if !isTerminal(phase) {
			original := proposal.DeepCopy()
			controllerutil.AddFinalizer(&proposal, rbacCleanupFinalizer)
			if err := r.Patch(ctx, &proposal, client.MergeFrom(original)); err != nil {
				return ctrl.Result{}, fmt.Errorf("add finalizer: %w", err)
			}
			if err := r.Get(ctx, req.NamespacedName, &proposal); err != nil {
				return ctrl.Result{}, client.IgnoreNotFound(err)
			}
		}
	}

	// --- Terminal phases ---
	switch phase {
	case agenticv1alpha1.ProposalPhaseCompleted,
		agenticv1alpha1.ProposalPhaseDenied,
		agenticv1alpha1.ProposalPhaseEscalated:
		if hasSandboxClaims(&proposal) {
			if err := r.Agent.ReleaseSandboxes(ctx, &proposal); err != nil {
				log.Error(err, "sandbox cleanup failed at terminal phase")
			}
		}
		return ctrl.Result{}, nil

	case agenticv1alpha1.ProposalPhaseFailed:
		return r.handleFailed(ctx, log, &proposal)
	}

	// --- Ensure ProposalApproval exists ---
	policy, err := getApprovalPolicy(ctx, r.Client)
	if err != nil {
		log.Error(err, "failed to get ApprovalPolicy")
	}

	approval, err := ensureProposalApproval(ctx, r.Client, &proposal, policy)
	if err != nil {
		log.Error(err, "failed to ensure ProposalApproval")
		return ctrl.Result{Requeue: true}, nil
	}

	// --- Resolve agents/LLMs ---
	resolved, err := resolveProposal(ctx, r.Client, &proposal, approval)
	if err != nil {
		log.Error(err, "workflow resolution failed")
		base := proposal.DeepCopy()
		meta.SetStatusCondition(&proposal.Status.Conditions, metav1.Condition{
			Type:               agenticv1alpha1.ProposalConditionAnalyzed,
			Status:             metav1.ConditionFalse,
			Reason:             reasonWorkflowFailed,
			Message:            err.Error(),
			ObservedGeneration: proposal.Generation,
		})
		if statusErr := r.statusPatch(ctx, &proposal, base); statusErr != nil {
			log.Error(statusErr, "failed to patch status after workflow resolution failure")
		}
		return ctrl.Result{}, nil
	}

	log.Info("reconciling", "phase", phase)

	// --- Phase routing ---
	switch phase {
	case agenticv1alpha1.ProposalPhasePending, agenticv1alpha1.ProposalPhaseAnalyzing:
		if needsRevision(&proposal) {
			return r.handleRevision(ctx, log, &proposal, resolved)
		}
		return r.handleAnalysis(ctx, log, &proposal, resolved, approval, policy)

	case agenticv1alpha1.ProposalPhaseProposed, agenticv1alpha1.ProposalPhaseExecuting:
		if needsRevision(&proposal) {
			return r.handleRevision(ctx, log, &proposal, resolved)
		}
		return r.handleExecution(ctx, log, &proposal, resolved, approval, policy)

	case agenticv1alpha1.ProposalPhaseVerifying:
		return r.handleVerification(ctx, log, &proposal, resolved, approval, policy)

	case agenticv1alpha1.ProposalPhaseEscalating:
		if needsRevision(&proposal) {
			return r.handleRevision(ctx, log, &proposal, resolved)
		}
		return r.handleEscalation(ctx, log, &proposal, resolved, approval, policy)

	default:
		log.Info("unhandled phase, no-op", "phase", phase)
		return ctrl.Result{}, nil
	}
}

// SetupWithManager sets up the controller with the Manager.
func (r *ProposalReconciler) SetupWithManager(mgr ctrl.Manager) error {
	maxConcurrent := int(agenticv1alpha1.DefaultMaxConcurrentProposals)
	var ap agenticv1alpha1.ApprovalPolicy
	if err := mgr.GetAPIReader().Get(context.Background(), client.ObjectKey{Name: "cluster"}, &ap); err == nil {
		if ap.Spec.MaxConcurrentProposals > 0 {
			maxConcurrent = int(ap.Spec.MaxConcurrentProposals)
		}
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&agenticv1alpha1.Proposal{}).
		Owns(&agenticv1alpha1.ProposalApproval{}).
		Owns(&agenticv1alpha1.AnalysisResult{}).
		Owns(&agenticv1alpha1.ExecutionResult{}).
		Owns(&agenticv1alpha1.VerificationResult{}).
		Owns(&agenticv1alpha1.EscalationResult{}).
		Watches(&agenticv1alpha1.ApprovalPolicy{}, handler.EnqueueRequestsFromMapFunc(
			func(ctx context.Context, obj client.Object) []ctrl.Request {
				var proposals agenticv1alpha1.ProposalList
				if err := r.List(ctx, &proposals); err != nil {
					return nil
				}
				var reqs []ctrl.Request
				for _, p := range proposals.Items {
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
		Named("proposal").
		WithOptions(controller.Options{MaxConcurrentReconciles: maxConcurrent}).
		Complete(r)
}
