package agenticrun

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
)

func TestNeedsRevision(t *testing.T) {
	tests := []struct {
		name               string
		generation         int64
		observedGeneration int64
		feedback           string
		want               bool
	}{
		{"no_feedback", 1, 0, "", false},
		{"feedback_with_new_generation", 2, 1, "fix the memory issue", true},
		{"feedback_already_observed", 2, 2, "fix the memory issue", false},
		{"feedback_generation_3_observed_1", 3, 1, "try again", true},
		{"empty_feedback_new_generation", 2, 1, "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			run := &agenticv1alpha1.AgenticRun{
				ObjectMeta: metav1.ObjectMeta{Generation: tt.generation},
				Spec: agenticv1alpha1.AgenticRunSpec{
					RevisionFeedback: tt.feedback,
				},
				Status: agenticv1alpha1.AgenticRunStatus{},
			}
			if tt.observedGeneration > 0 {
				run.Status.Conditions = []metav1.Condition{{
					Type:               agenticv1alpha1.AgenticRunConditionAnalyzed,
					Status:             metav1.ConditionTrue,
					Reason:             "Complete",
					ObservedGeneration: tt.observedGeneration,
				}}
			}
			if got := needsRevision(run); got != tt.want {
				t.Errorf("needsRevision() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestBuildRevisionContext_WithFeedback(t *testing.T) {
	run := &agenticv1alpha1.AgenticRun{
		ObjectMeta: metav1.ObjectMeta{Name: "test-run", Namespace: "default", Generation: 2},
		Spec: agenticv1alpha1.AgenticRunSpec{
			RevisionFeedback: "Please focus on the memory issue, not CPU",
		},
	}
	result := buildRevisionContext(run)
	if !strings.Contains(result, "Please focus on the memory issue, not CPU") {
		t.Errorf("expected feedback in revision context, got: %s", result)
	}
	if !strings.Contains(result, "## User Feedback") {
		t.Errorf("expected User Feedback header in revision context, got: %s", result)
	}
}

func TestBuildRevisionContext_WithoutFeedback(t *testing.T) {
	run := &agenticv1alpha1.AgenticRun{
		ObjectMeta: metav1.ObjectMeta{Name: "test-run", Namespace: "default", Generation: 2},
		Spec:       agenticv1alpha1.AgenticRunSpec{},
	}
	result := buildRevisionContext(run)
	if strings.Contains(result, "## User Feedback") {
		t.Errorf("expected no User Feedback header when feedback is empty, got: %s", result)
	}
	if !strings.Contains(result, "generation 2") {
		t.Errorf("expected generation number in context, got: %s", result)
	}
}

func TestBuildAnalysisQuery_FullAgenticRun(t *testing.T) {
	run := &agenticv1alpha1.AgenticRun{
		Spec: agenticv1alpha1.AgenticRunSpec{
			Execution:    agenticv1alpha1.AgenticRunStep{Agent: "default"},
			Verification: agenticv1alpha1.AgenticRunStep{Agent: "default"},
		},
	}
	result := buildAnalysisQuery("Fix the crash", run)
	if !strings.Contains(result, "Derive RBAC") {
		t.Error("full run should mention RBAC derivation")
	}
	if !strings.Contains(result, "Verification plan") {
		t.Error("full run should mention verification plan")
	}
	if !strings.Contains(result, "Fix the crash") {
		t.Error("should contain the request text")
	}
	if !strings.Contains(result, "kubectl") {
		t.Error("should instruct use of kubectl")
	}
	if !strings.Contains(result, "remediation script") {
		t.Error("should require a remediation script")
	}
	if !strings.Contains(result, "SKILL.md") {
		t.Error("should instruct skill loading")
	}
}

func TestBuildAnalysisQuery_TrustMode(t *testing.T) {
	run := &agenticv1alpha1.AgenticRun{
		Spec: agenticv1alpha1.AgenticRunSpec{
			Execution: agenticv1alpha1.AgenticRunStep{Agent: "default"},
		},
	}
	result := buildAnalysisQuery("Fix the crash", run)
	if !strings.Contains(result, "Derive RBAC") {
		t.Error("execution run should mention RBAC derivation")
	}
	if strings.Contains(result, "Verification plan") {
		t.Error("trust-mode run should NOT mention verification plan")
	}
}

func TestBuildAnalysisQuery_Advisory(t *testing.T) {
	run := &agenticv1alpha1.AgenticRun{}
	result := buildAnalysisQuery("What is 2+2?", run)
	if strings.Contains(result, "Derive RBAC") {
		t.Error("advisory run should NOT mention RBAC derivation")
	}
	if strings.Contains(result, "Verification plan") {
		t.Error("advisory run should NOT mention verification plan")
	}
	if !strings.Contains(result, "What is 2+2?") {
		t.Error("should contain the request text")
	}
}
