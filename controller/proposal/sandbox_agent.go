package proposal

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
)

const (
	defaultSandboxTimeout   = 5 * time.Minute
	defaultBaseTemplateName = "lightspeed-agent"

	ErrAnalysisAgentCall         = "analysis agent call"
	ErrParseAnalysisResponse     = "parse analysis response"
	ErrExecutionAgentCall        = "execution agent call"
	ErrParseExecutionResponse    = "parse execution response"
	ErrVerificationAgentCall     = "verification agent call"
	ErrParseVerificationResponse = "parse verification response"
	ErrEscalationAgentCall       = "escalation agent call"
	ErrParseEscalationResponse   = "parse escalation response"
	ErrClaimSandbox              = "claim sandbox"
	ErrWaitForSandbox            = "wait for sandbox"
)

type analysisResponse struct {
	Success bool                                `json:"success"`
	Options []agenticv1alpha1.RemediationOption `json:"options"`
}

type executionResponse struct {
	Success      bool                                   `json:"success"`
	ActionsTaken []agenticv1alpha1.ExecutionAction      `json:"actionsTaken"`
	Verification *agenticv1alpha1.ExecutionVerification `json:"verification,omitempty"`
}

type verificationResponse struct {
	Success bool                          `json:"success"`
	Checks  []agenticv1alpha1.VerifyCheck `json:"checks"`
	Summary string                        `json:"summary"`
}

// SandboxAgentCaller implements AgentCaller by claiming a sandbox pod,
// calling the agent HTTP service, and releasing the sandbox on completion.
type SandboxAgentCaller struct {
	Sandbox       SandboxProvider
	K8sClient     client.Client
	ClientFactory func(endpoint string) AgentHTTPClientInterface
	Namespace     string
	Timeout       time.Duration
}

func NewSandboxAgentCaller(
	sandbox SandboxProvider,
	k8sClient client.Client,
	clientFactory func(endpoint string) AgentHTTPClientInterface,
	namespace string,
) *SandboxAgentCaller {
	return &SandboxAgentCaller{
		Sandbox:       sandbox,
		K8sClient:     k8sClient,
		ClientFactory: clientFactory,
		Namespace:     namespace,
		Timeout:       defaultSandboxTimeout,
	}
}

func stepString(step agenticv1alpha1.SandboxStep) string {
	return strings.ToLower(string(step))
}

func (s *SandboxAgentCaller) Analyze(ctx context.Context, proposal *agenticv1alpha1.Proposal, step resolvedStep, requestText string, serviceAccount string) (*AnalysisOutput, error) {
	query := buildAnalysisQuery(requestText, proposal)
	raw, err := s.callWithSandbox(ctx, proposal, stepString(agenticv1alpha1.SandboxStepAnalysis), step, query, buildAgentContext(proposal), serviceAccount)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", ErrAnalysisAgentCall, err)
	}

	var resp analysisResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("%s: %w", ErrParseAnalysisResponse, err)
	}

	return &AnalysisOutput{
		Success: resp.Success,
		Options: resp.Options,
	}, nil
}

func (s *SandboxAgentCaller) Execute(ctx context.Context, proposal *agenticv1alpha1.Proposal, step resolvedStep, option *agenticv1alpha1.RemediationOption, serviceAccount string) (*ExecutionOutput, error) {
	agentCtx := buildAgentContext(proposal)
	if option != nil {
		agentCtx.ApprovedOption = option
	}

	query := buildExecutionQuery(option)
	raw, err := s.callWithSandbox(ctx, proposal, stepString(agenticv1alpha1.SandboxStepExecution), step, query, agentCtx, serviceAccount)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", ErrExecutionAgentCall, err)
	}

	var resp executionResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("%s: %w", ErrParseExecutionResponse, err)
	}

	out := &ExecutionOutput{
		Success:      resp.Success,
		ActionsTaken: resp.ActionsTaken,
	}
	if resp.Verification != nil {
		out.Verification = *resp.Verification
	}
	return out, nil
}

func (s *SandboxAgentCaller) Verify(ctx context.Context, proposal *agenticv1alpha1.Proposal, step resolvedStep, option *agenticv1alpha1.RemediationOption, exec *ExecutionOutput, serviceAccount string) (*VerificationOutput, error) {
	agentCtx := buildAgentContext(proposal)
	if option != nil {
		agentCtx.ApprovedOption = option
	}
	agentCtx.ExecutionResult = executionOutputToAgentResult(exec)

	query := buildVerificationQuery(option, exec)
	raw, err := s.callWithSandbox(ctx, proposal, stepString(agenticv1alpha1.SandboxStepVerification), step, query, agentCtx, serviceAccount)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", ErrVerificationAgentCall, err)
	}

	var resp verificationResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("%s: %w", ErrParseVerificationResponse, err)
	}

	return &VerificationOutput{
		Success: resp.Success,
		Checks:  resp.Checks,
		Summary: resp.Summary,
	}, nil
}

func (s *SandboxAgentCaller) Escalate(ctx context.Context, proposal *agenticv1alpha1.Proposal, step resolvedStep, requestText string, serviceAccount string) (*EscalationOutput, error) {
	agentCtx := buildAgentContext(proposal)
	raw, err := s.callWithSandbox(ctx, proposal, stepString(agenticv1alpha1.SandboxStepEscalation), step, requestText, agentCtx, serviceAccount)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", ErrEscalationAgentCall, err)
	}

	var resp struct {
		Success bool   `json:"success"`
		Summary string `json:"summary"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("%s: %w", ErrParseEscalationResponse, err)
	}

	return &EscalationOutput{
		Success: resp.Success,
		Summary: resp.Summary,
		Content: resp.Content,
	}, nil
}

func (s *SandboxAgentCaller) callWithSandbox(
	ctx context.Context,
	proposal *agenticv1alpha1.Proposal,
	stepName string,
	step resolvedStep,
	query string,
	agentCtx *agentContext,
	serviceAccount string,
) (json.RawMessage, error) {
	s.Sandbox.SetStep(step.Agent, step.LLM, step.Tools, serviceAccount)

	claimName, err := s.Sandbox.Claim(ctx, proposal.Name, stepName, "")
	if err != nil {
		return nil, fmt.Errorf("%s: %w", ErrClaimSandbox, err)
	}

	// Write sandbox info immediately so the console can stream logs
	// while the sandbox is still starting up
	s.patchSandboxInfo(ctx, proposal, stepName, claimName)

	timeout := s.Timeout
	if timeout == 0 {
		timeout = defaultSandboxTimeout
	}

	endpoint, err := s.Sandbox.WaitReady(ctx, claimName, timeout)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", ErrWaitForSandbox, err)
	}

	agentURL := endpoint
	if !strings.HasPrefix(endpoint, "http") {
		agentURL = fmt.Sprintf("http://%s:8080", endpoint)
	}

	schema := outputSchemaForStep(stepName, proposal)

	client := s.ClientFactory(agentURL)
	resp, err := client.Run(ctx, "", query, schema, agentCtx)
	if err != nil {
		return nil, err
	}

	return resp.Response, nil
}

func (s *SandboxAgentCaller) ReleaseSandboxes(ctx context.Context, proposal *agenticv1alpha1.Proposal) error {
	log := logf.FromContext(ctx)
	var firstErr error

	for _, info := range []agenticv1alpha1.SandboxInfo{
		proposal.Status.Steps.Analysis.Sandbox,
		proposal.Status.Steps.Execution.Sandbox,
		proposal.Status.Steps.Verification.Sandbox,
		proposal.Status.Steps.Escalation.Sandbox,
	} {
		if info.ClaimName == "" {
			continue
		}
		if err := s.Sandbox.Release(ctx, info.ClaimName); err != nil {
			log.Error(err, "failed to release sandbox", LogKeyClaim, info.ClaimName)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

func (s *SandboxAgentCaller) patchSandboxInfo(ctx context.Context, proposal *agenticv1alpha1.Proposal, step, claimName string) {
	log := logf.FromContext(ctx)

	var current agenticv1alpha1.Proposal
	if err := s.K8sClient.Get(ctx, client.ObjectKeyFromObject(proposal), &current); err != nil {
		log.Error(err, "failed to get proposal for sandbox info patch")
		return
	}

	base := current.DeepCopy()
	info := agenticv1alpha1.SandboxInfo{
		ClaimName: claimName,
		Namespace: s.Namespace,
	}

	switch step {
	case "analysis":
		current.Status.Steps.Analysis.Sandbox = info
	case "execution":
		current.Status.Steps.Execution.Sandbox = info
	case "verification":
		current.Status.Steps.Verification.Sandbox = info
	case "escalation":
		current.Status.Steps.Escalation.Sandbox = info
	}

	if err := s.K8sClient.Status().Patch(ctx, &current, client.MergeFrom(base)); err != nil {
		log.Error(err, "failed to patch sandbox info", LogKeyStep, step, LogKeyClaim, claimName)
	}
}

func collectFailedResults(results []agenticv1alpha1.StepResultRef, stepName string) []agentPreviousAttempt {
	var attempts []agentPreviousAttempt
	for i, ref := range results {
		if ref.Outcome != agenticv1alpha1.ActionOutcomeSucceeded {
			attempts = append(attempts, agentPreviousAttempt{
				Attempt:       int32(i + 1),
				FailureReason: fmt.Sprintf("%s attempt %d failed", stepName, i+1),
			})
		}
	}
	return attempts
}

func buildAgentContext(proposal *agenticv1alpha1.Proposal) *agentContext {
	ctx := &agentContext{
		TargetNamespaces: proposal.Spec.TargetNamespaces,
	}

	ctx.PreviousAttempts = append(ctx.PreviousAttempts, collectFailedResults(proposal.Status.Steps.Analysis.Results, "analysis")...)
	ctx.PreviousAttempts = append(ctx.PreviousAttempts, collectFailedResults(proposal.Status.Steps.Execution.Results, "execution")...)
	ctx.PreviousAttempts = append(ctx.PreviousAttempts, collectFailedResults(proposal.Status.Steps.Verification.Results, "verification")...)

	return ctx
}
