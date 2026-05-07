package proposal

import (
	"context"
	"strings"
	"testing"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestDeny_ExplicitStage(t *testing.T) {
	streams, out, _ := fakeStreams()
	p := testProposalWithStatus("fix-crash", "default", agenticv1alpha1.ProposalPhasePending)
	approval := &agenticv1alpha1.ProposalApproval{
		ObjectMeta: metav1.ObjectMeta{Name: "fix-crash", Namespace: "default"},
	}

	fc := fake.NewClientBuilder().WithScheme(testScheme()).
		WithObjects(p, approval).WithStatusSubresource(p).Build()

	o := &DenyOptions{
		client:    fc,
		name:      "fix-crash",
		namespace: "default",
		stage:     "execution",
		IOStreams:  streams,
	}
	if err := o.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if !strings.Contains(out.String(), "denied") {
		t.Errorf("expected 'denied' in output, got: %s", out.String())
	}
	if !strings.Contains(out.String(), "execution") {
		t.Errorf("expected 'execution' in output, got: %s", out.String())
	}

	var updated agenticv1alpha1.ProposalApproval
	if err := fc.Get(context.Background(), types.NamespacedName{Name: "fix-crash", Namespace: "default"}, &updated); err != nil {
		t.Fatalf("Get ProposalApproval: %v", err)
	}
	if len(updated.Spec.Stages) != 1 {
		t.Fatalf("expected 1 stage, got %d", len(updated.Spec.Stages))
	}
	if updated.Spec.Stages[0].Type != agenticv1alpha1.ApprovalStageExecution {
		t.Errorf("expected Execution stage, got %s", updated.Spec.Stages[0].Type)
	}
	if updated.Spec.Stages[0].Decision != agenticv1alpha1.ApprovalDecisionDenied {
		t.Error("expected stage to be denied")
	}
}

func TestDeny_NextPendingStage(t *testing.T) {
	streams, out, _ := fakeStreams()
	p := testProposal("fix-crash", "default")
	p.Spec.Execution = agenticv1alpha1.ProposalStep{Agent: "default"}
	// Analysis already approved, so next pending is execution
	approval := &agenticv1alpha1.ProposalApproval{
		ObjectMeta: metav1.ObjectMeta{Name: "fix-crash", Namespace: "default"},
		Spec: agenticv1alpha1.ProposalApprovalSpec{
			Stages: []agenticv1alpha1.ApprovalStage{
				{Type: agenticv1alpha1.ApprovalStageAnalysis, Analysis: agenticv1alpha1.AnalysisApproval{}},
			},
		},
	}

	fc := fake.NewClientBuilder().WithScheme(testScheme()).
		WithObjects(p, approval).WithStatusSubresource(p).Build()

	o := &DenyOptions{
		client:    fc,
		name:      "fix-crash",
		namespace: "default",
		// no stage specified — should pick next pending
		IOStreams: streams,
	}
	if err := o.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if !strings.Contains(out.String(), "execution") {
		t.Errorf("expected 'execution' in output, got: %s", out.String())
	}
}

func TestDeny_AlreadyDenied(t *testing.T) {
	streams, _, _ := fakeStreams()
	p := testProposalWithStatus("fix-crash", "default", agenticv1alpha1.ProposalPhasePending)
	approval := &agenticv1alpha1.ProposalApproval{
		ObjectMeta: metav1.ObjectMeta{Name: "fix-crash", Namespace: "default"},
		Spec: agenticv1alpha1.ProposalApprovalSpec{
			Stages: []agenticv1alpha1.ApprovalStage{
				{Type: agenticv1alpha1.ApprovalStageAnalysis, Decision: agenticv1alpha1.ApprovalDecisionDenied, Analysis: agenticv1alpha1.AnalysisApproval{}},
			},
		},
	}

	fc := fake.NewClientBuilder().WithScheme(testScheme()).
		WithObjects(p, approval).WithStatusSubresource(p).Build()

	o := &DenyOptions{
		client:    fc,
		name:      "fix-crash",
		namespace: "default",
		stage:     "analysis",
		IOStreams:  streams,
	}
	err := o.Run(context.Background())
	if err == nil {
		t.Fatal("expected error for already denied stage")
	}
	if !strings.Contains(err.Error(), "already denied") {
		t.Errorf("error should mention 'already denied', got: %v", err)
	}
}

func TestDeny_AlreadyApprovedCannotDeny(t *testing.T) {
	streams, _, _ := fakeStreams()
	p := testProposalWithStatus("fix-crash", "default", agenticv1alpha1.ProposalPhasePending)
	approval := &agenticv1alpha1.ProposalApproval{
		ObjectMeta: metav1.ObjectMeta{Name: "fix-crash", Namespace: "default"},
		Spec: agenticv1alpha1.ProposalApprovalSpec{
			Stages: []agenticv1alpha1.ApprovalStage{
				{Type: agenticv1alpha1.ApprovalStageAnalysis, Analysis: agenticv1alpha1.AnalysisApproval{}},
			},
		},
	}

	fc := fake.NewClientBuilder().WithScheme(testScheme()).
		WithObjects(p, approval).WithStatusSubresource(p).Build()

	o := &DenyOptions{
		client:    fc,
		name:      "fix-crash",
		namespace: "default",
		stage:     "analysis",
		IOStreams:  streams,
	}
	err := o.Run(context.Background())
	if err == nil {
		t.Fatal("expected error for already approved stage")
	}
	if !strings.Contains(err.Error(), "already approved") {
		t.Errorf("error should mention 'already approved', got: %v", err)
	}
}

func TestDeny_NoPendingStages(t *testing.T) {
	streams, _, _ := fakeStreams()
	p := testProposal("fix-crash", "default")
	// Only analysis step, already decided
	approval := &agenticv1alpha1.ProposalApproval{
		ObjectMeta: metav1.ObjectMeta{Name: "fix-crash", Namespace: "default"},
		Spec: agenticv1alpha1.ProposalApprovalSpec{
			Stages: []agenticv1alpha1.ApprovalStage{
				{Type: agenticv1alpha1.ApprovalStageAnalysis, Analysis: agenticv1alpha1.AnalysisApproval{}},
			},
		},
	}

	fc := fake.NewClientBuilder().WithScheme(testScheme()).
		WithObjects(p, approval).WithStatusSubresource(p).Build()

	o := &DenyOptions{
		client:    fc,
		name:      "fix-crash",
		namespace: "default",
		// no stage — will try next pending, but all are decided
		IOStreams: streams,
	}
	err := o.Run(context.Background())
	if err == nil {
		t.Fatal("expected error when no pending stages to deny")
	}
	if !strings.Contains(err.Error(), "no pending stages") {
		t.Errorf("error should mention 'no pending stages', got: %v", err)
	}
}

func TestDeny_NotFound(t *testing.T) {
	streams, _, _ := fakeStreams()
	fc := fake.NewClientBuilder().WithScheme(testScheme()).Build()

	o := &DenyOptions{
		client:    fc,
		name:      "nonexistent",
		namespace: "default",
		stage:     "analysis",
		IOStreams:  streams,
	}
	err := o.Run(context.Background())
	if err == nil {
		t.Fatal("expected error for nonexistent proposal")
	}
}
