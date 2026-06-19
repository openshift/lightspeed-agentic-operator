package system

import (
	"context"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
)

func TestStatus_Active(t *testing.T) {
	streams, out, _ := fakeStreams()
	fc := fake.NewClientBuilder().WithScheme(testScheme()).
		WithObjects(testConfig(false)).Build()

	o := &StatusOptions{client: fc, IOStreams: streams, now: time.Now}
	if err := o.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got := out.String(); !strings.Contains(got, "Active") {
		t.Errorf("expected Active, got %q", got)
	}
}

func TestStatus_Suspended(t *testing.T) {
	streams, out, _ := fakeStreams()
	fc := fake.NewClientBuilder().WithScheme(testScheme()).
		WithObjects(testConfig(true)).Build()

	o := &StatusOptions{client: fc, IOStreams: streams, now: time.Now}
	if err := o.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got := out.String(); !strings.Contains(got, "SUSPENDED") {
		t.Errorf("expected SUSPENDED, got %q", got)
	}
}

func TestStatus_SuspendedWithCondition(t *testing.T) {
	streams, out, _ := fakeStreams()
	transition := metav1.NewTime(time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC))
	now := transition.Add(14 * time.Minute)

	cfg := testConfig(true)
	cfg.Status.Conditions = []metav1.Condition{{
		Type:               agenticv1alpha1.AgenticOLSConfigConditionSuspended,
		Status:             metav1.ConditionTrue,
		Reason:             "AdminActivated",
		Message:            "System suspended; 2 proposals emergency-stopped",
		LastTransitionTime: transition,
	}}

	fc := fake.NewClientBuilder().WithScheme(testScheme()).
		WithObjects(cfg).Build()

	o := &StatusOptions{client: fc, IOStreams: streams, now: func() time.Time { return now }}
	if err := o.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	got := strings.TrimSpace(out.String())
	want := "Agentic System: SUSPENDED (since 14m ago, 2026-06-19T12:00:00Z, System suspended; 2 proposals emergency-stopped)"
	if got != want {
		t.Errorf("output = %q, want %q", got, want)
	}
}

func TestStatus_ActiveWithDeactivationCondition(t *testing.T) {
	streams, out, _ := fakeStreams()
	transition := metav1.NewTime(time.Date(2026, 6, 19, 10, 0, 0, 0, time.UTC))
	now := transition.Add(2 * time.Hour)

	cfg := testConfig(false)
	cfg.Status.Conditions = []metav1.Condition{{
		Type:               agenticv1alpha1.AgenticOLSConfigConditionSuspended,
		Status:             metav1.ConditionFalse,
		Reason:             "AdminDeactivated",
		LastTransitionTime: transition,
	}}

	fc := fake.NewClientBuilder().WithScheme(testScheme()).
		WithObjects(cfg).Build()

	o := &StatusOptions{client: fc, IOStreams: streams, now: func() time.Time { return now }}
	if err := o.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	got := strings.TrimSpace(out.String())
	want := "Agentic System: Active (resumed 2h ago, 2026-06-19T10:00:00Z)"
	if got != want {
		t.Errorf("output = %q, want %q", got, want)
	}
}

func TestStatus_NoCR(t *testing.T) {
	streams, out, _ := fakeStreams()
	fc := fake.NewClientBuilder().WithScheme(testScheme()).Build()

	o := &StatusOptions{client: fc, IOStreams: streams, now: time.Now}
	if err := o.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got := out.String(); !strings.Contains(got, "Active") {
		t.Errorf("expected Active when no CR exists, got %q", got)
	}
}

func TestFormatDuration(t *testing.T) {
	tests := []struct {
		name string
		d    time.Duration
		want string
	}{
		{name: "seconds", d: 45 * time.Second, want: "45s"},
		{name: "minutes", d: 14 * time.Minute, want: "14m"},
		{name: "hours", d: 2 * time.Hour, want: "2h"},
		{name: "days", d: 72 * time.Hour, want: "3d"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := formatDuration(tt.d); got != tt.want {
				t.Errorf("formatDuration(%v) = %q, want %q", tt.d, got, tt.want)
			}
		})
	}
}
