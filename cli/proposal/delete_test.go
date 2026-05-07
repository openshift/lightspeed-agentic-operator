package proposal

import (
	"context"
	"strings"
	"testing"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestDelete_Success(t *testing.T) {
	streams, out, _ := fakeStreams()
	p := testProposalWithStatus("fix-crash", "default", agenticv1alpha1.ProposalPhaseCompleted)

	fc := fake.NewClientBuilder().WithScheme(testScheme()).
		WithObjects(p).Build()

	o := &DeleteOptions{
		client:    fc,
		name:      "fix-crash",
		namespace: "default",
		IOStreams:  streams,
	}
	if err := o.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if !strings.Contains(out.String(), "deleted") {
		t.Errorf("expected 'deleted' in output, got: %s", out.String())
	}

	// Verify it's gone
	list := &agenticv1alpha1.ProposalList{}
	if err := fc.List(context.Background(), list); err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list.Items) != 0 {
		t.Errorf("expected 0 proposals after delete, got %d", len(list.Items))
	}
}

func TestDelete_NotFound(t *testing.T) {
	streams, _, _ := fakeStreams()
	fc := fake.NewClientBuilder().WithScheme(testScheme()).Build()

	o := &DeleteOptions{
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

func TestDelete_OutputMessage(t *testing.T) {
	streams, out, _ := fakeStreams()
	p := testProposalWithStatus("my-proposal", "default", agenticv1alpha1.ProposalPhasePending)

	fc := fake.NewClientBuilder().WithScheme(testScheme()).
		WithObjects(p).Build()

	o := &DeleteOptions{
		client:    fc,
		name:      "my-proposal",
		namespace: "default",
		IOStreams:  streams,
	}
	if err := o.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if out.String() != "proposal/my-proposal deleted\n" {
		t.Errorf("unexpected output: %q", out.String())
	}
}
