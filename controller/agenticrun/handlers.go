package agenticrun

import (
	"context"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
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
	ErrUpdateToAnalyzingRevision = "update to Analyzing (revision)"
	ErrClearTerminalLabel        = "clear terminal TTL label"
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
		if isStopAcceptedError(err) {
			return ctrl.Result{Requeue: true}, nil
		}
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

	if run.Labels[terminalTTLLabel] != "" {
		base := run.DeepCopy()
		delete(run.Labels, terminalTTLLabel)
		if err := r.Patch(ctx, run, client.MergeFrom(base)); err != nil {
			return ctrl.Result{}, fmt.Errorf("%s: %w", ErrClearTerminalLabel, err)
		}
	}

	base := run.DeepCopy()
	meta.RemoveStatusCondition(&run.Status.Conditions, agenticv1alpha1.AgenticRunConditionExecuted)
	meta.RemoveStatusCondition(&run.Status.Conditions, agenticv1alpha1.AgenticRunConditionVerified)
	meta.RemoveStatusCondition(&run.Status.Conditions, agenticv1alpha1.AgenticRunConditionEscalated)
	resetExecutionAndVerification(&run.Status.Steps)
	// A later terminal transition receives a fresh deletion deadline.
	run.Status.TerminalTime = nil
	run.Status.DeleteAfter = nil
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
		if isStopAcceptedError(err) {
			return ctrl.Result{Requeue: true}, nil
		}
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
		if isStopAcceptedError(err) {
			return ctrl.Result{Requeue: true}, nil
		}
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
		if isStopAcceptedError(err) {
			return ctrl.Result{Requeue: true}, nil
		}
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

	if preserveFailedSandbox(run) {
		log.Info("preserving failed sandbox for debugging")
		return ctrl.Result{}, nil
	}

	spoke, spokeErr := spokeAccessForRun(ctx, r.Client, run, r.Namespace)
	if spokeErr != nil {
		log.Error(spokeErr, "RBAC cleanup: spoke unreachable")
	}

	// Clean SAs, reader CRBs, and execution RBAC for all steps.
	// Spoke: per-run CRBs + SAs on spoke. Hub: per-run CRBs + execution RBAC.
	for _, step := range []string{"analysis", "execution", "verification", "escalation"} {
		// Hub path: only clean execution RBAC if annotation is present (SAs are GC'd via owner refs).
		// Spoke path: always clean all steps.
		if spoke == nil && step != "execution" {
			continue
		}
		if spoke == nil && run.Annotations[rbacNamespacesAnnotation] == "" {
			continue
		}
		if err := cleanupStepRBAC(ctx, spoke, r.Client, r.Namespace, run, step); err != nil {
			log.Error(err, "RBAC cleanup on failure", LogKeyStep, step)
		}
	}

	return ctrl.Result{}, nil
}

func (r *AgenticRunReconciler) handleSuspensionAt(
	ctx context.Context,
	run *agenticv1alpha1.AgenticRun,
	activationStart metav1.Time,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	if r.APIReader == nil {
		return ctrl.Result{}, fmt.Errorf("uncached API reader is required for suspension cleanup")
	}
	if r.Sandbox == nil {
		return ctrl.Result{}, fmt.Errorf("sandbox manager is required for suspension cleanup")
	}

	key := client.ObjectKeyFromObject(run)
	current := &agenticv1alpha1.AgenticRun{}
	if err := r.APIReader.Get(ctx, key, current); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if current.UID != run.UID {
		return ctrl.Result{}, nil
	}
	if current.UID == "" {
		return ctrl.Result{}, fmt.Errorf("run UID is required for suspension cleanup")
	}
	*run = *current

	phase := agenticv1alpha1.DerivePhase(current.Status.Conditions)
	historicalTerminal := isTerminal(phase) && !hasAcceptedStop(current)
	stopStart := activationStart
	stopping := meta.FindStatusCondition(current.Status.Conditions, agenticv1alpha1.AgenticRunConditionStopping)
	if stopping != nil && stopping.Status == metav1.ConditionTrue && stopping.Reason == reasonSystemSuspended &&
		!stopping.LastTransitionTime.IsZero() {
		stopStart = stopping.LastTransitionTime
	}

	remaining := time.Duration(0)
	if !stopStart.IsZero() {
		remaining = stopStart.Add(SuspensionStopWindow).Sub(r.now())
	}
	if !historicalTerminal && !hasAcceptedStop(current) {
		if stopStart.IsZero() || remaining <= 0 {
			return ctrl.Result{RequeueAfter: suspensionRequeueAfter}, nil
		}
		acceptanceCtx, cancelAcceptance := context.WithTimeout(ctx, remaining)
		updated, err := r.persistRunStop(acceptanceCtx, key, run.UID, stopStart)
		cancelAcceptance()
		if err != nil {
			return ctrl.Result{}, err
		}
		if updated == nil {
			return ctrl.Result{}, nil
		}
		current = updated
		*run = *current
		phase = agenticv1alpha1.DerivePhase(current.Status.Conditions)
		historicalTerminal = isTerminal(phase) && !hasAcceptedStop(current)
	}

	finalOutcome := hasFinalRunStopOutcome(current)
	if !stopStart.IsZero() && !finalOutcome {
		remaining = stopStart.Add(SuspensionStopWindow).Sub(r.now())
	}
	if !historicalTerminal && !finalOutcome && !stopStart.IsZero() && remaining <= 0 {
		updated, err := r.writeSuspensionOutcome(ctx, key, run.UID, false)
		if err != nil {
			return ctrl.Result{}, err
		}
		if updated == nil {
			return ctrl.Result{}, nil
		}
		current = updated
		*run = *current

		report, cleanupErr, remoteEvidence := r.suspensionCleanupPass(ctx, current, cleanupTimeout)
		if cleanupErr != nil {
			log.Error(cleanupErr, "background cleanup after suspension deadline failed", LogKeyName, current.Name)
		}
		if cleanupErr == nil && report.Confirmed() && remoteEvidence == "" {
			cleaned, err := r.clearRunSandboxInfo(ctx, key, run.UID)
			if err != nil {
				return ctrl.Result{}, err
			}
			if cleaned != nil {
				*run = *cleaned
			}
		}
		return ctrl.Result{RequeueAfter: suspensionRequeueAfter}, nil
	}

	passTimeout := cleanupTimeout
	if remaining > 0 && !finalOutcome {
		passTimeout = remaining
	}
	report, cleanupErr, remoteEvidence := r.suspensionCleanupPass(ctx, current, passTimeout)
	if cleanupErr != nil {
		log.Error(cleanupErr, "suspension cleanup pass failed", LogKeyName, current.Name)
	}
	confirmed := cleanupErr == nil && report.Confirmed() && remoteEvidence == ""
	current = &agenticv1alpha1.AgenticRun{}
	if err := r.APIReader.Get(ctx, key, current); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if current.UID != run.UID {
		return ctrl.Result{}, nil
	}
	*run = *current
	phase = agenticv1alpha1.DerivePhase(current.Status.Conditions)
	historicalTerminal = isTerminal(phase) && !hasAcceptedStop(current)
	finalOutcome = hasFinalRunStopOutcome(current)

	if confirmed {
		if !historicalTerminal && !finalOutcome {
			updated, err := r.writeSuspensionOutcome(ctx, key, run.UID, true)
			if err != nil {
				return ctrl.Result{}, err
			}
			if updated != nil {
				*run = *updated
			}
		} else {
			updated, err := r.clearRunSandboxInfo(ctx, key, run.UID)
			if err != nil {
				return ctrl.Result{}, err
			}
			if updated != nil {
				*run = *updated
			}
		}
		return ctrl.Result{}, nil
	}

	deadlineReached := !stopStart.IsZero() && !r.now().Before(stopStart.Add(SuspensionStopWindow))
	if deadlineReached && !historicalTerminal && !finalOutcome {
		updated, err := r.writeSuspensionOutcome(ctx, key, run.UID, false)
		if err != nil {
			return ctrl.Result{}, err
		}
		if updated != nil {
			*run = *updated
		}
		return ctrl.Result{RequeueAfter: suspensionRequeueAfter}, nil
	}

	requeueAfter := suspensionRequeueAfter
	if !stopStart.IsZero() {
		if remaining := stopStart.Add(SuspensionStopWindow).Sub(r.now()); remaining > 0 && remaining < requeueAfter {
			requeueAfter = remaining
		}
	}
	return ctrl.Result{RequeueAfter: requeueAfter}, nil
}

func (r *AgenticRunReconciler) suspensionCleanupPass(
	ctx context.Context,
	run *agenticv1alpha1.AgenticRun,
	timeout time.Duration,
) (SuspensionCleanup, error, string) {
	passCtx, cancel := context.WithTimeout(ctx, timeout)
	report, cleanupErr := r.Sandbox.CleanupSuspension(passCtx, run.UID)
	remoteEvidence := knownRemoteExecutionOrAccess(run)
	if remoteEvidence != "" && r.Agent != nil {
		if err := r.Agent.ReleaseSandboxes(passCtx, run, true); err != nil {
			logf.FromContext(ctx).Error(err, "best-effort remote sandbox release during suspension", "evidence", remoteEvidence)
		}
	}
	cancel()
	return report, cleanupErr, remoteEvidence
}

func (r *AgenticRunReconciler) persistRunStop(
	ctx context.Context,
	key types.NamespacedName,
	uid types.UID,
	start metav1.Time,
) (*agenticv1alpha1.AgenticRun, error) {
	for range 5 {
		current := &agenticv1alpha1.AgenticRun{}
		if err := r.APIReader.Get(ctx, key, current); err != nil {
			return nil, client.IgnoreNotFound(err)
		}
		if current.UID != uid {
			return nil, nil
		}
		if isTerminal(agenticv1alpha1.DerivePhase(current.Status.Conditions)) || hasAcceptedStop(current) {
			return current, nil
		}

		base := current.DeepCopy()
		meta.SetStatusCondition(&current.Status.Conditions, metav1.Condition{
			Type:               agenticv1alpha1.AgenticRunConditionStopping,
			Status:             metav1.ConditionTrue,
			Reason:             reasonSystemSuspended,
			Message:            "Stop accepted by global system suspension",
			ObservedGeneration: current.Generation,
		})
		meta.FindStatusCondition(current.Status.Conditions, agenticv1alpha1.AgenticRunConditionStopping).LastTransitionTime = start
		err := r.Status().Patch(ctx, current, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
		if apierrors.IsConflict(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("persist run stop acceptance: %w", err)
		}
		return current, nil
	}
	return nil, fmt.Errorf("persist run stop acceptance: status conflict retry limit reached")
}

func (r *AgenticRunReconciler) writeSuspensionOutcome(
	ctx context.Context,
	key types.NamespacedName,
	uid types.UID,
	confirmed bool,
) (*agenticv1alpha1.AgenticRun, error) {
	for range 5 {
		current := &agenticv1alpha1.AgenticRun{}
		if err := r.APIReader.Get(ctx, key, current); err != nil {
			return nil, client.IgnoreNotFound(err)
		}
		if current.UID != uid {
			return nil, nil
		}
		if hasFinalRunStopOutcome(current) {
			return current, nil
		}
		phase := agenticv1alpha1.DerivePhase(current.Status.Conditions)
		stopping := meta.FindStatusCondition(current.Status.Conditions, agenticv1alpha1.AgenticRunConditionStopping)
		terminalStopIntent := isTerminal(phase) && stopping != nil &&
			stopping.Status == metav1.ConditionTrue && stopping.Reason == reasonSystemSuspended
		if isTerminal(phase) && !terminalStopIntent {
			return current, nil
		}
		if !hasAcceptedStop(current) {
			return current, nil
		}

		base := current.DeepCopy()
		if confirmed {
			meta.SetStatusCondition(&current.Status.Conditions, metav1.Condition{
				Type:               agenticv1alpha1.AgenticRunConditionStopping,
				Status:             metav1.ConditionFalse,
				Reason:             reasonTeardownComplete,
				Message:            "Sandbox workload and access cleanup confirmed",
				ObservedGeneration: current.Generation,
			})
			if !terminalStopIntent {
				meta.SetStatusCondition(&current.Status.Conditions, metav1.Condition{
					Type:               agenticv1alpha1.AgenticRunConditionEmergencyStopped,
					Status:             metav1.ConditionTrue,
					Reason:             reasonSystemSuspended,
					Message:            "Terminated by system kill switch (AgenticOLSConfig.spec.suspended=true)",
					ObservedGeneration: current.Generation,
				})
			}
			clearRunSandboxInfo(&current.Status)
		} else {
			meta.SetStatusCondition(&current.Status.Conditions, metav1.Condition{
				Type:               agenticv1alpha1.AgenticRunConditionStopping,
				Status:             metav1.ConditionFalse,
				Reason:             reasonTeardownFailed,
				Message:            "Sandbox cleanup remains unconfirmed after the suspension window",
				ObservedGeneration: current.Generation,
			})
			if !terminalStopIntent {
				if condType := suspensionFailureCondition(phase); condType != "" {
					meta.SetStatusCondition(&current.Status.Conditions, metav1.Condition{
						Type:               condType,
						Status:             metav1.ConditionFalse,
						Reason:             reasonEmergencyStopFailed,
						Message:            "Sandbox cleanup could not be confirmed; see suspension diagnostics",
						ObservedGeneration: current.Generation,
					})
				}
			}
		}

		err := r.Status().Patch(ctx, current, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
		if apierrors.IsConflict(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("persist run suspension outcome: %w", err)
		}
		return current, nil
	}
	return nil, fmt.Errorf("persist run suspension outcome: status conflict retry limit reached")
}

func (r *AgenticRunReconciler) clearRunSandboxInfo(
	ctx context.Context,
	key types.NamespacedName,
	uid types.UID,
) (*agenticv1alpha1.AgenticRun, error) {
	for range 5 {
		current := &agenticv1alpha1.AgenticRun{}
		if err := r.APIReader.Get(ctx, key, current); err != nil {
			return nil, client.IgnoreNotFound(err)
		}
		if current.UID != uid {
			return nil, nil
		}
		if !hasRunSandboxInfo(current) {
			return current, nil
		}

		base := current.DeepCopy()
		clearRunSandboxInfo(&current.Status)
		err := r.Status().Patch(ctx, current, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
		if apierrors.IsConflict(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("clear confirmed sandbox references: %w", err)
		}
		return current, nil
	}
	return nil, fmt.Errorf("clear confirmed sandbox references: status conflict retry limit reached")
}

func clearRunSandboxInfo(status *agenticv1alpha1.AgenticRunStatus) {
	status.Steps.Analysis.Sandbox = agenticv1alpha1.SandboxInfo{}
	status.Steps.Execution.Sandbox = agenticv1alpha1.SandboxInfo{}
	status.Steps.Verification.Sandbox = agenticv1alpha1.SandboxInfo{}
	status.Steps.Escalation.Sandbox = agenticv1alpha1.SandboxInfo{}
}

func hasRunSandboxInfo(run *agenticv1alpha1.AgenticRun) bool {
	return run.Status.Steps.Analysis.Sandbox.ClaimName != "" || run.Status.Steps.Analysis.Sandbox.Namespace != "" ||
		run.Status.Steps.Execution.Sandbox.ClaimName != "" || run.Status.Steps.Execution.Sandbox.Namespace != "" ||
		run.Status.Steps.Verification.Sandbox.ClaimName != "" || run.Status.Steps.Verification.Sandbox.Namespace != "" ||
		run.Status.Steps.Escalation.Sandbox.ClaimName != "" || run.Status.Steps.Escalation.Sandbox.Namespace != ""
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

func knownRemoteExecutionOrAccess(run *agenticv1alpha1.AgenticRun) string {
	if run.Spec.TargetCluster == "" {
		return ""
	}
	var evidence string
	for _, step := range []struct {
		name string
		info agenticv1alpha1.SandboxInfo
	}{
		{name: "analysis", info: run.Status.Steps.Analysis.Sandbox},
		{name: "execution", info: run.Status.Steps.Execution.Sandbox},
		{name: "verification", info: run.Status.Steps.Verification.Sandbox},
		{name: "escalation", info: run.Status.Steps.Escalation.Sandbox},
	} {
		if step.info.ClaimName == "" {
			continue
		}
		if evidence != "" {
			evidence += ", "
		}
		evidence += step.name + " sandbox " + step.info.ClaimName
	}
	if run.Annotations[rbacNamespacesAnnotation] != "" {
		if evidence != "" {
			evidence += ", "
		}
		evidence += "remote execution RBAC annotation"
	}
	if evidence == "" {
		for _, condition := range run.Status.Conditions {
			if condition.Status == metav1.ConditionUnknown && condition.Type != agenticv1alpha1.AgenticRunConditionStopping {
				evidence = "active workflow condition " + condition.Type
				break
			}
		}
	}
	if evidence == "" {
		return ""
	}
	return fmt.Sprintf("remote execution/access unverified for %s/%s uid=%s target=%s: %s",
		run.Namespace, run.Name, run.UID, run.Spec.TargetCluster, evidence)
}

func suspensionFailureCondition(phase agenticv1alpha1.AgenticRunPhase) string {
	switch phase {
	case agenticv1alpha1.AgenticRunPhasePending, agenticv1alpha1.AgenticRunPhaseAnalyzing:
		return agenticv1alpha1.AgenticRunConditionAnalyzed
	case agenticv1alpha1.AgenticRunPhaseProposed, agenticv1alpha1.AgenticRunPhaseExecuting:
		return agenticv1alpha1.AgenticRunConditionExecuted
	case agenticv1alpha1.AgenticRunPhaseVerifying:
		return agenticv1alpha1.AgenticRunConditionVerified
	case agenticv1alpha1.AgenticRunPhaseEscalating:
		return agenticv1alpha1.AgenticRunConditionEscalated
	default:
		return ""
	}
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
		if escalated.Status == metav1.ConditionUnknown &&
			(escalated.Reason == reasonInProgress || escalated.Reason == ReasonRunning || escalated.Reason == ReasonWaitingForSandbox) {
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
		if isStopAcceptedError(err) {
			return ctrl.Result{Requeue: true}, nil
		}
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
