package v1alpha1

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func cond(t string, s metav1.ConditionStatus, reason string) metav1.Condition {
	return metav1.Condition{Type: t, Status: s, Reason: reason}
}

func TestDerivePhase(t *testing.T) {
	tests := []struct {
		name       string
		conditions []metav1.Condition
		want       AgenticRunPhase
	}{
		{
			name:       "no conditions",
			conditions: nil,
			want:       AgenticRunPhasePending,
		},
		{
			name:       "empty conditions",
			conditions: []metav1.Condition{},
			want:       AgenticRunPhasePending,
		},
		{
			name: "analyzing",
			conditions: []metav1.Condition{
				cond(AgenticRunConditionAnalyzed, metav1.ConditionUnknown, "InProgress"),
			},
			want: AgenticRunPhaseAnalyzing,
		},
		{
			name: "analysis complete - proposed",
			conditions: []metav1.Condition{
				cond(AgenticRunConditionAnalyzed, metav1.ConditionTrue, "Complete"),
			},
			want: AgenticRunPhaseProposed,
		},
		{
			name: "analysis failed",
			conditions: []metav1.Condition{
				cond(AgenticRunConditionAnalyzed, metav1.ConditionFalse, "Failed"),
			},
			want: AgenticRunPhaseFailed,
		},
		{
			name: "denied",
			conditions: []metav1.Condition{
				cond(AgenticRunConditionDenied, metav1.ConditionTrue, "UserDenied"),
			},
			want: AgenticRunPhaseDenied,
		},
		{
			name: "executing",
			conditions: []metav1.Condition{
				cond(AgenticRunConditionAnalyzed, metav1.ConditionTrue, "Complete"),
				cond(AgenticRunConditionExecuted, metav1.ConditionUnknown, "InProgress"),
			},
			want: AgenticRunPhaseExecuting,
		},
		{
			name: "execution failed",
			conditions: []metav1.Condition{
				cond(AgenticRunConditionAnalyzed, metav1.ConditionTrue, "Complete"),
				cond(AgenticRunConditionExecuted, metav1.ConditionFalse, "Failed"),
			},
			want: AgenticRunPhaseFailed,
		},
		{
			name: "execution complete - verifying",
			conditions: []metav1.Condition{
				cond(AgenticRunConditionAnalyzed, metav1.ConditionTrue, "Complete"),
				cond(AgenticRunConditionExecuted, metav1.ConditionTrue, "Complete"),
			},
			want: AgenticRunPhaseVerifying,
		},
		{
			name: "verifying in progress",
			conditions: []metav1.Condition{
				cond(AgenticRunConditionAnalyzed, metav1.ConditionTrue, "Complete"),
				cond(AgenticRunConditionExecuted, metav1.ConditionTrue, "Complete"),
				cond(AgenticRunConditionVerified, metav1.ConditionUnknown, "InProgress"),
			},
			want: AgenticRunPhaseVerifying,
		},
		{
			name: "verification passed - completed",
			conditions: []metav1.Condition{
				cond(AgenticRunConditionAnalyzed, metav1.ConditionTrue, "Complete"),
				cond(AgenticRunConditionExecuted, metav1.ConditionTrue, "Complete"),
				cond(AgenticRunConditionVerified, metav1.ConditionTrue, "Passed"),
			},
			want: AgenticRunPhaseCompleted,
		},
		{
			name: "verification failed - terminal",
			conditions: []metav1.Condition{
				cond(AgenticRunConditionAnalyzed, metav1.ConditionTrue, "Complete"),
				cond(AgenticRunConditionExecuted, metav1.ConditionTrue, "Complete"),
				cond(AgenticRunConditionVerified, metav1.ConditionFalse, "Failed"),
			},
			want: AgenticRunPhaseFailed,
		},
		{
			name: "verification failed - retrying execution",
			conditions: []metav1.Condition{
				cond(AgenticRunConditionAnalyzed, metav1.ConditionTrue, "Complete"),
				cond(AgenticRunConditionVerified, metav1.ConditionFalse, "RetryingExecution"),
			},
			want: AgenticRunPhaseExecuting,
		},
		{
			name: "verification failed - retries exhausted (without escalated condition)",
			conditions: []metav1.Condition{
				cond(AgenticRunConditionAnalyzed, metav1.ConditionTrue, "Complete"),
				cond(AgenticRunConditionVerified, metav1.ConditionFalse, "RetriesExhausted"),
			},
			want: AgenticRunPhaseFailed,
		},
		{
			name: "advisory completed - exec and verify skipped",
			conditions: []metav1.Condition{
				cond(AgenticRunConditionAnalyzed, metav1.ConditionTrue, "Complete"),
				cond(AgenticRunConditionExecuted, metav1.ConditionTrue, "Skipped"),
				cond(AgenticRunConditionVerified, metav1.ConditionTrue, "Skipped"),
			},
			want: AgenticRunPhaseCompleted,
		},
		{
			name: "escalated",
			conditions: []metav1.Condition{
				cond(AgenticRunConditionAnalyzed, metav1.ConditionTrue, "Complete"),
				cond(AgenticRunConditionEscalated, metav1.ConditionTrue, "MaxAttemptsExhausted"),
			},
			want: AgenticRunPhaseEscalated,
		},
		{
			name: "escalated takes priority over other conditions",
			conditions: []metav1.Condition{
				cond(AgenticRunConditionAnalyzed, metav1.ConditionTrue, "Complete"),
				cond(AgenticRunConditionExecuted, metav1.ConditionFalse, "Failed"),
				cond(AgenticRunConditionEscalated, metav1.ConditionTrue, "MaxAttemptsExhausted"),
			},
			want: AgenticRunPhaseEscalated,
		},
		{
			name: "escalating - in progress",
			conditions: []metav1.Condition{
				cond(AgenticRunConditionEscalated, metav1.ConditionUnknown, "InProgress"),
			},
			want: AgenticRunPhaseEscalating,
		},
		{
			name: "escalating takes priority over verified retries exhausted",
			conditions: []metav1.Condition{
				cond(AgenticRunConditionAnalyzed, metav1.ConditionTrue, "Complete"),
				cond(AgenticRunConditionVerified, metav1.ConditionFalse, "RetriesExhausted"),
				cond(AgenticRunConditionEscalated, metav1.ConditionUnknown, "RetriesExhausted"),
			},
			want: AgenticRunPhaseEscalating,
		},
		{
			name: "escalation failed",
			conditions: []metav1.Condition{
				cond(AgenticRunConditionEscalated, metav1.ConditionFalse, "Failed"),
			},
			want: AgenticRunPhaseFailed,
		},
		{
			name: "denied takes priority over analyzed",
			conditions: []metav1.Condition{
				cond(AgenticRunConditionAnalyzed, metav1.ConditionTrue, "Complete"),
				cond(AgenticRunConditionDenied, metav1.ConditionTrue, "UserDenied"),
			},
			want: AgenticRunPhaseDenied,
		},
		{
			name: "emergency stopped",
			conditions: []metav1.Condition{
				cond(AgenticRunConditionEmergencyStopped, metav1.ConditionTrue, "SystemSuspended"),
			},
			want: AgenticRunPhaseEmergencyStopped,
		},
		{
			name: "emergency stopped takes priority over analyzed",
			conditions: []metav1.Condition{
				cond(AgenticRunConditionAnalyzed, metav1.ConditionTrue, "Complete"),
				cond(AgenticRunConditionEmergencyStopped, metav1.ConditionTrue, "SystemSuspended"),
			},
			want: AgenticRunPhaseEmergencyStopped,
		},
		{
			name: "emergency stopped takes priority over escalated",
			conditions: []metav1.Condition{
				cond(AgenticRunConditionEscalated, metav1.ConditionTrue, "MaxAttemptsExhausted"),
				cond(AgenticRunConditionEmergencyStopped, metav1.ConditionTrue, "SystemSuspended"),
			},
			want: AgenticRunPhaseEmergencyStopped,
		},
		{
			name: "emergency stopped takes priority over denied",
			conditions: []metav1.Condition{
				cond(AgenticRunConditionDenied, metav1.ConditionTrue, "UserDenied"),
				cond(AgenticRunConditionEmergencyStopped, metav1.ConditionTrue, "SystemSuspended"),
			},
			want: AgenticRunPhaseEmergencyStopped,
		},
		{
			name: "emergency stopped false does not affect phase",
			conditions: []metav1.Condition{
				cond(AgenticRunConditionAnalyzed, metav1.ConditionTrue, "Complete"),
				cond(AgenticRunConditionEmergencyStopped, metav1.ConditionFalse, ""),
			},
			want: AgenticRunPhaseProposed,
		},
		{
			name: "no action required",
			conditions: []metav1.Condition{
				cond(AgenticRunConditionAnalyzed, metav1.ConditionTrue, ReasonNoActionRequired),
			},
			want: AgenticRunPhaseNoActionRequired,
		},
		{
			name: "no action required overridden by denied",
			conditions: []metav1.Condition{
				cond(AgenticRunConditionAnalyzed, metav1.ConditionTrue, ReasonNoActionRequired),
				cond(AgenticRunConditionDenied, metav1.ConditionTrue, "UserDenied"),
			},
			want: AgenticRunPhaseDenied,
		},
		{
			name: "no action required overridden by emergency stopped",
			conditions: []metav1.Condition{
				cond(AgenticRunConditionAnalyzed, metav1.ConditionTrue, ReasonNoActionRequired),
				cond(AgenticRunConditionEmergencyStopped, metav1.ConditionTrue, "SystemSuspended"),
			},
			want: AgenticRunPhaseEmergencyStopped,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := DerivePhase(tt.conditions)
			if got != tt.want {
				t.Errorf("DerivePhase() = %q, want %q", got, tt.want)
			}
		})
	}
}
