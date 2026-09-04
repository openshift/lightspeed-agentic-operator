package agenticrun

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"text/template"
	"time"
	"unicode/utf8"

	"gomodules.xyz/jsonpatch/v2"
	admissionv1 "k8s.io/api/admission/v1"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
)

// --- AgenticRunApproval mutating webhook ---

// AgenticRunApprovalMutator is a MutatingAdmissionWebhook handler that injects
// the authenticated user's identity into spec.approver on every UPDATE to a
// AgenticRunApproval, overwriting any client-submitted values.
type AgenticRunApprovalMutator struct{}

func (m *AgenticRunApprovalMutator) Handle(_ context.Context, req admission.Request) admission.Response {
	// Defensive: manifest registers UPDATE only; CREATE never reaches this handler.
	if req.Operation == admissionv1.Create {
		return admission.Allowed("create passes through")
	}

	approver := agenticv1alpha1.ApproverInfo{
		UID:        req.UserInfo.UID,
		Username:   req.UserInfo.Username,
		ApprovedAt: time.Now().UTC().Format(time.RFC3339),
	}

	approverJSON, err := json.Marshal(approver)
	if err != nil {
		return admission.Errored(http.StatusInternalServerError, err)
	}

	var rawApprover json.RawMessage = approverJSON

	var obj map[string]json.RawMessage
	if err := json.Unmarshal(req.Object.Raw, &obj); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}

	var spec map[string]json.RawMessage
	hasSpec := false
	if specRaw, ok := obj["spec"]; ok && string(specRaw) != "null" {
		hasSpec = true
		if err := json.Unmarshal(specRaw, &spec); err != nil {
			return admission.Errored(http.StatusBadRequest, err)
		}
	}

	var patches []jsonpatch.JsonPatchOperation
	if !hasSpec {
		patches = []jsonpatch.JsonPatchOperation{
			{Operation: "add", Path: "/spec", Value: map[string]interface{}{"approver": rawApprover}},
		}
		return admission.Patched("injected spec.approver", patches...)
	}

	_, hasApprover := spec["approver"]
	switch {
	case !hasApprover:
		patches = []jsonpatch.JsonPatchOperation{
			{Operation: "add", Path: "/spec/approver", Value: &rawApprover},
		}
	default:
		patches = []jsonpatch.JsonPatchOperation{
			{Operation: "replace", Path: "/spec/approver", Value: &rawApprover},
		}
	}

	return admission.Patched("injected spec.approver", patches...)
}

// --- Agent validating webhook ---

const maxPromptLength = 32768

// AgentValidator is a ValidatingAdmissionWebhook handler that rejects
// Agent create/update when any prompt is too long or contains invalid Go
// template syntax.
type AgentValidator struct{}

func (v *AgentValidator) Handle(_ context.Context, req admission.Request) admission.Response {
	var agent agenticv1alpha1.Agent
	if err := json.Unmarshal(req.Object.Raw, &agent); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}

	if agent.Spec.Instructions == nil {
		return admission.Allowed("no instructions configured")
	}

	steps := map[string]*agenticv1alpha1.StepInstructions{
		"analysis":     agent.Spec.Instructions.Analysis,
		"execution":    agent.Spec.Instructions.Execution,
		"verification": agent.Spec.Instructions.Verification,
		"escalation":   agent.Spec.Instructions.Escalation,
	}

	for step, si := range steps {
		if si == nil {
			continue
		}
		if utf8.RuneCountInString(si.SystemPrompt) > maxPromptLength {
			return admission.Denied(fmt.Sprintf(
				"spec.instructions.%s.systemPrompt: must not exceed %d characters", step, maxPromptLength))
		}
		if si.UserPrompt == "" {
			continue
		}
		if utf8.RuneCountInString(si.UserPrompt) > maxPromptLength {
			return admission.Denied(fmt.Sprintf(
				"spec.instructions.%s.userPrompt: must not exceed %d characters", step, maxPromptLength))
		}
		if _, err := template.New(step).Parse(si.UserPrompt); err != nil {
			return admission.Denied(fmt.Sprintf(
				"spec.instructions.%s.userPrompt: invalid Go template: %v", step, err))
		}
	}

	return admission.Allowed("templates valid")
}
