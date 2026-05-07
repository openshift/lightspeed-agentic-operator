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

func TestApprove_AnalysisStage(t *testing.T) {
	streams, out, _ := fakeStreams()
	p := testProposalWithStatus("fix-crash", "default", agenticv1alpha1.ProposalPhasePending)
	approval := &agenticv1alpha1.ProposalApproval{
		ObjectMeta: metav1.ObjectMeta{Name: "fix-crash", Namespace: "default"},
	}

	fc := fake.NewClientBuilder().WithScheme(testScheme()).
		WithObjects(p, approval).WithStatusSubresource(p).Build()

	o := &ApproveOptions{
		client:    fc,
		name:      "fix-crash",
		namespace: "default",
		stage:     "analysis",
		IOStreams:  streams,
	}
	if err := o.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if !strings.Contains(out.String(), "approved") {
		t.Errorf("expected 'approved' in output, got: %s", out.String())
	}
	if !strings.Contains(out.String(), "analysis") {
		t.Errorf("expected 'analysis' in output, got: %s", out.String())
	}

	// Verify the ProposalApproval was patched
	var updated agenticv1alpha1.ProposalApproval
	if err := fc.Get(context.Background(), types.NamespacedName{Name: "fix-crash", Namespace: "default"}, &updated); err != nil {
		t.Fatalf("Get ProposalApproval: %v", err)
	}
	if len(updated.Spec.Stages) != 1 {
		t.Fatalf("expected 1 stage, got %d", len(updated.Spec.Stages))
	}
	if updated.Spec.Stages[0].Type != agenticv1alpha1.ApprovalStageAnalysis {
		t.Errorf("expected Analysis stage, got %s", updated.Spec.Stages[0].Type)
	}
	if updated.Spec.Stages[0].Decision == agenticv1alpha1.ApprovalDecisionDenied {
		t.Error("expected stage to not be denied")
	}
}

func TestApprove_ExecutionWithOption(t *testing.T) {
	streams, out, _ := fakeStreams()
	p := testProposalWithStatus("fix-crash", "default", agenticv1alpha1.ProposalPhaseAnalyzing)
	approval := &agenticv1alpha1.ProposalApproval{
		ObjectMeta: metav1.ObjectMeta{Name: "fix-crash", Namespace: "default"},
	}

	fc := fake.NewClientBuilder().WithScheme(testScheme()).
		WithObjects(p, approval).WithStatusSubresource(p).Build()

	o := &ApproveOptions{
		client:    fc,
		name:      "fix-crash",
		namespace: "default",
		stage:     "execution",
		option:    1,
		IOStreams:  streams,
	}
	if err := o.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
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
	if updated.Spec.Stages[0].Execution.Option == nil || *updated.Spec.Stages[0].Execution.Option != 1 {
		t.Errorf("expected option=1, got %v", updated.Spec.Stages[0].Execution.Option)
	}
}

func TestApprove_ExecutionWithAgent(t *testing.T) {
	streams, _, _ := fakeStreams()
	p := testProposalWithStatus("fix-crash", "default", agenticv1alpha1.ProposalPhasePending)
	approval := &agenticv1alpha1.ProposalApproval{
		ObjectMeta: metav1.ObjectMeta{Name: "fix-crash", Namespace: "default"},
	}

	fc := fake.NewClientBuilder().WithScheme(testScheme()).
		WithObjects(p, approval).WithStatusSubresource(p).Build()

	o := &ApproveOptions{
		client:    fc,
		name:      "fix-crash",
		namespace: "default",
		stage:     "execution",
		agent:     "fast",
		IOStreams:  streams,
	}
	if err := o.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	var updated agenticv1alpha1.ProposalApproval
	if err := fc.Get(context.Background(), types.NamespacedName{Name: "fix-crash", Namespace: "default"}, &updated); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if updated.Spec.Stages[0].Execution.Agent != "fast" {
		t.Errorf("expected agent=fast, got %s", updated.Spec.Stages[0].Execution.Agent)
	}
}

func TestApprove_AllStages(t *testing.T) {
	streams, out, _ := fakeStreams()
	p := testProposal("fix-crash", "default")
	p.Spec.Execution = agenticv1alpha1.ProposalStep{Agent: "default"}
	p.Spec.Verification = agenticv1alpha1.ProposalStep{Agent: "default"}
	approval := &agenticv1alpha1.ProposalApproval{
		ObjectMeta: metav1.ObjectMeta{Name: "fix-crash", Namespace: "default"},
	}

	fc := fake.NewClientBuilder().WithScheme(testScheme()).
		WithObjects(p, approval).WithStatusSubresource(p).Build()

	o := &ApproveOptions{
		client:    fc,
		name:      "fix-crash",
		namespace: "default",
		all:       true,
		IOStreams:  streams,
	}
	if err := o.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	output := out.String()
	if !strings.Contains(output, "analysis approved") {
		t.Error("expected analysis approval in output")
	}
	if !strings.Contains(output, "execution approved") {
		t.Error("expected execution approval in output")
	}
	if !strings.Contains(output, "verification approved") {
		t.Error("expected verification approval in output")
	}

	var updated agenticv1alpha1.ProposalApproval
	if err := fc.Get(context.Background(), types.NamespacedName{Name: "fix-crash", Namespace: "default"}, &updated); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(updated.Spec.Stages) != 3 {
		t.Errorf("expected 3 stages, got %d", len(updated.Spec.Stages))
	}
}

func TestApprove_AlreadyApproved(t *testing.T) {
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

	o := &ApproveOptions{
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

func TestApprove_AlreadyDenied(t *testing.T) {
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

	o := &ApproveOptions{
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

func TestApprove_CreatesApprovalIfMissing(t *testing.T) {
	streams, out, _ := fakeStreams()
	p := testProposalWithStatus("fix-crash", "default", agenticv1alpha1.ProposalPhasePending)

	fc := fake.NewClientBuilder().WithScheme(testScheme()).
		WithObjects(p).WithStatusSubresource(p).Build()

	o := &ApproveOptions{
		client:    fc,
		name:      "fix-crash",
		namespace: "default",
		stage:     "analysis",
		IOStreams:  streams,
	}
	if err := o.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if !strings.Contains(out.String(), "approved") {
		t.Errorf("expected 'approved' in output, got: %s", out.String())
	}

	// Verify ProposalApproval was created
	var approval agenticv1alpha1.ProposalApproval
	if err := fc.Get(context.Background(), types.NamespacedName{Name: "fix-crash", Namespace: "default"}, &approval); err != nil {
		t.Fatalf("ProposalApproval should have been created: %v", err)
	}
	if len(approval.Spec.Stages) != 1 {
		t.Fatalf("expected 1 stage, got %d", len(approval.Spec.Stages))
	}
}

func TestApprove_NotFound(t *testing.T) {
	streams, _, _ := fakeStreams()
	fc := fake.NewClientBuilder().WithScheme(testScheme()).Build()

	o := &ApproveOptions{
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

func TestApprove_Validate(t *testing.T) {
	tests := []struct {
		name    string
		opts    ApproveOptions
		wantErr bool
		errMsg  string
	}{
		{
			name:    "no stage or all",
			opts:    ApproveOptions{option: 0},
			wantErr: true,
			errMsg:  "--stage is required",
		},
		{
			name:    "valid stage analysis",
			opts:    ApproveOptions{stage: "analysis"},
			wantErr: false,
		},
		{
			name:    "valid stage execution",
			opts:    ApproveOptions{stage: "execution"},
			wantErr: false,
		},
		{
			name:    "valid stage verification",
			opts:    ApproveOptions{stage: "verification"},
			wantErr: false,
		},
		{
			name:    "invalid stage",
			opts:    ApproveOptions{stage: "invalid"},
			wantErr: true,
			errMsg:  "must be analysis, execution, or verification",
		},
		{
			name:    "all flag",
			opts:    ApproveOptions{all: true},
			wantErr: false,
		},
		{
			name:    "negative option",
			opts:    ApproveOptions{stage: "execution", option: -1},
			wantErr: true,
			errMsg:  "--option must be >= 0",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.opts.Validate()
			if (err != nil) != tc.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.wantErr && tc.errMsg != "" && err != nil {
				if !strings.Contains(err.Error(), tc.errMsg) {
					t.Errorf("expected error to contain %q, got: %v", tc.errMsg, err)
				}
			}
		})
	}
}

func TestApprove_AllNoPending(t *testing.T) {
	streams, _, _ := fakeStreams()
	p := testProposal("fix-crash", "default")
	// Only analysis step (no execution, no verification)
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

	o := &ApproveOptions{
		client:    fc,
		name:      "fix-crash",
		namespace: "default",
		all:       true,
		IOStreams:  streams,
	}
	err := o.Run(context.Background())
	if err == nil {
		t.Fatal("expected error when no pending stages")
	}
	if !strings.Contains(err.Error(), "no pending stages") {
		t.Errorf("error should mention 'no pending stages', got: %v", err)
	}
}
