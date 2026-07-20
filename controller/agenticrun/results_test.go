package agenticrun

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
)

func TestResultLabels_TruncatesLongAgenticRunName(t *testing.T) {
	longName := strings.Repeat("a", 80)
	labels := resultLabels(longName, "analysis")
	if len(labels[LabelRun]) > 63 {
		t.Fatalf("run label length %d exceeds 63", len(labels[LabelRun]))
	}
	if labels[LabelRun] != strings.Repeat("a", 63) {
		t.Errorf("run label = %q, want %q", labels[LabelRun], strings.Repeat("a", 63))
	}
	if labels[LabelStep] != "analysis" {
		t.Errorf("step label = %q, want analysis", labels[LabelStep])
	}
}

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
			AgenticRunName: "test-run",
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

	if got.Spec.AgenticRunName != "test-run" {
		t.Errorf("agenticRunName = %q, want test-run", got.Spec.AgenticRunName)
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
			AgenticRunName: "test-run",
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
			AgenticRunName: "test-run",
		},
		Status: agenticv1alpha1.AnalysisResultStatus{
			Options: []agenticv1alpha1.RemediationOption{
				{Title: "Updated option from retry"},
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

	if len(got.Status.Options) != 1 || got.Status.Options[0].Title != "Updated option from retry" {
		t.Error("AlreadyExists should update status with latest result")
	}
}

func TestCreateIdempotent_AlreadyExists_OverwritesStaleFailure(t *testing.T) {
	scheme := testScheme()

	existing := &agenticv1alpha1.AnalysisResult{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-analysis-1",
			Namespace: "default",
		},
		Spec: agenticv1alpha1.AnalysisResultSpec{
			AgenticRunName: "test-run",
		},
		Status: agenticv1alpha1.AnalysisResultStatus{
			Conditions: []metav1.Condition{
				{Type: "Completed", Status: metav1.ConditionTrue, Reason: "Failed", LastTransitionTime: metav1.Now()},
			},
			FailureReason: "sandbox DNS unreachable",
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
			AgenticRunName: "test-run",
		},
		Status: agenticv1alpha1.AnalysisResultStatus{
			Conditions: []metav1.Condition{
				{Type: "Completed", Status: metav1.ConditionTrue, Reason: "Succeeded", LastTransitionTime: metav1.Now()},
			},
			Options: []agenticv1alpha1.RemediationOption{
				{Title: "Increase memory limit"},
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

	if len(got.Status.Options) != 1 || got.Status.Options[0].Title != "Increase memory limit" {
		t.Errorf("expected success options, got %v", got.Status.Options)
	}
	if got.Status.FailureReason != "" {
		t.Errorf("stale FailureReason not cleared: %q", got.Status.FailureReason)
	}
	if len(got.Status.Conditions) != 1 || got.Status.Conditions[0].Reason != "Succeeded" {
		t.Errorf("condition not updated: %v", got.Status.Conditions)
	}
}

// TestCreateAnalysisResult_EmptyTopLevelDiagnosis reproduces OLS-3654:
// when the agent returns a top-level diagnosis with empty summary or
// rootCause, the CRD's MinLength=1 rejects the status patch.
func TestCreateAnalysisResult_EmptyTopLevelDiagnosis(t *testing.T) {
	cases := []struct {
		name      string
		diagnosis *agenticv1alpha1.DiagnosisResult
	}{
		{"both empty", &agenticv1alpha1.DiagnosisResult{Confidence: agenticv1alpha1.ConfidenceLevelLow, RootCause: "", Summary: ""}},
		{"summary only empty", &agenticv1alpha1.DiagnosisResult{Confidence: agenticv1alpha1.ConfidenceLevelLow, RootCause: "some cause", Summary: ""}},
		{"rootCause only empty", &agenticv1alpha1.DiagnosisResult{Confidence: agenticv1alpha1.ConfidenceLevelLow, RootCause: "", Summary: "some summary"}},
		{"confidence empty", &agenticv1alpha1.DiagnosisResult{Confidence: "", RootCause: "some cause", Summary: "some summary"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			scheme := testScheme()
			fc := fake.NewClientBuilder().WithScheme(scheme).
				WithStatusSubresource(&agenticv1alpha1.AnalysisResult{}).Build()

			run := testAgenticRun()
			run.UID = "test-uid"

			actionRequired := true
			result := &AnalysisOutput{
				Success:        true,
				ActionRequired: &actionRequired,
				Diagnosis:      tc.diagnosis,
				Options: []agenticv1alpha1.RemediationOption{{
					Title: "Increase connection pool limits",
					Diagnosis: agenticv1alpha1.DiagnosisResult{
						Confidence: agenticv1alpha1.ConfidenceLevelHigh,
						RootCause:  "reporting-service v1.0.2 opens a new PostgreSQL transaction every 10s",
						Summary:    "PostgresqlTooManyConnections is firing in payments",
					},
					RemediationPlan: agenticv1alpha1.RemediationPlan{
						Description: "Increase max_connections",
						Actions:     []agenticv1alpha1.ProposedAction{{Command: "kubectl patch", Type: "patch", Description: "Patch configmap"}},
						Risk:        agenticv1alpha1.RiskLevelLow,
					},
				}},
			}

			r := &AgenticRunReconciler{Client: fc}
			startTime := metav1.Now()
			completionTime := metav1.Now()
			_, snapshot, err := r.createAnalysisResult(
				context.Background(), run, result,
				agenticv1alpha1.SandboxInfo{ClaimName: "test-sandbox", Namespace: "openshift-lightspeed"},
				&startTime, &completionTime, "",
			)
			if err != nil {
				t.Fatalf("createAnalysisResult: %v", err)
			}

			if snapshot.Status.Diagnosis.Summary != "" || snapshot.Status.Diagnosis.RootCause != "" || snapshot.Status.Diagnosis.Confidence != "" {
				t.Errorf("expected zero-value diagnosis in status, got confidence=%q summary=%q rootCause=%q",
					snapshot.Status.Diagnosis.Confidence,
					snapshot.Status.Diagnosis.Summary,
					snapshot.Status.Diagnosis.RootCause)
			}

			if len(snapshot.Status.Options) != 1 {
				t.Fatalf("expected 1 option, got %d", len(snapshot.Status.Options))
			}
			if snapshot.Status.Options[0].Diagnosis.RootCause == "" {
				t.Error("per-option diagnosis should be preserved")
			}
		})
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
			AgenticRunName: "test-run",

			RetryIndex: &retryIdx,
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
