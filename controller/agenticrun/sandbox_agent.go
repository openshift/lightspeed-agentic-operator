package agenticrun

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-logr/logr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
)

const (
	defaultSandboxTimeout = 5 * time.Minute

	analysisStepTimeout     = 10 * time.Minute
	executionStepTimeout    = 10 * time.Minute
	verificationStepTimeout = 30 * time.Minute

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

	// CRD maxLength limits for analysis option fields, used by
	// sanitizeAnalysisOptions as a safety net for LLM output that
	// exceeds CRD validation constraints.
	maxLenOptionTitle              = 256
	maxLenOptionSummary            = 1024
	maxLenDiagnosisSummary         = 8192
	maxLenDiagnosisRootCause       = 1024
	maxLenPlanDescription          = 8192
	maxLenActionCommand            = 4096
	maxLenActionType               = 256
	maxLenActionDescription        = 4096
	maxLenRollbackDescription      = 4096
	maxLenRollbackCommand          = 4096
	maxLenVerificationDescription  = 4096
	maxLenVerificationStepName     = 253
	maxLenVerificationStepCommand  = 4096
	maxLenVerificationStepExpected = 1024
	maxLenVerificationStepType     = 256
	maxLenRBACJustification        = 1024
)

type analysisResponse struct {
	Success        bool                                `json:"success"`
	Summary        string                              `json:"summary,omitempty"`
	ActionRequired *bool                               `json:"actionRequired,omitempty"`
	Diagnosis      *agenticv1alpha1.DiagnosisResult    `json:"diagnosis,omitempty"`
	Options        []agenticv1alpha1.RemediationOption `json:"options"`
}

type executionResponse struct {
	Success      bool                              `json:"success"`
	Summary      string                            `json:"summary,omitempty"`
	ActionsTaken []agenticv1alpha1.ExecutionAction `json:"actionsTaken"`
}

type verificationResponse struct {
	Success bool                          `json:"success"`
	Checks  []agenticv1alpha1.VerifyCheck `json:"checks"`
	Summary string                        `json:"summary"`
}

// SandboxLifecycle is the interface for sandbox create/wait/release.
type SandboxLifecycle interface {
	Create(ctx context.Context, run *agenticv1alpha1.AgenticRun, step string, agent *agenticv1alpha1.Agent, llm *agenticv1alpha1.LLMProvider, tools *agenticv1alpha1.ToolsSpec, serviceAccount string, deadline time.Duration) (string, error)
	WaitReady(ctx context.Context, name string, timeout time.Duration) (endpoint string, err error)
	Release(ctx context.Context, name string) error
}

// SandboxAgentCaller implements AgentCaller by creating a sandbox (bare pod
// or sandbox claim), calling the agent HTTP service, and releasing it.
type SandboxAgentCaller struct {
	Sandbox       SandboxLifecycle
	K8sClient     client.Client
	ClientFactory func(endpoint string, timeout time.Duration) AgentHTTPClientInterface
	Namespace     string
	Audit         AuditLogger
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

	sanitizeAnalysisOptions(log, run.Name, resp.Options)

	if resp.Diagnosis != nil && (resp.Diagnosis.Summary == "" || resp.Diagnosis.RootCause == "") {
		log.Info("ignoring empty top-level diagnosis (per-option diagnoses used instead)", "run", run.Name)
		resp.Diagnosis = nil
	}

	actionRequired := resp.ActionRequired == nil || *resp.ActionRequired
	if resp.Diagnosis == nil && !actionRequired {
		log.Error(nil, "analysis response missing diagnosis for no-action-required result", "run", run.Name)
	}

	return &AnalysisOutput{
		Success:        resp.Success,
		Summary:        resp.Summary,
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

	return &ExecutionOutput{
		Success:      resp.Success,
		Summary:      resp.Summary,
		ActionsTaken: resp.ActionsTaken,
	}, nil
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

func stepTimeout(step string) time.Duration {
	switch step {
	case "analysis", "escalation":
		return analysisStepTimeout
	case "execution":
		return executionStepTimeout
	case "verification":
		return verificationStepTimeout
	default:
		return analysisStepTimeout
	}
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
	agentTimeout := stepTimeout(stepName)
	podDeadline := agentTimeout + defaultSandboxTimeout

	name, err := s.Sandbox.Create(ctx, run, stepName, step.Agent, step.LLM, step.Tools, serviceAccount, podDeadline)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", ErrClaimSandbox, err)
	}

	s.patchSandboxInfo(ctx, run, stepName, name)

	endpoint, err := s.Sandbox.WaitReady(ctx, name, defaultSandboxTimeout)
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

	client := s.ClientFactory(agentURL, podDeadline)
	resp, err := client.Run(ctx, "", query, schema, agentCtx, headers, agentTimeout)
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

// truncate returns s unchanged if its byte length is within max,
// otherwise it returns the first max bytes.
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max]
}

// sanitizeAnalysisOptions cleans up LLM-produced options before they are
// written to an AnalysisResult CR. It addresses two failure modes:
//   - per-option Diagnosis with empty required fields (Summary or RootCause)
//     is zeroed so omitzero omits the struct entirely, preventing CRD
//     MinLength validation failures.
//   - string fields that exceed CRD MaxLength limits are truncated to the
//     constant-defined cap so the status patch is not rejected.
func sanitizeAnalysisOptions(log logr.Logger, runName string, options []agenticv1alpha1.RemediationOption) {
	for i := range options {
		opt := &options[i]

		if opt.Diagnosis.Summary == "" || opt.Diagnosis.RootCause == "" {
			log.Info("zeroing per-option diagnosis with empty required fields", "run", runName, "option", i)
			opt.Diagnosis = agenticv1alpha1.DiagnosisResult{}
		}

		if n := len(opt.Title); n > maxLenOptionTitle {
			log.Info("truncating option title", "run", runName, "option", i, "from", n, "to", maxLenOptionTitle)
			opt.Title = truncate(opt.Title, maxLenOptionTitle)
		}
		if n := len(opt.Summary); n > maxLenOptionSummary {
			log.Info("truncating option summary", "run", runName, "option", i, "from", n, "to", maxLenOptionSummary)
			opt.Summary = truncate(opt.Summary, maxLenOptionSummary)
		}
		if n := len(opt.Diagnosis.Summary); n > maxLenDiagnosisSummary {
			log.Info("truncating option diagnosis summary", "run", runName, "option", i, "from", n, "to", maxLenDiagnosisSummary)
			opt.Diagnosis.Summary = truncate(opt.Diagnosis.Summary, maxLenDiagnosisSummary)
		}
		if n := len(opt.Diagnosis.RootCause); n > maxLenDiagnosisRootCause {
			log.Info("truncating option diagnosis rootCause", "run", runName, "option", i, "from", n, "to", maxLenDiagnosisRootCause)
			opt.Diagnosis.RootCause = truncate(opt.Diagnosis.RootCause, maxLenDiagnosisRootCause)
		}

		if n := len(opt.RemediationPlan.Description); n > maxLenPlanDescription {
			log.Info("truncating remediationPlan description", "run", runName, "option", i, "from", n, "to", maxLenPlanDescription)
			opt.RemediationPlan.Description = truncate(opt.RemediationPlan.Description, maxLenPlanDescription)
		}
		for j := range opt.RemediationPlan.Actions {
			a := &opt.RemediationPlan.Actions[j]
			if n := len(a.Command); n > maxLenActionCommand {
				log.Info("truncating action command", "run", runName, "option", i, "action", j, "from", n, "to", maxLenActionCommand)
				a.Command = truncate(a.Command, maxLenActionCommand)
			}
			if n := len(a.Type); n > maxLenActionType {
				a.Type = truncate(a.Type, maxLenActionType)
			}
			if n := len(a.Description); n > maxLenActionDescription {
				a.Description = truncate(a.Description, maxLenActionDescription)
			}
		}

		if n := len(opt.RemediationPlan.RollbackPlan.Description); n > maxLenRollbackDescription {
			opt.RemediationPlan.RollbackPlan.Description = truncate(opt.RemediationPlan.RollbackPlan.Description, maxLenRollbackDescription)
		}
		if n := len(opt.RemediationPlan.RollbackPlan.Command); n > maxLenRollbackCommand {
			opt.RemediationPlan.RollbackPlan.Command = truncate(opt.RemediationPlan.RollbackPlan.Command, maxLenRollbackCommand)
		}

		if n := len(opt.Verification.Description); n > maxLenVerificationDescription {
			opt.Verification.Description = truncate(opt.Verification.Description, maxLenVerificationDescription)
		}
		for j := range opt.Verification.Steps {
			s := &opt.Verification.Steps[j]
			if n := len(s.Name); n > maxLenVerificationStepName {
				s.Name = truncate(s.Name, maxLenVerificationStepName)
			}
			if n := len(s.Command); n > maxLenVerificationStepCommand {
				s.Command = truncate(s.Command, maxLenVerificationStepCommand)
			}
			if n := len(s.Expected); n > maxLenVerificationStepExpected {
				s.Expected = truncate(s.Expected, maxLenVerificationStepExpected)
			}
			if n := len(s.Type); n > maxLenVerificationStepType {
				s.Type = truncate(s.Type, maxLenVerificationStepType)
			}
		}

		for j := range opt.RBAC.NamespaceScoped {
			r := &opt.RBAC.NamespaceScoped[j]
			if n := len(r.Justification); n > maxLenRBACJustification {
				r.Justification = truncate(r.Justification, maxLenRBACJustification)
			}
		}
		for j := range opt.RBAC.ClusterScoped {
			r := &opt.RBAC.ClusterScoped[j]
			if n := len(r.Justification); n > maxLenRBACJustification {
				r.Justification = truncate(r.Justification, maxLenRBACJustification)
			}
		}
	}
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
