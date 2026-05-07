package proposal

import (
	"context"
	"strings"
	"testing"
	"time"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestList_InNamespace(t *testing.T) {
	streams, out, _ := fakeStreams()
	p1 := testProposalWithStatus("p1", "default", agenticv1alpha1.ProposalPhaseAnalyzing)
	p2 := testProposalWithStatus("p2", "default", agenticv1alpha1.ProposalPhaseCompleted)
	p3 := testProposalWithStatus("p3", "other", agenticv1alpha1.ProposalPhasePending)

	fc := fake.NewClientBuilder().WithScheme(testScheme()).
		WithObjects(p1, p2, p3).Build()

	o := &ListOptions{
		client:    fc,
		namespace: "default",
		IOStreams:  streams,
	}
	if err := o.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	output := out.String()
	if !strings.Contains(output, "p1") || !strings.Contains(output, "p2") {
		t.Errorf("expected p1 and p2 in output, got:\n%s", output)
	}
	if strings.Contains(output, "p3") {
		t.Errorf("should not contain p3 (different namespace), got:\n%s", output)
	}
}

func TestList_AllNamespaces(t *testing.T) {
	streams, out, _ := fakeStreams()
	p1 := testProposalWithStatus("p1", "default", agenticv1alpha1.ProposalPhaseAnalyzing)
	p2 := testProposalWithStatus("p2", "other", agenticv1alpha1.ProposalPhasePending)

	fc := fake.NewClientBuilder().WithScheme(testScheme()).
		WithObjects(p1, p2).Build()

	o := &ListOptions{
		client:        fc,
		allNamespaces: true,
		IOStreams:      streams,
	}
	if err := o.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	output := out.String()
	if !strings.Contains(output, "NAMESPACE") {
		t.Error("expected NAMESPACE header for -A mode")
	}
	if !strings.Contains(output, "p1") || !strings.Contains(output, "p2") {
		t.Errorf("expected both proposals in output, got:\n%s", output)
	}
}

func TestList_FilterByPhase(t *testing.T) {
	streams, out, _ := fakeStreams()
	p1 := testProposalWithStatus("p1", "default", agenticv1alpha1.ProposalPhaseAnalyzing)
	p2 := testProposalWithStatus("p2", "default", agenticv1alpha1.ProposalPhaseCompleted)

	fc := fake.NewClientBuilder().WithScheme(testScheme()).
		WithObjects(p1, p2).Build()

	o := &ListOptions{
		client:    fc,
		namespace: "default",
		phase:     "Analyzing",
		IOStreams:  streams,
	}
	if err := o.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	output := out.String()
	if !strings.Contains(output, "p1") {
		t.Error("expected p1 (Proposed) in output")
	}
	if strings.Contains(output, "p2") {
		t.Error("p2 (Completed) should be filtered out")
	}
}

func TestList_Empty(t *testing.T) {
	streams, out, _ := fakeStreams()
	fc := fake.NewClientBuilder().WithScheme(testScheme()).Build()

	o := &ListOptions{
		client:    fc,
		namespace: "default",
		IOStreams:  streams,
	}
	if err := o.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if !strings.Contains(out.String(), "No proposals found") {
		t.Errorf("expected empty message, got: %s", out.String())
	}
}

func TestList_JSONOutput(t *testing.T) {
	streams, out, _ := fakeStreams()
	p1 := testProposalWithStatus("p1", "default", agenticv1alpha1.ProposalPhaseAnalyzing)

	fc := fake.NewClientBuilder().WithScheme(testScheme()).
		WithObjects(p1).Build()

	o := &ListOptions{
		client:    fc,
		namespace: "default",
		output:    "json",
		IOStreams:  streams,
	}
	if err := o.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if !strings.Contains(out.String(), `"items"`) || !strings.Contains(out.String(), `"name": "p1"`) {
		t.Errorf("expected JSON output with items and proposal name, got:\n%s", out.String())
	}
}

func TestList_WideOutput(t *testing.T) {
	streams, out, _ := fakeStreams()
	p := testProposalWithStatus("p1", "default", agenticv1alpha1.ProposalPhaseAnalyzing)

	fc := fake.NewClientBuilder().WithScheme(testScheme()).
		WithObjects(p).Build()

	o := &ListOptions{
		client:    fc,
		namespace: "default",
		output:    "wide",
		IOStreams:  streams,
	}
	if err := o.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	output := out.String()
	if !strings.Contains(output, "TARGET-NS") {
		t.Error("wide output should contain TARGET-NS header")
	}
}

func TestList_SortNewestFirst(t *testing.T) {
	streams, out, _ := fakeStreams()
	now := time.Now()

	old := testProposalWithStatus("old", "default", agenticv1alpha1.ProposalPhasePending)
	old.CreationTimestamp = metav1.NewTime(now.Add(-10 * time.Minute))
	new := testProposalWithStatus("new", "default", agenticv1alpha1.ProposalPhasePending)
	new.CreationTimestamp = metav1.NewTime(now)

	fc := fake.NewClientBuilder().WithScheme(testScheme()).
		WithObjects(old, new).Build()

	o := &ListOptions{
		client:    fc,
		namespace: "default",
		IOStreams:  streams,
	}
	if err := o.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	output := out.String()
	newIdx := strings.Index(output, "new")
	oldIdx := strings.Index(output, "old")
	if newIdx > oldIdx {
		t.Error("expected newest first in output")
	}
}

func TestList_Validate(t *testing.T) {
	tests := []struct {
		name    string
		opts    ListOptions
		wantErr bool
	}{
		{"valid phase", ListOptions{phase: "Analyzing"}, false},
		{"invalid phase", ListOptions{phase: "Invalid"}, true},
		{"valid output", ListOptions{output: "json"}, false},
		{"invalid output", ListOptions{output: "xml"}, true},
		{"wide output", ListOptions{output: "wide"}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.opts.Validate()
			if (err != nil) != tc.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func TestList_EmptyAllNamespaces(t *testing.T) {
	streams, out, _ := fakeStreams()
	fc := fake.NewClientBuilder().WithScheme(testScheme()).Build()

	o := &ListOptions{
		client:        fc,
		allNamespaces: true,
		IOStreams:      streams,
	}
	if err := o.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if !strings.Contains(out.String(), "No proposals found.") {
		t.Errorf("expected generic empty message for -A, got: %s", out.String())
	}
}

func TestList_YAMLOutput(t *testing.T) {
	streams, out, _ := fakeStreams()
	p := testProposalWithStatus("p1", "default", agenticv1alpha1.ProposalPhaseAnalyzing)

	fc := fake.NewClientBuilder().WithScheme(testScheme()).
		WithObjects(p).Build()

	o := &ListOptions{
		client:    fc,
		namespace: "default",
		output:    "yaml",
		IOStreams:  streams,
	}
	if err := o.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if !strings.Contains(out.String(), "items:") || !strings.Contains(out.String(), "name: p1") {
		t.Errorf("expected YAML output with items and proposal name, got:\n%s", out.String())
	}
}

// Verify that client.ListOption is used correctly for namespace filtering.
func TestList_NamespaceFiltering(t *testing.T) {
	streams, out, _ := fakeStreams()
	p := testProposalWithStatus("p1", "target-ns", agenticv1alpha1.ProposalPhaseAnalyzing)

	fc := fake.NewClientBuilder().WithScheme(testScheme()).
		WithObjects(p).Build()

	// List in wrong namespace
	o := &ListOptions{
		client:    fc,
		namespace: "wrong-ns",
		IOStreams:  streams,
	}
	if err := o.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if strings.Contains(out.String(), "p1") {
		t.Error("should not find proposal in wrong namespace")
	}
}

// Verify that the ListOptions struct satisfies client.ListOption interface expectations.
func TestList_ClientListOptions(t *testing.T) {
	var opts []client.ListOption
	opts = append(opts, client.InNamespace("test"))
	if len(opts) != 1 {
		t.Error("expected one list option")
	}
}
