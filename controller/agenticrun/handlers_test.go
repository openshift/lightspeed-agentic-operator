package agenticrun

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
)

func reviseAgenticRun(t *testing.T, fc client.WithWatch, name string, feedback string) {
	t.Helper()
	var p agenticv1alpha1.AgenticRun
	if err := fc.Get(context.Background(), types.NamespacedName{Name: name, Namespace: "default"}, &p); err != nil {
		t.Fatalf("get run for revision: %v", err)
	}
	original := p.DeepCopy()
	p.Spec.RevisionFeedback = feedback
	// Fake client doesn't auto-increment generation; simulate API server behavior.
	p.Generation++
	if err := fc.Patch(context.Background(), &p, client.MergeFrom(original)); err != nil {
		t.Fatalf("patch revision: %v", err)
	}
}

func TestReconcile_WorkflowVariants(t *testing.T) {
	tests := []struct {
		name      string
		run       *agenticv1alpha1.AgenticRun
		wantPhase agenticv1alpha1.AgenticRunPhase
	}{
		{
			name:      "full_lifecycle_reaches_verifying",
			run:       testAgenticRun(),
			wantPhase: agenticv1alpha1.AgenticRunPhaseVerifying,
		},
		{
			name: "advisory_only_completes",
			run: &agenticv1alpha1.AgenticRun{
				ObjectMeta: metav1.ObjectMeta{Name: "fix-crash", Namespace: "default"},
				Spec: agenticv1alpha1.AgenticRunSpec{
					Request:          "Investigate issue",
					Tools:            testTools(),
					TargetNamespaces: []string{"production"},
					Analysis:         agenticv1alpha1.AgenticRunStep{Agent: "default"},
				},
			},
			wantPhase: agenticv1alpha1.AgenticRunPhaseCompleted,
		},
		{
			name: "assisted_reaches_verifying",
			run: &agenticv1alpha1.AgenticRun{
				ObjectMeta: metav1.ObjectMeta{Name: "fix-crash", Namespace: "default"},
				Spec: agenticv1alpha1.AgenticRunSpec{
					Request:          "Fix with manual apply",
					Tools:            testTools(),
					TargetNamespaces: []string{"production"},
					Analysis:         agenticv1alpha1.AgenticRunStep{Agent: "default"},
					Verification:     agenticv1alpha1.AgenticRunStep{Agent: "default"},
				},
			},
			wantPhase: agenticv1alpha1.AgenticRunPhaseVerifying,
		},
		{
			name: "no_verification_skips_verification",
			run: &agenticv1alpha1.AgenticRun{
				ObjectMeta: metav1.ObjectMeta{Name: "fix-crash", Namespace: "default"},
				Spec: agenticv1alpha1.AgenticRunSpec{
					Request:          "Trust mode fix",
					Tools:            testTools(),
					TargetNamespaces: []string{"production"},
					Analysis:         agenticv1alpha1.AgenticRunStep{Agent: "default"},
					Execution:        agenticv1alpha1.AgenticRunStep{Agent: "default"},
				},
			},
			wantPhase: agenticv1alpha1.AgenticRunPhaseCompleted,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scheme := testScheme()
			run := tt.run

			objs := []client.Object{run, testDefaultAgent(), testLLM("smart"), testAutoApprovePolicy()}
			fc := fake.NewClientBuilder().WithScheme(scheme).
				WithObjects(objs...).
				WithStatusSubresource(run, &agenticv1alpha1.AnalysisResult{}, &agenticv1alpha1.ExecutionResult{}, &agenticv1alpha1.VerificationResult{}, &agenticv1alpha1.EscalationResult{}).Build()

			r := &AgenticRunReconciler{Client: fc, Agent: newTestAgentCaller(), Namespace: "default"}

			if _, err := reconcileOnce(r, "fix-crash"); err != nil {
				t.Fatalf("analysis reconcile: %v", err)
			}
			p, _ := getAgenticRun(r, "fix-crash")
			if agenticv1alpha1.DerivePhase(p.Status.Conditions) != agenticv1alpha1.AgenticRunPhaseProposed {
				t.Fatalf("after analysis: expected Proposed, got %s", agenticv1alpha1.DerivePhase(p.Status.Conditions))
			}

			approveAgenticRun(t, fc, "fix-crash")

			if _, err := reconcileOnce(r, "fix-crash"); err != nil {
				t.Fatalf("post-approval reconcile: %v", err)
			}
			p, _ = getAgenticRun(r, "fix-crash")
			if agenticv1alpha1.DerivePhase(p.Status.Conditions) != tt.wantPhase {
				t.Fatalf("after approval: expected %s, got %s", tt.wantPhase, agenticv1alpha1.DerivePhase(p.Status.Conditions))
			}
		})
	}
}

func TestReconcile_HappyPath_FullLifecycle(t *testing.T) {
	scheme := testScheme()
	run := testAgenticRun()

	objs := append([]client.Object{run}, defaultObjects()...)
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
		WithStatusSubresource(run, &agenticv1alpha1.AnalysisResult{}, &agenticv1alpha1.ExecutionResult{}, &agenticv1alpha1.VerificationResult{}, &agenticv1alpha1.EscalationResult{}).Build()

	r := &AgenticRunReconciler{Client: fc, Agent: newTestAgentCaller(), Namespace: "default"}

	// Reconcile 1: Pending → Proposed (analysis complete)
	result, err := reconcileOnce(r, "fix-crash")
	if err != nil {
		t.Fatalf("reconcile 1: %v", err)
	}
	if result.Requeue {
		t.Error("should not requeue — watch event drives next reconcile")
	}

	p, _ := getAgenticRun(r, "fix-crash")
	if agenticv1alpha1.DerivePhase(p.Status.Conditions) != agenticv1alpha1.AgenticRunPhaseProposed {
		t.Fatalf("expected Proposed, got %s", agenticv1alpha1.DerivePhase(p.Status.Conditions))
	}
	if len(p.Status.Steps.Analysis.Results) == 0 {
		t.Fatal("analysis results not set")
	}
	var analysisResult agenticv1alpha1.AnalysisResult
	if err := fc.Get(context.Background(), types.NamespacedName{Name: p.Status.Steps.Analysis.Results[0].Name, Namespace: "default"}, &analysisResult); err != nil {
		t.Fatalf("get AnalysisResult: %v", err)
	}
	if len(analysisResult.Status.Options) == 0 {
		t.Fatal("analysis options not set")
	}
	assertResultConditions(t, analysisResult.Status.Conditions, "Succeeded")

	// Approve
	approveAgenticRun(t, fc, "fix-crash")

	// Reconcile 2: Executing → Verifying
	result, err = reconcileOnce(r, "fix-crash")
	if err != nil {
		t.Fatalf("reconcile 2: %v", err)
	}
	if result.Requeue {
		t.Error("should not requeue — watch event drives next reconcile")
	}

	p, _ = getAgenticRun(r, "fix-crash")
	if agenticv1alpha1.DerivePhase(p.Status.Conditions) != agenticv1alpha1.AgenticRunPhaseVerifying {
		t.Fatalf("expected Verifying, got %s", agenticv1alpha1.DerivePhase(p.Status.Conditions))
	}
	if len(p.Status.Steps.Execution.Results) == 0 {
		t.Fatal("execution results not set")
	}
	var execResult agenticv1alpha1.ExecutionResult
	if err := fc.Get(context.Background(), types.NamespacedName{Name: p.Status.Steps.Execution.Results[0].Name, Namespace: "default"}, &execResult); err != nil {
		t.Fatalf("get ExecutionResult: %v", err)
	}
	if len(execResult.Status.ActionsTaken) == 0 {
		t.Fatal("execution actions not set")
	}
	assertResultConditions(t, execResult.Status.Conditions, "Succeeded")

	// Reconcile 3: Verifying → Completed
	_, err = reconcileOnce(r, "fix-crash")
	if err != nil {
		t.Fatalf("reconcile 3: %v", err)
	}

	p, _ = getAgenticRun(r, "fix-crash")
	if agenticv1alpha1.DerivePhase(p.Status.Conditions) != agenticv1alpha1.AgenticRunPhaseCompleted {
		t.Fatalf("expected Completed, got %s", agenticv1alpha1.DerivePhase(p.Status.Conditions))
	}
	if len(p.Status.Steps.Verification.Results) == 0 {
		t.Fatal("verification results not set")
	}
	var verifyResult agenticv1alpha1.VerificationResult
	if err := fc.Get(context.Background(), types.NamespacedName{Name: p.Status.Steps.Verification.Results[0].Name, Namespace: "default"}, &verifyResult); err != nil {
		t.Fatalf("get VerificationResult: %v", err)
	}
	if verifyResult.Status.Summary == "" {
		t.Fatal("verification summary not set")
	}
	assertResultConditions(t, verifyResult.Status.Conditions, "Succeeded")
}

func TestReconcile_VerificationWithLongSource_Succeeds(t *testing.T) {
	agent := newTestAgentCaller()
	// Source longer than the old 256-byte limit (OLS-3735).
	longSource := "oc get pod -n payments-processing-system-production-us-east-1 -l app.kubernetes.io/name=payment-gateway-service,app.kubernetes.io/component=transaction-processor -o jsonpath='{.items[?(@.status.containerStatuses[0].state.waiting.reason==\"CrashLoopBackOff\")].metadata.name}'"
	if len(longSource) <= 256 {
		t.Fatalf("test source must exceed 256 bytes, got %d", len(longSource))
	}
	agent.verifyResult = &VerificationOutput{
		Success: true,
		Checks: []agenticv1alpha1.VerifyCheck{{
			Name:   "pod-running",
			Source: longSource,
			Value:  "Running",
			Result: agenticv1alpha1.CheckResultPassed,
		}},
		Summary: "All checks passed",
	}

	scheme := testScheme()
	run := testAgenticRun()
	objs := append([]client.Object{run}, defaultObjects()...)
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
		WithStatusSubresource(run, &agenticv1alpha1.AnalysisResult{}, &agenticv1alpha1.ExecutionResult{}, &agenticv1alpha1.VerificationResult{}, &agenticv1alpha1.EscalationResult{}).Build()

	r := &AgenticRunReconciler{Client: fc, Agent: agent, Namespace: "default"}

	// Analysis → approve → execution → verification
	reconcileOnce(r, "fix-crash")
	approveAgenticRun(t, fc, "fix-crash")
	reconcileOnce(r, "fix-crash")

	_, err := reconcileOnce(r, "fix-crash")
	if err != nil {
		t.Fatalf("verification reconcile: %v", err)
	}

	p, _ := getAgenticRun(r, "fix-crash")
	if agenticv1alpha1.DerivePhase(p.Status.Conditions) != agenticv1alpha1.AgenticRunPhaseCompleted {
		t.Fatalf("expected Completed, got %s", agenticv1alpha1.DerivePhase(p.Status.Conditions))
	}

	var verifyResult agenticv1alpha1.VerificationResult
	if err := fc.Get(context.Background(), types.NamespacedName{Name: p.Status.Steps.Verification.Results[0].Name, Namespace: "default"}, &verifyResult); err != nil {
		t.Fatalf("get VerificationResult: %v", err)
	}
	if verifyResult.Status.Checks[0].Source != longSource {
		t.Fatalf("source was truncated: got %d bytes, want %d", len(verifyResult.Status.Checks[0].Source), len(longSource))
	}
}

func TestReconcile_AnalysisSystemFailure_Terminal(t *testing.T) {
	agent := newTestAgentCaller()
	agent.analyzeErr = fmt.Errorf("LLM timeout")
	scheme := testScheme()

	run := testAgenticRun()
	objs := append([]client.Object{run}, defaultObjects()...)
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
		WithStatusSubresource(run, &agenticv1alpha1.AnalysisResult{}, &agenticv1alpha1.ExecutionResult{}, &agenticv1alpha1.VerificationResult{}, &agenticv1alpha1.EscalationResult{}).Build()

	r := &AgenticRunReconciler{Client: fc, Agent: agent, Namespace: "default"}

	// Reconcile 1: Pending → Failed (system failure)
	result, err := reconcileOnce(r, "fix-crash")
	if err != nil {
		t.Fatalf("reconcile 1: %v", err)
	}
	if result.Requeue {
		t.Error("should not requeue — watch event drives next reconcile")
	}
	p, _ := getAgenticRun(r, "fix-crash")
	if agenticv1alpha1.DerivePhase(p.Status.Conditions) != agenticv1alpha1.AgenticRunPhaseFailed {
		t.Fatalf("expected Failed, got %s", agenticv1alpha1.DerivePhase(p.Status.Conditions))
	}

	// Reconcile 2: Failed stays Failed (terminal, no retry)
	reconcileOnce(r, "fix-crash")
	p, _ = getAgenticRun(r, "fix-crash")
	if agenticv1alpha1.DerivePhase(p.Status.Conditions) != agenticv1alpha1.AgenticRunPhaseFailed {
		t.Fatalf("expected Failed (terminal), got %s", agenticv1alpha1.DerivePhase(p.Status.Conditions))
	}
	if len(p.Status.Steps.Analysis.Results) != 1 {
		t.Fatalf("expected 1 analysis result recorded, got %d", len(p.Status.Steps.Analysis.Results))
	}
	if p.Status.Steps.Analysis.Results[0].Outcome != agenticv1alpha1.ActionOutcomeFailed {
		t.Fatal("expected failed analysis result")
	}
}

func TestReconcile_VerificationObjectiveFailure_Escalates(t *testing.T) {
	agent := newTestAgentCaller()
	scheme := testScheme()

	run := testAgenticRun()

	objs := append([]client.Object{run}, defaultObjects()...)
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
		WithStatusSubresource(run, &agenticv1alpha1.AnalysisResult{}, &agenticv1alpha1.ExecutionResult{}, &agenticv1alpha1.VerificationResult{}, &agenticv1alpha1.EscalationResult{}).Build()

	r := &AgenticRunReconciler{Client: fc, Agent: agent, Namespace: "default"}

	// Analysis → approve → execution → verifying
	reconcileOnce(r, "fix-crash")
	approveAgenticRun(t, fc, "fix-crash")
	reconcileOnce(r, "fix-crash")

	// Make verification fail (objective failure, not system error)
	agent.verifyResult = &VerificationOutput{
		Success: false,
		Checks:  []agenticv1alpha1.VerifyCheck{{Name: "pod-running", Source: "oc", Value: "CrashLoopBackOff", Result: agenticv1alpha1.CheckResultFailed}},
		Summary: "Pod still crashing",
	}

	// Verification fails → escalates directly (no retry).
	result, err := reconcileOnce(r, "fix-crash")
	if err != nil {
		t.Fatalf("verification reconcile: %v", err)
	}
	if result.Requeue {
		t.Error("should not requeue — watch event drives next reconcile")
	}
	p, _ := getAgenticRun(r, "fix-crash")
	if agenticv1alpha1.DerivePhase(p.Status.Conditions) != agenticv1alpha1.AgenticRunPhaseEscalating {
		t.Fatalf("expected Escalating, got %s", agenticv1alpha1.DerivePhase(p.Status.Conditions))
	}
	verified := meta.FindStatusCondition(p.Status.Conditions, agenticv1alpha1.AgenticRunConditionVerified)
	if verified == nil || verified.Status != metav1.ConditionFalse || verified.Reason != agenticv1alpha1.ReasonVerificationFailed {
		t.Fatalf("expected Verified=False/VerificationFailed, got %+v", verified)
	}
}

func TestReconcile_SystemFailure_Execution_Terminal(t *testing.T) {
	agent := newTestAgentCaller()
	scheme := testScheme()

	run := testAgenticRun()
	objs := append([]client.Object{run}, defaultObjects()...)
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
		WithStatusSubresource(run, &agenticv1alpha1.AnalysisResult{}, &agenticv1alpha1.ExecutionResult{}, &agenticv1alpha1.VerificationResult{}, &agenticv1alpha1.EscalationResult{}).Build()

	r := &AgenticRunReconciler{Client: fc, Agent: agent, Namespace: "default"}

	// Analysis → approve
	reconcileOnce(r, "fix-crash")
	approveAgenticRun(t, fc, "fix-crash")

	// Execution system failure
	agent.executeErr = fmt.Errorf("sandbox pod crashed")
	result, err := reconcileOnce(r, "fix-crash")
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if result.Requeue {
		t.Error("should not requeue — watch event drives next reconcile")
	}
	p, _ := getAgenticRun(r, "fix-crash")
	if agenticv1alpha1.DerivePhase(p.Status.Conditions) != agenticv1alpha1.AgenticRunPhaseFailed {
		t.Fatalf("expected Failed, got %s", agenticv1alpha1.DerivePhase(p.Status.Conditions))
	}

	// Terminal — stays Failed
	reconcileOnce(r, "fix-crash")
	p, _ = getAgenticRun(r, "fix-crash")
	if agenticv1alpha1.DerivePhase(p.Status.Conditions) != agenticv1alpha1.AgenticRunPhaseFailed {
		t.Fatalf("expected Failed (terminal), got %s", agenticv1alpha1.DerivePhase(p.Status.Conditions))
	}
}

func TestReconcile_SystemFailure_Verification_Terminal(t *testing.T) {
	agent := newTestAgentCaller()
	scheme := testScheme()

	run := testAgenticRun()
	objs := append([]client.Object{run}, defaultObjects()...)
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
		WithStatusSubresource(run, &agenticv1alpha1.AnalysisResult{}, &agenticv1alpha1.ExecutionResult{}, &agenticv1alpha1.VerificationResult{}, &agenticv1alpha1.EscalationResult{}).Build()

	r := &AgenticRunReconciler{Client: fc, Agent: agent, Namespace: "default"}

	// Analysis → approve → execution → verifying
	reconcileOnce(r, "fix-crash")
	approveAgenticRun(t, fc, "fix-crash")
	reconcileOnce(r, "fix-crash")

	// Verification system failure
	agent.verifyErr = fmt.Errorf("network unreachable")
	result, err := reconcileOnce(r, "fix-crash")
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if result.Requeue {
		t.Error("should not requeue — watch event drives next reconcile")
	}
	p, _ := getAgenticRun(r, "fix-crash")
	if agenticv1alpha1.DerivePhase(p.Status.Conditions) != agenticv1alpha1.AgenticRunPhaseFailed {
		t.Fatalf("expected Failed, got %s", agenticv1alpha1.DerivePhase(p.Status.Conditions))
	}

	// Terminal — stays Failed
	reconcileOnce(r, "fix-crash")
	p, _ = getAgenticRun(r, "fix-crash")
	if agenticv1alpha1.DerivePhase(p.Status.Conditions) != agenticv1alpha1.AgenticRunPhaseFailed {
		t.Fatalf("expected Failed (terminal), got %s", agenticv1alpha1.DerivePhase(p.Status.Conditions))
	}
}

func TestReconcile_ObjectiveFailure_ThenRevise(t *testing.T) {
	agent := newTestAgentCaller()
	scheme := testScheme()

	run := testAgenticRun()

	objs := append([]client.Object{run}, defaultObjects()...)
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
		WithStatusSubresource(run, &agenticv1alpha1.AnalysisResult{}, &agenticv1alpha1.ExecutionResult{}, &agenticv1alpha1.VerificationResult{}, &agenticv1alpha1.EscalationResult{}).Build()

	r := &AgenticRunReconciler{Client: fc, Agent: agent, Namespace: "default"}

	// Full lifecycle to verification failure → escalates directly, then revised → Analyzing
	reconcileOnce(r, "fix-crash")
	approveAgenticRun(t, fc, "fix-crash")
	reconcileOnce(r, "fix-crash")

	agent.verifyResult = &VerificationOutput{
		Checks:  []agenticv1alpha1.VerifyCheck{{Name: "pod-running", Source: "oc", Value: "CrashLoopBackOff", Result: agenticv1alpha1.CheckResultFailed}},
		Summary: "Pod still crashing",
	}
	// Verification fails → Escalating immediately (no retries)
	reconcileOnce(r, "fix-crash")
	// Re-reconcile while escalating is idempotent
	reconcileOnce(r, "fix-crash")
	reconcileOnce(r, "fix-crash")

	p, _ := getAgenticRun(r, "fix-crash")
	if agenticv1alpha1.DerivePhase(p.Status.Conditions) != agenticv1alpha1.AgenticRunPhaseEscalating {
		t.Fatalf("expected Escalating (verification failed), got %s", agenticv1alpha1.DerivePhase(p.Status.Conditions))
	}

	// Admin submits revision
	agent.verifyResult = &VerificationOutput{
		Success: true,
		Checks:  []agenticv1alpha1.VerifyCheck{{Name: "pod-running", Source: "oc", Value: "Running", Result: agenticv1alpha1.CheckResultPassed}},
		Summary: "Pod running",
	}
	reviseAgenticRun(t, fc, "fix-crash", "revise analysis")
	reconcileOnce(r, "fix-crash") // revision re-analysis

	p, _ = getAgenticRun(r, "fix-crash")
	if agenticv1alpha1.DerivePhase(p.Status.Conditions) != agenticv1alpha1.AgenticRunPhaseProposed {
		t.Fatalf("expected Proposed after revision, got %s", agenticv1alpha1.DerivePhase(p.Status.Conditions))
	}

	// Approve and complete
	approveAgenticRun(t, fc, "fix-crash")
	reconcileOnce(r, "fix-crash") // execution + verification
	p, _ = getAgenticRun(r, "fix-crash")
	if agenticv1alpha1.DerivePhase(p.Status.Conditions) != agenticv1alpha1.AgenticRunPhaseVerifying {
		t.Fatalf("expected Verifying, got %s", agenticv1alpha1.DerivePhase(p.Status.Conditions))
	}
	reconcileOnce(r, "fix-crash")
	p, _ = getAgenticRun(r, "fix-crash")
	if agenticv1alpha1.DerivePhase(p.Status.Conditions) != agenticv1alpha1.AgenticRunPhaseCompleted {
		t.Fatalf("expected Completed, got %s", agenticv1alpha1.DerivePhase(p.Status.Conditions))
	}
}

func TestReconcile_RevisionHappyPath(t *testing.T) {
	scheme := testScheme()
	run := testAgenticRun()

	objs := append([]client.Object{run}, defaultObjects()...)
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
		WithStatusSubresource(run, &agenticv1alpha1.AnalysisResult{}, &agenticv1alpha1.ExecutionResult{}, &agenticv1alpha1.VerificationResult{}, &agenticv1alpha1.EscalationResult{}).Build()

	r := &AgenticRunReconciler{Client: fc, Agent: newTestAgentCaller(), Namespace: "default"}

	// Reconcile 1: Pending → Executing (analysis complete)
	if _, err := reconcileOnce(r, "fix-crash"); err != nil {
		t.Fatalf("reconcile 1: %v", err)
	}
	p, _ := getAgenticRun(r, "fix-crash")
	if agenticv1alpha1.DerivePhase(p.Status.Conditions) != agenticv1alpha1.AgenticRunPhaseProposed {
		t.Fatalf("expected Proposed, got %s", agenticv1alpha1.DerivePhase(p.Status.Conditions))
	}
	initialResultCount := len(p.Status.Steps.Analysis.Results)

	// Submit revision
	reviseAgenticRun(t, fc, "fix-crash", "revise analysis")

	// Reconcile 2: Executing → Analyzing → Executing (revised)
	if _, err := reconcileOnce(r, "fix-crash"); err != nil {
		t.Fatalf("reconcile 2 (revision): %v", err)
	}
	p, _ = getAgenticRun(r, "fix-crash")
	if agenticv1alpha1.DerivePhase(p.Status.Conditions) != agenticv1alpha1.AgenticRunPhaseProposed {
		t.Fatalf("expected Proposed after revision, got %s", agenticv1alpha1.DerivePhase(p.Status.Conditions))
	}
	if analyzed := meta.FindStatusCondition(p.Status.Conditions, agenticv1alpha1.AgenticRunConditionAnalyzed); analyzed == nil || analyzed.ObservedGeneration == 0 {
		t.Fatal("observedGeneration not set after revision")
	}
	if len(p.Status.Steps.Analysis.Results) <= initialResultCount {
		t.Fatal("results should have a new entry after revision")
	}

	// Approve and continue
	approveAgenticRun(t, fc, "fix-crash")
	if _, err := reconcileOnce(r, "fix-crash"); err != nil {
		t.Fatalf("reconcile 3 (post-approval): %v", err)
	}
	p, _ = getAgenticRun(r, "fix-crash")
	if agenticv1alpha1.DerivePhase(p.Status.Conditions) != agenticv1alpha1.AgenticRunPhaseVerifying {
		t.Fatalf("expected Verifying after approval, got %s", agenticv1alpha1.DerivePhase(p.Status.Conditions))
	}
}

func TestReconcile_RevisionMultipleRounds(t *testing.T) {
	scheme := testScheme()
	run := testAgenticRun()

	objs := append([]client.Object{run}, defaultObjects()...)
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
		WithStatusSubresource(run, &agenticv1alpha1.AnalysisResult{}, &agenticv1alpha1.ExecutionResult{}, &agenticv1alpha1.VerificationResult{}, &agenticv1alpha1.EscalationResult{}).Build()

	r := &AgenticRunReconciler{Client: fc, Agent: newTestAgentCaller(), Namespace: "default"}

	// Initial analysis
	reconcileOnce(r, "fix-crash")

	// Revision 1
	reviseAgenticRun(t, fc, "fix-crash", "revise analysis")
	reconcileOnce(r, "fix-crash")

	// Second revision
	reviseAgenticRun(t, fc, "fix-crash", "revise again")
	reconcileOnce(r, "fix-crash")

	p, _ := getAgenticRun(r, "fix-crash")
	if agenticv1alpha1.DerivePhase(p.Status.Conditions) != agenticv1alpha1.AgenticRunPhaseProposed {
		t.Fatalf("expected Proposed, got %s", agenticv1alpha1.DerivePhase(p.Status.Conditions))
	}
	if analyzed := meta.FindStatusCondition(p.Status.Conditions, agenticv1alpha1.AgenticRunConditionAnalyzed); analyzed == nil || analyzed.ObservedGeneration == 0 {
		t.Fatal("observedGeneration not set after second revision")
	}

	// Approve and proceed
	approveAgenticRun(t, fc, "fix-crash")
	reconcileOnce(r, "fix-crash")
	p, _ = getAgenticRun(r, "fix-crash")
	if agenticv1alpha1.DerivePhase(p.Status.Conditions) != agenticv1alpha1.AgenticRunPhaseVerifying {
		t.Fatalf("expected Verifying, got %s", agenticv1alpha1.DerivePhase(p.Status.Conditions))
	}
}

func TestReconcile_RevisionNoOp_WhenObserved(t *testing.T) {
	scheme := testScheme()
	run := testAgenticRun()

	objs := append([]client.Object{run}, defaultObjects()...)
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
		WithStatusSubresource(run, &agenticv1alpha1.AnalysisResult{}, &agenticv1alpha1.ExecutionResult{}, &agenticv1alpha1.VerificationResult{}, &agenticv1alpha1.EscalationResult{}).Build()

	r := &AgenticRunReconciler{Client: fc, Agent: newTestAgentCaller(), Namespace: "default"}

	// Initial analysis
	reconcileOnce(r, "fix-crash")

	// Simulate already-observed generation (feedback set but already processed)
	p, _ := getAgenticRun(r, "fix-crash")
	base := p.DeepCopy()
	p.Spec.RevisionFeedback = "some feedback"
	p.Generation = 2
	if err := fc.Patch(context.Background(), p, client.MergeFrom(base)); err != nil {
		t.Fatalf("patch spec revisionFeedback: %v", err)
	}
	p, _ = getAgenticRun(r, "fix-crash")
	base = p.DeepCopy()
	meta.SetStatusCondition(&p.Status.Conditions, metav1.Condition{
		Type:               agenticv1alpha1.AgenticRunConditionAnalyzed,
		Status:             metav1.ConditionTrue,
		Reason:             reasonRevisionComplete,
		Message:            "Revision complete",
		ObservedGeneration: 2,
	})
	if err := fc.Status().Patch(context.Background(), p, client.MergeFrom(base)); err != nil {
		t.Fatalf("patch status observedGeneration: %v", err)
	}

	// Reconcile should be a no-op
	result, err := reconcileOnce(r, "fix-crash")
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if result.Requeue {
		t.Error("should not requeue when revision already observed")
	}

	p, _ = getAgenticRun(r, "fix-crash")
	if agenticv1alpha1.DerivePhase(p.Status.Conditions) != agenticv1alpha1.AgenticRunPhaseProposed {
		t.Fatalf("expected Proposed, got %s", agenticv1alpha1.DerivePhase(p.Status.Conditions))
	}
}

func TestReconcile_RevisionReanalysis(t *testing.T) {
	scheme := testScheme()
	run := testAgenticRun()

	objs := append([]client.Object{run}, defaultObjects()...)
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
		WithStatusSubresource(run, &agenticv1alpha1.AnalysisResult{}, &agenticv1alpha1.ExecutionResult{}, &agenticv1alpha1.VerificationResult{}, &agenticv1alpha1.EscalationResult{}).Build()

	r := &AgenticRunReconciler{Client: fc, Agent: newTestAgentCaller(), Namespace: "default"}

	// Analysis → Executing
	reconcileOnce(r, "fix-crash")

	// Submit revision
	reviseAgenticRun(t, fc, "fix-crash", "revise analysis")

	// Reconcile revision
	reconcileOnce(r, "fix-crash")
}

func TestReconcile_RevisionAnalysisFailure(t *testing.T) {
	agent := newTestAgentCaller()
	scheme := testScheme()
	run := testAgenticRun()

	objs := append([]client.Object{run}, defaultObjects()...)
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
		WithStatusSubresource(run, &agenticv1alpha1.AnalysisResult{}, &agenticv1alpha1.ExecutionResult{}, &agenticv1alpha1.VerificationResult{}, &agenticv1alpha1.EscalationResult{}).Build()

	r := &AgenticRunReconciler{Client: fc, Agent: agent, Namespace: "default"}

	// Initial analysis succeeds
	reconcileOnce(r, "fix-crash")
	p, _ := getAgenticRun(r, "fix-crash")
	if agenticv1alpha1.DerivePhase(p.Status.Conditions) != agenticv1alpha1.AgenticRunPhaseProposed {
		t.Fatalf("expected Proposed, got %s", agenticv1alpha1.DerivePhase(p.Status.Conditions))
	}

	// Submit revision, but agent will fail
	reviseAgenticRun(t, fc, "fix-crash", "revise analysis")
	agent.analyzeErr = fmt.Errorf("LLM timeout during revision")

	// Reconcile → revision analysis fails → Failed
	result, err := reconcileOnce(r, "fix-crash")
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if result.Requeue {
		t.Error("should not requeue — watch event drives next reconcile")
	}
	p, _ = getAgenticRun(r, "fix-crash")
	if agenticv1alpha1.DerivePhase(p.Status.Conditions) != agenticv1alpha1.AgenticRunPhaseFailed {
		t.Fatalf("expected Failed, got %s", agenticv1alpha1.DerivePhase(p.Status.Conditions))
	}

	// Failed is terminal for system failures — stays Failed
	agent.analyzeErr = nil
	reconcileOnce(r, "fix-crash")
	p, _ = getAgenticRun(r, "fix-crash")
	if agenticv1alpha1.DerivePhase(p.Status.Conditions) != agenticv1alpha1.AgenticRunPhaseFailed {
		t.Fatalf("expected Failed (terminal), got %s", agenticv1alpha1.DerivePhase(p.Status.Conditions))
	}
}

func TestReconcile_RevisionWithFeedback(t *testing.T) {
	scheme := testScheme()
	run := testAgenticRun()

	objs := append([]client.Object{run}, defaultObjects()...)
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
		WithStatusSubresource(run, &agenticv1alpha1.AnalysisResult{}, &agenticv1alpha1.ExecutionResult{}, &agenticv1alpha1.VerificationResult{}, &agenticv1alpha1.EscalationResult{}).Build()

	r := &AgenticRunReconciler{Client: fc, Agent: newTestAgentCaller(), Namespace: "default"}

	// Initial analysis
	if _, err := reconcileOnce(r, "fix-crash"); err != nil {
		t.Fatalf("initial analysis: %v", err)
	}

	// Submit revision with feedback
	reviseAgenticRun(t, fc, "fix-crash", "Focus on the memory limit, not CPU throttling")

	// Reconcile revision
	if _, err := reconcileOnce(r, "fix-crash"); err != nil {
		t.Fatalf("revision reconcile: %v", err)
	}

	p, _ := getAgenticRun(r, "fix-crash")
	if agenticv1alpha1.DerivePhase(p.Status.Conditions) != agenticv1alpha1.AgenticRunPhaseProposed {
		t.Fatalf("expected Proposed after revision, got %s", agenticv1alpha1.DerivePhase(p.Status.Conditions))
	}
	if analyzed := meta.FindStatusCondition(p.Status.Conditions, agenticv1alpha1.AgenticRunConditionAnalyzed); analyzed == nil || analyzed.ObservedGeneration == 0 {
		t.Fatal("observedGeneration not set after revision")
	}
	if p.Spec.RevisionFeedback != "Focus on the memory limit, not CPU throttling" {
		t.Fatalf("expected revisionFeedback to be preserved, got %q", p.Spec.RevisionFeedback)
	}
}

func TestReconcile_RevisionFromCompleted(t *testing.T) {
	scheme := testScheme()
	// Advisory-only run (no Execution/Verification) reaches Completed in one reconcile
	run := &agenticv1alpha1.AgenticRun{
		ObjectMeta: metav1.ObjectMeta{Name: "fix-crash", Namespace: "default"},
		Spec: agenticv1alpha1.AgenticRunSpec{
			Request:          "Investigate issue",
			Tools:            testTools(),
			TargetNamespaces: []string{"production"},
			Analysis:         agenticv1alpha1.AgenticRunStep{Agent: "default"},
		},
	}

	objs := append([]client.Object{run}, defaultObjects()...)
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
		WithStatusSubresource(run, &agenticv1alpha1.AnalysisResult{}, &agenticv1alpha1.ExecutionResult{}, &agenticv1alpha1.VerificationResult{}, &agenticv1alpha1.EscalationResult{}).Build()

	r := &AgenticRunReconciler{Client: fc, Agent: newTestAgentCaller(), Namespace: "default"}

	// Reconcile 1: Pending → Proposed (analysis complete)
	if _, err := reconcileOnce(r, "fix-crash"); err != nil {
		t.Fatalf("reconcile 1: %v", err)
	}
	p, _ := getAgenticRun(r, "fix-crash")
	if agenticv1alpha1.DerivePhase(p.Status.Conditions) != agenticv1alpha1.AgenticRunPhaseProposed {
		t.Fatalf("expected Proposed, got %s", agenticv1alpha1.DerivePhase(p.Status.Conditions))
	}

	// Approve and reconcile 2: Proposed → Completed (advisory-only, no execution)
	approveAgenticRun(t, fc, "fix-crash")
	if _, err := reconcileOnce(r, "fix-crash"); err != nil {
		t.Fatalf("reconcile 2: %v", err)
	}
	p, _ = getAgenticRun(r, "fix-crash")
	if agenticv1alpha1.DerivePhase(p.Status.Conditions) != agenticv1alpha1.AgenticRunPhaseCompleted {
		t.Fatalf("expected Completed, got %s", agenticv1alpha1.DerivePhase(p.Status.Conditions))
	}

	// Submit revision on the completed run
	reviseAgenticRun(t, fc, "fix-crash", "re-analyse with different focus")

	// Reconcile 3: Completed → revision → Proposed
	if _, err := reconcileOnce(r, "fix-crash"); err != nil {
		t.Fatalf("reconcile 3 (revision from Completed): %v", err)
	}
	p, _ = getAgenticRun(r, "fix-crash")
	if agenticv1alpha1.DerivePhase(p.Status.Conditions) != agenticv1alpha1.AgenticRunPhaseProposed {
		t.Fatalf("expected Proposed after revision from Completed, got %s", agenticv1alpha1.DerivePhase(p.Status.Conditions))
	}
	if analyzed := meta.FindStatusCondition(p.Status.Conditions, agenticv1alpha1.AgenticRunConditionAnalyzed); analyzed == nil || analyzed.ObservedGeneration != p.Generation {
		t.Fatalf("expected observedGeneration to equal current generation %d after revision from Completed", p.Generation)
	}
}

// TestReconcile_RevisionClearsTerminalTime verifies that a run which already
// carries a terminalTime (stamped by handleTerminalTTL, OLS-3566) has it
// cleared once a revision moves it back out of the terminal phase --
// otherwise a later terminal phase would compute TTL expiry off the stale,
// earlier terminal event instead of a fresh one (run-lifecycle.md rule 23/24).
func TestReconcile_RevisionClearsTerminalTime(t *testing.T) {
	scheme := testScheme()
	run := &agenticv1alpha1.AgenticRun{
		ObjectMeta: metav1.ObjectMeta{Name: "fix-crash", Namespace: "default"},
		Spec: agenticv1alpha1.AgenticRunSpec{
			Request:          "Investigate issue",
			Tools:            testTools(),
			TargetNamespaces: []string{"production"},
			Analysis:         agenticv1alpha1.AgenticRunStep{Agent: "default"},
		},
	}

	objs := append([]client.Object{run}, defaultObjects()...)
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
		WithStatusSubresource(run, &agenticv1alpha1.AnalysisResult{}, &agenticv1alpha1.ExecutionResult{}, &agenticv1alpha1.VerificationResult{}, &agenticv1alpha1.EscalationResult{}).Build()

	r := &AgenticRunReconciler{Client: fc, Agent: newTestAgentCaller(), Namespace: "default"}

	if _, err := reconcileOnce(r, "fix-crash"); err != nil {
		t.Fatalf("reconcile 1: %v", err)
	}
	approveAgenticRun(t, fc, "fix-crash")
	if _, err := reconcileOnce(r, "fix-crash"); err != nil {
		t.Fatalf("reconcile 2: %v", err)
	}
	p, _ := getAgenticRun(r, "fix-crash")
	if agenticv1alpha1.DerivePhase(p.Status.Conditions) != agenticv1alpha1.AgenticRunPhaseCompleted {
		t.Fatalf("expected Completed, got %s", agenticv1alpha1.DerivePhase(p.Status.Conditions))
	}

	// Simulate handleTerminalTTL having already stamped terminalTime on an
	// earlier reconcile of this terminal run.
	staleTerminalTime := metav1.NewTime(time.Now().Add(-1 * time.Hour))
	base := p.DeepCopy()
	p.Status.TerminalTime = &staleTerminalTime
	if err := fc.Status().Patch(context.Background(), p, client.MergeFrom(base)); err != nil {
		t.Fatalf("stamp stale terminalTime: %v", err)
	}

	reviseAgenticRun(t, fc, "fix-crash", "re-analyse with different focus")
	if _, err := reconcileOnce(r, "fix-crash"); err != nil {
		t.Fatalf("reconcile 3 (revision from Completed): %v", err)
	}

	p, _ = getAgenticRun(r, "fix-crash")
	if agenticv1alpha1.DerivePhase(p.Status.Conditions) != agenticv1alpha1.AgenticRunPhaseProposed {
		t.Fatalf("expected Proposed after revision from Completed, got %s", agenticv1alpha1.DerivePhase(p.Status.Conditions))
	}
	if p.Status.TerminalTime != nil {
		t.Errorf("expected terminalTime to be cleared once revision moves run out of terminal phase, got %v", p.Status.TerminalTime)
	}
}

func TestReconcile_RevisionFromFailed(t *testing.T) {
	agent := newTestAgentCaller()
	scheme := testScheme()

	// Advisory-only run
	run := &agenticv1alpha1.AgenticRun{
		ObjectMeta: metav1.ObjectMeta{Name: "fix-crash", Namespace: "default"},
		Spec: agenticv1alpha1.AgenticRunSpec{
			Request:          "Investigate issue",
			Tools:            testTools(),
			TargetNamespaces: []string{"production"},
			Analysis:         agenticv1alpha1.AgenticRunStep{Agent: "default"},
		},
	}

	objs := append([]client.Object{run}, defaultObjects()...)
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
		WithStatusSubresource(run, &agenticv1alpha1.AnalysisResult{}, &agenticv1alpha1.ExecutionResult{}, &agenticv1alpha1.VerificationResult{}, &agenticv1alpha1.EscalationResult{}).Build()

	r := &AgenticRunReconciler{Client: fc, Agent: agent, Namespace: "default"}

	// Make analysis fail
	agent.analyzeErr = fmt.Errorf("LLM timeout")

	// Reconcile 1: Pending → Failed
	if _, err := reconcileOnce(r, "fix-crash"); err != nil {
		t.Fatalf("reconcile 1: %v", err)
	}
	p, _ := getAgenticRun(r, "fix-crash")
	if agenticv1alpha1.DerivePhase(p.Status.Conditions) != agenticv1alpha1.AgenticRunPhaseFailed {
		t.Fatalf("expected Failed, got %s", agenticv1alpha1.DerivePhase(p.Status.Conditions))
	}

	// Fix the agent and submit revision
	agent.analyzeErr = nil
	reviseAgenticRun(t, fc, "fix-crash", "retry after timeout")

	// Reconcile 2: Failed → revision → Proposed
	if _, err := reconcileOnce(r, "fix-crash"); err != nil {
		t.Fatalf("reconcile 2 (revision from Failed): %v", err)
	}
	p, _ = getAgenticRun(r, "fix-crash")
	if agenticv1alpha1.DerivePhase(p.Status.Conditions) != agenticv1alpha1.AgenticRunPhaseProposed {
		t.Fatalf("expected Proposed after revision from Failed, got %s", agenticv1alpha1.DerivePhase(p.Status.Conditions))
	}
	if analyzed := meta.FindStatusCondition(p.Status.Conditions, agenticv1alpha1.AgenticRunConditionAnalyzed); analyzed == nil || analyzed.ObservedGeneration != p.Generation {
		t.Fatalf("expected observedGeneration to equal current generation %d after revision from Failed", p.Generation)
	}
}

func TestReconcile_ExecutionRBACCreatedOnApproval(t *testing.T) {
	agent := newTestAgentCaller()
	agent.analyzeResult = &AnalysisOutput{
		Success:        true,
		ActionRequired: ptr.To(true),
		Options: []agenticv1alpha1.RemediationOption{{
			Title: "Increase memory",
			Diagnosis: agenticv1alpha1.DiagnosisResult{
				Summary: "OOM", RootCause: "Low limit",
			},
			RemediationPlan: agenticv1alpha1.RemediationPlan{
				Description: "Increase to 512Mi",
				Actions:     []agenticv1alpha1.ProposedAction{{Command: "kubectl patch deployment/web -n production -p '{}'", Type: "mutation", Description: "Patch deploy"}},
				Reversible:  agenticv1alpha1.ReversibilityReversible,
			},
			RBAC: agenticv1alpha1.RBACResult{
				NamespaceScoped: []agenticv1alpha1.RBACRule{{
					APIGroups:     []string{"apps"},
					Resources:     []string{"deployments"},
					Verbs:         []string{"get", "patch"},
					Justification: "Patch deployment memory",
				}},
				ClusterScoped: []agenticv1alpha1.RBACRule{{
					APIGroups:     []string{""},
					Resources:     []string{"nodes"},
					Verbs:         []string{"get", "list"},
					Justification: "Check node capacity",
				}},
			},
		}},
	}

	scheme := testScheme()
	run := testAgenticRun()

	objs := append([]client.Object{run}, defaultObjects()...)
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
		WithStatusSubresource(run, &agenticv1alpha1.AnalysisResult{}, &agenticv1alpha1.ExecutionResult{}, &agenticv1alpha1.VerificationResult{}, &agenticv1alpha1.EscalationResult{}).Build()

	r := &AgenticRunReconciler{Client: fc, Agent: agent, Namespace: "default"}

	// Pending → Proposed (analysis complete)
	reconcileOnce(r, "fix-crash")
	p, _ := getAgenticRun(r, "fix-crash")
	if agenticv1alpha1.DerivePhase(p.Status.Conditions) != agenticv1alpha1.AgenticRunPhaseProposed {
		t.Fatalf("expected Proposed, got %s", agenticv1alpha1.DerivePhase(p.Status.Conditions))
	}

	// Approve
	approveAgenticRun(t, fc, "fix-crash")

	// Executing → Verifying
	reconcileOnce(r, "fix-crash")
	p, _ = getAgenticRun(r, "fix-crash")
	if agenticv1alpha1.DerivePhase(p.Status.Conditions) != agenticv1alpha1.AgenticRunPhaseVerifying {
		t.Fatalf("expected Verifying, got %s", agenticv1alpha1.DerivePhase(p.Status.Conditions))
	}

	// RBAC was created during execution and cleaned up immediately after.
	// Verify via annotation (proves RBAC was materialized in "production" namespace).
	p, _ = getAgenticRun(r, "fix-crash")
	if p.Annotations[rbacNamespacesAnnotation] != "production" {
		t.Fatalf("expected rbac-namespaces annotation 'production', got %q", p.Annotations[rbacNamespacesAnnotation])
	}

	// Verify RBAC is already cleaned up (deleted immediately after execution completes).
	roleName := executionRoleName("fix-crash")
	var role rbacv1.Role
	if err := fc.Get(context.Background(), types.NamespacedName{Name: roleName, Namespace: "production"}, &role); err == nil {
		t.Fatal("Role should be cleaned up after execution completes")
	}
	crName := clusterRoleName("fix-crash")
	var cr rbacv1.ClusterRole
	if err := fc.Get(context.Background(), types.NamespacedName{Name: crName}, &cr); err == nil {
		t.Fatal("ClusterRole should be cleaned up after execution completes")
	}

	// Complete lifecycle
	reconcileOnce(r, "fix-crash")
	p, _ = getAgenticRun(r, "fix-crash")
	if agenticv1alpha1.DerivePhase(p.Status.Conditions) != agenticv1alpha1.AgenticRunPhaseCompleted {
		t.Fatalf("expected Completed, got %s", agenticv1alpha1.DerivePhase(p.Status.Conditions))
	}
}

func TestReconcile_ExecutionRBACCleanedOnFailure(t *testing.T) {
	agent := newTestAgentCaller()
	agent.analyzeResult = &AnalysisOutput{
		Success:        true,
		ActionRequired: ptr.To(true),
		Options: []agenticv1alpha1.RemediationOption{{
			Title: "Fix it",
			Diagnosis: agenticv1alpha1.DiagnosisResult{
				Summary: "Broken", RootCause: "Bug",
			},
			RemediationPlan: agenticv1alpha1.RemediationPlan{
				Description: "Apply fix",
				Actions:     []agenticv1alpha1.ProposedAction{{Command: "kubectl patch deployment/web -n production -p '{}'", Type: "mutation", Description: "Patch"}},
				Reversible:  agenticv1alpha1.ReversibilityReversible,
			},
			RBAC: agenticv1alpha1.RBACResult{
				NamespaceScoped: []agenticv1alpha1.RBACRule{{
					APIGroups: []string{"apps"}, Resources: []string{"deployments"},
					Verbs: []string{"get", "patch"}, Justification: "Fix deploy",
				}},
			},
		}},
	}

	scheme := testScheme()
	run := testAgenticRun()

	objs := append([]client.Object{run}, defaultObjects()...)
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
		WithStatusSubresource(run, &agenticv1alpha1.AnalysisResult{}, &agenticv1alpha1.ExecutionResult{}, &agenticv1alpha1.VerificationResult{}, &agenticv1alpha1.EscalationResult{}).Build()

	r := &AgenticRunReconciler{Client: fc, Agent: agent, Namespace: "default"}

	// Analysis → approve
	reconcileOnce(r, "fix-crash")
	approveAgenticRun(t, fc, "fix-crash")

	// Execution succeeds, creates RBAC, but verification will fail with system error
	reconcileOnce(r, "fix-crash")
	p, _ := getAgenticRun(r, "fix-crash")
	if agenticv1alpha1.DerivePhase(p.Status.Conditions) != agenticv1alpha1.AgenticRunPhaseVerifying {
		t.Fatalf("expected Verifying, got %s", agenticv1alpha1.DerivePhase(p.Status.Conditions))
	}

	// RBAC should already be cleaned up after execution completes (before verification starts)
	roleName := executionRoleName("fix-crash")
	var role rbacv1.Role
	if err := fc.Get(context.Background(), types.NamespacedName{Name: roleName, Namespace: "production"}, &role); err == nil {
		t.Fatal("Role should be cleaned up immediately after execution completes")
	}
	var bindingCheck rbacv1.RoleBinding
	if err := fc.Get(context.Background(), types.NamespacedName{Name: roleName, Namespace: "production"}, &bindingCheck); err == nil {
		t.Fatal("RoleBinding should be cleaned up immediately after execution completes")
	}
}

// TestFullLifecycle_WithSandboxAgent exercises the full Pending → Completed
// lifecycle using SandboxAgentCaller with mocked sandbox and HTTP, proving
// the real agent caller implementation works through the reconciler.
func TestFullLifecycle_WithSandboxAgent(t *testing.T) {
	analysisJSON, _ := json.Marshal(analysisResponse{
		Success: true,
		Options: []agenticv1alpha1.RemediationOption{{
			Title: "Increase memory limit",
			Diagnosis: agenticv1alpha1.DiagnosisResult{
				Summary:   "Pod OOMKilled due to 256Mi memory limit",
				RootCause: "Memory limit too low for workload",
			},
			RemediationPlan: agenticv1alpha1.RemediationPlan{
				Description: "Increase deployment memory limit to 512Mi",
				Actions:     []agenticv1alpha1.ProposedAction{{Command: "kubectl set resources deployment/web -n production --limits=memory=512Mi", Type: "mutation", Description: "Patch deployment memory limit"}},
				Reversible:  agenticv1alpha1.ReversibilityReversible,
			},
		}},
	})

	executionJSON, _ := json.Marshal(executionResponse{
		Success: true,
		ActionsTaken: []agenticv1alpha1.ExecutionAction{{
			Type:        "patch",
			Description: "Patched deployment/web memory limit to 512Mi",
			Outcome:     agenticv1alpha1.ActionOutcomeSucceeded,
		}},
	})

	verificationJSON, _ := json.Marshal(verificationResponse{
		Success: true,
		Checks: []agenticv1alpha1.VerifyCheck{{
			Name:   "pod-running",
			Source: "oc",
			Value:  "Running",
			Result: agenticv1alpha1.CheckResultPassed,
		}},
		Summary: "All verification checks passed",
	})

	sandboxAgent, sandbox := newMockSandboxAgent(string(analysisJSON), string(executionJSON), string(verificationJSON))

	scheme := testScheme()
	run := testAgenticRun()

	objs := append([]client.Object{run}, defaultObjects()...)
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
		WithStatusSubresource(run, &agenticv1alpha1.AnalysisResult{}, &agenticv1alpha1.ExecutionResult{}, &agenticv1alpha1.VerificationResult{}, &agenticv1alpha1.EscalationResult{}).Build()

	r := &AgenticRunReconciler{Client: fc, Agent: sandboxAgent, Namespace: "default"}

	// Reconcile 1: Pending → Executing (via sandbox analysis)
	if _, err := reconcileOnce(r, "fix-crash"); err != nil {
		t.Fatalf("analysis reconcile: %v", err)
	}
	p, _ := getAgenticRun(r, "fix-crash")
	if agenticv1alpha1.DerivePhase(p.Status.Conditions) != agenticv1alpha1.AgenticRunPhaseProposed {
		t.Fatalf("expected Proposed, got %s", agenticv1alpha1.DerivePhase(p.Status.Conditions))
	}
	if len(p.Status.Steps.Analysis.Results) != 1 {
		t.Fatalf("expected 1 analysis result, got %d", len(p.Status.Steps.Analysis.Results))
	}
	var ar agenticv1alpha1.AnalysisResult
	if err := fc.Get(context.Background(), types.NamespacedName{Name: p.Status.Steps.Analysis.Results[0].Name, Namespace: "default"}, &ar); err != nil {
		t.Fatalf("get AnalysisResult: %v", err)
	}
	if len(ar.Status.Options) != 1 {
		t.Fatalf("expected 1 option, got %d", len(ar.Status.Options))
	}
	if ar.Status.Options[0].Title != "Increase memory limit" {
		t.Errorf("option title = %q", ar.Status.Options[0].Title)
	}

	// Approve
	approveAgenticRun(t, fc, "fix-crash")

	// Reconcile 2: Executing → Verifying (via sandbox execution)
	result, err := reconcileOnce(r, "fix-crash")
	if err != nil {
		t.Fatalf("execution reconcile: %v", err)
	}
	if result.Requeue {
		t.Error("should not requeue — watch event drives next reconcile")
	}
	p, _ = getAgenticRun(r, "fix-crash")
	if agenticv1alpha1.DerivePhase(p.Status.Conditions) != agenticv1alpha1.AgenticRunPhaseVerifying {
		t.Fatalf("expected Verifying, got %s", agenticv1alpha1.DerivePhase(p.Status.Conditions))
	}
	if len(p.Status.Steps.Execution.Results) != 1 {
		t.Fatalf("expected 1 execution result, got %d", len(p.Status.Steps.Execution.Results))
	}
	var er agenticv1alpha1.ExecutionResult
	if err := fc.Get(context.Background(), types.NamespacedName{Name: p.Status.Steps.Execution.Results[0].Name, Namespace: "default"}, &er); err != nil {
		t.Fatalf("get ExecutionResult: %v", err)
	}
	if len(er.Status.ActionsTaken) != 1 {
		t.Fatalf("expected 1 action, got %d", len(er.Status.ActionsTaken))
	}
	if er.Status.ActionsTaken[0].Outcome != agenticv1alpha1.ActionOutcomeSucceeded {
		t.Errorf("action outcome = %q", er.Status.ActionsTaken[0].Outcome)
	}
	// Reconcile 3: Verifying → Completed (via sandbox verification)
	if _, err := reconcileOnce(r, "fix-crash"); err != nil {
		t.Fatalf("verification reconcile: %v", err)
	}
	p, _ = getAgenticRun(r, "fix-crash")
	if agenticv1alpha1.DerivePhase(p.Status.Conditions) != agenticv1alpha1.AgenticRunPhaseCompleted {
		t.Fatalf("expected Completed, got %s", agenticv1alpha1.DerivePhase(p.Status.Conditions))
	}
	if len(p.Status.Steps.Verification.Results) != 1 {
		t.Fatalf("expected 1 verification result, got %d", len(p.Status.Steps.Verification.Results))
	}
	var vr agenticv1alpha1.VerificationResult
	if err := fc.Get(context.Background(), types.NamespacedName{Name: p.Status.Steps.Verification.Results[0].Name, Namespace: "default"}, &vr); err != nil {
		t.Fatalf("get VerificationResult: %v", err)
	}
	if vr.Status.Summary != "All verification checks passed" {
		t.Errorf("summary = %q", vr.Status.Summary)
	}
	if len(vr.Status.Checks) != 1 {
		t.Fatalf("expected 1 check, got %d", len(vr.Status.Checks))
	}
	if vr.Status.Checks[0].Result != agenticv1alpha1.CheckResultPassed {
		t.Errorf("check result = %q", vr.Status.Checks[0].Result)
	}

	// Verify sandbox was claimed for each phase (release is deferred to terminal phase)
	if sandbox.claimCalls != 3 {
		t.Errorf("sandbox claim calls = %d, want 3 (analysis + execution + verification)", sandbox.claimCalls)
	}
	if sandbox.releaseCalls != 0 {
		t.Errorf("sandbox release calls = %d, want 0 (reconciler releases at terminal phase)", sandbox.releaseCalls)
	}
}

func TestReconcile_ExecutingPhase_DoesNotReExecute(t *testing.T) {
	scheme := testScheme()
	run := testAgenticRun()

	objs := append([]client.Object{run}, defaultObjects()...)
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
		WithStatusSubresource(run, &agenticv1alpha1.AnalysisResult{}, &agenticv1alpha1.ExecutionResult{}, &agenticv1alpha1.VerificationResult{}, &agenticv1alpha1.EscalationResult{}).Build()

	agent := newTestAgentCaller()
	r := &AgenticRunReconciler{Client: fc, Agent: agent, Namespace: "default"}

	// Run analysis
	reconcileOnce(r, "fix-crash")

	// Approve → execution runs → phase should be Verifying
	approveAgenticRun(t, fc, "fix-crash")
	reconcileOnce(r, "fix-crash")

	p, _ := getAgenticRun(r, "fix-crash")
	if agenticv1alpha1.DerivePhase(p.Status.Conditions) != agenticv1alpha1.AgenticRunPhaseVerifying {
		t.Fatalf("expected Verifying after execution, got %s", agenticv1alpha1.DerivePhase(p.Status.Conditions))
	}

	// Simulate stale cache: set Executed back to Unknown (as if informer lagged)
	base := p.DeepCopy()
	meta.SetStatusCondition(&p.Status.Conditions, metav1.Condition{
		Type:   agenticv1alpha1.AgenticRunConditionExecuted,
		Status: metav1.ConditionUnknown,
		Reason: "ExecutionInProgress",
	})
	if err := fc.Status().Patch(context.Background(), p, client.MergeFrom(base)); err != nil {
		t.Fatalf("patch conditions to Executing: %v", err)
	}

	// Reconcile — should NOT re-execute (in-progress guard), stays Executing
	reconcileOnce(r, "fix-crash")

	p, _ = getAgenticRun(r, "fix-crash")
	if agenticv1alpha1.DerivePhase(p.Status.Conditions) != agenticv1alpha1.AgenticRunPhaseExecuting {
		t.Fatalf("expected Executing (in-progress guard), got %s", agenticv1alpha1.DerivePhase(p.Status.Conditions))
	}
}

func TestReconcile_ExecutionMutationFailed_FailsStep(t *testing.T) {
	scheme := testScheme()
	run := testAgenticRun()

	objs := append([]client.Object{run}, defaultObjects()...)
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
		WithStatusSubresource(run, &agenticv1alpha1.AnalysisResult{}, &agenticv1alpha1.ExecutionResult{}, &agenticv1alpha1.VerificationResult{}, &agenticv1alpha1.EscalationResult{}).Build()

	agent := newTestAgentCaller()
	agent.executeResult = &ExecutionOutput{
		Success:      false,
		ActionsTaken: []agenticv1alpha1.ExecutionAction{{Type: "patch", Description: "Failed patch", Outcome: agenticv1alpha1.ActionOutcomeFailed}},
	}
	r := &AgenticRunReconciler{Client: fc, Agent: agent, Namespace: "default"}

	reconcileOnce(r, "fix-crash")
	approveAgenticRun(t, fc, "fix-crash")
	reconcileOnce(r, "fix-crash")

	p, _ := getAgenticRun(r, "fix-crash")
	if agenticv1alpha1.DerivePhase(p.Status.Conditions) != agenticv1alpha1.AgenticRunPhaseFailed {
		t.Fatalf("expected Failed when mutation action failed, got %s", agenticv1alpha1.DerivePhase(p.Status.Conditions))
	}
}

func TestReconcile_ExecutionPreCheckFailed_ProceedsToVerification(t *testing.T) {
	scheme := testScheme()
	run := testAgenticRun()

	objs := append([]client.Object{run}, defaultObjects()...)
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
		WithStatusSubresource(run, &agenticv1alpha1.AnalysisResult{}, &agenticv1alpha1.ExecutionResult{}, &agenticv1alpha1.VerificationResult{}, &agenticv1alpha1.EscalationResult{}).Build()

	agent := newTestAgentCaller()
	agent.executeResult = &ExecutionOutput{
		Success: false,
		ActionsTaken: []agenticv1alpha1.ExecutionAction{
			{Type: "pre-check", Description: "Confirmed problem exists", Outcome: agenticv1alpha1.ActionOutcomeFailed},
			{Type: "patch", Description: "Patched deployment", Outcome: agenticv1alpha1.ActionOutcomeSucceeded},
		},
	}
	r := &AgenticRunReconciler{Client: fc, Agent: agent, Namespace: "default"}

	reconcileOnce(r, "fix-crash")
	approveAgenticRun(t, fc, "fix-crash")
	reconcileOnce(r, "fix-crash")

	p, _ := getAgenticRun(r, "fix-crash")
	phase := agenticv1alpha1.DerivePhase(p.Status.Conditions)
	if phase != agenticv1alpha1.AgenticRunPhaseVerifying {
		t.Fatalf("expected Verifying when only pre-check failed (observational), got %s", phase)
	}
}

func TestReconcile_ExecutionInlineVerificationFailed_ProceedsToVerification(t *testing.T) {
	scheme := testScheme()
	run := testAgenticRun()

	objs := append([]client.Object{run}, defaultObjects()...)
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
		WithStatusSubresource(run, &agenticv1alpha1.AnalysisResult{}, &agenticv1alpha1.ExecutionResult{}, &agenticv1alpha1.VerificationResult{}, &agenticv1alpha1.EscalationResult{}).Build()

	agent := newTestAgentCaller()
	agent.executeResult = &ExecutionOutput{
		Success: false,
		ActionsTaken: []agenticv1alpha1.ExecutionAction{
			{Type: "patch", Description: "Patched NetworkPolicy", Outcome: agenticv1alpha1.ActionOutcomeSucceeded},
			{Type: "verification", Description: "Checked frontend logs", Outcome: agenticv1alpha1.ActionOutcomeFailed},
		},
	}
	r := &AgenticRunReconciler{Client: fc, Agent: agent, Namespace: "default"}

	reconcileOnce(r, "fix-crash")
	approveAgenticRun(t, fc, "fix-crash")
	reconcileOnce(r, "fix-crash")

	p, _ := getAgenticRun(r, "fix-crash")
	phase := agenticv1alpha1.DerivePhase(p.Status.Conditions)
	if phase != agenticv1alpha1.AgenticRunPhaseVerifying {
		t.Fatalf("expected Verifying when only observation action failed, got %s", phase)
	}
}

func TestReconcile_VerificationOutcomeFailed_Escalates(t *testing.T) {
	scheme := testScheme()
	run := testAgenticRun()

	objs := append([]client.Object{run}, defaultObjects()...)
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
		WithStatusSubresource(run, &agenticv1alpha1.AnalysisResult{}, &agenticv1alpha1.ExecutionResult{}, &agenticv1alpha1.VerificationResult{}, &agenticv1alpha1.EscalationResult{}).Build()

	agent := newTestAgentCaller()
	agent.verifyResult = &VerificationOutput{
		Success: false,
		Checks:  []agenticv1alpha1.VerifyCheck{{Name: "health", Result: agenticv1alpha1.CheckResultFailed}},
		Summary: "Health check failed",
	}
	r := &AgenticRunReconciler{Client: fc, Agent: agent, Namespace: "default"}

	// Analysis → Executing → Approve → Execute → Verify (fail) → escalate
	reconcileOnce(r, "fix-crash")
	approveAgenticRun(t, fc, "fix-crash")
	reconcileOnce(r, "fix-crash") // execution
	reconcileOnce(r, "fix-crash") // verification → escalate

	p, _ := getAgenticRun(r, "fix-crash")
	if agenticv1alpha1.DerivePhase(p.Status.Conditions) != agenticv1alpha1.AgenticRunPhaseEscalating {
		t.Fatalf("expected Escalating when verification success=false, got %s", agenticv1alpha1.DerivePhase(p.Status.Conditions))
	}
	verified := meta.FindStatusCondition(p.Status.Conditions, agenticv1alpha1.AgenticRunConditionVerified)
	if verified == nil || verified.Status != metav1.ConditionFalse || verified.Reason != agenticv1alpha1.ReasonVerificationFailed {
		t.Fatalf("expected Verified=False/VerificationFailed, got %+v", verified)
	}
}

func TestReconcile_ExecutionSelectsOption(t *testing.T) {
	scheme := testScheme()
	run := testAgenticRun()

	objs := append([]client.Object{run}, defaultObjects()...)
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
		WithStatusSubresource(run, &agenticv1alpha1.AnalysisResult{}, &agenticv1alpha1.ExecutionResult{}, &agenticv1alpha1.VerificationResult{}, &agenticv1alpha1.EscalationResult{}).Build()

	agent := newTestAgentCaller()
	agent.analyzeResult = &AnalysisOutput{
		Success:        true,
		ActionRequired: ptr.To(true),
		Options: []agenticv1alpha1.RemediationOption{
			{Title: "Option A", Diagnosis: agenticv1alpha1.DiagnosisResult{Summary: "diag-A"}},
			{Title: "Option B", Diagnosis: agenticv1alpha1.DiagnosisResult{Summary: "diag-B"}},
			{Title: "Option C", Diagnosis: agenticv1alpha1.DiagnosisResult{Summary: "diag-C"}},
		},
	}
	r := &AgenticRunReconciler{Client: fc, Agent: agent, Namespace: "default"}

	// Analysis
	reconcileOnce(r, "fix-crash")

	p, _ := getAgenticRun(r, "fix-crash")
	if len(p.Status.Steps.Analysis.Results) == 0 {
		t.Fatal("expected analysis results after analysis")
	}
	var ar agenticv1alpha1.AnalysisResult
	if err := fc.Get(context.Background(), types.NamespacedName{Name: p.Status.Steps.Analysis.Results[0].Name, Namespace: "default"}, &ar); err != nil {
		t.Fatalf("get AnalysisResult: %v", err)
	}
	if len(ar.Status.Options) != 3 {
		t.Fatalf("expected 3 options in AnalysisResult, got %d", len(ar.Status.Options))
	}

	// Approve option 1 (Option B)
	approveAgenticRunWithOption(t, fc, "fix-crash", 1)

	// Execution reconcile — should trim to just Option B
	reconcileOnce(r, "fix-crash")

	p, _ = getAgenticRun(r, "fix-crash")
	if err := fc.Get(context.Background(), types.NamespacedName{Name: p.Status.Steps.Analysis.Results[0].Name, Namespace: "default"}, &ar); err != nil {
		t.Fatalf("get AnalysisResult after trim: %v", err)
	}
	if len(ar.Status.Options) != 1 {
		t.Fatalf("expected 1 option after trim, got %d", len(ar.Status.Options))
	}
	if ar.Status.Options[0].Title != "Option B" {
		t.Errorf("expected trimmed option %q, got %q", "Option B", ar.Status.Options[0].Title)
	}
}

func TestReconcile_ExecutionSingleOption(t *testing.T) {
	scheme := testScheme()
	run := testAgenticRun()

	objs := append([]client.Object{run}, defaultObjects()...)
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
		WithStatusSubresource(run, &agenticv1alpha1.AnalysisResult{}, &agenticv1alpha1.ExecutionResult{}, &agenticv1alpha1.VerificationResult{}, &agenticv1alpha1.EscalationResult{}).Build()

	r := &AgenticRunReconciler{Client: fc, Agent: newTestAgentCaller(), Namespace: "default"}

	// Analysis (default stub returns 1 option)
	reconcileOnce(r, "fix-crash")

	// Approve option 0
	approveAgenticRun(t, fc, "fix-crash")

	// Execution
	reconcileOnce(r, "fix-crash")

	p, _ := getAgenticRun(r, "fix-crash")
	if len(p.Status.Steps.Analysis.Results) == 0 {
		t.Fatal("expected analysis results")
	}
}

func TestReconcile_TrimOptionsOnExecution(t *testing.T) {
	scheme := testScheme()
	run := testAgenticRun()

	objs := append([]client.Object{run}, defaultObjects()...)
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
		WithStatusSubresource(run, &agenticv1alpha1.AnalysisResult{}, &agenticv1alpha1.ExecutionResult{}, &agenticv1alpha1.VerificationResult{}, &agenticv1alpha1.EscalationResult{}).Build()

	agent := newTestAgentCaller()
	agent.analyzeResult = &AnalysisOutput{
		Success:        true,
		ActionRequired: ptr.To(true),
		Options: []agenticv1alpha1.RemediationOption{
			{Title: "Option A", Diagnosis: agenticv1alpha1.DiagnosisResult{Summary: "diag-A"}},
			{Title: "Option B", Diagnosis: agenticv1alpha1.DiagnosisResult{Summary: "diag-B"}},
			{Title: "Option C", Diagnosis: agenticv1alpha1.DiagnosisResult{Summary: "diag-C"}},
		},
	}
	agent.verifyResult = &VerificationOutput{
		Success: false,
		Checks:  []agenticv1alpha1.VerifyCheck{{Name: "health", Result: agenticv1alpha1.CheckResultFailed}},
		Summary: "Health check failed",
	}
	r := &AgenticRunReconciler{Client: fc, Agent: agent, Namespace: "default"}

	// Analysis
	reconcileOnce(r, "fix-crash")

	// Approve option 2 (Option C)
	approveAgenticRunWithOption(t, fc, "fix-crash", 2)

	// Execution — should trim options to just Option C
	reconcileOnce(r, "fix-crash")

	p, _ := getAgenticRun(r, "fix-crash")

	// Verify AnalysisResult was trimmed to 1 option
	var ar agenticv1alpha1.AnalysisResult
	if err := fc.Get(context.Background(), types.NamespacedName{Name: p.Status.Steps.Analysis.Results[0].Name, Namespace: "default"}, &ar); err != nil {
		t.Fatalf("get AnalysisResult: %v", err)
	}
	if len(ar.Status.Options) != 1 {
		t.Fatalf("expected 1 option in AnalysisResult after trim, got %d", len(ar.Status.Options))
	}
	if ar.Status.Options[0].Title != "Option C" {
		t.Errorf("expected trimmed option title %q, got %q", "Option C", ar.Status.Options[0].Title)
	}

	// Verification fails → escalates directly (no retry)
	reconcileOnce(r, "fix-crash")

	p, _ = getAgenticRun(r, "fix-crash")
	if agenticv1alpha1.DerivePhase(p.Status.Conditions) != agenticv1alpha1.AgenticRunPhaseEscalating {
		t.Fatalf("expected Escalating, got %s", agenticv1alpha1.DerivePhase(p.Status.Conditions))
	}

	// AnalysisResult should still have just 1 option after escalation
	if err := fc.Get(context.Background(), types.NamespacedName{Name: p.Status.Steps.Analysis.Results[0].Name, Namespace: "default"}, &ar); err != nil {
		t.Fatalf("get AnalysisResult after escalation: %v", err)
	}
	if len(ar.Status.Options) != 1 {
		t.Fatalf("expected 1 option after escalation, got %d", len(ar.Status.Options))
	}
	if ar.Status.Options[0].Title != "Option C" {
		t.Errorf("expected option %q after escalation, got %q", "Option C", ar.Status.Options[0].Title)
	}
}

func assertResultConditions(t *testing.T, conditions []metav1.Condition, wantReason string) {
	t.Helper()
	if len(conditions) < 2 {
		t.Fatalf("expected at least 2 conditions (Started, Completed), got %d", len(conditions))
	}
	var started, completed *metav1.Condition
	for i := range conditions {
		switch conditions[i].Type {
		case agenticv1alpha1.ResultConditionStarted:
			started = &conditions[i]
		case agenticv1alpha1.ResultConditionCompleted:
			completed = &conditions[i]
		}
	}
	if started == nil {
		t.Fatal("missing Started condition on result CR")
	}
	if started.Status != metav1.ConditionTrue {
		t.Errorf("Started condition status = %s, want True", started.Status)
	}
	if started.Reason != agenticv1alpha1.ResultReasonStepStarted {
		t.Errorf("Started condition reason = %q, want %q", started.Reason, agenticv1alpha1.ResultReasonStepStarted)
	}
	if completed == nil {
		t.Fatal("missing Completed condition on result CR")
	}
	if completed.Status != metav1.ConditionTrue {
		t.Errorf("Completed condition status = %s, want True", completed.Status)
	}
	if completed.Reason != wantReason {
		t.Errorf("Completed condition reason = %q, want %q", completed.Reason, wantReason)
	}
	if !started.LastTransitionTime.Before(&completed.LastTransitionTime) && started.LastTransitionTime.Time != completed.LastTransitionTime.Time {
		t.Errorf("Started time (%v) should be <= Completed time (%v)", started.LastTransitionTime, completed.LastTransitionTime)
	}
}

func TestResultCR_FailureConditions(t *testing.T) {
	agent := newTestAgentCaller()
	agent.analyzeErr = fmt.Errorf("LLM call failed")

	scheme := testScheme()
	run := testAgenticRun()

	objs := append([]client.Object{run}, defaultObjects()...)
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
		WithStatusSubresource(run, &agenticv1alpha1.AnalysisResult{}, &agenticv1alpha1.ExecutionResult{}, &agenticv1alpha1.VerificationResult{}, &agenticv1alpha1.EscalationResult{}).Build()

	r := &AgenticRunReconciler{Client: fc, Agent: agent, Namespace: "default"}

	reconcileOnce(r, "fix-crash")
	p, _ := getAgenticRun(r, "fix-crash")

	if len(p.Status.Steps.Analysis.Results) == 0 {
		t.Fatal("expected failure result ref")
	}
	ref := p.Status.Steps.Analysis.Results[0]
	if ref.Outcome != agenticv1alpha1.ActionOutcomeFailed {
		t.Fatalf("expected Failed outcome on ref, got %s", ref.Outcome)
	}

	var ar agenticv1alpha1.AnalysisResult
	if err := fc.Get(context.Background(), types.NamespacedName{Name: ref.Name, Namespace: "default"}, &ar); err != nil {
		t.Fatalf("get AnalysisResult: %v", err)
	}

	assertResultConditions(t, ar.Status.Conditions, "Failed")
	if ar.Status.FailureReason == "" {
		t.Error("expected failureReason to be set")
	}
}

func TestConditionTime(t *testing.T) {
	now := metav1.Now()
	conditions := []metav1.Condition{
		{Type: "Foo", Status: metav1.ConditionTrue, LastTransitionTime: now, Reason: "R"},
		{Type: "Bar", Status: metav1.ConditionFalse, LastTransitionTime: now, Reason: "R"},
	}

	got := conditionTime(conditions, "Foo")
	if got == nil {
		t.Fatal("expected non-nil time for existing condition")
	}
	if !got.Equal(&now) {
		t.Errorf("expected %v, got %v", now, *got)
	}

	got = conditionTime(conditions, "Missing")
	if got != nil {
		t.Errorf("expected nil for missing condition, got %v", *got)
	}
}

func TestAnalysisFailureMessage(t *testing.T) {
	tests := []struct {
		name   string
		result *AnalysisOutput
		want   string
	}{
		{
			name: "summary takes priority",
			result: &AnalysisOutput{
				Summary:   "Unable to connect to cluster API",
				Diagnosis: &agenticv1alpha1.DiagnosisResult{Summary: "should not appear"},
			},
			want: "Analysis failed: Unable to connect to cluster API",
		},
		{
			name: "falls back to top-level diagnosis",
			result: &AnalysisOutput{
				Diagnosis: &agenticv1alpha1.DiagnosisResult{Summary: "OOMKilled due to memory limit of 256Mi"},
			},
			want: "Analysis failed: OOMKilled due to memory limit of 256Mi",
		},
		{
			name: "falls back to per-option diagnosis",
			result: &AnalysisOutput{
				Options: []agenticv1alpha1.RemediationOption{
					{Diagnosis: agenticv1alpha1.DiagnosisResult{Summary: "CrashLoopBackOff caused by missing config"}},
				},
			},
			want: "Analysis failed: CrashLoopBackOff caused by missing config",
		},
		{
			name: "uses JSON summary when no top-level summary property",
			result: &AnalysisOutput{
				Summary:   `{"success": false, "options": []}`,
				Diagnosis: &agenticv1alpha1.DiagnosisResult{Summary: "real diagnosis"},
			},
			want: `Analysis failed: {"success": false, "options": []}`,
		},
		{
			name:   "no details available",
			result: &AnalysisOutput{},
			want:   "Analysis agent reported failure",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := analysisFailureMessage(tt.result); got != tt.want {
				t.Errorf("analysisFailureMessage() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestExecutionFailureMessage(t *testing.T) {
	tests := []struct {
		name   string
		result *ExecutionOutput
		want   string
	}{
		{
			name: "summary takes priority",
			result: &ExecutionOutput{
				Summary: "Timed out waiting for pod readiness",
				ActionsTaken: []agenticv1alpha1.ExecutionAction{
					{Description: "should not appear", Outcome: agenticv1alpha1.ActionOutcomeFailed, Error: "also ignored"},
				},
			},
			want: "Execution failed: Timed out waiting for pod readiness",
		},
		{
			name: "falls back to failed action with error",
			result: &ExecutionOutput{
				ActionsTaken: []agenticv1alpha1.ExecutionAction{
					{Description: "Patched deployment/web", Outcome: agenticv1alpha1.ActionOutcomeFailed, Error: "forbidden: insufficient permissions"},
				},
			},
			want: "Execution failed: Patched deployment/web — forbidden: insufficient permissions",
		},
		{
			name: "falls back to failed action without error",
			result: &ExecutionOutput{
				ActionsTaken: []agenticv1alpha1.ExecutionAction{
					{Description: "Scale deployment to 3 replicas", Outcome: agenticv1alpha1.ActionOutcomeFailed},
				},
			},
			want: "Execution failed: Scale deployment to 3 replicas",
		},
		{
			name: "uses JSON summary when no top-level summary property",
			result: &ExecutionOutput{
				Summary: `{"success": false, "actionsTaken": []}`,
				ActionsTaken: []agenticv1alpha1.ExecutionAction{
					{Description: "Patched deployment", Outcome: agenticv1alpha1.ActionOutcomeFailed},
				},
			},
			want: `Execution failed: {"success": false, "actionsTaken": []}`,
		},
		{
			name:   "no details available",
			result: &ExecutionOutput{},
			want:   "Execution agent reported failure",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := executionFailureMessage(tt.result); got != tt.want {
				t.Errorf("executionFailureMessage() = %q, want %q", got, tt.want)
			}
		})
	}
}
