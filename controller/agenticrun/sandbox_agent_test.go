package agenticrun

import (
	"context"
	"fmt"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
)

// --- Hand-written mocks ---

type mockSandboxProvider struct {
	claimName    string
	claimErr     error
	claimErrors  []error // per-call errors; takes precedence over claimErr when non-empty
	releaseErr   error
	claimCalls   int
	releaseCalls int
}

func (m *mockSandboxProvider) Create(_ context.Context, _ *agenticv1alpha1.AgenticRun, _ string, _ *agenticv1alpha1.Agent, _ *agenticv1alpha1.LLMProvider, _ *agenticv1alpha1.ToolsSpec, _ time.Duration, _ *agentContext) (string, error) {
	m.claimCalls++
	if len(m.claimErrors) > 0 {
		idx := m.claimCalls - 1
		if idx >= len(m.claimErrors) {
			idx = len(m.claimErrors) - 1
		}
		if err := m.claimErrors[idx]; err != nil {
			return "", err
		}
		return m.claimName, nil
	}
	return m.claimName, m.claimErr
}
func (m *mockSandboxProvider) Release(_ context.Context, _ *agenticv1alpha1.AgenticRun, _ string) error {
	m.releaseCalls++
	return m.releaseErr
}

func newTestSandboxAgentCaller(sandbox *mockSandboxProvider) *SandboxAgentCaller {
	run := testSandboxAgenticRun()
	return newTestSandboxAgentCallerWithAgenticRun(sandbox, run)
}

func newTestSandboxAgentCallerWithAgenticRun(sandbox *mockSandboxProvider, run *agenticv1alpha1.AgenticRun) *SandboxAgentCaller {
	fc := fake.NewClientBuilder().WithScheme(testScheme()).
		WithObjects(run).
		WithStatusSubresource(run, &agenticv1alpha1.AnalysisResult{}, &agenticv1alpha1.ExecutionResult{}, &agenticv1alpha1.VerificationResult{}, &agenticv1alpha1.EscalationResult{}).
		Build()
	_ = fc.Create(context.Background(), fakeBaseTemplate())
	return &SandboxAgentCaller{
		Sandbox:   sandbox,
		K8sClient: fc,
		Namespace: "test-ns",
	}
}

func testSandboxAgenticRun() *agenticv1alpha1.AgenticRun {
	return testAgenticRun()
}

func testSandboxStep() resolvedStep {
	tools := testTools()
	return resolvedStep{
		Agent: testDefaultAgent(),
		LLM:   testLLM("smart"),
		Tools: &tools,
	}
}

// --- Launch tests ---

func TestSandboxAgentCaller_Analyze_CreatesSandbox(t *testing.T) {
	sandbox := &mockSandboxProvider{claimName: "ls-analysis-fix-crash"}
	caller := newTestSandboxAgentCaller(sandbox)

	err := caller.Analyze(context.Background(), testSandboxAgenticRun(), testSandboxStep())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sandbox.claimCalls != 1 {
		t.Errorf("expected 1 Create call, got %d", sandbox.claimCalls)
	}
}

func TestSandboxAgentCaller_Execute_CreatesSandbox(t *testing.T) {
	sandbox := &mockSandboxProvider{claimName: "ls-execution-fix-crash"}
	caller := newTestSandboxAgentCaller(sandbox)

	option := &agenticv1alpha1.RemediationOption{
		Title: "Scale up",
		RemediationPlan: agenticv1alpha1.RemediationPlan{
			Description: "Increase replicas",
			Actions:     []agenticv1alpha1.ProposedAction{{Command: "kubectl scale deploy/app --replicas=3", Type: "command", Description: "scale"}},
		},
	}
	err := caller.Execute(context.Background(), testSandboxAgenticRun(), testSandboxStep(), option)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sandbox.claimCalls != 1 {
		t.Errorf("expected 1 Create call, got %d", sandbox.claimCalls)
	}
}

func TestSandboxAgentCaller_Verify_CreatesSandbox(t *testing.T) {
	sandbox := &mockSandboxProvider{claimName: "ls-verification-fix-crash"}
	caller := newTestSandboxAgentCaller(sandbox)

	err := caller.Verify(context.Background(), testSandboxAgenticRun(), testSandboxStep(), nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sandbox.claimCalls != 1 {
		t.Errorf("expected 1 Create call, got %d", sandbox.claimCalls)
	}
}

func TestSandboxAgentCaller_Escalate_CreatesSandbox(t *testing.T) {
	sandbox := &mockSandboxProvider{claimName: "ls-escalation-fix-crash"}
	caller := newTestSandboxAgentCaller(sandbox)

	err := caller.Escalate(context.Background(), testSandboxAgenticRun(), testSandboxStep())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sandbox.claimCalls != 1 {
		t.Errorf("expected 1 Create call, got %d", sandbox.claimCalls)
	}
}

func TestSandboxAgentCaller_Analyze_CreateError(t *testing.T) {
	sandbox := &mockSandboxProvider{claimErr: fmt.Errorf("sandbox unavailable")}
	caller := newTestSandboxAgentCaller(sandbox)

	err := caller.Analyze(context.Background(), testSandboxAgenticRun(), testSandboxStep())
	if err == nil {
		t.Fatal("expected error on sandbox create failure")
	}
}

func TestSandboxAgentCaller_Execute_CreateError(t *testing.T) {
	sandbox := &mockSandboxProvider{claimErr: fmt.Errorf("sandbox unavailable")}
	caller := newTestSandboxAgentCaller(sandbox)

	err := caller.Execute(context.Background(), testSandboxAgenticRun(), testSandboxStep(), nil)
	if err == nil {
		t.Fatal("expected error on sandbox create failure")
	}
}

func TestSandboxAgentCaller_PatchesSandboxInfo(t *testing.T) {
	sandbox := &mockSandboxProvider{claimName: "ls-analysis-fix-crash"}
	run := testSandboxAgenticRun()
	caller := newTestSandboxAgentCallerWithAgenticRun(sandbox, run)

	err := caller.Analyze(context.Background(), run, testSandboxStep())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var updated agenticv1alpha1.AgenticRun
	if err := caller.K8sClient.Get(context.Background(), client.ObjectKeyFromObject(run), &updated); err != nil {
		t.Fatalf("get run: %v", err)
	}
	if updated.Status.Steps.Analysis.Sandbox.ClaimName != "ls-analysis-fix-crash" {
		t.Errorf("expected sandbox claim name 'ls-analysis-fix-crash', got %q", updated.Status.Steps.Analysis.Sandbox.ClaimName)
	}
}

func TestSandboxAgentCaller_ReleaseSandbox(t *testing.T) {
	sandbox := &mockSandboxProvider{}
	caller := newTestSandboxAgentCaller(sandbox)

	run := testSandboxAgenticRun()
	run.Status.Steps.Analysis.Sandbox.ClaimName = "test-claim"
	if err := caller.ReleaseSandbox(context.Background(), run, "analysis"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sandbox.releaseCalls != 1 {
		t.Errorf("expected 1 Release call, got %d", sandbox.releaseCalls)
	}
}

func TestSandboxAgentCaller_ReleaseSandboxes(t *testing.T) {
	run := testSandboxAgenticRun()
	run.Status.Steps = agenticv1alpha1.StepsStatus{
		Analysis:     agenticv1alpha1.AnalysisStepStatus{Sandbox: agenticv1alpha1.SandboxInfo{ClaimName: "ls-analysis-test"}},
		Execution:    agenticv1alpha1.ExecutionStepStatus{Sandbox: agenticv1alpha1.SandboxInfo{ClaimName: "ls-execution-test"}},
		Verification: agenticv1alpha1.VerificationStepStatus{Sandbox: agenticv1alpha1.SandboxInfo{ClaimName: "ls-verification-test"}},
	}
	sandbox := &mockSandboxProvider{}
	caller := newTestSandboxAgentCaller(sandbox)

	if err := caller.ReleaseSandboxes(context.Background(), run); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sandbox.releaseCalls != 3 {
		t.Errorf("expected 3 Release calls, got %d", sandbox.releaseCalls)
	}
}

func TestSandboxAgentCaller_ReleaseSandboxes_PartialError(t *testing.T) {
	run := testSandboxAgenticRun()
	run.Status.Steps = agenticv1alpha1.StepsStatus{
		Analysis:     agenticv1alpha1.AnalysisStepStatus{Sandbox: agenticv1alpha1.SandboxInfo{ClaimName: "ls-analysis-test"}},
		Execution:    agenticv1alpha1.ExecutionStepStatus{Sandbox: agenticv1alpha1.SandboxInfo{ClaimName: "ls-execution-test"}},
		Verification: agenticv1alpha1.VerificationStepStatus{Sandbox: agenticv1alpha1.SandboxInfo{ClaimName: "ls-verification-test"}},
	}

	released := []string{}
	tracker := &trackingMockSandbox{released: &released, errOnClaim: "ls-execution-test"}
	fc := fake.NewClientBuilder().WithScheme(testScheme()).Build()
	_ = fc.Create(context.Background(), fakeBaseTemplate())
	caller := &SandboxAgentCaller{Sandbox: tracker, K8sClient: fc, Namespace: "test-ns"}

	if err := caller.ReleaseSandboxes(context.Background(), run); err == nil {
		t.Fatal("expected error when one release fails")
	}
	if len(*tracker.released) != 3 {
		t.Fatalf("expected 3 release attempts, got %d", len(*tracker.released))
	}
}

func TestBuildAgentContext_TargetNamespaces(t *testing.T) {
	run := testSandboxAgenticRun()
	run.Spec.TargetNamespaces = []string{"payments", "frontend"}
	ctx := buildAgentContext(run)
	if len(ctx.TargetNamespaces) != 2 || ctx.TargetNamespaces[0] != "payments" {
		t.Errorf("expected target namespaces [payments frontend], got %v", ctx.TargetNamespaces)
	}
}

func TestBuildAgentContext_PreviousAttempts(t *testing.T) {
	run := testSandboxAgenticRun()
	run.Status.Steps.Execution.Results = []agenticv1alpha1.StepResultRef{
		{Name: "exec-1", Outcome: agenticv1alpha1.ActionOutcomeFailed},
	}
	run.Status.Conditions = []metav1.Condition{
		{Type: agenticv1alpha1.AgenticRunConditionVerified, Status: metav1.ConditionFalse, Message: "check failed"},
	}
	ctx := buildAgentContext(run)
	if len(ctx.PreviousAttempts) != 1 {
		t.Errorf("expected 1 previous attempt, got %d", len(ctx.PreviousAttempts))
	}
}

func TestStepTimeout_Values(t *testing.T) {
	if got := stepTimeout("analysis"); got != analysisStepTimeout {
		t.Errorf("analysis timeout = %v, want %v", got, analysisStepTimeout)
	}
	if got := stepTimeout("execution"); got != executionStepTimeout {
		t.Errorf("execution timeout = %v, want %v", got, executionStepTimeout)
	}
	if got := stepTimeout("verification"); got != verificationStepTimeout {
		t.Errorf("verification timeout = %v, want %v", got, verificationStepTimeout)
	}
}

type trackingMockSandbox struct {
	released   *[]string
	errOnClaim string
}

func (m *trackingMockSandbox) Create(_ context.Context, _ *agenticv1alpha1.AgenticRun, _ string, _ *agenticv1alpha1.Agent, _ *agenticv1alpha1.LLMProvider, _ *agenticv1alpha1.ToolsSpec, _ time.Duration, _ *agentContext) (string, error) {
	return "", nil
}
func (m *trackingMockSandbox) Release(_ context.Context, run *agenticv1alpha1.AgenticRun, step string) error {
	claimName := sandboxClaimName(run, step)
	*m.released = append(*m.released, claimName)
	if m.errOnClaim != "" && claimName == m.errOnClaim {
		return fmt.Errorf("simulated release error for %s", claimName)
	}
	return nil
}

// --- Retry integration tests ---

func TestSandboxAgentCaller_TransientRetryThenSuccess(t *testing.T) {
	withFastRetry(t)
	gr := schema.GroupResource{Group: "", Resource: "pods"}
	sandbox := &mockSandboxProvider{
		claimName: "ls-analysis-fix-crash",
		claimErrors: []error{
			apierrors.NewServerTimeout(gr, "create", 0),
			nil,
		},
	}
	run := testSandboxAgenticRun()
	caller := newTestSandboxAgentCallerWithAgenticRun(sandbox, run)

	err := caller.Analyze(context.Background(), run, testSandboxStep())
	if err != nil {
		t.Fatalf("expected success after transient retry, got: %v", err)
	}
	if sandbox.claimCalls != 2 {
		t.Errorf("expected 2 Create calls (1 transient + 1 success), got %d", sandbox.claimCalls)
	}
}

func TestSandboxAgentCaller_PermanentErrorNoRetry(t *testing.T) {
	withFastRetry(t)
	sandbox := &mockSandboxProvider{
		claimErr: apierrors.NewForbidden(schema.GroupResource{}, "x", fmt.Errorf("escalation")),
	}
	caller := newTestSandboxAgentCaller(sandbox)

	err := caller.Analyze(context.Background(), testSandboxAgenticRun(), testSandboxStep())
	if err == nil {
		t.Fatal("expected error for permanent failure")
	}
	if sandbox.claimCalls != 1 {
		t.Errorf("permanent error should not retry, got %d calls", sandbox.claimCalls)
	}
}

func TestSandboxAgentCaller_TransientExhaustsRetries(t *testing.T) {
	withFastRetry(t)
	gr := schema.GroupResource{Group: "", Resource: "pods"}
	sandbox := &mockSandboxProvider{
		claimErr: apierrors.NewServerTimeout(gr, "create", 0),
	}
	caller := newTestSandboxAgentCaller(sandbox)

	err := caller.Analyze(context.Background(), testSandboxAgenticRun(), testSandboxStep())
	if err == nil {
		t.Fatal("expected error after exhausting retries")
	}
	if sandbox.claimCalls != maxCreateRetries {
		t.Errorf("expected %d Create calls, got %d", maxCreateRetries, sandbox.claimCalls)
	}
}
