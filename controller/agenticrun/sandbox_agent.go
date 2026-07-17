package agenticrun

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
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
	Success        bool                                `json:"success"`
	ActionRequired *bool                               `json:"actionRequired,omitempty"`
	Diagnosis      *agenticv1alpha1.DiagnosisResult    `json:"diagnosis,omitempty"`
	Options        []agenticv1alpha1.RemediationOption `json:"options"`
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
	Audit         AuditLogger
}

func NewSandboxAgentCaller(
	sandbox SandboxProvider,
	k8sClient client.Client,
	clientFactory func(endpoint string) AgentHTTPClientInterface,
	namespace string,
	audit AuditLogger,
) *SandboxAgentCaller {
	return &SandboxAgentCaller{
		Sandbox:       sandbox,
		K8sClient:     k8sClient,
		ClientFactory: clientFactory,
		Namespace:     namespace,
		Timeout:       defaultSandboxTimeout,
		Audit:         audit,
	}
}

func stepString(step agenticv1alpha1.SandboxStep) string {
	return strings.ToLower(string(step))
}

func (s *SandboxAgentCaller) Analyze(ctx context.Context, run *agenticv1alpha1.AgenticRun, step resolvedStep, requestText string, serviceAccount string) (*AnalysisOutput, error) {
	query := buildAnalysisQuery(requestText, run)
	raw, err := s.callWithSandbox(ctx, run, stepString(agenticv1alpha1.SandboxStepAnalysis), step, query, buildAgentContext(run), serviceAccount)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", ErrAnalysisAgentCall, err)
	}

	var resp analysisResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("%s: %w", ErrParseAnalysisResponse, err)
	}

	log := logf.FromContext(ctx)
	for i, opt := range resp.Options {
		for j, action := range opt.RemediationPlan.Actions {
			if action.Command == "" {
				log.Info("analysis action missing command field", "option", i, "action", j, "type", action.Type, "run", run.Name)
			}
		}
	}

	actionRequired := resp.ActionRequired == nil || *resp.ActionRequired
	if resp.Diagnosis == nil && !actionRequired {
		log.Error(nil, "analysis response missing diagnosis for no-action-required result", "run", run.Name)
	}

	return &AnalysisOutput{
		Success:        resp.Success,
		ActionRequired: &actionRequired,
		Options:        resp.Options,
		Diagnosis:      resp.Diagnosis,
	}, nil
}

func (s *SandboxAgentCaller) Execute(ctx context.Context, run *agenticv1alpha1.AgenticRun, step resolvedStep, option *agenticv1alpha1.RemediationOption, serviceAccount string) (*ExecutionOutput, error) {
	agentCtx := buildAgentContext(run)
	if option != nil {
		agentCtx.ApprovedOption = option
	}

	query := buildExecutionQuery(option)
	raw, err := s.callWithSandbox(ctx, run, stepString(agenticv1alpha1.SandboxStepExecution), step, query, agentCtx, serviceAccount)
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

func (s *SandboxAgentCaller) Verify(ctx context.Context, run *agenticv1alpha1.AgenticRun, step resolvedStep, option *agenticv1alpha1.RemediationOption, exec *ExecutionOutput, serviceAccount string) (*VerificationOutput, error) {
	agentCtx := buildAgentContext(run)
	if option != nil {
		agentCtx.ApprovedOption = option
	}
	agentCtx.ExecutionResult = executionOutputToAgentResult(exec)

	query := buildVerificationQuery(option, exec)
	raw, err := s.callWithSandbox(ctx, run, stepString(agenticv1alpha1.SandboxStepVerification), step, query, agentCtx, serviceAccount)
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

func (s *SandboxAgentCaller) Escalate(ctx context.Context, run *agenticv1alpha1.AgenticRun, step resolvedStep, requestText string, serviceAccount string) (*EscalationOutput, error) {
	agentCtx := buildAgentContext(run)
	raw, err := s.callWithSandbox(ctx, run, stepString(agenticv1alpha1.SandboxStepEscalation), step, requestText, agentCtx, serviceAccount)
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
	run *agenticv1alpha1.AgenticRun,
	stepName string,
	step resolvedStep,
	query string,
	agentCtx *agentContext,
	serviceAccount string,
) (json.RawMessage, error) {
	s.Sandbox.SetStep(step.Agent, step.LLM, step.Tools, serviceAccount)

	claimName, err := s.Sandbox.Claim(ctx, run, stepName, "")
	if err != nil {
		return nil, fmt.Errorf("%s: %w", ErrClaimSandbox, err)
	}

	// Write sandbox info immediately so the console can stream logs
	// while the sandbox is still starting up
	s.patchSandboxInfo(ctx, run, stepName, claimName)

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

	schema := outputSchemaForStep(stepName, run)

	// Inject W3C traceparent header for trace propagation
	headers := http.Header{}
	if s.Audit != nil {
		s.Audit.InjectTraceContext(ctx, run, headers)
	}

	client := s.ClientFactory(agentURL)
	resp, err := client.Run(ctx, "", query, schema, agentCtx, headers)
	if err != nil {
		return nil, err
	}

	return resp.Response, nil
}

func (s *SandboxAgentCaller) ReleaseSandboxes(ctx context.Context, run *agenticv1alpha1.AgenticRun) error {
	log := logf.FromContext(ctx)
	var firstErr error

	for _, info := range []agenticv1alpha1.SandboxInfo{
		run.Status.Steps.Analysis.Sandbox,
		run.Status.Steps.Execution.Sandbox,
		run.Status.Steps.Verification.Sandbox,
		run.Status.Steps.Escalation.Sandbox,
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

func (s *SandboxAgentCaller) patchSandboxInfo(ctx context.Context, run *agenticv1alpha1.AgenticRun, step, claimName string) {
	log := logf.FromContext(ctx)

	var current agenticv1alpha1.AgenticRun
	if err := s.K8sClient.Get(ctx, client.ObjectKeyFromObject(run), &current); err != nil {
		log.Error(err, "failed to get run for sandbox info patch")
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

func buildAgentContext(run *agenticv1alpha1.AgenticRun) *agentContext {
	ctx := &agentContext{
		TargetNamespaces: run.Spec.TargetNamespaces,
	}

	ctx.PreviousAttempts = append(ctx.PreviousAttempts, collectFailedResults(run.Status.Steps.Analysis.Results, "analysis")...)
	ctx.PreviousAttempts = append(ctx.PreviousAttempts, collectFailedResults(run.Status.Steps.Execution.Results, "execution")...)
	ctx.PreviousAttempts = append(ctx.PreviousAttempts, collectFailedResults(run.Status.Steps.Verification.Results, "verification")...)

	return ctx
}
