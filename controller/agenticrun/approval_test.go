package agenticrun

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
)

func TestEnsureAgenticRunApproval_OwnerReference(t *testing.T) {
	run := testAgenticRun()
	run.UID = "test-uid-123"
	fc := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(run).Build()

	approval, err := ensureAgenticRunApproval(context.Background(), fc, run, nil)
	if err != nil {
		t.Fatalf("ensureAgenticRunApproval: %v", err)
	}

	if len(approval.OwnerReferences) != 1 {
		t.Fatalf("expected 1 owner reference, got %d", len(approval.OwnerReferences))
	}

	ref := approval.OwnerReferences[0]
	if ref.APIVersion != "agentic.openshift.io/v1alpha1" {
		t.Errorf("apiVersion = %q, want agentic.openshift.io/v1alpha1", ref.APIVersion)
	}
	if ref.Kind != "AgenticRun" {
		t.Errorf("kind = %q, want AgenticRun", ref.Kind)
	}
	if ref.Name != run.Name {
		t.Errorf("name = %q, want %q", ref.Name, run.Name)
	}
	if ref.UID != run.UID {
		t.Errorf("uid = %q, want %q", ref.UID, run.UID)
	}
	if ref.Controller == nil || !*ref.Controller {
		t.Error("controller must be true (required for Owns() watch)")
	}
	if ref.BlockOwnerDeletion == nil || !*ref.BlockOwnerDeletion {
		t.Error("blockOwnerDeletion must be true")
	}
}

func TestEnsureAgenticRunApproval_AutoApproveStages(t *testing.T) {
	run := testAgenticRun()
	fc := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(run).Build()
	policy := testAutoApprovePolicy()

	approval, err := ensureAgenticRunApproval(context.Background(), fc, run, policy)
	if err != nil {
		t.Fatalf("ensureAgenticRunApproval: %v", err)
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

func TestEnsureAgenticRunApproval_AnalysisOnly_SkipsAbsentStages(t *testing.T) {
	run := &agenticv1alpha1.AgenticRun{
		ObjectMeta: metav1.ObjectMeta{Name: "advisory", Namespace: "default"},
		Spec: agenticv1alpha1.AgenticRunSpec{
			Request:  "investigate this",
			Analysis: agenticv1alpha1.AgenticRunStep{Agent: "smart"},
		},
	}
	fc := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(run).Build()
	policy := testAutoApprovePolicy() // auto-approves Analysis + Verification

	approval, err := ensureAgenticRunApproval(context.Background(), fc, run, policy)
	if err != nil {
		t.Fatalf("ensureAgenticRunApproval: %v", err)
	}

	for _, s := range approval.Spec.Stages {
		switch s.Type {
		case agenticv1alpha1.ApprovalStageAnalysis:
			if s.Analysis == nil {
				t.Fatal("expected non-nil Analysis arm")
			}
			if s.Analysis.Agent != "" {
				t.Errorf("auto-seeded Analysis should omit agent override, got %q", s.Analysis.Agent)
			}
		case agenticv1alpha1.ApprovalStageVerification:
			t.Error("Verification stage should be skipped for analysis-only run")
		case agenticv1alpha1.ApprovalStageExecution:
			t.Error("Execution stage should be skipped for analysis-only run")
		}
	}
	if len(approval.Spec.Stages) != 1 {
		t.Errorf("expected 1 stage (Analysis only), got %d", len(approval.Spec.Stages))
	}
}

func TestEnsureAgenticRunApproval_NoPolicy(t *testing.T) {
	run := testAgenticRun()
	fc := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(run).Build()

	approval, err := ensureAgenticRunApproval(context.Background(), fc, run, nil)
	if err != nil {
		t.Fatalf("ensureAgenticRunApproval: %v", err)
	}
	if len(approval.Spec.Stages) != 0 {
		t.Errorf("expected 0 stages with nil policy, got %d", len(approval.Spec.Stages))
	}
}

func TestGetStageOverrideAgent_OmittedMeansNoOverride(t *testing.T) {
	approval := &agenticv1alpha1.AgenticRunApproval{
		Spec: agenticv1alpha1.AgenticRunApprovalSpec{
			Stages: []agenticv1alpha1.ApprovalStage{
				agenticv1alpha1.NewApprovalStage(agenticv1alpha1.ApprovalStageAnalysis, "", "", nil, 0),
				agenticv1alpha1.NewApprovalStage(agenticv1alpha1.ApprovalStageExecution, "", "fast", nil, 0),
			},
		},
	}
	if got := getStageOverrideAgent(approval, agenticv1alpha1.SandboxStepAnalysis); got != "" {
		t.Errorf("empty analysis.agent should be no override, got %q", got)
	}
	if got := getStageOverrideAgent(approval, agenticv1alpha1.SandboxStepExecution); got != "fast" {
		t.Errorf("execution agent override = %q, want fast", got)
	}
}

func TestGetStageOption_FromApproval(t *testing.T) {
	option := int32(2)
	approval := &agenticv1alpha1.AgenticRunApproval{
		Spec: agenticv1alpha1.AgenticRunApprovalSpec{
			Stages: []agenticv1alpha1.ApprovalStage{
				{
					Type:      agenticv1alpha1.ApprovalStageExecution,
					Execution: &agenticv1alpha1.ExecutionApproval{Option: &option},
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
	approval := &agenticv1alpha1.AgenticRunApproval{
		Spec: agenticv1alpha1.AgenticRunApprovalSpec{
			Stages: []agenticv1alpha1.ApprovalStage{
				{
					Type:      agenticv1alpha1.ApprovalStageExecution,
					Execution: &agenticv1alpha1.ExecutionApproval{Option: &approvalOpt},
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

func TestEnsureAgenticRunApproval_Idempotent(t *testing.T) {
	run := testAgenticRun()
	run.UID = "test-uid-456"
	fc := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(run).Build()

	policy := &agenticv1alpha1.ApprovalPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster"},
		Spec: agenticv1alpha1.ApprovalPolicySpec{
			Stages: []agenticv1alpha1.ApprovalPolicyStage{
				{Name: agenticv1alpha1.SandboxStepAnalysis, Approval: agenticv1alpha1.ApprovalModeAutomatic},
			},
		},
	}

	first, err := ensureAgenticRunApproval(context.Background(), fc, run, policy)
	if err != nil {
		t.Fatalf("first call: %v", err)
	}

	second, err := ensureAgenticRunApproval(context.Background(), fc, run, policy)
	if err != nil {
		t.Fatalf("second call: %v", err)
	}

	if first.UID != second.UID {
		t.Errorf("second call returned different UID: %q vs %q", first.UID, second.UID)
	}
}
