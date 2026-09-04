package agenticrun

import (
	"context"
	"fmt"
	"strings"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
)

const (
	defaultSandboxTimeout = 5 * time.Minute

	analysisStepTimeout     = 10 * time.Minute
	executionStepTimeout    = 10 * time.Minute
	verificationStepTimeout = 30 * time.Minute

	ErrClaimSandbox = "claim sandbox"

	// Batch sandbox execution (OLS-3066 / OLS-3794).
	// Spec: .ai/spec/what/sandbox-execution.md, .ai/spec/what/run-lifecycle.md rule 14a.

	// Step condition reasons (run-lifecycle.md rule 14a).
	ReasonWaitingForSandbox = "WaitingForSandbox"
	ReasonRunning           = "Running"
	ReasonSucceeded         = "Succeeded"
	ReasonSandboxTimeout    = "SandboxTimeout"
	ReasonSandboxFailed     = "SandboxFailed"

	// Pod start timeout — covers image pull, scheduling, resource limits, etc.
	podStartTimeout = 5 * time.Minute

	// Input ConfigMap (sandbox-execution.md rule 7).
	inputConfigMapMountPath       = "/input"
	inputConfigMapKeyQuery        = "query"
	inputConfigMapKeySystemPrompt = "system-prompt"
	inputConfigMapKeySchema       = "output-schema"
	inputConfigMapKeyCtx          = "context"
	inputConfigMapKeyTmpl         = "result-template"

	// CRD maxLength limits for analysis option fields, injected into
	// the LLM output schema so the model respects CRD constraints.
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

// stepTimeout returns the hard deadline for a sandbox step (OLS-3781 / PR 423).
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

// Agent context types — shared by input ConfigMap builder and helpers.

type agentContext struct {
	TargetNamespaces []string                           `json:"targetNamespaces,omitempty"`
	PreviousAttempts []agentPreviousAttempt             `json:"previousAttempts,omitempty"`
	ApprovedOption   *agenticv1alpha1.RemediationOption `json:"approvedOption,omitempty"`
	ExecutionResult  *agentExecutionResult              `json:"executionResult,omitempty"`
}

type agentExecutionResult struct {
	Success      bool                              `json:"success"`
	ActionsTaken []agenticv1alpha1.ExecutionAction `json:"actionsTaken"`
}

func executionOutputToAgentResult(exec *ExecutionOutput) *agentExecutionResult {
	if exec == nil {
		return nil
	}
	return &agentExecutionResult{
		Success:      exec.Success,
		ActionsTaken: exec.ActionsTaken,
	}
}

type agentPreviousAttempt struct {
	Attempt       int32  `json:"attempt"`
	FailureReason string `json:"failureReason,omitempty"`
}

// SandboxLifecycle is the interface for sandbox create/release.
// Create handles all sandbox setup: SA, RBAC, ConfigMap, pod, owner refs.
// Release handles all cleanup: pod deletion (GC handles children) plus
// explicit cross-namespace/cluster-scoped RBAC teardown.
type SandboxLifecycle interface {
	Create(ctx context.Context, run *agenticv1alpha1.AgenticRun, step string, agent *agenticv1alpha1.Agent, llm *agenticv1alpha1.LLMProvider, tools *agenticv1alpha1.ToolsSpec, deadline time.Duration, agentCtx *agentContext) (string, error)
	Release(ctx context.Context, run *agenticv1alpha1.AgenticRun, step string) error
}

// SandboxAgentCaller implements AgentCaller by creating a sandbox pod
// with an input ConfigMap. The sandbox runs the agent autonomously and
// creates the Result CR before exiting.
type SandboxAgentCaller struct {
	Sandbox   SandboxLifecycle
	K8sClient client.Client
	Namespace string
	Audit     AuditLogger
}

func stepString(step agenticv1alpha1.SandboxStep) string {
	return strings.ToLower(string(step))
}

func (s *SandboxAgentCaller) Analyze(ctx context.Context, run *agenticv1alpha1.AgenticRun, step resolvedStep) error {
	return s.launchSandbox(ctx, run, stepString(agenticv1alpha1.SandboxStepAnalysis), step, buildAgentContext(run))
}

func (s *SandboxAgentCaller) Execute(ctx context.Context, run *agenticv1alpha1.AgenticRun, step resolvedStep, option *agenticv1alpha1.RemediationOption) error {
	agentCtx := buildAgentContext(run)
	if option != nil {
		agentCtx.ApprovedOption = option
	}
	return s.launchSandbox(ctx, run, stepString(agenticv1alpha1.SandboxStepExecution), step, agentCtx)
}

func (s *SandboxAgentCaller) Verify(ctx context.Context, run *agenticv1alpha1.AgenticRun, step resolvedStep, option *agenticv1alpha1.RemediationOption, exec *ExecutionOutput) error {
	agentCtx := buildAgentContext(run)
	if option != nil {
		agentCtx.ApprovedOption = option
	}
	agentCtx.ExecutionResult = executionOutputToAgentResult(exec)
	return s.launchSandbox(ctx, run, stepString(agenticv1alpha1.SandboxStepVerification), step, agentCtx)
}

func (s *SandboxAgentCaller) Escalate(ctx context.Context, run *agenticv1alpha1.AgenticRun, step resolvedStep) error {
	return s.launchSandbox(ctx, run, stepString(agenticv1alpha1.SandboxStepEscalation), step, buildAgentContext(run))
}

// launchSandbox delegates to SandboxLifecycle.Create which handles all setup
// (ConfigMap, SA, RBAC, pod) and patches the sandbox info on the status.
func (s *SandboxAgentCaller) launchSandbox(
	ctx context.Context,
	run *agenticv1alpha1.AgenticRun,
	stepName string,
	step resolvedStep,
	agentCtx *agentContext,
) error {
	podDeadline := stepTimeout(stepName) + defaultSandboxTimeout
	var name string
	if err := retryOnTransient(ctx, func() error {
		var createErr error
		name, createErr = s.Sandbox.Create(ctx, run, stepName, step.Agent, step.LLM, step.Tools, podDeadline, agentCtx)
		return createErr
	}); err != nil {
		return fmt.Errorf("%s: %w", ErrClaimSandbox, err)
	}

	if err := s.patchSandboxInfo(ctx, run, stepName, name); err != nil {
		return fmt.Errorf("patch sandbox info for step %s: %w", stepName, err)
	}
	return nil
}

func (s *SandboxAgentCaller) ReleaseSandbox(ctx context.Context, run *agenticv1alpha1.AgenticRun, step string) error {
	return s.Sandbox.Release(ctx, run, step)
}

func (s *SandboxAgentCaller) ReleaseSandboxes(ctx context.Context, run *agenticv1alpha1.AgenticRun) error {
	log := logf.FromContext(ctx)
	var firstErr error

	executionReleased := false
	for _, step := range []string{"analysis", "execution", "verification", "escalation"} {
		claimName := sandboxClaimName(run, step)
		if claimName == "" {
			continue
		}
		if step == "execution" {
			executionReleased = true
		}
		if err := s.Sandbox.Release(ctx, run, step); err != nil {
			log.Error(err, "failed to release sandbox", LogKeyClaim, claimName, LogKeyStep, step)
			if firstErr == nil {
				firstErr = err
			}
		}
	}

	// If execution RBAC was created (annotation present) but patchSandboxInfo
	// failed (no claim name), Release("execution") was skipped above. Clean up
	// the RBAC unconditionally to prevent leaks.
	if !executionReleased && len(annotatedRBACNamespaces(run)) > 0 {
		if err := cleanupExecutionRBAC(ctx, s.K8sClient, run); err != nil {
			log.Error(err, "failed to clean up orphaned execution RBAC")
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

func (s *SandboxAgentCaller) patchSandboxInfo(ctx context.Context, run *agenticv1alpha1.AgenticRun, step, claimName string) error {
	var current agenticv1alpha1.AgenticRun
	if err := s.K8sClient.Get(ctx, client.ObjectKeyFromObject(run), &current); err != nil {
		return fmt.Errorf("get run for sandbox info patch: %w", err)
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
		return fmt.Errorf("patch sandbox info for step %s: %w", step, err)
	}
	return nil
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
