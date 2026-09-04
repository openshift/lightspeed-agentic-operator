package agenticrun

import (
	"encoding/json"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
)

const (
	ErrBuildInputConfigMap   = "build input ConfigMap"
	ErrBuildResultTemplate   = "build result template"
	ErrMarshalInputContext   = "marshal input context"
	ErrMarshalResultTemplate = "marshal result template"
	ErrUnknownStep           = "unknown step"
)

// resolvePrompts returns the system prompt and rendered query for the step.
// For each step: system prompt comes from Agent CR or empty (sandbox default).
// Query is rendered from Agent CR's custom template or the built-in template.
// Returns an error if template resolution or rendering fails.
func resolvePrompts(agent *agenticv1alpha1.Agent, step, operatorNamespace string, run *agenticv1alpha1.AgenticRun, agentCtx *agentContext) (systemPrompt, query string, err error) {
	var tmpl string
	switch step {
	case "analysis":
		systemPrompt, tmpl, err = resolveStepPrompts(agent, step, "templates/analysis_query.tmpl")
		if err != nil {
			return "", "", err
		}
		request := run.Spec.Request
		if run.Spec.RevisionFeedback != "" {
			revCtx, revErr := buildRevisionContext(run)
			if revErr != nil {
				return "", "", revErr
			}
			request += "\n\n" + revCtx
		}
		query, err = renderTemplate(tmpl, analysisQuery{
			Request:         request,
			HasExecution:    !run.Spec.Execution.IsZero(),
			HasVerification: !run.Spec.Verification.IsZero(),
		})
	case "execution":
		systemPrompt, tmpl, err = resolveStepPrompts(agent, step, "templates/execution_query.tmpl")
		if err != nil {
			return "", "", err
		}
		query, err = renderTemplate(tmpl, executionQuery{
			OptionJSON: prettyJSON(agentCtx.ApprovedOption),
		})
	case "verification":
		systemPrompt, tmpl, err = resolveStepPrompts(agent, step, "templates/verification_query.tmpl")
		if err != nil {
			return "", "", err
		}
		query, err = renderTemplate(tmpl, verificationQuery{
			OptionJSON:    prettyJSON(agentCtx.ApprovedOption),
			ExecutionJSON: prettyJSON(agentCtx.ExecutionResult),
		})
	case "escalation":
		systemPrompt, tmpl, err = resolveStepPrompts(agent, step, "templates/escalation_request.tmpl")
		if err != nil {
			return "", "", err
		}
		query, err = renderTemplate(tmpl, escalationData{
			Name:                run.Name,
			Namespace:           run.Namespace,
			ResultNamespace:     operatorNamespace,
			Request:             run.Spec.Request,
			AnalysisResults:     run.Status.Steps.Analysis.Results,
			ExecutionResults:    run.Status.Steps.Execution.Results,
			VerificationResults: run.Status.Steps.Verification.Results,
		})
	}
	return
}

// resolveStepPrompts returns the system prompt and user prompt template
// for the step. System prompt is from Agent CR or empty. User prompt
// template is from Agent CR or the built-in default.
func resolveStepPrompts(agent *agenticv1alpha1.Agent, step, builtinTemplate string) (systemPrompt, userPromptTemplate string, err error) {
	var si *agenticv1alpha1.StepInstructions
	if agent != nil && agent.Spec.Instructions != nil {
		switch step {
		case "analysis":
			si = agent.Spec.Instructions.Analysis
		case "execution":
			si = agent.Spec.Instructions.Execution
		case "verification":
			si = agent.Spec.Instructions.Verification
		case "escalation":
			si = agent.Spec.Instructions.Escalation
		}
	}

	if si != nil {
		systemPrompt = si.SystemPrompt
		if si.UserPrompt != "" {
			return systemPrompt, si.UserPrompt, nil
		}
	}

	tmpl, err := readBuiltinTemplate(builtinTemplate)
	if err != nil {
		return "", "", err
	}
	return systemPrompt, tmpl, nil
}

// inputConfigMapName returns the per-step ConfigMap name: ls-{step}-{uid}.
func inputConfigMapName(step string, uid string) string {
	return fmt.Sprintf("ls-%s-%s", step, uid)
}

// buildInputConfigMap builds the batch input ConfigMap for a step (rule 7).
// Name is ls-{step}-{uid}, unique per step to prevent GC conflicts.
// Resolves both system-prompt and query internally from the Agent CR
// (custom templates) or built-in defaults.
func buildInputConfigMap(
	operatorNamespace string,
	run *agenticv1alpha1.AgenticRun,
	step string,
	agent *agenticv1alpha1.Agent,
	schema json.RawMessage,
	agentCtx *agentContext,
) (*corev1.ConfigMap, error) {
	if agentCtx == nil {
		agentCtx = &agentContext{}
	}
	ctxJSON, err := json.Marshal(agentCtx)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", ErrMarshalInputContext, err)
	}
	tmpl, err := buildResultTemplate(run, step, operatorNamespace)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", ErrBuildInputConfigMap, err)
	}

	systemPrompt, query, err := resolvePrompts(agent, step, operatorNamespace, run, agentCtx)
	if err != nil {
		return nil, fmt.Errorf("resolve prompts for step %s: %w", step, err)
	}

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      inputConfigMapName(step, string(run.UID)),
			Namespace: operatorNamespace,
			Labels: map[string]string{
				LabelRun:  string(run.UID),
				LabelStep: step,
			},
			OwnerReferences: []metav1.OwnerReference{agenticRunOwnerRef(run)},
		},
		Data: map[string]string{
			inputConfigMapKeyQuery:  query,
			inputConfigMapKeySchema: string(schema),
			inputConfigMapKeyCtx:    string(ctxJSON),
			inputConfigMapKeyTmpl:   tmpl,
		},
	}
	if systemPrompt != "" {
		cm.Data[inputConfigMapKeySystemPrompt] = systemPrompt
	}
	return cm, nil
}

// buildResultTemplate returns JSON for result-template (rule 7a): apiVersion,
// kind, metadata, and spec only — sandbox fills status.
func buildResultTemplate(run *agenticv1alpha1.AgenticRun, step, namespace string) (string, error) {
	index := nextResultIndex(run, step)
	name := resultCRName(run.Name, step, index)
	apiVersion := agenticv1alpha1.GroupVersion.String()
	ownerRef := agenticRunOwnerRef(run)

	var obj any
	switch step {
	case "analysis":
		obj = &agenticv1alpha1.AnalysisResult{
			TypeMeta: metav1.TypeMeta{APIVersion: apiVersion, Kind: "AnalysisResult"},
			ObjectMeta: metav1.ObjectMeta{
				Name:            name,
				Namespace:       namespace,
				Labels:          resultLabels(string(run.UID), step),
				OwnerReferences: []metav1.OwnerReference{ownerRef},
			},
			Spec: agenticv1alpha1.AnalysisResultSpec{AgenticRunName: run.Name},
		}
	case "execution":
		obj = &agenticv1alpha1.ExecutionResult{
			TypeMeta: metav1.TypeMeta{APIVersion: apiVersion, Kind: "ExecutionResult"},
			ObjectMeta: metav1.ObjectMeta{
				Name:            name,
				Namespace:       namespace,
				Labels:          resultLabels(string(run.UID), step),
				OwnerReferences: []metav1.OwnerReference{ownerRef},
			},
			Spec: agenticv1alpha1.ExecutionResultSpec{
				AgenticRunName: run.Name,
			},
		}
	case "verification":
		obj = &agenticv1alpha1.VerificationResult{
			TypeMeta: metav1.TypeMeta{APIVersion: apiVersion, Kind: "VerificationResult"},
			ObjectMeta: metav1.ObjectMeta{
				Name:            name,
				Namespace:       namespace,
				Labels:          resultLabels(string(run.UID), step),
				OwnerReferences: []metav1.OwnerReference{ownerRef},
			},
			Spec: agenticv1alpha1.VerificationResultSpec{
				AgenticRunName: run.Name,
			},
		}
	case "escalation":
		obj = &agenticv1alpha1.EscalationResult{
			TypeMeta: metav1.TypeMeta{APIVersion: apiVersion, Kind: "EscalationResult"},
			ObjectMeta: metav1.ObjectMeta{
				Name:            name,
				Namespace:       namespace,
				Labels:          resultLabels(string(run.UID), step),
				OwnerReferences: []metav1.OwnerReference{ownerRef},
			},
			Spec: agenticv1alpha1.EscalationResultSpec{AgenticRunName: run.Name},
		}
	default:
		return "", fmt.Errorf("%s: %s %q", ErrBuildResultTemplate, ErrUnknownStep, step)
	}

	raw, err := json.Marshal(obj)
	if err != nil {
		return "", fmt.Errorf("%s: %w", ErrMarshalResultTemplate, err)
	}
	return string(raw), nil
}

func nextResultIndex(run *agenticv1alpha1.AgenticRun, step string) int {
	switch step {
	case "analysis":
		return len(run.Status.Steps.Analysis.Results) + 1
	case "execution":
		return len(run.Status.Steps.Execution.Results) + 1
	case "verification":
		return len(run.Status.Steps.Verification.Results) + 1
	case "escalation":
		return len(run.Status.Steps.Escalation.Results) + 1
	default:
		return 1
	}
}
