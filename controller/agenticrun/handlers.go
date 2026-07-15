package agenticrun

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

	spanCtx := ctx
	var span trace.Span
	if r.Audit != nil {
		spanCtx, span = r.Audit.StartAnalysisSpan(ctx, run)
		if span != nil {
			defer span.End()
		}
		r.Audit.EmitAgenticRunReceived(spanCtx, run)
	}

	analysisResult, err := r.Agent.Analyze(spanCtx, run, resolved.Analysis, run.Spec.Request, defaultSandboxSA)
	if err != nil {
		return r.failStep(spanCtx, run, agenticv1alpha1.AgenticRunConditionAnalyzed, err)
	}
	if !analysisResult.Success {
		return r.failStep(spanCtx, run, agenticv1alpha1.AgenticRunConditionAnalyzed, fmt.Errorf("analysis agent reported failure"))
	}
	base = run.DeepCopy()
	completedAt := metav1.Now()
	startTime := conditionTime(run.Status.Conditions, agenticv1alpha1.AgenticRunConditionAnalyzed)
	crName, analysisCR, crErr := r.createAnalysisResult(spanCtx, run, analysisResult, run.Status.Steps.Analysis.Sandbox, startTime, &completedAt, "")
	if crErr != nil {
		return r.failStep(spanCtx, run, agenticv1alpha1.AgenticRunConditionAnalyzed, fmt.Errorf("%s: %w", ErrCreateAnalysisResult, crErr))
	}
	if r.Audit != nil {
		r.Audit.EmitAnalysisCompleted(spanCtx, run, analysisCR)
	}
	run.Status.Steps.Analysis.Results = append(run.Status.Steps.Analysis.Results, agenticv1alpha1.StepResultRef{Name: crName, Outcome: agenticv1alpha1.ActionOutcomeFromBool(analysisResult.Success)})

	if !analysisResult.IsActionRequired() {
		return r.setNoActionRequired(ctx, run, base, ErrUpdateAfterAnalysis, "Analysis determined no remediation action is required")
	}

	meta.SetStatusCondition(&run.Status.Conditions, metav1.Condition{
		Type:               agenticv1alpha1.AgenticRunConditionAnalyzed,
		Status:             metav1.ConditionTrue,
		Reason:             reasonComplete,
		Message:            fmt.Sprintf("Analysis complete with %d option(s)", len(analysisResult.Options)),
		ObservedGeneration: run.Generation,
	})
	if err := r.statusPatch(ctx, run, base); err != nil {
		return ctrl.Result{}, fmt.Errorf("%s: %w", ErrUpdateAfterAnalysis, err)
	}

	log.Info("analysis complete", "options", len(analysisResult.Options))
	return ctrl.Result{}, nil
}

// handleRevision re-runs analysis with revision context appended to the
// agent's system prompt.
func (r *AgenticRunReconciler) handleRevision(
	ctx context.Context,
	run *agenticv1alpha1.AgenticRun,
	resolved *resolvedWorkflow,
	approval *agenticv1alpha1.AgenticRunApproval,
	policy *agenticv1alpha1.ApprovalPolicy,
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

	spanCtx := ctx
	var span trace.Span
	if r.Audit != nil {
		spanCtx, span = r.Audit.StartAnalysisSpan(ctx, run)
		if span != nil {
			defer span.End()
		}
	}

	revisionSuffix := buildRevisionContext(run)
	requestWithRevision := run.Spec.Request + "\n\n" + revisionSuffix

	analysisResult, err := r.Agent.Analyze(spanCtx, run, resolved.Analysis, requestWithRevision, defaultSandboxSA)
	if err != nil {
		return r.failStep(spanCtx, run, agenticv1alpha1.AgenticRunConditionAnalyzed, err)
	}
	if !analysisResult.Success {
		return r.failStep(spanCtx, run, agenticv1alpha1.AgenticRunConditionAnalyzed, fmt.Errorf("analysis agent reported failure"))
	}

	base = run.DeepCopy()
	completedAt := metav1.Now()
	startTime := conditionTime(run.Status.Conditions, agenticv1alpha1.AgenticRunConditionAnalyzed)
	crName, analysisCR, crErr := r.createAnalysisResult(spanCtx, run, analysisResult, run.Status.Steps.Analysis.Sandbox, startTime, &completedAt, "")
	if crErr != nil {
		return r.failStep(spanCtx, run, agenticv1alpha1.AgenticRunConditionAnalyzed, fmt.Errorf("%s: %w", ErrCreateAnalysisResult, crErr))
	}
	if r.Audit != nil {
		r.Audit.EmitAnalysisCompleted(spanCtx, run, analysisCR)
	}
	run.Status.Steps.Analysis.Results = append(run.Status.Steps.Analysis.Results, agenticv1alpha1.StepResultRef{Name: crName, Outcome: agenticv1alpha1.ActionOutcomeFromBool(analysisResult.Success)})

	if !analysisResult.IsActionRequired() {
		return r.setNoActionRequired(ctx, run, base, ErrUpdateAfterRevision, "Revision analysis determined no remediation action is required")
	}

	meta.SetStatusCondition(&run.Status.Conditions, metav1.Condition{
		Type:               agenticv1alpha1.AgenticRunConditionAnalyzed,
		Status:             metav1.ConditionTrue,
		Reason:             reasonRevisionComplete,
		Message:            fmt.Sprintf("Revision complete (generation %d) with %d option(s)", generation, len(analysisResult.Options)),
		ObservedGeneration: generation,
	})
	if err := r.statusPatch(ctx, run, base); err != nil {
		return ctrl.Result{}, fmt.Errorf("%s: %w", ErrUpdateAfterRevision, err)
	}

	log.Info("revision analysis complete", "generation", generation, "options", len(analysisResult.Options))
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

	selectedOption, trimErr := r.trimNonSelectedOptions(ctx, run, approval, policy)
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

	// Determine which SA the execution pod should run as.
	execSA := defaultSandboxSA
	base := run.DeepCopy()
	if selectedOption != nil && (len(selectedOption.RBAC.NamespaceScoped) > 0 || len(selectedOption.RBAC.ClusterScoped) > 0) {
		if err := ensureExecutionRBAC(ctx, r.Client, run, &selectedOption.RBAC, r.Namespace); err != nil {
			return r.failStep(ctx, run, agenticv1alpha1.AgenticRunConditionExecuted, fmt.Errorf("%s: %w", ErrEnsureExecutionRBAC, err))
		}
		if err := r.Patch(ctx, run, client.MergeFrom(base)); err != nil {
			return ctrl.Result{}, fmt.Errorf("%s: %w", ErrPersistRBACAnnotation, err)
		}
		base = run.DeepCopy()
		execSA = executionSAName(run)
	}

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

	spanCtx := ctx
	var span trace.Span
	if r.Audit != nil {
		spanCtx, span = r.Audit.StartExecutionSpan(ctx, run)
		if span != nil {
			defer span.End()
		}
	}

	execResult, err := r.Agent.Execute(spanCtx, run, *resolved.Execution, selectedOption, execSA)
	if err != nil {
		return r.failStep(spanCtx, run, agenticv1alpha1.AgenticRunConditionExecuted, err)
	}
	if !execResult.Success {
		return r.failStep(spanCtx, run, agenticv1alpha1.AgenticRunConditionExecuted, fmt.Errorf("execution agent reported failure"))
	}

	base = run.DeepCopy()
	completedAt := metav1.Now()
	startTime := conditionTime(run.Status.Conditions, agenticv1alpha1.AgenticRunConditionExecuted)
	execCRName, execCR, execCRErr := r.createExecutionResult(spanCtx, run, execResult, run.Status.Steps.Execution.Sandbox, startTime, &completedAt, "")
	if execCRErr != nil {
		return r.failStep(spanCtx, run, agenticv1alpha1.AgenticRunConditionExecuted, fmt.Errorf("%s: %w", ErrCreateExecutionResult, execCRErr))
	}
	if r.Audit != nil {
		r.Audit.EmitExecutionCompleted(spanCtx, run, execCR)
	}
	run.Status.Steps.Execution.Results = append(run.Status.Steps.Execution.Results, agenticv1alpha1.StepResultRef{Name: execCRName, Outcome: agenticv1alpha1.ActionOutcomeFromBool(execResult.Success)})
	meta.SetStatusCondition(&run.Status.Conditions, metav1.Condition{
		Type:               agenticv1alpha1.AgenticRunConditionExecuted,
		Status:             metav1.ConditionTrue,
		Reason:             reasonComplete,
		Message:            "Execution completed",
		ObservedGeneration: run.Generation,
	})

	if resolved.Verification == nil {
		setVerificationSkipped(run)
		if err := r.statusPatch(ctx, run, base); err != nil {
			return ctrl.Result{}, fmt.Errorf("%s: %w", ErrUpdateToCompletedTrust, err)
		}
		log.Info("execution complete, verification skipped")
	} else {
		if err := r.statusPatch(ctx, run, base); err != nil {
			return ctrl.Result{}, fmt.Errorf("%s: %w", ErrUpdateToVerifying, err)
		}
		log.Info("execution complete, verifying")
	}

	// Clean up per-run execution SA + Roles if one was created.
	if execSA != defaultSandboxSA {
		if err := cleanupExecutionRBAC(ctx, r.Client, run, r.Namespace); err != nil {
			log.Error(err, "RBAC cleanup after execution, retrying")
			return ctrl.Result{Requeue: true}, nil
		}
	}

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
	// Retry execution RBAC cleanup if it failed during handleExecution transition.
	// Always attempt — idempotent; covers both namespace-scoped and cluster-scoped-only cases.
	if err := cleanupExecutionRBAC(ctx, r.Client, run, r.Namespace); err != nil {
		log.Error(err, "RBAC cleanup retry in verification, requeuing")
		return ctrl.Result{Requeue: true}, nil
	}

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
		if err := r.Get(ctx, types.NamespacedName{Name: latestRef.Name, Namespace: run.Namespace}, &execCR); err == nil {
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
		spanCtx, span = r.Audit.StartVerificationSpan(ctx, run)
		if span != nil {
			defer span.End()
		}
	}

	verifyResult, err := r.Agent.Verify(spanCtx, run, *resolved.Verification, selectedOption, execOutput, defaultSandboxSA)
	if err != nil {
		return r.failStep(spanCtx, run, agenticv1alpha1.AgenticRunConditionVerified, err)
	}

	base = run.DeepCopy()
	completedAt := metav1.Now()
	startTime := conditionTime(run.Status.Conditions, agenticv1alpha1.AgenticRunConditionVerified)
	verifyCRName, verifyCR, verifyCRErr := r.createVerificationResult(spanCtx, run, verifyResult, run.Status.Steps.Verification.Sandbox, startTime, &completedAt, "")
	if verifyCRErr != nil {
		return r.failStep(spanCtx, run, agenticv1alpha1.AgenticRunConditionVerified, fmt.Errorf("%s: %w", ErrCreateVerificationResult, verifyCRErr))
	}
	run.Status.Steps.Verification.Results = append(run.Status.Steps.Verification.Results, agenticv1alpha1.StepResultRef{Name: verifyCRName, Outcome: agenticv1alpha1.ActionOutcomeFromBool(verifyResult.Success)})

	allPassed := verifyResult.Success
	for _, check := range verifyResult.Checks {
		if check.Result != agenticv1alpha1.CheckResultPassed {
			allPassed = false
			break
		}
	}

	if !allPassed {
		retryCount := int32(0)
		if run.Status.Steps.Execution.RetryCount != nil {
			retryCount = *run.Status.Steps.Execution.RetryCount
		}
		maxRetries := maxAttempts(approval, policy)

		if int(retryCount) < maxRetries-1 {
			next := retryCount + 1
			log.Info("verification failed, retrying execution", "attempt", next+1, "maxAttempts", maxRetries, LogKeySummary, verifyResult.Summary)
			if r.Audit != nil {
				r.Audit.EmitVerificationRetry(spanCtx, run, verifyCR, int(next))
			}
			run.Status.Steps.Execution.RetryCount = &next
			resetExecutionAndVerification(&run.Status.Steps)
			meta.RemoveStatusCondition(&run.Status.Conditions, agenticv1alpha1.AgenticRunConditionExecuted)
			meta.SetStatusCondition(&run.Status.Conditions, metav1.Condition{
				Type:               agenticv1alpha1.AgenticRunConditionVerified,
				Status:             metav1.ConditionFalse,
				Reason:             reasonRetryingExecution,
				Message:            fmt.Sprintf("Verification failed (attempt %d/%d): %s", next+1, maxRetries, verifyResult.Summary),
				ObservedGeneration: run.Generation,
			})
			if err := r.statusPatch(spanCtx, run, base); err != nil {
				return ctrl.Result{}, fmt.Errorf("%s: %w", ErrUpdateForExecRetry, err)
			}
			return ctrl.Result{}, nil
		}

		log.Info("verification retries exhausted, escalating", "retryCount", retryCount, LogKeySummary, verifyResult.Summary)
		if r.Audit != nil {
			r.Audit.EmitVerificationCompleted(spanCtx, run, verifyCR)
		}
		meta.SetStatusCondition(&run.Status.Conditions, metav1.Condition{
			Type:               agenticv1alpha1.AgenticRunConditionVerified,
			Status:             metav1.ConditionFalse,
			Reason:             reasonRetriesExhausted,
			Message:            fmt.Sprintf("Verification failed after %d attempt(s): %s", retryCount+1, verifyResult.Summary),
			ObservedGeneration: run.Generation,
		})
		meta.SetStatusCondition(&run.Status.Conditions, metav1.Condition{
			Type:               agenticv1alpha1.AgenticRunConditionEscalated,
			Status:             metav1.ConditionUnknown,
			Reason:             reasonRetriesExhausted,
			Message:            fmt.Sprintf("Verification failed after %d attempt(s), escalating", retryCount+1),
			ObservedGeneration: run.Generation,
		})
		if err := r.statusPatch(ctx, run, base); err != nil {
			return ctrl.Result{}, fmt.Errorf("%s: %w", ErrUpdateRetriesExhausted, err)
		}
		return ctrl.Result{}, nil
	}

	if r.Audit != nil {
		r.Audit.EmitVerificationCompleted(spanCtx, run, verifyCR)
	}

	meta.SetStatusCondition(&run.Status.Conditions, metav1.Condition{
		Type:               agenticv1alpha1.AgenticRunConditionVerified,
		Status:             metav1.ConditionTrue,
		Reason:             reasonPassed,
		Message:            verifyResult.Summary,
		ObservedGeneration: run.Generation,
	})
	if err := r.statusPatch(spanCtx, run, base); err != nil {
		return ctrl.Result{}, fmt.Errorf("%s: %w", ErrUpdateToCompleted, err)
	}

	log.Info("verification passed", LogKeySummary, verifyResult.Summary)
	return ctrl.Result{}, nil
}

// handleFailed performs cleanup for system failures.
func (r *AgenticRunReconciler) handleFailed(
	ctx context.Context,
	run *agenticv1alpha1.AgenticRun,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	log.Info("handling system failure (terminal)")

	if r.Audit != nil {
		r.Audit.EmitTerminalSpan(ctx, run, string(agenticv1alpha1.AgenticRunPhaseFailed), terminalReason(run))
		r.Audit.Cleanup(run)
	}

	if run.Annotations[rbacNamespacesAnnotation] != "" {
		if err := cleanupExecutionRBAC(ctx, r.Client, run, r.Namespace); err != nil {
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

	if run.Annotations[rbacNamespacesAnnotation] != "" {
		if err := cleanupExecutionRBAC(ctx, r.Client, run, r.Namespace); err != nil {
			log.Error(err, "best-effort RBAC cleanup during suspension")
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

	spanCtx := ctx
	var span trace.Span
	if r.Audit != nil {
		spanCtx, span = r.Audit.StartEscalationSpan(ctx, run)
		if span != nil {
			defer span.End()
		}
	}

	escalationText := buildEscalationRequest(run)
	escalationResult, err := r.Agent.Escalate(spanCtx, run, step, escalationText, defaultSandboxSA)
	if err != nil {
		return r.failStep(spanCtx, run, agenticv1alpha1.AgenticRunConditionEscalated, err)
	}

	base = run.DeepCopy()
	completedAt := metav1.Now()
	startTime := conditionTime(run.Status.Conditions, agenticv1alpha1.AgenticRunConditionEscalated)
	crName, escalationCR, crErr := r.createEscalationResult(spanCtx, run, escalationResult, run.Status.Steps.Escalation.Sandbox, startTime, &completedAt, "")
	if crErr != nil {
		return r.failStep(spanCtx, run, agenticv1alpha1.AgenticRunConditionEscalated, fmt.Errorf("%s: %w", ErrCreateEscalationResult, crErr))
	}
	if r.Audit != nil {
		r.Audit.EmitEscalationCompleted(spanCtx, run, escalationCR)
	}
	run.Status.Steps.Escalation.Results = append(run.Status.Steps.Escalation.Results, agenticv1alpha1.StepResultRef{Name: crName, Outcome: agenticv1alpha1.ActionOutcomeFromBool(escalationResult.Success)})

	if run.Annotations[rbacNamespacesAnnotation] != "" {
		if cleanErr := cleanupExecutionRBAC(ctx, r.Client, run, r.Namespace); cleanErr != nil {
			log.Error(cleanErr, "RBAC cleanup on escalation")
		}
	}

	meta.SetStatusCondition(&run.Status.Conditions, metav1.Condition{
		Type:               agenticv1alpha1.AgenticRunConditionEscalated,
		Status:             metav1.ConditionTrue,
		Reason:             reasonComplete,
		Message:            escalationResult.Summary,
		ObservedGeneration: run.Generation,
	})
	if err := r.statusPatch(ctx, run, base); err != nil {
		return ctrl.Result{}, fmt.Errorf("%s: %w", ErrUpdateToEscalated, err)
	}

	log.Info("escalation complete", LogKeySummary, escalationResult.Summary)
	return ctrl.Result{}, nil
}

func (r *AgenticRunReconciler) setNoActionRequired(ctx context.Context, run *agenticv1alpha1.AgenticRun, base *agenticv1alpha1.AgenticRun, errSentinel, message string) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	meta.SetStatusCondition(&run.Status.Conditions, metav1.Condition{
		Type:               agenticv1alpha1.AgenticRunConditionAnalyzed,
		Status:             metav1.ConditionTrue,
		Reason:             reasonNoActionRequired,
		Message:            message,
		ObservedGeneration: run.Generation,
	})
	if err := r.statusPatch(ctx, run, base); err != nil {
		return ctrl.Result{}, fmt.Errorf("%s: %w", errSentinel, err)
	}
	log.Info("no action required", "message", message)
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
