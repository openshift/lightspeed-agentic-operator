package agenticrun

import (
	"context"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
)

// AnalysisOutput holds the analysis agent's output.
// ActionRequired is a pointer: nil means "action required" (safe default,
// backward-compatible with agents that don't emit the field).
type AnalysisOutput struct {
	Success        bool
	Summary        string
	ActionRequired *bool
	Options        []agenticv1alpha1.RemediationOption
	Diagnosis      *agenticv1alpha1.DiagnosisResult
}

// IsActionRequired returns true when ActionRequired is nil (safe default)
// or explicitly true.
func (a *AnalysisOutput) IsActionRequired() bool {
	return a.ActionRequired == nil || *a.ActionRequired
}

// ExecutionOutput holds the execution agent's output.
type ExecutionOutput struct {
	Success      bool
	Summary      string
	ActionsTaken []agenticv1alpha1.ExecutionAction
}

// VerificationOutput holds the verification agent's output.
type VerificationOutput struct {
	Success bool
	Checks  []agenticv1alpha1.VerifyCheck
	Summary string
}

// EscalationOutput holds the escalation agent's output.
type EscalationOutput struct {
	Success bool
	Summary string
	Content string
}

// AgentCaller abstracts the agent invocation path. Each method
// launches a sandbox pod with the step's input ConfigMap. The pod
// runs autonomously, creates the Result CR, and exits. The pod
// handler watches for completion and patches the step condition.
type AgentCaller interface {
	Analyze(ctx context.Context, run *agenticv1alpha1.AgenticRun, step resolvedStep) error
	Execute(ctx context.Context, run *agenticv1alpha1.AgenticRun, step resolvedStep, option *agenticv1alpha1.RemediationOption) error
	Verify(ctx context.Context, run *agenticv1alpha1.AgenticRun, step resolvedStep, option *agenticv1alpha1.RemediationOption, exec *ExecutionOutput) error
	Escalate(ctx context.Context, run *agenticv1alpha1.AgenticRun, step resolvedStep) error
	ReleaseSandboxes(ctx context.Context, run *agenticv1alpha1.AgenticRun) error
	ReleaseSandbox(ctx context.Context, run *agenticv1alpha1.AgenticRun, step string) error
}

// StubAgentCaller is a no-op implementation for testing.
type StubAgentCaller struct{}

func (s *StubAgentCaller) Analyze(_ context.Context, _ *agenticv1alpha1.AgenticRun, _ resolvedStep) error {
	return nil
}

func (s *StubAgentCaller) Execute(_ context.Context, _ *agenticv1alpha1.AgenticRun, _ resolvedStep, _ *agenticv1alpha1.RemediationOption) error {
	return nil
}

func (s *StubAgentCaller) Verify(_ context.Context, _ *agenticv1alpha1.AgenticRun, _ resolvedStep, _ *agenticv1alpha1.RemediationOption, _ *ExecutionOutput) error {
	return nil
}

func (s *StubAgentCaller) Escalate(_ context.Context, _ *agenticv1alpha1.AgenticRun, _ resolvedStep) error {
	return nil
}

func (s *StubAgentCaller) ReleaseSandboxes(_ context.Context, _ *agenticv1alpha1.AgenticRun) error {
	return nil
}

func (s *StubAgentCaller) ReleaseSandbox(_ context.Context, _ *agenticv1alpha1.AgenticRun, _ string) error {
	return nil
}
