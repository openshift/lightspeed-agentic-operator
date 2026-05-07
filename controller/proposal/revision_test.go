package proposal

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
			proposal := &agenticv1alpha1.Proposal{
				ObjectMeta: metav1.ObjectMeta{Generation: tt.generation},
				Spec: agenticv1alpha1.ProposalSpec{
					RevisionFeedback: tt.feedback,
				},
				Status: agenticv1alpha1.ProposalStatus{},
			}
			if tt.observedGeneration > 0 {
				proposal.Status.Conditions = []metav1.Condition{{
					Type:               agenticv1alpha1.ProposalConditionAnalyzed,
					Status:             metav1.ConditionTrue,
					Reason:             "Complete",
					ObservedGeneration: tt.observedGeneration,
				}}
			}
			if got := needsRevision(proposal); got != tt.want {
				t.Errorf("needsRevision() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestBuildRevisionContext_WithFeedback(t *testing.T) {
	proposal := &agenticv1alpha1.Proposal{
		ObjectMeta: metav1.ObjectMeta{Name: "test-proposal", Namespace: "default", Generation: 2},
		Spec: agenticv1alpha1.ProposalSpec{
			RevisionFeedback: "Please focus on the memory issue, not CPU",
		},
	}
	result := buildRevisionContext(proposal)
	if !strings.Contains(result, "Please focus on the memory issue, not CPU") {
		t.Errorf("expected feedback in revision context, got: %s", result)
	}
	if !strings.Contains(result, "## User Feedback") {
		t.Errorf("expected User Feedback header in revision context, got: %s", result)
	}
}

func TestBuildRevisionContext_WithoutFeedback(t *testing.T) {
	proposal := &agenticv1alpha1.Proposal{
		ObjectMeta: metav1.ObjectMeta{Name: "test-proposal", Namespace: "default", Generation: 2},
		Spec:       agenticv1alpha1.ProposalSpec{},
	}
	result := buildRevisionContext(proposal)
	if strings.Contains(result, "## User Feedback") {
		t.Errorf("expected no User Feedback header when feedback is empty, got: %s", result)
	}
	if !strings.Contains(result, "generation 2") {
		t.Errorf("expected generation number in context, got: %s", result)
	}
}

func TestBuildAnalysisQuery_FullProposal(t *testing.T) {
	proposal := &agenticv1alpha1.Proposal{
		Spec: agenticv1alpha1.ProposalSpec{
			Execution:    agenticv1alpha1.ProposalStep{Agent: "default"},
			Verification: agenticv1alpha1.ProposalStep{Agent: "default"},
		},
	}
	result := buildAnalysisQuery("Fix the crash", proposal)
	if !strings.Contains(result, "RBAC permissions") {
		t.Error("full proposal should mention RBAC permissions")
	}
	if !strings.Contains(result, "verification plan") {
		t.Error("full proposal should mention verification plan")
	}
	if !strings.Contains(result, "Fix the crash") {
		t.Error("should contain the request text")
	}
}

func TestBuildAnalysisQuery_TrustMode(t *testing.T) {
	proposal := &agenticv1alpha1.Proposal{
		Spec: agenticv1alpha1.ProposalSpec{
			Execution: agenticv1alpha1.ProposalStep{Agent: "default"},
		},
	}
	result := buildAnalysisQuery("Fix the crash", proposal)
	if !strings.Contains(result, "RBAC permissions") {
		t.Error("execution proposal should mention RBAC permissions")
	}
	if strings.Contains(result, "verification plan") {
		t.Error("trust-mode proposal should NOT mention verification plan")
	}
}

func TestBuildAnalysisQuery_Advisory(t *testing.T) {
	proposal := &agenticv1alpha1.Proposal{}
	result := buildAnalysisQuery("What is 2+2?", proposal)
	if strings.Contains(result, "RBAC permissions") {
		t.Error("advisory proposal should NOT mention RBAC permissions")
	}
	if strings.Contains(result, "verification plan") {
		t.Error("advisory proposal should NOT mention verification plan")
	}
	if !strings.Contains(result, "What is 2+2?") {
		t.Error("should contain the request text")
	}
}
