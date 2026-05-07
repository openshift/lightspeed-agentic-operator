package proposal

import (
	"strings"
	"testing"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
)

func TestLogs_Validate(t *testing.T) {
	tests := []struct {
		name    string
		step    string
		wantErr bool
	}{
		{"empty step", "", false},
		{"Analysis", "Analysis", false},
		{"Execution", "Execution", false},
		{"Verification", "Verification", false},
		{"lowercase", "analysis", false},
		{"uppercase", "ANALYSIS", false},
		{"invalid", "invalid", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			o := &LogsOptions{step: tc.step}
			err := o.Validate()
			if (err != nil) != tc.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func TestLogs_ValidateErrorMessage(t *testing.T) {
	o := &LogsOptions{step: "badvalue"}
	err := o.Validate()
	if err == nil {
		t.Fatal("expected error")
	}
	for _, expected := range []string{"Analysis", "Execution", "Verification"} {
		if !strings.Contains(err.Error(), expected) {
			t.Errorf("error should list valid steps, missing %q: %v", expected, err)
		}
	}
}

func TestLogs_ResolveSandbox_ExplicitStep(t *testing.T) {
	p := testProposalWithStatus("test", "default", agenticv1alpha1.ProposalPhaseExecuting)
	p.Status.Steps.Analysis.Sandbox = agenticv1alpha1.SandboxInfo{ClaimName: "analysis-pod"}
	p.Status.Steps.Execution.Sandbox = agenticv1alpha1.SandboxInfo{ClaimName: "exec-pod"}
	p.Status.Steps.Verification.Sandbox = agenticv1alpha1.SandboxInfo{ClaimName: "verify-pod"}

	tests := []struct {
		step string
		want string
	}{
		{"Analysis", "analysis-pod"},
		{"Execution", "exec-pod"},
		{"Verification", "verify-pod"},
		{"analysis", "analysis-pod"},
	}
	for _, tc := range tests {
		t.Run(tc.step, func(t *testing.T) {
			o := &LogsOptions{step: tc.step}
			sandbox := o.resolveSandbox(p)
			if sandbox == nil {
				t.Fatal("expected non-nil sandbox")
			}
			if sandbox.ClaimName != tc.want {
				t.Errorf("expected claim %q, got %q", tc.want, sandbox.ClaimName)
			}
		})
	}
}

func TestLogs_ResolveSandbox_AutoDetect(t *testing.T) {
	tests := []struct {
		name       string
		analysis   string
		execution  string
		verify     string
		wantClaim  string
		wantNil    bool
	}{
		{"prefer verification", "a-pod", "e-pod", "v-pod", "v-pod", false},
		{"fallback to execution", "a-pod", "e-pod", "", "e-pod", false},
		{"fallback to analysis", "a-pod", "", "", "a-pod", false},
		{"no sandbox", "", "", "", "", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := testProposalWithStatus("test", "default", agenticv1alpha1.ProposalPhaseVerifying)
			p.Status.Steps.Analysis.Sandbox = agenticv1alpha1.SandboxInfo{ClaimName: tc.analysis}
			p.Status.Steps.Execution.Sandbox = agenticv1alpha1.SandboxInfo{ClaimName: tc.execution}
			p.Status.Steps.Verification.Sandbox = agenticv1alpha1.SandboxInfo{ClaimName: tc.verify}

			o := &LogsOptions{}
			sandbox := o.resolveSandbox(p)
			if tc.wantNil {
				if sandbox != nil {
					t.Errorf("expected nil sandbox, got %+v", sandbox)
				}
				return
			}
			if sandbox == nil {
				t.Fatal("expected non-nil sandbox")
			}
			if sandbox.ClaimName != tc.wantClaim {
				t.Errorf("expected claim %q, got %q", tc.wantClaim, sandbox.ClaimName)
			}
		})
	}
}
