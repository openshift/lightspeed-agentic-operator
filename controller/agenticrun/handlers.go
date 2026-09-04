package agenticrun

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
)

const (
	ErrUpdateToAnalyzing         = "update to Analyzing"
	ErrUpdateToAnalyzingRevision = "update to Analyzing (revision)"
	ErrUpdateToCompletedAdvisory = "update to Completed (advisory)"
	ErrUpdateAfterExecSkip       = "update after execution skip"
	ErrUpdateToExecuting         = "update to Executing"
	ErrUpdateToVerifying         = "update to Verifying"
	ErrResolveSelectedOption     = "resolve selected option"
	ErrGetOverrideAgent          = "get override Agent"
	ErrGetEscalationLLMProvider  = "get LLMProvider"
	ErrUpdateToEscalating        = "update to Escalating"
	ErrUpdateToDenied            = "update to Denied"
)

// handleAnalysis checks approval for the analysis step and runs it.
func (r *AgenticRunReconciler) handleAnalysis(
	ctx context.Context,
	run *agenticv1alpha1.AgenticRun,
	resolved *resolvedWorkflow,
	approval *agenticv1alpha1.AgenticRunApproval,
	policy *agenticv1alpha1.ApprovalPolicy,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	log.V(1).Info("handling analysis")

	if isStageDenied(approval, agenticv1alpha1.SandboxStepAnalysis) {
		if r.Audit != nil {
			r.Audit.EmitApprovalSpan(ctx, run, approval, "")
		}
		return r.denyAgenticRun(ctx, run, "Analysis denied by user")
	}

	if !isStageApproved(approval, policy, agenticv1alpha1.SandboxStepAnalysis) {
		log.V(1).Info("analysis pending approval")
		return ctrl.Result{}, nil
	}

	analyzed := meta.FindStatusCondition(run.Status.Conditions, agenticv1alpha1.AgenticRunConditionAnalyzed)
	if analyzed != nil {
		if analyzed.Status == metav1.ConditionUnknown {
			log.V(1).Info("analysis already in progress, waiting")
			return ctrl.Result{}, nil
		}
		if analyzed.Status == metav1.ConditionTrue {
			log.V(1).Info("analysis already completed")
			return ctrl.Result{}, nil
		}
	}

	base := run.DeepCopy()
	meta.SetStatusCondition(&run.Status.Conditions, metav1.Condition{
		Type:               agenticv1alpha1.AgenticRunConditionAnalyzed,
		Status:             metav1.ConditionUnknown,
		Reason:             reasonInProgress,
		Message:            "Analysis agent is running",
		ObservedGeneration: run.Generation,
	})
	if err := r.statusPatch(ctx, run, base); err != nil {
		return ctrl.Result{}, fmt.Errorf("%s: %w", ErrUpdateToAnalyzing, err)
	}

	if err := r.Agent.Analyze(ctx, run, resolved.Analysis); err != nil {
		return r.failStep(ctx, run, agenticv1alpha1.AgenticRunConditionAnalyzed, err)
	}

	log.Info("analysis sandbox launched")
	return ctrl.Result{}, nil
}

// handleRevision re-runs analysis with revision context appended to the
// agent's system prompt.
func (r *AgenticRunReconciler) handleRevision(
	ctx context.Context,
	run *agenticv1alpha1.AgenticRun,
	resolved *resolvedWorkflow,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	generation := run.Generation
	log.V(1).Info("handling revision", "generation", generation)

	analyzed := meta.FindStatusCondition(run.Status.Conditions, agenticv1alpha1.AgenticRunConditionAnalyzed)
	if analyzed != nil && analyzed.Status == metav1.ConditionUnknown {
		log.V(1).Info("revision already in progress, waiting")
		return ctrl.Result{}, nil
	}

	base := run.DeepCopy()
	meta.RemoveStatusCondition(&run.Status.Conditions, agenticv1alpha1.AgenticRunConditionExecuted)
	meta.RemoveStatusCondition(&run.Status.Conditions, agenticv1alpha1.AgenticRunConditionVerified)
	meta.RemoveStatusCondition(&run.Status.Conditions, agenticv1alpha1.AgenticRunConditionEscalated)
	resetExecutionAndVerification(&run.Status.Steps)
	// The run is leaving its terminal phase to re-analyze; clear terminalTime
	// so that if it reaches a terminal phase again, handleTerminalTTL stamps
	// a fresh timestamp instead of computing expiry off the prior terminal
	// event (see run-lifecycle.md rule 23/24).
	run.Status.TerminalTime = nil
	meta.SetStatusCondition(&run.Status.Conditions, metav1.Condition{
		Type:               agenticv1alpha1.AgenticRunConditionAnalyzed,
		Status:             metav1.ConditionUnknown,
		Reason:             reasonRevising,
		Message:            fmt.Sprintf("Re-analyzing for generation %d", generation),
		ObservedGeneration: run.Generation,
	})
	if err := r.statusPatch(ctx, run, base); err != nil {
		return ctrl.Result{}, fmt.Errorf("%s: %w", ErrUpdateToAnalyzingRevision, err)
	}

	if err := r.Agent.Analyze(ctx, run, resolved.Analysis); err != nil {
		return r.failStep(ctx, run, agenticv1alpha1.AgenticRunConditionAnalyzed, err)
	}

	log.Info("revision sandbox launched", "generation", generation)
	return ctrl.Result{}, nil
}

// handleExecution checks approval and runs execution (or skips if not configured).
func (r *AgenticRunReconciler) handleExecution(
	ctx context.Context,
	run *agenticv1alpha1.AgenticRun,
	resolved *resolvedWorkflow,
	approval *agenticv1alpha1.AgenticRunApproval,
	policy *agenticv1alpha1.ApprovalPolicy,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	log.V(1).Info("handling execution")

	if resolved.Execution == nil {
		base := run.DeepCopy()
		meta.SetStatusCondition(&run.Status.Conditions, metav1.Condition{
			Type:               agenticv1alpha1.AgenticRunConditionExecuted,
			Status:             metav1.ConditionTrue,
			Reason:             reasonSkipped,
			Message:            "Execution step not configured",
			ObservedGeneration: run.Generation,
		})

		if resolved.Verification == nil {
			setVerificationSkipped(run)
			if err := r.statusPatch(ctx, run, base); err != nil {
				return ctrl.Result{}, fmt.Errorf("%s: %w", ErrUpdateToCompletedAdvisory, err)
			}
			log.Info("advisory-only — completed")
			return ctrl.Result{}, nil
		}

		if err := r.statusPatch(ctx, run, base); err != nil {
			return ctrl.Result{}, fmt.Errorf("%s: %w", ErrUpdateAfterExecSkip, err)
		}
		return ctrl.Result{}, nil
	}

	if isStageDenied(approval, agenticv1alpha1.SandboxStepExecution) {
		if r.Audit != nil {
			r.Audit.EmitApprovalSpan(ctx, run, approval, "")
		}
		return r.denyAgenticRun(ctx, run, "Execution denied by user")
	}

	if !isStageApproved(approval, policy, agenticv1alpha1.SandboxStepExecution) {
		log.V(1).Info("execution pending approval")
		return ctrl.Result{}, nil
	}

	executed := meta.FindStatusCondition(run.Status.Conditions, agenticv1alpha1.AgenticRunConditionExecuted)
	if executed != nil {
		if executed.Status == metav1.ConditionUnknown {
			log.V(1).Info("execution already in progress, waiting")
			return ctrl.Result{}, nil
		}
		if executed.Status == metav1.ConditionTrue {
			log.V(1).Info("execution already completed")
			return ctrl.Result{}, nil
		}
	}

	selectedOption, trimErr := r.trimNonSelectedOptions(ctx, run, approval)
	if trimErr != nil {
		return r.failStep(ctx, run, agenticv1alpha1.AgenticRunConditionExecuted, trimErr)
	}

	if r.Audit != nil && !isAutoApprovedByPolicy(policy, agenticv1alpha1.SandboxStepExecution) {
		optTitle := ""
		if selectedOption != nil {
			optTitle = selectedOption.Title
		}
		r.Audit.EmitApprovalSpan(ctx, run, approval, optTitle)
	}

	base := run.DeepCopy()
	meta.RemoveStatusCondition(&run.Status.Conditions, agenticv1alpha1.AgenticRunConditionVerified)
	meta.SetStatusCondition(&run.Status.Conditions, metav1.Condition{
		Type:               agenticv1alpha1.AgenticRunConditionExecuted,
		Status:             metav1.ConditionUnknown,
		Reason:             reasonInProgress,
		Message:            "Execution agent is running",
		ObservedGeneration: run.Generation,
	})
	if err := r.statusPatch(ctx, run, base); err != nil {
		return ctrl.Result{}, fmt.Errorf("%s: %w", ErrUpdateToExecuting, err)
	}

	if err := r.Agent.Execute(ctx, run, *resolved.Execution, selectedOption); err != nil {
		return r.failStep(ctx, run, agenticv1alpha1.AgenticRunConditionExecuted, err)
	}

	log.Info("execution sandbox launched")
	return ctrl.Result{}, nil
}

// handleVerification checks approval and runs verification.
func (r *AgenticRunReconciler) handleVerification(
	ctx context.Context,
	run *agenticv1alpha1.AgenticRun,
	resolved *resolvedWorkflow,
	approval *agenticv1alpha1.AgenticRunApproval,
	policy *agenticv1alpha1.ApprovalPolicy,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	log.Info("verifying")

	base := run.DeepCopy()

	if resolved.Verification == nil {
		setVerificationSkipped(run)
		if err := r.statusPatch(ctx, run, base); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	if isStageDenied(approval, agenticv1alpha1.SandboxStepVerification) {
		if r.Audit != nil {
			r.Audit.EmitApprovalSpan(ctx, run, approval, "")
		}
		return r.denyAgenticRun(ctx, run, "Verification denied by user")
	}

	if !isStageApproved(approval, policy, agenticv1alpha1.SandboxStepVerification) {
		log.V(1).Info("verification pending approval")
		return ctrl.Result{}, nil
	}

	verified := meta.FindStatusCondition(run.Status.Conditions, agenticv1alpha1.AgenticRunConditionVerified)
	if verified != nil && verified.Status == metav1.ConditionUnknown {
		log.V(1).Info("verification already in progress, waiting")
		return ctrl.Result{}, nil
	}

	meta.SetStatusCondition(&run.Status.Conditions, metav1.Condition{
		Type:               agenticv1alpha1.AgenticRunConditionVerified,
		Status:             metav1.ConditionUnknown,
		Reason:             reasonInProgress,
		Message:            "Verification agent is running",
		ObservedGeneration: run.Generation,
	})
	if err := r.statusPatch(ctx, run, base); err != nil {
		return ctrl.Result{}, fmt.Errorf("%s: %w", ErrUpdateToVerifying, err)
	}
	base = run.DeepCopy()

	selectedOption, selErr := r.selectedOption(ctx, run)
	if selErr != nil {
		return r.failStep(ctx, run, agenticv1alpha1.AgenticRunConditionVerified, fmt.Errorf("%s: %w", ErrResolveSelectedOption, selErr))
	}

	var execOutput *ExecutionOutput
	if refs := run.Status.Steps.Execution.Results; len(refs) > 0 {
		latestRef := refs[len(refs)-1]
		var execCR agenticv1alpha1.ExecutionResult
		if err := r.Get(ctx, types.NamespacedName{Name: latestRef.Name, Namespace: r.Namespace}, &execCR); err == nil {
			execOutput = &ExecutionOutput{
				Success:      latestRef.Outcome == agenticv1alpha1.ActionOutcomeSucceeded,
				ActionsTaken: execCR.Status.ActionsTaken,
			}
		}
	}

	if err := r.Agent.Verify(ctx, run, *resolved.Verification, selectedOption, execOutput); err != nil {
		return r.failStep(ctx, run, agenticv1alpha1.AgenticRunConditionVerified, err)
	}

	log.Info("verification sandbox launched")
	return ctrl.Result{}, nil
}

// handleFailed performs RBAC cleanup for system failures.
// Audit emission is handled by handleTerminalCleanup which is called after this.
func (r *AgenticRunReconciler) handleFailed(
	ctx context.Context,
	run *agenticv1alpha1.AgenticRun,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	log.Info("handling system failure (terminal)")

	if run.Annotations[rbacNamespacesAnnotation] != "" {
		if err := cleanupExecutionRBAC(ctx, r.Client, run); err != nil {
			log.Error(err, "RBAC cleanup on failure")
		}
	}
	return ctrl.Result{}, nil
}

func (r *AgenticRunReconciler) handleSuspension(
	ctx context.Context,
	run *agenticv1alpha1.AgenticRun,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	phase := agenticv1alpha1.DerivePhase(run.Status.Conditions)

	log.Info("terminating run due to system suspension", LogKeyPhase, phase)

	if hasSandboxClaims(run) {
		if err := r.Agent.ReleaseSandboxes(ctx, run); err != nil {
			log.Error(err, "best-effort sandbox release during suspension")
		}
	}

	if isTerminal(phase) {
		return ctrl.Result{}, nil
	}

	base := run.DeepCopy()
	meta.SetStatusCondition(&run.Status.Conditions, metav1.Condition{
		Type:               agenticv1alpha1.AgenticRunConditionEmergencyStopped,
		Status:             metav1.ConditionTrue,
		Reason:             reasonSystemSuspended,
		Message:            "Terminated by system kill switch (AgenticOLSConfig.spec.suspended=true)",
		ObservedGeneration: run.Generation,
	})
	if err := r.statusPatch(ctx, run, base); err != nil {
		return ctrl.Result{}, fmt.Errorf("patch EmergencyStopped condition: %w", err)
	}
	return ctrl.Result{}, nil
}

// handleEscalation runs the escalation step: checks approval, calls the
// agent with an escalation prompt, and stores the result. Uses the analysis
// step's agent by default (or an approval-time override).
func (r *AgenticRunReconciler) handleEscalation(
	ctx context.Context,
	run *agenticv1alpha1.AgenticRun,
	resolved *resolvedWorkflow,
	approval *agenticv1alpha1.AgenticRunApproval,
	policy *agenticv1alpha1.ApprovalPolicy,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	log.V(1).Info("handling escalation")

	if isStageDenied(approval, agenticv1alpha1.SandboxStepEscalation) {
		if r.Audit != nil {
			r.Audit.EmitApprovalSpan(ctx, run, approval, "")
		}
		return r.denyAgenticRun(ctx, run, "Escalation denied by user")
	}

	if !isStageApproved(approval, policy, agenticv1alpha1.SandboxStepEscalation) {
		log.V(1).Info("escalation pending approval")
		return ctrl.Result{}, nil
	}

	escalated := meta.FindStatusCondition(run.Status.Conditions, agenticv1alpha1.AgenticRunConditionEscalated)
	if escalated != nil {
		if escalated.Status == metav1.ConditionUnknown && escalated.Reason == reasonInProgress {
			log.V(1).Info("escalation already in progress, waiting")
			return ctrl.Result{}, nil
		}
		if escalated.Status == metav1.ConditionTrue {
			log.V(1).Info("escalation already completed")
			return ctrl.Result{}, nil
		}
	}

	step := resolved.Analysis
	if override := getStageOverrideAgent(approval, agenticv1alpha1.SandboxStepEscalation); override != "" {
		var agent agenticv1alpha1.Agent
		if err := r.Get(ctx, types.NamespacedName{Name: override}, &agent); err != nil {
			return r.failStep(ctx, run, agenticv1alpha1.AgenticRunConditionEscalated, fmt.Errorf("%s %q: %w", ErrGetOverrideAgent, override, err))
		}
		var llm agenticv1alpha1.LLMProvider
		if err := r.Get(ctx, types.NamespacedName{Name: agent.Spec.LLMProvider.Name}, &llm); err != nil {
			return r.failStep(ctx, run, agenticv1alpha1.AgenticRunConditionEscalated, fmt.Errorf("%s %q: %w", ErrGetEscalationLLMProvider, agent.Spec.LLMProvider.Name, err))
		}
		step = resolvedStep{Agent: &agent, LLM: &llm, Tools: step.Tools}
	}

	base := run.DeepCopy()
	meta.SetStatusCondition(&run.Status.Conditions, metav1.Condition{
		Type:               agenticv1alpha1.AgenticRunConditionEscalated,
		Status:             metav1.ConditionUnknown,
		Reason:             reasonInProgress,
		Message:            "Escalation agent is running",
		ObservedGeneration: run.Generation,
	})
	if err := r.statusPatch(ctx, run, base); err != nil {
		return ctrl.Result{}, fmt.Errorf("%s: %w", ErrUpdateToEscalating, err)
	}

	if err := r.Agent.Escalate(ctx, run, step); err != nil {
		return r.failStep(ctx, run, agenticv1alpha1.AgenticRunConditionEscalated, err)
	}

	log.Info("escalation sandbox launched")
	return ctrl.Result{}, nil
}

func conditionTime(conditions []metav1.Condition, condType string) *metav1.Time {
	if c := meta.FindStatusCondition(conditions, condType); c != nil {
		return &c.LastTransitionTime
	}
	return nil
}

// denyAgenticRun transitions the run to Denied (terminal).
func (r *AgenticRunReconciler) denyAgenticRun(
	ctx context.Context,
	run *agenticv1alpha1.AgenticRun,
	message string,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	log.Info("denying run", "message", message)
	base := run.DeepCopy()
	meta.SetStatusCondition(&run.Status.Conditions, metav1.Condition{
		Type:               agenticv1alpha1.AgenticRunConditionDenied,
		Status:             metav1.ConditionTrue,
		Reason:             reasonUserDenied,
		Message:            message,
		ObservedGeneration: run.Generation,
	})
	if err := r.statusPatch(ctx, run, base); err != nil {
		return ctrl.Result{}, fmt.Errorf("%s: %w", ErrUpdateToDenied, err)
	}
	return ctrl.Result{}, nil
}
