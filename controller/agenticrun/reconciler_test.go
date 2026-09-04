package agenticrun

import (
	"context"
	"fmt"
	"testing"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
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

	// Optional result content — when set AND fc is non-nil, the step method
	// simulates sandbox completion by creating a Result CR, recording a
	// StepResultRef, and patching the step condition to True.
	analyzeResult  *AnalysisOutput
	executeResult  *ExecutionOutput
	verifyResult   *VerificationOutput
	escalateResult *EscalationOutput

	fc client.Client
	ns string
	t  *testing.T

	releaseAllCount int
	releasedSteps   []string
}

func (ta *testAgentCaller) record(err error) bool {
	if err == nil || apierrors.IsAlreadyExists(err) || apierrors.IsConflict(err) {
		return true
	}
	if ta.t != nil {
		ta.t.Helper()
		ta.t.Errorf("testAgentCaller simulation error: %v", err)
	}
	return false
}

func newTestAgentCaller() *testAgentCaller {
	actionRequired := true
	return &testAgentCaller{
		analyzeResult: &AnalysisOutput{
			Success:        true,
			ActionRequired: &actionRequired,
			Options: []agenticv1alpha1.RemediationOption{{
				Title: "Stub remediation",
				Diagnosis: agenticv1alpha1.DiagnosisResult{
					Summary:   "Stub diagnosis",
					RootCause: "Stub root cause",
				},
				RemediationPlan: agenticv1alpha1.RemediationPlan{
					Description: "Stub remediation plan",
					Actions:     []agenticv1alpha1.ProposedAction{{Command: "kubectl get pods -n default", Type: "pre-check", Description: "Stub action"}},
					Reversible:  agenticv1alpha1.ReversibilityReversible,
				},
			}},
		},
		executeResult: &ExecutionOutput{
			Success: true,
			ActionsTaken: []agenticv1alpha1.ExecutionAction{{
				Type: "stub", Description: "Stub execution action", Outcome: agenticv1alpha1.ActionOutcomeSucceeded,
			}},
		},
		verifyResult: &VerificationOutput{
			Success: true,
			Checks:  []agenticv1alpha1.VerifyCheck{{Name: "stub-check", Source: "stub", Value: "ok", Result: agenticv1alpha1.CheckResultPassed}},
			Summary: "Stub verification passed",
		},
		escalateResult: &EscalationOutput{Success: true, Summary: "Stub escalation", Content: "Stub content"},
	}
}

// withClient wires the fake client so that step methods simulate sandbox
// completion inline (create Result CR + patch condition True).
func (ta *testAgentCaller) withClient(t *testing.T, fc client.Client, ns string) *testAgentCaller {
	t.Helper()
	ta.fc = fc
	ta.ns = ns
	ta.t = t
	return ta
}

func (ta *testAgentCaller) Analyze(ctx context.Context, run *agenticv1alpha1.AgenticRun, _ resolvedStep) error {
	if ta.analyzeErr != nil {
		return ta.analyzeErr
	}
	ta.completeAnalysis(ctx, run)
	return nil
}

func (ta *testAgentCaller) Execute(ctx context.Context, run *agenticv1alpha1.AgenticRun, _ resolvedStep, _ *agenticv1alpha1.RemediationOption) error {
	if ta.executeErr != nil {
		return ta.executeErr
	}
	ta.completeExecution(ctx, run)
	return nil
}

func (ta *testAgentCaller) Verify(ctx context.Context, run *agenticv1alpha1.AgenticRun, _ resolvedStep, _ *agenticv1alpha1.RemediationOption, _ *ExecutionOutput) error {
	if ta.verifyErr != nil {
		return ta.verifyErr
	}
	ta.completeVerification(ctx, run)
	return nil
}

func (ta *testAgentCaller) Escalate(ctx context.Context, run *agenticv1alpha1.AgenticRun, _ resolvedStep) error {
	if ta.escalateErr != nil {
		return ta.escalateErr
	}
	ta.completeEscalation(ctx, run)
	return nil
}

func (ta *testAgentCaller) ReleaseSandboxes(_ context.Context, _ *agenticv1alpha1.AgenticRun) error {
	ta.releaseAllCount++
	return nil
}

func (ta *testAgentCaller) ReleaseSandbox(_ context.Context, _ *agenticv1alpha1.AgenticRun, step string) error {
	ta.releasedSteps = append(ta.releasedSteps, step)
	return nil
}

// --- Inline sandbox completion simulation ---
// These methods create Result CRs and patch step conditions to True,
// simulating what the real sandbox + pod handler would do asynchronously.

func (ta *testAgentCaller) completeAnalysis(ctx context.Context, run *agenticv1alpha1.AgenticRun) {
	if ta.fc == nil || ta.analyzeResult == nil {
		return
	}
	var fresh agenticv1alpha1.AgenticRun
	if !ta.record(ta.fc.Get(ctx, client.ObjectKeyFromObject(run), &fresh)) {
		return
	}

	now := metav1.Now()
	outcome := agenticv1alpha1.ActionOutcomeFromBool(ta.analyzeResult.Success)
	crName := resultCRName(fresh.Name, "analysis", len(fresh.Status.Steps.Analysis.Results))
	cr := &agenticv1alpha1.AnalysisResult{
		ObjectMeta: metav1.ObjectMeta{
			Name: crName, Namespace: fresh.Namespace,
			Labels:          resultLabels(string(fresh.UID), "analysis"),
			OwnerReferences: []metav1.OwnerReference{agenticRunOwnerRef(&fresh)},
		},
	}
	ta.record(ta.fc.Create(ctx, cr))
	cr.Status = agenticv1alpha1.AnalysisResultStatus{
		Options:    ta.analyzeResult.Options,
		Conditions: resultConditions(&now, now, outcome),
	}
	if ta.analyzeResult.Diagnosis != nil {
		cr.Status.Diagnosis = *ta.analyzeResult.Diagnosis
	}
	if ta.analyzeResult.ActionRequired != nil {
		cr.Status.ActionRequired = agenticv1alpha1.ActionRequiredFromBool(*ta.analyzeResult.ActionRequired)
	}
	ta.record(ta.fc.Status().Update(ctx, cr))

	base := fresh.DeepCopy()
	fresh.Status.Steps.Analysis.Results = append(fresh.Status.Steps.Analysis.Results,
		agenticv1alpha1.StepResultRef{Name: crName, Outcome: outcome})

	reason := reasonComplete
	msg := fmt.Sprintf("Analysis complete with %d option(s)", len(ta.analyzeResult.Options))
	status := metav1.ConditionTrue
	if !ta.analyzeResult.Success {
		reason = reasonFailed
		msg = "Analysis failed"
		status = metav1.ConditionFalse
	} else if ta.analyzeResult.ActionRequired != nil && !*ta.analyzeResult.ActionRequired {
		reason = reasonNoActionRequired
		msg = "No action required"
	}
	meta.SetStatusCondition(&fresh.Status.Conditions, metav1.Condition{
		Type: agenticv1alpha1.AgenticRunConditionAnalyzed, Status: status, Reason: reason, Message: msg,
		ObservedGeneration: fresh.Generation,
	})
	ta.record(ta.fc.Status().Patch(ctx, &fresh, client.MergeFrom(base)))
}

func (ta *testAgentCaller) completeExecution(ctx context.Context, run *agenticv1alpha1.AgenticRun) {
	if ta.fc == nil || ta.executeResult == nil {
		return
	}
	var fresh agenticv1alpha1.AgenticRun
	if !ta.record(ta.fc.Get(ctx, client.ObjectKeyFromObject(run), &fresh)) {
		return
	}

	now := metav1.Now()
	outcome := agenticv1alpha1.ActionOutcomeFromBool(ta.executeResult.Success)
	idx := len(fresh.Status.Steps.Execution.Results)
	crName := resultCRName(fresh.Name, "execution", idx)
	cr := &agenticv1alpha1.ExecutionResult{
		ObjectMeta: metav1.ObjectMeta{
			Name: crName, Namespace: fresh.Namespace,
			Labels:          resultLabels(string(fresh.UID), "execution"),
			OwnerReferences: []metav1.OwnerReference{agenticRunOwnerRef(&fresh)},
		},
	}
	ta.record(ta.fc.Create(ctx, cr))
	cr.Status = agenticv1alpha1.ExecutionResultStatus{
		ActionsTaken: ta.executeResult.ActionsTaken,
		Conditions:   resultConditions(&now, now, outcome),
	}
	ta.record(ta.fc.Status().Update(ctx, cr))

	base := fresh.DeepCopy()
	fresh.Status.Steps.Execution.Results = append(fresh.Status.Steps.Execution.Results,
		agenticv1alpha1.StepResultRef{Name: crName, Outcome: outcome})

	if ta.executeResult.Success || hasMutationSuccess(ta.executeResult.ActionsTaken) {
		meta.SetStatusCondition(&fresh.Status.Conditions, metav1.Condition{
			Type: agenticv1alpha1.AgenticRunConditionExecuted, Status: metav1.ConditionTrue, Reason: reasonComplete, Message: "Execution completed",
			ObservedGeneration: fresh.Generation,
		})
	} else {
		meta.SetStatusCondition(&fresh.Status.Conditions, metav1.Condition{
			Type: agenticv1alpha1.AgenticRunConditionExecuted, Status: metav1.ConditionFalse, Reason: reasonFailed, Message: executionFailureMessage(ta.executeResult),
			ObservedGeneration: fresh.Generation,
		})
	}
	ta.record(ta.fc.Status().Patch(ctx, &fresh, client.MergeFrom(base)))
}

func (ta *testAgentCaller) completeVerification(ctx context.Context, run *agenticv1alpha1.AgenticRun) {
	if ta.fc == nil || ta.verifyResult == nil {
		return
	}
	var fresh agenticv1alpha1.AgenticRun
	if !ta.record(ta.fc.Get(ctx, client.ObjectKeyFromObject(run), &fresh)) {
		return
	}

	now := metav1.Now()
	outcome := agenticv1alpha1.ActionOutcomeFromBool(ta.verifyResult.Success)
	crName := resultCRName(fresh.Name, "verification", len(fresh.Status.Steps.Verification.Results))
	cr := &agenticv1alpha1.VerificationResult{
		ObjectMeta: metav1.ObjectMeta{
			Name: crName, Namespace: fresh.Namespace,
			Labels:          resultLabels(string(fresh.UID), "verification"),
			OwnerReferences: []metav1.OwnerReference{agenticRunOwnerRef(&fresh)},
		},
	}
	ta.record(ta.fc.Create(ctx, cr))
	cr.Status = agenticv1alpha1.VerificationResultStatus{
		Checks:     ta.verifyResult.Checks,
		Summary:    ta.verifyResult.Summary,
		Conditions: resultConditions(&now, now, outcome),
	}
	ta.record(ta.fc.Status().Update(ctx, cr))

	base := fresh.DeepCopy()
	fresh.Status.Steps.Verification.Results = append(fresh.Status.Steps.Verification.Results,
		agenticv1alpha1.StepResultRef{Name: crName, Outcome: outcome})

	if !ta.verifyResult.Success {
		// Mirror pod_handler.patchVerificationFailedEscalating (OLS-3817): an
		// objective verification failure escalates directly instead of
		// terminating. Verified=False/VerificationFailed plus
		// Escalated=Unknown/VerificationFailed makes DerivePhase yield Escalating.
		meta.SetStatusCondition(&fresh.Status.Conditions, metav1.Condition{
			Type: agenticv1alpha1.AgenticRunConditionVerified, Status: metav1.ConditionFalse,
			Reason: agenticv1alpha1.ReasonVerificationFailed, Message: fmt.Sprintf("Verification failed: %s", ta.verifyResult.Summary),
			ObservedGeneration: fresh.Generation,
		})
		meta.SetStatusCondition(&fresh.Status.Conditions, metav1.Condition{
			Type: agenticv1alpha1.AgenticRunConditionEscalated, Status: metav1.ConditionUnknown,
			Reason: agenticv1alpha1.ReasonVerificationFailed, Message: "Verification failed, escalating",
			ObservedGeneration: fresh.Generation,
		})
	} else {
		meta.SetStatusCondition(&fresh.Status.Conditions, metav1.Condition{
			Type: agenticv1alpha1.AgenticRunConditionVerified, Status: metav1.ConditionTrue,
			Reason: reasonPassed, Message: ta.verifyResult.Summary,
			ObservedGeneration: fresh.Generation,
		})
	}
	ta.record(ta.fc.Status().Patch(ctx, &fresh, client.MergeFrom(base)))
}

func (ta *testAgentCaller) completeEscalation(ctx context.Context, run *agenticv1alpha1.AgenticRun) {
	if ta.fc == nil || ta.escalateResult == nil {
		return
	}
	var fresh agenticv1alpha1.AgenticRun
	if !ta.record(ta.fc.Get(ctx, client.ObjectKeyFromObject(run), &fresh)) {
		return
	}

	now := metav1.Now()
	outcome := agenticv1alpha1.ActionOutcomeFromBool(ta.escalateResult.Success)
	crName := resultCRName(fresh.Name, "escalation", len(fresh.Status.Steps.Escalation.Results))
	cr := &agenticv1alpha1.EscalationResult{
		ObjectMeta: metav1.ObjectMeta{
			Name: crName, Namespace: fresh.Namespace,
			Labels:          resultLabels(string(fresh.UID), "escalation"),
			OwnerReferences: []metav1.OwnerReference{agenticRunOwnerRef(&fresh)},
		},
	}
	ta.record(ta.fc.Create(ctx, cr))
	cr.Status = agenticv1alpha1.EscalationResultStatus{
		Summary:    ta.escalateResult.Summary,
		Content:    ta.escalateResult.Content,
		Conditions: resultConditions(&now, now, outcome),
	}
	ta.record(ta.fc.Status().Update(ctx, cr))

	base := fresh.DeepCopy()
	fresh.Status.Steps.Escalation.Results = append(fresh.Status.Steps.Escalation.Results,
		agenticv1alpha1.StepResultRef{Name: crName, Outcome: outcome})
	meta.SetStatusCondition(&fresh.Status.Conditions, metav1.Condition{
		Type: agenticv1alpha1.AgenticRunConditionEscalated, Status: metav1.ConditionTrue, Reason: reasonComplete, Message: ta.escalateResult.Summary,
		ObservedGeneration: fresh.Generation,
	})
	ta.record(ta.fc.Status().Patch(ctx, &fresh, client.MergeFrom(base)))
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
	return &agenticv1alpha1.ApprovalPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster"},
		Spec: agenticv1alpha1.ApprovalPolicySpec{
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

func mustReconcile(t *testing.T, r *AgenticRunReconciler, name string) {
	t.Helper()
	if _, err := reconcileOnce(r, name); err != nil {
		t.Fatalf("reconcile %q: %v", name, err)
	}
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
			"apiVersion": "extensions.agents.x-k8s.io/v1beta1",
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

	r := &AgenticRunReconciler{Client: fc, Agent: newTestAgentCaller().withClient(t, fc, "default"), Namespace: "default"}

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
	r := &AgenticRunReconciler{Client: fc, Agent: newTestAgentCaller().withClient(t, fc, "default"), Namespace: "default"}

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
	r := &AgenticRunReconciler{Client: fc, Agent: newTestAgentCaller().withClient(t, fc, "default"), Namespace: "default", TempLog: cleaner}

	mustReconcile(t, r, "fix-crash")

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
	r := &AgenticRunReconciler{Client: fc, Agent: newTestAgentCaller().withClient(t, fc, "default"), Namespace: "default", TempLog: cleaner}

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
	r := &AgenticRunReconciler{Client: fc, Agent: newTestAgentCaller().withClient(t, fc, "default"), Namespace: "default", TempLog: cleaner}

	mustReconcile(t, r, "fix-crash")

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
	r := &AgenticRunReconciler{Client: fc, Agent: newTestAgentCaller().withClient(t, fc, "default"), Namespace: "default", TempLog: cleaner}

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

	r := &AgenticRunReconciler{Client: fc, Agent: newTestAgentCaller().withClient(t, fc, "default"), Namespace: "default"}
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

func TestDeletion_BothFinalizersInOneReconcile(t *testing.T) {
	now := metav1.Now()
	run := testAgenticRun()
	run.UID = "a1b2c3d4-e5f6-7890-abcd-ef1234567890"
	run.DeletionTimestamp = &now
	run.Finalizers = []string{rbacCleanupFinalizer, templogCleanupFinalizer}

	objs := append([]client.Object{run}, defaultObjects()...)
	fc := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(objs...).
		WithStatusSubresource(run).Build()

	cleaner := &mockTempLogCleaner{}
	r := &AgenticRunReconciler{Client: fc, Agent: newTestAgentCaller().withClient(t, fc, "default"), Namespace: "default", TempLog: cleaner}

	result, err := reconcileOnce(r, "fix-crash")
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if result.Requeue || result.RequeueAfter != 0 {
		t.Errorf("expected no requeue, got %+v", result)
	}
	if cleaner.callCount != 1 {
		t.Errorf("DeleteLogs called %d times, want 1", cleaner.callCount)
	}

	var updated agenticv1alpha1.AgenticRun
	err = fc.Get(context.Background(), types.NamespacedName{Name: "fix-crash", Namespace: "default"}, &updated)
	if client.IgnoreNotFound(err) != nil {
		t.Fatalf("get after reconcile: %v", err)
	}
	if err == nil {
		if controllerutil.ContainsFinalizer(&updated, rbacCleanupFinalizer) {
			t.Error("rbac finalizer should be removed")
		}
		if controllerutil.ContainsFinalizer(&updated, templogCleanupFinalizer) {
			t.Error("templog finalizer should be removed")
		}
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
