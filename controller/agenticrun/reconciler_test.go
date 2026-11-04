package agenticrun

import (
	"context"
	"fmt"
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
	"testing"
	"time"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
	"github.com/openshift/lightspeed-agentic-operator/pkg/configuration"
)

func TestPreserveFailedSandbox(t *testing.T) {
	tests := []struct {
		name        string
		annotations map[string]string
		want        bool
	}{
		{name: "absent", want: false},
		{name: "true", annotations: map[string]string{preserveSandboxAnnotation: "true"}, want: true},
		{name: "case insensitive", annotations: map[string]string{preserveSandboxAnnotation: " TRUE "}, want: true},
		{name: "false", annotations: map[string]string{preserveSandboxAnnotation: "false"}, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			run := &agenticv1alpha1.AgenticRun{ObjectMeta: metav1.ObjectMeta{Annotations: tt.annotations}}
			if got := preserveFailedSandbox(run); got != tt.want {
				t.Fatalf("preserveFailedSandbox() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestReadRunnableRunRejectsReplacementUID(t *testing.T) {
	run := testAgenticRun()
	run.UID = types.UID("current-run-uid")
	reader := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(run).Build()

	_, err := readRunnableRun(context.Background(), reader, types.NamespacedName{
		Name: run.Name, Namespace: run.Namespace,
	}, types.UID("previous-run-uid"))
	if err != errStopAccepted {
		t.Fatalf("readRunnableRun() error = %v, want %v", err, errStopAccepted)
	}
}

func TestReadRunnableRunPreservesAcceptedStopAfterResumptionOrConfigDeletion(t *testing.T) {
	tests := []struct {
		name          string
		conditionType string
		status        metav1.ConditionStatus
		reason        string
		configExists  bool
		wantStopped   bool
	}{
		{
			name:          "ongoing stop after config deletion",
			conditionType: agenticv1alpha1.AgenticRunConditionStopping,
			status:        metav1.ConditionTrue,
			reason:        reasonSystemSuspended,
			wantStopped:   true,
		},
		{
			name:          "completed stop after resumption",
			conditionType: agenticv1alpha1.AgenticRunConditionStopping,
			status:        metav1.ConditionFalse,
			reason:        reasonTeardownComplete,
			configExists:  true,
			wantStopped:   true,
		},
		{
			name:          "failed teardown after config deletion",
			conditionType: agenticv1alpha1.AgenticRunConditionStopping,
			status:        metav1.ConditionFalse,
			reason:        reasonTeardownFailed,
			wantStopped:   true,
		},
		{
			name:          "emergency-stopped run after resumption",
			conditionType: agenticv1alpha1.AgenticRunConditionEmergencyStopped,
			status:        metav1.ConditionTrue,
			reason:        reasonSystemSuspended,
			configExists:  true,
			wantStopped:   true,
		},
		{
			name:          "emergency-stop failure after config deletion",
			conditionType: agenticv1alpha1.AgenticRunConditionAnalyzed,
			status:        metav1.ConditionFalse,
			reason:        reasonEmergencyStopFailed,
			wantStopped:   true,
		},
		{
			name:          "ordinary workflow failure after resumption",
			conditionType: agenticv1alpha1.AgenticRunConditionAnalyzed,
			status:        metav1.ConditionFalse,
			reason:        reasonFailed,
			configExists:  true,
		},
		{
			name:          "ordinary workflow failure after config deletion",
			conditionType: agenticv1alpha1.AgenticRunConditionAnalyzed,
			status:        metav1.ConditionFalse,
			reason:        reasonFailed,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			run := testAgenticRun()
			run.UID = types.UID("run-uid")
			run.Generation = 5
			run.Status.Conditions = []metav1.Condition{{
				Type:               tt.conditionType,
				Status:             tt.status,
				Reason:             tt.reason,
				ObservedGeneration: 4,
			}}
			objects := []client.Object{run}
			if tt.configExists {
				objects = append(objects, &agenticv1alpha1.AgenticOLSConfig{
					ObjectMeta: metav1.ObjectMeta{Name: "cluster"},
					Spec:       agenticv1alpha1.AgenticOLSConfigSpec{Suspended: false},
				})
			}
			reader := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(objects...).Build()

			got, err := readRunnableRun(context.Background(), reader, types.NamespacedName{
				Name: run.Name, Namespace: run.Namespace,
			}, run.UID)
			if tt.wantStopped {
				if err != errStopAccepted || got != nil {
					t.Fatalf("readRunnableRun() = (%v, %v), want accepted-stop rejection", got, err)
				}
				return
			}
			if err != nil || got == nil || got.UID != run.UID {
				t.Fatalf("readRunnableRun() = (%v, %v), want ordinary failed run to remain readable", got, err)
			}
		})
	}
}

func TestReadRunnableRunRejectsSuspendedConfig(t *testing.T) {
	run := testAgenticRun()
	run.UID = types.UID("run-uid")
	config := &agenticv1alpha1.AgenticOLSConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster"},
		Spec:       agenticv1alpha1.AgenticOLSConfigSpec{Suspended: true},
	}
	reader := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(run, config).Build()

	_, err := readRunnableRun(context.Background(), reader, types.NamespacedName{
		Name: run.Name, Namespace: run.Namespace,
	}, run.UID)
	if err != errStopAccepted {
		t.Fatalf("readRunnableRun() error = %v, want suspended-config guard %v", err, errStopAccepted)
	}
}

func TestReconcilePreservesFailedSandbox(t *testing.T) {
	tests := []struct {
		name        string
		annotations map[string]string
		wantRelease int
	}{
		{name: "preserved", annotations: map[string]string{preserveSandboxAnnotation: "true"}, wantRelease: 0},
		{name: "not preserved", wantRelease: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			run := testAgenticRun()
			run.Annotations = tt.annotations
			run.Status.Steps.Analysis.Sandbox.ClaimName = "analysis-sandbox"
			run.Status.Conditions = []metav1.Condition{{
				Type:   agenticv1alpha1.AgenticRunConditionAnalyzed,
				Status: metav1.ConditionFalse,
				Reason: reasonFailed,
			}}
			caller := newTestAgentCaller()
			r := ttlTestReconciler(t, run, ttlTestCache(t, "14"))
			r.Agent = caller

			if _, err := reconcileOnce(r, run.Name); err != nil {
				t.Fatalf("reconcile: %v", err)
			}
			if caller.releaseAllCount != tt.wantRelease {
				t.Fatalf("ReleaseSandboxes called %d times, want %d", caller.releaseAllCount, tt.wantRelease)
			}
		})
	}
}

// --- Configurable agent stub for tests ---

type testAgentCaller struct {
	analyzeErr    error
	executeErr    error
	verifyErr     error
	escalateErr   error
	escalateCalls int

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
	ta.escalateCalls++
	if ta.escalateErr != nil {
		return ta.escalateErr
	}
	ta.completeEscalation(ctx, run)
	return nil
}

func (ta *testAgentCaller) ReleaseSandboxes(_ context.Context, _ *agenticv1alpha1.AgenticRun, _ bool) error {
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
			Request:      "Pod crashing in production",
			Tools:        testTools(),
			Analysis:     agenticv1alpha1.AgenticRunStep{Agent: "default"},
			Execution:    agenticv1alpha1.AgenticRunStep{Agent: "default"},
			Verification: agenticv1alpha1.AgenticRunStep{Agent: "default"},
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
		testDefaultAgent(), testLLM("smart"), &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "llm-secret", Namespace: "default"}}, testAutoApprovePolicy(), testReaderClusterRoleBinding(),
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
	// Most tests exercise workflow behavior with an available ConfigMap.
	// Missing-config tests call Reconcile directly instead.
	if r.Config == nil {
		r.Config = &configuration.Cache{}
		if err := r.Config.OnConfigMapChange(context.Background(), &corev1.ConfigMap{}); err != nil {
			return ctrl.Result{}, err
		}
	}
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

	r := &AgenticRunReconciler{Client: fc, APIReader: fc, Agent: newTestAgentCaller().withClient(t, fc, "default"), Namespace: "default"}

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
	r := &AgenticRunReconciler{Client: fc, APIReader: fc, Agent: newTestAgentCaller().withClient(t, fc, "default"), Namespace: "default"}

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

func suspendedConfigAt(start time.Time, generation int64) *agenticv1alpha1.AgenticOLSConfig {
	return &agenticv1alpha1.AgenticOLSConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster", Generation: generation},
		Spec:       agenticv1alpha1.AgenticOLSConfigSpec{Suspended: true},
		Status: agenticv1alpha1.AgenticOLSConfigStatus{
			Conditions: []metav1.Condition{{
				Type:               agenticv1alpha1.AgenticOLSConfigConditionSuspended,
				Status:             metav1.ConditionTrue,
				Reason:             "Stopping",
				ObservedGeneration: generation,
				LastTransitionTime: metav1.NewTime(start),
			}},
		},
	}
}

type terminalAuditCounter struct {
	NoOpAuditLogger
	terminalSpans int
}

func (a *terminalAuditCounter) EmitTerminalSpan(context.Context, *agenticv1alpha1.AgenticRun, string, string) {
	a.terminalSpans++
}

func TestStopAcceptedAgentErrorRequeuesWithoutFailure(t *testing.T) {
	run := testAgenticRun()
	fc := fake.NewClientBuilder().
		WithScheme(testScheme()).
		WithObjects(append(defaultObjects(), run)...).
		WithStatusSubresource(run, &agenticv1alpha1.AnalysisResult{}, &agenticv1alpha1.ExecutionResult{}, &agenticv1alpha1.VerificationResult{}, &agenticv1alpha1.EscalationResult{}).
		Build()
	agent := newTestAgentCaller()
	agent.analyzeErr = errStopAccepted
	r := &AgenticRunReconciler{
		Client:    fc,
		APIReader: fc,
		Agent:     agent.withClient(t, fc, "default"),
		Namespace: "default",
	}

	result, err := reconcileOnce(r, run.Name)
	if err != nil {
		t.Fatalf("Reconcile accepted stop error: %v", err)
	}
	if !result.Requeue {
		t.Fatalf("accepted stop result = %+v, want immediate stop reconciliation", result)
	}
	got, err := getAgenticRun(r, run.Name)
	if err != nil {
		t.Fatal(err)
	}
	analyzed := meta.FindStatusCondition(got.Status.Conditions, agenticv1alpha1.AgenticRunConditionAnalyzed)
	if analyzed == nil || analyzed.Status != metav1.ConditionUnknown {
		t.Fatalf("accepted stop was converted into a failed or completed analysis: %+v", analyzed)
	}
	if len(got.Status.Steps.Analysis.Results) != 0 {
		t.Fatalf("accepted stop synthesized a failure result: %+v", got.Status.Steps.Analysis.Results)
	}
}

func TestFailStepDoesNotCreateResultForAcceptedStop(t *testing.T) {
	ctx := context.Background()
	run := testAgenticRun()
	run.UID = "stopped-fail-step"
	run.Status.Conditions = []metav1.Condition{
		{
			Type:   agenticv1alpha1.AgenticRunConditionAnalyzed,
			Status: metav1.ConditionUnknown,
			Reason: reasonInProgress,
		},
		{
			Type:               agenticv1alpha1.AgenticRunConditionStopping,
			Status:             metav1.ConditionTrue,
			Reason:             reasonSystemSuspended,
			LastTransitionTime: metav1.NewTime(time.Unix(100, 0).UTC()),
			ObservedGeneration: run.Generation,
		},
	}
	fc := fake.NewClientBuilder().WithScheme(testScheme()).
		WithObjects(run).
		WithStatusSubresource(run, &agenticv1alpha1.AnalysisResult{}).
		Build()
	r := &AgenticRunReconciler{Client: fc, APIReader: fc}

	result, err := r.failStep(ctx, run, agenticv1alpha1.AgenticRunConditionAnalyzed, fmt.Errorf("late LLM failure"))
	if err != nil {
		t.Fatalf("failStep accepted stop: %v", err)
	}
	if !result.Requeue {
		t.Fatalf("failStep result = %+v, want stop reconciliation", result)
	}
	var results agenticv1alpha1.AnalysisResultList
	if err := fc.List(ctx, &results); err != nil {
		t.Fatal(err)
	}
	if len(results.Items) != 0 {
		t.Fatalf("accepted stop synthesized AnalysisResults: %v", results.Items)
	}
	got, err := getAgenticRun(r, run.Name)
	if err != nil {
		t.Fatal(err)
	}
	analyzed := meta.FindStatusCondition(got.Status.Conditions, agenticv1alpha1.AgenticRunConditionAnalyzed)
	if analyzed == nil || analyzed.Status != metav1.ConditionUnknown {
		t.Fatalf("accepted stop changed workflow failure condition: %+v", analyzed)
	}
}

func TestReconcileSuspension(t *testing.T) {
	start := time.Unix(1_000_000, 0).UTC()
	for _, tt := range []struct {
		name       string
		conditions []metav1.Condition
		wantPhase  agenticv1alpha1.AgenticRunPhase
	}{
		{name: "pending without workloads", wantPhase: agenticv1alpha1.AgenticRunPhaseEmergencyStopped},
		{
			name: "proposed without workloads",
			conditions: []metav1.Condition{{
				Type:   agenticv1alpha1.AgenticRunConditionAnalyzed,
				Status: metav1.ConditionTrue,
				Reason: "AnalysisComplete",
			}},
			wantPhase: agenticv1alpha1.AgenticRunPhaseEmergencyStopped,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			run := testAgenticRun()
			run.UID = types.UID("suspension-run")
			run.Status.Conditions = tt.conditions
			config := suspendedConfigAt(start, 4)
			objects := append(defaultObjects(), run, config)
			fc := fake.NewClientBuilder().
				WithScheme(suspensionTestScheme()).
				WithObjects(objects...).
				WithStatusSubresource(&agenticv1alpha1.AgenticRun{}).
				Build()
			cache := testCache(t, "bare-pod")
			manager := NewSandboxManager(fc, fc, cache, "default", nil)
			r := &AgenticRunReconciler{
				Client:    fc,
				APIReader: fc,
				Agent:     newTestAgentCaller().withClient(t, fc, "default"),
				Sandbox:   manager,
				Config:    cache,
				Namespace: "default",
				Now:       func() time.Time { return start.Add(time.Second) },
			}

			result, err := reconcileOnce(r, run.Name)
			if err != nil {
				t.Fatalf("Reconcile: %v", err)
			}
			if result.Requeue || result.RequeueAfter != 0 {
				t.Fatalf("confirmed stop unexpectedly requeued: %+v", result)
			}
			got, err := getAgenticRun(r, run.Name)
			if err != nil {
				t.Fatal(err)
			}
			if phase := agenticv1alpha1.DerivePhase(got.Status.Conditions); phase != tt.wantPhase {
				t.Fatalf("phase = %s, want %s", phase, tt.wantPhase)
			}
			stopping := meta.FindStatusCondition(got.Status.Conditions, agenticv1alpha1.AgenticRunConditionStopping)
			emergency := meta.FindStatusCondition(got.Status.Conditions, agenticv1alpha1.AgenticRunConditionEmergencyStopped)
			if stopping == nil || stopping.Status != metav1.ConditionFalse || stopping.Reason != reasonTeardownComplete {
				t.Fatalf("Stopping = %+v, want confirmed completion", stopping)
			}
			if emergency == nil || emergency.Status != metav1.ConditionTrue || emergency.Reason != reasonSystemSuspended {
				t.Fatalf("EmergencyStopped = %+v, want successful suspension outcome", emergency)
			}
			if hasRunSandboxInfo(got) {
				t.Fatalf("confirmed cleanup retained sandbox references: %+v", got.Status.Steps)
			}
		})
	}

	t.Run("suspended config without current activation blocks", func(t *testing.T) {
		run := testAgenticRun()
		run.UID = "pending-run"
		config := &agenticv1alpha1.AgenticOLSConfig{
			ObjectMeta: metav1.ObjectMeta{Name: "cluster", Generation: 5},
			Spec:       agenticv1alpha1.AgenticOLSConfigSpec{Suspended: true},
		}
		fc := fake.NewClientBuilder().
			WithScheme(suspensionTestScheme()).
			WithObjects(append(defaultObjects(), run, config)...).
			WithStatusSubresource(&agenticv1alpha1.AgenticRun{}).
			Build()
		cache := testCache(t, "bare-pod")
		r := &AgenticRunReconciler{
			Client:    fc,
			APIReader: fc,
			Agent:     newTestAgentCaller().withClient(t, fc, "default"),
			Sandbox:   NewSandboxManager(fc, fc, cache, "default", nil),
			Config:    cache,
			Namespace: "default",
			Now:       func() time.Time { return start },
		}
		result, err := reconcileOnce(r, run.Name)
		if err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		if result.RequeueAfter != suspensionRequeueAfter {
			t.Fatalf("requeue = %s, want %s", result.RequeueAfter, suspensionRequeueAfter)
		}
		got, err := getAgenticRun(r, run.Name)
		if err != nil {
			t.Fatal(err)
		}
		if len(got.Status.Conditions) != 0 {
			t.Fatalf("run advanced without a current activation: %+v", got.Status.Conditions)
		}
	})

	t.Run("not suspended proceeds normally", func(t *testing.T) {
		run := testAgenticRun()
		config := &agenticv1alpha1.AgenticOLSConfig{
			ObjectMeta: metav1.ObjectMeta{Name: "cluster"},
			Spec:       agenticv1alpha1.AgenticOLSConfigSpec{Suspended: false},
		}
		fc := fake.NewClientBuilder().
			WithScheme(testScheme()).
			WithObjects(append(defaultObjects(), run, config)...).
			WithStatusSubresource(run, &agenticv1alpha1.AnalysisResult{}, &agenticv1alpha1.ExecutionResult{}, &agenticv1alpha1.VerificationResult{}, &agenticv1alpha1.EscalationResult{}).
			Build()
		r := &AgenticRunReconciler{Client: fc, APIReader: fc, Agent: newTestAgentCaller().withClient(t, fc, "default"), Namespace: "default"}
		if _, err := reconcileOnce(r, run.Name); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		got, err := getAgenticRun(r, run.Name)
		if err != nil {
			t.Fatal(err)
		}
		if phase := agenticv1alpha1.DerivePhase(got.Status.Conditions); phase == agenticv1alpha1.AgenticRunPhaseEmergencyStopped {
			t.Fatal("run was emergency-stopped while config was active")
		}
	})

	t.Run("no config proceeds normally", func(t *testing.T) {
		run := testAgenticRun()
		fc := fake.NewClientBuilder().
			WithScheme(testScheme()).
			WithObjects(append(defaultObjects(), run)...).
			WithStatusSubresource(run, &agenticv1alpha1.AnalysisResult{}, &agenticv1alpha1.ExecutionResult{}, &agenticv1alpha1.VerificationResult{}, &agenticv1alpha1.EscalationResult{}).
			Build()
		r := &AgenticRunReconciler{Client: fc, APIReader: fc, Agent: newTestAgentCaller().withClient(t, fc, "default"), Namespace: "default"}
		if _, err := reconcileOnce(r, run.Name); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		got, err := getAgenticRun(r, run.Name)
		if err != nil {
			t.Fatal(err)
		}
		if phase := agenticv1alpha1.DerivePhase(got.Status.Conditions); phase == agenticv1alpha1.AgenticRunPhaseEmergencyStopped {
			t.Fatal("run was emergency-stopped without a config")
		}
	})

	t.Run("accepted terminal run remains stopped after config deletion", func(t *testing.T) {
		run := testAgenticRun()
		run.UID = "already-stopped"
		run.Status.Conditions = []metav1.Condition{{
			Type:               agenticv1alpha1.AgenticRunConditionEmergencyStopped,
			Status:             metav1.ConditionTrue,
			Reason:             reasonSystemSuspended,
			ObservedGeneration: run.Generation,
		}}
		fc := fake.NewClientBuilder().
			WithScheme(suspensionTestScheme()).
			WithObjects(append(defaultObjects(), run)...).
			WithStatusSubresource(&agenticv1alpha1.AgenticRun{}).
			Build()
		cache := testCache(t, "bare-pod")
		r := &AgenticRunReconciler{
			Client:    fc,
			APIReader: fc,
			Agent:     newTestAgentCaller().withClient(t, fc, "default"),
			Sandbox:   NewSandboxManager(fc, fc, cache, "default", nil),
			Config:    cache,
			Namespace: "default",
		}
		if _, err := reconcileOnce(r, run.Name); err != nil {
			t.Fatalf("Reconcile accepted terminal run: %v", err)
		}
		got, err := getAgenticRun(r, run.Name)
		if err != nil {
			t.Fatal(err)
		}
		if phase := agenticv1alpha1.DerivePhase(got.Status.Conditions); phase != agenticv1alpha1.AgenticRunPhaseEmergencyStopped {
			t.Fatalf("phase after config deletion = %s, want EmergencyStopped", phase)
		}
	})
}

func TestAcceptedStopBlocksRevisionAfterConfigDeletion(t *testing.T) {
	run := testAgenticRun()
	run.UID = "accepted-stop-revision"
	run.Generation = 5
	run.Spec.RevisionFeedback = "continue from the last result"
	run.Status.Conditions = []metav1.Condition{
		{
			Type:               agenticv1alpha1.AgenticRunConditionStopping,
			Status:             metav1.ConditionFalse,
			Reason:             reasonTeardownFailed,
			ObservedGeneration: 4,
		},
		{
			Type:               agenticv1alpha1.AgenticRunConditionAnalyzed,
			Status:             metav1.ConditionFalse,
			Reason:             reasonEmergencyStopFailed,
			ObservedGeneration: 4,
		},
	}
	run.Status.Steps.Analysis.Sandbox = agenticv1alpha1.SandboxInfo{
		ClaimName: "stale-analysis-reference",
		Namespace: "default",
	}
	fc := fake.NewClientBuilder().
		WithScheme(suspensionTestScheme()).
		WithObjects(append(defaultObjects(), run)...).
		WithStatusSubresource(&agenticv1alpha1.AgenticRun{}).
		Build()
	cache := testCache(t, "bare-pod")
	agent := newTestAgentCaller().withClient(t, fc, "default")
	r := &AgenticRunReconciler{
		Client:    fc,
		APIReader: fc,
		Agent:     agent,
		Sandbox:   NewSandboxManager(fc, fc, cache, "default", nil),
		Config:    cache,
		Namespace: "default",
	}

	if _, err := reconcileOnce(r, run.Name); err != nil {
		t.Fatalf("Reconcile accepted run after config deletion: %v", err)
	}
	got, err := getAgenticRun(r, run.Name)
	if err != nil {
		t.Fatal(err)
	}
	if phase := agenticv1alpha1.DerivePhase(got.Status.Conditions); phase != agenticv1alpha1.AgenticRunPhaseFailed {
		t.Fatalf("phase after revision feedback = %s, want final Failed", phase)
	}
	stopping := meta.FindStatusCondition(got.Status.Conditions, agenticv1alpha1.AgenticRunConditionStopping)
	analyzed := meta.FindStatusCondition(got.Status.Conditions, agenticv1alpha1.AgenticRunConditionAnalyzed)
	if stopping == nil || stopping.Reason != reasonTeardownFailed || analyzed == nil ||
		analyzed.Status != metav1.ConditionFalse || analyzed.Reason != reasonEmergencyStopFailed {
		t.Fatalf("revision changed accepted stop outcome: %+v", got.Status.Conditions)
	}
	if meta.FindStatusCondition(got.Status.Conditions, agenticv1alpha1.AgenticRunConditionAnalyzed).Status == metav1.ConditionUnknown {
		t.Fatal("accepted run restarted analysis after config deletion")
	}
	if hasRunSandboxInfo(got) {
		t.Fatalf("confirmed background cleanup retained sandbox references: %+v", got.Status.Steps)
	}
}

func TestSuspensionDeadlineSurvivesRestartAndFailureStaysFinal(t *testing.T) {
	ctx := context.Background()
	start := time.Unix(1_000_000, 0).UTC()
	run := testSMRun()
	run.Status.Steps.Analysis.Sandbox = agenticv1alpha1.SandboxInfo{
		ClaimName: "ls-analysis-abc-123",
		Namespace: "test-ns",
	}
	run.Annotations = map[string]string{preserveSandboxAnnotation: "true"}
	pod := suspensionManagedPod("analysis-pod", "analysis-pod-uid", run.UID, "analysis", corev1.PodRunning)
	pod.Finalizers = []string{"test/hold"}
	config := suspendedConfigAt(start, 8)
	fc := fake.NewClientBuilder().
		WithScheme(suspensionTestScheme()).
		WithObjects(append(defaultObjects(), run, config, pod)...).
		WithStatusSubresource(&agenticv1alpha1.AgenticRun{}).
		Build()
	cache := testCache(t, "bare-pod")
	audit := &terminalAuditCounter{}
	newReconciler := func(now time.Time) *AgenticRunReconciler {
		return &AgenticRunReconciler{
			Client:    fc,
			APIReader: fc,
			Agent:     newTestAgentCaller().withClient(t, fc, "test-ns"),
			Sandbox:   newTestSandboxManager(fc, cache),
			Config:    cache,
			Namespace: "test-ns",
			Audit:     audit,
			Now:       func() time.Time { return now },
		}
	}
	reconcileRun := func(r *AgenticRunReconciler) (ctrl.Result, error) {
		return r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(run)})
	}
	getRun := func(r *AgenticRunReconciler) (*agenticv1alpha1.AgenticRun, error) {
		current := &agenticv1alpha1.AgenticRun{}
		if err := r.Get(ctx, client.ObjectKeyFromObject(run), current); err != nil {
			return nil, err
		}
		return current, nil
	}

	first := newReconciler(start.Add(SuspensionStopWindow - time.Second))
	result, err := reconcileRun(first)
	if err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	if result.RequeueAfter != time.Second {
		t.Fatalf("first requeue = %s, want remaining activation second", result.RequeueAfter)
	}
	stoppingRun, err := getRun(first)
	if err != nil {
		t.Fatal(err)
	}
	accepted := meta.FindStatusCondition(stoppingRun.Status.Conditions, agenticv1alpha1.AgenticRunConditionStopping)
	if accepted == nil || accepted.Status != metav1.ConditionTrue ||
		!accepted.LastTransitionTime.Time.Equal(start) {
		t.Fatalf("stop acceptance = %+v, want copied activation timestamp %s", accepted, start)
	}

	if err := fc.Delete(ctx, config); err != nil {
		t.Fatalf("resume/delete config fixture: %v", err)
	}
	restarted := newReconciler(start.Add(SuspensionStopWindow))
	result, err = reconcileRun(restarted)
	if err != nil {
		t.Fatalf("reconcile at original deadline after restart: %v", err)
	}
	if result.RequeueAfter != suspensionRequeueAfter {
		t.Fatalf("post-deadline requeue = %s, want background pass %s", result.RequeueAfter, suspensionRequeueAfter)
	}
	failed, err := getRun(restarted)
	if err != nil {
		t.Fatal(err)
	}
	stopping := meta.FindStatusCondition(failed.Status.Conditions, agenticv1alpha1.AgenticRunConditionStopping)
	analyzed := meta.FindStatusCondition(failed.Status.Conditions, agenticv1alpha1.AgenticRunConditionAnalyzed)
	if stopping == nil || stopping.Status != metav1.ConditionFalse || stopping.Reason != reasonTeardownFailed {
		t.Fatalf("Stopping = %+v, want final TeardownFailed", stopping)
	}
	if analyzed == nil || analyzed.Status != metav1.ConditionFalse || analyzed.Reason != reasonEmergencyStopFailed {
		t.Fatalf("Analyzed = %+v, want EmergencyStopFailed", analyzed)
	}
	if emergency := meta.FindStatusCondition(failed.Status.Conditions, agenticv1alpha1.AgenticRunConditionEmergencyStopped); emergency != nil && emergency.Status == metav1.ConditionTrue {
		t.Fatalf("unconfirmed cleanup wrote success: %+v", emergency)
	}
	if !hasRunSandboxInfo(failed) {
		t.Fatal("deadline failure cleared unresolved sandbox references")
	}
	if failed.Status.TerminalTime == nil || failed.Status.DeleteAfter == nil {
		t.Fatalf("failed stop did not reach terminal TTL handling: %+v", failed.Status)
	}
	fixedTerminalTime := failed.Status.TerminalTime.Time
	fixedDeleteAfter := failed.Status.DeleteAfter.Time
	if audit.terminalSpans != 1 {
		t.Fatalf("terminal audit spans after stop failure = %d, want 1", audit.terminalSpans)
	}

	var remainingPod corev1.Pod
	if err := fc.Get(ctx, client.ObjectKeyFromObject(pod), &remainingPod); err != nil {
		t.Fatalf("read finalizer-held Pod: %v", err)
	}
	remainingPod.Finalizers = nil
	if err := fc.Update(ctx, &remainingPod); err != nil {
		t.Fatalf("remove test Pod finalizer: %v", err)
	}
	if err := fc.Delete(ctx, &remainingPod); err != nil && !apierrors.IsNotFound(err) {
		t.Fatalf("remove recovered Pod: %v", err)
	}

	recovered := newReconciler(start.Add(2 * SuspensionStopWindow))
	if _, err := reconcileRun(recovered); err != nil {
		t.Fatalf("reconcile after cleanup recovery: %v", err)
	}
	got, err := getRun(recovered)
	if err != nil {
		t.Fatal(err)
	}
	if phase := agenticv1alpha1.DerivePhase(got.Status.Conditions); phase != agenticv1alpha1.AgenticRunPhaseFailed {
		t.Fatalf("recovered phase = %s, want final Failed", phase)
	}
	stopping = meta.FindStatusCondition(got.Status.Conditions, agenticv1alpha1.AgenticRunConditionStopping)
	analyzed = meta.FindStatusCondition(got.Status.Conditions, agenticv1alpha1.AgenticRunConditionAnalyzed)
	if stopping == nil || stopping.Reason != reasonTeardownFailed || analyzed == nil || analyzed.Reason != reasonEmergencyStopFailed {
		t.Fatalf("recovery changed final stop failure: %+v", got.Status.Conditions)
	}
	if hasRunSandboxInfo(got) {
		t.Fatalf("confirmed recovery retained sandbox references: %+v", got.Status.Steps)
	}
	if got.Status.TerminalTime == nil || !got.Status.TerminalTime.Time.Equal(fixedTerminalTime) ||
		got.Status.DeleteAfter == nil || !got.Status.DeleteAfter.Time.Equal(fixedDeleteAfter) {
		t.Fatalf("recovery moved fixed TTL timestamps: terminalTime=%v deleteAfter=%v", got.Status.TerminalTime, got.Status.DeleteAfter)
	}
	if audit.terminalSpans != 1 {
		t.Fatalf("cleanup recovery duplicated terminal audit: spans=%d", audit.terminalSpans)
	}
}

type deadlineAdvancingReader struct {
	client.Reader
	now      *time.Time
	deadline time.Time
	lists    int
}

func (r *deadlineAdvancingReader) List(ctx context.Context, _ client.ObjectList, _ ...client.ListOption) error {
	r.lists++
	<-ctx.Done()
	*r.now = r.deadline
	return ctx.Err()
}

func TestKnownRemoteEvidenceCannotConfirmHubOnlyCleanup(t *testing.T) {
	start := time.Unix(3_500_000, 0).UTC()
	now := start.Add(time.Second)
	run := testAgenticRun()
	run.UID = "remote-stop-run"
	run.Spec.TargetCluster = "spoke-a"
	run.Status.Conditions = []metav1.Condition{{
		Type:               agenticv1alpha1.AgenticRunConditionAnalyzed,
		Status:             metav1.ConditionUnknown,
		Reason:             reasonInProgress,
		ObservedGeneration: run.Generation,
	}}
	run.Status.Steps.Analysis.Sandbox = agenticv1alpha1.SandboxInfo{
		ClaimName: "remote-analysis-claim",
		Namespace: "spoke-namespace",
	}
	config := suspendedConfigAt(start, 11)
	fc := fake.NewClientBuilder().
		WithScheme(suspensionTestScheme()).
		WithObjects(append(defaultObjects(), run, config)...).
		WithStatusSubresource(&agenticv1alpha1.AgenticRun{}).
		Build()
	cache := testCache(t, "bare-pod")
	agent := newTestAgentCaller().withClient(t, fc, "default")
	r := &AgenticRunReconciler{
		Client:    fc,
		APIReader: fc,
		Agent:     agent,
		Sandbox:   NewSandboxManager(fc, fc, cache, "default", nil),
		Config:    cache,
		Namespace: "default",
		Now:       func() time.Time { return now },
	}

	first, err := reconcileOnce(r, run.Name)
	if err != nil {
		t.Fatalf("first remote cleanup request: %v", err)
	}
	if first.RequeueAfter == 0 || agent.releaseAllCount != 1 {
		t.Fatalf("remote cleanup was not requested/requeued: result=%+v releases=%d", first, agent.releaseAllCount)
	}
	got, err := getAgenticRun(r, run.Name)
	if err != nil {
		t.Fatal(err)
	}
	if stopping := meta.FindStatusCondition(got.Status.Conditions, agenticv1alpha1.AgenticRunConditionStopping); stopping == nil || stopping.Status != metav1.ConditionTrue {
		t.Fatalf("hub-only confirmation ended remote stop: %+v", stopping)
	}
	if emergency := meta.FindStatusCondition(got.Status.Conditions, agenticv1alpha1.AgenticRunConditionEmergencyStopped); emergency != nil && emergency.Status == metav1.ConditionTrue {
		t.Fatalf("unverified remote cleanup was recorded successful: %+v", emergency)
	}

	now = start.Add(SuspensionStopWindow)
	if _, err := reconcileOnce(r, run.Name); err != nil {
		t.Fatalf("deadline remote outcome: %v", err)
	}
	if agent.releaseAllCount != 2 {
		t.Fatalf("deadline background remote release calls = %d, want 2", agent.releaseAllCount)
	}
	got, err = getAgenticRun(r, run.Name)
	if err != nil {
		t.Fatal(err)
	}
	if stopping := meta.FindStatusCondition(got.Status.Conditions, agenticv1alpha1.AgenticRunConditionStopping); stopping == nil || stopping.Reason != reasonTeardownFailed {
		t.Fatalf("remote evidence did not produce final teardown failure: %+v", stopping)
	}
	if !hasRunSandboxInfo(got) {
		t.Fatal("unverified remote sandbox reference was cleared")
	}
	if emergency := meta.FindStatusCondition(got.Status.Conditions, agenticv1alpha1.AgenticRunConditionEmergencyStopped); emergency != nil && emergency.Status == metav1.ConditionTrue {
		t.Fatalf("remote cleanup failure wrote EmergencyStopped success: %+v", emergency)
	}

	now = start.Add(SuspensionStopWindow + time.Second)
	if _, err := reconcileOnce(r, run.Name); err != nil {
		t.Fatalf("background remote cleanup request: %v", err)
	}
	if agent.releaseAllCount != 3 {
		t.Fatalf("background remote release calls = %d, want 3", agent.releaseAllCount)
	}
}

func TestStalledCleanupDoesNotBlockDeadlineStatus(t *testing.T) {
	start := time.Unix(3_000_000, 0).UTC()
	now := start.Add(SuspensionStopWindow - time.Second)
	run := testAgenticRun()
	run.UID = "stalled-cleanup-run"
	config := suspendedConfigAt(start, 10)
	fc := fake.NewClientBuilder().
		WithScheme(suspensionTestScheme()).
		WithObjects(append(defaultObjects(), run, config)...).
		WithStatusSubresource(&agenticv1alpha1.AgenticRun{}).
		Build()
	reader := &deadlineAdvancingReader{Reader: fc, now: &now, deadline: start.Add(SuspensionStopWindow)}
	cache := testCache(t, "bare-pod")
	nowCalls := 0
	nowAtDeadline := start.Add(SuspensionStopWindow)
	nowFunc := func() time.Time {
		nowCalls++
		switch nowCalls {
		case 1:
			return start.Add(time.Second)
		case 2:
			return nowAtDeadline.Add(-time.Nanosecond)
		default:
			return now
		}
	}
	r := &AgenticRunReconciler{
		Client:    fc,
		APIReader: reader,
		Agent:     newTestAgentCaller().withClient(t, fc, "default"),
		Sandbox:   NewSandboxManager(fc, reader, cache, "default", nil),
		Config:    cache,
		Namespace: "default",
		Now:       nowFunc,
	}

	result, err := reconcileOnce(r, run.Name)
	if err != nil {
		t.Fatalf("Reconcile stalled cleanup: %v", err)
	}
	if reader.lists == 0 {
		t.Fatal("cleanup inventory was not attempted")
	}
	if !now.Equal(start.Add(SuspensionStopWindow)) {
		t.Fatalf("injected clock = %s, want original activation deadline", now)
	}
	if result.RequeueAfter != suspensionRequeueAfter {
		t.Fatalf("deadline result = %+v, want bounded background requeue", result)
	}
	got, err := getAgenticRun(r, run.Name)
	if err != nil {
		t.Fatal(err)
	}
	stopping := meta.FindStatusCondition(got.Status.Conditions, agenticv1alpha1.AgenticRunConditionStopping)
	analyzed := meta.FindStatusCondition(got.Status.Conditions, agenticv1alpha1.AgenticRunConditionAnalyzed)
	if stopping == nil || stopping.Status != metav1.ConditionFalse || stopping.Reason != reasonTeardownFailed ||
		analyzed == nil || analyzed.Status != metav1.ConditionFalse || analyzed.Reason != reasonEmergencyStopFailed {
		t.Fatalf("live parent context did not persist deadline outcome: %+v", got.Status.Conditions)
	}
}

func TestSuspensionDiscoversUnreferencedPodAndPreservesHistoricalTTL(t *testing.T) {
	ctx := context.Background()
	start := time.Unix(2_000_000, 0).UTC()
	terminalTime := metav1.NewTime(start.Add(-24 * time.Hour))
	deleteAfter := metav1.NewTime(start.Add(14 * 24 * time.Hour))
	run := testSMRun()
	run.Status.Conditions = []metav1.Condition{{
		Type:   agenticv1alpha1.AgenticRunConditionVerified,
		Status: metav1.ConditionTrue,
		Reason: "Complete",
	}}
	run.Status.TerminalTime = &terminalTime
	run.Status.DeleteAfter = &deleteAfter
	run.Labels = map[string]string{terminalTTLLabel: "true"}
	pod := suspensionManagedPod("late-managed-pod", "managed-pod-uid", run.UID, "analysis", corev1.PodRunning)
	config := suspendedConfigAt(start, 9)
	fc := fake.NewClientBuilder().
		WithScheme(suspensionTestScheme()).
		WithObjects(append(defaultObjects(), run, config, pod)...).
		WithStatusSubresource(&agenticv1alpha1.AgenticRun{}).
		Build()
	recorder := &podDeleteOptionsClient{Client: fc}
	cache := testCache(t, "bare-pod")
	audit := &terminalAuditCounter{}
	manager := NewSandboxManager(recorder, fc, cache, "test-ns", nil)
	r := &AgenticRunReconciler{
		Client:    recorder,
		APIReader: fc,
		Sandbox:   manager,
		Config:    cache,
		Namespace: "test-ns",
		Audit:     audit,
		Now:       func() time.Time { return start.Add(time.Second) },
	}
	result, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(run)})
	if err != nil {
		t.Fatalf("Reconcile historical terminal run: %v", err)
	}
	if result.Requeue || result.RequeueAfter != 0 {
		t.Fatalf("confirmed historical cleanup unexpectedly requeued: %+v", result)
	}

	var deleted corev1.Pod
	if err := fc.Get(ctx, client.ObjectKeyFromObject(pod), &deleted); !apierrors.IsNotFound(err) {
		t.Fatalf("unreferenced managed Pod remains after suspension cleanup: err=%v", err)
	}
	if len(recorder.podDeletes) != 1 || recorder.podDeletes[0].grace == nil || *recorder.podDeletes[0].grace != 0 {
		t.Fatalf("suspension Pod deletes = %+v, want one zero-grace request", recorder.podDeletes)
	}
	got := &agenticv1alpha1.AgenticRun{}
	if err := fc.Get(ctx, client.ObjectKeyFromObject(run), got); err != nil {
		t.Fatal(err)
	}
	if phase := agenticv1alpha1.DerivePhase(got.Status.Conditions); phase != agenticv1alpha1.AgenticRunPhaseCompleted {
		t.Fatalf("historical phase = %s, want Completed", phase)
	}
	if meta.FindStatusCondition(got.Status.Conditions, agenticv1alpha1.AgenticRunConditionStopping) != nil ||
		meta.FindStatusCondition(got.Status.Conditions, agenticv1alpha1.AgenticRunConditionEmergencyStopped) != nil {
		t.Fatalf("suspension rewrote historical terminal conditions: %+v", got.Status.Conditions)
	}
	if got.Status.TerminalTime == nil || !got.Status.TerminalTime.Time.Equal(terminalTime.Time) ||
		got.Status.DeleteAfter == nil || !got.Status.DeleteAfter.Time.Equal(deleteAfter.Time) {
		t.Fatalf("historical TTL moved: terminalTime=%v deleteAfter=%v", got.Status.TerminalTime, got.Status.DeleteAfter)
	}
	if audit.terminalSpans != 0 {
		t.Fatalf("suspension duplicated historical terminal audit: %d calls", audit.terminalSpans)
	}
}

func TestAuxiliaryTerminalStopMarkerPreservesWorkflowOutcome(t *testing.T) {
	ctx := context.Background()
	start := time.Unix(2_100_000, 0).UTC()
	terminalTime := metav1.NewTime(start)
	deleteAfter := metav1.NewTime(start.Add(14 * 24 * time.Hour))
	run := testSMRun()
	run.Status.Conditions = []metav1.Condition{
		{
			Type:               agenticv1alpha1.AgenticRunConditionVerified,
			Status:             metav1.ConditionTrue,
			Reason:             "Complete",
			ObservedGeneration: 7,
		},
		{
			Type:               agenticv1alpha1.AgenticRunConditionStopping,
			Status:             metav1.ConditionTrue,
			Reason:             reasonSystemSuspended,
			LastTransitionTime: metav1.NewTime(start),
			ObservedGeneration: 7,
		},
	}
	run.Generation = 7
	run.Status.TerminalTime = &terminalTime
	run.Status.DeleteAfter = &deleteAfter
	run.Labels = map[string]string{terminalTTLLabel: "true"}
	run.Status.Steps.Analysis.Sandbox = agenticv1alpha1.SandboxInfo{
		ClaimName: "stale-terminal-reference",
		Namespace: "test-ns",
	}
	config := suspendedConfigAt(start, run.Generation)
	fc := fake.NewClientBuilder().
		WithScheme(suspensionTestScheme()).
		WithObjects(append(defaultObjects(), run, config)...).
		WithStatusSubresource(&agenticv1alpha1.AgenticRun{}).
		Build()
	cache := testCache(t, "bare-pod")
	audit := &terminalAuditCounter{}
	r := &AgenticRunReconciler{
		Client:    fc,
		APIReader: fc,
		Sandbox:   NewSandboxManager(fc, fc, cache, "test-ns", nil),
		Config:    cache,
		Namespace: "test-ns",
		Audit:     audit,
		Now:       func() time.Time { return start.Add(time.Second) },
	}
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(run)}); err != nil {
		t.Fatalf("Reconcile auxiliary terminal stop marker: %v", err)
	}
	got := &agenticv1alpha1.AgenticRun{}
	if err := fc.Get(ctx, client.ObjectKeyFromObject(run), got); err != nil {
		t.Fatal(err)
	}
	if phase := agenticv1alpha1.DerivePhase(got.Status.Conditions); phase != agenticv1alpha1.AgenticRunPhaseCompleted {
		t.Fatalf("auxiliary stop changed historical phase to %s", phase)
	}
	if stopping := meta.FindStatusCondition(got.Status.Conditions, agenticv1alpha1.AgenticRunConditionStopping); stopping == nil ||
		stopping.Status != metav1.ConditionFalse || stopping.Reason != reasonTeardownComplete {
		t.Fatalf("auxiliary Stopping outcome = %+v, want TeardownComplete", stopping)
	}
	if emergency := meta.FindStatusCondition(got.Status.Conditions, agenticv1alpha1.AgenticRunConditionEmergencyStopped); emergency != nil {
		t.Fatalf("auxiliary stop replaced historical outcome with EmergencyStopped: %+v", emergency)
	}
	if got.Status.TerminalTime == nil || !got.Status.TerminalTime.Time.Equal(terminalTime.Time) ||
		got.Status.DeleteAfter == nil || !got.Status.DeleteAfter.Time.Equal(deleteAfter.Time) {
		t.Fatalf("auxiliary stop moved fixed TTL: terminalTime=%v deleteAfter=%v", got.Status.TerminalTime, got.Status.DeleteAfter)
	}
	if hasRunSandboxInfo(got) {
		t.Fatalf("confirmed auxiliary cleanup retained sandbox references: %+v", got.Status.Steps)
	}
	if audit.terminalSpans != 0 {
		t.Fatalf("auxiliary cleanup duplicated terminal audit: %d calls", audit.terminalSpans)
	}
}

func TestStoppedDeletionRetainsCleanupFinalizerUntilConfirmed(t *testing.T) {
	ctx := context.Background()
	now := metav1.NewTime(time.Unix(2_500_000, 0).UTC())
	run := testSMRun()
	run.DeletionTimestamp = &now
	run.Finalizers = []string{rbacCleanupFinalizer, templogCleanupFinalizer}
	run.Annotations = map[string]string{preserveSandboxAnnotation: "true"}
	run.Labels = map[string]string{terminalTTLLabel: "true"}
	run.Status.Conditions = []metav1.Condition{
		{
			Type:               agenticv1alpha1.AgenticRunConditionStopping,
			Status:             metav1.ConditionFalse,
			Reason:             reasonTeardownFailed,
			ObservedGeneration: run.Generation,
		},
		{
			Type:               agenticv1alpha1.AgenticRunConditionAnalyzed,
			Status:             metav1.ConditionFalse,
			Reason:             reasonEmergencyStopFailed,
			ObservedGeneration: run.Generation,
		},
	}
	terminalTime := metav1.NewTime(now.Add(-time.Hour))
	deleteAfter := metav1.NewTime(now.Add(24 * time.Hour))
	run.Status.TerminalTime = &terminalTime
	run.Status.DeleteAfter = &deleteAfter
	run.Status.Steps.Analysis.Sandbox = agenticv1alpha1.SandboxInfo{
		ClaimName: "ls-analysis-abc-123",
		Namespace: "test-ns",
	}
	pod := suspensionManagedPod("analysis-pod", "analysis-pod-uid", run.UID, "analysis", corev1.PodRunning)
	pod.Finalizers = []string{"test/hold"}
	fc := fake.NewClientBuilder().
		WithScheme(suspensionTestScheme()).
		WithObjects(append(defaultObjects(), run, pod)...).
		WithStatusSubresource(&agenticv1alpha1.AgenticRun{}).
		Build()
	recorder := &podDeleteOptionsClient{Client: fc}
	cache := testCache(t, "bare-pod")
	agent := &testAgentCaller{}
	r := &AgenticRunReconciler{
		Client:    recorder,
		APIReader: fc,
		Agent:     agent,
		Sandbox:   NewSandboxManager(recorder, fc, cache, "test-ns", nil),
		Config:    cache,
		Namespace: "test-ns",
		TempLog:   &mockTempLogCleaner{err: fmt.Errorf("collector unavailable")},
		Now:       func() time.Time { return now.Add(24 * time.Hour) },
	}
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(run)}

	result, err := r.Reconcile(ctx, request)
	if err != nil {
		t.Fatalf("Reconcile unconfirmed deletion: %v", err)
	}
	if result.RequeueAfter != suspensionRequeueAfter {
		t.Fatalf("unconfirmed deletion requeue = %s, want %s", result.RequeueAfter, suspensionRequeueAfter)
	}
	if agent.releaseAllCount != 0 {
		t.Fatalf("ordinary ReleaseSandboxes ran before suspension cleanup: %d calls", agent.releaseAllCount)
	}
	if len(recorder.podDeletes) != 1 || recorder.podDeletes[0].grace == nil || *recorder.podDeletes[0].grace != 0 {
		t.Fatalf("suspension delete requests = %+v, want one zero-grace Pod request", recorder.podDeletes)
	}
	pending := &agenticv1alpha1.AgenticRun{}
	if err := fc.Get(ctx, client.ObjectKeyFromObject(run), pending); err != nil {
		t.Fatal(err)
	}
	if !controllerutil.ContainsFinalizer(pending, rbacCleanupFinalizer) {
		t.Fatalf("unconfirmed stop removed cleanup finalizer: %v", pending.Finalizers)
	}
	if stopping := meta.FindStatusCondition(pending.Status.Conditions, agenticv1alpha1.AgenticRunConditionStopping); stopping == nil || stopping.Reason != reasonTeardownFailed {
		t.Fatalf("deletion changed final failure outcome: %+v", stopping)
	}

	var remainingPod corev1.Pod
	if err := fc.Get(ctx, client.ObjectKeyFromObject(pod), &remainingPod); err != nil {
		t.Fatalf("read held Pod: %v", err)
	}
	remainingPod.Finalizers = nil
	if err := fc.Update(ctx, &remainingPod); err != nil {
		t.Fatalf("remove test Pod finalizer: %v", err)
	}
	if err := fc.Delete(ctx, &remainingPod); err != nil && !apierrors.IsNotFound(err) {
		t.Fatalf("remove recovered Pod: %v", err)
	}

	result, err = r.Reconcile(ctx, request)
	if err != nil {
		t.Fatalf("Reconcile confirmed deletion: %v", err)
	}
	if result.RequeueAfter != templogCleanupRequeueAfter {
		t.Fatalf("confirmed deletion result = %+v, want templog retry after cleanup finalizer removal", result)
	}
	if agent.releaseAllCount != 0 {
		t.Fatalf("ordinary ReleaseSandboxes ran after suspension cleanup: %d calls", agent.releaseAllCount)
	}
	cleaned := &agenticv1alpha1.AgenticRun{}
	if err := fc.Get(ctx, client.ObjectKeyFromObject(run), cleaned); err != nil {
		t.Fatal(err)
	}
	if controllerutil.ContainsFinalizer(cleaned, rbacCleanupFinalizer) ||
		!controllerutil.ContainsFinalizer(cleaned, templogCleanupFinalizer) {
		t.Fatalf("finalizers after confirmed cleanup = %v, want only templog finalizer", cleaned.Finalizers)
	}
	if hasRunSandboxInfo(cleaned) {
		t.Fatalf("confirmed deletion retained sandbox references: %+v", cleaned.Status.Steps)
	}
	if stopping := meta.FindStatusCondition(cleaned.Status.Conditions, agenticv1alpha1.AgenticRunConditionStopping); stopping == nil || stopping.Reason != reasonTeardownFailed {
		t.Fatalf("cleanup recovery rewrote the final stop outcome: %+v", stopping)
	}
}

type recordedPodDelete struct {
	name  string
	grace *int64
}

type podDeleteOptionsClient struct {
	client.Client
	podDeletes []recordedPodDelete
}

func (c *podDeleteOptionsClient) Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
	if pod, ok := obj.(*corev1.Pod); ok {
		deleteOptions := (&client.DeleteOptions{}).ApplyOptions(opts)
		var grace *int64
		if deleteOptions.GracePeriodSeconds != nil {
			value := *deleteOptions.GracePeriodSeconds
			grace = &value
		}
		c.podDeletes = append(c.podDeletes, recordedPodDelete{name: pod.Name, grace: grace})
	}
	return c.Client.Delete(ctx, obj, opts...)
}

func sandboxReleaseObjects(t *testing.T, run *agenticv1alpha1.AgenticRun, claimName, podName string, claimMode bool) []client.Object {
	t.Helper()

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: podName, Namespace: "test-ns"},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
	objects := []client.Object{run, pod}
	if !claimMode {
		return objects
	}

	claim := &unstructured.Unstructured{}
	claim.SetGroupVersionKind(smClaimGVK)
	claim.SetName(claimName)
	claim.SetNamespace("test-ns")
	claim.SetUID(types.UID("claim-uid"))
	if err := unstructured.SetNestedField(claim.Object, "sandbox-real", "status", "sandbox", "name"); err != nil {
		t.Fatalf("set SandboxClaim status: %v", err)
	}

	sandbox := &unstructured.Unstructured{}
	sandbox.SetGroupVersionKind(smSandboxGVK)
	sandbox.SetName("sandbox-real")
	sandbox.SetNamespace("test-ns")
	sandbox.SetUID(types.UID("sandbox-uid"))
	if podName != sandbox.GetName() {
		sandbox.SetAnnotations(map[string]string{"agents.x-k8s.io/pod-name": podName})
	}
	controller := true
	sandbox.SetOwnerReferences([]metav1.OwnerReference{{
		APIVersion: smClaimGVK.GroupVersion().String(),
		Kind:       smClaimGVK.Kind,
		Name:       claimName,
		UID:        claim.GetUID(),
		Controller: &controller,
	}})
	pod.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: smSandboxGVK.GroupVersion().String(),
		Kind:       smSandboxGVK.Kind,
		Name:       sandbox.GetName(),
		UID:        sandbox.GetUID(),
		Controller: &controller,
	}}
	return append(objects, claim, sandbox)
}

func TestSandboxManagerSuspensionReleasePreservesPodOwnership(t *testing.T) {
	ctx := context.Background()
	claimName := "ls-analysis-abc-123"
	for _, tt := range []struct {
		name                   string
		mode                   string
		podName                string
		sandboxOwnerMismatch   bool
		podOwnerMismatch       bool
		unrelatedClaimNamedPod bool
		wantPodDeleted         bool
	}{
		{
			name:           "bare Pod deletion",
			mode:           "bare-pod",
			podName:        claimName,
			wantPodDeleted: true,
		},
		{
			name:           "SandboxClaim backing Pod deletion",
			mode:           "sandbox-claim",
			podName:        "backing-pod",
			wantPodDeleted: true,
		},
		{
			name:           "Sandbox name fallback",
			mode:           "sandbox-claim",
			podName:        "sandbox-real",
			wantPodDeleted: true,
		},
		{
			name:                 "Sandbox not owned by claim",
			mode:                 "sandbox-claim",
			podName:              "backing-pod",
			sandboxOwnerMismatch: true,
		},
		{
			name:             "Pod not owned by Sandbox",
			mode:             "sandbox-claim",
			podName:          "backing-pod",
			podOwnerMismatch: true,
		},
		{
			name:                 "same-named claim Pod survives mismatched Sandbox ownership",
			mode:                 "sandbox-claim",
			podName:              claimName,
			sandboxOwnerMismatch: true,
		},
		{
			name:                   "unrelated claim-named Pod survives backing Pod deletion",
			mode:                   "sandbox-claim",
			podName:                "backing-pod",
			unrelatedClaimNamedPod: true,
			wantPodDeleted:         true,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			run := testSMRun()
			run.Status.Steps.Analysis.Sandbox.ClaimName = claimName
			objects := sandboxReleaseObjects(t, run, claimName, tt.podName, tt.mode == "sandbox-claim")
			pod := objects[1].(*corev1.Pod)
			if tt.sandboxOwnerMismatch {
				sandbox := objects[3].(*unstructured.Unstructured)
				sandbox.SetOwnerReferences([]metav1.OwnerReference{
					suspensionOwnerRef(smClaimGVK, claimName, "other-claim"),
				})
			}
			if tt.podOwnerMismatch {
				sandbox := objects[3].(*unstructured.Unstructured)
				pod.OwnerReferences = []metav1.OwnerReference{
					suspensionOwnerRef(smSandboxGVK, sandbox.GetName(), "other-sandbox"),
				}
			}

			var unrelatedPod *corev1.Pod
			if tt.unrelatedClaimNamedPod {
				unrelatedPod = &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:            claimName,
						Namespace:       "test-ns",
						UID:             "unrelated-pod-uid",
						ResourceVersion: "1",
						OwnerReferences: []metav1.OwnerReference{
							suspensionOwnerRef(smSandboxGVK, "other-sandbox", "other-sandbox"),
						},
					},
					Status: corev1.PodStatus{Phase: corev1.PodRunning},
				}
				objects = append(objects, unrelatedPod)
			}

			fc := fake.NewClientBuilder().
				WithScheme(suspensionTestScheme()).
				WithObjects(objects...).
				Build()
			recorder := &podDeleteOptionsClient{Client: fc}
			manager := newTestSandboxManager(recorder, testCache(t, tt.mode))
			if err := manager.Release(ctx, run, "analysis", nil, true); err != nil {
				t.Fatalf("Release during suspension: %v", err)
			}

			var actualPod corev1.Pod
			err := fc.Get(ctx, client.ObjectKeyFromObject(pod), &actualPod)
			if tt.wantPodDeleted {
				if !apierrors.IsNotFound(err) {
					t.Fatalf("backing Pod %q should be deleted, got error %v", pod.Name, err)
				}
			} else if err != nil {
				t.Fatalf("backing Pod %q should be retained: %v", pod.Name, err)
			}
			if unrelatedPod != nil {
				var retainedPod corev1.Pod
				if err := fc.Get(ctx, client.ObjectKeyFromObject(unrelatedPod), &retainedPod); err != nil {
					t.Fatalf("unrelated claim-named Pod should be retained: %v", err)
				}
			}

			if tt.wantPodDeleted {
				found := false
				for _, call := range recorder.podDeletes {
					if call.name != pod.Name {
						continue
					}
					if call.grace == nil || *call.grace != 0 {
						t.Fatalf("Pod %q delete grace = %v, want zero", call.name, call.grace)
					}
					found = true
				}
				if !found {
					t.Fatalf("no zero-grace delete request for Pod %q", pod.Name)
				}
			}
		})
	}
}

func TestSandboxManagerReleaseKeepsDefaultPodGrace(t *testing.T) {
	for _, mode := range []string{"bare-pod", "sandbox-claim"} {
		t.Run(mode, func(t *testing.T) {
			run := testSMRun()
			claimName := "ls-analysis-abc-123"
			run.Status.Steps.Analysis.Sandbox.ClaimName = claimName
			fc := fake.NewClientBuilder().
				WithScheme(testScheme()).
				WithObjects(sandboxReleaseObjects(t, run, claimName, claimName, mode == "sandbox-claim")...).
				Build()
			recorder := &podDeleteOptionsClient{Client: fc}
			manager := newTestSandboxManager(recorder, testCache(t, mode))

			if err := manager.Release(context.Background(), run, "analysis", nil, false); err != nil {
				t.Fatalf("Release: %v", err)
			}

			for _, call := range recorder.podDeletes {
				if call.name != claimName {
					continue
				}
				if call.grace != nil {
					t.Fatalf("ordinary Pod delete grace = %d, want Kubernetes default", *call.grace)
				}
				return
			}
			t.Fatalf("no Pod delete request for %q; requests: %+v", claimName, recorder.podDeletes)
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
	r := &AgenticRunReconciler{Client: fc, APIReader: fc, Agent: newTestAgentCaller().withClient(t, fc, "default"), Namespace: "default", TempLog: cleaner}

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
	r := &AgenticRunReconciler{Client: fc, APIReader: fc, Agent: newTestAgentCaller().withClient(t, fc, "default"), Namespace: "default", TempLog: cleaner}

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
	r := &AgenticRunReconciler{Client: fc, APIReader: fc, Agent: newTestAgentCaller().withClient(t, fc, "default"), Namespace: "default", TempLog: cleaner}

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
	r := &AgenticRunReconciler{Client: fc, APIReader: fc, Agent: newTestAgentCaller().withClient(t, fc, "default"), Namespace: "default", TempLog: cleaner}

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

	r := &AgenticRunReconciler{Client: fc, APIReader: fc, Agent: newTestAgentCaller().withClient(t, fc, "default"), Namespace: "default"}
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

func TestDeletion_PreservedSandboxStillReleases(t *testing.T) {
	now := metav1.Now()
	run := testAgenticRun()
	run.UID = "a1b2c3d4-e5f6-7890-abcd-ef1234567890"
	run.DeletionTimestamp = &now
	run.Finalizers = []string{rbacCleanupFinalizer}
	run.Annotations = map[string]string{preserveSandboxAnnotation: "true"}
	run.Status.Steps.Analysis.Sandbox.ClaimName = "analysis-sandbox"

	fc := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(run).
		WithStatusSubresource(run).Build()
	caller := newTestAgentCaller().withClient(t, fc, "default")
	r := &AgenticRunReconciler{Client: fc, APIReader: fc, Agent: caller, Namespace: "default"}

	if _, err := reconcileOnce(r, "fix-crash"); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if caller.releaseAllCount != 1 {
		t.Fatalf("ReleaseSandboxes called %d times, want 1", caller.releaseAllCount)
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
	r := &AgenticRunReconciler{Client: fc, APIReader: fc, Agent: newTestAgentCaller().withClient(t, fc, "default"), Namespace: "default", TempLog: cleaner}

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
