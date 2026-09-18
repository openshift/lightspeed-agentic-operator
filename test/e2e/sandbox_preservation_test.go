//go:build e2e

package e2e

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
)

const preserveSandboxAnnotationKey = "agentic.openshift.io/preserve-sandbox"

// TestPreserveSandboxAnnotationKeepsFailedPod verifies that a failed run with
// the preserve-sandbox annotation keeps its sandbox Pod for inspection.
func TestPreserveSandboxAnnotationKeepsFailedPod(t *testing.T) {
	c := newClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	prop := &agenticv1alpha1.AgenticRun{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "e2e-preserve-sandbox",
			Namespace: testNS,
			Annotations: map[string]string{
				preserveSandboxAnnotationKey: "true",
			},
		},
		Spec: agenticv1alpha1.AgenticRunSpec{
			Request:          "MOCK_CRASH",
			TargetNamespaces: []string{"staging"},
			Tools: agenticv1alpha1.ToolsSpec{Skills: []agenticv1alpha1.SkillsSource{{
				Image: "quay.io/openshift-lightspeed/ols-qe:lightspeed-mock-agent",
				Paths: []string{"/skills"},
			}}},
			Analysis:     agenticv1alpha1.AgenticRunStep{Agent: "e2e-agent"},
			Execution:    agenticv1alpha1.AgenticRunStep{Agent: "e2e-agent"},
			Verification: agenticv1alpha1.AgenticRunStep{Agent: "e2e-agent"},
		},
	}
	cleanup(t, c, &agenticv1alpha1.AgenticRun{ObjectMeta: metav1.ObjectMeta{Name: prop.Name, Namespace: testNS}})
	cleanup(t, c, &agenticv1alpha1.AgenticRunApproval{ObjectMeta: metav1.ObjectMeta{Name: prop.Name, Namespace: testNS}})
	if err := c.Create(ctx, prop); err != nil {
		t.Fatalf("create preserved AgenticRun: %v", err)
	}
	t.Cleanup(func() { cleanup(t, c, prop) })

	updated := waitForPhase(t, c, prop.Name, agenticv1alpha1.AgenticRunPhaseFailed)
	pod, err := findAnalysisPod(ctx, c, &updated)
	if err != nil {
		t.Fatalf("find preserved sandbox pod: %v", err)
	}
	if pod == nil {
		t.Fatal("expected failed sandbox Pod to be preserved")
	}
	t.Logf("preserved failed sandbox Pod %s", pod.Name)
}
