package proposal

import (
	"context"
	"fmt"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
)

// handleAnalysis checks approval for the analysis step and runs it.
func (r *ProposalReconciler) handleAnalysis(
	ctx context.Context,
	log logr.Logger,
	proposal *agenticv1alpha1.Proposal,
	resolved *resolvedWorkflow,
	approval *agenticv1alpha1.ProposalApproval,
	policy *agenticv1alpha1.ApprovalPolicy,
) (ctrl.Result, error) {
	log.Info("handling analysis")

	if isStageDenied(approval, agenticv1alpha1.SandboxStepAnalysis) {
		return r.denyProposal(ctx, log, proposal, "Analysis denied by user")
	}

	if !isStageApproved(approval, policy, agenticv1alpha1.SandboxStepAnalysis) {
		log.Info("analysis pending approval")
		return ctrl.Result{}, nil
	}

	analyzed := meta.FindStatusCondition(proposal.Status.Conditions, agenticv1alpha1.ProposalConditionAnalyzed)
	if analyzed != nil {
		if analyzed.Status == metav1.ConditionUnknown {
			log.Info("analysis already in progress, waiting")
			return ctrl.Result{}, nil
		}
		if analyzed.Status == metav1.ConditionTrue {
			log.Info("analysis already completed")
			return ctrl.Result{}, nil
		}
	}

	base := proposal.DeepCopy()
	meta.SetStatusCondition(&proposal.Status.Conditions, metav1.Condition{
		Type:               agenticv1alpha1.ProposalConditionAnalyzed,
		Status:             metav1.ConditionUnknown,
		Reason:             reasonInProgress,
		Message:            "Analysis agent is running",
		ObservedGeneration: proposal.Generation,
	})
	if err := r.statusPatch(ctx, proposal, base); err != nil {
		return ctrl.Result{}, fmt.Errorf("update to Analyzing: %w", err)
	}

	analysisResult, err := r.Agent.Analyze(ctx, proposal, resolved.Analysis, proposal.Spec.Request)
	if err != nil {
		return r.failStep(ctx, log, proposal, agenticv1alpha1.ProposalConditionAnalyzed, err)
	}
	base = proposal.DeepCopy()
	completedAt := metav1.Now()
	startTime := conditionTime(proposal.Status.Conditions, agenticv1alpha1.ProposalConditionAnalyzed)
	crName, crErr := r.createAnalysisResult(ctx, proposal, analysisResult, proposal.Status.Steps.Analysis.Sandbox, startTime, &completedAt, "")
	if crErr != nil {
		return r.failStep(ctx, log, proposal, agenticv1alpha1.ProposalConditionAnalyzed, fmt.Errorf("create analysis result: %w", crErr))
	}
	proposal.Status.Steps.Analysis.Results = append(proposal.Status.Steps.Analysis.Results, agenticv1alpha1.StepResultRef{Name: crName, Outcome: agenticv1alpha1.ActionOutcomeFromBool(analysisResult.Success)})
	meta.SetStatusCondition(&proposal.Status.Conditions, metav1.Condition{
		Type:               agenticv1alpha1.ProposalConditionAnalyzed,
		Status:             metav1.ConditionTrue,
		Reason:             reasonComplete,
		Message:            fmt.Sprintf("Analysis complete with %d option(s)", len(analysisResult.Options)),
		ObservedGeneration: proposal.Generation,
	})
	if err := r.statusPatch(ctx, proposal, base); err != nil {
		return ctrl.Result{}, fmt.Errorf("update after analysis: %w", err)
	}

	log.Info("analysis complete", "options", len(analysisResult.Options))
	return ctrl.Result{}, nil
}

// handleRevision re-runs analysis with revision context appended to the
// agent's system prompt.
func (r *ProposalReconciler) handleRevision(
	ctx context.Context,
	log logr.Logger,
	proposal *agenticv1alpha1.Proposal,
	resolved *resolvedWorkflow,
) (ctrl.Result, error) {
	generation := proposal.Generation
	log.Info("handling revision", "generation", generation)

	analyzed := meta.FindStatusCondition(proposal.Status.Conditions, agenticv1alpha1.ProposalConditionAnalyzed)
	if analyzed != nil && analyzed.Status == metav1.ConditionUnknown {
		log.Info("revision already in progress, waiting")
		return ctrl.Result{}, nil
	}

	base := proposal.DeepCopy()
	meta.RemoveStatusCondition(&proposal.Status.Conditions, agenticv1alpha1.ProposalConditionExecuted)
	meta.RemoveStatusCondition(&proposal.Status.Conditions, agenticv1alpha1.ProposalConditionVerified)
	meta.RemoveStatusCondition(&proposal.Status.Conditions, agenticv1alpha1.ProposalConditionEscalated)
	resetExecutionAndVerification(&proposal.Status.Steps)
	meta.SetStatusCondition(&proposal.Status.Conditions, metav1.Condition{
		Type:               agenticv1alpha1.ProposalConditionAnalyzed,
		Status:             metav1.ConditionUnknown,
		Reason:             reasonRevising,
		Message:            fmt.Sprintf("Re-analyzing for generation %d", generation),
		ObservedGeneration: proposal.Generation,
	})
	if err := r.statusPatch(ctx, proposal, base); err != nil {
		return ctrl.Result{}, fmt.Errorf("update to Analyzing (revision): %w", err)
	}

	revisionSuffix := buildRevisionContext(proposal)
	requestWithRevision := proposal.Spec.Request + "\n\n" + revisionSuffix

	analysisResult, err := r.Agent.Analyze(ctx, proposal, resolved.Analysis, requestWithRevision)
	if err != nil {
		return r.failStep(ctx, log, proposal, agenticv1alpha1.ProposalConditionAnalyzed, err)
	}

	base = proposal.DeepCopy()
	completedAt := metav1.Now()
	startTime := conditionTime(proposal.Status.Conditions, agenticv1alpha1.ProposalConditionAnalyzed)
	crName, crErr := r.createAnalysisResult(ctx, proposal, analysisResult, proposal.Status.Steps.Analysis.Sandbox, startTime, &completedAt, "")
	if crErr != nil {
		return r.failStep(ctx, log, proposal, agenticv1alpha1.ProposalConditionAnalyzed, fmt.Errorf("create analysis result: %w", crErr))
	}
	proposal.Status.Steps.Analysis.Results = append(proposal.Status.Steps.Analysis.Results, agenticv1alpha1.StepResultRef{Name: crName, Outcome: agenticv1alpha1.ActionOutcomeFromBool(analysisResult.Success)})
	meta.SetStatusCondition(&proposal.Status.Conditions, metav1.Condition{
		Type:               agenticv1alpha1.ProposalConditionAnalyzed,
		Status:             metav1.ConditionTrue,
		Reason:             reasonRevisionComplete,
		Message:            fmt.Sprintf("Revision complete (generation %d) with %d option(s)", generation, len(analysisResult.Options)),
		ObservedGeneration: generation,
	})
	if err := r.statusPatch(ctx, proposal, base); err != nil {
		return ctrl.Result{}, fmt.Errorf("update after revision: %w", err)
	}

	log.Info("revision analysis complete", "generation", generation, "options", len(analysisResult.Options))
	return ctrl.Result{}, nil
}

// handleExecution checks approval and runs execution (or skips if not configured).
func (r *ProposalReconciler) handleExecution(
	ctx context.Context,
	log logr.Logger,
	proposal *agenticv1alpha1.Proposal,
	resolved *resolvedWorkflow,
	approval *agenticv1alpha1.ProposalApproval,
	policy *agenticv1alpha1.ApprovalPolicy,
) (ctrl.Result, error) {
	log.Info("handling execution")

	if resolved.Execution == nil {
		base := proposal.DeepCopy()
		meta.SetStatusCondition(&proposal.Status.Conditions, metav1.Condition{
			Type:               agenticv1alpha1.ProposalConditionExecuted,
			Status:             metav1.ConditionTrue,
			Reason:             reasonSkipped,
			Message:            "Execution step not configured",
			ObservedGeneration: proposal.Generation,
		})

		if resolved.Verification == nil {
			setVerificationSkipped(proposal)
			if err := r.statusPatch(ctx, proposal, base); err != nil {
				return ctrl.Result{}, fmt.Errorf("update to Completed (advisory): %w", err)
			}
			log.Info("advisory-only — completed")
			return ctrl.Result{}, nil
		}

		if err := r.statusPatch(ctx, proposal, base); err != nil {
			return ctrl.Result{}, fmt.Errorf("update after execution skip: %w", err)
		}
		return ctrl.Result{}, nil
	}

	if isStageDenied(approval, agenticv1alpha1.SandboxStepExecution) {
		return r.denyProposal(ctx, log, proposal, "Execution denied by user")
	}

	if !isStageApproved(approval, policy, agenticv1alpha1.SandboxStepExecution) {
		log.Info("execution pending approval")
		return ctrl.Result{}, nil
	}

	executed := meta.FindStatusCondition(proposal.Status.Conditions, agenticv1alpha1.ProposalConditionExecuted)
	if executed != nil {
		if executed.Status == metav1.ConditionUnknown {
			log.Info("execution already in progress, waiting")
			return ctrl.Result{}, nil
		}
		if executed.Status == metav1.ConditionTrue {
			log.Info("execution already completed")
			return ctrl.Result{}, nil
		}
	}

	selectedOption, trimErr := r.trimNonSelectedOptions(ctx, proposal, approval, policy)
	if trimErr != nil {
		return r.failStep(ctx, log, proposal, agenticv1alpha1.ProposalConditionExecuted, trimErr)
	}

	base := proposal.DeepCopy()
	if selectedOption != nil && (len(selectedOption.RBAC.NamespaceScoped) > 0 || len(selectedOption.RBAC.ClusterScoped) > 0) {
		if err := ensureExecutionRBAC(ctx, r.Client, proposal, &selectedOption.RBAC, defaultSandboxSA, proposal.Namespace); err != nil {
			return r.failStep(ctx, log, proposal, agenticv1alpha1.ProposalConditionExecuted, fmt.Errorf("ensure execution RBAC: %w", err))
		}
		if err := r.Patch(ctx, proposal, client.MergeFrom(base)); err != nil {
			return ctrl.Result{}, fmt.Errorf("persist RBAC annotation: %w", err)
		}
		base = proposal.DeepCopy()
	}

	meta.RemoveStatusCondition(&proposal.Status.Conditions, agenticv1alpha1.ProposalConditionVerified)
	meta.SetStatusCondition(&proposal.Status.Conditions, metav1.Condition{
		Type:               agenticv1alpha1.ProposalConditionExecuted,
		Status:             metav1.ConditionUnknown,
		Reason:             reasonInProgress,
		Message:            "Execution agent is running",
		ObservedGeneration: proposal.Generation,
	})
	if err := r.statusPatch(ctx, proposal, base); err != nil {
		return ctrl.Result{}, fmt.Errorf("update to Executing: %w", err)
	}

	execResult, err := r.Agent.Execute(ctx, proposal, *resolved.Execution, selectedOption)
	if err != nil {
		return r.failStep(ctx, log, proposal, agenticv1alpha1.ProposalConditionExecuted, err)
	}
	if !execResult.Success {
		return r.failStep(ctx, log, proposal, agenticv1alpha1.ProposalConditionExecuted, fmt.Errorf("execution agent reported failure"))
	}

	base = proposal.DeepCopy()
	completedAt := metav1.Now()
	startTime := conditionTime(proposal.Status.Conditions, agenticv1alpha1.ProposalConditionExecuted)
	execCRName, execCRErr := r.createExecutionResult(ctx, proposal, execResult, proposal.Status.Steps.Execution.Sandbox, startTime, &completedAt, "")
	if execCRErr != nil {
		return r.failStep(ctx, log, proposal, agenticv1alpha1.ProposalConditionExecuted, fmt.Errorf("create execution result: %w", execCRErr))
	}
	proposal.Status.Steps.Execution.Results = append(proposal.Status.Steps.Execution.Results, agenticv1alpha1.StepResultRef{Name: execCRName, Outcome: agenticv1alpha1.ActionOutcomeFromBool(execResult.Success)})
	meta.SetStatusCondition(&proposal.Status.Conditions, metav1.Condition{
		Type:               agenticv1alpha1.ProposalConditionExecuted,
		Status:             metav1.ConditionTrue,
		Reason:             reasonComplete,
		Message:            "Execution completed",
		ObservedGeneration: proposal.Generation,
	})

	if resolved.Verification == nil {
		setVerificationSkipped(proposal)
		if err := r.statusPatch(ctx, proposal, base); err != nil {
			return ctrl.Result{}, fmt.Errorf("update to Completed (trust-mode): %w", err)
		}
		log.Info("execution complete, verification skipped")
		return ctrl.Result{}, nil
	}

	if err := r.statusPatch(ctx, proposal, base); err != nil {
		return ctrl.Result{}, fmt.Errorf("update to Verifying: %w", err)
	}

	log.Info("execution complete, verifying")
	return ctrl.Result{}, nil
}

// handleVerification checks approval and runs verification.
func (r *ProposalReconciler) handleVerification(
	ctx context.Context,
	log logr.Logger,
	proposal *agenticv1alpha1.Proposal,
	resolved *resolvedWorkflow,
	approval *agenticv1alpha1.ProposalApproval,
	policy *agenticv1alpha1.ApprovalPolicy,
) (ctrl.Result, error) {
	log.Info("verifying")

	base := proposal.DeepCopy()

	if resolved.Verification == nil {
		setVerificationSkipped(proposal)
		if err := r.statusPatch(ctx, proposal, base); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	if isStageDenied(approval, agenticv1alpha1.SandboxStepVerification) {
		return r.denyProposal(ctx, log, proposal, "Verification denied by user")
	}

	if !isStageApproved(approval, policy, agenticv1alpha1.SandboxStepVerification) {
		log.Info("verification pending approval")
		return ctrl.Result{}, nil
	}

	verified := meta.FindStatusCondition(proposal.Status.Conditions, agenticv1alpha1.ProposalConditionVerified)
	if verified != nil && verified.Status == metav1.ConditionUnknown {
		log.Info("verification already in progress, waiting")
		return ctrl.Result{}, nil
	}

	meta.SetStatusCondition(&proposal.Status.Conditions, metav1.Condition{
		Type:               agenticv1alpha1.ProposalConditionVerified,
		Status:             metav1.ConditionUnknown,
		Reason:             reasonInProgress,
		Message:            "Verification agent is running",
		ObservedGeneration: proposal.Generation,
	})

	selectedOption, selErr := r.selectedOption(ctx, proposal)
	if selErr != nil {
		return r.failStep(ctx, log, proposal, agenticv1alpha1.ProposalConditionVerified, fmt.Errorf("resolve selected option: %w", selErr))
	}

	var execOutput *ExecutionOutput
	if refs := proposal.Status.Steps.Execution.Results; len(refs) > 0 {
		latestRef := refs[len(refs)-1]
		var execCR agenticv1alpha1.ExecutionResult
		if err := r.Get(ctx, types.NamespacedName{Name: latestRef.Name, Namespace: proposal.Namespace}, &execCR); err == nil {
			execOutput = &ExecutionOutput{
				Success:      latestRef.Outcome == agenticv1alpha1.ActionOutcomeSucceeded,
				ActionsTaken: execCR.Status.ActionsTaken,
				Verification: execCR.Status.Verification,
			}
		}
	}

	verifyResult, err := r.Agent.Verify(ctx, proposal, *resolved.Verification, selectedOption, execOutput)
	if err != nil {
		return r.failStep(ctx, log, proposal, agenticv1alpha1.ProposalConditionVerified, err)
	}

	base = proposal.DeepCopy()
	completedAt := metav1.Now()
	startTime := conditionTime(proposal.Status.Conditions, agenticv1alpha1.ProposalConditionVerified)
	verifyCRName, verifyCRErr := r.createVerificationResult(ctx, proposal, verifyResult, proposal.Status.Steps.Verification.Sandbox, startTime, &completedAt, "")
	if verifyCRErr != nil {
		return r.failStep(ctx, log, proposal, agenticv1alpha1.ProposalConditionVerified, fmt.Errorf("create verification result: %w", verifyCRErr))
	}
	proposal.Status.Steps.Verification.Results = append(proposal.Status.Steps.Verification.Results, agenticv1alpha1.StepResultRef{Name: verifyCRName, Outcome: agenticv1alpha1.ActionOutcomeFromBool(verifyResult.Success)})

	allPassed := verifyResult.Success
	for _, check := range verifyResult.Checks {
		if check.Result != agenticv1alpha1.CheckResultPassed {
			allPassed = false
			break
		}
	}

	if !allPassed {
		retryCount := int32(0)
		if proposal.Status.Steps.Execution.RetryCount != nil {
			retryCount = *proposal.Status.Steps.Execution.RetryCount
		}
		maxRetries := maxAttempts(approval, policy)

		if int(retryCount) < maxRetries-1 {
			next := retryCount + 1
			log.Info("verification failed, retrying execution", "attempt", next+1, "maxAttempts", maxRetries, "summary", verifyResult.Summary)
			proposal.Status.Steps.Execution.RetryCount = &next
			resetExecutionAndVerification(&proposal.Status.Steps)
			meta.RemoveStatusCondition(&proposal.Status.Conditions, agenticv1alpha1.ProposalConditionExecuted)
			meta.SetStatusCondition(&proposal.Status.Conditions, metav1.Condition{
				Type:               agenticv1alpha1.ProposalConditionVerified,
				Status:             metav1.ConditionFalse,
				Reason:             reasonRetryingExecution,
				Message:            fmt.Sprintf("Verification failed (attempt %d/%d): %s", next+1, maxRetries, verifyResult.Summary),
				ObservedGeneration: proposal.Generation,
			})
			if err := r.statusPatch(ctx, proposal, base); err != nil {
				return ctrl.Result{}, fmt.Errorf("update for execution retry: %w", err)
			}
			return ctrl.Result{}, nil
		}

		log.Info("verification retries exhausted, escalating", "retryCount", retryCount, "summary", verifyResult.Summary)
		meta.SetStatusCondition(&proposal.Status.Conditions, metav1.Condition{
			Type:               agenticv1alpha1.ProposalConditionVerified,
			Status:             metav1.ConditionFalse,
			Reason:             reasonRetriesExhausted,
			Message:            fmt.Sprintf("Verification failed after %d attempt(s): %s", retryCount, verifyResult.Summary),
			ObservedGeneration: proposal.Generation,
		})
		meta.SetStatusCondition(&proposal.Status.Conditions, metav1.Condition{
			Type:               agenticv1alpha1.ProposalConditionEscalated,
			Status:             metav1.ConditionUnknown,
			Reason:             reasonRetriesExhausted,
			Message:            fmt.Sprintf("Verification failed after %d attempt(s), escalating", retryCount),
			ObservedGeneration: proposal.Generation,
		})
		if err := r.statusPatch(ctx, proposal, base); err != nil {
			return ctrl.Result{}, fmt.Errorf("update (retries exhausted): %w", err)
		}
		return ctrl.Result{}, nil
	}

	meta.SetStatusCondition(&proposal.Status.Conditions, metav1.Condition{
		Type:               agenticv1alpha1.ProposalConditionVerified,
		Status:             metav1.ConditionTrue,
		Reason:             reasonPassed,
		Message:            verifyResult.Summary,
		ObservedGeneration: proposal.Generation,
	})
	if err := r.statusPatch(ctx, proposal, base); err != nil {
		return ctrl.Result{}, fmt.Errorf("update to Completed: %w", err)
	}

	log.Info("verification passed", "summary", verifyResult.Summary)
	return ctrl.Result{}, nil
}

// handleFailed performs cleanup for system failures.
func (r *ProposalReconciler) handleFailed(
	ctx context.Context,
	log logr.Logger,
	proposal *agenticv1alpha1.Proposal,
) (ctrl.Result, error) {
	log.Info("handling system failure (terminal)")

	if proposal.Annotations[rbacNamespacesAnnotation] != "" {
		if err := cleanupExecutionRBAC(ctx, r.Client, proposal); err != nil {
			log.Error(err, "RBAC cleanup on failure")
		}
	}
	return ctrl.Result{}, nil
}

// handleEscalation runs the escalation step: checks approval, calls the
// agent with an escalation prompt, and stores the result. Uses the analysis
// step's agent by default (or an approval-time override).
func (r *ProposalReconciler) handleEscalation(
	ctx context.Context,
	log logr.Logger,
	proposal *agenticv1alpha1.Proposal,
	resolved *resolvedWorkflow,
	approval *agenticv1alpha1.ProposalApproval,
	policy *agenticv1alpha1.ApprovalPolicy,
) (ctrl.Result, error) {
	log.Info("handling escalation")

	if isStageDenied(approval, agenticv1alpha1.SandboxStepEscalation) {
		return r.denyProposal(ctx, log, proposal, "Escalation denied by user")
	}

	if !isStageApproved(approval, policy, agenticv1alpha1.SandboxStepEscalation) {
		log.Info("escalation pending approval")
		return ctrl.Result{}, nil
	}

	escalated := meta.FindStatusCondition(proposal.Status.Conditions, agenticv1alpha1.ProposalConditionEscalated)
	if escalated != nil {
		if escalated.Status == metav1.ConditionUnknown && escalated.Reason == reasonInProgress {
			log.Info("escalation already in progress, waiting")
			return ctrl.Result{}, nil
		}
		if escalated.Status == metav1.ConditionTrue {
			log.Info("escalation already completed")
			return ctrl.Result{}, nil
		}
	}

	step := resolved.Analysis
	if override := getStageOverrideAgent(approval, agenticv1alpha1.SandboxStepEscalation); override != "" {
		var agent agenticv1alpha1.Agent
		if err := r.Get(ctx, types.NamespacedName{Name: override}, &agent); err != nil {
			return r.failStep(ctx, log, proposal, agenticv1alpha1.ProposalConditionEscalated, fmt.Errorf("get override Agent %q: %w", override, err))
		}
		var llm agenticv1alpha1.LLMProvider
		if err := r.Get(ctx, types.NamespacedName{Name: agent.Spec.LLMProvider.Name}, &llm); err != nil {
			return r.failStep(ctx, log, proposal, agenticv1alpha1.ProposalConditionEscalated, fmt.Errorf("get LLMProvider %q: %w", agent.Spec.LLMProvider.Name, err))
		}
		step = resolvedStep{Agent: &agent, LLM: &llm, Tools: step.Tools}
	}

	base := proposal.DeepCopy()
	meta.SetStatusCondition(&proposal.Status.Conditions, metav1.Condition{
		Type:               agenticv1alpha1.ProposalConditionEscalated,
		Status:             metav1.ConditionUnknown,
		Reason:             reasonInProgress,
		Message:            "Escalation agent is running",
		ObservedGeneration: proposal.Generation,
	})
	if err := r.statusPatch(ctx, proposal, base); err != nil {
		return ctrl.Result{}, fmt.Errorf("update to Escalating: %w", err)
	}

	escalationText := buildEscalationRequest(proposal)
	escalationResult, err := r.Agent.Escalate(ctx, proposal, step, escalationText)
	if err != nil {
		return r.failStep(ctx, log, proposal, agenticv1alpha1.ProposalConditionEscalated, err)
	}

	base = proposal.DeepCopy()
	completedAt := metav1.Now()
	startTime := conditionTime(proposal.Status.Conditions, agenticv1alpha1.ProposalConditionEscalated)
	crName, crErr := r.createEscalationResult(ctx, proposal, escalationResult, proposal.Status.Steps.Escalation.Sandbox, startTime, &completedAt, "")
	if crErr != nil {
		return r.failStep(ctx, log, proposal, agenticv1alpha1.ProposalConditionEscalated, fmt.Errorf("create escalation result: %w", crErr))
	}
	proposal.Status.Steps.Escalation.Results = append(proposal.Status.Steps.Escalation.Results, agenticv1alpha1.StepResultRef{Name: crName, Outcome: agenticv1alpha1.ActionOutcomeFromBool(escalationResult.Success)})

	if proposal.Annotations[rbacNamespacesAnnotation] != "" {
		if cleanErr := cleanupExecutionRBAC(ctx, r.Client, proposal); cleanErr != nil {
			log.Error(cleanErr, "RBAC cleanup on escalation")
		}
	}

	meta.SetStatusCondition(&proposal.Status.Conditions, metav1.Condition{
		Type:               agenticv1alpha1.ProposalConditionEscalated,
		Status:             metav1.ConditionTrue,
		Reason:             reasonComplete,
		Message:            escalationResult.Summary,
		ObservedGeneration: proposal.Generation,
	})
	if err := r.statusPatch(ctx, proposal, base); err != nil {
		return ctrl.Result{}, fmt.Errorf("update to Escalated: %w", err)
	}

	log.Info("escalation complete", "summary", escalationResult.Summary)
	return ctrl.Result{}, nil
}

func conditionTime(conditions []metav1.Condition, condType string) *metav1.Time {
	if c := meta.FindStatusCondition(conditions, condType); c != nil {
		return &c.LastTransitionTime
	}
	return nil
}

// denyProposal transitions the proposal to Denied (terminal).
func (r *ProposalReconciler) denyProposal(
	ctx context.Context,
	log logr.Logger,
	proposal *agenticv1alpha1.Proposal,
	message string,
) (ctrl.Result, error) {
	log.Info("denying proposal", "message", message)
	base := proposal.DeepCopy()
	meta.SetStatusCondition(&proposal.Status.Conditions, metav1.Condition{
		Type:               agenticv1alpha1.ProposalConditionDenied,
		Status:             metav1.ConditionTrue,
		Reason:             reasonUserDenied,
		Message:            message,
		ObservedGeneration: proposal.Generation,
	})
	if err := r.statusPatch(ctx, proposal, base); err != nil {
		return ctrl.Result{}, fmt.Errorf("update to Denied: %w", err)
	}
	return ctrl.Result{}, nil
}
