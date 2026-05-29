package proposal

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
)

// --- Hand-written mocks ---

type mockSandboxProvider struct {
	claimName    string
	claimErr     error
	endpoint     string
	readyErr     error
	releaseErr   error
	claimCalls   int
	releaseCalls int
}

func (m *mockSandboxProvider) Claim(_ context.Context, _, _, _ string) (string, error) {
	m.claimCalls++
	return m.claimName, m.claimErr
}
func (m *mockSandboxProvider) WaitReady(_ context.Context, _ string, _ time.Duration) (string, error) {
	return m.endpoint, m.readyErr
}
func (m *mockSandboxProvider) Release(_ context.Context, _ string) error {
	m.releaseCalls++
	return m.releaseErr
}

type mockHTTPClient struct {
	response   *agentRunResponse
	err        error
	lastQuery  string
	lastPrompt string
	lastCtx    *agentContext
}

func (m *mockHTTPClient) Run(_ context.Context, systemPrompt, query string, _ json.RawMessage, agentCtx *agentContext) (*agentRunResponse, error) {
	m.lastQuery = query
	m.lastPrompt = systemPrompt
	m.lastCtx = agentCtx
	return m.response, m.err
}

func newTestSandboxAgentCaller(sandbox *mockSandboxProvider, httpClient *mockHTTPClient) *SandboxAgentCaller {
	fc := fake.NewClientBuilder().WithScheme(testScheme()).Build()
	_ = fc.Create(context.Background(), fakeBaseTemplate())
	return &SandboxAgentCaller{
		Sandbox:          sandbox,
		K8sClient:        fc,
		ClientFactory:    func(_ string, _ time.Duration) AgentHTTPClientInterface { return httpClient },
		Namespace:        "test-ns",
		Timeout:          5 * time.Minute,
	}
}

func newTestSandboxAgentCallerWithProposal(sandbox *mockSandboxProvider, httpClient *mockHTTPClient, proposal *agenticv1alpha1.Proposal) *SandboxAgentCaller {
	fc := fake.NewClientBuilder().WithScheme(testScheme()).
		WithObjects(proposal).
		WithStatusSubresource(proposal, &agenticv1alpha1.AnalysisResult{}, &agenticv1alpha1.ExecutionResult{}, &agenticv1alpha1.VerificationResult{}, &agenticv1alpha1.EscalationResult{}).
		Build()
	_ = fc.Create(context.Background(), fakeBaseTemplate())
	return &SandboxAgentCaller{
		Sandbox:          sandbox,
		K8sClient:        fc,
		ClientFactory:    func(_ string, _ time.Duration) AgentHTTPClientInterface { return httpClient },
		Namespace:        "test-ns",
		Timeout:          5 * time.Minute,
	}
}

func testSandboxProposal() *agenticv1alpha1.Proposal {
	return testProposal()
}

func testSandboxStep() resolvedStep {
	tools := testTools()
	return resolvedStep{
		Agent: testDefaultAgent(),
		LLM:   testLLM("smart"),
		Tools: &tools,
	}
}

// --- Happy path tests ---

func TestSandboxAgentCaller_Analyze_HappyPath(t *testing.T) {
	sandbox := &mockSandboxProvider{claimName: "ls-analysis-fix-crash", endpoint: "http://sandbox:8080"}
	httpClient := &mockHTTPClient{
		response: &agentRunResponse{
			Response: json.RawMessage(`{"success": true, "options": [{"title": "Increase memory", "diagnosis": {"summary": "OOM", "confidence": "High", "rootCause": "memory limit"}, "proposal": {"description": "Bump memory", "actions": [{"type": "patch", "description": "patch deploy"}], "risk": "Low"}}]}`),
		},
	}

	caller := newTestSandboxAgentCaller(sandbox, httpClient)
	result, err := caller.Analyze(context.Background(), testSandboxProposal(), testSandboxStep(), "Pod crashing", defaultSandboxTimeout)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.Options) != 1 {
		t.Fatalf("expected 1 option, got %d", len(result.Options))
	}
	if result.Options[0].Title != "Increase memory" {
		t.Errorf("title = %q", result.Options[0].Title)
	}
	if result.Options[0].Diagnosis.Confidence != "High" {
		t.Errorf("confidence = %q", result.Options[0].Diagnosis.Confidence)
	}
}

func TestSandboxAgentCaller_Execute_HappyPath(t *testing.T) {
	sandbox := &mockSandboxProvider{claimName: "ls-execution-fix-crash", endpoint: "http://sandbox:8080"}
	httpClient := &mockHTTPClient{
		response: &agentRunResponse{
			Response: json.RawMessage(`{"success": true, "actionsTaken": [{"type": "patch", "description": "Patched deployment", "outcome": "Succeeded"}], "verification": {"conditionOutcome": "Improved", "summary": "Pod running"}}`),
		},
	}

	caller := newTestSandboxAgentCaller(sandbox, httpClient)
	option := &agenticv1alpha1.RemediationOption{Title: "Fix it"}
	result, err := caller.Execute(context.Background(), testSandboxProposal(), testSandboxStep(), option, defaultSandboxTimeout)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.ActionsTaken) != 1 {
		t.Fatalf("expected 1 action, got %d", len(result.ActionsTaken))
	}
	if result.ActionsTaken[0].Outcome != agenticv1alpha1.ActionOutcomeSucceeded {
		t.Errorf("outcome = %q", result.ActionsTaken[0].Outcome)
	}
	if result.Verification.ConditionOutcome != agenticv1alpha1.ConditionOutcomeImproved {
		t.Errorf("conditionOutcome = %q", result.Verification.ConditionOutcome)
	}
}

func TestSandboxAgentCaller_Verify_HappyPath(t *testing.T) {
	sandbox := &mockSandboxProvider{claimName: "ls-verification-fix-crash", endpoint: "http://sandbox:8080"}
	httpClient := &mockHTTPClient{
		response: &agentRunResponse{
			Response: json.RawMessage(`{"success": true, "checks": [{"name": "pod-running", "source": "oc", "value": "Running", "result": "Passed"}], "summary": "All checks passed"}`),
		},
	}

	caller := newTestSandboxAgentCaller(sandbox, httpClient)
	result, err := caller.Verify(context.Background(), testSandboxProposal(), testSandboxStep(), nil, nil, defaultSandboxTimeout)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.Checks) != 1 {
		t.Fatalf("expected 1 check, got %d", len(result.Checks))
	}
	if result.Checks[0].Result != agenticv1alpha1.CheckResultPassed {
		t.Errorf("result = %q", result.Checks[0].Result)
	}
	if result.Summary != "All checks passed" {
		t.Errorf("summary = %q", result.Summary)
	}
}

// --- Error handling tests ---

func TestSandboxAgentCaller_ClaimError(t *testing.T) {
	sandbox := &mockSandboxProvider{claimErr: fmt.Errorf("quota exceeded")}
	httpClient := &mockHTTPClient{}

	caller := newTestSandboxAgentCaller(sandbox, httpClient)
	_, err := caller.Analyze(context.Background(), testSandboxProposal(), testSandboxStep(), "test", defaultSandboxTimeout)
	if err == nil {
		t.Fatal("expected error")
	}
	if httpClient.lastQuery != "" {
		t.Error("HTTP client should not have been called")
	}
	if sandbox.releaseCalls != 0 {
		t.Errorf("Release should not be called on Claim failure, got %d calls", sandbox.releaseCalls)
	}
}

func TestSandboxAgentCaller_WaitReadyError(t *testing.T) {
	sandbox := &mockSandboxProvider{claimName: "claim-1", readyErr: fmt.Errorf("timeout")}
	httpClient := &mockHTTPClient{}

	caller := newTestSandboxAgentCaller(sandbox, httpClient)
	_, err := caller.Execute(context.Background(), testSandboxProposal(), testSandboxStep(), nil, defaultSandboxTimeout)
	if err == nil {
		t.Fatal("expected error")
	}
	if sandbox.releaseCalls != 0 {
		t.Errorf("Release calls = %d, want 0 (reconciler handles release)", sandbox.releaseCalls)
	}
}

func TestSandboxAgentCaller_HTTPError(t *testing.T) {
	sandbox := &mockSandboxProvider{claimName: "claim-1", endpoint: "http://sandbox:8080"}
	httpClient := &mockHTTPClient{err: fmt.Errorf("connection refused")}

	caller := newTestSandboxAgentCaller(sandbox, httpClient)
	_, err := caller.Verify(context.Background(), testSandboxProposal(), testSandboxStep(), nil, nil, defaultSandboxTimeout)
	if err == nil {
		t.Fatal("expected error")
	}
	if sandbox.releaseCalls != 0 {
		t.Errorf("Release calls = %d, want 0 (reconciler handles release)", sandbox.releaseCalls)
	}
}

func TestSandboxAgentCaller_ParseError(t *testing.T) {
	sandbox := &mockSandboxProvider{claimName: "claim-1", endpoint: "http://sandbox:8080"}
	httpClient := &mockHTTPClient{
		response: &agentRunResponse{Response: json.RawMessage("not valid json")},
	}

	caller := newTestSandboxAgentCaller(sandbox, httpClient)
	_, err := caller.Analyze(context.Background(), testSandboxProposal(), testSandboxStep(), "test", defaultSandboxTimeout)
	if err == nil {
		t.Fatal("expected parse error")
	}
	if sandbox.releaseCalls != 0 {
		t.Errorf("Release calls = %d, want 0 (reconciler handles release)", sandbox.releaseCalls)
	}
}

func TestSandboxAgentCaller_SandboxNotReleasedAfterCall(t *testing.T) {
	sandbox := &mockSandboxProvider{claimName: "claim-1", endpoint: "http://sandbox:8080"}
	httpClient := &mockHTTPClient{
		response: &agentRunResponse{Response: json.RawMessage(`{"success": true, "options": []}`)},
	}

	caller := newTestSandboxAgentCaller(sandbox, httpClient)
	_, _ = caller.Analyze(context.Background(), testSandboxProposal(), testSandboxStep(), "test", defaultSandboxTimeout)

	if sandbox.claimCalls != 1 {
		t.Errorf("Claim calls = %d, want 1", sandbox.claimCalls)
	}
	if sandbox.releaseCalls != 0 {
		t.Errorf("Release calls = %d, want 0 (reconciler handles release at terminal phase)", sandbox.releaseCalls)
	}
}

// --- Context propagation tests ---

func TestSandboxAgentCaller_ContextPropagation(t *testing.T) {
	sandbox := &mockSandboxProvider{claimName: "claim-1", endpoint: "http://sandbox:8080"}
	httpClient := &mockHTTPClient{
		response: &agentRunResponse{Response: json.RawMessage(`{"success": true, "options": []}`)},
	}

	caller := newTestSandboxAgentCaller(sandbox, httpClient)

	proposal := &agenticv1alpha1.Proposal{
		ObjectMeta: metav1.ObjectMeta{Name: "fix-crash", Namespace: "default"},
		Spec: agenticv1alpha1.ProposalSpec{
			Request:          "Pod crashing",
			Tools:            testTools(),
			TargetNamespaces: []string{"production", "staging"},
			Analysis:         agenticv1alpha1.ProposalStep{Agent: "default"},
			Execution:        agenticv1alpha1.ProposalStep{Agent: "default"},
			Verification:     agenticv1alpha1.ProposalStep{Agent: "default"},
		},
		Status: agenticv1alpha1.ProposalStatus{
			Steps: agenticv1alpha1.StepsStatus{
				Execution: agenticv1alpha1.ExecutionStepStatus{
					Results: []agenticv1alpha1.StepResultRef{
						{Name: "fix-crash-execution-1", Outcome: agenticv1alpha1.ActionOutcomeFailed},
					},
				},
			},
		},
	}

	_, _ = caller.Analyze(context.Background(), proposal, testSandboxStep(), "test", defaultSandboxTimeout)

	if httpClient.lastCtx == nil {
		t.Fatal("expected context to be set")
	}
	if len(httpClient.lastCtx.TargetNamespaces) != 2 {
		t.Errorf("targetNamespaces count = %d, want 2", len(httpClient.lastCtx.TargetNamespaces))
	}
	if len(httpClient.lastCtx.PreviousAttempts) != 1 {
		t.Fatalf("previousAttempts count = %d, want 1", len(httpClient.lastCtx.PreviousAttempts))
	}
	if httpClient.lastCtx.PreviousAttempts[0].FailureReason != "execution attempt 1 failed" {
		t.Errorf("failureReason = %q", httpClient.lastCtx.PreviousAttempts[0].FailureReason)
	}
}

func TestSandboxAgentCaller_VerifyPassesExecutionResult(t *testing.T) {
	sandbox := &mockSandboxProvider{claimName: "claim-1", endpoint: "http://sandbox:8080"}
	httpClient := &mockHTTPClient{
		response: &agentRunResponse{Response: json.RawMessage(`{"success": true, "checks": [], "summary": "ok"}`)},
	}

	caller := newTestSandboxAgentCaller(sandbox, httpClient)
	option := &agenticv1alpha1.RemediationOption{Title: "Scale up replicas"}
	exec := &ExecutionOutput{
		Success: true,
		ActionsTaken: []agenticv1alpha1.ExecutionAction{
			{Type: "patch", Description: "Patched deployment", Outcome: agenticv1alpha1.ActionOutcomeSucceeded, Output: "deployment.apps/nginx patched"},
			{Type: "scale", Description: "Scaled to 3 replicas", Outcome: agenticv1alpha1.ActionOutcomeSucceeded},
		},
		Verification: agenticv1alpha1.ExecutionVerification{
			ConditionOutcome: agenticv1alpha1.ConditionOutcomeImproved,
			Summary:          "Pod running after patch",
		},
	}

	_, _ = caller.Verify(context.Background(), testSandboxProposal(), testSandboxStep(), option, exec, defaultSandboxTimeout)

	if httpClient.lastCtx == nil {
		t.Fatal("expected context to be set")
	}
	if httpClient.lastCtx.ApprovedOption == nil || httpClient.lastCtx.ApprovedOption.Title != "Scale up replicas" {
		t.Errorf("approvedOption.title = %v", httpClient.lastCtx.ApprovedOption)
	}
	if httpClient.lastCtx.ExecutionResult == nil {
		t.Fatal("expected executionResult in context")
	}
	if !httpClient.lastCtx.ExecutionResult.Success {
		t.Error("executionResult.success should be true")
	}
	if len(httpClient.lastCtx.ExecutionResult.ActionsTaken) != 2 {
		t.Errorf("executionResult.actionsTaken count = %d, want 2", len(httpClient.lastCtx.ExecutionResult.ActionsTaken))
	}
	if httpClient.lastCtx.ExecutionResult.ActionsTaken[0].Description != "Patched deployment" {
		t.Errorf("actionsTaken[0].description = %q", httpClient.lastCtx.ExecutionResult.ActionsTaken[0].Description)
	}
	if httpClient.lastCtx.ExecutionResult.Verification == nil {
		t.Fatal("executionResult.verification should be set")
	}
	if httpClient.lastCtx.ExecutionResult.Verification.ConditionOutcome != agenticv1alpha1.ConditionOutcomeImproved {
		t.Errorf("verification.conditionOutcome = %q", httpClient.lastCtx.ExecutionResult.Verification.ConditionOutcome)
	}
}

func TestSandboxAgentCaller_VerifyNilExecLeavesExecutionResultNil(t *testing.T) {
	sandbox := &mockSandboxProvider{claimName: "claim-1", endpoint: "http://sandbox:8080"}
	httpClient := &mockHTTPClient{
		response: &agentRunResponse{Response: json.RawMessage(`{"success": true, "checks": [], "summary": "ok"}`)},
	}

	caller := newTestSandboxAgentCaller(sandbox, httpClient)
	_, _ = caller.Verify(context.Background(), testSandboxProposal(), testSandboxStep(), nil, nil, defaultSandboxTimeout)

	if httpClient.lastCtx == nil {
		t.Fatal("expected context to be set")
	}
	if httpClient.lastCtx.ExecutionResult != nil {
		t.Errorf("executionResult should be nil when exec is nil, got %+v", httpClient.lastCtx.ExecutionResult)
	}
}

func TestSandboxAgentCaller_VerifyExecWithoutInlineVerification(t *testing.T) {
	sandbox := &mockSandboxProvider{claimName: "claim-1", endpoint: "http://sandbox:8080"}
	httpClient := &mockHTTPClient{
		response: &agentRunResponse{Response: json.RawMessage(`{"success": true, "checks": [], "summary": "ok"}`)},
	}

	caller := newTestSandboxAgentCaller(sandbox, httpClient)
	exec := &ExecutionOutput{
		Success: true,
		ActionsTaken: []agenticv1alpha1.ExecutionAction{
			{Type: "apply", Description: "Applied manifest", Outcome: agenticv1alpha1.ActionOutcomeSucceeded},
		},
	}

	_, _ = caller.Verify(context.Background(), testSandboxProposal(), testSandboxStep(), nil, exec, defaultSandboxTimeout)

	if httpClient.lastCtx.ExecutionResult == nil {
		t.Fatal("expected executionResult in context")
	}
	if httpClient.lastCtx.ExecutionResult.Verification != nil {
		t.Error("executionResult.verification should be nil when exec has zero-value verification")
	}
}

func TestSandboxAgentCaller_ExecutePassesApprovedOption(t *testing.T) {
	sandbox := &mockSandboxProvider{claimName: "claim-1", endpoint: "http://sandbox:8080"}
	httpClient := &mockHTTPClient{
		response: &agentRunResponse{Response: json.RawMessage(`{"success": true, "actionsTaken": []}`)},
	}

	caller := newTestSandboxAgentCaller(sandbox, httpClient)
	option := &agenticv1alpha1.RemediationOption{Title: "Scale up replicas"}
	_, _ = caller.Execute(context.Background(), testSandboxProposal(), testSandboxStep(), option, defaultSandboxTimeout)

	if httpClient.lastCtx == nil || httpClient.lastCtx.ApprovedOption == nil {
		t.Fatal("expected approved option in context")
	}
	if httpClient.lastCtx.ApprovedOption.Title != "Scale up replicas" {
		t.Errorf("approvedOption.title = %q", httpClient.lastCtx.ApprovedOption.Title)
	}
}

// --- Per-phase query construction tests ---

func TestSandboxAgentCaller_AnalysisQueryFraming(t *testing.T) {
	sandbox := &mockSandboxProvider{claimName: "claim-1", endpoint: "http://sandbox:8080"}
	httpClient := &mockHTTPClient{
		response: &agentRunResponse{Response: json.RawMessage(`{"success": true, "options": []}`)},
	}

	caller := newTestSandboxAgentCaller(sandbox, httpClient)
	_, _ = caller.Analyze(context.Background(), testSandboxProposal(), testSandboxStep(), "Pod crashing with OOMKilled", defaultSandboxTimeout)

	if !strings.Contains(httpClient.lastQuery, "analysis agent") {
		t.Error("analysis query should contain role framing")
	}
	if !strings.Contains(httpClient.lastQuery, "Pod crashing with OOMKilled") {
		t.Error("analysis query should contain the original request")
	}
	if !strings.Contains(httpClient.lastQuery, "Do NOT attempt to fix") {
		t.Error("analysis query should instruct agent not to execute")
	}
}

func TestSandboxAgentCaller_ExecutionQueryFraming(t *testing.T) {
	sandbox := &mockSandboxProvider{claimName: "claim-1", endpoint: "http://sandbox:8080"}
	httpClient := &mockHTTPClient{
		response: &agentRunResponse{Response: json.RawMessage(`{"success": true, "actionsTaken": []}`)},
	}

	caller := newTestSandboxAgentCaller(sandbox, httpClient)
	option := &agenticv1alpha1.RemediationOption{
		Title: "Increase memory limit",
		Proposal: agenticv1alpha1.ProposalResult{
			Description: "Patch deployment memory",
			Actions:     []agenticv1alpha1.ProposedAction{{Type: "patch", Description: "Set memory to 512Mi"}},
			Risk:        "Low",
		},
	}
	proposal := testSandboxProposal()
	proposal.Spec.Request = "Pod crashing with OOMKilled"
	_, _ = caller.Execute(context.Background(), proposal, testSandboxStep(), option, defaultSandboxTimeout)

	if !strings.Contains(httpClient.lastQuery, "execution agent") {
		t.Error("execution query should contain role framing")
	}
	if !strings.Contains(httpClient.lastQuery, "Increase memory limit") {
		t.Error("execution query should contain approved option title")
	}
	if strings.Contains(httpClient.lastQuery, "Pod crashing with OOMKilled") {
		t.Error("execution query should NOT contain the original request")
	}
}

func TestSandboxAgentCaller_VerificationQueryFraming(t *testing.T) {
	sandbox := &mockSandboxProvider{claimName: "claim-1", endpoint: "http://sandbox:8080"}
	httpClient := &mockHTTPClient{
		response: &agentRunResponse{Response: json.RawMessage(`{"success": true, "checks": [], "summary": "ok"}`)},
	}

	caller := newTestSandboxAgentCaller(sandbox, httpClient)
	option := &agenticv1alpha1.RemediationOption{Title: "Increase memory limit"}
	exec := &ExecutionOutput{
		Success: true,
		ActionsTaken: []agenticv1alpha1.ExecutionAction{
			{Type: "patch", Description: "Patched deployment memory", Outcome: agenticv1alpha1.ActionOutcomeSucceeded},
		},
	}
	proposal := testSandboxProposal()
	proposal.Spec.Request = "Pod crashing with OOMKilled"
	_, _ = caller.Verify(context.Background(), proposal, testSandboxStep(), option, exec, defaultSandboxTimeout)

	if !strings.Contains(httpClient.lastQuery, "verification agent") {
		t.Error("verification query should contain role framing")
	}
	if !strings.Contains(httpClient.lastQuery, "Increase memory limit") {
		t.Error("verification query should contain approved option")
	}
	if !strings.Contains(httpClient.lastQuery, "Patched deployment memory") {
		t.Error("verification query should contain execution results")
	}
	if strings.Contains(httpClient.lastQuery, "Pod crashing with OOMKilled") {
		t.Error("verification query should NOT contain the original request")
	}
}

func TestSandboxAgentCaller_ExecutionQueryNilOption(t *testing.T) {
	sandbox := &mockSandboxProvider{claimName: "claim-1", endpoint: "http://sandbox:8080"}
	httpClient := &mockHTTPClient{
		response: &agentRunResponse{Response: json.RawMessage(`{"success": true, "actionsTaken": []}`)},
	}

	caller := newTestSandboxAgentCaller(sandbox, httpClient)
	_, _ = caller.Execute(context.Background(), testSandboxProposal(), testSandboxStep(), nil, defaultSandboxTimeout)

	if !strings.Contains(httpClient.lastQuery, "execution agent") {
		t.Error("execution query should still contain role framing with nil option")
	}
	if !strings.Contains(httpClient.lastQuery, "{}") {
		t.Error("execution query should contain empty JSON object for nil option")
	}
}

// --- Sandbox info patching tests ---

func TestSandboxAgentCaller_Analyze_PatchesSandboxInfo(t *testing.T) {
	sandbox := &mockSandboxProvider{claimName: "ls-analysis-fix-crash", endpoint: "http://sandbox:8080"}
	httpClient := &mockHTTPClient{
		response: &agentRunResponse{
			Response: json.RawMessage(`{"success": true, "options": [{"title": "Fix it", "diagnosis": {"summary": "broken", "confidence": "High", "rootCause": "bug"}, "proposal": {"description": "fix", "actions": [{"type": "patch", "description": "patch"}], "risk": "Low"}}]}`),
		},
	}

	proposal := testSandboxProposal()
	caller := newTestSandboxAgentCallerWithProposal(sandbox, httpClient, proposal)

	_, err := caller.Analyze(context.Background(), proposal, testSandboxStep(), "Pod crashing", defaultSandboxTimeout)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var updated agenticv1alpha1.Proposal
	if err := caller.K8sClient.Get(context.Background(), client.ObjectKeyFromObject(proposal), &updated); err != nil {
		t.Fatalf("get proposal: %v", err)
	}

	if updated.Status.Steps.Analysis.Sandbox.ClaimName != "ls-analysis-fix-crash" {
		t.Errorf("sandbox claimName = %q, want %q", updated.Status.Steps.Analysis.Sandbox.ClaimName, "ls-analysis-fix-crash")
	}
	if updated.Status.Steps.Analysis.Sandbox.Namespace != "test-ns" {
		t.Errorf("sandbox namespace = %q, want %q", updated.Status.Steps.Analysis.Sandbox.Namespace, "test-ns")
	}
}

func TestSandboxAgentCaller_Execute_PatchesSandboxInfo(t *testing.T) {
	sandbox := &mockSandboxProvider{claimName: "ls-execution-fix-crash", endpoint: "http://sandbox:8080"}
	httpClient := &mockHTTPClient{
		response: &agentRunResponse{
			Response: json.RawMessage(`{"success": true, "actionsTaken": [{"type": "patch", "description": "patched deploy"}]}`),
		},
	}

	proposal := testSandboxProposal()
	caller := newTestSandboxAgentCallerWithProposal(sandbox, httpClient, proposal)

	_, err := caller.Execute(context.Background(), proposal, testSandboxStep(), nil, defaultSandboxTimeout)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var updated agenticv1alpha1.Proposal
	if err := caller.K8sClient.Get(context.Background(), client.ObjectKeyFromObject(proposal), &updated); err != nil {
		t.Fatalf("get proposal: %v", err)
	}

	if updated.Status.Steps.Execution.Sandbox.ClaimName != "ls-execution-fix-crash" {
		t.Errorf("sandbox claimName = %q, want %q", updated.Status.Steps.Execution.Sandbox.ClaimName, "ls-execution-fix-crash")
	}
}

func TestSandboxAgentCaller_Verify_PatchesSandboxInfo(t *testing.T) {
	sandbox := &mockSandboxProvider{claimName: "ls-verification-fix-crash", endpoint: "http://sandbox:8080"}
	httpClient := &mockHTTPClient{
		response: &agentRunResponse{
			Response: json.RawMessage(`{"success": true, "checks": [{"name": "pod-running", "source": "oc", "value": "Running", "result": "Passed"}], "summary": "All checks passed"}`),
		},
	}

	proposal := testSandboxProposal()
	caller := newTestSandboxAgentCallerWithProposal(sandbox, httpClient, proposal)

	_, err := caller.Verify(context.Background(), proposal, testSandboxStep(), nil, nil, defaultSandboxTimeout)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var updated agenticv1alpha1.Proposal
	if err := caller.K8sClient.Get(context.Background(), client.ObjectKeyFromObject(proposal), &updated); err != nil {
		t.Fatalf("get proposal: %v", err)
	}

	if updated.Status.Steps.Verification.Sandbox.ClaimName != "ls-verification-fix-crash" {
		t.Errorf("sandbox claimName = %q, want %q", updated.Status.Steps.Verification.Sandbox.ClaimName, "ls-verification-fix-crash")
	}
}

func TestSandboxAgentCaller_SandboxInfoPatch_DoesNotBlockOnError(t *testing.T) {
	sandbox := &mockSandboxProvider{claimName: "ls-analysis-fix-crash", endpoint: "http://sandbox:8080"}
	httpClient := &mockHTTPClient{
		response: &agentRunResponse{
			Response: json.RawMessage(`{"success": true, "options": []}`),
		},
	}

	// Use caller WITHOUT proposal in the fake client — patchSandboxInfo will fail to Get
	// but the analysis call should still succeed
	caller := newTestSandboxAgentCaller(sandbox, httpClient)
	proposal := testSandboxProposal()

	_, err := caller.Analyze(context.Background(), proposal, testSandboxStep(), "test", defaultSandboxTimeout)
	if err != nil {
		t.Fatalf("analysis should succeed even when sandbox info patch fails: %v", err)
	}
}

func TestReleaseSandboxes_ReleasesAllSteps(t *testing.T) {
	releasedClaims := []string{}
	tracker := &trackingMockSandbox{released: &releasedClaims}

	caller := &SandboxAgentCaller{
		Sandbox:   tracker,
		Namespace: "test-ns",
	}

	proposal := &agenticv1alpha1.Proposal{
		Status: agenticv1alpha1.ProposalStatus{
			Steps: agenticv1alpha1.StepsStatus{
				Analysis: agenticv1alpha1.AnalysisStepStatus{
					Sandbox: agenticv1alpha1.SandboxInfo{ClaimName: "claim-analysis"},
				},
				Execution: agenticv1alpha1.ExecutionStepStatus{
					Sandbox: agenticv1alpha1.SandboxInfo{ClaimName: "claim-execution"},
				},
				Verification: agenticv1alpha1.VerificationStepStatus{
					Sandbox: agenticv1alpha1.SandboxInfo{ClaimName: "claim-verification"},
				},
			},
		},
	}

	err := caller.ReleaseSandboxes(context.Background(), proposal)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(*tracker.released) != 3 {
		t.Fatalf("expected 3 releases, got %d", len(*tracker.released))
	}
	expected := []string{"claim-analysis", "claim-execution", "claim-verification"}
	for i, name := range expected {
		if (*tracker.released)[i] != name {
			t.Errorf("release[%d] = %q, want %q", i, (*tracker.released)[i], name)
		}
	}
}

func TestReleaseSandboxes_SkipsEmptyClaims(t *testing.T) {
	releasedClaims := []string{}
	tracker := &trackingMockSandbox{released: &releasedClaims}

	caller := &SandboxAgentCaller{Sandbox: tracker, Namespace: "test-ns"}

	proposal := &agenticv1alpha1.Proposal{
		Status: agenticv1alpha1.ProposalStatus{
			Steps: agenticv1alpha1.StepsStatus{
				Analysis: agenticv1alpha1.AnalysisStepStatus{
					Sandbox: agenticv1alpha1.SandboxInfo{ClaimName: "claim-analysis"},
				},
			},
		},
	}

	err := caller.ReleaseSandboxes(context.Background(), proposal)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(*tracker.released) != 1 {
		t.Fatalf("expected 1 release, got %d", len(*tracker.released))
	}
}

func TestReleaseSandboxes_ContinuesOnError(t *testing.T) {
	releasedClaims := []string{}
	tracker := &trackingMockSandbox{
		released:   &releasedClaims,
		errOnClaim: "claim-execution",
	}

	caller := &SandboxAgentCaller{Sandbox: tracker, Namespace: "test-ns"}

	proposal := &agenticv1alpha1.Proposal{
		Status: agenticv1alpha1.ProposalStatus{
			Steps: agenticv1alpha1.StepsStatus{
				Analysis: agenticv1alpha1.AnalysisStepStatus{
					Sandbox: agenticv1alpha1.SandboxInfo{ClaimName: "claim-analysis"},
				},
				Execution: agenticv1alpha1.ExecutionStepStatus{
					Sandbox: agenticv1alpha1.SandboxInfo{ClaimName: "claim-execution"},
				},
				Verification: agenticv1alpha1.VerificationStepStatus{
					Sandbox: agenticv1alpha1.SandboxInfo{ClaimName: "claim-verification"},
				},
			},
		},
	}

	err := caller.ReleaseSandboxes(context.Background(), proposal)
	if err == nil {
		t.Fatal("expected error from failing release")
	}
	// Should still attempt all three
	if len(*tracker.released) != 3 {
		t.Fatalf("expected 3 release attempts, got %d", len(*tracker.released))
	}
}

type trackingMockSandbox struct {
	released   *[]string
	errOnClaim string
}

func (m *trackingMockSandbox) Claim(_ context.Context, _, _, _ string) (string, error) {
	return "", nil
}
func (m *trackingMockSandbox) WaitReady(_ context.Context, _ string, _ time.Duration) (string, error) {
	return "", nil
}
func (m *trackingMockSandbox) Release(_ context.Context, claimName string) error {
	*m.released = append(*m.released, claimName)
	if m.errOnClaim != "" && claimName == m.errOnClaim {
		return fmt.Errorf("simulated release error for %s", claimName)
	}
	return nil
}
