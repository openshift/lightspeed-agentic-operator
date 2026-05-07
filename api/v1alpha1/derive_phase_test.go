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
		want       ProposalPhase
	}{
		{
			name:       "no conditions",
			conditions: nil,
			want:       ProposalPhasePending,
		},
		{
			name:       "empty conditions",
			conditions: []metav1.Condition{},
			want:       ProposalPhasePending,
		},
		{
			name: "analyzing",
			conditions: []metav1.Condition{
				cond(ProposalConditionAnalyzed, metav1.ConditionUnknown, "InProgress"),
			},
			want: ProposalPhaseAnalyzing,
		},
		{
			name: "analysis complete - proposed",
			conditions: []metav1.Condition{
				cond(ProposalConditionAnalyzed, metav1.ConditionTrue, "Complete"),
			},
			want: ProposalPhaseProposed,
		},
		{
			name: "analysis failed",
			conditions: []metav1.Condition{
				cond(ProposalConditionAnalyzed, metav1.ConditionFalse, "Failed"),
			},
			want: ProposalPhaseFailed,
		},
		{
			name: "denied",
			conditions: []metav1.Condition{
				cond(ProposalConditionDenied, metav1.ConditionTrue, "UserDenied"),
			},
			want: ProposalPhaseDenied,
		},
		{
			name: "executing",
			conditions: []metav1.Condition{
				cond(ProposalConditionAnalyzed, metav1.ConditionTrue, "Complete"),
				cond(ProposalConditionExecuted, metav1.ConditionUnknown, "InProgress"),
			},
			want: ProposalPhaseExecuting,
		},
		{
			name: "execution failed",
			conditions: []metav1.Condition{
				cond(ProposalConditionAnalyzed, metav1.ConditionTrue, "Complete"),
				cond(ProposalConditionExecuted, metav1.ConditionFalse, "Failed"),
			},
			want: ProposalPhaseFailed,
		},
		{
			name: "execution complete - verifying",
			conditions: []metav1.Condition{
				cond(ProposalConditionAnalyzed, metav1.ConditionTrue, "Complete"),
				cond(ProposalConditionExecuted, metav1.ConditionTrue, "Complete"),
			},
			want: ProposalPhaseVerifying,
		},
		{
			name: "verifying in progress",
			conditions: []metav1.Condition{
				cond(ProposalConditionAnalyzed, metav1.ConditionTrue, "Complete"),
				cond(ProposalConditionExecuted, metav1.ConditionTrue, "Complete"),
				cond(ProposalConditionVerified, metav1.ConditionUnknown, "InProgress"),
			},
			want: ProposalPhaseVerifying,
		},
		{
			name: "verification passed - completed",
			conditions: []metav1.Condition{
				cond(ProposalConditionAnalyzed, metav1.ConditionTrue, "Complete"),
				cond(ProposalConditionExecuted, metav1.ConditionTrue, "Complete"),
				cond(ProposalConditionVerified, metav1.ConditionTrue, "Passed"),
			},
			want: ProposalPhaseCompleted,
		},
		{
			name: "verification failed - terminal",
			conditions: []metav1.Condition{
				cond(ProposalConditionAnalyzed, metav1.ConditionTrue, "Complete"),
				cond(ProposalConditionExecuted, metav1.ConditionTrue, "Complete"),
				cond(ProposalConditionVerified, metav1.ConditionFalse, "Failed"),
			},
			want: ProposalPhaseFailed,
		},
		{
			name: "verification failed - retrying execution",
			conditions: []metav1.Condition{
				cond(ProposalConditionAnalyzed, metav1.ConditionTrue, "Complete"),
				cond(ProposalConditionVerified, metav1.ConditionFalse, "RetryingExecution"),
			},
			want: ProposalPhaseExecuting,
		},
		{
			name: "verification failed - retries exhausted (without escalated condition)",
			conditions: []metav1.Condition{
				cond(ProposalConditionAnalyzed, metav1.ConditionTrue, "Complete"),
				cond(ProposalConditionVerified, metav1.ConditionFalse, "RetriesExhausted"),
			},
			want: ProposalPhaseFailed,
		},
		{
			name: "advisory completed - exec and verify skipped",
			conditions: []metav1.Condition{
				cond(ProposalConditionAnalyzed, metav1.ConditionTrue, "Complete"),
				cond(ProposalConditionExecuted, metav1.ConditionTrue, "Skipped"),
				cond(ProposalConditionVerified, metav1.ConditionTrue, "Skipped"),
			},
			want: ProposalPhaseCompleted,
		},
		{
			name: "escalated",
			conditions: []metav1.Condition{
				cond(ProposalConditionAnalyzed, metav1.ConditionTrue, "Complete"),
				cond(ProposalConditionEscalated, metav1.ConditionTrue, "MaxAttemptsExhausted"),
			},
			want: ProposalPhaseEscalated,
		},
		{
			name: "escalated takes priority over other conditions",
			conditions: []metav1.Condition{
				cond(ProposalConditionAnalyzed, metav1.ConditionTrue, "Complete"),
				cond(ProposalConditionExecuted, metav1.ConditionFalse, "Failed"),
				cond(ProposalConditionEscalated, metav1.ConditionTrue, "MaxAttemptsExhausted"),
			},
			want: ProposalPhaseEscalated,
		},
		{
			name: "escalating - in progress",
			conditions: []metav1.Condition{
				cond(ProposalConditionEscalated, metav1.ConditionUnknown, "InProgress"),
			},
			want: ProposalPhaseEscalating,
		},
		{
			name: "escalating takes priority over verified retries exhausted",
			conditions: []metav1.Condition{
				cond(ProposalConditionAnalyzed, metav1.ConditionTrue, "Complete"),
				cond(ProposalConditionVerified, metav1.ConditionFalse, "RetriesExhausted"),
				cond(ProposalConditionEscalated, metav1.ConditionUnknown, "RetriesExhausted"),
			},
			want: ProposalPhaseEscalating,
		},
		{
			name: "escalation failed",
			conditions: []metav1.Condition{
				cond(ProposalConditionEscalated, metav1.ConditionFalse, "Failed"),
			},
			want: ProposalPhaseFailed,
		},
		{
			name: "denied takes priority over analyzed",
			conditions: []metav1.Condition{
				cond(ProposalConditionAnalyzed, metav1.ConditionTrue, "Complete"),
				cond(ProposalConditionDenied, metav1.ConditionTrue, "UserDenied"),
			},
			want: ProposalPhaseDenied,
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
