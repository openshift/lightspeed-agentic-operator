package proposal

import (
	"context"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
)

// AnalysisOutput holds the analysis agent's output.
type AnalysisOutput struct {
	Success bool
	Options []agenticv1alpha1.RemediationOption
}

// ExecutionOutput holds the execution agent's output.
type ExecutionOutput struct {
	Success      bool
	ActionsTaken []agenticv1alpha1.ExecutionAction
	Verification agenticv1alpha1.ExecutionVerification
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

// AgentCaller abstracts the agent invocation path. The reconciler
// passes structured data; the implementation decides how to format
// it for the LLM (text-only prompt vs multimodal with binary
// attachments). In production this manages sandbox lifecycle + HTTP
// calls; in tests a stub returns canned results.
//
// HTTP implementations POST to /v1/agent/run — a step-agnostic
// endpoint where all workflow context is in the request payload.
type AgentCaller interface {
	Analyze(ctx context.Context, proposal *agenticv1alpha1.Proposal, step resolvedStep, requestText string) (*AnalysisOutput, error)
	Execute(ctx context.Context, proposal *agenticv1alpha1.Proposal, step resolvedStep, option *agenticv1alpha1.RemediationOption) (*ExecutionOutput, error)
	Verify(ctx context.Context, proposal *agenticv1alpha1.Proposal, step resolvedStep, option *agenticv1alpha1.RemediationOption, exec *ExecutionOutput) (*VerificationOutput, error)
	Escalate(ctx context.Context, proposal *agenticv1alpha1.Proposal, step resolvedStep, requestText string) (*EscalationOutput, error)
	ReleaseSandboxes(ctx context.Context, proposal *agenticv1alpha1.Proposal) error
}

// StubAgentCaller returns canned success results. Wire in a real
// implementation (sandbox + HTTP) when the agent infrastructure is ready.
type StubAgentCaller struct{}

func (s *StubAgentCaller) Analyze(_ context.Context, _ *agenticv1alpha1.Proposal, _ resolvedStep, _ string) (*AnalysisOutput, error) {
	return &AnalysisOutput{
		Success: true,
		Options: []agenticv1alpha1.RemediationOption{{
			Title: "Stub remediation",
			Diagnosis: agenticv1alpha1.DiagnosisResult{
				Summary:    "Stub diagnosis",
				Confidence: "Medium",
				RootCause:  "Stub root cause",
			},
			Proposal: agenticv1alpha1.ProposalResult{
				Description: "Stub proposal",
				Actions:     []agenticv1alpha1.ProposedAction{{Type: "stub", Description: "Stub action"}},
				Risk:        "Low",
				Reversible:  agenticv1alpha1.ReversibilityReversible,
			},
		}},
	}, nil
}

func (s *StubAgentCaller) Execute(_ context.Context, _ *agenticv1alpha1.Proposal, _ resolvedStep, _ *agenticv1alpha1.RemediationOption) (*ExecutionOutput, error) {
	return &ExecutionOutput{
		Success: true,
		ActionsTaken: []agenticv1alpha1.ExecutionAction{{
			Type:        "stub",
			Description: "Stub execution action",
			Outcome:     agenticv1alpha1.ActionOutcomeSucceeded,
		}},
		Verification: agenticv1alpha1.ExecutionVerification{
			ConditionOutcome: agenticv1alpha1.ConditionOutcomeImproved,
			Summary:          "Stub inline verification passed",
		},
	}, nil
}

func (s *StubAgentCaller) Escalate(_ context.Context, _ *agenticv1alpha1.Proposal, _ resolvedStep, _ string) (*EscalationOutput, error) {
	return &EscalationOutput{
		Success: true,
		Summary: "Stub escalation summary",
		Content: "Stub escalation content",
	}, nil
}

func (s *StubAgentCaller) ReleaseSandboxes(_ context.Context, _ *agenticv1alpha1.Proposal) error {
	return nil
}

func (s *StubAgentCaller) Verify(_ context.Context, _ *agenticv1alpha1.Proposal, _ resolvedStep, _ *agenticv1alpha1.RemediationOption, _ *ExecutionOutput) (*VerificationOutput, error) {
	return &VerificationOutput{
		Success: true,
		Checks:  []agenticv1alpha1.VerifyCheck{{
			Name:   "stub-check",
			Source: "stub",
			Value:  "ok",
			Result: agenticv1alpha1.CheckResultPassed,
		}},
		Summary: "Stub verification passed",
	}, nil
}
