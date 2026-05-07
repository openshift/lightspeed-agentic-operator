package proposal

import (
	"context"
	"strings"
	"testing"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func newSandboxClient(objects ...client.Object) client.Client {
	s := runtime.NewScheme()

	mapper := apimeta.NewDefaultRESTMapper([]schema.GroupVersion{
		{Group: "extensions.agents.x-k8s.io", Version: "v1alpha1"},
	})
	mapper.Add(schema.GroupVersionKind{
		Group: "extensions.agents.x-k8s.io", Version: "v1alpha1", Kind: "SandboxClaim",
	}, apimeta.RESTScopeNamespace)

	builder := fake.NewClientBuilder().
		WithScheme(s).
		WithRESTMapper(mapper)

	if len(objects) > 0 {
		builder = builder.WithObjects(objects...)
	}

	return builder.Build()
}

func TestBuildClaim_Structure(t *testing.T) {
	m := NewSandboxManager(nil, "test-ns")
	claim := m.buildClaim("my-claim", "my-proposal", "analysis", "my-template")

	if got := claim.GetName(); got != "my-claim" {
		t.Errorf("name = %q, want %q", got, "my-claim")
	}
	if got := claim.GetNamespace(); got != "test-ns" {
		t.Errorf("namespace = %q, want %q", got, "test-ns")
	}
	if claim.GetAPIVersion() != "extensions.agents.x-k8s.io/v1alpha1" {
		t.Errorf("apiVersion = %q", claim.GetAPIVersion())
	}
	if claim.GetKind() != "SandboxClaim" {
		t.Errorf("kind = %q", claim.GetKind())
	}
}

func TestBuildClaim_Labels(t *testing.T) {
	m := NewSandboxManager(nil, "ns")
	claim := m.buildClaim("c", "prop-1", "execution", "tpl")

	labels := claim.GetLabels()
	if labels[LabelProposal] != "prop-1" {
		t.Errorf("proposal label = %q", labels[LabelProposal])
	}
	if labels[LabelStep] != "execution" {
		t.Errorf("phase label = %q", labels[LabelStep])
	}
}

func TestBuildClaim_TemplateRef(t *testing.T) {
	m := NewSandboxManager(nil, "ns")
	claim := m.buildClaim("c", "p", "analysis", "my-template")

	templateRef, found, _ := unstructured.NestedString(claim.Object, "spec", "sandboxTemplateRef", "name")
	if !found || templateRef != "my-template" {
		t.Errorf("templateRef = %q, want %q", templateRef, "my-template")
	}

	shutdown, found, _ := unstructured.NestedString(claim.Object, "spec", "lifecycle", "shutdownPolicy")
	if !found || shutdown != "Delete" {
		t.Errorf("shutdownPolicy = %q, want %q", shutdown, "Delete")
	}
}

func TestClaim_Creates(t *testing.T) {
	c := newSandboxClient()
	m := NewSandboxManager(c, "test-ns")

	claimName, err := m.Claim(context.Background(), "my-proposal", "analysis", "analysis-template")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if claimName != "ls-analysis-my-proposal" {
		t.Errorf("claim name = %q, want %q", claimName, "ls-analysis-my-proposal")
	}

	claim := &unstructured.Unstructured{}
	claim.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "extensions.agents.x-k8s.io", Version: "v1alpha1", Kind: "SandboxClaim",
	})
	err = c.Get(context.Background(), types.NamespacedName{
		Name: claimName, Namespace: "test-ns",
	}, claim)
	if err != nil {
		t.Fatalf("failed to get created claim: %v", err)
	}
}

func TestClaim_AlreadyExists(t *testing.T) {
	existing := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "extensions.agents.x-k8s.io/v1alpha1",
			"kind":       "SandboxClaim",
			"metadata": map[string]any{
				"name":      "ls-analysis-my-proposal",
				"namespace": "test-ns",
			},
		},
	}

	c := newSandboxClient(existing)
	m := NewSandboxManager(c, "test-ns")

	claimName, err := m.Claim(context.Background(), "my-proposal", "analysis", "analysis-template")
	if err != nil {
		t.Fatalf("unexpected error for already-existing claim: %v", err)
	}
	if claimName != "ls-analysis-my-proposal" {
		t.Errorf("claim name = %q", claimName)
	}
}

func TestClaim_LongName(t *testing.T) {
	c := newSandboxClient()
	m := NewSandboxManager(c, "test-ns")

	longProposalName := strings.Repeat("a", 100)
	claimName, err := m.Claim(context.Background(), longProposalName, "analysis", "template")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(claimName) > 63 {
		t.Errorf("claim name too long: %d chars", len(claimName))
	}
}

func TestClaim_ExecutionPhase(t *testing.T) {
	c := newSandboxClient()
	m := NewSandboxManager(c, "test-ns")

	claimName, err := m.Claim(context.Background(), "my-proposal", "execution", "exec-template")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if claimName != "ls-execution-my-proposal" {
		t.Errorf("claim name = %q, want %q", claimName, "ls-execution-my-proposal")
	}

	claim := &unstructured.Unstructured{}
	claim.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "extensions.agents.x-k8s.io", Version: "v1alpha1", Kind: "SandboxClaim",
	})
	_ = c.Get(context.Background(), types.NamespacedName{
		Name: claimName, Namespace: "test-ns",
	}, claim)

	labels := claim.GetLabels()
	if labels[LabelStep] != "execution" {
		t.Errorf("phase label = %q, want 'execution'", labels[LabelStep])
	}
}

func TestClaim_VerificationPhase(t *testing.T) {
	c := newSandboxClient()
	m := NewSandboxManager(c, "test-ns")

	claimName, err := m.Claim(context.Background(), "my-proposal", "verification", "validate-template")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if claimName != "ls-verification-my-proposal" {
		t.Errorf("claim name = %q", claimName)
	}
}

func TestRelease_Deletes(t *testing.T) {
	existing := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "extensions.agents.x-k8s.io/v1alpha1",
			"kind":       "SandboxClaim",
			"metadata": map[string]any{
				"name":      "ls-execution-my-proposal",
				"namespace": "test-ns",
			},
		},
	}

	c := newSandboxClient(existing)
	m := NewSandboxManager(c, "test-ns")

	err := m.Release(context.Background(), "ls-execution-my-proposal")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	claim := &unstructured.Unstructured{}
	claim.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "extensions.agents.x-k8s.io", Version: "v1alpha1", Kind: "SandboxClaim",
	})
	err = c.Get(context.Background(), types.NamespacedName{
		Name: "ls-execution-my-proposal", Namespace: "test-ns",
	}, claim)
	if err == nil {
		t.Error("expected claim to be deleted")
	}
}

func TestRelease_NotFound(t *testing.T) {
	c := newSandboxClient()
	m := NewSandboxManager(c, "test-ns")

	err := m.Release(context.Background(), "nonexistent")
	if err != nil {
		t.Fatalf("expected no error for not-found claim, got %v", err)
	}
}
