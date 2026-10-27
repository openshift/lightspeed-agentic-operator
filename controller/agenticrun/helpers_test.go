package agenticrun

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
)

func ptr32(v int32) *int32 { return &v }

func TestSelectedOption_ReturnsFirstOption(t *testing.T) {
	scheme := testScheme()

	run := &agenticv1alpha1.AgenticRun{}
	run.Name = "test"
	run.Namespace = "default"
	run.Status.Steps.Analysis.Results = []agenticv1alpha1.StepResultRef{
		{Name: "test-analysis-1", Outcome: agenticv1alpha1.ActionOutcomeSucceeded},
	}

	analysisResult := &agenticv1alpha1.AnalysisResult{}
	analysisResult.Name = "test-analysis-1"
	analysisResult.Namespace = "default"
	analysisResult.Status.Options = []agenticv1alpha1.RemediationOption{
		{Title: "A"},
	}

	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(analysisResult).Build()
	r := &AgenticRunReconciler{Client: fc, APIReader: fc, Namespace: "default"}

	got, err := r.selectedOption(context.Background(), run)
	if err != nil {
		t.Fatalf("selectedOption() error: %v", err)
	}
	if got == nil {
		t.Fatal("selectedOption() returned nil")
	}
	if got.Title != "A" {
		t.Errorf("selectedOption().Title = %q, want %q", got.Title, "A")
	}
}

func TestSelectedOption_NoResults(t *testing.T) {
	scheme := testScheme()

	run := &agenticv1alpha1.AgenticRun{}
	run.Name = "test"
	run.Namespace = "default"

	fc := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &AgenticRunReconciler{Client: fc, APIReader: fc, Namespace: "default"}

	got, err := r.selectedOption(context.Background(), run)
	if err != nil {
		t.Fatalf("selectedOption() error: %v", err)
	}
	if got != nil {
		t.Errorf("selectedOption() should return nil when no results, got %+v", got)
	}
}

func TestTrimNonSelectedOptions_SingleOptionNoop(t *testing.T) {
	scheme := testScheme()
	analysisResult := &agenticv1alpha1.AnalysisResult{}
	analysisResult.Name = "test-analysis-1"
	analysisResult.Namespace = "default"
	analysisResult.Status.Options = []agenticv1alpha1.RemediationOption{
		{Title: "Only"},
	}

	run := &agenticv1alpha1.AgenticRun{}
	run.Name = "test"
	run.Namespace = "default"
	run.Status.Steps.Analysis.Results = []agenticv1alpha1.StepResultRef{
		{Name: "test-analysis-1", Outcome: agenticv1alpha1.ActionOutcomeSucceeded},
	}

	approval := &agenticv1alpha1.AgenticRunApproval{
		Spec: agenticv1alpha1.AgenticRunApprovalSpec{
			Stages: []agenticv1alpha1.ApprovalStage{
				{Type: agenticv1alpha1.ApprovalStageExecution, Execution: &agenticv1alpha1.ExecutionApproval{Option: ptr32(0)}},
			},
		},
	}

	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(analysisResult).WithStatusSubresource(analysisResult).Build()
	r := &AgenticRunReconciler{Client: fc, APIReader: fc, Namespace: "default"}

	got, err := r.trimNonSelectedOptions(context.Background(), run, approval)
	if err != nil {
		t.Fatalf("trimNonSelectedOptions() error: %v", err)
	}
	if got == nil || got.Title != "Only" {
		t.Errorf("single option should be returned unchanged")
	}
}

func TestTrimThenSelectedOption_EndToEnd(t *testing.T) {
	scheme := testScheme()

	tests := []struct {
		name      string
		options   []agenticv1alpha1.RemediationOption
		selectIdx int32
		wantTitle string
	}{
		{"select first of 3", []agenticv1alpha1.RemediationOption{{Title: "A"}, {Title: "B"}, {Title: "C"}}, 0, "A"},
		{"select middle of 3", []agenticv1alpha1.RemediationOption{{Title: "A"}, {Title: "B"}, {Title: "C"}}, 1, "B"},
		{"select last of 3", []agenticv1alpha1.RemediationOption{{Title: "A"}, {Title: "B"}, {Title: "C"}}, 2, "C"},
		{"select second of 2", []agenticv1alpha1.RemediationOption{{Title: "X"}, {Title: "Y"}}, 1, "Y"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			analysisResult := &agenticv1alpha1.AnalysisResult{}
			analysisResult.Name = "test-analysis-1"
			analysisResult.Namespace = "default"
			analysisResult.Status.Options = tt.options

			run := &agenticv1alpha1.AgenticRun{}
			run.Name = "test"
			run.Namespace = "default"
			run.Status.Steps.Analysis.Results = []agenticv1alpha1.StepResultRef{
				{Name: "test-analysis-1", Outcome: agenticv1alpha1.ActionOutcomeSucceeded},
			}

			approval := &agenticv1alpha1.AgenticRunApproval{
				Spec: agenticv1alpha1.AgenticRunApprovalSpec{
					Stages: []agenticv1alpha1.ApprovalStage{
						{Type: agenticv1alpha1.ApprovalStageExecution, Execution: &agenticv1alpha1.ExecutionApproval{Option: &tt.selectIdx}},
					},
				},
			}

			fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(analysisResult).WithStatusSubresource(analysisResult).Build()
			r := &AgenticRunReconciler{Client: fc, APIReader: fc, Namespace: "default"}

			got, err := r.trimNonSelectedOptions(context.Background(), run, approval)
			if err != nil {
				t.Fatalf("trim error: %v", err)
			}
			if got == nil {
				t.Fatal("trimNonSelectedOptions() returned nil")
			}
			if got.Title != tt.wantTitle {
				t.Errorf("selectedOption().Title = %q, want %q", got.Title, tt.wantTitle)
			}
		})
	}
}

func TestTrimNonSelectedOptions_OutOfRange(t *testing.T) {
	scheme := testScheme()

	analysisResult := &agenticv1alpha1.AnalysisResult{}
	analysisResult.Name = "test-analysis-1"
	analysisResult.Namespace = "default"
	analysisResult.Status.Options = []agenticv1alpha1.RemediationOption{
		{Title: "A"}, {Title: "B"},
	}

	run := &agenticv1alpha1.AgenticRun{}
	run.Name = "test"
	run.Namespace = "default"
	run.Status.Steps.Analysis.Results = []agenticv1alpha1.StepResultRef{
		{Name: "test-analysis-1", Outcome: agenticv1alpha1.ActionOutcomeSucceeded},
	}

	approval := &agenticv1alpha1.AgenticRunApproval{
		Spec: agenticv1alpha1.AgenticRunApprovalSpec{
			Stages: []agenticv1alpha1.ApprovalStage{
				{Type: agenticv1alpha1.ApprovalStageExecution, Execution: &agenticv1alpha1.ExecutionApproval{Option: ptr32(5)}},
			},
		},
	}

	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(analysisResult).WithStatusSubresource(analysisResult).Build()
	r := &AgenticRunReconciler{Client: fc, APIReader: fc, Namespace: "default"}

	_, err := r.trimNonSelectedOptions(context.Background(), run, approval)
	if err == nil {
		t.Fatal("expected error for out-of-range option index")
	}
}

// --- isTransient tests ---

type fakeNetError struct{ temporary bool }

func (e *fakeNetError) Error() string   { return "network error" }
func (e *fakeNetError) Timeout() bool   { return e.temporary }
func (e *fakeNetError) Temporary() bool { return e.temporary }

var _ net.Error = (*fakeNetError)(nil)

func TestIsTransient(t *testing.T) {
	gr := schema.GroupResource{Group: "", Resource: "pods"}
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"plain error", fmt.Errorf("something broke"), false},
		{"not found", apierrors.NewNotFound(gr, "x"), false},
		{"forbidden", apierrors.NewForbidden(gr, "x", fmt.Errorf("nope")), false},
		{"invalid", apierrors.NewInvalid(schema.GroupKind{Group: "", Kind: "Pod"}, "x", nil), false},
		{"server timeout", apierrors.NewServerTimeout(gr, "create", 5), true},
		{"too many requests", apierrors.NewTooManyRequests("slow down", 1), true},
		{"service unavailable", apierrors.NewServiceUnavailable("gone"), true},
		{"internal error", apierrors.NewInternalError(fmt.Errorf("oops")), true},
		{"conflict", apierrors.NewConflict(gr, "x", fmt.Errorf("conflict")), true},
		{"net error", &fakeNetError{temporary: true}, true},
		{"wrapped transient", fmt.Errorf("outer: %w", apierrors.NewServerTimeout(gr, "get", 1)), true},
		{"wrapped permanent", fmt.Errorf("outer: %w", apierrors.NewNotFound(gr, "x")), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isTransient(tt.err); got != tt.want {
				t.Errorf("isTransient() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRetryBackoff(t *testing.T) {
	tests := []struct {
		attempt int
		want    time.Duration
	}{
		{1, 5 * time.Second},
		{2, 10 * time.Second},
		{3, 20 * time.Second},
	}
	for _, tt := range tests {
		if got := retryBackoff(tt.attempt); got != tt.want {
			t.Errorf("retryBackoff(%d) = %v, want %v", tt.attempt, got, tt.want)
		}
	}
}

func withFastRetry(t *testing.T) {
	t.Helper()
	orig := retryBaseDelay
	retryBaseDelay = 1 * time.Millisecond
	t.Cleanup(func() { retryBaseDelay = orig })
}

func TestRetryOnTransient_SucceedsImmediately(t *testing.T) {
	withFastRetry(t)
	calls := 0
	err := retryOnTransient(context.Background(), func() error {
		calls++
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls != 1 {
		t.Errorf("expected 1 call, got %d", calls)
	}
}

func TestRetryOnTransient_PermanentErrorNoRetry(t *testing.T) {
	withFastRetry(t)
	calls := 0
	permErr := apierrors.NewNotFound(schema.GroupResource{}, "x")
	err := retryOnTransient(context.Background(), func() error {
		calls++
		return permErr
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if calls != 1 {
		t.Errorf("permanent error should not retry, got %d calls", calls)
	}
}

func TestRetryOnTransient_TransientThenSuccess(t *testing.T) {
	withFastRetry(t)
	gr := schema.GroupResource{Group: "", Resource: "pods"}
	calls := 0
	err := retryOnTransient(context.Background(), func() error {
		calls++
		if calls < 2 {
			return apierrors.NewServerTimeout(gr, "create", 0)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls != 2 {
		t.Errorf("expected 2 calls (1 transient + 1 success), got %d", calls)
	}
}

func TestRetryOnTransient_ExhaustsRetries(t *testing.T) {
	withFastRetry(t)
	gr := schema.GroupResource{Group: "", Resource: "pods"}
	calls := 0
	err := retryOnTransient(context.Background(), func() error {
		calls++
		return apierrors.NewServerTimeout(gr, "create", 0)
	})
	if err == nil {
		t.Fatal("expected error after exhausting retries")
	}
	if calls != maxCreateRetries {
		t.Errorf("expected %d calls, got %d", maxCreateRetries, calls)
	}
}

func TestRetryOnTransient_RespectsContextCancellation(t *testing.T) {
	withFastRetry(t)
	gr := schema.GroupResource{Group: "", Resource: "pods"}
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	err := retryOnTransient(ctx, func() error {
		calls++
		cancel()
		return apierrors.NewServerTimeout(gr, "create", 0)
	})
	if err != context.Canceled {
		t.Errorf("expected context.Canceled, got %v", err)
	}
	if calls != 1 {
		t.Errorf("expected 1 call before cancellation, got %d", calls)
	}
}
