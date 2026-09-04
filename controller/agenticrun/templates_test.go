package agenticrun

import (
	"strings"
	"testing"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestBuildEscalationRequest_UsesOutcome(t *testing.T) {
	run := &agenticv1alpha1.AgenticRun{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-run",
			Namespace: "default",
		},
		Spec: agenticv1alpha1.AgenticRunSpec{
			Request: "fix the widget",
		},
		Status: agenticv1alpha1.AgenticRunStatus{
			Steps: agenticv1alpha1.StepsStatus{
				Analysis: agenticv1alpha1.AnalysisStepStatus{
					Results: []agenticv1alpha1.StepResultRef{
						{Name: "analysis-1", Outcome: agenticv1alpha1.ActionOutcomeSucceeded},
					},
				},
				Execution: agenticv1alpha1.ExecutionStepStatus{
					Results: []agenticv1alpha1.StepResultRef{
						{Name: "exec-1", Outcome: agenticv1alpha1.ActionOutcomeFailed},
					},
				},
				Verification: agenticv1alpha1.VerificationStepStatus{
					Results: []agenticv1alpha1.StepResultRef{
						{Name: "verify-1", Outcome: agenticv1alpha1.ActionOutcomeFailed},
					},
				},
			},
		},
	}

	result, err := buildEscalationRequest(run, "openshift-lightspeed")
	if err != nil {
		t.Fatalf("template rendering failed: %v", err)
	}

	if !strings.Contains(result, "AnalysisResult: analysis-1 (outcome=Succeeded)") {
		t.Errorf("expected %q in output, got: %s", "AnalysisResult: analysis-1 (outcome=Succeeded)", result)
	}
	if !strings.Contains(result, "ExecutionResult: exec-1 (outcome=Failed)") {
		t.Errorf("expected %q in output, got: %s", "ExecutionResult: exec-1 (outcome=Failed)", result)
	}
	if !strings.Contains(result, "VerificationResult: verify-1 (outcome=Failed)") {
		t.Errorf("expected %q in output, got: %s", "VerificationResult: verify-1 (outcome=Failed)", result)
	}
	if !strings.Contains(result, "openshift-lightspeed") {
		t.Errorf("expected result namespace %q in output, got: %s", "openshift-lightspeed", result)
	}
}
