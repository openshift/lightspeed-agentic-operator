package proposal

import (
	"bytes"
	"strings"
	"testing"
	"time"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestIsTerminalPhase(t *testing.T) {
	tests := []struct {
		phase    agenticv1alpha1.ProposalPhase
		terminal bool
	}{
		{agenticv1alpha1.ProposalPhaseCompleted, true},
		{agenticv1alpha1.ProposalPhaseFailed, true},
		{agenticv1alpha1.ProposalPhaseDenied, true},
		{agenticv1alpha1.ProposalPhaseEscalated, true},
		{agenticv1alpha1.ProposalPhasePending, false},
		{agenticv1alpha1.ProposalPhaseAnalyzing, false},
		{agenticv1alpha1.ProposalPhaseExecuting, false},
		{agenticv1alpha1.ProposalPhaseVerifying, false},
	}
	for _, tc := range tests {
		t.Run(string(tc.phase), func(t *testing.T) {
			if got := IsTerminalPhase(tc.phase); got != tc.terminal {
				t.Errorf("IsTerminalPhase(%s) = %v, want %v", tc.phase, got, tc.terminal)
			}
		})
	}
}

func TestPhaseColor(t *testing.T) {
	if c := PhaseColor(agenticv1alpha1.ProposalPhaseCompleted); c != ColorGreen {
		t.Errorf("expected green for Completed, got %q", c)
	}
	if c := PhaseColor(agenticv1alpha1.ProposalPhaseFailed); c != ColorRed {
		t.Errorf("expected red for Failed, got %q", c)
	}
	if c := PhaseColor(agenticv1alpha1.ProposalPhasePending); c != ColorReset {
		t.Errorf("expected reset for Pending, got %q", c)
	}
	if c := PhaseColor(agenticv1alpha1.ProposalPhaseEscalated); c != ColorMagenta {
		t.Errorf("expected magenta for Escalated, got %q", c)
	}
}

func TestColoredPhase(t *testing.T) {
	result := ColoredPhase(agenticv1alpha1.ProposalPhaseCompleted)
	if !strings.Contains(result, "Completed") {
		t.Errorf("ColoredPhase should contain phase name, got %q", result)
	}
	if !strings.HasPrefix(result, ColorGreen) {
		t.Errorf("ColoredPhase(Completed) should start with green, got %q", result)
	}
	if !strings.HasSuffix(result, ColorReset) {
		t.Errorf("ColoredPhase should end with reset, got %q", result)
	}
}

func TestIsValidPhase(t *testing.T) {
	for _, p := range validProposalPhases {
		if !IsValidPhase(p) {
			t.Errorf("expected %q to be valid", p)
		}
	}
	if IsValidPhase("NotAPhase") {
		t.Error("expected NotAPhase to be invalid")
	}
	if IsValidPhase("") {
		t.Error("expected empty string to be invalid")
	}
}

func TestIsValidStep(t *testing.T) {
	tests := []struct {
		step  string
		valid bool
	}{
		{"Analysis", true},
		{"Execution", true},
		{"Verification", true},
		{"analysis", true},
		{"ANALYSIS", true},
		{"invalid", false},
		{"", false},
	}
	for _, tc := range tests {
		t.Run(tc.step, func(t *testing.T) {
			if got := IsValidStep(tc.step); got != tc.valid {
				t.Errorf("IsValidStep(%q) = %v, want %v", tc.step, got, tc.valid)
			}
		})
	}
}

func TestNormalizeStep(t *testing.T) {
	tests := []struct {
		input string
		want  agenticv1alpha1.SandboxStep
	}{
		{"analysis", agenticv1alpha1.SandboxStepAnalysis},
		{"Analysis", agenticv1alpha1.SandboxStepAnalysis},
		{"EXECUTION", agenticv1alpha1.SandboxStepExecution},
		{"verification", agenticv1alpha1.SandboxStepVerification},
	}
	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			if got := NormalizeStep(tc.input); got != tc.want {
				t.Errorf("NormalizeStep(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestValidateOutputFormat(t *testing.T) {
	tests := []struct {
		name      string
		format    string
		allowWide bool
		wantErr   bool
	}{
		{"empty", "", false, false},
		{"json", "json", false, false},
		{"yaml", "yaml", false, false},
		{"wide allowed", "wide", true, false},
		{"wide not allowed", "wide", false, true},
		{"invalid", "xml", false, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateOutputFormat(tc.format, tc.allowWide)
			if (err != nil) != tc.wantErr {
				t.Errorf("ValidateOutputFormat(%q, %v) error = %v, wantErr %v", tc.format, tc.allowWide, err, tc.wantErr)
			}
		})
	}
}

func TestSortProposalsByAge(t *testing.T) {
	now := time.Now()
	items := []agenticv1alpha1.Proposal{
		{ObjectMeta: metav1.ObjectMeta{Name: "old", CreationTimestamp: metav1.NewTime(now.Add(-10 * time.Minute))}},
		{ObjectMeta: metav1.ObjectMeta{Name: "new", CreationTimestamp: metav1.NewTime(now)}},
		{ObjectMeta: metav1.ObjectMeta{Name: "mid", CreationTimestamp: metav1.NewTime(now.Add(-5 * time.Minute))}},
	}
	SortProposalsByAge(items)
	if items[0].Name != "new" || items[1].Name != "mid" || items[2].Name != "old" {
		t.Errorf("expected newest-first order, got %s, %s, %s", items[0].Name, items[1].Name, items[2].Name)
	}
}

func TestMarshalOutput_JSON(t *testing.T) {
	var buf bytes.Buffer
	data := map[string]string{"key": "value"}
	if err := MarshalOutput(&buf, data, OutputJSON); err != nil {
		t.Fatalf("MarshalOutput JSON: %v", err)
	}
	if !strings.Contains(buf.String(), `"key": "value"`) {
		t.Errorf("expected JSON output, got %q", buf.String())
	}
}

func TestMarshalOutput_YAML(t *testing.T) {
	var buf bytes.Buffer
	data := map[string]string{"key": "value"}
	if err := MarshalOutput(&buf, data, OutputYAML); err != nil {
		t.Fatalf("MarshalOutput YAML: %v", err)
	}
	if !strings.Contains(buf.String(), "key: value") {
		t.Errorf("expected YAML output, got %q", buf.String())
	}
}

func TestMarshalOutput_Unknown(t *testing.T) {
	var buf bytes.Buffer
	if err := MarshalOutput(&buf, nil, "xml"); err == nil {
		t.Error("expected error for unknown format")
	}
}

func TestStepStatusFromConditions(t *testing.T) {
	tests := []struct {
		name       string
		conditions []metav1.Condition
		condType   string
		want       string
	}{
		{
			name:       "no conditions",
			conditions: nil,
			condType:   "Analyzed",
			want:       "-",
		},
		{
			name: "condition true",
			conditions: []metav1.Condition{
				{Type: "Analyzed", Status: metav1.ConditionTrue, Reason: "Success"},
			},
			condType: "Analyzed",
			want:     ColorGreen + "True" + ColorReset + " (Success)",
		},
		{
			name: "condition false",
			conditions: []metav1.Condition{
				{Type: "Executed", Status: metav1.ConditionFalse, Reason: "AgentError"},
			},
			condType: "Executed",
			want:     ColorRed + "False" + ColorReset + " (AgentError)",
		},
		{
			name: "condition unknown",
			conditions: []metav1.Condition{
				{Type: "Analyzed", Status: metav1.ConditionUnknown, Reason: "InProgress"},
			},
			condType: "Analyzed",
			want:     ColorYellow + "Unknown" + ColorReset + " (InProgress)",
		},
		{
			name: "wrong type",
			conditions: []metav1.Condition{
				{Type: "Analyzed", Status: metav1.ConditionTrue, Reason: "Done"},
			},
			condType: "Executed",
			want:     "-",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := stepStatusFromConditions(tc.conditions, tc.condType); got != tc.want {
				t.Errorf("stepStatusFromConditions() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestValueOrDash(t *testing.T) {
	if got := valueOrDash(""); got != "-" {
		t.Errorf("valueOrDash(\"\") = %q, want \"-\"", got)
	}
	if got := valueOrDash("hello"); got != "hello" {
		t.Errorf("valueOrDash(\"hello\") = %q, want \"hello\"", got)
	}
}

func TestInt32PtrStr(t *testing.T) {
	if got := int32PtrStr(nil); got != "-" {
		t.Errorf("int32PtrStr(nil) = %q, want \"-\"", got)
	}
	v := int32(5)
	if got := int32PtrStr(&v); got != "5" {
		t.Errorf("int32PtrStr(5) = %q, want \"5\"", got)
	}
}
