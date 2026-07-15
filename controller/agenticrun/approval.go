package agenticrun

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
)

const (
	ErrGetApprovalPolicy        = "get ApprovalPolicy"
	ErrGetAgenticRunApproval    = "get AgenticRunApproval"
	ErrCreateAgenticRunApproval = "create AgenticRunApproval"
)

func getApprovalPolicy(ctx context.Context, c client.Client) (*agenticv1alpha1.ApprovalPolicy, error) {
	policy := &agenticv1alpha1.ApprovalPolicy{}
	err := c.Get(ctx, types.NamespacedName{Name: "cluster"}, policy)
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w", ErrGetApprovalPolicy, err)
	}
	return policy, nil
}

func getAgenticRunApproval(ctx context.Context, c client.Client, run *agenticv1alpha1.AgenticRun) (*agenticv1alpha1.AgenticRunApproval, error) {
	approval := &agenticv1alpha1.AgenticRunApproval{}
	err := c.Get(ctx, types.NamespacedName{Name: run.Name, Namespace: run.Namespace}, approval)
	if err != nil {
		return nil, err
	}
	return approval, nil
}

func ensureAgenticRunApproval(
	ctx context.Context,
	c client.Client,
	run *agenticv1alpha1.AgenticRun,
	policy *agenticv1alpha1.ApprovalPolicy,
) (*agenticv1alpha1.AgenticRunApproval, error) {
	existing, err := getAgenticRunApproval(ctx, c, run)
	if err == nil {
		return existing, nil
	}
	if !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("%s: %w", ErrGetAgenticRunApproval, err)
	}

	var autoStages []agenticv1alpha1.ApprovalStage
	if policy != nil {
		for _, ps := range policy.Spec.Stages {
			if ps.Approval != agenticv1alpha1.ApprovalModeAutomatic {
				continue
			}
			switch ps.Name {
			case agenticv1alpha1.SandboxStepAnalysis:
				autoStages = append(autoStages, agenticv1alpha1.NewApprovalStage(
					agenticv1alpha1.ApprovalStageAnalysis, "", "", nil, 0))
			case agenticv1alpha1.SandboxStepExecution:
				if run.Spec.Execution.IsZero() {
					continue
				}
				autoStages = append(autoStages, agenticv1alpha1.NewApprovalStage(
					agenticv1alpha1.ApprovalStageExecution, "", "", nil, 0))
			case agenticv1alpha1.SandboxStepVerification:
				if run.Spec.Verification.IsZero() {
					continue
				}
				autoStages = append(autoStages, agenticv1alpha1.NewApprovalStage(
					agenticv1alpha1.ApprovalStageVerification, "", "", nil, 0))
			case agenticv1alpha1.SandboxStepEscalation:
				continue
			}
		}
	}

	approval := &agenticv1alpha1.AgenticRunApproval{
		ObjectMeta: metav1.ObjectMeta{
			Name:      run.Name,
			Namespace: run.Namespace,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion:         "agentic.openshift.io/v1alpha1",
				Kind:               "AgenticRun",
				Name:               run.Name,
				UID:                run.UID,
				Controller:         ptr.To(true),
				BlockOwnerDeletion: ptr.To(true),
			}},
		},
		Spec: agenticv1alpha1.AgenticRunApprovalSpec{
			Stages: autoStages,
		},
	}

	if err := c.Create(ctx, approval); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return getAgenticRunApproval(ctx, c, run)
		}
		return nil, fmt.Errorf("%s: %w", ErrCreateAgenticRunApproval, err)
	}
	return approval, nil
}

func isStageApproved(approval *agenticv1alpha1.AgenticRunApproval, policy *agenticv1alpha1.ApprovalPolicy, stage agenticv1alpha1.SandboxStep) bool {
	if approval != nil {
		for _, s := range approval.Spec.Stages {
			if string(s.Type) == string(stage) && s.Decision != agenticv1alpha1.ApprovalDecisionDenied {
				return true
			}
		}
	}
	if policy != nil {
		for _, ps := range policy.Spec.Stages {
			if ps.Name == stage && ps.Approval == agenticv1alpha1.ApprovalModeAutomatic {
				return true
			}
		}
	}
	return false
}

func isAutoApprovedByPolicy(policy *agenticv1alpha1.ApprovalPolicy, stage agenticv1alpha1.SandboxStep) bool {
	if policy == nil {
		return false
	}
	for _, ps := range policy.Spec.Stages {
		if ps.Name == stage && ps.Approval == agenticv1alpha1.ApprovalModeAutomatic {
			return true
		}
	}
	return false
}

func isStageDenied(approval *agenticv1alpha1.AgenticRunApproval, stage agenticv1alpha1.SandboxStep) bool {
	if approval == nil {
		return false
	}
	for _, s := range approval.Spec.Stages {
		if string(s.Type) == string(stage) && s.Decision == agenticv1alpha1.ApprovalDecisionDenied {
			return true
		}
	}
	return false
}

func getStageOverrideAgent(approval *agenticv1alpha1.AgenticRunApproval, stage agenticv1alpha1.SandboxStep) string {
	if approval == nil {
		return ""
	}
	for _, s := range approval.Spec.Stages {
		if string(s.Type) != string(stage) {
			continue
		}
		switch stage {
		case agenticv1alpha1.SandboxStepAnalysis:
			if s.Analysis != nil {
				return s.Analysis.Agent
			}
		case agenticv1alpha1.SandboxStepExecution:
			if s.Execution != nil {
				return s.Execution.Agent
			}
		case agenticv1alpha1.SandboxStepVerification:
			if s.Verification != nil {
				return s.Verification.Agent
			}
		case agenticv1alpha1.SandboxStepEscalation:
			if s.Escalation != nil {
				return s.Escalation.Agent
			}
		}
	}
	return ""
}

func getStageOption(approval *agenticv1alpha1.AgenticRunApproval, _ *agenticv1alpha1.ApprovalPolicy) *int32 {
	if approval != nil {
		for _, s := range approval.Spec.Stages {
			if s.Type == agenticv1alpha1.ApprovalStageExecution && s.Execution != nil && s.Execution.Option != nil {
				return s.Execution.Option
			}
		}
	}
	return ptr.To(int32(0))
}
