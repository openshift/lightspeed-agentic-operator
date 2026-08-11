package agenticrun

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
)

// --- Configurable agent stub for tests ---

type testAgentCaller struct {
	analyzeErr  error
	executeErr  error
	verifyErr   error
	escalateErr error

	analyzeResult  *AnalysisOutput
	executeResult  *ExecutionOutput
	verifyResult   *VerificationOutput
	escalateResult *EscalationOutput
}

func newTestAgentCaller() *testAgentCaller {
	stub := &StubAgentCaller{}
	a, _ := stub.Analyze(context.Background(), nil, resolvedStep{}, "", "")
	e, _ := stub.Execute(context.Background(), nil, resolvedStep{}, nil, "")
	v, _ := stub.Verify(context.Background(), nil, resolvedStep{}, nil, nil, "")
	esc, _ := stub.Escalate(context.Background(), nil, resolvedStep{}, "", "")
	return &testAgentCaller{analyzeResult: a, executeResult: e, verifyResult: v, escalateResult: esc}
}

func (ta *testAgentCaller) Analyze(_ context.Context, _ *agenticv1alpha1.AgenticRun, _ resolvedStep, _ string, _ string) (*AnalysisOutput, error) {
	if ta.analyzeErr != nil {
		return nil, ta.analyzeErr
	}
	return ta.analyzeResult, nil
}
func (ta *testAgentCaller) Execute(_ context.Context, _ *agenticv1alpha1.AgenticRun, _ resolvedStep, _ *agenticv1alpha1.RemediationOption, _ string) (*ExecutionOutput, error) {
	if ta.executeErr != nil {
		return nil, ta.executeErr
	}
	return ta.executeResult, nil
}
func (ta *testAgentCaller) Verify(_ context.Context, _ *agenticv1alpha1.AgenticRun, _ resolvedStep, _ *agenticv1alpha1.RemediationOption, _ *ExecutionOutput, _ string) (*VerificationOutput, error) {
	if ta.verifyErr != nil {
		return nil, ta.verifyErr
	}
	return ta.verifyResult, nil
}
func (ta *testAgentCaller) Escalate(_ context.Context, _ *agenticv1alpha1.AgenticRun, _ resolvedStep, _ string, _ string) (*EscalationOutput, error) {
	if ta.escalateErr != nil {
		return nil, ta.escalateErr
	}
	return ta.escalateResult, nil
}
func (ta *testAgentCaller) ReleaseSandboxes(_ context.Context, _ *agenticv1alpha1.AgenticRun) error {
	return nil
}

// --- Test fixtures ---

func testScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	utilruntime.Must(agenticv1alpha1.AddToScheme(s))
	utilruntime.Must(corev1.AddToScheme(s))
	utilruntime.Must(rbacv1.AddToScheme(s))
	return s
}

func testDefaultAgent() *agenticv1alpha1.Agent {
	return &agenticv1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "default"},
		Spec: agenticv1alpha1.AgentSpec{
			LLMProvider: agenticv1alpha1.LLMProviderReference{Name: "smart"},
			Model:       "claude-opus-4-6",
		},
	}
}

func testTools() agenticv1alpha1.ToolsSpec {
	return agenticv1alpha1.ToolsSpec{
		Skills: []agenticv1alpha1.SkillsSource{{Image: "registry.example.com/skills:latest"}},
	}
}

func testLLM(name string) *agenticv1alpha1.LLMProvider {
	return &agenticv1alpha1.LLMProvider{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: agenticv1alpha1.LLMProviderSpec{
			Type: agenticv1alpha1.LLMProviderGoogleCloudVertex,
			GoogleCloudVertex: agenticv1alpha1.GoogleCloudVertexConfig{
				CredentialsSecret: agenticv1alpha1.SecretReference{Name: "llm-secret"},
				ProjectID:         "test-project",
				Region:            "us-central1",
				ModelProvider:     agenticv1alpha1.GoogleCloudVertexModelProviderAnthropic,
			},
		},
	}
}

func testAgenticRun() *agenticv1alpha1.AgenticRun {
	return &agenticv1alpha1.AgenticRun{
		ObjectMeta: metav1.ObjectMeta{Name: "fix-crash", Namespace: "default"},
		Spec: agenticv1alpha1.AgenticRunSpec{
			Request:          "Pod crashing in production",
			Tools:            testTools(),
			TargetNamespaces: []string{"production"},
			Analysis:         agenticv1alpha1.AgenticRunStep{Agent: "default"},
			Execution:        agenticv1alpha1.AgenticRunStep{Agent: "default"},
			Verification:     agenticv1alpha1.AgenticRunStep{Agent: "default"},
		},
	}
}

// testAutoApprovePolicy returns an ApprovalPolicy that auto-approves analysis
// and verification stages, so tests only need to explicitly approve execution
// (which carries the selected option).
func testAutoApprovePolicy() *agenticv1alpha1.ApprovalPolicy {
	return testAutoApprovePolicyWithMaxAttempts(0)
}

func testAutoApprovePolicyWithMaxAttempts(maxAttempts int32) *agenticv1alpha1.ApprovalPolicy {
	return &agenticv1alpha1.ApprovalPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster"},
		Spec: agenticv1alpha1.ApprovalPolicySpec{
			MaxAttempts: maxAttempts,
			Stages: []agenticv1alpha1.ApprovalPolicyStage{
				{Name: agenticv1alpha1.SandboxStepAnalysis, Approval: agenticv1alpha1.ApprovalModeAutomatic},
				{Name: agenticv1alpha1.SandboxStepVerification, Approval: agenticv1alpha1.ApprovalModeAutomatic},
			},
		},
	}
}

// defaultObjects returns the standard set of cluster-scoped and namespaced
// objects needed to resolve a full workflow.
func defaultObjects() []client.Object {
	return []client.Object{
		testDefaultAgent(), testLLM("smart"), testAutoApprovePolicy(), testReaderClusterRoleBinding(),
	}
}

func defaultObjectsWithMaxAttempts(maxAttempts int32) []client.Object {
	return []client.Object{
		testDefaultAgent(), testLLM("smart"), testAutoApprovePolicyWithMaxAttempts(maxAttempts), testReaderClusterRoleBinding(),
	}
}

func testReaderClusterRoleBinding() *rbacv1.ClusterRoleBinding {
	return &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: defaultReaderClusterRoleBinding},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "cluster-reader"},
		Subjects: []rbacv1.Subject{{
			Kind:      rbacv1.ServiceAccountKind,
			Name:      "lightspeed-agent",
			Namespace: "default",
		}},
	}
}

func reconcileOnce(r *AgenticRunReconciler, name string) (ctrl.Result, error) {
	return r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: name, Namespace: "default"},
	})
}

func getAgenticRun(r *AgenticRunReconciler, name string) (*agenticv1alpha1.AgenticRun, error) {
	var p agenticv1alpha1.AgenticRun
	err := r.Get(context.Background(), types.NamespacedName{Name: name, Namespace: "default"}, &p)
	return &p, err
}

func approveAgenticRun(t *testing.T, fc client.WithWatch, name string) {
	t.Helper()
	approveAgenticRunWithOption(t, fc, name, 0)
}

func approveAgenticRunWithOption(t *testing.T, fc client.WithWatch, name string, optionIndex int32) {
	t.Helper()
	var approval agenticv1alpha1.AgenticRunApproval
	if err := fc.Get(context.Background(), types.NamespacedName{Name: name, Namespace: "default"}, &approval); err != nil {
		t.Fatalf("get AgenticRunApproval for approval: %v", err)
	}
	base := approval.DeepCopy()
	hasExecution := false
	for i, s := range approval.Spec.Stages {
		if s.Type == agenticv1alpha1.ApprovalStageExecution {
			approval.Spec.Stages[i].Execution = &agenticv1alpha1.ExecutionApproval{Option: &optionIndex}
			hasExecution = true
			break
		}
	}
	if !hasExecution {
		approval.Spec.Stages = append(approval.Spec.Stages, agenticv1alpha1.ApprovalStage{
			Type:      agenticv1alpha1.ApprovalStageExecution,
			Execution: &agenticv1alpha1.ExecutionApproval{Option: &optionIndex},
		})
	}
	if err := fc.Patch(context.Background(), &approval, client.MergeFrom(base)); err != nil {
		t.Fatalf("approve execution with option %d: %v", optionIndex, err)
	}
}

// --- Sandbox-based reconciler helpers ---

func fakeBaseTemplate() *unstructured.Unstructured {
	return &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "extensions.agents.x-k8s.io/v1alpha1",
			"kind":       "SandboxTemplate",
			"metadata": map[string]any{
				"name":      "lightspeed-agent",
				"namespace": "test-ns",
			},
			"spec": map[string]any{
				"podTemplate": map[string]any{
					"spec": map[string]any{
						"serviceAccountName": "lightspeed-agent",
						"containers": []any{
							map[string]any{
								"name":  "agent",
								"image": "test-agent:latest",
								"env":   []any{},
							},
						},
						"volumes": []any{
							map[string]any{"name": "skills", "image": map[string]any{"reference": "placeholder:latest"}},
						},
					},
				},
			},
		},
	}
}

func newMockSandboxAgent(analysisJSON, executionJSON, verificationJSON string) (*SandboxAgentCaller, *mockSandboxProvider) {
	sandbox := &mockSandboxProvider{claimName: "ls-test-claim", endpoint: "http://sandbox:8080"}

	fc := fake.NewClientBuilder().WithScheme(testScheme()).Build()
	_ = fc.Create(context.Background(), fakeBaseTemplate())

	callCount := 0
	responses := []string{analysisJSON, executionJSON, verificationJSON}

	httpClient := &mockHTTPClient{}
	caller := &SandboxAgentCaller{
		Sandbox:   sandbox,
		K8sClient: fc,
		ClientFactory: func(_ string, _ time.Duration) AgentHTTPClientInterface {
			resp := responses[callCount%len(responses)]
			callCount++
			httpClient.response = &agentRunResponse{Response: json.RawMessage(resp)}
			return httpClient
		},
		Namespace: "test-ns",
		Timeout:   5 * time.Minute,
	}
	return caller, sandbox
}

// --- Reconciler-level tests ---

func TestReconcile_StatusInitialization(t *testing.T) {
	scheme := testScheme()
	run := &agenticv1alpha1.AgenticRun{
		ObjectMeta: metav1.ObjectMeta{Name: "fresh", Namespace: "default"},
		Spec: agenticv1alpha1.AgenticRunSpec{
			Request:      "Pod crashing",
			Tools:        testTools(),
			Analysis:     agenticv1alpha1.AgenticRunStep{Agent: "default"},
			Execution:    agenticv1alpha1.AgenticRunStep{Agent: "default"},
			Verification: agenticv1alpha1.AgenticRunStep{Agent: "default"},
		},
	}

	objs := append([]client.Object{run}, defaultObjects()...)
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
		WithStatusSubresource(run, &agenticv1alpha1.AnalysisResult{}, &agenticv1alpha1.ExecutionResult{}, &agenticv1alpha1.VerificationResult{}, &agenticv1alpha1.EscalationResult{}).Build()

	r := &AgenticRunReconciler{Client: fc, Agent: newTestAgentCaller(), Namespace: "default"}

	_, err := reconcileOnce(r, "fresh")
	if err != nil {
		t.Fatalf("reconcile on nil status: %v", err)
	}

	p, _ := getAgenticRun(r, "fresh")
	phase := agenticv1alpha1.DerivePhase(p.Status.Conditions)
	if phase != agenticv1alpha1.AgenticRunPhaseProposed {
		t.Fatalf("expected Proposed (analysis complete), got %s", phase)
	}
}

func TestReconcile_Denied_Terminal(t *testing.T) {
	scheme := testScheme()

	run := testAgenticRun()
	run.Status = agenticv1alpha1.AgenticRunStatus{
		Conditions: []metav1.Condition{
			{Type: agenticv1alpha1.AgenticRunConditionAnalyzed, Status: metav1.ConditionTrue, Reason: "AnalysisComplete"},
			{Type: agenticv1alpha1.AgenticRunConditionDenied, Status: metav1.ConditionTrue, Reason: "UserDenied"},
		},
	}

	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(run).WithStatusSubresource(run, &agenticv1alpha1.AnalysisResult{}, &agenticv1alpha1.ExecutionResult{}, &agenticv1alpha1.VerificationResult{}, &agenticv1alpha1.EscalationResult{}).Build()
	r := &AgenticRunReconciler{Client: fc, Agent: newTestAgentCaller(), Namespace: "default"}

	result, err := reconcileOnce(r, "fix-crash")
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if result.Requeue {
		t.Error("terminal phase should not requeue")
	}
	p, _ := getAgenticRun(r, "fix-crash")
	if agenticv1alpha1.DerivePhase(p.Status.Conditions) != agenticv1alpha1.AgenticRunPhaseDenied {
		t.Fatalf("expected Denied, got %s", agenticv1alpha1.DerivePhase(p.Status.Conditions))
	}
}

func TestReconcileSuspension(t *testing.T) {
	suspendedConfig := &agenticv1alpha1.AgenticOLSConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster"},
		Spec:       agenticv1alpha1.AgenticOLSConfigSpec{Suspended: true},
	}

	t.Run("suspended terminates pending run", func(t *testing.T) {
		p := testAgenticRun()
		objs := append(defaultObjects(), p, suspendedConfig)
		fc := fake.NewClientBuilder().
			WithScheme(testScheme()).
			WithObjects(objs...).
			WithStatusSubresource(&agenticv1alpha1.AgenticRun{}).
			Build()
		r := &AgenticRunReconciler{
			Client:    fc,
			Agent:     newTestAgentCaller(),
			Namespace: "default",
		}
		_, err := reconcileOnce(r, "fix-crash")
		if err != nil {
			t.Fatalf("Reconcile error: %v", err)
		}
		got, _ := getAgenticRun(r, "fix-crash")
		phase := agenticv1alpha1.DerivePhase(got.Status.Conditions)
		if phase != agenticv1alpha1.AgenticRunPhaseEmergencyStopped {
			t.Errorf("phase = %s, want EmergencyStopped", phase)
		}
	})

	t.Run("not suspended proceeds normally", func(t *testing.T) {
		p := testAgenticRun()
		activeConfig := &agenticv1alpha1.AgenticOLSConfig{
			ObjectMeta: metav1.ObjectMeta{Name: "cluster"},
			Spec:       agenticv1alpha1.AgenticOLSConfigSpec{Suspended: false},
		}
		objs := append(defaultObjects(), p, activeConfig)
		fc := fake.NewClientBuilder().
			WithScheme(testScheme()).
			WithObjects(objs...).
			WithStatusSubresource(&agenticv1alpha1.AgenticRun{}).
			Build()
		r := &AgenticRunReconciler{
			Client:    fc,
			Agent:     newTestAgentCaller(),
			Namespace: "default",
		}
		_, err := reconcileOnce(r, "fix-crash")
		if err != nil {
			t.Fatalf("Reconcile error: %v", err)
		}
		got, _ := getAgenticRun(r, "fix-crash")
		phase := agenticv1alpha1.DerivePhase(got.Status.Conditions)
		if phase == agenticv1alpha1.AgenticRunPhaseEmergencyStopped {
			t.Error("run should NOT be EmergencyStopped when not suspended")
		}
	})

	t.Run("no config CR proceeds normally", func(t *testing.T) {
		p := testAgenticRun()
		objs := append(defaultObjects(), p) // no AgenticOLSConfig
		fc := fake.NewClientBuilder().
			WithScheme(testScheme()).
			WithObjects(objs...).
			WithStatusSubresource(&agenticv1alpha1.AgenticRun{}).
			Build()
		r := &AgenticRunReconciler{
			Client:    fc,
			Agent:     newTestAgentCaller(),
			Namespace: "default",
		}
		_, err := reconcileOnce(r, "fix-crash")
		if err != nil {
			t.Fatalf("Reconcile error: %v", err)
		}
		got, _ := getAgenticRun(r, "fix-crash")
		phase := agenticv1alpha1.DerivePhase(got.Status.Conditions)
		if phase == agenticv1alpha1.AgenticRunPhaseEmergencyStopped {
			t.Error("run should NOT be EmergencyStopped when config is absent")
		}
	})

	t.Run("EmergencyStopped run takes terminal path without error", func(t *testing.T) {
		p := testAgenticRun()
		p.Status.Conditions = []metav1.Condition{{
			Type:   agenticv1alpha1.AgenticRunConditionEmergencyStopped,
			Status: metav1.ConditionTrue,
			Reason: reasonSystemSuspended,
		}}
		objs := append(defaultObjects(), p)
		fc := fake.NewClientBuilder().
			WithScheme(testScheme()).
			WithObjects(objs...).
			WithStatusSubresource(&agenticv1alpha1.AgenticRun{}).
			Build()
		r := &AgenticRunReconciler{
			Client:    fc,
			Agent:     newTestAgentCaller(),
			Namespace: "default",
		}
		_, err := reconcileOnce(r, "fix-crash")
		if err != nil {
			t.Fatalf("Reconcile error: %v", err)
		}
	})
}

func TestHandleSuspension(t *testing.T) {
	tests := []struct {
		name          string
		run           *agenticv1alpha1.AgenticRun
		wantPhase     agenticv1alpha1.AgenticRunPhase
		wantCondition bool
	}{
		{
			name: "non-terminal run gets EmergencyStopped",
			run: func() *agenticv1alpha1.AgenticRun {
				p := testAgenticRun()
				return p
			}(),
			wantPhase:     agenticv1alpha1.AgenticRunPhaseEmergencyStopped,
			wantCondition: true,
		},
		{
			name: "already-completed run is unchanged",
			run: func() *agenticv1alpha1.AgenticRun {
				p := testAgenticRun()
				p.Status.Conditions = []metav1.Condition{{
					Type:   agenticv1alpha1.AgenticRunConditionVerified,
					Status: metav1.ConditionTrue,
					Reason: "Complete",
				}}
				return p
			}(),
			wantPhase:     agenticv1alpha1.AgenticRunPhaseCompleted,
			wantCondition: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			objs := append(defaultObjects(), tt.run)
			fc := fake.NewClientBuilder().
				WithScheme(testScheme()).
				WithObjects(objs...).
				WithStatusSubresource(&agenticv1alpha1.AgenticRun{}).
				Build()
			r := &AgenticRunReconciler{
				Client:    fc,
				Agent:     newTestAgentCaller(),
				Namespace: "default",
			}
			_, err := r.handleSuspension(context.Background(), tt.run)
			if err != nil {
				t.Fatalf("handleSuspension() error: %v", err)
			}
			got, _ := getAgenticRun(r, tt.run.Name)
			phase := agenticv1alpha1.DerivePhase(got.Status.Conditions)
			if phase != tt.wantPhase {
				t.Errorf("phase = %s, want %s", phase, tt.wantPhase)
			}
			if tt.wantCondition {
				found := false
				for _, c := range got.Status.Conditions {
					if c.Type == agenticv1alpha1.AgenticRunConditionEmergencyStopped && c.Status == metav1.ConditionTrue {
						if c.Reason != reasonSystemSuspended {
							t.Errorf("reason = %s, want %s", c.Reason, reasonSystemSuspended)
						}
						found = true
					}
				}
				if !found {
					t.Error("EmergencyStopped condition not found")
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Templog cleanup finalizer
// ---------------------------------------------------------------------------

type mockTempLogCleaner struct {
	err       error
	callCount int
}

func (m *mockTempLogCleaner) DeleteLogs(_ context.Context, _ string) error {
	m.callCount++
	return m.err
}

func TestTemplogCleanup_HappyPath(t *testing.T) {
	now := metav1.Now()
	run := testAgenticRun()
	run.UID = "a1b2c3d4-e5f6-7890-abcd-ef1234567890"
	run.DeletionTimestamp = &now
	run.Finalizers = []string{templogCleanupFinalizer}

	objs := append([]client.Object{run}, defaultObjects()...)
	fc := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(objs...).
		WithStatusSubresource(run).Build()

	cleaner := &mockTempLogCleaner{}
	r := &AgenticRunReconciler{Client: fc, Agent: newTestAgentCaller(), Namespace: "default", TempLog: cleaner}

	reconcileOnce(r, "fix-crash")

	var updated agenticv1alpha1.AgenticRun
	_ = fc.Get(context.Background(), types.NamespacedName{Name: "fix-crash", Namespace: "default"}, &updated)

	if controllerutil.ContainsFinalizer(&updated, templogCleanupFinalizer) {
		t.Error("templog finalizer should be removed after successful cleanup")
	}
	if cleaner.callCount != 1 {
		t.Errorf("DeleteLogs called %d times, want 1", cleaner.callCount)
	}
}

func TestTemplogCleanup_RetryOnFailure(t *testing.T) {
	now := metav1.Now()
	run := testAgenticRun()
	run.UID = "a1b2c3d4-e5f6-7890-abcd-ef1234567890"
	run.DeletionTimestamp = &now
	run.Finalizers = []string{templogCleanupFinalizer}

	objs := append([]client.Object{run}, defaultObjects()...)
	fc := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(objs...).
		WithStatusSubresource(run).Build()

	cleaner := &mockTempLogCleaner{err: fmt.Errorf("collector unavailable")}
	r := &AgenticRunReconciler{Client: fc, Agent: newTestAgentCaller(), Namespace: "default", TempLog: cleaner}

	result, _ := reconcileOnce(r, "fix-crash")
	if result.RequeueAfter != templogCleanupRequeueAfter {
		t.Errorf("RequeueAfter = %v, want %v", result.RequeueAfter, templogCleanupRequeueAfter)
	}

	var updated agenticv1alpha1.AgenticRun
	_ = fc.Get(context.Background(), types.NamespacedName{Name: "fix-crash", Namespace: "default"}, &updated)

	if !controllerutil.ContainsFinalizer(&updated, templogCleanupFinalizer) {
		t.Error("finalizer should still be present after failed attempt")
	}
	if updated.Annotations[templogCleanupAttemptsAnnotation] != "1" {
		t.Errorf("attempts annotation = %q, want '1'", updated.Annotations[templogCleanupAttemptsAnnotation])
	}
}

func TestTemplogCleanup_ExhaustedRetries(t *testing.T) {
	now := metav1.Now()
	run := testAgenticRun()
	run.UID = "a1b2c3d4-e5f6-7890-abcd-ef1234567890"
	run.DeletionTimestamp = &now
	run.Finalizers = []string{templogCleanupFinalizer}
	run.Annotations = map[string]string{
		templogCleanupAttemptsAnnotation: "3",
	}

	objs := append([]client.Object{run}, defaultObjects()...)
	fc := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(objs...).
		WithStatusSubresource(run).Build()

	cleaner := &mockTempLogCleaner{err: fmt.Errorf("still failing")}
	r := &AgenticRunReconciler{Client: fc, Agent: newTestAgentCaller(), Namespace: "default", TempLog: cleaner}

	reconcileOnce(r, "fix-crash")

	var updated agenticv1alpha1.AgenticRun
	_ = fc.Get(context.Background(), types.NamespacedName{Name: "fix-crash", Namespace: "default"}, &updated)

	if controllerutil.ContainsFinalizer(&updated, templogCleanupFinalizer) {
		t.Error("finalizer should be removed after exhausting retries")
	}
	if cleaner.callCount != 0 {
		t.Errorf("DeleteLogs should not be called when retries exhausted, got %d calls", cleaner.callCount)
	}
}

func TestTemplogCleanup_InvalidAttemptsAnnotationResets(t *testing.T) {
	now := metav1.Now()
	run := testAgenticRun()
	run.UID = "a1b2c3d4-e5f6-7890-abcd-ef1234567890"
	run.DeletionTimestamp = &now
	run.Finalizers = []string{templogCleanupFinalizer}
	run.Annotations = map[string]string{
		templogCleanupAttemptsAnnotation: "not-a-number",
	}

	objs := append([]client.Object{run}, defaultObjects()...)
	fc := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(objs...).
		WithStatusSubresource(run).Build()

	cleaner := &mockTempLogCleaner{}
	r := &AgenticRunReconciler{Client: fc, Agent: newTestAgentCaller(), Namespace: "default", TempLog: cleaner}

	if _, err := reconcileOnce(r, "fix-crash"); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if cleaner.callCount != 1 {
		t.Errorf("DeleteLogs called %d times, want 1 (invalid annotation must not skip cleanup)", cleaner.callCount)
	}
}

func TestReconcile_AddsFinalizersOnTerminalRun(t *testing.T) {
	run := testAgenticRun()
	run.Status.Conditions = []metav1.Condition{{
		Type:   agenticv1alpha1.AgenticRunConditionVerified,
		Status: metav1.ConditionTrue,
		Reason: "Complete",
	}}

	objs := append([]client.Object{run}, defaultObjects()...)
	fc := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(objs...).
		WithStatusSubresource(run).Build()

	r := &AgenticRunReconciler{Client: fc, Agent: newTestAgentCaller(), Namespace: "default"}
	if _, err := reconcileOnce(r, "fix-crash"); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var updated agenticv1alpha1.AgenticRun
	if err := fc.Get(context.Background(), types.NamespacedName{Name: "fix-crash", Namespace: "default"}, &updated); err != nil {
		t.Fatalf("get: %v", err)
	}
	if !controllerutil.ContainsFinalizer(&updated, rbacCleanupFinalizer) {
		t.Error("rbac finalizer should be added on first sight of terminal run")
	}
	if !controllerutil.ContainsFinalizer(&updated, templogCleanupFinalizer) {
		t.Error("templog finalizer should be added on first sight of terminal run")
	}
}

func TestDeletion_RequeuesAfterRBACFinalizerBeforeTemplog(t *testing.T) {
	now := metav1.Now()
	run := testAgenticRun()
	run.UID = "a1b2c3d4-e5f6-7890-abcd-ef1234567890"
	run.DeletionTimestamp = &now
	run.Finalizers = []string{rbacCleanupFinalizer, templogCleanupFinalizer}

	objs := append([]client.Object{run}, defaultObjects()...)
	fc := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(objs...).
		WithStatusSubresource(run).Build()

	cleaner := &mockTempLogCleaner{}
	r := &AgenticRunReconciler{Client: fc, Agent: newTestAgentCaller(), Namespace: "default", TempLog: cleaner}

	result, err := reconcileOnce(r, "fix-crash")
	if err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	if !result.Requeue {
		t.Error("expected requeue after removing rbac finalizer")
	}
	if cleaner.callCount != 0 {
		t.Errorf("DeleteLogs should not run until next reconcile, got %d calls", cleaner.callCount)
	}

	var updated agenticv1alpha1.AgenticRun
	if err := fc.Get(context.Background(), types.NamespacedName{Name: "fix-crash", Namespace: "default"}, &updated); err != nil {
		t.Fatalf("get after first reconcile: %v", err)
	}
	if controllerutil.ContainsFinalizer(&updated, rbacCleanupFinalizer) {
		t.Error("rbac finalizer should be removed")
	}
	if !controllerutil.ContainsFinalizer(&updated, templogCleanupFinalizer) {
		t.Error("templog finalizer should still be present")
	}

	result, err = reconcileOnce(r, "fix-crash")
	if err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	if result.Requeue || result.RequeueAfter != 0 {
		t.Errorf("expected no requeue after templog cleanup, got %+v", result)
	}
	if cleaner.callCount != 1 {
		t.Errorf("DeleteLogs called %d times, want 1", cleaner.callCount)
	}
	err = fc.Get(context.Background(), types.NamespacedName{Name: "fix-crash", Namespace: "default"}, &updated)
	if client.IgnoreNotFound(err) != nil {
		t.Fatalf("get after second reconcile: %v", err)
	}
	if err == nil && controllerutil.ContainsFinalizer(&updated, templogCleanupFinalizer) {
		t.Error("templog finalizer should be removed after cleanup")
	}
}

// --- copyResultStatus tests ---

func TestCopyResultStatus_AnalysisResult(t *testing.T) {
	src := &agenticv1alpha1.AnalysisResult{}
	src.Status.ActionRequired = agenticv1alpha1.ActionRequiredTrue
	src.Status.Diagnosis = agenticv1alpha1.DiagnosisResult{RootCause: "OOM"}
	src.Status.FailureReason = "timeout"
	src.Status.Sandbox = agenticv1alpha1.SandboxInfo{ClaimName: "claim-a"}
	src.Status.Options = []agenticv1alpha1.RemediationOption{{Title: "fix1"}}

	dst := &agenticv1alpha1.AnalysisResult{}
	copyResultStatus(dst, src)

	if dst.Status.ActionRequired != agenticv1alpha1.ActionRequiredTrue {
		t.Errorf("ActionRequired = %q", dst.Status.ActionRequired)
	}
	if dst.Status.Diagnosis.RootCause != "OOM" {
		t.Errorf("Diagnosis.RootCause = %q", dst.Status.Diagnosis.RootCause)
	}
	if dst.Status.FailureReason != "timeout" {
		t.Errorf("FailureReason = %q", dst.Status.FailureReason)
	}
	if dst.Status.Sandbox.ClaimName != "claim-a" {
		t.Errorf("Sandbox.ClaimName = %q", dst.Status.Sandbox.ClaimName)
	}
	if len(dst.Status.Options) != 1 || dst.Status.Options[0].Title != "fix1" {
		t.Errorf("Options = %v", dst.Status.Options)
	}
}

func TestCopyResultStatus_ExecutionResult(t *testing.T) {
	src := &agenticv1alpha1.ExecutionResult{}
	src.Status.ActionsTaken = []agenticv1alpha1.ExecutionAction{{Description: "scaled up"}}
	src.Status.FailureReason = "rbac denied"
	src.Status.Sandbox = agenticv1alpha1.SandboxInfo{ClaimName: "claim-e"}

	dst := &agenticv1alpha1.ExecutionResult{}
	copyResultStatus(dst, src)

	if len(dst.Status.ActionsTaken) != 1 || dst.Status.ActionsTaken[0].Description != "scaled up" {
		t.Errorf("ActionsTaken = %v", dst.Status.ActionsTaken)
	}
	if dst.Status.FailureReason != "rbac denied" {
		t.Errorf("FailureReason = %q", dst.Status.FailureReason)
	}
	if dst.Status.Sandbox.ClaimName != "claim-e" {
		t.Errorf("Sandbox.ClaimName = %q", dst.Status.Sandbox.ClaimName)
	}
}

func TestCopyResultStatus_VerificationResult(t *testing.T) {
	src := &agenticv1alpha1.VerificationResult{}
	src.Status.Checks = []agenticv1alpha1.VerifyCheck{
		{Name: "pod-running", Source: "oc", Value: "Running", Result: agenticv1alpha1.CheckResultPassed},
	}
	src.Status.Summary = "all green"
	src.Status.FailureReason = "check timeout"
	src.Status.Sandbox = agenticv1alpha1.SandboxInfo{ClaimName: "claim-v"}

	dst := &agenticv1alpha1.VerificationResult{}
	copyResultStatus(dst, src)

	if len(dst.Status.Checks) != 1 || dst.Status.Checks[0].Name != "pod-running" {
		t.Errorf("Checks = %v", dst.Status.Checks)
	}
	if dst.Status.Summary != "all green" {
		t.Errorf("Summary = %q", dst.Status.Summary)
	}
	if dst.Status.FailureReason != "check timeout" {
		t.Errorf("FailureReason = %q", dst.Status.FailureReason)
	}
	if dst.Status.Sandbox.ClaimName != "claim-v" {
		t.Errorf("Sandbox.ClaimName = %q", dst.Status.Sandbox.ClaimName)
	}
}

func TestCopyResultStatus_EscalationResult(t *testing.T) {
	src := &agenticv1alpha1.EscalationResult{}
	src.Status.Summary = "needs human"
	src.Status.Content = "detailed report"
	src.Status.FailureReason = "agent error"
	src.Status.Sandbox = agenticv1alpha1.SandboxInfo{ClaimName: "claim-esc"}

	dst := &agenticv1alpha1.EscalationResult{}
	copyResultStatus(dst, src)

	if dst.Status.Summary != "needs human" {
		t.Errorf("Summary = %q", dst.Status.Summary)
	}
	if dst.Status.Content != "detailed report" {
		t.Errorf("Content = %q", dst.Status.Content)
	}
	if dst.Status.FailureReason != "agent error" {
		t.Errorf("FailureReason = %q", dst.Status.FailureReason)
	}
	if dst.Status.Sandbox.ClaimName != "claim-esc" {
		t.Errorf("Sandbox.ClaimName = %q", dst.Status.Sandbox.ClaimName)
	}
}

func TestCopyResultStatus_TypeMismatch(t *testing.T) {
	src := &agenticv1alpha1.AnalysisResult{}
	src.Status.FailureReason = "should not copy"

	dst := &agenticv1alpha1.ExecutionResult{}
	copyResultStatus(dst, src)

	if dst.Status.FailureReason != "" {
		t.Error("mismatched types should not copy anything")
	}
}

// --- handleRBACCleanup tests ---

type failingAgentCaller struct {
	testAgentCaller
	releaseErr error
}

func (f *failingAgentCaller) ReleaseSandboxes(_ context.Context, _ *agenticv1alpha1.AgenticRun) error {
	return f.releaseErr
}

func TestHandleRBACCleanup_HappyPath(t *testing.T) {
	now := metav1.Now()
	run := testAgenticRun()
	run.DeletionTimestamp = &now
	run.Finalizers = []string{rbacCleanupFinalizer}

	objs := append([]client.Object{run}, defaultObjects()...)
	fc := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(objs...).
		WithStatusSubresource(run).Build()

	r := &AgenticRunReconciler{Client: fc, Agent: newTestAgentCaller(), Namespace: "default"}
	result, err := r.handleRBACCleanup(context.Background(), run)
	if err != nil {
		t.Fatalf("handleRBACCleanup: %v", err)
	}
	if !result.Requeue {
		t.Error("expected requeue after removing RBAC finalizer")
	}

	var updated agenticv1alpha1.AgenticRun
	err = fc.Get(context.Background(), types.NamespacedName{Name: "fix-crash", Namespace: "default"}, &updated)
	if client.IgnoreNotFound(err) != nil {
		t.Fatalf("get updated run: %v", err)
	}
	if err == nil && controllerutil.ContainsFinalizer(&updated, rbacCleanupFinalizer) {
		t.Error("rbac finalizer should be removed on success")
	}
}

func TestHandleRBACCleanup_RetryOnSandboxError(t *testing.T) {
	now := metav1.Now()
	run := testAgenticRun()
	run.DeletionTimestamp = &now
	run.Finalizers = []string{rbacCleanupFinalizer}

	objs := append([]client.Object{run}, defaultObjects()...)
	fc := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(objs...).
		WithStatusSubresource(run).Build()

	agent := &failingAgentCaller{
		testAgentCaller: *newTestAgentCaller(),
		releaseErr:      fmt.Errorf("sandbox unreachable"),
	}
	r := &AgenticRunReconciler{Client: fc, Agent: agent, Namespace: "default"}
	result, err := r.handleRBACCleanup(context.Background(), run)
	if err != nil {
		t.Fatalf("handleRBACCleanup: %v", err)
	}
	if result.RequeueAfter != rbacCleanupRequeueAfter {
		t.Errorf("RequeueAfter = %v, want %v", result.RequeueAfter, rbacCleanupRequeueAfter)
	}

	var updated agenticv1alpha1.AgenticRun
	if err := fc.Get(context.Background(), types.NamespacedName{Name: "fix-crash", Namespace: "default"}, &updated); err != nil {
		t.Fatalf("get updated run: %v", err)
	}
	if !controllerutil.ContainsFinalizer(&updated, rbacCleanupFinalizer) {
		t.Error("finalizer should remain on failure")
	}
	if updated.Annotations[rbacCleanupAttemptsAnnotation] != "1" {
		t.Errorf("attempts = %q, want '1'", updated.Annotations[rbacCleanupAttemptsAnnotation])
	}
}

func TestHandleRBACCleanup_ExhaustedRetries(t *testing.T) {
	now := metav1.Now()
	run := testAgenticRun()
	run.DeletionTimestamp = &now
	run.Finalizers = []string{rbacCleanupFinalizer}
	run.Annotations = map[string]string{
		rbacCleanupAttemptsAnnotation: fmt.Sprintf("%d", rbacMaxCleanupAttempts),
	}

	objs := append([]client.Object{run}, defaultObjects()...)
	fc := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(objs...).
		WithStatusSubresource(run).Build()

	agent := &failingAgentCaller{
		testAgentCaller: *newTestAgentCaller(),
		releaseErr:      fmt.Errorf("still broken"),
	}
	r := &AgenticRunReconciler{Client: fc, Agent: agent, Namespace: "default"}
	result, err := r.handleRBACCleanup(context.Background(), run)
	if err != nil {
		t.Fatalf("handleRBACCleanup: %v", err)
	}
	if !result.Requeue {
		t.Error("expected Requeue after removing finalizer")
	}

	var updated agenticv1alpha1.AgenticRun
	err = fc.Get(context.Background(), types.NamespacedName{Name: "fix-crash", Namespace: "default"}, &updated)
	if client.IgnoreNotFound(err) != nil {
		t.Fatalf("get updated run: %v", err)
	}
	if err == nil && controllerutil.ContainsFinalizer(&updated, rbacCleanupFinalizer) {
		t.Error("finalizer should be removed after exhausting retries")
	}
}

func TestHandleRBACCleanup_InvalidAnnotation(t *testing.T) {
	now := metav1.Now()
	run := testAgenticRun()
	run.DeletionTimestamp = &now
	run.Finalizers = []string{rbacCleanupFinalizer}
	run.Annotations = map[string]string{
		rbacCleanupAttemptsAnnotation: "garbage",
	}

	objs := append([]client.Object{run}, defaultObjects()...)
	fc := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(objs...).
		WithStatusSubresource(run).Build()

	r := &AgenticRunReconciler{Client: fc, Agent: newTestAgentCaller(), Namespace: "default"}
	result, err := r.handleRBACCleanup(context.Background(), run)
	if err != nil {
		t.Fatalf("handleRBACCleanup: %v", err)
	}
	if !result.Requeue {
		t.Error("expected Requeue (cleanup succeeded, annotation reset to 0)")
	}
}
