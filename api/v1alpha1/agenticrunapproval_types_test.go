package v1alpha1

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestNewApprovalStage_MarshalsDiscriminantArms(t *testing.T) {
	tests := []struct {
		name        string
		stage       ApprovalStage
		wantContain []string
		wantOmit    []string
	}{
		{
			name:        "analysis empty agent",
			stage:       NewApprovalStage(ApprovalStageAnalysis, "", "", nil, 0),
			wantContain: []string{`"type":"Analysis"`, `"analysis":{}`},
			wantOmit:    []string{`"execution"`, `"verification"`, `"escalation"`, `"agent"`},
		},
		{
			name:        "analysis with agent",
			stage:       NewApprovalStage(ApprovalStageAnalysis, "", "fast", nil, 0),
			wantContain: []string{`"type":"Analysis"`, `"analysis":{"agent":"fast"}`},
		},
		{
			name:        "verification empty agent",
			stage:       NewApprovalStage(ApprovalStageVerification, "", "", nil, 0),
			wantContain: []string{`"type":"Verification"`, `"verification":{}`},
			wantOmit:    []string{`"analysis"`, `"execution"`, `"escalation"`},
		},
		{
			name:        "execution option zero",
			stage:       NewApprovalStage(ApprovalStageExecution, "", "", ptrInt32(0), 0),
			wantContain: []string{`"type":"Execution"`, `"execution":{"option":0}`},
		},
		{
			name:        "deny analysis",
			stage:       NewApprovalStage(ApprovalStageAnalysis, ApprovalDecisionDenied, "", nil, 0),
			wantContain: []string{`"type":"Analysis"`, `"decision":"Denied"`, `"analysis":{}`},
			wantOmit:    []string{`"agent"`},
		},
		{
			name:        "escalation empty",
			stage:       NewApprovalStage(ApprovalStageEscalation, "", "", nil, 0),
			wantContain: []string{`"type":"Escalation"`, `"escalation":{}`},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b, err := json.Marshal(tc.stage)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			got := string(b)
			for _, want := range tc.wantContain {
				if !strings.Contains(got, want) {
					t.Errorf("marshaled JSON %s missing %q", got, want)
				}
			}
			for _, omit := range tc.wantOmit {
				if strings.Contains(got, omit) {
					t.Errorf("marshaled JSON %s should omit %q", got, omit)
				}
			}
		})
	}
}

func ptrInt32(v int32) *int32 { return &v }
