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

	ErrMarshalRequest    = "failed to marshal request"
	ErrCreateHTTPRequest = "failed to create HTTP request"
	ErrPost              = "POST"
	ErrReadResponseBody  = "failed to read response body"
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

// RunMetrics contains sandbox-owned telemetry from the agent response envelope.
type RunMetrics struct {
	LatencyMs      int64   `json:"latency_ms"`
	InputTokens    int64   `json:"input_tokens"`
	OutputTokens   int64   `json:"output_tokens"`
	CostUSD        *string `json:"cost_usd,omitempty"`
	Model          string  `json:"model"`
	Provider       string  `json:"provider"`
	ToolCallsCount int     `json:"tool_calls_count"`
}

// agentRunResponse is the envelope returned by POST /v1/agent/run.
type agentRunResponse struct {
	Metrics *RunMetrics     `json:"metrics"`
	Result  json.RawMessage `json:"result"`
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

func NewAgentHTTPClient(endpoint string) AgentHTTPClientInterface {
	return &AgentHTTPClient{
		httpClient: &http.Client{
			Timeout: 5 * time.Minute,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // internal cluster traffic
			},
		},
		endpoint: endpoint,
	}
}

func (c *AgentHTTPClient) Run(ctx context.Context, systemPrompt, query string, outputSchema json.RawMessage, agentCtx *agentContext) (*agentRunResponse, error) {
	req := agentRunRequest{
		Query:        query,
		SystemPrompt: systemPrompt,
		OutputSchema: outputSchema,
		Context:      agentCtx,
	}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", ErrMarshalRequest, err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint+runPath, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", ErrCreateHTTPRequest, err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("%s %s failed: %w", ErrPost, runPath, err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseSize))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", ErrReadResponseBody, err)
	}

	if resp.StatusCode != http.StatusOK {
		truncated := string(respBody)
		if len(truncated) > maxErrorBodyLen {
			truncated = truncated[:maxErrorBodyLen]
		}
		return nil, fmt.Errorf("POST %s returned HTTP %d: %s", runPath, resp.StatusCode, truncated)
	}

	var response agentRunResponse
	if err := json.Unmarshal(respBody, &response); err != nil {
		return nil, fmt.Errorf("parse response envelope: %w", err)
	}
	if len(response.Result) == 0 || string(response.Result) == "null" {
		return nil, fmt.Errorf("response envelope missing or null 'result' field")
	}

	return &response, nil
}
