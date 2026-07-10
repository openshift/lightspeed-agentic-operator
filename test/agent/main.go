// Mock agent HTTP server for e2e and local testing. Implements the same
// contract as the in-sandbox agent service: POST /v1/agent/run with JSON
// body matching controller/agenticrun/client.go (query, systemPrompt,
// outputSchema, context, timeout_ms). Response body is raw JSON matching
// controller/agenticrun/sandbox_agent.go expectations per step.
//
// Request JSON must stay in sync with agentRunRequest + agentContext in
// controller/agenticrun/client.go. Response bodies must unmarshal into the
// per-step structs in controller/agenticrun/sandbox_agent.go (analysisResponse,
// executionResponse, verificationResponse, and the anonymous struct for
// Escalate).
//
// After editing this binary, rebuild and restart the process so callers hit
// the new behavior.
//
// Run: go run ./test/agent -addr :8080
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
	agenticrun "github.com/openshift/lightspeed-agentic-operator/controller/agenticrun"
)

// Per-phase response delays (hardcoded). Execution and verification get 60s delay so that
// e2e tests can observe intermediate state (e.g. RBAC exists while execution is in-flight).

type runRequest struct {
	Query        string          `json:"query"`
	SystemPrompt string          `json:"systemPrompt,omitempty"`
	OutputSchema json.RawMessage `json:"outputSchema,omitempty"`
	Context      *runContext     `json:"context,omitempty"`
	TimeoutMs    *int64          `json:"timeout_ms,omitempty"`
}

// runContext mirrors controller/agenticrun/client.go agentContext JSON so the
// operator's marshaled requests decode without dropping fields.
type runContext struct {
	TargetNamespaces []string                           `json:"targetNamespaces,omitempty"`
	PreviousAttempts []runPreviousAttempt               `json:"previousAttempts,omitempty"`
	ApprovedOption   *agenticv1alpha1.RemediationOption `json:"approvedOption,omitempty"`
	ExecutionResult  *runExecutionResult                `json:"executionResult,omitempty"`
}

type runPreviousAttempt struct {
	Attempt       int32  `json:"attempt"`
	FailureReason string `json:"failureReason,omitempty"`
}

type runExecutionResult struct {
	Success      bool                                   `json:"success"`
	ActionsTaken []agenticv1alpha1.ExecutionAction      `json:"actionsTaken"`
	Verification *agenticv1alpha1.ExecutionVerification `json:"verification,omitempty"`
}

func main() {
	addr := flag.String("addr", ":8080", "listen address (e.g. :8080)")
	flag.Parse()

	listen := *addr
	if v := os.Getenv("MOCK_AGENT_ADDR"); v != "" {
		listen = v
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/agent/run", handleRun)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/ready", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" && r.Method == http.MethodGet {
			http.Redirect(w, r, "/healthz", http.StatusFound)
			return
		}
		http.NotFound(w, r)
	})

	log.Printf("mock agent listening on %s (POST /v1/agent/run, GET /healthz, GET /health, GET /ready)", listen)
	log.Printf("note: rebuild and restart this process after changing mock code or controller schemas")
	if err := http.ListenAndServe(listen, logRequests(mux)); err != nil {
		log.Fatalf("server: %v", err)
	}
}

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Printf("%s %s", r.Method, r.URL.Path)
		next.ServeHTTP(w, r)
	})
}

const maxRequestLogBytes = 8192

func handleRun(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	ct := r.Header.Get("Content-Type")
	if !strings.HasPrefix(ct, "application/json") {
		log.Printf("/v1/agent/run bad Content-Type remote=%s ct=%q", r.RemoteAddr, ct)
		http.Error(w, "expected application/json", http.StatusBadRequest)
		return
	}

	const maxBodyBytes = 10 << 20 // 10 MB
	rawBody, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	if err != nil {
		log.Printf("/v1/agent/run read body remote=%s err=%v", r.RemoteAddr, err)
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	preview := string(rawBody)
	if len(preview) > maxRequestLogBytes {
		preview = preview[:maxRequestLogBytes] + fmt.Sprintf("… (%d bytes total)", len(rawBody))
	}
	log.Printf("/v1/agent/run remote=%s content-type=%q content-length=%d body_bytes=%d body=%s",
		r.RemoteAddr, ct, r.ContentLength, len(rawBody), preview)

	var req runRequest
	if err := json.NewDecoder(bytes.NewReader(rawBody)).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("invalid json: %v", err), http.StatusBadRequest)
		return
	}

	ns := pickNamespace(req.Context)
	phase := phaseFromSchema(req.OutputSchema)
	log.Printf("/v1/agent/run decoded query_len=%d phase=%s target_ns=%s", len(req.Query), phase, ns)

	if d := phaseDelay(phase); d > 0 {
		log.Printf("/v1/agent/run delaying %s for phase=%s", d, phase)
		time.Sleep(d)
	}

	body := cannedResponse(phase, ns)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(body); err != nil {
		log.Printf("write response: %v", err)
	}
}

// Hardcoded per-phase delays to simulate real agent work. Gives e2e tests a
// window to observe intermediate state (e.g. RBAC exists while execution is in-flight).
func phaseDelay(phase string) time.Duration {
	switch phase {
	case "execution", "verification":
		return 60 * time.Second
	default:
		return 0
	}
}

func pickNamespace(ctx *runContext) string {
	if ctx != nil && len(ctx.TargetNamespaces) > 0 && ctx.TargetNamespaces[0] != "" {
		return ctx.TargetNamespaces[0]
	}
	return "default"
}

func compactJSON(raw json.RawMessage) []byte {
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return raw
	}
	return buf.Bytes()
}

func phaseFromSchema(schema json.RawMessage) string {
	compact := compactJSON(schema)
	switch {
	case bytes.Equal(compact, compactJSON(agenticrun.ExecutionOutputSchema)):
		return "execution"
	case bytes.Equal(compact, compactJSON(agenticrun.VerificationOutputSchema)):
		return "verification"
	case bytes.Equal(compact, compactJSON(agenticrun.EscalationOutputSchema)):
		return "escalation"
	default:
		return "analysis"
	}
}

func cannedResponse(phase, targetNS string) []byte {
	switch phase {
	case "execution":
		return []byte(`{
  "success": true,
  "actionsTaken": [
    {
      "type": "mock",
      "description": "mock execution action",
      "outcome": "Succeeded"
    }
  ],
  "verification": {
    "conditionOutcome": "Improved",
    "summary": "mock inline verification"
  }
}`)
	case "verification":
		return []byte(`{
  "success": true,
  "checks": [
    {
      "name": "mock-check",
      "source": "mock",
      "value": "ok",
      "result": "Passed"
    }
  ],
  "summary": "mock verification summary"
}`)
	case "escalation":
		return []byte(`{
  "success": true,
  "summary": "mock escalation summary",
  "content": "mock escalation content"
}`)
	default:
		// Analysis: one option with diagnosis, proposal, rbac, verification (default workflow shape).
		return []byte(fmt.Sprintf(`{
  "success": true,
  "options": [
    {
      "title": "mock-remediation",
      "summary": "mock option summary",
      "diagnosis": {
        "summary": "mock diagnosis",
        "confidence": "High",
        "rootCause": "mock root cause"
      },
      "remediationPlan": {
        "description": "mock proposal description",
        "actions": [
          { "command": "kubectl get configmap -n %s", "type": "pre-check", "description": "Check current configmap state" },
          { "command": "kubectl patch configmap mock-cm -n %s -p '{\"data\":{\"key\":\"value\"}}'", "type": "mutation", "description": "Patch configmap with fix" },
          { "command": "kubectl get configmap mock-cm -n %s -o jsonpath='{.data.key}'", "type": "post-check", "description": "Verify configmap was patched" }
        ],
        "risk": "Low",
        "reversible": "Reversible"
      },
      "verification": {
        "description": "mock verification plan",
        "steps": [
          {
            "name": "mock-step",
            "command": "true",
            "expected": "ok",
            "type": "command"
          }
        ]
      },
      "rbac": {
        "namespaceScoped": [
          {
            "namespace": %q,
            "apiGroups": [""],
            "resources": ["configmaps"],
            "verbs": ["get", "list", "patch"],
            "justification": "Read and patch configmaps for mock remediation"
          }
        ],
        "clusterScoped": []
      }
    }
  ]
}`, targetNS, targetNS, targetNS, targetNS))
	}
}
