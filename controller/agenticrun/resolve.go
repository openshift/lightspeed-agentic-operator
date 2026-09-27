package agenticrun

import (
	"context"
	stderrors "errors"
	"fmt"
	"slices"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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

// validateAzureCredentials checks for a usable API key or complete Entra ID service principal.
func validateAzureCredentials(secret *corev1.Secret) error {
	if len(secret.Data["apitoken"]) > 0 {
		return nil
	}
	for _, key := range []string{"client_id", "tenant_id", "client_secret"} {
		if len(secret.Data[key]) == 0 {
			return fmt.Errorf("%s: azureOpenAI secret %q has missing or empty %q; provide either 'apitoken' or all of client_id/tenant_id/client_secret", ErrValidateCredentials, secret.Name, key)
		}
	}
	return nil
}

func validateAgentDependencies(ctx context.Context, c client.Client, namespace, agentName string) (*agenticv1alpha1.Agent, *agenticv1alpha1.LLMProvider, []error) {
	if agentName == "" {
		agentName = "default"
	}
	var issues []error
	agent := &agenticv1alpha1.Agent{}
	var llm *agenticv1alpha1.LLMProvider
	if err := c.Get(ctx, types.NamespacedName{Name: agentName}, agent); err != nil {
		if apierrors.IsNotFound(err) {
			issues = append(issues, fmt.Errorf("Agent %q not found", agentName))
		} else {
			issues = append(issues, fmt.Errorf("get Agent %q: %w", agentName, err))
		}
	} else {
		llmName := agent.Spec.LLMProvider.Name
		llm = &agenticv1alpha1.LLMProvider{}
		if err := c.Get(ctx, types.NamespacedName{Name: llmName}, llm); err != nil {
			if apierrors.IsNotFound(err) {
				issues = append(issues, fmt.Errorf("Agent %q references missing LLMProvider %q", agentName, llmName))
			} else {
				issues = append(issues, fmt.Errorf("get LLMProvider %q referenced by Agent %q: %w", llmName, agentName, err))
			}
		} else {
			secretName := credentialsSecretName(llm)
			if secretName == "" {
				issues = append(issues, fmt.Errorf("LLMProvider %q has no credentials Secret configured", llmName))
			} else {
				secret := &corev1.Secret{}
				if err := c.Get(ctx, types.NamespacedName{Name: secretName, Namespace: namespace}, secret); err != nil {
					if apierrors.IsNotFound(err) {
						issues = append(issues, fmt.Errorf("LLMProvider %q references missing Secret %q in namespace %q", llmName, secretName, namespace))
					} else {
						issues = append(issues, fmt.Errorf("get Secret %q for LLMProvider %q: %w", secretName, llmName, err))
					}
				} else if llm.Spec.Type == agenticv1alpha1.LLMProviderAzureOpenAI {
					if err := validateAzureCredentials(secret); err != nil {
						issues = append(issues, fmt.Errorf("Agent %q, LLMProvider %q: %w", agentName, llmName, err))
					}
				}
			}
		}
	}
	if len(issues) > 0 {
		return nil, nil, issues
	}
	return agent, llm, nil
}

func validateAgenticRun(ctx context.Context, c client.Client, run *agenticv1alpha1.AgenticRun, approval *agenticv1alpha1.AgenticRunApproval, operatorNamespace string) ([]error, map[string]*agenticv1alpha1.Agent, map[string]*agenticv1alpha1.LLMProvider) {
	errors := []error{}
	validatedAgents := map[string]*agenticv1alpha1.Agent{}
	validatedLLMProviders := map[string]*agenticv1alpha1.LLMProvider{}
	agents := []string{}

	steps := []struct {
		stage      agenticv1alpha1.SandboxStep
		step       agenticv1alpha1.AgenticRunStep
		configured bool
	}{
		{agenticv1alpha1.SandboxStepAnalysis, run.Spec.Analysis, true},
		{agenticv1alpha1.SandboxStepExecution, run.Spec.Execution, !run.Spec.Execution.IsZero()},
		{agenticv1alpha1.SandboxStepVerification, run.Spec.Verification, !run.Spec.Verification.IsZero()},
		{agenticv1alpha1.SandboxStepEscalation, run.Spec.Analysis, true},
	}
	for _, step := range steps {
		if !step.configured {
			continue
		}
		agentName := effectiveStepAgentName(approval, step.stage, step.step)
		if agentName == "" || slices.Contains(agents, agentName) {
			continue
		}
		agents = append(agents, agentName)
		agent, llm, issues := validateAgentDependencies(ctx, c, operatorNamespace, agentName)
		errors = append(errors, issues...)
		if len(issues) == 0 {
			validatedAgents[agentName] = agent
			validatedLLMProviders[agent.Spec.LLMProvider.Name] = llm
		}
	}
	return errors, validatedAgents, validatedLLMProviders
}

func resolveAgenticRun(ctx context.Context, c client.Client, run *agenticv1alpha1.AgenticRun, approval *agenticv1alpha1.AgenticRunApproval, operatorNamespace string) (*resolvedWorkflow, error) {
	validationErrors, agents, llmProviders := validateAgenticRun(ctx, c, run, approval, operatorNamespace)
	if len(validationErrors) > 0 {
		return nil, stderrors.Join(validationErrors...)
	}

	resolved := &resolvedWorkflow{}
	tools := &run.Spec.Tools

	agentName := effectiveStepAgentName(approval, agenticv1alpha1.SandboxStepAnalysis, run.Spec.Analysis)
	agent, ok := agents[agentName]
	if !ok {
		return nil, fmt.Errorf("%s: %s %q", ErrResolveAnalysisStep, ErrGetAgent, agentName)
	}
	llm, ok := llmProviders[agent.Spec.LLMProvider.Name]
	if !ok {
		return nil, fmt.Errorf("%s: %s %q (referenced by Agent %q)", ErrResolveAnalysisStep, ErrGetLLMProvider, agent.Spec.LLMProvider.Name, agentName)
	}
	resolved.Analysis = resolvedStep{Agent: agent, LLM: llm, Tools: tools}

	if !run.Spec.Execution.IsZero() {
		agentName := effectiveStepAgentName(approval, agenticv1alpha1.SandboxStepExecution, run.Spec.Execution)
		agent, ok = agents[agentName]
		if !ok {
			return nil, fmt.Errorf("%s: %s %q", ErrResolveExecutionStep, ErrGetAgent, agentName)
		}
		llm, ok = llmProviders[agent.Spec.LLMProvider.Name]
		if !ok {
			return nil, fmt.Errorf("%s: %s %q (referenced by Agent %q)", ErrResolveExecutionStep, ErrGetLLMProvider, agent.Spec.LLMProvider.Name, agentName)
		}
		resolved.Execution = &resolvedStep{Agent: agent, LLM: llm, Tools: tools}
	}

	if !run.Spec.Verification.IsZero() {
		agentName := effectiveStepAgentName(approval, agenticv1alpha1.SandboxStepVerification, run.Spec.Verification)
		agent, ok = agents[agentName]
		if !ok {
			return nil, fmt.Errorf("%s: %s %q", ErrResolveVerificationStep, ErrGetAgent, agentName)
		}
		llm, ok = llmProviders[agent.Spec.LLMProvider.Name]
		if !ok {
			return nil, fmt.Errorf("%s: %s %q (referenced by Agent %q)", ErrResolveVerificationStep, ErrGetLLMProvider, agent.Spec.LLMProvider.Name, agentName)
		}
		resolved.Verification = &resolvedStep{Agent: agent, LLM: llm, Tools: tools}
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
