package agenticrun

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
)

const msgSandboxNoResult = "sandbox exited without creating result"

// ---------------------------------------------------------------------------
// Pod event handler
// ---------------------------------------------------------------------------

// handlePodEvent is the Watches handler for sandbox pods. It resolves
// the owning AgenticRun using either labels (bare-pod) or the ownership
// chain Pod → Sandbox → SandboxClaim (sandbox-claim mode).
func (r *AgenticRunReconciler) handlePodEvent(ctx context.Context, obj client.Object) []ctrl.Request {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		return nil
	}

	var step, runName string
	var runUID types.UID
	var metadataErr error
	if r.isSandboxClaimMode() {
		step, runName, runUID, metadataErr = resolveSandboxPodMetadata(ctx, r.Client, pod)
	} else {
		step, runName, runUID = resolveBarePodMetadata(pod)
	}
	if metadataErr != nil {
		logf.FromContext(ctx).Error(metadataErr, "failed to resolve sandbox Pod identity", LogKeyName, pod.Name)
		return nil
	}
	if step == "" || runUID == "" {
		return nil
	}

	run, err := r.runForSandboxIdentity(ctx, runName, runUID)
	if err != nil {
		logf.FromContext(ctx).Error(err, "failed to resolve run for sandbox Pod", LogKeyName, pod.Name)
		return nil
	}
	if run == nil {
		return nil
	}
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(run)}
	current, guardErr := readRunnableRun(ctx, r.APIReader, client.ObjectKeyFromObject(run), run.UID)
	if guardErr == errStopAccepted {
		return []ctrl.Request{request}
	}
	if guardErr != nil {
		logf.FromContext(ctx).Error(guardErr, "failed to verify run before processing sandbox Pod", LogKeyName, pod.Name)
		return nil
	}
	run = current
	if isTerminal(agenticv1alpha1.DerivePhase(run.Status.Conditions)) {
		return nil
	}

	condType := stepConditionType(step)
	claimName := sandboxClaimName(run, step)
	ctx = logf.IntoContext(ctx, logf.FromContext(ctx).WithValues(
		LogKeyName, pod.Name, LogKeyStep, step,
		LogKeyClaim, claimName, "runUID", string(run.UID), LogKeyCondition, condType,
	))

	if pod.Status.Phase == corev1.PodRunning || pod.Status.Phase == corev1.PodPending || pod.Status.Phase == corev1.PodUnknown {
		reason := ReasonRunning
		if pod.Status.Phase != corev1.PodRunning {
			reason = ReasonWaitingForSandbox
		}
		if err := r.patchStepCondition(ctx, run, condType, metav1.ConditionUnknown, reason, "Sandbox pod "+reason); isStopAcceptedError(err) {
			return []ctrl.Request{request}
		}
		return nil
	}

	if err := r.completeStep(ctx, run, pod, step, condType, "", ""); isStopAcceptedError(err) {
		return []ctrl.Request{request}
	}
	return nil
}

func (r *AgenticRunReconciler) runForSandboxIdentity(ctx context.Context, runName string, runUID types.UID) (*agenticv1alpha1.AgenticRun, error) {
	if r.APIReader == nil {
		return nil, fmt.Errorf("uncached API reader is required to resolve sandbox Pod ownership")
	}
	if runUID == "" {
		return nil, nil
	}
	if runName != "" {
		run := &agenticv1alpha1.AgenticRun{}
		err := r.APIReader.Get(ctx, client.ObjectKey{Name: runName, Namespace: r.Namespace}, run)
		if err == nil && run.UID == runUID {
			return run, nil
		}
		if err != nil && client.IgnoreNotFound(err) != nil {
			return nil, err
		}
	}

	var runs agenticv1alpha1.AgenticRunList
	if err := r.APIReader.List(ctx, &runs); err != nil {
		return nil, err
	}
	for i := range runs.Items {
		run := &runs.Items[i]
		if run.UID == runUID && (runName == "" || run.Name == runName) {
			return run, nil
		}
	}
	return nil, nil
}

func (r *AgenticRunReconciler) reconcileAcceptedStop(ctx context.Context, key types.NamespacedName, err error) {
	if !isStopAcceptedError(err) {
		return
	}
	if _, reconcileErr := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); reconcileErr != nil {
		logf.FromContext(ctx).Error(reconcileErr, "failed to reconcile accepted stop", LogKeyName, key.Name)
	}
}

// resolveBarePodMetadata uses the run UID as identity and the name annotation
// only as a lookup hint, with an AgenticRun owner reference for legacy Pods.
func resolveBarePodMetadata(pod *corev1.Pod) (step, runName string, runUID types.UID) {
	step = pod.Labels[LabelStep]
	runName = pod.Annotations[AnnotationRunName]
	runUID = types.UID(pod.Labels[LabelRun])
	for _, owner := range pod.OwnerReferences {
		if owner.Kind != "AgenticRun" {
			continue
		}
		if runUID == "" {
			runUID = owner.UID
		}
		if runName == "" {
			runName = owner.Name
		}
	}
	return step, runName, runUID
}

// sandboxClaimForPod walks the agents.x-k8s.io ownership chain from a sandbox Pod
// to the SandboxClaim that created it. The pod-event handler needs the identity
// labels carried along that chain and the timeout loop needs the claim's creation
// time, so the walk lives here once instead of once per caller.
//
// A nil Sandbox means the Pod has no Sandbox owner; a nil Claim with a non-nil
// Sandbox means the chain is incomplete rather than broken. The first Sandbox
// owner wins: a Pod never has more than one, and a partial chain is not worth a
// second lookup. The Claim is always fetched, so callers that could stop early
// pay one cached read.
func sandboxClaimForPod(ctx context.Context, c client.Client, pod *corev1.Pod) (sandbox, claim *unstructured.Unstructured, err error) {
	for _, podRef := range pod.OwnerReferences {
		if podRef.Kind != smSandboxGVK.Kind {
			continue
		}
		sb := &unstructured.Unstructured{}
		sb.SetGroupVersionKind(smSandboxGVK)
		if err := c.Get(ctx, client.ObjectKey{Name: podRef.Name, Namespace: pod.Namespace}, sb); err != nil {
			return nil, nil, err
		}
		sandbox = sb
		for _, sbRef := range sb.GetOwnerReferences() {
			if sbRef.Kind != smClaimGVK.Kind {
				continue
			}
			cl := &unstructured.Unstructured{}
			cl.SetGroupVersionKind(smClaimGVK)
			if err := c.Get(ctx, client.ObjectKey{Name: sbRef.Name, Namespace: pod.Namespace}, cl); err != nil {
				return sandbox, nil, err
			}
			return sandbox, cl, nil
		}
		return sandbox, nil, nil
	}
	return nil, nil, nil
}

// resolveSandboxPodMetadata prefers identity copied onto the Pod template and
// falls back to its existing Sandbox → SandboxClaim ownership chain.
func resolveSandboxPodMetadata(ctx context.Context, c client.Client, pod *corev1.Pod) (step, runName string, runUID types.UID, err error) {
	step = pod.Labels[LabelStep]
	runName = pod.Annotations[AnnotationRunName]
	runUID = types.UID(pod.Labels[LabelRun])
	for _, owner := range pod.OwnerReferences {
		if owner.Kind == "AgenticRun" && runUID == "" {
			runUID = owner.UID
			if runName == "" {
				runName = owner.Name
			}
		}
	}
	if step != "" && runUID != "" {
		return step, runName, runUID, nil
	}

	sandbox, claim, err := sandboxClaimForPod(ctx, c, pod)
	if err != nil {
		return step, runName, runUID, err
	}
	if sandbox == nil {
		return step, runName, runUID, nil
	}

	labels := sandbox.GetLabels()
	if runUID == "" {
		runUID = types.UID(labels[LabelRun])
	}
	if step == "" {
		step = labels[LabelStep]
	}
	if runName == "" {
		runName = sandbox.GetAnnotations()[AnnotationRunName]
	}
	if claim == nil {
		return step, runName, runUID, nil
	}

	claimLabels := claim.GetLabels()
	if runUID == "" {
		runUID = types.UID(claimLabels[LabelRun])
	}
	if step == "" {
		step = claimLabels[LabelStep]
	}
	if runName == "" {
		runName = claim.GetAnnotations()[AnnotationRunName]
	}
	return step, runName, runUID, nil
}

// completeStep handles step completion: patches the step condition (and result ref
// on success), emits audit events, and releases the sandbox. When timeoutMsg is
// non-empty the pod phase is ignored and a timeout failure is recorded. The timeoutReason
// specifies which timeout condition occurred (e.g., ReasonSandboxStartupTimeout or ReasonSandboxTimeout).
// Returns an error if the status patch fails — the caller should bail so the timeout loop
// can retry.
func (r *AgenticRunReconciler) completeStep(ctx context.Context, run *agenticv1alpha1.AgenticRun, pod *corev1.Pod, step, condType, timeoutMsg, timeoutReason string) error {
	if r.APIReader == nil {
		return fmt.Errorf("uncached API reader is required before completing a run step")
	}
	current, err := readRunnableRun(ctx, r.APIReader, client.ObjectKeyFromObject(run), run.UID)
	if err != nil {
		return err
	}
	run = current
	if c := meta.FindStatusCondition(run.Status.Conditions, condType); c != nil && c.Status != metav1.ConditionUnknown {
		return nil
	}

	log := logf.FromContext(ctx)
	var resultCR client.Object
	var patchErr error

	if timeoutMsg != "" {
		log.Info("sandbox timeout", "message", timeoutMsg)
		if timeoutReason == "" {
			timeoutReason = ReasonSandboxTimeout
		}
		patchErr = r.patchStepCondition(ctx, run, condType, metav1.ConditionFalse, timeoutReason, timeoutMsg)
	} else if pod.Status.Phase == corev1.PodSucceeded {
		var reason string
		resultCR, reason = validateResultCR(ctx, r.Client, run, step, r.Namespace)
		if resultCR != nil && reason == "" {
			stepReason := ReasonSucceeded
			stepMsg := "Sandbox completed"
			if step == "analysis" {
				if ar, ok := resultCR.(*agenticv1alpha1.AnalysisResult); ok {
					if ar.Status.ActionRequired == agenticv1alpha1.ActionRequiredFalse {
						stepReason = reasonNoActionRequired
						stepMsg = "Analysis complete — no action required"
					} else if err := r.augmentOptions(ctx, ar, preApprovalAugmentations); err != nil {
						// Augmentations to options applied here so they are visible in AnalysisResult
						log.Error(err, "option augmentation failed; keeping step in progress")
						patchErr = err
					}
				}
			}
			if patchErr == nil {
				log.Info("sandbox step succeeded", "reason", stepReason)
				patchErr = r.patchStepResult(ctx, run, step, condType, resultCR.GetName(), resultCR,
					metav1.ConditionTrue, stepReason, stepMsg)
			}
		} else if resultCR != nil {
			log.Info("agent reported failure", "reason", reason)
			if reason == agenticv1alpha1.ResultReasonAgentTimeout && step == "verification" {
				patchErr = r.patchVerificationFailedEscalating(ctx, run, resultCR.GetName(), resultCR, reason)
			} else if reason == agenticv1alpha1.ResultReasonAgentTimeout {
				patchErr = r.patchStepResult(ctx, run, step, condType, resultCR.GetName(), resultCR,
					metav1.ConditionFalse, ReasonAgentTimeout, "Agent exceeded its configured execution budget")
			} else if step == "verification" {
				patchErr = r.patchVerificationFailedEscalating(ctx, run, resultCR.GetName(), resultCR, reason)
			} else {
				patchErr = r.patchStepResult(ctx, run, step, condType, resultCR.GetName(), resultCR,
					metav1.ConditionFalse, ReasonSandboxFailed, reason)
			}
		} else {
			log.Error(nil, "sandbox result validation failed", "reason", reason)
			patchErr = r.patchStepCondition(ctx, run, condType, metav1.ConditionFalse, ReasonSandboxFailed, reason)
		}
	} else {
		failMsg := podFailMessage(pod)
		failReason := ReasonSandboxFailed
		if terminationMessage, _ := podTerminatedInfo(pod); terminationMessage == ReasonToolResultSafetyInspectionFailed {
			failReason = ReasonToolResultSafetyInspectionFailed
		}
		log.Info("sandbox step failed", "message", failMsg)
		patchErr = r.patchStepCondition(ctx, run, condType, metav1.ConditionFalse, failReason, failMsg)
	}

	if patchErr != nil {
		return patchErr
	}

	current, err = readRunnableRun(ctx, r.APIReader, client.ObjectKeyFromObject(run), run.UID)
	if err != nil {
		return err
	}
	run = current
	if r.Audit != nil {
		r.Audit.CompleteStep(run, step, resultCR)
	}
	condition := meta.FindStatusCondition(run.Status.Conditions, condType)
	preserve := preserveFailedSandbox(run) && condition != nil && condition.Status == metav1.ConditionFalse
	if preserve {
		log.Info("preserving failed sandbox for debugging", LogKeyStep, step)
	} else {
		current, err = readRunnableRun(ctx, r.APIReader, client.ObjectKeyFromObject(run), run.UID)
		if err != nil {
			return err
		}
		run = current
		r.releaseSandbox(ctx, run, step)
	}
	return nil
}

// patchStepCondition patches the step condition on the AgenticRun if it changed.
func (r *AgenticRunReconciler) patchStepCondition(ctx context.Context, run *agenticv1alpha1.AgenticRun, condType string, status metav1.ConditionStatus, reason, message string) error {
	if condType == "" {
		return nil
	}
	if r.APIReader == nil {
		return fmt.Errorf("uncached API reader is required before patching a run step")
	}
	current, err := readRunnableRun(ctx, r.APIReader, client.ObjectKeyFromObject(run), run.UID)
	if err != nil {
		return err
	}
	run = current

	if c := meta.FindStatusCondition(run.Status.Conditions, condType); c != nil {
		if c.Status != metav1.ConditionUnknown {
			return nil
		}
		if c.Status == status && c.Reason == reason {
			return nil
		}
	}

	base := run.DeepCopy()
	meta.SetStatusCondition(&run.Status.Conditions, metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: run.Generation,
	})
	if err := r.statusPatch(ctx, run, base); err != nil {
		logf.FromContext(ctx).Error(err, "pod handler: failed to patch step condition", LogKeyCondition, condType)
		return err
	}
	return nil
}

// patchStepResult patches both the result ref and the step condition in a single
// status update so the reconciler sees them atomically. The resultCR is the
// already-loaded Result CR from validateResultCR — its tokenUsage is aggregated
// into the run's cumulative total without a redundant API server fetch.
func (r *AgenticRunReconciler) patchStepResult(ctx context.Context, run *agenticv1alpha1.AgenticRun, step, condType, crName string, resultCR client.Object, status metav1.ConditionStatus, reason, message string) error {
	if condType == "" {
		return nil
	}
	if r.APIReader == nil {
		return fmt.Errorf("uncached API reader is required before linking a run result")
	}
	current, err := readRunnableRun(ctx, r.APIReader, client.ObjectKeyFromObject(run), run.UID)
	if err != nil {
		return err
	}
	run = current
	// completeStep's terminal check ran against an older read: a pod event and a
	// timeout tick can both pass it, and the loser arrives here after the winner's
	// patch has landed. This read then returns the committed state, so the
	// optimistic lock below would happily accept a second aggregation of the same
	// Result CR. Bail out instead; the caller's remaining work (audit, release) is
	// idempotent, so returning nil is enough.
	if c := meta.FindStatusCondition(run.Status.Conditions, condType); c != nil && c.Status != metav1.ConditionUnknown {
		logf.FromContext(ctx).Info("pod handler: step already completed by another caller, skipping", LogKeyCondition, condType, LogKeyResultCR, crName)
		return nil
	}
	base := run.DeepCopy()
	outcome := agenticv1alpha1.ActionOutcomeSucceeded
	if status != metav1.ConditionTrue {
		outcome = agenticv1alpha1.ActionOutcomeFailed
	}
	appendResultRef(run, step, crName, outcome)
	// Extract once, at step completion, so aggregation and completeness reporting
	// can never disagree about the numbers they are describing.
	usage := extractTokenUsage(resultCR)
	aggregateTokenUsage(run, usage)
	missingFields := r.reportTokenUsage(ctx, run, step, usage, status == metav1.ConditionTrue)
	meta.SetStatusCondition(&run.Status.Conditions, metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: run.Generation,
	})
	if err := r.statusPatch(ctx, run, base); err != nil {
		logf.FromContext(ctx).Error(err, "pod handler: failed to patch step result", LogKeyCondition, condType)
		return err
	}
	recordMissingTokenUsage(missingFields)
	recordTokenUsage(run.Namespace, step, usage)
	return nil
}

// patchVerificationFailedEscalating records an objective verification failure and
// routes the run onto the escalation path (OLS-3817). It appends the verification
// result ref and, in a single status patch, sets Verified=False/VerificationFailed
// plus Escalated=Unknown/VerificationFailed so DerivePhase yields Escalating.
func (r *AgenticRunReconciler) patchVerificationFailedEscalating(ctx context.Context, run *agenticv1alpha1.AgenticRun, crName string, resultCR client.Object, reason string) error {
	if r.APIReader == nil {
		return fmt.Errorf("uncached API reader is required before linking a verification result")
	}
	current, err := readRunnableRun(ctx, r.APIReader, client.ObjectKeyFromObject(run), run.UID)
	if err != nil {
		return err
	}
	run = current
	// Same late-caller guard as patchStepResult: once verification is resolved the
	// committed usage already includes this Result CR.
	if c := meta.FindStatusCondition(run.Status.Conditions, agenticv1alpha1.AgenticRunConditionVerified); c != nil && c.Status != metav1.ConditionUnknown {
		logf.FromContext(ctx).Info("pod handler: verification already resolved by another caller, skipping", LogKeyResultCR, crName)
		return nil
	}
	base := run.DeepCopy()
	appendResultRef(run, "verification", crName, agenticv1alpha1.ActionOutcomeFailed)
	usage := extractTokenUsage(resultCR)
	aggregateTokenUsage(run, usage)
	// A verification failure is a failed step: aggregate only, never assess completeness.
	meta.SetStatusCondition(&run.Status.Conditions, metav1.Condition{
		Type:               agenticv1alpha1.AgenticRunConditionVerified,
		Status:             metav1.ConditionFalse,
		Reason:             agenticv1alpha1.ReasonVerificationFailed,
		Message:            fmt.Sprintf("Verification failed: %s", reason),
		ObservedGeneration: run.Generation,
	})
	meta.SetStatusCondition(&run.Status.Conditions, metav1.Condition{
		Type:               agenticv1alpha1.AgenticRunConditionEscalated,
		Status:             metav1.ConditionUnknown,
		Reason:             agenticv1alpha1.ReasonVerificationFailed,
		Message:            "Verification failed, escalating",
		ObservedGeneration: run.Generation,
	})
	if err := r.statusPatch(ctx, run, base); err != nil {
		logf.FromContext(ctx).Error(err, "pod handler: failed to patch verification-failed escalation")
		return err
	}
	recordTokenUsage(run.Namespace, "verification", usage)
	return nil
}

// aggregateTokenUsage folds the usage extracted from a completed step's Result CR
// into the run's cumulative total. A Result CR that reports nothing is skipped, and
// a count it does not report adds nothing — it never clears what earlier steps
// reported. So the run carries the sum of the counts it has actually seen, and the
// question "is this the first contribution?" never has to be asked: there is no
// seeding step to get wrong.
//
// Counts add, peakInputTokens keeps the maximum, and model is never aggregated
// (spec: token-usage.md rule 13). An explicit zero is a reported value and adds
// normally. A total is only as complete as its addends, so under-reporting is
// signalled by the TokenUsageReported condition and the per-field counter, not by
// withholding the numbers.
func aggregateTokenUsage(run *agenticv1alpha1.AgenticRun, tu agenticv1alpha1.TokenUsage) {
	if tu.IsZero() {
		return
	}

	sum := run.Status.TokenUsage
	sum.InputTokens = addTokenCount(sum.InputTokens, tu.InputTokens)
	sum.OutputTokens = addTokenCount(sum.OutputTokens, tu.OutputTokens)
	sum.ReasoningTokens = addTokenCount(sum.ReasoningTokens, tu.ReasoningTokens)
	sum.ToolTokens = addTokenCount(sum.ToolTokens, tu.ToolTokens)
	sum.PeakInputTokens = maxTokenCount(sum.PeakInputTokens, tu.PeakInputTokens)
	run.Status.TokenUsage = sum
}

// addTokenCount adds a reported count to the aggregate. An unreported count adds
// nothing and a reported zero is a value, so the total only ever grows by what was
// actually reported.
func addTokenCount(current, incoming *int64) *int64 {
	if incoming == nil {
		return current
	}
	if current == nil {
		return ptr.To(*incoming)
	}
	return ptr.To(*current + *incoming)
}

// maxTokenCount is addTokenCount's maximum-based counterpart, used for
// peakInputTokens.
func maxTokenCount(current, incoming *int64) *int64 {
	if incoming == nil {
		return current
	}
	if current == nil || *incoming > *current {
		return ptr.To(*incoming)
	}
	return current
}

// extractTokenUsage returns the tokenUsage from a Result CR via type switch.
func extractTokenUsage(obj client.Object) agenticv1alpha1.TokenUsage {
	switch cr := obj.(type) {
	case *agenticv1alpha1.AnalysisResult:
		return cr.Status.TokenUsage
	case *agenticv1alpha1.ExecutionResult:
		return cr.Status.TokenUsage
	case *agenticv1alpha1.VerificationResult:
		return cr.Status.TokenUsage
	case *agenticv1alpha1.EscalationResult:
		return cr.Status.TokenUsage
	default:
		return agenticv1alpha1.TokenUsage{}
	}
}

// appendResultRef appends a StepResultRef to the run's status for the given step.
func appendResultRef(run *agenticv1alpha1.AgenticRun, step, name string, outcome agenticv1alpha1.ActionOutcome) {
	ref := agenticv1alpha1.StepResultRef{Name: name, Outcome: outcome}
	switch step {
	case "analysis":
		run.Status.Steps.Analysis.Results = append(run.Status.Steps.Analysis.Results, ref)
	case "execution":
		run.Status.Steps.Execution.Results = append(run.Status.Steps.Execution.Results, ref)
	case "verification":
		run.Status.Steps.Verification.Results = append(run.Status.Steps.Verification.Results, ref)
	case "escalation":
		run.Status.Steps.Escalation.Results = append(run.Status.Steps.Escalation.Results, ref)
	}
}

func (r *AgenticRunReconciler) releaseSandbox(ctx context.Context, run *agenticv1alpha1.AgenticRun, step string) {
	if err := r.Agent.ReleaseSandbox(ctx, run, step); err != nil {
		logf.FromContext(ctx).Error(err, "failed to release sandbox", LogKeyStep, step)
	}
}

// ---------------------------------------------------------------------------
// Step ↔ condition / claim mapping helpers
// ---------------------------------------------------------------------------

// stepConditionType maps a step label value to the AgenticRun condition type.
func stepConditionType(step string) string {
	switch step {
	case "analysis":
		return agenticv1alpha1.AgenticRunConditionAnalyzed
	case "execution":
		return agenticv1alpha1.AgenticRunConditionExecuted
	case "verification":
		return agenticv1alpha1.AgenticRunConditionVerified
	case "escalation":
		return agenticv1alpha1.AgenticRunConditionEscalated
	default:
		return ""
	}
}

// isStepInProgress returns true if the step condition is Unknown (in-progress).
func isStepInProgress(run *agenticv1alpha1.AgenticRun, condType string) bool {
	c := meta.FindStatusCondition(run.Status.Conditions, condType)
	return c != nil && c.Status == metav1.ConditionUnknown
}

func sandboxClaimName(run *agenticv1alpha1.AgenticRun, step string) string {
	switch step {
	case "analysis":
		return run.Status.Steps.Analysis.Sandbox.ClaimName
	case "execution":
		return run.Status.Steps.Execution.Sandbox.ClaimName
	case "verification":
		return run.Status.Steps.Verification.Sandbox.ClaimName
	case "escalation":
		return run.Status.Steps.Escalation.Sandbox.ClaimName
	default:
		return ""
	}
}

// validateResultCR fetches the Result CR for the step, validates it has a
// Completed=True condition, and checks for agent failure (failureReason or
// Completed reason=Failed). Returns the CR and an empty reason on success,
// or nil/reason on any failure.
func validateResultCR(ctx context.Context, c client.Client, run *agenticv1alpha1.AgenticRun, step, namespace string) (client.Object, string) {
	name := resultCRName(run.Name, step, nextResultIndex(run, step))
	key := client.ObjectKey{Name: name, Namespace: namespace}

	obj, conditions, failureReason := fetchResultCRByStep(ctx, c, key, step)
	if obj == nil {
		return nil, msgSandboxNoResult
	}

	cond := meta.FindStatusCondition(conditions, agenticv1alpha1.ResultConditionCompleted)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		return nil, "result CR missing Completed condition"
	}
	// Cooperative timeout is a distinct agent outcome. Check the Completed
	// condition before failureReason because the sandbox includes both fields.
	if cond.Reason == agenticv1alpha1.ResultReasonAgentTimeout {
		return obj, agenticv1alpha1.ResultReasonAgentTimeout
	}
	if failureReason != "" {
		return obj, failureReason
	}
	if cond.Reason == agenticv1alpha1.ResultReasonFailed {
		return obj, "agent reported failure"
	}
	if reason := validateResultStatus(obj); reason != "" {
		return nil, reason
	}
	return obj, ""
}

// validateResultStatus checks that the Result CR has mandatory status fields
// populated for a successful result.
func validateResultStatus(obj client.Object) string {
	switch cr := obj.(type) {
	case *agenticv1alpha1.AnalysisResult:
		if cr.Status.ActionRequired != agenticv1alpha1.ActionRequiredFalse && len(cr.Status.Options) == 0 {
			return "result CR missing options"
		}
	case *agenticv1alpha1.ExecutionResult:
		if len(cr.Status.ActionsTaken) == 0 {
			return "result CR missing actionsTaken"
		}
	case *agenticv1alpha1.VerificationResult:
		if len(cr.Status.Checks) == 0 {
			return "result CR missing checks"
		}
	case *agenticv1alpha1.EscalationResult:
		if cr.Status.Summary == "" {
			return "result CR missing summary"
		}
	}
	return ""
}

func fetchResultCRByStep(ctx context.Context, c client.Client, key client.ObjectKey, step string) (client.Object, []metav1.Condition, string) {
	switch step {
	case "analysis":
		cr := &agenticv1alpha1.AnalysisResult{}
		if err := c.Get(ctx, key, cr); err != nil {
			return nil, nil, ""
		}
		return cr, cr.Status.Conditions, cr.Status.FailureReason
	case "execution":
		cr := &agenticv1alpha1.ExecutionResult{}
		if err := c.Get(ctx, key, cr); err != nil {
			return nil, nil, ""
		}
		return cr, cr.Status.Conditions, cr.Status.FailureReason
	case "verification":
		cr := &agenticv1alpha1.VerificationResult{}
		if err := c.Get(ctx, key, cr); err != nil {
			return nil, nil, ""
		}
		return cr, cr.Status.Conditions, cr.Status.FailureReason
	case "escalation":
		cr := &agenticv1alpha1.EscalationResult{}
		if err := c.Get(ctx, key, cr); err != nil {
			return nil, nil, ""
		}
		return cr, cr.Status.Conditions, cr.Status.FailureReason
	default:
		return nil, nil, ""
	}
}

// podFailMessage returns a human-readable failure description from the pod's
// first terminated container, falling back to a generic message.
func podFailMessage(pod *corev1.Pod) string {
	if pod.Status.Phase == corev1.PodSucceeded {
		return msgSandboxNoResult
	}
	msg, exitCode := podTerminatedInfo(pod)
	if msg != "" {
		if msg == ReasonToolResultSafetyInspectionFailed {
			return msgToolResultSafetyInspectionFailed
		}
		return msg
	}
	if exitCode != nil {
		return fmt.Sprintf("sandbox pod failed (exit %d)", *exitCode)
	}
	return "sandbox pod failed"
}

// podTerminatedInfo returns the first terminated container's message and exit code.
func podTerminatedInfo(pod *corev1.Pod) (msg string, exitCode *int32) {
	if pod == nil || len(pod.Status.ContainerStatuses) == 0 {
		return "", nil
	}
	term := pod.Status.ContainerStatuses[0].State.Terminated
	if term == nil {
		return "", nil
	}
	code := term.ExitCode
	return term.Message, &code
}

// ---------------------------------------------------------------------------
// Token usage completeness reporting
// ---------------------------------------------------------------------------

// Token-usage reporting contract (spec: token-usage.md):
//   - required: inputTokens, outputTokens and peakInputTokens whenever tokenUsage
//     is present. Their absence means the sandbox dropped data we cannot
//     reconstruct (peakInputTokens is the only measure of context occupancy), so
//     it is reported loudly.
//   - optional: reasoningTokens and toolTokens. Omission is expected and
//     legitimate: providers that cannot surface reasoning details (Bedrock,
//     Vertex) MUST omit reasoningTokens rather than report 0, and tool token
//     attribution is an estimate that may be unavailable.
//
// skill tokens are intentionally NOT a field: skill content is read through tools,
// so it is already part of toolTokens.

// tokenUsageRequiredFields and tokenUsageOptionalFields are in canonical order so
// messages, events and metric series stay stable and diffable.
var (
	tokenUsageRequiredFields = []string{"inputTokens", "outputTokens", "peakInputTokens"}
	tokenUsageOptionalFields = []string{"reasoningTokens", "toolTokens"}
)

// tokenUsageReporting is the assessment of one TokenUsage value.
type tokenUsageReporting struct {
	Status  metav1.ConditionStatus
	Reason  string
	Message string
	// RequiredMissing and OptionalMissing list field names in canonical order.
	RequiredMissing []string
	OptionalMissing []string
}

// unreportedTokenUsageFields returns which of the requested fields were not
// reported. An explicit zero counts as reported: absence is "not reported",
// zero is "reported as none".
func unreportedTokenUsageFields(tu agenticv1alpha1.TokenUsage, fields []string) []string {
	reported := map[string]*int64{
		"inputTokens":     tu.InputTokens,
		"outputTokens":    tu.OutputTokens,
		"reasoningTokens": tu.ReasoningTokens,
		"toolTokens":      tu.ToolTokens,
		"peakInputTokens": tu.PeakInputTokens,
	}
	var missing []string
	for _, field := range fields {
		if reported[field] == nil {
			missing = append(missing, field)
		}
	}
	return missing
}

// assessTokenUsageReporting classifies required aggregate field presence in a
// TokenUsage value. It does not verify that every underlying model call reported
// usage. Only missing required fields (or an entirely absent struct) make the
// assessment negative; missing optional fields keep it positive and are merely
// described.
func assessTokenUsageReporting(tu agenticv1alpha1.TokenUsage) tokenUsageReporting {
	requiredMissing := unreportedTokenUsageFields(tu, tokenUsageRequiredFields)
	optionalMissing := unreportedTokenUsageFields(tu, tokenUsageOptionalFields)

	switch {
	case tu.IsZero():
		return tokenUsageReporting{
			Status:          metav1.ConditionFalse,
			Reason:          agenticv1alpha1.ReasonTokenUsageReportedNone,
			Message:         "No token usage was reported by the sandbox",
			RequiredMissing: requiredMissing,
		}
	case len(requiredMissing) > 0:
		return tokenUsageReporting{
			Status:          metav1.ConditionFalse,
			Reason:          agenticv1alpha1.ReasonTokenUsageReportedIncomplete,
			Message:         fmt.Sprintf("Required token usage fields not reported: %s", strings.Join(requiredMissing, ", ")),
			RequiredMissing: requiredMissing,
			OptionalMissing: optionalMissing,
		}
	case len(optionalMissing) > 0:
		return tokenUsageReporting{
			Status:          metav1.ConditionTrue,
			Reason:          agenticv1alpha1.ReasonTokenUsageReportedComplete,
			Message:         fmt.Sprintf("Token usage reported; optional fields omitted: %s", strings.Join(optionalMissing, ", ")),
			OptionalMissing: optionalMissing,
		}
	default:
		return tokenUsageReporting{
			Status:  metav1.ConditionTrue,
			Reason:  agenticv1alpha1.ReasonTokenUsageReportedComplete,
			Message: "All required token usage fields were reported",
		}
	}
}

// reportTokenUsage records whether a step reported the required aggregate token-
// usage fields. It does not measure per-model-call coverage and MUST be called
// after aggregateTokenUsage.
//
// The required-field reporting contract applies to SUCCEEDED steps only. A failed step that
// reports no token usage is expected — it may have died before its first model
// call, or been stopped mid-flight — so failures record nothing here: their usage
// is still aggregated by aggregateTokenUsage, but no counter, event or condition is
// produced. Flagging them would turn every failure into a false alarm.
//
// It is deliberately advisory: it never returns an error and never blocks a step,
// because token accounting is telemetry while the run's purpose is remediation.
// Signals for a succeeded step:
//   - one counter increment for each missing field;
//   - a warning-oriented operator log for missing required fields;
//   - the TokenUsageReported condition on the AgenticRun, which is sticky once
//     False: a run whose totals are known to be incomplete stays flagged.
//
// Token-usage telemetry never creates Kubernetes Events: the condition is the
// API-visible per-run signal, while logs and metrics provide operational detail.
func (r *AgenticRunReconciler) reportTokenUsage(ctx context.Context, run *agenticv1alpha1.AgenticRun, step string, usage agenticv1alpha1.TokenUsage, stepSucceeded bool) []string {
	if !stepSucceeded {
		return nil
	}

	assessment := assessTokenUsageReporting(usage)

	missingFields := append(assessment.RequiredMissing, assessment.OptionalMissing...)

	if len(assessment.RequiredMissing) > 0 {
		logf.FromContext(ctx).Info("step Result CR did not report required token usage fields",
			LogKeyName, run.Name, LogKeyStep, step, "fields", strings.Join(assessment.RequiredMissing, ","))
	}

	// Sticky: once the run is known to be under-reported, a later well-behaved
	// step cannot repair an already-summed total.
	if existing := meta.FindStatusCondition(run.Status.Conditions, agenticv1alpha1.AgenticRunConditionTokenUsageReported); existing != nil && existing.Status == metav1.ConditionFalse {
		return missingFields
	}

	meta.SetStatusCondition(&run.Status.Conditions, metav1.Condition{
		Type:               agenticv1alpha1.AgenticRunConditionTokenUsageReported,
		Status:             assessment.Status,
		Reason:             assessment.Reason,
		Message:            assessment.Message,
		ObservedGeneration: run.Generation,
	})
	return missingFields
}
