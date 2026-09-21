package agenticrun

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
)

const (
	ErrGetAgent                = "get Agent"
	ErrGetLLMProvider          = "get LLMProvider"
	ErrValidateCredentials     = "validate credentials"
	ErrResolveAnalysisStep     = "resolve analysis step"
	ErrResolveExecutionStep    = "resolve execution step"
	ErrResolveVerificationStep = "resolve verification step"
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

// validateCredentials checks that the LLM provider's credentials secret
// contains a valid key set. Currently validates Azure OpenAI only (rule 21a).
func validateCredentials(ctx context.Context, c client.Client, llm *agenticv1alpha1.LLMProvider, namespace string) error {
	if llm.Spec.Type != agenticv1alpha1.LLMProviderAzureOpenAI {
		return nil // only Azure validated today
	}

	secretName := credentialsSecretName(llm)
	secret := &corev1.Secret{}
	if err := c.Get(ctx, types.NamespacedName{Name: secretName, Namespace: namespace}, secret); err != nil {
		return fmt.Errorf("%s: get secret %q: %w", ErrValidateCredentials, secretName, err)
	}

	// API key mode: apitoken key present → valid
	if _, ok := secret.Data["apitoken"]; ok {
		return nil
	}

	// Entra ID mode: all three SP keys required
	for _, key := range []string{"client_id", "tenant_id", "client_secret"} {
		if _, ok := secret.Data[key]; !ok {
			return fmt.Errorf(
				"%s: azureOpenAI secret %q has incomplete service-principal set (missing %q); "+
					"provide either 'apitoken' or all of client_id/tenant_id/client_secret",
				ErrValidateCredentials, secretName, key,
			)
		}
	}
	return nil
}

func resolveAgenticRun(ctx context.Context, c client.Client, run *agenticv1alpha1.AgenticRun, approval *agenticv1alpha1.AgenticRunApproval, operatorNamespace ...string) (*resolvedWorkflow, error) {
	agentCache := map[string]*agenticv1alpha1.Agent{}
	llmCache := map[string]*agenticv1alpha1.LLMProvider{}

	// Operator namespace for secret lookups; optional to preserve existing callers.
	ns := ""
	if len(operatorNamespace) > 0 {
		ns = operatorNamespace[0]
	}

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
			if ns != "" {
				if err := validateCredentials(ctx, c, llm, ns); err != nil {
					return nil, nil, fmt.Errorf("Agent %q, LLMProvider %q: %w", agentName, llmName, err)
				}
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
		return effectiveStepAgentName(approval, stage, step)
	}

	resolved := &resolvedWorkflow{}

	agent, llm, err := resolveAgent(effectiveAgent(agenticv1alpha1.SandboxStepAnalysis, run.Spec.Analysis))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", ErrResolveAnalysisStep, err)
	}
	resolved.Analysis = resolvedStep{Agent: agent, LLM: llm, Tools: toolsForStep(run.Spec.Analysis)}

	if !run.Spec.Execution.IsZero() {
		agent, llm, err := resolveAgent(effectiveAgent(agenticv1alpha1.SandboxStepExecution, run.Spec.Execution))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", ErrResolveExecutionStep, err)
		}
		resolved.Execution = &resolvedStep{Agent: agent, LLM: llm, Tools: toolsForStep(run.Spec.Execution)}
	}

	if !run.Spec.Verification.IsZero() {
		agent, llm, err := resolveAgent(effectiveAgent(agenticv1alpha1.SandboxStepVerification, run.Spec.Verification))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", ErrResolveVerificationStep, err)
		}
		resolved.Verification = &resolvedStep{Agent: agent, LLM: llm, Tools: toolsForStep(run.Spec.Verification)}
	}

	return resolved, nil
}

func stepAgentName(step agenticv1alpha1.AgenticRunStep) string {
	if step.Agent != "" {
		return step.Agent
	}
	return "default"
}

// effectiveStepAgentName is the single source of truth for selecting a stage's
// Agent. Timeout enforcement and sandbox launch must use the same override
// resolution.
func effectiveStepAgentName(approval *agenticv1alpha1.AgenticRunApproval, stage agenticv1alpha1.SandboxStep, step agenticv1alpha1.AgenticRunStep) string {
	if override := getStageOverrideAgent(approval, stage); override != "" {
		return override
	}
	return stepAgentName(step)
}
