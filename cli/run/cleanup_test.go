package run

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func terminalTime(ago time.Duration) *metav1.Time {
	t := metav1.NewTime(time.Now().Add(-ago))
	return &t
}

func completedRun(name, ns string, termTime *metav1.Time) *agenticv1alpha1.AgenticRun {
	return &agenticv1alpha1.AgenticRun{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       agenticv1alpha1.AgenticRunSpec{Request: "test"},
		Status: agenticv1alpha1.AgenticRunStatus{
			TerminalTime: termTime,
			Conditions: []metav1.Condition{
				{Type: agenticv1alpha1.AgenticRunConditionVerified, Status: metav1.ConditionTrue, Reason: "Complete"},
			},
		},
	}
}

func failedRun(name, ns string, termTime *metav1.Time) *agenticv1alpha1.AgenticRun {
	return &agenticv1alpha1.AgenticRun{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       agenticv1alpha1.AgenticRunSpec{Request: "test"},
		Status: agenticv1alpha1.AgenticRunStatus{
			TerminalTime: termTime,
			Conditions: []metav1.Condition{
				{Type: agenticv1alpha1.AgenticRunConditionAnalyzed, Status: metav1.ConditionFalse, Reason: "Failed", Message: "error"},
			},
		},
	}
}

func deniedRun(name, ns string, termTime *metav1.Time) *agenticv1alpha1.AgenticRun {
	return &agenticv1alpha1.AgenticRun{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       agenticv1alpha1.AgenticRunSpec{Request: "test"},
		Status: agenticv1alpha1.AgenticRunStatus{
			TerminalTime: termTime,
			Conditions: []metav1.Condition{
				{Type: agenticv1alpha1.AgenticRunConditionAnalyzed, Status: metav1.ConditionTrue, Reason: "Complete"},
				{Type: agenticv1alpha1.AgenticRunConditionDenied, Status: metav1.ConditionTrue, Reason: "UserDenied"},
			},
		},
	}
}

func pendingRun(name, ns string) *agenticv1alpha1.AgenticRun {
	return &agenticv1alpha1.AgenticRun{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       agenticv1alpha1.AgenticRunSpec{Request: "test"},
	}
}

func buildFakeClient(objs ...client.Object) client.Client {
	return fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&agenticv1alpha1.AgenticRun{}).
		Build()
}

func runExists(fc client.Client, name, ns string) bool {
	var run agenticv1alpha1.AgenticRun
	err := fc.Get(context.Background(), types.NamespacedName{Name: name, Namespace: ns}, &run)
	return err == nil
}

func TestCleanup_DeletesAllTerminalRuns(t *testing.T) {
	fc := buildFakeClient(
		completedRun("run-a", "default", terminalTime(1*time.Hour)),
		failedRun("run-b", "default", terminalTime(2*time.Hour)),
		pendingRun("run-c", "default"),
	)

	var out bytes.Buffer
	o := &CleanupOptions{
		client:    fc,
		namespace: "default",
		yes:       true,
		IOStreams: genericclioptions.IOStreams{Out: &out, ErrOut: &out},
	}

	if err := o.Run(context.Background()); err != nil {
		t.Fatalf("Run() error: %v", err)
	}

	if runExists(fc, "run-a", "default") {
		t.Error("run-a should be deleted")
	}
	if runExists(fc, "run-b", "default") {
		t.Error("run-b should be deleted")
	}
	if !runExists(fc, "run-c", "default") {
		t.Error("run-c (pending) should NOT be deleted")
	}
	if !strings.Contains(out.String(), "Deleted 2 run(s).") {
		t.Errorf("unexpected output: %s", out.String())
	}
}

func TestCleanup_StateFilter(t *testing.T) {
	fc := buildFakeClient(
		completedRun("run-a", "default", terminalTime(1*time.Hour)),
		failedRun("run-b", "default", terminalTime(1*time.Hour)),
		deniedRun("run-c", "default", terminalTime(1*time.Hour)),
	)

	var out bytes.Buffer
	o := &CleanupOptions{
		client:         fc,
		namespace:      "default",
		states:         "completed,failed",
		hasStateFilter: true,
		stateFilter:    map[string]bool{"completed": true, "failed": true},
		yes:            true,
		IOStreams:      genericclioptions.IOStreams{Out: &out, ErrOut: &out},
	}

	if err := o.Run(context.Background()); err != nil {
		t.Fatalf("Run() error: %v", err)
	}

	if runExists(fc, "run-a", "default") {
		t.Error("run-a (completed) should be deleted")
	}
	if runExists(fc, "run-b", "default") {
		t.Error("run-b (failed) should be deleted")
	}
	if !runExists(fc, "run-c", "default") {
		t.Error("run-c (denied) should NOT be deleted when state filter excludes it")
	}
}

func TestCleanup_OlderThanFilter(t *testing.T) {
	fc := buildFakeClient(
		completedRun("old-run", "default", terminalTime(48*time.Hour)),
		completedRun("new-run", "default", terminalTime(1*time.Hour)),
	)

	var out bytes.Buffer
	o := &CleanupOptions{
		client:       fc,
		namespace:    "default",
		hasOlderThan: true,
		olderThanDur: 24 * time.Hour,
		yes:          true,
		IOStreams:    genericclioptions.IOStreams{Out: &out, ErrOut: &out},
	}

	if err := o.Run(context.Background()); err != nil {
		t.Fatalf("Run() error: %v", err)
	}

	if runExists(fc, "old-run", "default") {
		t.Error("old-run should be deleted (older than 24h)")
	}
	if !runExists(fc, "new-run", "default") {
		t.Error("new-run should NOT be deleted (only 1h old)")
	}
}

func TestCleanup_OlderThanSkipsNoTerminalTime(t *testing.T) {
	run := completedRun("no-time", "default", nil) // no terminalTime

	fc := buildFakeClient(run)

	var out, errOut bytes.Buffer
	o := &CleanupOptions{
		client:       fc,
		namespace:    "default",
		hasOlderThan: true,
		olderThanDur: 1 * time.Hour,
		IOStreams:    genericclioptions.IOStreams{Out: &out, ErrOut: &errOut},
	}

	if err := o.Run(context.Background()); err != nil {
		t.Fatalf("Run() error: %v", err)
	}

	if !runExists(fc, "no-time", "default") {
		t.Error("run without terminalTime should NOT be deleted")
	}
	if !strings.Contains(errOut.String(), "Warning") || !strings.Contains(errOut.String(), "no-time") {
		t.Errorf("expected warning about missing terminalTime, got: %s", errOut.String())
	}
}

func TestCleanup_DryRun(t *testing.T) {
	fc := buildFakeClient(
		completedRun("run-a", "default", terminalTime(1*time.Hour)),
		failedRun("run-b", "default", terminalTime(2*time.Hour)),
	)

	var out bytes.Buffer
	o := &CleanupOptions{
		client:    fc,
		namespace: "default",
		dryRun:    true,
		IOStreams: genericclioptions.IOStreams{Out: &out, ErrOut: &out},
	}

	if err := o.Run(context.Background()); err != nil {
		t.Fatalf("Run() error: %v", err)
	}

	// Runs should still exist.
	if !runExists(fc, "run-a", "default") {
		t.Error("run-a should NOT be deleted in dry-run")
	}
	if !runExists(fc, "run-b", "default") {
		t.Error("run-b should NOT be deleted in dry-run")
	}
	if !strings.Contains(out.String(), "would be deleted (dry-run)") {
		t.Errorf("expected dry-run summary, got: %s", out.String())
	}
	if !strings.Contains(out.String(), "run-a") || !strings.Contains(out.String(), "run-b") {
		t.Errorf("expected run names in output, got: %s", out.String())
	}
}

func TestCleanup_AllNamespaces(t *testing.T) {
	fc := buildFakeClient(
		completedRun("run-ns1", "ns1", terminalTime(1*time.Hour)),
		completedRun("run-ns2", "ns2", terminalTime(1*time.Hour)),
	)

	var out bytes.Buffer
	o := &CleanupOptions{
		client:        fc,
		allNamespaces: true,
		yes:           true,
		IOStreams:     genericclioptions.IOStreams{Out: &out, ErrOut: &out},
	}

	if err := o.Run(context.Background()); err != nil {
		t.Fatalf("Run() error: %v", err)
	}

	if runExists(fc, "run-ns1", "ns1") {
		t.Error("run-ns1 should be deleted")
	}
	if runExists(fc, "run-ns2", "ns2") {
		t.Error("run-ns2 should be deleted")
	}
	if !strings.Contains(out.String(), "Deleted 2 run(s).") {
		t.Errorf("unexpected output: %s", out.String())
	}
}

func TestCleanup_NonTerminalRunsUntouched(t *testing.T) {
	fc := buildFakeClient(
		pendingRun("pending-run", "default"),
	)

	var out bytes.Buffer
	o := &CleanupOptions{
		client:    fc,
		namespace: "default",
		IOStreams: genericclioptions.IOStreams{Out: &out, ErrOut: &out},
	}

	if err := o.Run(context.Background()); err != nil {
		t.Fatalf("Run() error: %v", err)
	}

	if !runExists(fc, "pending-run", "default") {
		t.Error("pending run should NOT be deleted")
	}
	if !strings.Contains(out.String(), "No matching terminal runs found.") {
		t.Errorf("expected no-match message, got: %s", out.String())
	}
}

func TestCleanup_PromptsAndAbortsOnNo(t *testing.T) {
	fc := buildFakeClient(
		completedRun("run-a", "default", terminalTime(1*time.Hour)),
	)

	var out bytes.Buffer
	in := strings.NewReader("n\n")
	o := &CleanupOptions{
		client:    fc,
		namespace: "default",
		IOStreams: genericclioptions.IOStreams{In: in, Out: &out, ErrOut: &out},
	}

	if err := o.Run(context.Background()); err != nil {
		t.Fatalf("Run() error: %v", err)
	}

	if !runExists(fc, "run-a", "default") {
		t.Error("run-a should NOT be deleted when the confirmation prompt is declined")
	}
	if !strings.Contains(out.String(), "Continue? [y/N]") {
		t.Errorf("expected confirmation prompt, got: %s", out.String())
	}
	if !strings.Contains(out.String(), "Aborted.") {
		t.Errorf("expected abort message, got: %s", out.String())
	}
}

func TestCleanup_PromptsAndDeletesOnYes(t *testing.T) {
	fc := buildFakeClient(
		completedRun("run-a", "default", terminalTime(1*time.Hour)),
	)

	var out bytes.Buffer
	in := strings.NewReader("y\n")
	o := &CleanupOptions{
		client:    fc,
		namespace: "default",
		IOStreams: genericclioptions.IOStreams{In: in, Out: &out, ErrOut: &out},
	}

	if err := o.Run(context.Background()); err != nil {
		t.Fatalf("Run() error: %v", err)
	}

	if runExists(fc, "run-a", "default") {
		t.Error("run-a should be deleted when the confirmation prompt is accepted")
	}
	if !strings.Contains(out.String(), "Deleted 1 run(s).") {
		t.Errorf("unexpected output: %s", out.String())
	}
}

func TestCleanup_YesSkipsPrompt(t *testing.T) {
	fc := buildFakeClient(
		completedRun("run-a", "default", terminalTime(1*time.Hour)),
	)

	var out bytes.Buffer
	o := &CleanupOptions{
		client:    fc,
		namespace: "default",
		yes:       true,
		// No IOStreams.In set — if the prompt were reached, reading from a
		// nil reader would error out and fail this test.
		IOStreams: genericclioptions.IOStreams{Out: &out, ErrOut: &out},
	}

	if err := o.Run(context.Background()); err != nil {
		t.Fatalf("Run() error: %v", err)
	}

	if runExists(fc, "run-a", "default") {
		t.Error("run-a should be deleted with --yes")
	}
	if strings.Contains(out.String(), "Continue?") {
		t.Errorf("--yes must skip the confirmation prompt, got: %s", out.String())
	}
}

func TestCleanup_DryRunSkipsPrompt(t *testing.T) {
	fc := buildFakeClient(
		completedRun("run-a", "default", terminalTime(1*time.Hour)),
	)

	var out bytes.Buffer
	o := &CleanupOptions{
		client:    fc,
		namespace: "default",
		dryRun:    true,
		// No IOStreams.In set — dry-run must never prompt.
		IOStreams: genericclioptions.IOStreams{Out: &out, ErrOut: &out},
	}

	if err := o.Run(context.Background()); err != nil {
		t.Fatalf("Run() error: %v", err)
	}

	if !runExists(fc, "run-a", "default") {
		t.Error("run-a should NOT be deleted in dry-run")
	}
	if strings.Contains(out.String(), "Continue?") {
		t.Errorf("--dry-run must skip the confirmation prompt, got: %s", out.String())
	}
}

func TestCleanup_ReturnsErrorWhenDeleteFails(t *testing.T) {
	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(
			completedRun("run-a", "default", terminalTime(1*time.Hour)),
			completedRun("run-b", "default", terminalTime(2*time.Hour)),
		).
		WithStatusSubresource(&agenticv1alpha1.AgenticRun{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				if obj.GetName() == "run-a" {
					return errors.New("simulated RBAC denial")
				}
				return c.Delete(ctx, obj, opts...)
			},
		}).
		Build()

	var out bytes.Buffer
	o := &CleanupOptions{
		client:    fc,
		namespace: "default",
		yes:       true,
		IOStreams: genericclioptions.IOStreams{Out: &out, ErrOut: &out},
	}

	err := o.Run(context.Background())
	if err == nil {
		t.Fatal("expected Run() to return an error when a delete fails, got nil")
	}

	if !runExists(fc, "run-a", "default") {
		t.Error("run-a delete failed and should still exist")
	}
	if runExists(fc, "run-b", "default") {
		t.Error("run-b should have been deleted despite run-a's failure")
	}
	if !strings.Contains(out.String(), "Deleted 1 run(s).") {
		t.Errorf("unexpected output: %s", out.String())
	}
}

func TestCleanup_NotFoundTreatedAsSuccess(t *testing.T) {
	// Simulate a race condition where the TTL controller deletes a run
	// between List and Delete. The delete should return NotFound, but
	// cleanup should treat it as success (not count as a failure).
	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(
			completedRun("run-a", "default", terminalTime(1*time.Hour)),
			completedRun("run-b", "default", terminalTime(2*time.Hour)),
		).
		WithStatusSubresource(&agenticv1alpha1.AgenticRun{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				if obj.GetName() == "run-a" {
					// Simulate TTL controller already deleted this run.
					// First actually delete it, then let our delete proceed
					// (which will then fail with NotFound).
					_ = c.Delete(ctx, obj, opts...)
				}
				return c.Delete(ctx, obj, opts...)
			},
		}).
		Build()

	var out, errOut bytes.Buffer
	o := &CleanupOptions{
		client:    fc,
		namespace: "default",
		yes:       true,
		IOStreams: genericclioptions.IOStreams{Out: &out, ErrOut: &errOut},
	}

	err := o.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() should not error when delete returns NotFound, got: %v", err)
	}

	// Both runs should be gone (one via interceptor, one via normal delete).
	if runExists(fc, "run-a", "default") {
		t.Error("run-a should be deleted")
	}
	if runExists(fc, "run-b", "default") {
		t.Error("run-b should be deleted")
	}
	// No warning should be printed for NotFound.
	if strings.Contains(errOut.String(), "Warning") {
		t.Errorf("NotFound should not produce a warning, got: %s", errOut.String())
	}
	// Both should count as deleted.
	if !strings.Contains(out.String(), "Deleted 2 run(s).") {
		t.Errorf("expected 2 deletions, got: %s", out.String())
	}
}

func TestCleanup_Validate_InvalidState(t *testing.T) {
	o := &CleanupOptions{states: "completed,invalid"}
	err := o.Validate()
	if err == nil {
		t.Fatal("expected error for invalid state")
	}
	if !strings.Contains(err.Error(), "invalid state") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestCleanup_Validate_InvalidDuration(t *testing.T) {
	o := &CleanupOptions{olderThan: "abc"}
	err := o.Validate()
	if err == nil {
		t.Fatal("expected error for invalid duration")
	}
	if !strings.Contains(err.Error(), "invalid --older-than") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestParseDuration(t *testing.T) {
	tests := []struct {
		input string
		want  time.Duration
		err   bool
	}{
		{"7d", 7 * 24 * time.Hour, false},
		{"1d", 24 * time.Hour, false},
		{"24h", 24 * time.Hour, false},
		{"30m", 30 * time.Minute, false},
		{"2h30m", 2*time.Hour + 30*time.Minute, false},
		{"abc", 0, true},
		{"10680000000d", 0, true}, // overflows time.Duration (int64 nanoseconds)
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got, err := parseDuration(tt.input)
			if tt.err {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("parseDuration(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}
