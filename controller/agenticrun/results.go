package agenticrun

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
)

const (
	ErrGetExistingResult          = "get existing"
	ErrUpdateExistingResultStatus = "update existing"
	ErrCreateResultCR             = "create"
	ErrPatchResultStatus          = "patch"
)

func resultCRName(agenticRunName, step string, index int) string {
	return truncateK8sName(fmt.Sprintf("%s-%s-%d", agenticRunName, step, index))
}

func agenticRunOwnerRef(run *agenticv1alpha1.AgenticRun) metav1.OwnerReference {
	return metav1.OwnerReference{
		APIVersion:         "agentic.openshift.io/v1alpha1",
		Kind:               "AgenticRun",
		Name:               run.Name,
		UID:                run.UID,
		Controller:         ptr.To(true),
		BlockOwnerDeletion: ptr.To(true),
	}
}

func resultLabels(agenticRunName, step string) map[string]string {
	return map[string]string{
		LabelRun:  truncateK8sName(agenticRunName),
		LabelStep: step,
	}
}

func executionRetryIndex(run *agenticv1alpha1.AgenticRun) int32 {
	if run.Status.Steps.Execution.RetryCount != nil {
		return *run.Status.Steps.Execution.RetryCount
	}
	return 0
}

func resultConditions(startTime *metav1.Time, completionTime metav1.Time, outcome agenticv1alpha1.ActionOutcome) []metav1.Condition {
	conditions := make([]metav1.Condition, 0, 2)
	if startTime != nil {
		conditions = append(conditions, metav1.Condition{
			Type:               agenticv1alpha1.ResultConditionStarted,
			Status:             metav1.ConditionTrue,
			LastTransitionTime: *startTime,
			Reason:             agenticv1alpha1.ResultReasonStepStarted,
		})
	}
	reason := agenticv1alpha1.ResultReasonFailed
	if outcome == agenticv1alpha1.ActionOutcomeSucceeded {
		reason = agenticv1alpha1.ResultReasonSucceeded
	}
	conditions = append(conditions, metav1.Condition{
		Type:               agenticv1alpha1.ResultConditionCompleted,
		Status:             metav1.ConditionTrue,
		LastTransitionTime: completionTime,
		Reason:             reason,
	})
	return conditions
}

func (r *AgenticRunReconciler) createAnalysisResult(
	ctx context.Context,
	run *agenticv1alpha1.AgenticRun,
	result *AnalysisOutput,
	sandbox agenticv1alpha1.SandboxInfo,
	startTime *metav1.Time,
	completionTime *metav1.Time,
	failureReason string,
) (string, *agenticv1alpha1.AnalysisResult, error) {
	crName := resultCRName(run.Name, "analysis", len(run.Status.Steps.Analysis.Results)+1)

	outcome := agenticv1alpha1.ActionOutcomeFailed
	if result != nil {
		outcome = agenticv1alpha1.ActionOutcomeFromBool(result.Success)
	}

	completedAt := metav1.Now()
	if completionTime != nil {
		completedAt = *completionTime
	}

	cr := &agenticv1alpha1.AnalysisResult{
		ObjectMeta: metav1.ObjectMeta{
			Name:            crName,
			Namespace:       run.Namespace,
			Labels:          resultLabels(run.Name, "analysis"),
			OwnerReferences: []metav1.OwnerReference{agenticRunOwnerRef(run)},
		},
		Spec: agenticv1alpha1.AnalysisResultSpec{
			AgenticRunName: run.Name,
		},
		Status: agenticv1alpha1.AnalysisResultStatus{
			Conditions:    resultConditions(startTime, completedAt, outcome),
			Sandbox:       sandbox,
			FailureReason: failureReason,
		},
	}

	if result != nil {
		cr.Status.Options = result.Options
		cr.Status.ActionRequired = agenticv1alpha1.ActionRequiredFromBool(result.IsActionRequired())
		if result.Diagnosis != nil && result.Diagnosis.Summary != "" && result.Diagnosis.RootCause != "" && result.Diagnosis.Confidence != "" {
			cr.Status.Diagnosis = *result.Diagnosis
		}
	}

	snapshot := cr.DeepCopy()
	if err := createIdempotent(ctx, r.Client, cr, "AnalysisResult"); err != nil {
		return crName, nil, err
	}
	snapshot.UID = cr.UID
	snapshot.CreationTimestamp = cr.CreationTimestamp
	return crName, snapshot, nil
}

func (r *AgenticRunReconciler) createExecutionResult(
	ctx context.Context,
	run *agenticv1alpha1.AgenticRun,
	result *ExecutionOutput,
	sandbox agenticv1alpha1.SandboxInfo,
	startTime *metav1.Time,
	completionTime *metav1.Time,
	failureReason string,
) (string, *agenticv1alpha1.ExecutionResult, error) {
	crName := resultCRName(run.Name, "execution", len(run.Status.Steps.Execution.Results)+1)

	outcome := agenticv1alpha1.ActionOutcomeFailed
	if result != nil {
		outcome = agenticv1alpha1.ActionOutcomeFromBool(result.Success)
	}

	completedAt := metav1.Now()
	if completionTime != nil {
		completedAt = *completionTime
	}

	cr := &agenticv1alpha1.ExecutionResult{
		ObjectMeta: metav1.ObjectMeta{
			Name:            crName,
			Namespace:       run.Namespace,
			Labels:          resultLabels(run.Name, "execution"),
			OwnerReferences: []metav1.OwnerReference{agenticRunOwnerRef(run)},
		},
		Spec: agenticv1alpha1.ExecutionResultSpec{
			AgenticRunName: run.Name,
			RetryIndex:     ptr.To(executionRetryIndex(run)),
		},
		Status: agenticv1alpha1.ExecutionResultStatus{
			Conditions:    resultConditions(startTime, completedAt, outcome),
			Sandbox:       sandbox,
			FailureReason: failureReason,
		},
	}

	if result != nil {
		cr.Status.ActionsTaken = result.ActionsTaken
		cr.Status.Verification = result.Verification
	}

	snapshot := cr.DeepCopy()
	if err := createIdempotent(ctx, r.Client, cr, "ExecutionResult"); err != nil {
		return crName, nil, err
	}
	snapshot.UID = cr.UID
	snapshot.CreationTimestamp = cr.CreationTimestamp
	return crName, snapshot, nil
}

func (r *AgenticRunReconciler) createVerificationResult(
	ctx context.Context,
	run *agenticv1alpha1.AgenticRun,
	result *VerificationOutput,
	sandbox agenticv1alpha1.SandboxInfo,
	startTime *metav1.Time,
	completionTime *metav1.Time,
	failureReason string,
) (string, *agenticv1alpha1.VerificationResult, error) {
	crName := resultCRName(run.Name, "verification", len(run.Status.Steps.Verification.Results)+1)

	outcome := agenticv1alpha1.ActionOutcomeFailed
	if result != nil {
		outcome = agenticv1alpha1.ActionOutcomeFromBool(result.Success)
	}

	completedAt := metav1.Now()
	if completionTime != nil {
		completedAt = *completionTime
	}

	cr := &agenticv1alpha1.VerificationResult{
		ObjectMeta: metav1.ObjectMeta{
			Name:            crName,
			Namespace:       run.Namespace,
			Labels:          resultLabels(run.Name, "verification"),
			OwnerReferences: []metav1.OwnerReference{agenticRunOwnerRef(run)},
		},
		Spec: agenticv1alpha1.VerificationResultSpec{
			AgenticRunName: run.Name,
			RetryIndex:     ptr.To(executionRetryIndex(run)),
		},
		Status: agenticv1alpha1.VerificationResultStatus{
			Conditions:    resultConditions(startTime, completedAt, outcome),
			Sandbox:       sandbox,
			FailureReason: failureReason,
		},
	}

	if result != nil {
		cr.Status.Checks = result.Checks
		cr.Status.Summary = result.Summary
	}

	snapshot := cr.DeepCopy()
	if err := createIdempotent(ctx, r.Client, cr, "VerificationResult"); err != nil {
		return crName, nil, err
	}
	snapshot.UID = cr.UID
	snapshot.CreationTimestamp = cr.CreationTimestamp
	return crName, snapshot, nil
}

func (r *AgenticRunReconciler) createEscalationResult(
	ctx context.Context,
	run *agenticv1alpha1.AgenticRun,
	result *EscalationOutput,
	sandbox agenticv1alpha1.SandboxInfo,
	startTime *metav1.Time,
	completionTime *metav1.Time,
	failureReason string,
) (string, *agenticv1alpha1.EscalationResult, error) {
	crName := resultCRName(run.Name, "escalation", len(run.Status.Steps.Escalation.Results)+1)

	outcome := agenticv1alpha1.ActionOutcomeFailed
	if result != nil {
		outcome = agenticv1alpha1.ActionOutcomeFromBool(result.Success)
	}

	completedAt := metav1.Now()
	if completionTime != nil {
		completedAt = *completionTime
	}

	cr := &agenticv1alpha1.EscalationResult{
		ObjectMeta: metav1.ObjectMeta{
			Name:            crName,
			Namespace:       run.Namespace,
			Labels:          resultLabels(run.Name, "escalation"),
			OwnerReferences: []metav1.OwnerReference{agenticRunOwnerRef(run)},
		},
		Spec: agenticv1alpha1.EscalationResultSpec{
			AgenticRunName: run.Name,
		},
		Status: agenticv1alpha1.EscalationResultStatus{
			Conditions:    resultConditions(startTime, completedAt, outcome),
			Sandbox:       sandbox,
			FailureReason: failureReason,
		},
	}

	if result != nil {
		cr.Status.Summary = result.Summary
		cr.Status.Content = result.Content
	}

	snapshot := cr.DeepCopy()
	if err := createIdempotent(ctx, r.Client, cr, "EscalationResult"); err != nil {
		return crName, nil, err
	}
	snapshot.UID = cr.UID
	snapshot.CreationTimestamp = cr.CreationTimestamp
	return crName, snapshot, nil
}

type statusHolder interface {
	client.Object
	GetConditions() []metav1.Condition
	SetConditions([]metav1.Condition)
}

// copyResultStatus copies result-specific status fields from src to dst.
// Both must be the same concrete type (guaranteed by callers which derive
// both from the same obj via DeepCopyObject).
func copyResultStatus(dst, src client.Object) {
	switch d := dst.(type) {
	case *agenticv1alpha1.AnalysisResult:
		if s, ok := src.(*agenticv1alpha1.AnalysisResult); ok {
			d.Status.Options = s.Status.Options
			d.Status.ActionRequired = s.Status.ActionRequired
			d.Status.Diagnosis = s.Status.Diagnosis
			d.Status.FailureReason = s.Status.FailureReason
			d.Status.Sandbox = s.Status.Sandbox
		}
	case *agenticv1alpha1.ExecutionResult:
		if s, ok := src.(*agenticv1alpha1.ExecutionResult); ok {
			d.Status.ActionsTaken = s.Status.ActionsTaken
			d.Status.Verification = s.Status.Verification
			d.Status.FailureReason = s.Status.FailureReason
			d.Status.Sandbox = s.Status.Sandbox
		}
	case *agenticv1alpha1.VerificationResult:
		if s, ok := src.(*agenticv1alpha1.VerificationResult); ok {
			d.Status.Checks = s.Status.Checks
			d.Status.Summary = s.Status.Summary
			d.Status.FailureReason = s.Status.FailureReason
			d.Status.Sandbox = s.Status.Sandbox
		}
	case *agenticv1alpha1.EscalationResult:
		if s, ok := src.(*agenticv1alpha1.EscalationResult); ok {
			d.Status.Summary = s.Status.Summary
			d.Status.Content = s.Status.Content
			d.Status.FailureReason = s.Status.FailureReason
			d.Status.Sandbox = s.Status.Sandbox
		}
	}
}

// createIdempotent creates obj then patches its full status. The Create
// call writes identity fields (agenticRunName, etc.) but the API
// server ignores .status on Create (status subresource). A follow-up
// Status().Patch writes the complete status including result data and
// conditions. On AlreadyExists the existing CR's status is updated
// to reflect the latest result.
func createIdempotent(ctx context.Context, c client.Client, obj client.Object, kind string) error {
	// Save full object with status before Create strips it.
	withStatus := obj.DeepCopyObject().(client.Object)

	if err := c.Create(ctx, obj); err != nil {
		if apierrors.IsAlreadyExists(err) {
			existing := obj.DeepCopyObject().(client.Object)
			if getErr := c.Get(ctx, client.ObjectKeyFromObject(obj), existing); getErr != nil {
				return fmt.Errorf("%s %s %s: %w", ErrGetExistingResult, kind, obj.GetName(), getErr)
			}
			patched := existing.DeepCopyObject().(client.Object)
			if sh, ok := patched.(statusHolder); ok {
				if src, ok := withStatus.(statusHolder); ok {
					sh.SetConditions(src.GetConditions())
				}
			}
			copyResultStatus(patched, withStatus)
			if patchErr := c.Status().Patch(ctx, patched, client.MergeFrom(existing)); patchErr != nil {
				return fmt.Errorf("%s %s %s status: %w", ErrUpdateExistingResultStatus, kind, obj.GetName(), patchErr)
			}
			obj.SetUID(existing.GetUID())
			obj.SetCreationTimestamp(existing.GetCreationTimestamp())
			return nil
		}
		return fmt.Errorf("%s %s %s: %w", ErrCreateResultCR, kind, obj.GetName(), err)
	}

	// After Create, obj has ResourceVersion but status is stripped.
	// Use the saved copy (with full status) for the status patch.
	withStatus.SetResourceVersion(obj.GetResourceVersion())
	if err := c.Status().Patch(ctx, withStatus, client.MergeFrom(obj)); err != nil {
		return fmt.Errorf("%s %s %s status: %w", ErrPatchResultStatus, kind, obj.GetName(), err)
	}
	return nil
}
