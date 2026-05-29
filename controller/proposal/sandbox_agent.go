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
	defaultSandboxTimeout  = 5 * time.Minute
	defaultBaseTemplateName = "lightspeed-agent"
)

type analysisResponse struct {
	Success bool                                `json:"success"`
	Options []agenticv1alpha1.RemediationOption `json:"options"`
}

type executionResponse struct {
	Success      bool                                  `json:"success"`
	ActionsTaken []agenticv1alpha1.ExecutionAction      `json:"actionsTaken"`
	Verification *agenticv1alpha1.ExecutionVerification `json:"verification,omitempty"`
}

type verificationResponse struct {
	Success bool                         `json:"success"`
	Checks  []agenticv1alpha1.VerifyCheck `json:"checks"`
	Summary string                        `json:"summary"`
}

// SandboxAgentCaller implements AgentCaller by claiming a sandbox pod,
// calling the agent HTTP service, and releasing the sandbox on completion.
type SandboxAgentCaller struct {
	Sandbox       SandboxProvider
	K8sClient     client.Client
	ClientFactory func(endpoint string, timeout time.Duration) AgentHTTPClientInterface
	Namespace     string
	Timeout       time.Duration
}

func NewSandboxAgentCaller(
	sandbox SandboxProvider,
	k8sClient client.Client,
	clientFactory func(endpoint string, timeout time.Duration) AgentHTTPClientInterface,
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

// proposalTimeout returns the effective timeout for sandbox operations.
// Reads spec.timeoutMinutes when set; falls back to defaultSandboxTimeout.
// This is the single place where timeout policy is decided.
func proposalTimeout(proposal *agenticv1alpha1.Proposal) time.Duration {
	if proposal.Spec.TimeoutMinutes != nil && *proposal.Spec.TimeoutMinutes > 0 {
		return time.Duration(*proposal.Spec.TimeoutMinutes) * time.Minute
	}
	return defaultSandboxTimeout
}

func stepString(step agenticv1alpha1.SandboxStep) string {
	return strings.ToLower(string(step))
}

func (s *SandboxAgentCaller) Analyze(ctx context.Context, proposal *agenticv1alpha1.Proposal, step resolvedStep, requestText string, timeout time.Duration) (*AnalysisOutput, error) {
	query := buildAnalysisQuery(requestText, proposal)
	raw, err := s.callWithSandbox(ctx, proposal, stepString(agenticv1alpha1.SandboxStepAnalysis), step, query, buildAgentContext(proposal), timeout)
	if err != nil {
		return nil, fmt.Errorf("analysis agent call: %w", err)
	}

	var resp analysisResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("parse analysis response: %w", err)
	}

	return &AnalysisOutput{
		Success: resp.Success,
		Options: resp.Options,
	}, nil
}

func (s *SandboxAgentCaller) Execute(ctx context.Context, proposal *agenticv1alpha1.Proposal, step resolvedStep, option *agenticv1alpha1.RemediationOption, timeout time.Duration) (*ExecutionOutput, error) {
	agentCtx := buildAgentContext(proposal)
	if option != nil {
		agentCtx.ApprovedOption = option
	}

	query := buildExecutionQuery(option)
	raw, err := s.callWithSandbox(ctx, proposal, stepString(agenticv1alpha1.SandboxStepExecution), step, query, agentCtx, timeout)
	if err != nil {
		return nil, fmt.Errorf("execution agent call: %w", err)
	}

	var resp executionResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("parse execution response: %w", err)
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

func (s *SandboxAgentCaller) Verify(ctx context.Context, proposal *agenticv1alpha1.Proposal, step resolvedStep, option *agenticv1alpha1.RemediationOption, exec *ExecutionOutput, timeout time.Duration) (*VerificationOutput, error) {
	agentCtx := buildAgentContext(proposal)
	if option != nil {
		agentCtx.ApprovedOption = option
	}
	agentCtx.ExecutionResult = executionOutputToAgentResult(exec)

	query := buildVerificationQuery(option, exec)
	raw, err := s.callWithSandbox(ctx, proposal, stepString(agenticv1alpha1.SandboxStepVerification), step, query, agentCtx, timeout)
	if err != nil {
		return nil, fmt.Errorf("verification agent call: %w", err)
	}

	var resp verificationResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("parse verification response: %w", err)
	}

	return &VerificationOutput{
		Success: resp.Success,
		Checks:  resp.Checks,
		Summary: resp.Summary,
	}, nil
}

func (s *SandboxAgentCaller) Escalate(ctx context.Context, proposal *agenticv1alpha1.Proposal, step resolvedStep, requestText string, timeout time.Duration) (*EscalationOutput, error) {
	agentCtx := buildAgentContext(proposal)
	raw, err := s.callWithSandbox(ctx, proposal, stepString(agenticv1alpha1.SandboxStepEscalation), step, requestText, agentCtx, timeout)
	if err != nil {
		return nil, fmt.Errorf("escalation agent call: %w", err)
	}

	var resp struct {
		Success bool   `json:"success"`
		Summary string `json:"summary"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("parse escalation response: %w", err)
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
	timeout time.Duration,
) (json.RawMessage, error) {
	if timeout <= 0 {
		timeout = defaultSandboxTimeout
	}

	templateName, err := EnsureAgentTemplate(ctx, s.K8sClient, defaultBaseTemplateName, s.Namespace, stepName, step.Agent, step.LLM, step.Tools, proposal.Spec.DataSource)
	if err != nil {
		return nil, fmt.Errorf("ensure agent template: %w", err)
	}

	claimName, err := s.Sandbox.Claim(ctx, proposal.Name, stepName, templateName)
	if err != nil {
		return nil, fmt.Errorf("claim sandbox: %w", err)
	}

	// Write sandbox info immediately so the console can stream logs
	// while the sandbox is still starting up
	s.patchSandboxInfo(ctx, proposal, stepName, claimName)

	endpoint, err := s.Sandbox.WaitReady(ctx, claimName, timeout)
	if err != nil {
		return nil, fmt.Errorf("wait for sandbox: %w", err)
	}

	agentURL := endpoint
	if !strings.HasPrefix(endpoint, "http") {
		agentURL = fmt.Sprintf("http://%s:8080", endpoint)
	}

	schema := outputSchemaForStep(stepName, proposal)

	client := s.ClientFactory(agentURL, timeout)
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
			log.Error(err, "failed to release sandbox", "claimName", info.ClaimName)
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
		log.Error(err, "failed to patch sandbox info", "step", step, "claimName", claimName)
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
