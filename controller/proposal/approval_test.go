package proposal

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
)

func TestEnsureProposalApproval_OwnerReference(t *testing.T) {
	proposal := testProposal()
	proposal.UID = "test-uid-123"
	fc := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(proposal).Build()

	approval, err := ensureProposalApproval(context.Background(), fc, proposal, nil)
	if err != nil {
		t.Fatalf("ensureProposalApproval: %v", err)
	}

	if len(approval.OwnerReferences) != 1 {
		t.Fatalf("expected 1 owner reference, got %d", len(approval.OwnerReferences))
	}

	ref := approval.OwnerReferences[0]
	if ref.APIVersion != "agentic.openshift.io/v1alpha1" {
		t.Errorf("apiVersion = %q, want agentic.openshift.io/v1alpha1", ref.APIVersion)
	}
	if ref.Kind != "Proposal" {
		t.Errorf("kind = %q, want Proposal", ref.Kind)
	}
	if ref.Name != proposal.Name {
		t.Errorf("name = %q, want %q", ref.Name, proposal.Name)
	}
	if ref.UID != proposal.UID {
		t.Errorf("uid = %q, want %q", ref.UID, proposal.UID)
	}
	if ref.Controller == nil || !*ref.Controller {
		t.Error("controller must be true (required for Owns() watch)")
	}
	if ref.BlockOwnerDeletion == nil || !*ref.BlockOwnerDeletion {
		t.Error("blockOwnerDeletion must be true")
	}
}

func TestEnsureProposalApproval_AutoApproveStages(t *testing.T) {
	proposal := testProposal()
	fc := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(proposal).Build()
	policy := testAutoApprovePolicy()

	approval, err := ensureProposalApproval(context.Background(), fc, proposal, policy)
	if err != nil {
		t.Fatalf("ensureProposalApproval: %v", err)
	}

	hasAnalysis, hasVerification := false, false
	for _, s := range approval.Spec.Stages {
		switch s.Type {
		case agenticv1alpha1.ApprovalStageAnalysis:
			hasAnalysis = true
		case agenticv1alpha1.ApprovalStageVerification:
			hasVerification = true
		case agenticv1alpha1.ApprovalStageExecution:
			t.Error("Execution should not be auto-approved by testAutoApprovePolicy")
		}
	}
	if !hasAnalysis {
		t.Error("expected auto-approved Analysis stage")
	}
	if !hasVerification {
		t.Error("expected auto-approved Verification stage")
	}
}

func TestEnsureProposalApproval_AnalysisOnly_SkipsAbsentStages(t *testing.T) {
	proposal := &agenticv1alpha1.Proposal{
		ObjectMeta: metav1.ObjectMeta{Name: "advisory", Namespace: "default"},
		Spec: agenticv1alpha1.ProposalSpec{
			Request:  "investigate this",
			Analysis: agenticv1alpha1.ProposalStep{Agent: "smart"},
		},
	}
	fc := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(proposal).Build()
	policy := testAutoApprovePolicy() // auto-approves Analysis + Verification

	approval, err := ensureProposalApproval(context.Background(), fc, proposal, policy)
	if err != nil {
		t.Fatalf("ensureProposalApproval: %v", err)
	}

	for _, s := range approval.Spec.Stages {
		switch s.Type {
		case agenticv1alpha1.ApprovalStageAnalysis:
			if s.Analysis.Agent != "smart" {
				t.Errorf("expected Analysis agent %q, got %q", "smart", s.Analysis.Agent)
			}
		case agenticv1alpha1.ApprovalStageVerification:
			t.Error("Verification stage should be skipped for analysis-only proposal")
		case agenticv1alpha1.ApprovalStageExecution:
			t.Error("Execution stage should be skipped for analysis-only proposal")
		}
	}
	if len(approval.Spec.Stages) != 1 {
		t.Errorf("expected 1 stage (Analysis only), got %d", len(approval.Spec.Stages))
	}
}

func TestEnsureProposalApproval_NoPolicy(t *testing.T) {
	proposal := testProposal()
	fc := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(proposal).Build()

	approval, err := ensureProposalApproval(context.Background(), fc, proposal, nil)
	if err != nil {
		t.Fatalf("ensureProposalApproval: %v", err)
	}
	if len(approval.Spec.Stages) != 0 {
		t.Errorf("expected 0 stages with nil policy, got %d", len(approval.Spec.Stages))
	}
}

func TestGetStageOption_FromApproval(t *testing.T) {
	option := int32(2)
	approval := &agenticv1alpha1.ProposalApproval{
		Spec: agenticv1alpha1.ProposalApprovalSpec{
			Stages: []agenticv1alpha1.ApprovalStage{
				{
					Type:      agenticv1alpha1.ApprovalStageExecution,
					Execution: agenticv1alpha1.ExecutionApproval{Option: &option},
				},
			},
		},
	}
	got := getStageOption(approval, nil)
	if got == nil || *got != 2 {
		t.Errorf("expected option 2 from approval, got %v", got)
	}
}

func TestGetStageOption_ApprovalTakesPrecedence(t *testing.T) {
	approvalOpt := int32(2)
	approval := &agenticv1alpha1.ProposalApproval{
		Spec: agenticv1alpha1.ProposalApprovalSpec{
			Stages: []agenticv1alpha1.ApprovalStage{
				{
					Type:      agenticv1alpha1.ApprovalStageExecution,
					Execution: agenticv1alpha1.ExecutionApproval{Option: &approvalOpt},
				},
			},
		},
	}
	got := getStageOption(approval, nil)
	if got == nil || *got != 2 {
		t.Errorf("expected option from approval, expected 2, got %v", got)
	}
}

func TestGetStageOption_FallbackToZero(t *testing.T) {
	got := getStageOption(nil, nil)
	if got == nil || *got != 0 {
		t.Errorf("expected fallback to 0, got %v", got)
	}
}

func TestEnsureProposalApproval_Idempotent(t *testing.T) {
	proposal := testProposal()
	proposal.UID = "test-uid-456"
	fc := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(proposal).Build()

	policy := &agenticv1alpha1.ApprovalPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster"},
		Spec: agenticv1alpha1.ApprovalPolicySpec{
			Stages: []agenticv1alpha1.ApprovalPolicyStage{
				{Name: agenticv1alpha1.SandboxStepAnalysis, Approval: agenticv1alpha1.ApprovalModeAutomatic},
			},
		},
	}

	first, err := ensureProposalApproval(context.Background(), fc, proposal, policy)
	if err != nil {
		t.Fatalf("first call: %v", err)
	}

	second, err := ensureProposalApproval(context.Background(), fc, proposal, policy)
	if err != nil {
		t.Fatalf("second call: %v", err)
	}

	if first.UID != second.UID {
		t.Errorf("second call returned different UID: %q vs %q", first.UID, second.UID)
	}
}
