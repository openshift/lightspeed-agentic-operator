package agenticrun

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
)

// --- Helpers ---

func makeApprovalJSON(t *testing.T, approval *agenticv1alpha1.AgenticRunApproval) []byte {
	t.Helper()
	raw, err := json.Marshal(approval)
	if err != nil {
		t.Fatalf("marshal AgenticRunApproval: %v", err)
	}
	return raw
}

func makeAgentJSON(t *testing.T, agent *agenticv1alpha1.Agent) []byte {
	t.Helper()
	raw, err := json.Marshal(agent)
	if err != nil {
		t.Fatalf("marshal Agent: %v", err)
	}
	return raw
}

func agentRequest(t *testing.T, agent *agenticv1alpha1.Agent) admission.Request {
	t.Helper()
	return admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			Operation: admissionv1.Create,
			Object:    runtime.RawExtension{Raw: makeAgentJSON(t, agent)},
		},
	}
}

func int32Ptr(i int32) *int32 {
	return &i
}

// --- AgenticRunApproval mutator tests ---

func TestApprovalWebhook_InjectsApproverOnUpdate(t *testing.T) {
	approval := &agenticv1alpha1.AgenticRunApproval{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-run",
			Namespace: "test-ns",
			OwnerReferences: []metav1.OwnerReference{
				{Kind: "AgenticRun", UID: "550e8400-e29b-41d4-a716-446655440000"},
			},
		},
		Spec: agenticv1alpha1.AgenticRunApprovalSpec{
			Stages: []agenticv1alpha1.ApprovalStage{
				{Type: agenticv1alpha1.ApprovalStageExecution, Execution: &agenticv1alpha1.ExecutionApproval{Option: int32Ptr(1)}},
			},
		},
	}

	m := &AgenticRunApprovalMutator{}
	req := admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			Operation: admissionv1.Update,
			UserInfo: authenticationv1.UserInfo{
				Username: "admin@example.com",
				UID:      "user-uid-123",
			},
			Object: runtime.RawExtension{Raw: makeApprovalJSON(t, approval)},
		},
	}

	resp := m.Handle(context.Background(), req)

	if !resp.Allowed {
		t.Fatalf("expected allowed, got denied: %v", resp.Result)
	}
	if len(resp.Patches) == 0 {
		t.Fatal("expected patches, got none")
	}

	found := false
	for _, p := range resp.Patches {
		if p.Path == "/spec/approver" {
			found = true
			approverMap, ok := p.Value.(*json.RawMessage)
			if !ok {
				t.Fatalf("unexpected approver patch value type: %T", p.Value)
			}
			var approver agenticv1alpha1.ApproverInfo
			if err := json.Unmarshal(*approverMap, &approver); err != nil {
				t.Fatalf("unmarshal approver: %v", err)
			}
			if approver.Username != "admin@example.com" {
				t.Errorf("username = %q, want %q", approver.Username, "admin@example.com")
			}
			if approver.UID != "user-uid-123" {
				t.Errorf("uid = %q, want %q", approver.UID, "user-uid-123")
			}
			if approver.ApprovedAt == "" {
				t.Error("timestamp is empty")
			}
		}
	}
	if !found {
		t.Error("no /spec/approver patch found")
	}
}

func TestApprovalWebhook_OverwritesClientSubmittedApprover(t *testing.T) {
	approval := &agenticv1alpha1.AgenticRunApproval{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-run",
			Namespace: "test-ns",
			OwnerReferences: []metav1.OwnerReference{
				{Kind: "AgenticRun", UID: "550e8400-e29b-41d4-a716-446655440000"},
			},
		},
		Spec: agenticv1alpha1.AgenticRunApprovalSpec{
			Approver: agenticv1alpha1.ApproverInfo{
				UID:        "fake-uid",
				Username:   "fake-user",
				ApprovedAt: "2020-01-01T00:00:00Z",
			},
			Stages: []agenticv1alpha1.ApprovalStage{
				{Type: agenticv1alpha1.ApprovalStageAnalysis, Analysis: &agenticv1alpha1.AnalysisApproval{}},
			},
		},
	}

	m := &AgenticRunApprovalMutator{}
	req := admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			Operation: admissionv1.Update,
			UserInfo: authenticationv1.UserInfo{
				Username: "real-admin",
				UID:      "real-uid",
			},
			Object: runtime.RawExtension{Raw: makeApprovalJSON(t, approval)},
		},
	}

	resp := m.Handle(context.Background(), req)

	if !resp.Allowed {
		t.Fatalf("expected allowed, got denied")
	}

	for _, p := range resp.Patches {
		if p.Path == "/spec/approver" {
			if p.Operation != "replace" {
				t.Errorf("operation = %q, want replace (overwrite)", p.Operation)
			}
			approverMap, ok := p.Value.(*json.RawMessage)
			if !ok {
				t.Fatalf("unexpected approver patch value type: %T", p.Value)
			}
			var approver agenticv1alpha1.ApproverInfo
			if err := json.Unmarshal(*approverMap, &approver); err != nil {
				t.Fatalf("unmarshal approver: %v", err)
			}
			if approver.Username != "real-admin" {
				t.Errorf("username = %q, want real-admin", approver.Username)
			}
			return
		}
	}
	t.Error("no /spec/approver patch found")
}

func TestApprovalWebhook_CreatePassesThrough(t *testing.T) {
	approval := &agenticv1alpha1.AgenticRunApproval{
		ObjectMeta: metav1.ObjectMeta{Name: "test-run", Namespace: "test-ns"},
		Spec: agenticv1alpha1.AgenticRunApprovalSpec{
			Stages: []agenticv1alpha1.ApprovalStage{
				{Type: agenticv1alpha1.ApprovalStageAnalysis, Analysis: &agenticv1alpha1.AnalysisApproval{}},
			},
		},
	}

	m := &AgenticRunApprovalMutator{}
	req := admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			Operation: admissionv1.Create,
			Object:    runtime.RawExtension{Raw: makeApprovalJSON(t, approval)},
		},
	}

	resp := m.Handle(context.Background(), req)

	if !resp.Allowed {
		t.Fatalf("expected allowed for CREATE")
	}
	if len(resp.Patches) > 0 {
		t.Errorf("expected no patches for CREATE, got %d", len(resp.Patches))
	}
}

func TestApprovalWebhook_MissingOwnerRef(t *testing.T) {
	approval := &agenticv1alpha1.AgenticRunApproval{
		ObjectMeta: metav1.ObjectMeta{Name: "orphan", Namespace: "test-ns"},
		Spec: agenticv1alpha1.AgenticRunApprovalSpec{
			Stages: []agenticv1alpha1.ApprovalStage{
				{Type: agenticv1alpha1.ApprovalStageAnalysis, Analysis: &agenticv1alpha1.AnalysisApproval{}},
			},
		},
	}

	m := &AgenticRunApprovalMutator{}
	req := admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			Operation: admissionv1.Update,
			UserInfo: authenticationv1.UserInfo{
				Username: "admin",
				UID:      "uid-456",
			},
			Object: runtime.RawExtension{Raw: makeApprovalJSON(t, approval)},
		},
	}

	resp := m.Handle(context.Background(), req)

	if !resp.Allowed {
		t.Fatal("expected allowed even with missing owner ref")
	}
	if len(resp.Patches) == 0 {
		t.Fatal("expected approver patch even with missing owner ref")
	}
}

func TestApprovalWebhook_AddsApproverWhenNoExistingApprover(t *testing.T) {
	approval := &agenticv1alpha1.AgenticRunApproval{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-run",
			Namespace: "test-ns",
			OwnerReferences: []metav1.OwnerReference{
				{Kind: "AgenticRun", UID: "550e8400-e29b-41d4-a716-446655440000"},
			},
		},
		Spec: agenticv1alpha1.AgenticRunApprovalSpec{
			Stages: []agenticv1alpha1.ApprovalStage{
				{Type: agenticv1alpha1.ApprovalStageAnalysis, Analysis: &agenticv1alpha1.AnalysisApproval{}},
			},
		},
	}

	m := &AgenticRunApprovalMutator{}
	req := admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			Operation: admissionv1.Update,
			UserInfo: authenticationv1.UserInfo{
				Username: "admin",
				UID:      "uid-789",
			},
			Object: runtime.RawExtension{Raw: makeApprovalJSON(t, approval)},
		},
	}

	resp := m.Handle(context.Background(), req)
	if !resp.Allowed {
		t.Fatal("expected allowed")
	}

	for _, p := range resp.Patches {
		if p.Path == "/spec/approver" {
			if p.Operation != "add" {
				t.Errorf("operation = %q, want add (no existing approver)", p.Operation)
			}
			return
		}
	}
	t.Error("no /spec/approver patch found")
}

func TestApprovalWebhook_MissingSpec(t *testing.T) {
	raw := []byte(`{"apiVersion":"agentic.openshift.io/v1alpha1","kind":"AgenticRunApproval","metadata":{"name":"no-spec","namespace":"test-ns"}}`)

	m := &AgenticRunApprovalMutator{}
	req := admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			Operation: admissionv1.Update,
			UserInfo: authenticationv1.UserInfo{
				Username: "admin",
				UID:      "uid-missing-spec",
			},
			Object: runtime.RawExtension{Raw: raw},
		},
	}

	resp := m.Handle(context.Background(), req)
	if !resp.Allowed {
		t.Fatalf("expected allowed, got denied: %v", resp.Result)
	}
	if len(resp.Patches) == 0 {
		t.Fatal("expected patches, got none")
	}
	for _, p := range resp.Patches {
		if p.Path == "/spec" && p.Operation == "add" {
			return
		}
	}
	t.Error("expected add /spec patch when spec is missing")
}

// --- Agent validator tests ---

func TestAgentValidator_NoInstructions(t *testing.T) {
	agent := &agenticv1alpha1.Agent{}
	resp := (&AgentValidator{}).Handle(context.Background(), agentRequest(t, agent))
	if !resp.Allowed {
		t.Fatalf("expected allowed, got denied: %s", resp.Result.Message)
	}
}

func TestAgentValidator_ValidTemplates(t *testing.T) {
	agent := &agenticv1alpha1.Agent{
		Spec: agenticv1alpha1.AgentSpec{
			Instructions: &agenticv1alpha1.AgentInstructions{
				Analysis: &agenticv1alpha1.StepInstructions{
					SystemPrompt: "You are an auditor.",
					UserPrompt:   "Audit: {{.Request}}",
				},
				Execution: &agenticv1alpha1.StepInstructions{
					UserPrompt: "Execute option:\n{{.OptionJSON}}",
				},
			},
		},
	}
	resp := (&AgentValidator{}).Handle(context.Background(), agentRequest(t, agent))
	if !resp.Allowed {
		t.Fatalf("expected allowed, got denied: %s", resp.Result.Message)
	}
}

func TestAgentValidator_PromptLength(t *testing.T) {
	for _, tc := range []struct {
		name   string
		field  string
		length int
		allow  bool
	}{
		{name: "systemPrompt at maximum", field: "systemPrompt", length: maxPromptLength, allow: true},
		{name: "systemPrompt over maximum", field: "systemPrompt", length: maxPromptLength + 1},
		{name: "userPrompt at maximum", field: "userPrompt", length: maxPromptLength, allow: true},
		{name: "userPrompt over maximum", field: "userPrompt", length: maxPromptLength + 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			instructions := &agenticv1alpha1.StepInstructions{}
			if tc.field == "systemPrompt" {
				instructions.SystemPrompt = strings.Repeat("x", tc.length)
			} else {
				instructions.UserPrompt = strings.Repeat("x", tc.length)
			}
			agent := &agenticv1alpha1.Agent{
				Spec: agenticv1alpha1.AgentSpec{
					Instructions: &agenticv1alpha1.AgentInstructions{Analysis: instructions},
				},
			}

			resp := (&AgentValidator{}).Handle(context.Background(), agentRequest(t, agent))
			if resp.Allowed != tc.allow {
				t.Fatalf("allowed = %v, want %v: %s", resp.Allowed, tc.allow, resp.Result.Message)
			}
		})
	}
}

func TestAgentValidator_MalformedTemplate(t *testing.T) {
	agent := &agenticv1alpha1.Agent{
		Spec: agenticv1alpha1.AgentSpec{
			Instructions: &agenticv1alpha1.AgentInstructions{
				Analysis: &agenticv1alpha1.StepInstructions{
					UserPrompt: "{{.Unclosed",
				},
			},
		},
	}
	resp := (&AgentValidator{}).Handle(context.Background(), agentRequest(t, agent))
	if resp.Allowed {
		t.Fatal("expected denied for malformed template, got allowed")
	}
	if !strings.Contains(resp.Result.Message, "spec.instructions.analysis.userPrompt") {
		t.Errorf("expected field path in message, got: %s", resp.Result.Message)
	}
}

func TestAgentValidator_InvalidFieldInOneStep(t *testing.T) {
	agent := &agenticv1alpha1.Agent{
		Spec: agenticv1alpha1.AgentSpec{
			Instructions: &agenticv1alpha1.AgentInstructions{
				Analysis: &agenticv1alpha1.StepInstructions{
					UserPrompt: "Valid: {{.Request}}",
				},
				Verification: &agenticv1alpha1.StepInstructions{
					UserPrompt: "Bad: {{.Unclosed",
				},
			},
		},
	}
	resp := (&AgentValidator{}).Handle(context.Background(), agentRequest(t, agent))
	if resp.Allowed {
		t.Fatal("expected denied when one step has invalid template")
	}
	if !strings.Contains(resp.Result.Message, "verification") {
		t.Errorf("expected verification step in message, got: %s", resp.Result.Message)
	}
}

func TestAgentValidator_SystemPromptOnly_Allowed(t *testing.T) {
	agent := &agenticv1alpha1.Agent{
		Spec: agenticv1alpha1.AgentSpec{
			Instructions: &agenticv1alpha1.AgentInstructions{
				Analysis: &agenticv1alpha1.StepInstructions{
					SystemPrompt: "You are a security agent.",
				},
			},
		},
	}
	resp := (&AgentValidator{}).Handle(context.Background(), agentRequest(t, agent))
	if !resp.Allowed {
		t.Fatalf("expected allowed when only systemPrompt is set, got denied: %s", resp.Result.Message)
	}
}
