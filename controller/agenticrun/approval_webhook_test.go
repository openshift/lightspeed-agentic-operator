package agenticrun

import (
	"context"
	"encoding/json"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
)

func makeApprovalJSON(t *testing.T, approval *agenticv1alpha1.AgenticRunApproval) []byte {
	t.Helper()
	raw, err := json.Marshal(approval)
	if err != nil {
		t.Fatalf("marshal AgenticRunApproval: %v", err)
	}
	return raw
}

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

func int32Ptr(i int32) *int32 {
	return &i
}
