package proposal

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"gomodules.xyz/jsonpatch/v3"
	admissionv1 "k8s.io/api/admission/v1"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
)

// ProposalApprovalMutator is a MutatingAdmissionWebhook handler that injects
// the authenticated user's identity into spec.approver on every UPDATE to a
// ProposalApproval, overwriting any client-submitted values.
type ProposalApprovalMutator struct{}

func (m *ProposalApprovalMutator) Handle(_ context.Context, req admission.Request) admission.Response {
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
