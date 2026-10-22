package v1alpha1

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

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
				cond(AgenticRunConditionEscalated, metav1.ConditionTrue, "VerificationFailed"),
			},
			want: AgenticRunPhaseEscalated,
		},
		{
			name: "escalated takes priority over other conditions",
			conditions: []metav1.Condition{
				cond(AgenticRunConditionAnalyzed, metav1.ConditionTrue, "Complete"),
				cond(AgenticRunConditionExecuted, metav1.ConditionFalse, "Failed"),
				cond(AgenticRunConditionEscalated, metav1.ConditionTrue, "VerificationFailed"),
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
			name: "escalating takes priority over verified false",
			conditions: []metav1.Condition{
				cond(AgenticRunConditionAnalyzed, metav1.ConditionTrue, "Complete"),
				cond(AgenticRunConditionVerified, metav1.ConditionFalse, ReasonVerificationFailed),
				cond(AgenticRunConditionEscalated, metav1.ConditionUnknown, ReasonVerificationFailed),
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
				cond(AgenticRunConditionEscalated, metav1.ConditionTrue, "VerificationFailed"),
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
			want: AgenticRunPhaseCompleted,
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
		{
			name: "stopping does not create a phase",
			conditions: []metav1.Condition{
				cond(AgenticRunConditionStopping, metav1.ConditionTrue, "Stopping"),
			},
			want: AgenticRunPhasePending,
		},
		{
			name: "stopping preserves the active phase",
			conditions: []metav1.Condition{
				cond(AgenticRunConditionAnalyzed, metav1.ConditionUnknown, "InProgress"),
				cond(AgenticRunConditionStopping, metav1.ConditionTrue, "Stopping"),
			},
			want: AgenticRunPhaseAnalyzing,
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

// A client must be able to request days and read the fixed deadline without
// depending on the removed seconds-based TTL or cluster lifecycle config.
func TestTerminalTTLJSONContract(t *testing.T) {
	days := int32(3)
	deadline := metav1.NewTime(time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC))
	run := AgenticRun{
		Spec:   AgenticRunSpec{TerminalTTL: &days},
		Status: AgenticRunStatus{DeleteAfter: &deadline},
	}
	data, err := json.Marshal(run)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{`"terminalTTL":3`, `"deleteAfter":"2026-09-28T12:00:00Z"`} {
		if !strings.Contains(string(data), field) {
			t.Errorf("run JSON %s missing %s", data, field)
		}
	}
	var decoded AgenticRun
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Spec.TerminalTTL == nil || *decoded.Spec.TerminalTTL != 3 || decoded.Status.DeleteAfter == nil || !decoded.Status.DeleteAfter.Equal(&deadline) {
		t.Errorf("round trip lost TTL request or deadline: %#v", decoded)
	}
	copy := decoded.DeepCopy()
	*copy.Spec.TerminalTTL = 5
	copy.Status.DeleteAfter.Time = copy.Status.DeleteAfter.Add(24 * time.Hour)
	if *decoded.Spec.TerminalTTL != 3 || !decoded.Status.DeleteAfter.Equal(&deadline) {
		t.Error("deep copy shares TTL request or deadline with original")
	}
}

func TestLegacyLifecycleFieldIsNotExposed(t *testing.T) {
	var config AgenticOLSConfig
	if err := json.Unmarshal([]byte(`{"spec":{"suspended":false,"lifecycle":{"terminalTTL":60}}}`), &config); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), `"lifecycle"`) || strings.Contains(string(data), `"terminalTTL"`) {
		t.Errorf("removed lifecycle field is still exposed: %s", data)
	}
}
