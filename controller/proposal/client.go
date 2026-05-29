package proposal

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
)

const (
	maxErrorBodyLen = 500
	maxResponseSize = 2 << 20 // 2 MiB
	runPath         = "/v1/agent/run"
)

type agentRunRequest struct {
	Query        string          `json:"query"`
	SystemPrompt string          `json:"systemPrompt,omitempty"`
	OutputSchema json.RawMessage `json:"outputSchema,omitempty"`
	Context      *agentContext   `json:"context,omitempty"`
	TimeoutMs    *int64          `json:"timeout_ms,omitempty"`
}

type agentContext struct {
	TargetNamespaces []string                           `json:"targetNamespaces,omitempty"`
	PreviousAttempts []agentPreviousAttempt             `json:"previousAttempts,omitempty"`
	ApprovedOption   *agenticv1alpha1.RemediationOption `json:"approvedOption,omitempty"`
	ExecutionResult  *agentExecutionResult              `json:"executionResult,omitempty"`
}

type agentExecutionResult struct {
	Success      bool                                   `json:"success"`
	ActionsTaken []agenticv1alpha1.ExecutionAction      `json:"actionsTaken"`
	Verification *agenticv1alpha1.ExecutionVerification `json:"verification,omitempty"`
}

func executionOutputToAgentResult(exec *ExecutionOutput) *agentExecutionResult {
	if exec == nil {
		return nil
	}
	r := &agentExecutionResult{
		Success:      exec.Success,
		ActionsTaken: exec.ActionsTaken,
	}
	if exec.Verification.Summary != "" || exec.Verification.ConditionOutcome != "" {
		r.Verification = &exec.Verification
	}
	return r
}

type agentPreviousAttempt struct {
	Attempt       int32  `json:"attempt"`
	FailureReason string `json:"failureReason,omitempty"`
}

type agentRunResponse struct {
	Response json.RawMessage
}

// AgentHTTPClientInterface abstracts HTTP calls to the agent service for testability.
type AgentHTTPClientInterface interface {
	Run(ctx context.Context, systemPrompt, query string, outputSchema json.RawMessage, agentCtx *agentContext) (*agentRunResponse, error)
}

// AgentHTTPClient communicates with the agentic-sandbox REST API.
type AgentHTTPClient struct {
	httpClient *http.Client
	endpoint   string
}

func NewAgentHTTPClient(endpoint string, timeout time.Duration) AgentHTTPClientInterface {
	if timeout <= 0 {
		timeout = defaultSandboxTimeout
	}
	return &AgentHTTPClient{
		httpClient: &http.Client{
			Timeout: timeout,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // internal cluster traffic
			},
		},
		endpoint: endpoint,
	}
}

func (c *AgentHTTPClient) Run(ctx context.Context, systemPrompt, query string, outputSchema json.RawMessage, agentCtx *agentContext) (*agentRunResponse, error) {
	timeoutMs := int64(c.httpClient.Timeout / time.Millisecond)
	req := agentRunRequest{
		Query:        query,
		SystemPrompt: systemPrompt,
		OutputSchema: outputSchema,
		Context:      agentCtx,
		TimeoutMs:    &timeoutMs,
	}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint+runPath, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("failed to create HTTP request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("POST %s failed: %w", runPath, err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseSize))
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		truncated := string(respBody)
		if len(truncated) > maxErrorBodyLen {
			truncated = truncated[:maxErrorBodyLen]
		}
		return nil, fmt.Errorf("POST %s returned HTTP %d: %s", runPath, resp.StatusCode, truncated)
	}

	return &agentRunResponse{Response: respBody}, nil
}
