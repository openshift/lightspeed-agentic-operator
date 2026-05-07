package proposal

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
)

func resultCRName(proposalName, step string, index int) string {
	return truncateK8sName(fmt.Sprintf("%s-%s-%d", proposalName, step, index))
}

func proposalOwnerRef(proposal *agenticv1alpha1.Proposal) metav1.OwnerReference {
	return metav1.OwnerReference{
		APIVersion:         "agentic.openshift.io/v1alpha1",
		Kind:               "Proposal",
		Name:               proposal.Name,
		UID:                proposal.UID,
		Controller:         ptr.To(true),
		BlockOwnerDeletion: ptr.To(true),
	}
}

func resultLabels(proposalName, step string) map[string]string {
	return map[string]string{
		LabelProposal: proposalName,
		LabelStep:     step,
	}
}

func executionRetryIndex(proposal *agenticv1alpha1.Proposal) int32 {
	if proposal.Status.Steps.Execution.RetryCount != nil {
		return *proposal.Status.Steps.Execution.RetryCount
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

func (r *ProposalReconciler) createAnalysisResult(
	ctx context.Context,
	proposal *agenticv1alpha1.Proposal,
	result *AnalysisOutput,
	sandbox agenticv1alpha1.SandboxInfo,
	startTime *metav1.Time,
	completionTime *metav1.Time,
	failureReason string,
) (string, error) {
	crName := resultCRName(proposal.Name, "analysis", len(proposal.Status.Steps.Analysis.Results)+1)

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
			Namespace:       proposal.Namespace,
			Labels:          resultLabels(proposal.Name, "analysis"),
			OwnerReferences: []metav1.OwnerReference{proposalOwnerRef(proposal)},
		},
		Spec: agenticv1alpha1.AnalysisResultSpec{
			ProposalName: proposal.Name,
		},
		Status: agenticv1alpha1.AnalysisResultStatus{
			Conditions:    resultConditions(startTime, completedAt, outcome),
			Sandbox:       sandbox,
			FailureReason: failureReason,
		},
	}

	if result != nil {
		cr.Status.Options = result.Options
	}

	return crName, createIdempotent(ctx, r.Client, cr, "AnalysisResult")
}

func (r *ProposalReconciler) createExecutionResult(
	ctx context.Context,
	proposal *agenticv1alpha1.Proposal,
	result *ExecutionOutput,
	sandbox agenticv1alpha1.SandboxInfo,
	startTime *metav1.Time,
	completionTime *metav1.Time,
	failureReason string,
) (string, error) {
	crName := resultCRName(proposal.Name, "execution", len(proposal.Status.Steps.Execution.Results)+1)

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
			Namespace:       proposal.Namespace,
			Labels:          resultLabels(proposal.Name, "execution"),
			OwnerReferences: []metav1.OwnerReference{proposalOwnerRef(proposal)},
		},
		Spec: agenticv1alpha1.ExecutionResultSpec{
			ProposalName: proposal.Name,
			RetryIndex:   ptr.To(executionRetryIndex(proposal)),
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

	return crName, createIdempotent(ctx, r.Client, cr, "ExecutionResult")
}

func (r *ProposalReconciler) createVerificationResult(
	ctx context.Context,
	proposal *agenticv1alpha1.Proposal,
	result *VerificationOutput,
	sandbox agenticv1alpha1.SandboxInfo,
	startTime *metav1.Time,
	completionTime *metav1.Time,
	failureReason string,
) (string, error) {
	crName := resultCRName(proposal.Name, "verification", len(proposal.Status.Steps.Verification.Results)+1)

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
			Namespace:       proposal.Namespace,
			Labels:          resultLabels(proposal.Name, "verification"),
			OwnerReferences: []metav1.OwnerReference{proposalOwnerRef(proposal)},
		},
		Spec: agenticv1alpha1.VerificationResultSpec{
			ProposalName: proposal.Name,
			RetryIndex:   ptr.To(executionRetryIndex(proposal)),
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

	return crName, createIdempotent(ctx, r.Client, cr, "VerificationResult")
}

func (r *ProposalReconciler) createEscalationResult(
	ctx context.Context,
	proposal *agenticv1alpha1.Proposal,
	result *EscalationOutput,
	sandbox agenticv1alpha1.SandboxInfo,
	startTime *metav1.Time,
	completionTime *metav1.Time,
	failureReason string,
) (string, error) {
	crName := resultCRName(proposal.Name, "escalation", len(proposal.Status.Steps.Escalation.Results)+1)

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
			Namespace:       proposal.Namespace,
			Labels:          resultLabels(proposal.Name, "escalation"),
			OwnerReferences: []metav1.OwnerReference{proposalOwnerRef(proposal)},
		},
		Spec: agenticv1alpha1.EscalationResultSpec{
			ProposalName: proposal.Name,
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

	return crName, createIdempotent(ctx, r.Client, cr, "EscalationResult")
}

type statusHolder interface {
	client.Object
	GetConditions() []metav1.Condition
	SetConditions([]metav1.Condition)
}

// createIdempotent creates obj then patches its full status. The Create
// call writes identity fields (proposalName, etc.) but the API
// server ignores .status on Create (status subresource). A follow-up
// Status().Patch writes the complete status including result data and
// conditions. On AlreadyExists the CR is assumed to already have its
// status from the original create.
func createIdempotent(ctx context.Context, c client.Client, obj client.Object, kind string) error {
	// Save full object with status before Create strips it.
	withStatus := obj.DeepCopyObject().(client.Object)

	if err := c.Create(ctx, obj); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return nil
		}
		return fmt.Errorf("create %s %s: %w", kind, obj.GetName(), err)
	}

	// After Create, obj has ResourceVersion but status is stripped.
	// Use the saved copy (with full status) for the status patch.
	withStatus.SetResourceVersion(obj.GetResourceVersion())
	if err := c.Status().Patch(ctx, withStatus, client.MergeFrom(obj)); err != nil {
		return fmt.Errorf("patch %s %s status: %w", kind, obj.GetName(), err)
	}
	return nil
}
