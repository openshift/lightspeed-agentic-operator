package proposal

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

func getApprovalPolicy(ctx context.Context, c client.Client) (*agenticv1alpha1.ApprovalPolicy, error) {
	policy := &agenticv1alpha1.ApprovalPolicy{}
	err := c.Get(ctx, types.NamespacedName{Name: "cluster"}, policy)
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get ApprovalPolicy: %w", err)
	}
	return policy, nil
}

func getProposalApproval(ctx context.Context, c client.Client, proposal *agenticv1alpha1.Proposal) (*agenticv1alpha1.ProposalApproval, error) {
	approval := &agenticv1alpha1.ProposalApproval{}
	err := c.Get(ctx, types.NamespacedName{Name: proposal.Name, Namespace: proposal.Namespace}, approval)
	if err != nil {
		return nil, err
	}
	return approval, nil
}

func ensureProposalApproval(
	ctx context.Context,
	c client.Client,
	proposal *agenticv1alpha1.Proposal,
	policy *agenticv1alpha1.ApprovalPolicy,
) (*agenticv1alpha1.ProposalApproval, error) {
	existing, err := getProposalApproval(ctx, c, proposal)
	if err == nil {
		return existing, nil
	}
	if !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("get ProposalApproval: %w", err)
	}

	var autoStages []agenticv1alpha1.ApprovalStage
	if policy != nil {
		for _, ps := range policy.Spec.Stages {
			if ps.Approval != agenticv1alpha1.ApprovalModeAutomatic {
				continue
			}
			stage := agenticv1alpha1.ApprovalStage{
				Type: agenticv1alpha1.ApprovalStageType(ps.Name),
			}
			switch ps.Name {
			case agenticv1alpha1.SandboxStepAnalysis:
				stage.Analysis = agenticv1alpha1.AnalysisApproval{}
			case agenticv1alpha1.SandboxStepExecution:
				stage.Execution = agenticv1alpha1.ExecutionApproval{}
			case agenticv1alpha1.SandboxStepVerification:
				stage.Verification = agenticv1alpha1.VerificationApproval{}
			case agenticv1alpha1.SandboxStepEscalation:
				stage.Escalation = agenticv1alpha1.EscalationApproval{}
			}
			autoStages = append(autoStages, stage)
		}
	}

	approval := &agenticv1alpha1.ProposalApproval{
		ObjectMeta: metav1.ObjectMeta{
			Name:      proposal.Name,
			Namespace: proposal.Namespace,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion:         "agentic.openshift.io/v1alpha1",
				Kind:               "Proposal",
				Name:               proposal.Name,
				UID:                proposal.UID,
				Controller:         ptr.To(true),
				BlockOwnerDeletion: ptr.To(true),
			}},
		},
		Spec: agenticv1alpha1.ProposalApprovalSpec{
			Stages: autoStages,
		},
	}

	if err := c.Create(ctx, approval); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return getProposalApproval(ctx, c, proposal)
		}
		return nil, fmt.Errorf("create ProposalApproval: %w", err)
	}
	return approval, nil
}

func isStageApproved(approval *agenticv1alpha1.ProposalApproval, policy *agenticv1alpha1.ApprovalPolicy, stage agenticv1alpha1.SandboxStep) bool {
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

func isStageDenied(approval *agenticv1alpha1.ProposalApproval, stage agenticv1alpha1.SandboxStep) bool {
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

func getStageOverrideAgent(approval *agenticv1alpha1.ProposalApproval, stage agenticv1alpha1.SandboxStep) string {
	if approval == nil {
		return ""
	}
	for _, s := range approval.Spec.Stages {
		if string(s.Type) != string(stage) {
			continue
		}
		switch stage {
		case agenticv1alpha1.SandboxStepAnalysis:
			return s.Analysis.Agent
		case agenticv1alpha1.SandboxStepExecution:
			return s.Execution.Agent
		case agenticv1alpha1.SandboxStepVerification:
			return s.Verification.Agent
		case agenticv1alpha1.SandboxStepEscalation:
			return s.Escalation.Agent
		}
	}
	return ""
}

func getStageOption(approval *agenticv1alpha1.ProposalApproval, policy *agenticv1alpha1.ApprovalPolicy) *int32 {
	if approval != nil {
		for _, s := range approval.Spec.Stages {
			if s.Type == agenticv1alpha1.ApprovalStageExecution && s.Execution.Option != nil {
				return s.Execution.Option
			}
		}
	}
	return ptr.To(int32(0))
}
