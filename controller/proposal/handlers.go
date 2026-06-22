package proposal

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel/trace"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
)

const (
	ErrUpdateToAnalyzing         = "update to Analyzing"
	ErrCreateAnalysisResult      = "create analysis result"
	ErrUpdateAfterAnalysis       = "update after analysis"
	ErrUpdateToAnalyzingRevision = "update to Analyzing (revision)"
	ErrUpdateAfterRevision       = "update after revision"
	ErrUpdateToCompletedAdvisory = "update to Completed (advisory)"
	ErrUpdateAfterExecSkip       = "update after execution skip"
	ErrEnsureExecutionRBAC       = "ensure execution RBAC"
	ErrPersistRBACAnnotation     = "persist RBAC annotation"
	ErrUpdateToExecuting         = "update to Executing"
	ErrCreateExecutionResult     = "create execution result"
	ErrUpdateToCompletedTrust    = "update to Completed (trust-mode)"
	ErrUpdateToVerifying         = "update to Verifying"
	ErrResolveSelectedOption     = "resolve selected option"
	ErrCreateVerificationResult  = "create verification result"
	ErrUpdateForExecRetry        = "update for execution retry"
	ErrUpdateRetriesExhausted    = "update (retries exhausted)"
	ErrUpdateToCompleted         = "update to Completed"
	ErrGetOverrideAgent          = "get override Agent"
	ErrGetEscalationLLMProvider  = "get LLMProvider"
	ErrUpdateToEscalating        = "update to Escalating"
	ErrCreateEscalationResult    = "create escalation result"
	ErrUpdateToEscalated         = "update to Escalated"
	ErrUpdateToDenied            = "update to Denied"
)

// handleAnalysis checks approval for the analysis step and runs it.
func (r *ProposalReconciler) handleAnalysis(
	ctx context.Context,
	proposal *agenticv1alpha1.Proposal,
	resolved *resolvedWorkflow,
	approval *agenticv1alpha1.ProposalApproval,
	policy *agenticv1alpha1.ApprovalPolicy,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	log.V(1).Info("handling analysis")

	if isStageDenied(approval, agenticv1alpha1.SandboxStepAnalysis) {
		if r.Audit != nil {
			r.Audit.EndApprovalWait(proposal)
			r.Audit.EmitApprovalReceived(ctx, proposal, approval)
		}
		return r.denyProposal(ctx, proposal, "Analysis denied by user")
	}

	if !isStageApproved(approval, policy, agenticv1alpha1.SandboxStepAnalysis) {
		log.V(1).Info("analysis pending approval")
		return ctrl.Result{}, nil
	}

	analyzed := meta.FindStatusCondition(proposal.Status.Conditions, agenticv1alpha1.ProposalConditionAnalyzed)
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

	base := proposal.DeepCopy()
	meta.SetStatusCondition(&proposal.Status.Conditions, metav1.Condition{
		Type:               agenticv1alpha1.ProposalConditionAnalyzed,
		Status:             metav1.ConditionUnknown,
		Reason:             reasonInProgress,
		Message:            "Analysis agent is running",
		ObservedGeneration: proposal.Generation,
	})
	if err := r.statusPatch(ctx, proposal, base); err != nil {
		return ctrl.Result{}, fmt.Errorf("%s: %w", ErrUpdateToAnalyzing, err)
	}

	spanCtx := ctx
	var span trace.Span
	if r.Audit != nil {
		spanCtx, span = r.Audit.StartAnalysisSpan(ctx, proposal)
		if span != nil {
			defer span.End()
		}
	}

	analysisResult, err := r.Agent.Analyze(spanCtx, proposal, resolved.Analysis, proposal.Spec.Request, defaultSandboxSA)
	if err != nil {
		return r.failStep(spanCtx, proposal, agenticv1alpha1.ProposalConditionAnalyzed, err)
	}
	if !analysisResult.Success {
		return r.failStep(spanCtx, proposal, agenticv1alpha1.ProposalConditionAnalyzed, fmt.Errorf("analysis agent reported failure"))
	}
	base = proposal.DeepCopy()
	completedAt := metav1.Now()
	startTime := conditionTime(proposal.Status.Conditions, agenticv1alpha1.ProposalConditionAnalyzed)
	crName, analysisCR, crErr := r.createAnalysisResult(spanCtx, proposal, analysisResult, proposal.Status.Steps.Analysis.Sandbox, startTime, &completedAt, "")
	if crErr != nil {
		return r.failStep(spanCtx, proposal, agenticv1alpha1.ProposalConditionAnalyzed, fmt.Errorf("%s: %w", ErrCreateAnalysisResult, crErr))
	}
	if r.Audit != nil {
		r.Audit.EmitAnalysisCompleted(spanCtx, proposal, analysisCR)
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
		return ctrl.Result{}, fmt.Errorf("%s: %w", ErrUpdateAfterAnalysis, err)
	}

	if r.Audit != nil && !isStageApproved(approval, policy, agenticv1alpha1.SandboxStepExecution) {
		r.Audit.StartApprovalWait(ctx, proposal)
	}

	log.Info("analysis complete", "options", len(analysisResult.Options))
	return ctrl.Result{}, nil
}

// handleRevision re-runs analysis with revision context appended to the
// agent's system prompt.
func (r *ProposalReconciler) handleRevision(
	ctx context.Context,
	proposal *agenticv1alpha1.Proposal,
	resolved *resolvedWorkflow,
	approval *agenticv1alpha1.ProposalApproval,
	policy *agenticv1alpha1.ApprovalPolicy,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	generation := proposal.Generation
	log.V(1).Info("handling revision", "generation", generation)

	analyzed := meta.FindStatusCondition(proposal.Status.Conditions, agenticv1alpha1.ProposalConditionAnalyzed)
	if analyzed != nil && analyzed.Status == metav1.ConditionUnknown {
		log.V(1).Info("revision already in progress, waiting")
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
		return ctrl.Result{}, fmt.Errorf("%s: %w", ErrUpdateToAnalyzingRevision, err)
	}

	spanCtx := ctx
	var span trace.Span
	if r.Audit != nil {
		spanCtx, span = r.Audit.StartAnalysisSpan(ctx, proposal)
		if span != nil {
			defer span.End()
		}
	}

	revisionSuffix := buildRevisionContext(proposal)
	requestWithRevision := proposal.Spec.Request + "\n\n" + revisionSuffix

	analysisResult, err := r.Agent.Analyze(spanCtx, proposal, resolved.Analysis, requestWithRevision, defaultSandboxSA)
	if err != nil {
		return r.failStep(spanCtx, proposal, agenticv1alpha1.ProposalConditionAnalyzed, err)
	}
	if !analysisResult.Success {
		return r.failStep(spanCtx, proposal, agenticv1alpha1.ProposalConditionAnalyzed, fmt.Errorf("analysis agent reported failure"))
	}

	base = proposal.DeepCopy()
	completedAt := metav1.Now()
	startTime := conditionTime(proposal.Status.Conditions, agenticv1alpha1.ProposalConditionAnalyzed)
	crName, analysisCR, crErr := r.createAnalysisResult(spanCtx, proposal, analysisResult, proposal.Status.Steps.Analysis.Sandbox, startTime, &completedAt, "")
	if crErr != nil {
		return r.failStep(spanCtx, proposal, agenticv1alpha1.ProposalConditionAnalyzed, fmt.Errorf("%s: %w", ErrCreateAnalysisResult, crErr))
	}
	if r.Audit != nil {
		r.Audit.EmitAnalysisCompleted(spanCtx, proposal, analysisCR)
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
		return ctrl.Result{}, fmt.Errorf("%s: %w", ErrUpdateAfterRevision, err)
	}

	if r.Audit != nil && !isStageApproved(approval, policy, agenticv1alpha1.SandboxStepExecution) {
		r.Audit.StartApprovalWait(ctx, proposal)
	}

	log.Info("revision analysis complete", "generation", generation, "options", len(analysisResult.Options))
	return ctrl.Result{}, nil
}

// handleExecution checks approval and runs execution (or skips if not configured).
func (r *ProposalReconciler) handleExecution(
	ctx context.Context,
	proposal *agenticv1alpha1.Proposal,
	resolved *resolvedWorkflow,
	approval *agenticv1alpha1.ProposalApproval,
	policy *agenticv1alpha1.ApprovalPolicy,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	log.V(1).Info("handling execution")

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
				return ctrl.Result{}, fmt.Errorf("%s: %w", ErrUpdateToCompletedAdvisory, err)
			}
			log.Info("advisory-only — completed")
			return ctrl.Result{}, nil
		}

		if err := r.statusPatch(ctx, proposal, base); err != nil {
			return ctrl.Result{}, fmt.Errorf("%s: %w", ErrUpdateAfterExecSkip, err)
		}
		return ctrl.Result{}, nil
	}

	if isStageDenied(approval, agenticv1alpha1.SandboxStepExecution) {
		if r.Audit != nil {
			r.Audit.EndApprovalWait(proposal)
			r.Audit.EmitApprovalReceived(ctx, proposal, approval)
		}
		return r.denyProposal(ctx, proposal, "Execution denied by user")
	}

	if !isStageApproved(approval, policy, agenticv1alpha1.SandboxStepExecution) {
		log.V(1).Info("execution pending approval")
		return ctrl.Result{}, nil
	}

	executed := meta.FindStatusCondition(proposal.Status.Conditions, agenticv1alpha1.ProposalConditionExecuted)
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

	if r.Audit != nil {
		r.Audit.EndApprovalWait(proposal)
		r.Audit.EmitApprovalReceived(ctx, proposal, approval)
	}

	selectedOption, trimErr := r.trimNonSelectedOptions(ctx, proposal, approval, policy)
	if trimErr != nil {
		return r.failStep(ctx, proposal, agenticv1alpha1.ProposalConditionExecuted, trimErr)
	}

	// Determine which SA the execution pod should run as.
	execSA := defaultSandboxSA
	base := proposal.DeepCopy()
	if selectedOption != nil && (len(selectedOption.RBAC.NamespaceScoped) > 0 || len(selectedOption.RBAC.ClusterScoped) > 0) {
		if err := ensureExecutionRBAC(ctx, r.Client, proposal, &selectedOption.RBAC, r.Namespace); err != nil {
			return r.failStep(ctx, proposal, agenticv1alpha1.ProposalConditionExecuted, fmt.Errorf("%s: %w", ErrEnsureExecutionRBAC, err))
		}
		if err := r.Patch(ctx, proposal, client.MergeFrom(base)); err != nil {
			return ctrl.Result{}, fmt.Errorf("%s: %w", ErrPersistRBACAnnotation, err)
		}
		base = proposal.DeepCopy()
		execSA = executionSAName(proposal)
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
		return ctrl.Result{}, fmt.Errorf("%s: %w", ErrUpdateToExecuting, err)
	}

	spanCtx := ctx
	var span trace.Span
	if r.Audit != nil {
		spanCtx, span = r.Audit.StartExecutionSpan(ctx, proposal)
		if span != nil {
			defer span.End()
		}
	}

	execResult, err := r.Agent.Execute(spanCtx, proposal, *resolved.Execution, selectedOption, execSA)
	if err != nil {
		return r.failStep(spanCtx, proposal, agenticv1alpha1.ProposalConditionExecuted, err)
	}
	if !execResult.Success {
		return r.failStep(spanCtx, proposal, agenticv1alpha1.ProposalConditionExecuted, fmt.Errorf("execution agent reported failure"))
	}

	base = proposal.DeepCopy()
	completedAt := metav1.Now()
	startTime := conditionTime(proposal.Status.Conditions, agenticv1alpha1.ProposalConditionExecuted)
	execCRName, execCR, execCRErr := r.createExecutionResult(spanCtx, proposal, execResult, proposal.Status.Steps.Execution.Sandbox, startTime, &completedAt, "")
	if execCRErr != nil {
		return r.failStep(spanCtx, proposal, agenticv1alpha1.ProposalConditionExecuted, fmt.Errorf("%s: %w", ErrCreateExecutionResult, execCRErr))
	}
	if r.Audit != nil {
		r.Audit.EmitExecutionCompleted(spanCtx, proposal, execCR)
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
			return ctrl.Result{}, fmt.Errorf("%s: %w", ErrUpdateToCompletedTrust, err)
		}
		log.Info("execution complete, verification skipped")
	} else {
		if err := r.statusPatch(ctx, proposal, base); err != nil {
			return ctrl.Result{}, fmt.Errorf("%s: %w", ErrUpdateToVerifying, err)
		}
		log.Info("execution complete, verifying")
	}

	// Clean up per-proposal execution SA + Roles if one was created.
	if execSA != defaultSandboxSA {
		if err := cleanupExecutionRBAC(ctx, r.Client, proposal, r.Namespace); err != nil {
			log.Error(err, "RBAC cleanup after execution, retrying")
			return ctrl.Result{Requeue: true}, nil
		}
	}

	return ctrl.Result{}, nil
}

// handleVerification checks approval and runs verification.
func (r *ProposalReconciler) handleVerification(
	ctx context.Context,
	proposal *agenticv1alpha1.Proposal,
	resolved *resolvedWorkflow,
	approval *agenticv1alpha1.ProposalApproval,
	policy *agenticv1alpha1.ApprovalPolicy,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	// Retry execution RBAC cleanup if it failed during handleExecution transition.
	// Always attempt — idempotent; covers both namespace-scoped and cluster-scoped-only cases.
	if err := cleanupExecutionRBAC(ctx, r.Client, proposal, r.Namespace); err != nil {
		log.Error(err, "RBAC cleanup retry in verification, requeuing")
		return ctrl.Result{Requeue: true}, nil
	}

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
		if r.Audit != nil {
			r.Audit.EndApprovalWait(proposal)
			r.Audit.EmitApprovalReceived(ctx, proposal, approval)
		}
		return r.denyProposal(ctx, proposal, "Verification denied by user")
	}

	if !isStageApproved(approval, policy, agenticv1alpha1.SandboxStepVerification) {
		log.V(1).Info("verification pending approval")
		return ctrl.Result{}, nil
	}

	verified := meta.FindStatusCondition(proposal.Status.Conditions, agenticv1alpha1.ProposalConditionVerified)
	if verified != nil && verified.Status == metav1.ConditionUnknown {
		log.V(1).Info("verification already in progress, waiting")
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
		return r.failStep(ctx, proposal, agenticv1alpha1.ProposalConditionVerified, fmt.Errorf("%s: %w", ErrResolveSelectedOption, selErr))
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

	spanCtx := ctx
	var span trace.Span
	if r.Audit != nil {
		spanCtx, span = r.Audit.StartVerificationSpan(ctx, proposal)
		if span != nil {
			defer span.End()
		}
	}

	verifyResult, err := r.Agent.Verify(spanCtx, proposal, *resolved.Verification, selectedOption, execOutput, defaultSandboxSA)
	if err != nil {
		return r.failStep(spanCtx, proposal, agenticv1alpha1.ProposalConditionVerified, err)
	}

	base = proposal.DeepCopy()
	completedAt := metav1.Now()
	startTime := conditionTime(proposal.Status.Conditions, agenticv1alpha1.ProposalConditionVerified)
	verifyCRName, verifyCR, verifyCRErr := r.createVerificationResult(spanCtx, proposal, verifyResult, proposal.Status.Steps.Verification.Sandbox, startTime, &completedAt, "")
	if verifyCRErr != nil {
		return r.failStep(spanCtx, proposal, agenticv1alpha1.ProposalConditionVerified, fmt.Errorf("%s: %w", ErrCreateVerificationResult, verifyCRErr))
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
			log.Info("verification failed, retrying execution", "attempt", next+1, "maxAttempts", maxRetries, LogKeySummary, verifyResult.Summary)
			if r.Audit != nil {
				r.Audit.EmitVerificationRetry(spanCtx, proposal, verifyCR, int(next))
			}
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
			if err := r.statusPatch(spanCtx, proposal, base); err != nil {
				return ctrl.Result{}, fmt.Errorf("%s: %w", ErrUpdateForExecRetry, err)
			}
			return ctrl.Result{}, nil
		}

		log.Info("verification retries exhausted, escalating", "retryCount", retryCount, LogKeySummary, verifyResult.Summary)
		if r.Audit != nil {
			r.Audit.EmitVerificationCompleted(spanCtx, proposal, verifyCR)
		}
		meta.SetStatusCondition(&proposal.Status.Conditions, metav1.Condition{
			Type:               agenticv1alpha1.ProposalConditionVerified,
			Status:             metav1.ConditionFalse,
			Reason:             reasonRetriesExhausted,
			Message:            fmt.Sprintf("Verification failed after %d attempt(s): %s", retryCount+1, verifyResult.Summary),
			ObservedGeneration: proposal.Generation,
		})
		meta.SetStatusCondition(&proposal.Status.Conditions, metav1.Condition{
			Type:               agenticv1alpha1.ProposalConditionEscalated,
			Status:             metav1.ConditionUnknown,
			Reason:             reasonRetriesExhausted,
			Message:            fmt.Sprintf("Verification failed after %d attempt(s), escalating", retryCount+1),
			ObservedGeneration: proposal.Generation,
		})
		if err := r.statusPatch(ctx, proposal, base); err != nil {
			return ctrl.Result{}, fmt.Errorf("%s: %w", ErrUpdateRetriesExhausted, err)
		}
		return ctrl.Result{}, nil
	}

	if r.Audit != nil {
		r.Audit.EmitVerificationCompleted(spanCtx, proposal, verifyCR)
	}

	meta.SetStatusCondition(&proposal.Status.Conditions, metav1.Condition{
		Type:               agenticv1alpha1.ProposalConditionVerified,
		Status:             metav1.ConditionTrue,
		Reason:             reasonPassed,
		Message:            verifyResult.Summary,
		ObservedGeneration: proposal.Generation,
	})
	if err := r.statusPatch(spanCtx, proposal, base); err != nil {
		return ctrl.Result{}, fmt.Errorf("%s: %w", ErrUpdateToCompleted, err)
	}

	log.Info("verification passed", LogKeySummary, verifyResult.Summary)
	return ctrl.Result{}, nil
}

// handleFailed performs cleanup for system failures.
func (r *ProposalReconciler) handleFailed(
	ctx context.Context,
	proposal *agenticv1alpha1.Proposal,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	log.Info("handling system failure (terminal)")

	if r.Audit != nil {
		r.Audit.EndApprovalWait(proposal)
		r.Audit.EmitProposalTerminal(ctx, proposal, string(agenticv1alpha1.ProposalPhaseFailed), terminalReason(proposal))
		r.Audit.EndLifecycleSpan(proposal)
	}

	if proposal.Annotations[rbacNamespacesAnnotation] != "" {
		if err := cleanupExecutionRBAC(ctx, r.Client, proposal, r.Namespace); err != nil {
			log.Error(err, "RBAC cleanup on failure")
		}
	}
	return ctrl.Result{}, nil
}

func (r *ProposalReconciler) handleSuspension(
	ctx context.Context,
	proposal *agenticv1alpha1.Proposal,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	phase := agenticv1alpha1.DerivePhase(proposal.Status.Conditions)

	log.Info("terminating proposal due to system suspension", LogKeyPhase, phase)

	if hasSandboxClaims(proposal) {
		if err := r.Agent.ReleaseSandboxes(ctx, proposal); err != nil {
			log.Error(err, "best-effort sandbox release during suspension")
		}
	}

	if proposal.Annotations[rbacNamespacesAnnotation] != "" {
		if err := cleanupExecutionRBAC(ctx, r.Client, proposal, r.Namespace); err != nil {
			log.Error(err, "best-effort RBAC cleanup during suspension")
		}
	}

	if isTerminal(phase) {
		return ctrl.Result{}, nil
	}

	base := proposal.DeepCopy()
	meta.SetStatusCondition(&proposal.Status.Conditions, metav1.Condition{
		Type:               agenticv1alpha1.ProposalConditionEmergencyStopped,
		Status:             metav1.ConditionTrue,
		Reason:             reasonSystemSuspended,
		Message:            "Terminated by system kill switch (AgenticOLSConfig.spec.suspended=true)",
		ObservedGeneration: proposal.Generation,
	})
	if err := r.statusPatch(ctx, proposal, base); err != nil {
		return ctrl.Result{}, fmt.Errorf("patch EmergencyStopped condition: %w", err)
	}
	return ctrl.Result{}, nil
}

// handleEscalation runs the escalation step: checks approval, calls the
// agent with an escalation prompt, and stores the result. Uses the analysis
// step's agent by default (or an approval-time override).
func (r *ProposalReconciler) handleEscalation(
	ctx context.Context,
	proposal *agenticv1alpha1.Proposal,
	resolved *resolvedWorkflow,
	approval *agenticv1alpha1.ProposalApproval,
	policy *agenticv1alpha1.ApprovalPolicy,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	log.V(1).Info("handling escalation")

	if isStageDenied(approval, agenticv1alpha1.SandboxStepEscalation) {
		if r.Audit != nil {
			r.Audit.EndApprovalWait(proposal)
			r.Audit.EmitApprovalReceived(ctx, proposal, approval)
		}
		return r.denyProposal(ctx, proposal, "Escalation denied by user")
	}

	if !isStageApproved(approval, policy, agenticv1alpha1.SandboxStepEscalation) {
		log.V(1).Info("escalation pending approval")
		return ctrl.Result{}, nil
	}

	escalated := meta.FindStatusCondition(proposal.Status.Conditions, agenticv1alpha1.ProposalConditionEscalated)
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
			return r.failStep(ctx, proposal, agenticv1alpha1.ProposalConditionEscalated, fmt.Errorf("%s %q: %w", ErrGetOverrideAgent, override, err))
		}
		var llm agenticv1alpha1.LLMProvider
		if err := r.Get(ctx, types.NamespacedName{Name: agent.Spec.LLMProvider.Name}, &llm); err != nil {
			return r.failStep(ctx, proposal, agenticv1alpha1.ProposalConditionEscalated, fmt.Errorf("%s %q: %w", ErrGetEscalationLLMProvider, agent.Spec.LLMProvider.Name, err))
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
		return ctrl.Result{}, fmt.Errorf("%s: %w", ErrUpdateToEscalating, err)
	}

	spanCtx := ctx
	var span trace.Span
	if r.Audit != nil {
		spanCtx, span = r.Audit.StartEscalationSpan(ctx, proposal)
		if span != nil {
			defer span.End()
		}
	}

	escalationText := buildEscalationRequest(proposal)
	escalationResult, err := r.Agent.Escalate(spanCtx, proposal, step, escalationText, defaultSandboxSA)
	if err != nil {
		return r.failStep(spanCtx, proposal, agenticv1alpha1.ProposalConditionEscalated, err)
	}

	base = proposal.DeepCopy()
	completedAt := metav1.Now()
	startTime := conditionTime(proposal.Status.Conditions, agenticv1alpha1.ProposalConditionEscalated)
	crName, escalationCR, crErr := r.createEscalationResult(spanCtx, proposal, escalationResult, proposal.Status.Steps.Escalation.Sandbox, startTime, &completedAt, "")
	if crErr != nil {
		return r.failStep(spanCtx, proposal, agenticv1alpha1.ProposalConditionEscalated, fmt.Errorf("%s: %w", ErrCreateEscalationResult, crErr))
	}
	if r.Audit != nil {
		r.Audit.EmitEscalationCompleted(spanCtx, proposal, escalationCR)
	}
	proposal.Status.Steps.Escalation.Results = append(proposal.Status.Steps.Escalation.Results, agenticv1alpha1.StepResultRef{Name: crName, Outcome: agenticv1alpha1.ActionOutcomeFromBool(escalationResult.Success)})

	if proposal.Annotations[rbacNamespacesAnnotation] != "" {
		if cleanErr := cleanupExecutionRBAC(ctx, r.Client, proposal, r.Namespace); cleanErr != nil {
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
		return ctrl.Result{}, fmt.Errorf("%s: %w", ErrUpdateToEscalated, err)
	}

	log.Info("escalation complete", LogKeySummary, escalationResult.Summary)
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
	proposal *agenticv1alpha1.Proposal,
	message string,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
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
		return ctrl.Result{}, fmt.Errorf("%s: %w", ErrUpdateToDenied, err)
	}
	return ctrl.Result{}, nil
}
