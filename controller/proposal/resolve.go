package proposal

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
)

type resolvedStep struct {
	Agent *agenticv1alpha1.Agent
	LLM   *agenticv1alpha1.LLMProvider
	Tools *agenticv1alpha1.ToolsSpec
}

type resolvedWorkflow struct {
	Analysis     resolvedStep
	Execution    *resolvedStep // nil = skip execution
	Verification *resolvedStep // nil = skip verification
}

func resolveProposal(ctx context.Context, c client.Client, proposal *agenticv1alpha1.Proposal, approval *agenticv1alpha1.ProposalApproval) (*resolvedWorkflow, error) {
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
				return nil, nil, fmt.Errorf("get Agent %q: %w", agentName, err)
			}
			agentCache[agentName] = agent
		}

		llmName := agent.Spec.LLMProvider.Name
		llm, ok := llmCache[llmName]
		if !ok {
			llm = &agenticv1alpha1.LLMProvider{}
			if err := c.Get(ctx, types.NamespacedName{Name: llmName}, llm); err != nil {
				return nil, nil, fmt.Errorf("get LLMProvider %q (referenced by Agent %q): %w", llmName, agentName, err)
			}
			llmCache[llmName] = llm
		}

		return agent, llm, nil
	}

	toolsForStep := func(step agenticv1alpha1.ProposalStep) *agenticv1alpha1.ToolsSpec {
		if !step.Tools.IsZero() {
			return &step.Tools
		}
		return &proposal.Spec.Tools
	}

	effectiveAgent := func(stage agenticv1alpha1.SandboxStep, step agenticv1alpha1.ProposalStep) string {
		if override := getStageOverrideAgent(approval, stage); override != "" {
			return override
		}
		return stepAgentName(step)
	}

	resolved := &resolvedWorkflow{}

	agent, llm, err := resolveAgent(effectiveAgent(agenticv1alpha1.SandboxStepAnalysis, proposal.Spec.Analysis))
	if err != nil {
		return nil, fmt.Errorf("resolve analysis step: %w", err)
	}
	resolved.Analysis = resolvedStep{Agent: agent, LLM: llm, Tools: toolsForStep(proposal.Spec.Analysis)}

	if !proposal.Spec.Execution.IsZero() {
		agent, llm, err := resolveAgent(effectiveAgent(agenticv1alpha1.SandboxStepExecution, proposal.Spec.Execution))
		if err != nil {
			return nil, fmt.Errorf("resolve execution step: %w", err)
		}
		resolved.Execution = &resolvedStep{Agent: agent, LLM: llm, Tools: toolsForStep(proposal.Spec.Execution)}
	}

	if !proposal.Spec.Verification.IsZero() {
		agent, llm, err := resolveAgent(effectiveAgent(agenticv1alpha1.SandboxStepVerification, proposal.Spec.Verification))
		if err != nil {
			return nil, fmt.Errorf("resolve verification step: %w", err)
		}
		resolved.Verification = &resolvedStep{Agent: agent, LLM: llm, Tools: toolsForStep(proposal.Spec.Verification)}
	}

	return resolved, nil
}

func stepAgentName(step agenticv1alpha1.ProposalStep) string {
	if step.Agent != "" {
		return step.Agent
	}
	return "default"
}
