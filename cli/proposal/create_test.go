package proposal

import (
	"context"
	"strings"
	"testing"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestCreate_Success(t *testing.T) {
	streams, out, _ := fakeStreams()
	fc := fake.NewClientBuilder().WithScheme(testScheme()).Build()

	o := &CreateOptions{
		client:    fc,
		namespace: "default",
		agent:     "default",
		request:   "Pod crashing",
		IOStreams:  streams,
	}
	if err := o.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if !strings.Contains(out.String(), "created") {
		t.Errorf("expected 'created' in output, got: %s", out.String())
	}
}

func TestCreate_GenerateNamePrefix(t *testing.T) {
	fc := fake.NewClientBuilder().WithScheme(testScheme()).Build()

	o := &CreateOptions{
		client:    fc,
		namespace: "default",
		agent:     "default",
		request:   "Pod crashing",
	}

	streams, out, _ := fakeStreams()
	o.IOStreams = streams

	if err := o.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	output := out.String()
	if !strings.Contains(output, "proposal/") {
		t.Errorf("expected proposal/ prefix in output, got: %s", output)
	}
}

func TestCreate_WithTargetNamespaces(t *testing.T) {
	streams, _, _ := fakeStreams()
	fc := fake.NewClientBuilder().WithScheme(testScheme()).Build()

	o := &CreateOptions{
		client:           fc,
		namespace:        "default",
		agent:            "smart",
		request:          "Pod crashing",
		targetNamespaces: []string{"prod", "staging"},
		IOStreams:         streams,
	}
	if err := o.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	list := &agenticv1alpha1.ProposalList{}
	if err := fc.List(context.Background(), list); err != nil {
		t.Fatalf("List: %v", err)
	}
	ns := list.Items[0].Spec.TargetNamespaces
	if len(ns) != 2 || ns[0] != "prod" || ns[1] != "staging" {
		t.Errorf("expected target namespaces [prod, staging], got %v", ns)
	}
}

func TestCreate_JSONOutput(t *testing.T) {
	streams, out, _ := fakeStreams()
	fc := fake.NewClientBuilder().WithScheme(testScheme()).Build()

	o := &CreateOptions{
		client:      fc,
		namespace:   "default",
		agent:       "smart",
		request:     "Pod crashing",
		output:      "json",
		IOStreams: streams,
	}
	if err := o.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if !strings.Contains(out.String(), `"request"`) || !strings.Contains(out.String(), `"analysis"`) {
		t.Errorf("expected JSON output with proposal fields, got:\n%s", out.String())
	}
}

func TestCreate_Validate(t *testing.T) {
	tests := []struct {
		name    string
		opts    CreateOptions
		wantErr bool
		errMsg  string
	}{
		{
			name:    "empty request",
			opts:    CreateOptions{request: "  "},
			wantErr: true,
			errMsg:  "--request",
		},
		{
			name:    "valid",
			opts:    CreateOptions{request: "fix"},
			wantErr: false,
		},
		{
			name:    "invalid output",
			opts:    CreateOptions{request: "fix", output: "xml"},
			wantErr: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.opts.Validate()
			if (err != nil) != tc.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.wantErr && tc.errMsg != "" && err != nil && !strings.Contains(err.Error(), tc.errMsg) {
				t.Errorf("error should contain %q, got: %v", tc.errMsg, err)
			}
		})
	}
}

func TestCreate_InlineAnalysisAgent(t *testing.T) {
	streams, _, _ := fakeStreams()
	fc := fake.NewClientBuilder().WithScheme(testScheme()).Build()

	o := &CreateOptions{
		client:      fc,
		namespace:   "default",
		agent:       "smart",
		request:     "test",
		IOStreams: streams,
	}
	if err := o.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	list := &agenticv1alpha1.ProposalList{}
	if err := fc.List(context.Background(), list); err != nil {
		t.Fatalf("List: %v", err)
	}
	if list.Items[0].Spec.Analysis.IsZero() || list.Items[0].Spec.Analysis.Agent != "smart" {
		t.Errorf("expected analysis agent 'smart', got %v", list.Items[0].Spec.Analysis)
	}
}
