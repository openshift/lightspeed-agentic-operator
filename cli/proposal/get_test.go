package proposal

import (
	"context"
	"strings"
	"testing"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestGet_BasicDetail(t *testing.T) {
	streams, out, _ := fakeStreams()
	p := testProposalWithStatus("fix-crash", "default", agenticv1alpha1.ProposalPhasePending)

	fc := fake.NewClientBuilder().WithScheme(testScheme()).
		WithObjects(p).Build()

	o := &GetOptions{
		client:    fc,
		name:      "fix-crash",
		namespace: "default",
		IOStreams:  streams,
	}
	if err := o.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	output := out.String()
	for _, want := range []string{"Name:", "fix-crash", "Namespace:", "default", "Phase:", "Pending"} {
		if !strings.Contains(output, want) {
			t.Errorf("expected %q in output, got:\n%s", want, output)
		}
	}
}

func TestGet_WithAnalysisResults(t *testing.T) {
	streams, out, _ := fakeStreams()
	p := testProposalWithStatus("fix-crash", "default", agenticv1alpha1.ProposalPhaseExecuting)
	p.Status.Steps.Analysis = agenticv1alpha1.AnalysisStepStatus{
		Conditions: []metav1.Condition{
			{Type: "Analyzed", Status: metav1.ConditionTrue, Reason: "Success", LastTransitionTime: metav1.Now()},
		},
		Results: []agenticv1alpha1.StepResultRef{{Name: "fix-crash-analysis-1", Outcome: agenticv1alpha1.ActionOutcomeSucceeded}},
	}

	fc := fake.NewClientBuilder().WithScheme(testScheme()).
		WithObjects(p).Build()

	o := &GetOptions{
		client:    fc,
		name:      "fix-crash",
		namespace: "default",
		IOStreams:  streams,
	}
	if err := o.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	output := out.String()
	if !strings.Contains(output, "fix-crash-analysis-1") {
		t.Error("expected result ref name in output")
	}
}

func TestGet_WithExecutionResults(t *testing.T) {
	streams, out, _ := fakeStreams()
	p := testProposalWithStatus("fix-crash", "default", agenticv1alpha1.ProposalPhaseExecuting)
	p.Status.Steps.Execution = agenticv1alpha1.ExecutionStepStatus{
		Conditions: []metav1.Condition{
			{Type: "Executed", Status: metav1.ConditionUnknown, Reason: "InProgress", LastTransitionTime: metav1.Now()},
		},
		Results: []agenticv1alpha1.StepResultRef{{Name: "fix-crash-execution-1", Outcome: agenticv1alpha1.ActionOutcomeSucceeded}},
	}

	fc := fake.NewClientBuilder().WithScheme(testScheme()).
		WithObjects(p).Build()

	o := &GetOptions{
		client:    fc,
		name:      "fix-crash",
		namespace: "default",
		IOStreams:  streams,
	}
	if err := o.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	output := out.String()
	if !strings.Contains(output, "fix-crash-execution-1") {
		t.Error("expected execution result ref in output")
	}
}

func TestGet_WithVerificationResults(t *testing.T) {
	streams, out, _ := fakeStreams()
	p := testProposalWithStatus("fix-crash", "default", agenticv1alpha1.ProposalPhaseVerifying)
	p.Status.Steps.Verification = agenticv1alpha1.VerificationStepStatus{
		Conditions: []metav1.Condition{
			{Type: "Verified", Status: metav1.ConditionTrue, Reason: "AllPassed", LastTransitionTime: metav1.Now()},
		},
		Results: []agenticv1alpha1.StepResultRef{{Name: "fix-crash-verification-1", Outcome: agenticv1alpha1.ActionOutcomeSucceeded}},
	}

	fc := fake.NewClientBuilder().WithScheme(testScheme()).
		WithObjects(p).Build()

	o := &GetOptions{
		client:    fc,
		name:      "fix-crash",
		namespace: "default",
		IOStreams:  streams,
	}
	if err := o.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	output := out.String()
	if !strings.Contains(output, "fix-crash-verification-1") {
		t.Error("expected verification result ref in output")
	}
}

func TestGet_WithConditions(t *testing.T) {
	streams, out, _ := fakeStreams()
	p := testProposalWithStatus("fix-crash", "default", agenticv1alpha1.ProposalPhaseCompleted)
	p.Status.Conditions = []metav1.Condition{
		{Type: "Analyzed", Status: metav1.ConditionTrue, Reason: "Success", LastTransitionTime: metav1.Now()},
		{Type: "Approved", Status: metav1.ConditionTrue, Reason: "UserApproved", LastTransitionTime: metav1.Now()},
	}

	fc := fake.NewClientBuilder().WithScheme(testScheme()).
		WithObjects(p).Build()

	o := &GetOptions{
		client:    fc,
		name:      "fix-crash",
		namespace: "default",
		IOStreams:  streams,
	}
	if err := o.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	output := out.String()
	if !strings.Contains(output, "Conditions:") {
		t.Error("expected conditions section")
	}
	if !strings.Contains(output, "Analyzed") {
		t.Error("expected Analyzed condition")
	}
}

func TestGet_JSONOutput(t *testing.T) {
	streams, out, _ := fakeStreams()
	p := testProposalWithStatus("fix-crash", "default", agenticv1alpha1.ProposalPhasePending)

	fc := fake.NewClientBuilder().WithScheme(testScheme()).
		WithObjects(p).Build()

	o := &GetOptions{
		client:    fc,
		name:      "fix-crash",
		namespace: "default",
		output:    "json",
		IOStreams:  streams,
	}
	if err := o.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if !strings.Contains(out.String(), `"request"`) || !strings.Contains(out.String(), `"fix-crash"`) {
		t.Errorf("expected JSON output with proposal fields, got:\n%s", out.String())
	}
}

func TestGet_NotFound(t *testing.T) {
	streams, _, _ := fakeStreams()
	fc := fake.NewClientBuilder().WithScheme(testScheme()).Build()

	o := &GetOptions{
		client:    fc,
		name:      "nonexistent",
		namespace: "default",
		IOStreams:  streams,
	}
	err := o.Run(context.Background())
	if err == nil {
		t.Fatal("expected error for nonexistent proposal")
	}
	if !strings.Contains(err.Error(), "nonexistent") {
		t.Errorf("error should mention proposal name, got: %v", err)
	}
}

func TestGet_StepStatusFromConditions(t *testing.T) {
	streams, out, _ := fakeStreams()
	p := testProposalWithStatus("fix-crash", "default", agenticv1alpha1.ProposalPhaseAnalyzing)
	p.Status.Steps.Analysis = agenticv1alpha1.AnalysisStepStatus{
		Conditions: []metav1.Condition{
			{Type: "Analyzed", Status: metav1.ConditionUnknown, Reason: "InProgress", LastTransitionTime: metav1.Now()},
		},
	}

	fc := fake.NewClientBuilder().WithScheme(testScheme()).
		WithObjects(p).Build()

	o := &GetOptions{
		client:    fc,
		name:      "fix-crash",
		namespace: "default",
		IOStreams:  streams,
	}
	if err := o.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	output := out.String()
	if !strings.Contains(output, "Unknown") {
		t.Error("expected Unknown status for in-progress analysis")
	}
	if !strings.Contains(output, "InProgress") {
		t.Error("expected InProgress reason")
	}
}

func TestGet_NoStepConditions(t *testing.T) {
	streams, out, _ := fakeStreams()
	p := testProposalWithStatus("fix-crash", "default", agenticv1alpha1.ProposalPhasePending)

	fc := fake.NewClientBuilder().WithScheme(testScheme()).
		WithObjects(p).Build()

	o := &GetOptions{
		client:    fc,
		name:      "fix-crash",
		namespace: "default",
		IOStreams:  streams,
	}
	if err := o.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	output := out.String()
	// When no conditions, step status should show "-"
	if !strings.Contains(output, "Analysis:          -") {
		t.Errorf("expected '-' for analysis with no conditions, got:\n%s", output)
	}
}
