package proposal

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
)

func TestAgentHTTPClient_RunSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/agent/run" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Errorf("unexpected method: %s", r.Method)
		}

		var req agentRunRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("failed to decode request: %v", err)
		}
		if req.Query != "check health" {
			t.Errorf("expected query='check health', got %q", req.Query)
		}
		if req.SystemPrompt != "You are an SRE agent" {
			t.Errorf("expected systemPrompt='You are an SRE agent', got %q", req.SystemPrompt)
		}

		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"options": [{"title": "Fix it"}]}`))
	}))
	defer server.Close()

	client := NewAgentHTTPClient(server.URL, 0)
	resp, err := client.Run(context.Background(), "You are an SRE agent", "check health", nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(resp.Response) == 0 {
		t.Error("expected non-empty response")
	}
}

func TestAgentHTTPClient_RunHTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("internal error"))
	}))
	defer server.Close()

	client := NewAgentHTTPClient(server.URL, 0)
	_, err := client.Run(context.Background(), "", "test", nil, nil)
	if err == nil {
		t.Fatal("expected error for HTTP 500")
	}
}

func TestAgentHTTPClient_RunConnectionError(t *testing.T) {
	client := NewAgentHTTPClient("http://127.0.0.1:1", 0)
	_, err := client.Run(context.Background(), "", "test", nil, nil)
	if err == nil {
		t.Fatal("expected error for connection failure")
	}
}

func TestAgentHTTPClient_RunWithExecutionResult(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req agentRunRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("failed to decode request: %v", err)
		}
		if req.Context == nil {
			t.Fatal("expected context to be set")
		}
		if req.Context.ExecutionResult == nil {
			t.Fatal("expected executionResult in context")
		}
		if !req.Context.ExecutionResult.Success {
			t.Error("executionResult.success should be true")
		}
		if len(req.Context.ExecutionResult.ActionsTaken) != 1 {
			t.Fatalf("actionsTaken count = %d, want 1", len(req.Context.ExecutionResult.ActionsTaken))
		}
		if req.Context.ExecutionResult.ActionsTaken[0].Description != "Patched deployment" {
			t.Errorf("actionsTaken[0].description = %q", req.Context.ExecutionResult.ActionsTaken[0].Description)
		}
		if req.Context.ExecutionResult.Verification == nil {
			t.Fatal("expected verification in executionResult")
		}
		if req.Context.ExecutionResult.Verification.Summary != "Pod running" {
			t.Errorf("verification.summary = %q", req.Context.ExecutionResult.Verification.Summary)
		}

		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"success": true}`))
	}))
	defer server.Close()

	client := NewAgentHTTPClient(server.URL, 0)
	agentCtx := &agentContext{
		TargetNamespaces: []string{"production"},
		ExecutionResult: &agentExecutionResult{
			Success: true,
			ActionsTaken: []agenticv1alpha1.ExecutionAction{
				{Type: "patch", Description: "Patched deployment", Outcome: "Succeeded"},
			},
			Verification: &agenticv1alpha1.ExecutionVerification{
				ConditionOutcome: "Improved",
				Summary:          "Pod running",
			},
		},
	}
	_, err := client.Run(context.Background(), "", "test", nil, agentCtx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestAgentHTTPClient_RunWithoutExecutionResult(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req agentRunRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("failed to decode request: %v", err)
		}
		if req.Context != nil && req.Context.ExecutionResult != nil {
			t.Error("executionResult should not be present")
		}

		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"success": true}`))
	}))
	defer server.Close()

	client := NewAgentHTTPClient(server.URL, 0)
	agentCtx := &agentContext{
		TargetNamespaces: []string{"production"},
	}
	_, err := client.Run(context.Background(), "", "test", nil, agentCtx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestAgentHTTPClient_RunWithContext(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req agentRunRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("failed to decode request: %v", err)
		}
		if req.Context == nil {
			t.Fatal("expected context to be set")
		}
		if len(req.Context.TargetNamespaces) != 1 || req.Context.TargetNamespaces[0] != "production" {
			t.Errorf("targetNamespaces = %v", req.Context.TargetNamespaces)
		}
		if len(req.Context.PreviousAttempts) != 1 {
			t.Fatalf("previousAttempts count = %d, want 1", len(req.Context.PreviousAttempts))
		}
		if req.Context.PreviousAttempts[0].FailureReason != "timeout" {
			t.Errorf("failureReason = %q", req.Context.PreviousAttempts[0].FailureReason)
		}

		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"success": true}`))
	}))
	defer server.Close()

	client := NewAgentHTTPClient(server.URL, 0)
	agentCtx := &agentContext{
		TargetNamespaces: []string{"production"},
		PreviousAttempts: []agentPreviousAttempt{{Attempt: 1, FailureReason: "timeout"}},
	}
	_, err := client.Run(context.Background(), "", "test", nil, agentCtx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}
