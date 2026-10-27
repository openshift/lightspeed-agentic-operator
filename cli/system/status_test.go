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

func TestStatus_ResumeSafety(t *testing.T) {
	const generation int64 = 5

	currentSafety := metav1.Condition{
		Type:               agenticv1alpha1.AgenticOLSConfigConditionSafeToResume,
		Status:             metav1.ConditionTrue,
		Reason:             "RunsStopped",
		Message:            "affected runs stopped",
		ObservedGeneration: generation,
	}
	staleSafety := currentSafety
	staleSafety.ObservedGeneration = generation - 1
	falseSafety := currentSafety
	falseSafety.Status = metav1.ConditionFalse
	falseSafety.Reason = "CleanupPending"
	falseSafety.Message = "sandbox cleanup is pending"

	tests := []struct {
		name             string
		suspendedReason  string
		suspendedMessage string
		safety           *metav1.Condition
		want             []string
	}{
		{
			name:             "current terminal safety is confirmed",
			suspendedReason:  "AdminActivated",
			suspendedMessage: "suspension activated",
			safety:           &currentSafety,
			want:             []string{"Resumption safety: confirmed"},
		},
		{
			name:             "missing safety is unconfirmed",
			suspendedReason:  "AdminActivated",
			suspendedMessage: "suspension activated",
			want: []string{
				"Resumption safety: unconfirmed",
				"SafeToResume condition is missing",
			},
		},
		{
			name:             "stale safety is unconfirmed",
			suspendedReason:  "AdminActivated",
			suspendedMessage: "suspension activated",
			safety:           &staleSafety,
			want: []string{
				"Resumption safety: unconfirmed",
				"SafeToResume observed generation 4",
				"current generation 5",
			},
		},
		{
			name:             "false safety is unconfirmed",
			suspendedReason:  "AdminActivated",
			suspendedMessage: "suspension activated",
			safety:           &falseSafety,
			want: []string{
				"Resumption safety: unconfirmed",
				"CleanupPending",
				"sandbox cleanup is pending",
			},
		},
		{
			name:             "stopping is unconfirmed even with safe condition",
			suspendedReason:  "Stopping",
			suspendedMessage: "waiting for stop acceptance",
			safety:           &currentSafety,
			want: []string{
				"stopping",
				"Resumption safety: unconfirmed",
				"Suspended reason is Stopping",
			},
		},
		{
			name:             "error diagnostics remain visible after safety recovers",
			suspendedReason:  "AdminActivatedWithErrors",
			suspendedMessage: "run stop request failed",
			safety:           &currentSafety,
			want: []string{
				"completed with errors",
				"run stop request failed",
				"Resumption safety: confirmed",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := testConfig(true)
			cfg.Generation = generation
			cfg.Status.Conditions = []metav1.Condition{{
				Type:               agenticv1alpha1.AgenticOLSConfigConditionSuspended,
				Status:             metav1.ConditionTrue,
				Reason:             tt.suspendedReason,
				Message:            tt.suspendedMessage,
				ObservedGeneration: generation,
			}}
			if tt.safety != nil {
				cfg.Status.Conditions = append(cfg.Status.Conditions, *tt.safety)
			}

			streams, out, _ := fakeStreams()
			fc := fake.NewClientBuilder().WithScheme(testScheme()).
				WithObjects(cfg).Build()
			o := &StatusOptions{client: fc, IOStreams: streams, now: time.Now}
			if err := o.Run(context.Background()); err != nil {
				t.Fatalf("Run: %v", err)
			}

			got := out.String()
			for _, want := range tt.want {
				if !strings.Contains(got, want) {
					t.Errorf("output %q does not contain %q", got, want)
				}
			}
		})
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
