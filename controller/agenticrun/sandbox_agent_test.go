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
	tools        *agenticv1alpha1.ToolsSpec
}

func (m *mockSandboxProvider) Create(_ context.Context, _ *agenticv1alpha1.AgenticRun, _ string, _ *agenticv1alpha1.Agent, _ *agenticv1alpha1.LLMProvider, tools *agenticv1alpha1.ToolsSpec, _ time.Duration, _ *agentContext) (string, error) {
	m.claimCalls++
	m.tools = tools
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
func (m *mockSandboxProvider) Release(_ context.Context, _ *agenticv1alpha1.AgenticRun, _ string, _ *SpokeAccess, _ bool) error {
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

func TestSandboxAgentCaller_Escalate_CreatesSandboxWithRunLevelTools(t *testing.T) {
	sandbox := &mockSandboxProvider{claimName: "ls-escalation-fix-crash"}
	run := testSandboxAgenticRun()
	caller := newTestSandboxAgentCallerWithAgenticRun(sandbox, run)
	step := testSandboxStep()
	step.Tools = &run.Spec.Tools

	err := caller.Escalate(context.Background(), run, step)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sandbox.claimCalls != 1 {
		t.Errorf("expected 1 Create call, got %d", sandbox.claimCalls)
	}
	if sandbox.tools != &run.Spec.Tools {
		t.Fatal("escalation sandbox must receive the run-level tools")
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

func TestSandboxAgentCaller_ReleaseUsesKubernetesDefaultPodGrace(t *testing.T) {
	tests := []struct {
		name        string
		claimNames  map[string]string
		releaseStep string
		bulk        bool
	}{
		{
			name: "per-step",
			claimNames: map[string]string{
				"analysis": "ls-analysis-caller-test",
			},
			releaseStep: "analysis",
		},
		{
			name: "bulk",
			claimNames: map[string]string{
				"analysis":     "ls-analysis-caller-test",
				"execution":    "ls-execution-caller-test",
				"verification": "ls-verification-caller-test",
			},
			bulk: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			run := testSMRun()
			for step, claimName := range tt.claimNames {
				switch step {
				case "analysis":
					run.Status.Steps.Analysis.Sandbox.ClaimName = claimName
				case "execution":
					run.Status.Steps.Execution.Sandbox.ClaimName = claimName
				case "verification":
					run.Status.Steps.Verification.Sandbox.ClaimName = claimName
				default:
					t.Fatalf("unsupported step %q", step)
				}
			}

			objects := []client.Object{run}
			for _, claimName := range tt.claimNames {
				objects = append(objects, sandboxReleaseObjects(t, run, claimName, claimName, false)[1])
			}
			fc := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(objects...).Build()
			recorder := &podDeleteOptionsClient{Client: fc}
			manager := newTestSandboxManager(recorder, testCache(t, "bare-pod"))
			caller := &SandboxAgentCaller{Sandbox: manager, K8sClient: recorder, Namespace: "test-ns"}

			var err error
			if tt.bulk {
				err = caller.ReleaseSandboxes(context.Background(), run, false)
			} else {
				err = caller.ReleaseSandbox(context.Background(), run, tt.releaseStep)
			}
			if err != nil {
				t.Fatalf("release: %v", err)
			}

			if len(recorder.podDeletes) != len(tt.claimNames) {
				t.Fatalf("Pod delete requests = %+v, want %d", recorder.podDeletes, len(tt.claimNames))
			}
			want := make(map[string]struct{}, len(tt.claimNames))
			for _, claimName := range tt.claimNames {
				want[claimName] = struct{}{}
			}
			for _, call := range recorder.podDeletes {
				if _, ok := want[call.name]; !ok {
					t.Fatalf("unexpected or duplicate Pod delete request for %q", call.name)
				}
				delete(want, call.name)
				if call.grace != nil {
					t.Fatalf("Pod %q delete GracePeriodSeconds = %d, want omitted", call.name, *call.grace)
				}
			}
			if len(want) != 0 {
				t.Fatalf("missing Pod delete requests for %v", want)
			}
		})
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

	if err := caller.ReleaseSandboxes(context.Background(), run, false); err == nil {
		t.Fatal("expected error when one release fails")
	}
	if len(*tracker.released) != 3 {
		t.Fatalf("expected 3 release attempts, got %d", len(*tracker.released))
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

func TestSandboxPodDeadline_UsesConfiguredTimeout(t *testing.T) {
	agent := &agenticv1alpha1.Agent{Spec: agenticv1alpha1.AgentSpec{
		Timeouts: agenticv1alpha1.AgentTimeouts{AnalysisSeconds: 30},
	}}
	want := sandboxStartupTimeout + 30*time.Second + sandboxRunningGrace
	if got := sandboxPodDeadline(agent, "analysis"); got != want {
		t.Fatalf("sandboxPodDeadline = %v, want %v", got, want)
	}
}

type trackingMockSandbox struct {
	released   *[]string
	errOnClaim string
}

func (m *trackingMockSandbox) Create(_ context.Context, _ *agenticv1alpha1.AgenticRun, _ string, _ *agenticv1alpha1.Agent, _ *agenticv1alpha1.LLMProvider, _ *agenticv1alpha1.ToolsSpec, _ time.Duration, _ *agentContext) (string, error) {
	return "", nil
}
func (m *trackingMockSandbox) Release(_ context.Context, run *agenticv1alpha1.AgenticRun, step string, _ *SpokeAccess, _ bool) error {
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
