package proposal

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
)

func TestCreateIdempotent_StatusFieldsWritten(t *testing.T) {
	scheme := testScheme()
	fc := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&agenticv1alpha1.AnalysisResult{}).Build()

	cr := &agenticv1alpha1.AnalysisResult{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-analysis-1",
			Namespace: "default",
		},
		Spec: agenticv1alpha1.AnalysisResultSpec{
			ProposalName: "test-proposal",

		},
		Status: agenticv1alpha1.AnalysisResultStatus{
			Conditions: []metav1.Condition{
				{Type: "Completed", Status: metav1.ConditionTrue, Reason: "Succeeded", LastTransitionTime: metav1.Now()},
			},
			Options: []agenticv1alpha1.RemediationOption{
				{Title: "Increase memory limit", Summary: "Bump to 512Mi"},
			},
			Sandbox: agenticv1alpha1.SandboxInfo{
				ClaimName: "test-sandbox",
				Namespace: "openshift-lightspeed",
			},
			FailureReason: "",
		},
	}

	if err := createIdempotent(context.Background(), fc, cr, "AnalysisResult"); err != nil {
		t.Fatalf("createIdempotent: %v", err)
	}

	var got agenticv1alpha1.AnalysisResult
	if err := fc.Get(context.Background(), types.NamespacedName{Name: "test-analysis-1", Namespace: "default"}, &got); err != nil {
		t.Fatalf("Get: %v", err)
	}

	if got.Spec.ProposalName != "test-proposal" {
		t.Errorf("proposalName = %q, want test-proposal", got.Spec.ProposalName)
	}
	if len(got.Status.Options) != 1 {
		t.Fatalf("expected 1 option in status, got %d", len(got.Status.Options))
	}
	if got.Status.Options[0].Title != "Increase memory limit" {
		t.Errorf("option title = %q", got.Status.Options[0].Title)
	}
	if got.Status.Sandbox.ClaimName != "test-sandbox" {
		t.Errorf("sandbox claimName = %q, want test-sandbox", got.Status.Sandbox.ClaimName)
	}
	if len(got.Status.Conditions) != 1 {
		t.Fatalf("expected 1 condition, got %d", len(got.Status.Conditions))
	}
	if got.Status.Conditions[0].Reason != "Succeeded" {
		t.Errorf("condition reason = %q, want Succeeded", got.Status.Conditions[0].Reason)
	}
}

func TestCreateIdempotent_AlreadyExists(t *testing.T) {
	scheme := testScheme()

	existing := &agenticv1alpha1.AnalysisResult{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-analysis-1",
			Namespace: "default",
		},
		Spec: agenticv1alpha1.AnalysisResultSpec{
			ProposalName: "test-proposal",

		},
	}

	fc := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(existing).
		WithStatusSubresource(&agenticv1alpha1.AnalysisResult{}).Build()

	cr := &agenticv1alpha1.AnalysisResult{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-analysis-1",
			Namespace: "default",
		},
		Spec: agenticv1alpha1.AnalysisResultSpec{
			ProposalName: "test-proposal",

		},
		Status: agenticv1alpha1.AnalysisResultStatus{
			Options: []agenticv1alpha1.RemediationOption{
				{Title: "Should not overwrite"},
			},
		},
	}

	if err := createIdempotent(context.Background(), fc, cr, "AnalysisResult"); err != nil {
		t.Fatalf("createIdempotent on existing: %v", err)
	}

	var got agenticv1alpha1.AnalysisResult
	if err := fc.Get(context.Background(), types.NamespacedName{Name: "test-analysis-1", Namespace: "default"}, &got); err != nil {
		t.Fatalf("Get: %v", err)
	}

	if len(got.Status.Options) != 0 {
		t.Error("AlreadyExists should not overwrite status")
	}
}

func TestCreateIdempotent_ExecutionResult(t *testing.T) {
	scheme := testScheme()
	fc := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&agenticv1alpha1.ExecutionResult{}).Build()

	retryIdx := int32(0)
	cr := &agenticv1alpha1.ExecutionResult{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-execution-1",
			Namespace: "default",
		},
		Spec: agenticv1alpha1.ExecutionResultSpec{
			ProposalName: "test-proposal",

			RetryIndex:   &retryIdx,
		},
		Status: agenticv1alpha1.ExecutionResultStatus{
			Conditions: []metav1.Condition{
				{Type: "Completed", Status: metav1.ConditionTrue, Reason: "Succeeded", LastTransitionTime: metav1.Now()},
			},
			ActionsTaken: []agenticv1alpha1.ExecutionAction{
				{Type: "patch", Description: "Increased memory limit", Outcome: agenticv1alpha1.ActionOutcomeSucceeded},
			},
		},
	}

	if err := createIdempotent(context.Background(), fc, cr, "ExecutionResult"); err != nil {
		t.Fatalf("createIdempotent: %v", err)
	}

	var got agenticv1alpha1.ExecutionResult
	if err := fc.Get(context.Background(), types.NamespacedName{Name: "test-execution-1", Namespace: "default"}, &got); err != nil {
		t.Fatalf("Get: %v", err)
	}

	if len(got.Status.ActionsTaken) != 1 {
		t.Fatalf("expected 1 action in status, got %d", len(got.Status.ActionsTaken))
	}
	if got.Status.ActionsTaken[0].Type != "patch" {
		t.Errorf("action type = %q, want patch", got.Status.ActionsTaken[0].Type)
	}
}
