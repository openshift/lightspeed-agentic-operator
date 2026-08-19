package agenticrun

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
)

const (
	ErrGetAgent                = "get Agent"
	ErrGetLLMProvider          = "get LLMProvider"
	ErrResolveAnalysisStep     = "resolve analysis step"
	ErrResolveExecutionStep    = "resolve execution step"
	ErrResolveVerificationStep = "resolve verification step"
)

type resolvedStep struct {
	Agent          *agenticv1alpha1.Agent
	LLM            *agenticv1alpha1.LLMProvider
	Tools          *agenticv1alpha1.ToolsSpec
	TimeoutMinutes int32
}

type resolvedWorkflow struct {
	Analysis     resolvedStep
	Execution    *resolvedStep // nil = skip execution
	Verification *resolvedStep // nil = skip verification
}

func resolveAgenticRun(ctx context.Context, c client.Client, run *agenticv1alpha1.AgenticRun, approval *agenticv1alpha1.AgenticRunApproval) (*resolvedWorkflow, error) {
	agentCache := map[string]*agenticv1alpha1.Agent{}
	llmCache := map[string]*agenticv1alpha1.LLMProvider{}

	resolveAgent := func(agentName string) (*agenticv1alpha1.Agent, *agenticv1alpha1.LLMProvider, error) {
		if agentName == "" {
			agentName = "default"
		}
		agent, ok := agentCache[agentName]
		if !ok {
			agent = &agenticv1alpha1.Agent{}
			if err := c.Get(ctx, types.NamespacedName{Name: agentName}, agent); err != nil {
				return nil, nil, fmt.Errorf("%s %q: %w", ErrGetAgent, agentName, err)
			}
			agentCache[agentName] = agent
		}

		llmName := agent.Spec.LLMProvider.Name
		llm, ok := llmCache[llmName]
		if !ok {
			llm = &agenticv1alpha1.LLMProvider{}
			if err := c.Get(ctx, types.NamespacedName{Name: llmName}, llm); err != nil {
				return nil, nil, fmt.Errorf("%s %q (referenced by Agent %q): %w", ErrGetLLMProvider, llmName, agentName, err)
			}
			llmCache[llmName] = llm
		}

		return agent, llm, nil
	}

	toolsForStep := func(step agenticv1alpha1.AgenticRunStep) *agenticv1alpha1.ToolsSpec {
		if !step.Tools.IsZero() {
			return &step.Tools
		}
		return &run.Spec.Tools
	}

	effectiveAgent := func(stage agenticv1alpha1.SandboxStep, step agenticv1alpha1.AgenticRunStep) string {
		if override := getStageOverrideAgent(approval, stage); override != "" {
			return override
		}
		return stepAgentName(step)
	}

	resolved := &resolvedWorkflow{}

	agent, llm, err := resolveAgent(effectiveAgent(agenticv1alpha1.SandboxStepAnalysis, run.Spec.Analysis))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", ErrResolveAnalysisStep, err)
	}
	resolved.Analysis = resolvedStep{Agent: agent, LLM: llm, Tools: toolsForStep(run.Spec.Analysis), TimeoutMinutes: run.Spec.Analysis.TimeoutMinutes}

	if !run.Spec.Execution.IsZero() {
		agent, llm, err := resolveAgent(effectiveAgent(agenticv1alpha1.SandboxStepExecution, run.Spec.Execution))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", ErrResolveExecutionStep, err)
		}
		resolved.Execution = &resolvedStep{Agent: agent, LLM: llm, Tools: toolsForStep(run.Spec.Execution), TimeoutMinutes: run.Spec.Execution.TimeoutMinutes}
	}

	if !run.Spec.Verification.IsZero() {
		agent, llm, err := resolveAgent(effectiveAgent(agenticv1alpha1.SandboxStepVerification, run.Spec.Verification))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", ErrResolveVerificationStep, err)
		}
		resolved.Verification = &resolvedStep{Agent: agent, LLM: llm, Tools: toolsForStep(run.Spec.Verification), TimeoutMinutes: run.Spec.Verification.TimeoutMinutes}
	}

	return resolved, nil
}

func stepAgentName(step agenticv1alpha1.AgenticRunStep) string {
	if step.Agent != "" {
		return step.Agent
	}
	return "default"
}
