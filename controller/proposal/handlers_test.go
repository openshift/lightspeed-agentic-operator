package proposal

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/go-logr/logr"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
)

func reviseProposal(t *testing.T, fc client.WithWatch, name string, feedback string) {
	t.Helper()
	var p agenticv1alpha1.Proposal
	if err := fc.Get(context.Background(), types.NamespacedName{Name: name, Namespace: "default"}, &p); err != nil {
		t.Fatalf("get proposal for revision: %v", err)
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
		proposal  *agenticv1alpha1.Proposal
		wantPhase agenticv1alpha1.ProposalPhase
	}{
		{
			name:      "full_lifecycle_reaches_verifying",
			proposal:  testProposal(),
			wantPhase: agenticv1alpha1.ProposalPhaseVerifying,
		},
		{
			name: "advisory_only_completes",
			proposal: &agenticv1alpha1.Proposal{
				ObjectMeta: metav1.ObjectMeta{Name: "fix-crash", Namespace: "default"},
				Spec: agenticv1alpha1.ProposalSpec{
					Request:          "Investigate issue",
					Tools:            testTools(),
					TargetNamespaces: []string{"production"},
					Analysis:         agenticv1alpha1.ProposalStep{Agent: "default"},
				},
			},
			wantPhase: agenticv1alpha1.ProposalPhaseCompleted,
		},
		{
			name: "assisted_reaches_verifying",
			proposal: &agenticv1alpha1.Proposal{
				ObjectMeta: metav1.ObjectMeta{Name: "fix-crash", Namespace: "default"},
				Spec: agenticv1alpha1.ProposalSpec{
					Request:          "Fix with manual apply",
					Tools:            testTools(),
					TargetNamespaces: []string{"production"},
					Analysis:         agenticv1alpha1.ProposalStep{Agent: "default"},
					Verification:     agenticv1alpha1.ProposalStep{Agent: "default"},
				},
			},
			wantPhase: agenticv1alpha1.ProposalPhaseVerifying,
		},
		{
			name: "no_verification_skips_verification",
			proposal: &agenticv1alpha1.Proposal{
				ObjectMeta: metav1.ObjectMeta{Name: "fix-crash", Namespace: "default"},
				Spec: agenticv1alpha1.ProposalSpec{
					Request:          "Trust mode fix",
					Tools:            testTools(),
					TargetNamespaces: []string{"production"},
					Analysis:         agenticv1alpha1.ProposalStep{Agent: "default"},
					Execution:        agenticv1alpha1.ProposalStep{Agent: "default"},
				},
			},
			wantPhase: agenticv1alpha1.ProposalPhaseCompleted,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scheme := testScheme()
			proposal := tt.proposal

			objs := []client.Object{proposal, testDefaultAgent(), testLLM("smart"), testAutoApprovePolicy()}
			fc := fake.NewClientBuilder().WithScheme(scheme).
				WithObjects(objs...).
				WithStatusSubresource(proposal, &agenticv1alpha1.AnalysisResult{}, &agenticv1alpha1.ExecutionResult{}, &agenticv1alpha1.VerificationResult{}, &agenticv1alpha1.EscalationResult{}).Build()

			r := &ProposalReconciler{Client: fc, Log: logr.Discard(), Agent: newTestAgentCaller()}

			if _, err := reconcileOnce(r, "fix-crash"); err != nil {
				t.Fatalf("analysis reconcile: %v", err)
			}
			p, _ := getProposal(r, "fix-crash")
			if agenticv1alpha1.DerivePhase(p.Status.Conditions) != agenticv1alpha1.ProposalPhaseProposed {
				t.Fatalf("after analysis: expected Proposed, got %s", agenticv1alpha1.DerivePhase(p.Status.Conditions))
			}

			approveProposal(t, fc, "fix-crash")

			if _, err := reconcileOnce(r, "fix-crash"); err != nil {
				t.Fatalf("post-approval reconcile: %v", err)
			}
			p, _ = getProposal(r, "fix-crash")
			if agenticv1alpha1.DerivePhase(p.Status.Conditions) != tt.wantPhase {
				t.Fatalf("after approval: expected %s, got %s", tt.wantPhase, agenticv1alpha1.DerivePhase(p.Status.Conditions))
			}
		})
	}
}

func TestReconcile_HappyPath_FullLifecycle(t *testing.T) {
	scheme := testScheme()
	proposal := testProposal()

	objs := append([]client.Object{proposal}, defaultObjects()...)
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
		WithStatusSubresource(proposal, &agenticv1alpha1.AnalysisResult{}, &agenticv1alpha1.ExecutionResult{}, &agenticv1alpha1.VerificationResult{}, &agenticv1alpha1.EscalationResult{}).Build()

	r := &ProposalReconciler{Client: fc, Log: logr.Discard(), Agent: newTestAgentCaller()}

	// Reconcile 1: Pending → Proposed (analysis complete)
	result, err := reconcileOnce(r, "fix-crash")
	if err != nil {
		t.Fatalf("reconcile 1: %v", err)
	}
	if result.Requeue {
		t.Error("should not requeue — watch event drives next reconcile")
	}

	p, _ := getProposal(r, "fix-crash")
	if agenticv1alpha1.DerivePhase(p.Status.Conditions) != agenticv1alpha1.ProposalPhaseProposed {
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
	approveProposal(t, fc, "fix-crash")

	// Reconcile 2: Executing → Verifying
	result, err = reconcileOnce(r, "fix-crash")
	if err != nil {
		t.Fatalf("reconcile 2: %v", err)
	}
	if result.Requeue {
		t.Error("should not requeue — watch event drives next reconcile")
	}

	p, _ = getProposal(r, "fix-crash")
	if agenticv1alpha1.DerivePhase(p.Status.Conditions) != agenticv1alpha1.ProposalPhaseVerifying {
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

	p, _ = getProposal(r, "fix-crash")
	if agenticv1alpha1.DerivePhase(p.Status.Conditions) != agenticv1alpha1.ProposalPhaseCompleted {
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

func TestReconcile_AnalysisSystemFailure_Terminal(t *testing.T) {
	agent := newTestAgentCaller()
	agent.analyzeErr = fmt.Errorf("LLM timeout")
	scheme := testScheme()

	proposal := testProposal()
	objs := append([]client.Object{proposal}, defaultObjects()...)
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
		WithStatusSubresource(proposal, &agenticv1alpha1.AnalysisResult{}, &agenticv1alpha1.ExecutionResult{}, &agenticv1alpha1.VerificationResult{}, &agenticv1alpha1.EscalationResult{}).Build()

	r := &ProposalReconciler{Client: fc, Log: logr.Discard(), Agent: agent}

	// Reconcile 1: Pending → Failed (system failure)
	result, err := reconcileOnce(r, "fix-crash")
	if err != nil {
		t.Fatalf("reconcile 1: %v", err)
	}
	if result.Requeue {
		t.Error("should not requeue — watch event drives next reconcile")
	}
	p, _ := getProposal(r, "fix-crash")
	if agenticv1alpha1.DerivePhase(p.Status.Conditions) != agenticv1alpha1.ProposalPhaseFailed {
		t.Fatalf("expected Failed, got %s", agenticv1alpha1.DerivePhase(p.Status.Conditions))
	}

	// Reconcile 2: Failed stays Failed (terminal, no retry)
	reconcileOnce(r, "fix-crash")
	p, _ = getProposal(r, "fix-crash")
	if agenticv1alpha1.DerivePhase(p.Status.Conditions) != agenticv1alpha1.ProposalPhaseFailed {
		t.Fatalf("expected Failed (terminal), got %s", agenticv1alpha1.DerivePhase(p.Status.Conditions))
	}
	if len(p.Status.Steps.Analysis.Results) != 1 {
		t.Fatalf("expected 1 analysis result recorded, got %d", len(p.Status.Steps.Analysis.Results))
	}
	if p.Status.Steps.Analysis.Results[0].Outcome != agenticv1alpha1.ActionOutcomeFailed {
		t.Fatal("expected failed analysis result")
	}
}

func TestReconcile_VerificationObjectiveFailure_RetriesExecution(t *testing.T) {
	agent := newTestAgentCaller()
	scheme := testScheme()

	proposal := testProposal()

	objs := append([]client.Object{proposal}, defaultObjectsWithMaxAttempts(3)...)
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
		WithStatusSubresource(proposal, &agenticv1alpha1.AnalysisResult{}, &agenticv1alpha1.ExecutionResult{}, &agenticv1alpha1.VerificationResult{}, &agenticv1alpha1.EscalationResult{}).Build()

	r := &ProposalReconciler{Client: fc, Log: logr.Discard(), Agent: agent}

	// Analysis → approve → execution → verifying
	reconcileOnce(r, "fix-crash")
	approveProposal(t, fc, "fix-crash")
	reconcileOnce(r, "fix-crash")

	// Make verification fail (objective failure, not system error)
	agent.verifyResult = &VerificationOutput{
		Success: false,
		Checks:  []agenticv1alpha1.VerifyCheck{{Name: "pod-running", Source: "oc", Value: "CrashLoopBackOff", Result: agenticv1alpha1.CheckResultFailed}},
		Summary: "Pod still crashing",
	}

	// Verification fails → back to Executing for retry (retryCount=1)
	result, err := reconcileOnce(r, "fix-crash")
	if err != nil {
		t.Fatalf("verification reconcile: %v", err)
	}
	if result.Requeue {
		t.Error("should not requeue — watch event drives next reconcile")
	}
	p, _ := getProposal(r, "fix-crash")
	if agenticv1alpha1.DerivePhase(p.Status.Conditions) != agenticv1alpha1.ProposalPhaseExecuting {
		t.Fatalf("expected Executing (retry), got %s", agenticv1alpha1.DerivePhase(p.Status.Conditions))
	}
	if p.Status.Steps.Execution.RetryCount == nil || *p.Status.Steps.Execution.RetryCount != 1 {
		t.Fatal("retryCount should be 1")
	}

	// Re-execute → Verifying
	reconcileOnce(r, "fix-crash")
	p, _ = getProposal(r, "fix-crash")
	if agenticv1alpha1.DerivePhase(p.Status.Conditions) != agenticv1alpha1.ProposalPhaseVerifying {
		t.Fatalf("expected Verifying (re-execution), got %s", agenticv1alpha1.DerivePhase(p.Status.Conditions))
	}

	// Re-verify → fails again → Executing (retryCount=2, requeue)
	reconcileOnce(r, "fix-crash")
	p, _ = getProposal(r, "fix-crash")
	if agenticv1alpha1.DerivePhase(p.Status.Conditions) != agenticv1alpha1.ProposalPhaseExecuting {
		t.Fatalf("expected Executing (retry 2), got %s", agenticv1alpha1.DerivePhase(p.Status.Conditions))
	}
	if *p.Status.Steps.Execution.RetryCount != 2 {
		t.Fatalf("expected retryCount 2, got %d", *p.Status.Steps.Execution.RetryCount)
	}

	// Re-execute again → Verifying
	reconcileOnce(r, "fix-crash")
	// Re-verify → retryCount=2 >= maxAttempts=2 → Escalating (exhausted)
	reconcileOnce(r, "fix-crash")
	p, _ = getProposal(r, "fix-crash")
	if agenticv1alpha1.DerivePhase(p.Status.Conditions) != agenticv1alpha1.ProposalPhaseEscalating {
		t.Fatalf("expected Escalating (retries exhausted), got %s", agenticv1alpha1.DerivePhase(p.Status.Conditions))
	}
}

func TestReconcile_SystemFailure_Execution_Terminal(t *testing.T) {
	agent := newTestAgentCaller()
	scheme := testScheme()

	proposal := testProposal()
	objs := append([]client.Object{proposal}, defaultObjects()...)
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
		WithStatusSubresource(proposal, &agenticv1alpha1.AnalysisResult{}, &agenticv1alpha1.ExecutionResult{}, &agenticv1alpha1.VerificationResult{}, &agenticv1alpha1.EscalationResult{}).Build()

	r := &ProposalReconciler{Client: fc, Log: logr.Discard(), Agent: agent}

	// Analysis → approve
	reconcileOnce(r, "fix-crash")
	approveProposal(t, fc, "fix-crash")

	// Execution system failure
	agent.executeErr = fmt.Errorf("sandbox pod crashed")
	result, err := reconcileOnce(r, "fix-crash")
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if result.Requeue {
		t.Error("should not requeue — watch event drives next reconcile")
	}
	p, _ := getProposal(r, "fix-crash")
	if agenticv1alpha1.DerivePhase(p.Status.Conditions) != agenticv1alpha1.ProposalPhaseFailed {
		t.Fatalf("expected Failed, got %s", agenticv1alpha1.DerivePhase(p.Status.Conditions))
	}

	// Terminal — stays Failed
	reconcileOnce(r, "fix-crash")
	p, _ = getProposal(r, "fix-crash")
	if agenticv1alpha1.DerivePhase(p.Status.Conditions) != agenticv1alpha1.ProposalPhaseFailed {
		t.Fatalf("expected Failed (terminal), got %s", agenticv1alpha1.DerivePhase(p.Status.Conditions))
	}
}

func TestReconcile_SystemFailure_Verification_Terminal(t *testing.T) {
	agent := newTestAgentCaller()
	scheme := testScheme()

	proposal := testProposal()
	objs := append([]client.Object{proposal}, defaultObjects()...)
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
		WithStatusSubresource(proposal, &agenticv1alpha1.AnalysisResult{}, &agenticv1alpha1.ExecutionResult{}, &agenticv1alpha1.VerificationResult{}, &agenticv1alpha1.EscalationResult{}).Build()

	r := &ProposalReconciler{Client: fc, Log: logr.Discard(), Agent: agent}

	// Analysis → approve → execution → verifying
	reconcileOnce(r, "fix-crash")
	approveProposal(t, fc, "fix-crash")
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
	p, _ := getProposal(r, "fix-crash")
	if agenticv1alpha1.DerivePhase(p.Status.Conditions) != agenticv1alpha1.ProposalPhaseFailed {
		t.Fatalf("expected Failed, got %s", agenticv1alpha1.DerivePhase(p.Status.Conditions))
	}

	// Terminal — stays Failed
	reconcileOnce(r, "fix-crash")
	p, _ = getProposal(r, "fix-crash")
	if agenticv1alpha1.DerivePhase(p.Status.Conditions) != agenticv1alpha1.ProposalPhaseFailed {
		t.Fatalf("expected Failed (terminal), got %s", agenticv1alpha1.DerivePhase(p.Status.Conditions))
	}
}

func TestReconcile_ObjectiveFailure_ThenRevise(t *testing.T) {
	agent := newTestAgentCaller()
	scheme := testScheme()

	proposal := testProposal()

	objs := append([]client.Object{proposal}, defaultObjectsWithMaxAttempts(1)...)
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
		WithStatusSubresource(proposal, &agenticv1alpha1.AnalysisResult{}, &agenticv1alpha1.ExecutionResult{}, &agenticv1alpha1.VerificationResult{}, &agenticv1alpha1.EscalationResult{}).Build()

	r := &ProposalReconciler{Client: fc, Log: logr.Discard(), Agent: agent}

	// Full lifecycle to verification failure, retries exhausted → Analyzing
	reconcileOnce(r, "fix-crash")
	approveProposal(t, fc, "fix-crash")
	reconcileOnce(r, "fix-crash")

	agent.verifyResult = &VerificationOutput{
		Checks:  []agenticv1alpha1.VerifyCheck{{Name: "pod-running", Source: "oc", Value: "CrashLoopBackOff", Result: agenticv1alpha1.CheckResultFailed}},
		Summary: "Pod still crashing",
	}
	// Verification fails → Executing (retry, retryCount=1)
	reconcileOnce(r, "fix-crash")
	// Re-execute → Verifying
	reconcileOnce(r, "fix-crash")
	// Re-verify → retryCount=1 >= maxAttempts=1 → Escalating (exhausted)
	reconcileOnce(r, "fix-crash")

	p, _ := getProposal(r, "fix-crash")
	if agenticv1alpha1.DerivePhase(p.Status.Conditions) != agenticv1alpha1.ProposalPhaseEscalating {
		t.Fatalf("expected Escalating (retries exhausted), got %s", agenticv1alpha1.DerivePhase(p.Status.Conditions))
	}

	// Admin submits revision
	agent.verifyResult = &VerificationOutput{
		Success: true,
		Checks:  []agenticv1alpha1.VerifyCheck{{Name: "pod-running", Source: "oc", Value: "Running", Result: agenticv1alpha1.CheckResultPassed}},
		Summary: "Pod running",
	}
	reviseProposal(t, fc, "fix-crash", "revise analysis")
	reconcileOnce(r, "fix-crash") // revision re-analysis

	p, _ = getProposal(r, "fix-crash")
	if agenticv1alpha1.DerivePhase(p.Status.Conditions) != agenticv1alpha1.ProposalPhaseProposed {
		t.Fatalf("expected Proposed after revision, got %s", agenticv1alpha1.DerivePhase(p.Status.Conditions))
	}

	// Approve and complete
	approveProposal(t, fc, "fix-crash")
	reconcileOnce(r, "fix-crash") // execution + verification
	p, _ = getProposal(r, "fix-crash")
	if agenticv1alpha1.DerivePhase(p.Status.Conditions) != agenticv1alpha1.ProposalPhaseVerifying {
		t.Fatalf("expected Verifying, got %s", agenticv1alpha1.DerivePhase(p.Status.Conditions))
	}
	reconcileOnce(r, "fix-crash")
	p, _ = getProposal(r, "fix-crash")
	if agenticv1alpha1.DerivePhase(p.Status.Conditions) != agenticv1alpha1.ProposalPhaseCompleted {
		t.Fatalf("expected Completed, got %s", agenticv1alpha1.DerivePhase(p.Status.Conditions))
	}
}

func TestReconcile_RevisionHappyPath(t *testing.T) {
	scheme := testScheme()
	proposal := testProposal()

	objs := append([]client.Object{proposal}, defaultObjects()...)
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
		WithStatusSubresource(proposal, &agenticv1alpha1.AnalysisResult{}, &agenticv1alpha1.ExecutionResult{}, &agenticv1alpha1.VerificationResult{}, &agenticv1alpha1.EscalationResult{}).Build()

	r := &ProposalReconciler{Client: fc, Log: logr.Discard(), Agent: newTestAgentCaller()}

	// Reconcile 1: Pending → Executing (analysis complete)
	if _, err := reconcileOnce(r, "fix-crash"); err != nil {
		t.Fatalf("reconcile 1: %v", err)
	}
	p, _ := getProposal(r, "fix-crash")
	if agenticv1alpha1.DerivePhase(p.Status.Conditions) != agenticv1alpha1.ProposalPhaseProposed {
		t.Fatalf("expected Proposed, got %s", agenticv1alpha1.DerivePhase(p.Status.Conditions))
	}
	initialResultCount := len(p.Status.Steps.Analysis.Results)

	// Submit revision
	reviseProposal(t, fc, "fix-crash", "revise analysis")

	// Reconcile 2: Executing → Analyzing → Executing (revised)
	if _, err := reconcileOnce(r, "fix-crash"); err != nil {
		t.Fatalf("reconcile 2 (revision): %v", err)
	}
	p, _ = getProposal(r, "fix-crash")
	if agenticv1alpha1.DerivePhase(p.Status.Conditions) != agenticv1alpha1.ProposalPhaseProposed {
		t.Fatalf("expected Proposed after revision, got %s", agenticv1alpha1.DerivePhase(p.Status.Conditions))
	}
	if analyzed := meta.FindStatusCondition(p.Status.Conditions, agenticv1alpha1.ProposalConditionAnalyzed); analyzed == nil || analyzed.ObservedGeneration == 0 {
		t.Fatal("observedGeneration not set after revision")
	}
	if len(p.Status.Steps.Analysis.Results) <= initialResultCount {
		t.Fatal("results should have a new entry after revision")
	}

	// Approve and continue
	approveProposal(t, fc, "fix-crash")
	if _, err := reconcileOnce(r, "fix-crash"); err != nil {
		t.Fatalf("reconcile 3 (post-approval): %v", err)
	}
	p, _ = getProposal(r, "fix-crash")
	if agenticv1alpha1.DerivePhase(p.Status.Conditions) != agenticv1alpha1.ProposalPhaseVerifying {
		t.Fatalf("expected Verifying after approval, got %s", agenticv1alpha1.DerivePhase(p.Status.Conditions))
	}
}

func TestReconcile_RevisionMultipleRounds(t *testing.T) {
	scheme := testScheme()
	proposal := testProposal()

	objs := append([]client.Object{proposal}, defaultObjects()...)
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
		WithStatusSubresource(proposal, &agenticv1alpha1.AnalysisResult{}, &agenticv1alpha1.ExecutionResult{}, &agenticv1alpha1.VerificationResult{}, &agenticv1alpha1.EscalationResult{}).Build()

	r := &ProposalReconciler{Client: fc, Log: logr.Discard(), Agent: newTestAgentCaller()}

	// Initial analysis
	reconcileOnce(r, "fix-crash")

	// Revision 1
	reviseProposal(t, fc, "fix-crash", "revise analysis")
	reconcileOnce(r, "fix-crash")

	// Second revision
	reviseProposal(t, fc, "fix-crash", "revise again")
	reconcileOnce(r, "fix-crash")

	p, _ := getProposal(r, "fix-crash")
	if agenticv1alpha1.DerivePhase(p.Status.Conditions) != agenticv1alpha1.ProposalPhaseProposed {
		t.Fatalf("expected Proposed, got %s", agenticv1alpha1.DerivePhase(p.Status.Conditions))
	}
	if analyzed := meta.FindStatusCondition(p.Status.Conditions, agenticv1alpha1.ProposalConditionAnalyzed); analyzed == nil || analyzed.ObservedGeneration == 0 {
		t.Fatal("observedGeneration not set after second revision")
	}

	// Approve and proceed
	approveProposal(t, fc, "fix-crash")
	reconcileOnce(r, "fix-crash")
	p, _ = getProposal(r, "fix-crash")
	if agenticv1alpha1.DerivePhase(p.Status.Conditions) != agenticv1alpha1.ProposalPhaseVerifying {
		t.Fatalf("expected Verifying, got %s", agenticv1alpha1.DerivePhase(p.Status.Conditions))
	}
}

func TestReconcile_RevisionNoOp_WhenObserved(t *testing.T) {
	scheme := testScheme()
	proposal := testProposal()

	objs := append([]client.Object{proposal}, defaultObjects()...)
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
		WithStatusSubresource(proposal, &agenticv1alpha1.AnalysisResult{}, &agenticv1alpha1.ExecutionResult{}, &agenticv1alpha1.VerificationResult{}, &agenticv1alpha1.EscalationResult{}).Build()

	r := &ProposalReconciler{Client: fc, Log: logr.Discard(), Agent: newTestAgentCaller()}

	// Initial analysis
	reconcileOnce(r, "fix-crash")

	// Simulate already-observed generation (feedback set but already processed)
	p, _ := getProposal(r, "fix-crash")
	base := p.DeepCopy()
	p.Spec.RevisionFeedback = "some feedback"
	p.Generation = 2
	if err := fc.Patch(context.Background(), p, client.MergeFrom(base)); err != nil {
		t.Fatalf("patch spec revisionFeedback: %v", err)
	}
	p, _ = getProposal(r, "fix-crash")
	base = p.DeepCopy()
	meta.SetStatusCondition(&p.Status.Conditions, metav1.Condition{
		Type:               agenticv1alpha1.ProposalConditionAnalyzed,
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

	p, _ = getProposal(r, "fix-crash")
	if agenticv1alpha1.DerivePhase(p.Status.Conditions) != agenticv1alpha1.ProposalPhaseProposed {
		t.Fatalf("expected Proposed, got %s", agenticv1alpha1.DerivePhase(p.Status.Conditions))
	}
}

func TestReconcile_RevisionReanalysis(t *testing.T) {
	scheme := testScheme()
	proposal := testProposal()

	objs := append([]client.Object{proposal}, defaultObjects()...)
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
		WithStatusSubresource(proposal, &agenticv1alpha1.AnalysisResult{}, &agenticv1alpha1.ExecutionResult{}, &agenticv1alpha1.VerificationResult{}, &agenticv1alpha1.EscalationResult{}).Build()

	r := &ProposalReconciler{Client: fc, Log: logr.Discard(), Agent: newTestAgentCaller()}

	// Analysis → Executing
	reconcileOnce(r, "fix-crash")

	// Submit revision
	reviseProposal(t, fc, "fix-crash", "revise analysis")

	// Reconcile revision
	reconcileOnce(r, "fix-crash")
}

func TestReconcile_RevisionAnalysisFailure(t *testing.T) {
	agent := newTestAgentCaller()
	scheme := testScheme()
	proposal := testProposal()

	objs := append([]client.Object{proposal}, defaultObjects()...)
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
		WithStatusSubresource(proposal, &agenticv1alpha1.AnalysisResult{}, &agenticv1alpha1.ExecutionResult{}, &agenticv1alpha1.VerificationResult{}, &agenticv1alpha1.EscalationResult{}).Build()

	r := &ProposalReconciler{Client: fc, Log: logr.Discard(), Agent: agent}

	// Initial analysis succeeds
	reconcileOnce(r, "fix-crash")
	p, _ := getProposal(r, "fix-crash")
	if agenticv1alpha1.DerivePhase(p.Status.Conditions) != agenticv1alpha1.ProposalPhaseProposed {
		t.Fatalf("expected Proposed, got %s", agenticv1alpha1.DerivePhase(p.Status.Conditions))
	}

	// Submit revision, but agent will fail
	reviseProposal(t, fc, "fix-crash", "revise analysis")
	agent.analyzeErr = fmt.Errorf("LLM timeout during revision")

	// Reconcile → revision analysis fails → Failed
	result, err := reconcileOnce(r, "fix-crash")
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if result.Requeue {
		t.Error("should not requeue — watch event drives next reconcile")
	}
	p, _ = getProposal(r, "fix-crash")
	if agenticv1alpha1.DerivePhase(p.Status.Conditions) != agenticv1alpha1.ProposalPhaseFailed {
		t.Fatalf("expected Failed, got %s", agenticv1alpha1.DerivePhase(p.Status.Conditions))
	}

	// Failed is terminal for system failures — stays Failed
	agent.analyzeErr = nil
	reconcileOnce(r, "fix-crash")
	p, _ = getProposal(r, "fix-crash")
	if agenticv1alpha1.DerivePhase(p.Status.Conditions) != agenticv1alpha1.ProposalPhaseFailed {
		t.Fatalf("expected Failed (terminal), got %s", agenticv1alpha1.DerivePhase(p.Status.Conditions))
	}
}

func TestReconcile_RevisionWithFeedback(t *testing.T) {
	scheme := testScheme()
	proposal := testProposal()

	objs := append([]client.Object{proposal}, defaultObjects()...)
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
		WithStatusSubresource(proposal, &agenticv1alpha1.AnalysisResult{}, &agenticv1alpha1.ExecutionResult{}, &agenticv1alpha1.VerificationResult{}, &agenticv1alpha1.EscalationResult{}).Build()

	r := &ProposalReconciler{Client: fc, Log: logr.Discard(), Agent: newTestAgentCaller()}

	// Initial analysis
	if _, err := reconcileOnce(r, "fix-crash"); err != nil {
		t.Fatalf("initial analysis: %v", err)
	}

	// Submit revision with feedback
	reviseProposal(t, fc, "fix-crash", "Focus on the memory limit, not CPU throttling")

	// Reconcile revision
	if _, err := reconcileOnce(r, "fix-crash"); err != nil {
		t.Fatalf("revision reconcile: %v", err)
	}

	p, _ := getProposal(r, "fix-crash")
	if agenticv1alpha1.DerivePhase(p.Status.Conditions) != agenticv1alpha1.ProposalPhaseProposed {
		t.Fatalf("expected Proposed after revision, got %s", agenticv1alpha1.DerivePhase(p.Status.Conditions))
	}
	if analyzed := meta.FindStatusCondition(p.Status.Conditions, agenticv1alpha1.ProposalConditionAnalyzed); analyzed == nil || analyzed.ObservedGeneration == 0 {
		t.Fatal("observedGeneration not set after revision")
	}
	if p.Spec.RevisionFeedback != "Focus on the memory limit, not CPU throttling" {
		t.Fatalf("expected revisionFeedback to be preserved, got %q", p.Spec.RevisionFeedback)
	}
}

func TestReconcile_ExecutionRBACCreatedOnApproval(t *testing.T) {
	agent := newTestAgentCaller()
	agent.analyzeResult = &AnalysisOutput{
		Success: true,
		Options: []agenticv1alpha1.RemediationOption{{
			Title: "Increase memory",
			Diagnosis: agenticv1alpha1.DiagnosisResult{
				Summary: "OOM", Confidence: "High", RootCause: "Low limit",
			},
			Proposal: agenticv1alpha1.ProposalResult{
				Description: "Increase to 512Mi",
				Actions:     []agenticv1alpha1.ProposedAction{{Type: "patch", Description: "Patch deploy"}},
				Risk:        "Low",
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
	proposal := testProposal()

	objs := append([]client.Object{proposal}, defaultObjects()...)
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
		WithStatusSubresource(proposal, &agenticv1alpha1.AnalysisResult{}, &agenticv1alpha1.ExecutionResult{}, &agenticv1alpha1.VerificationResult{}, &agenticv1alpha1.EscalationResult{}).Build()

	r := &ProposalReconciler{Client: fc, Log: logr.Discard(), Agent: agent}

	// Pending → Proposed (analysis complete)
	reconcileOnce(r, "fix-crash")
	p, _ := getProposal(r, "fix-crash")
	if agenticv1alpha1.DerivePhase(p.Status.Conditions) != agenticv1alpha1.ProposalPhaseProposed {
		t.Fatalf("expected Proposed, got %s", agenticv1alpha1.DerivePhase(p.Status.Conditions))
	}

	// Approve
	approveProposal(t, fc, "fix-crash")

	// Executing → Verifying
	reconcileOnce(r, "fix-crash")
	p, _ = getProposal(r, "fix-crash")
	if agenticv1alpha1.DerivePhase(p.Status.Conditions) != agenticv1alpha1.ProposalPhaseVerifying {
		t.Fatalf("expected Verifying, got %s", agenticv1alpha1.DerivePhase(p.Status.Conditions))
	}

	// Verify namespace-scoped Role+RoleBinding were created
	roleName := executionRoleName("fix-crash")
	var role rbacv1.Role
	if err := fc.Get(context.Background(), types.NamespacedName{Name: roleName, Namespace: "production"}, &role); err != nil {
		t.Fatalf("execution Role not created in production: %v", err)
	}
	if role.Rules[0].Resources[0] != "deployments" {
		t.Fatalf("unexpected Role rule: %+v", role.Rules)
	}
	var binding rbacv1.RoleBinding
	if err := fc.Get(context.Background(), types.NamespacedName{Name: roleName, Namespace: "production"}, &binding); err != nil {
		t.Fatalf("execution RoleBinding not created: %v", err)
	}

	// Verify cluster-scoped ClusterRole+ClusterRoleBinding were created
	crName := clusterRoleName("fix-crash")
	var cr rbacv1.ClusterRole
	if err := fc.Get(context.Background(), types.NamespacedName{Name: crName}, &cr); err != nil {
		t.Fatalf("execution ClusterRole not created: %v", err)
	}
	if cr.Rules[0].Resources[0] != "nodes" {
		t.Fatalf("unexpected ClusterRole rule: %+v", cr.Rules)
	}
	var crb rbacv1.ClusterRoleBinding
	if err := fc.Get(context.Background(), types.NamespacedName{Name: crName}, &crb); err != nil {
		t.Fatalf("execution ClusterRoleBinding not created: %v", err)
	}

	// Verify rbac-namespaces annotation was set
	p, _ = getProposal(r, "fix-crash")
	if p.Annotations[rbacNamespacesAnnotation] != "production" {
		t.Fatalf("expected rbac-namespaces annotation 'production', got %q", p.Annotations[rbacNamespacesAnnotation])
	}

	// Complete lifecycle
	reconcileOnce(r, "fix-crash")
	p, _ = getProposal(r, "fix-crash")
	if agenticv1alpha1.DerivePhase(p.Status.Conditions) != agenticv1alpha1.ProposalPhaseCompleted {
		t.Fatalf("expected Completed, got %s", agenticv1alpha1.DerivePhase(p.Status.Conditions))
	}
}

func TestReconcile_ExecutionRBACCleanedOnFailure(t *testing.T) {
	agent := newTestAgentCaller()
	agent.analyzeResult = &AnalysisOutput{
		Success: true,
		Options: []agenticv1alpha1.RemediationOption{{
			Title: "Fix it",
			Diagnosis: agenticv1alpha1.DiagnosisResult{
				Summary: "Broken", Confidence: "High", RootCause: "Bug",
			},
			Proposal: agenticv1alpha1.ProposalResult{
				Description: "Apply fix",
				Actions:     []agenticv1alpha1.ProposedAction{{Type: "patch", Description: "Patch"}},
				Risk:        "Low",
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
	proposal := testProposal()

	objs := append([]client.Object{proposal}, defaultObjects()...)
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
		WithStatusSubresource(proposal, &agenticv1alpha1.AnalysisResult{}, &agenticv1alpha1.ExecutionResult{}, &agenticv1alpha1.VerificationResult{}, &agenticv1alpha1.EscalationResult{}).Build()

	r := &ProposalReconciler{Client: fc, Log: logr.Discard(), Agent: agent}

	// Analysis → approve
	reconcileOnce(r, "fix-crash")
	approveProposal(t, fc, "fix-crash")

	// Execution succeeds, creates RBAC, but verification will fail with system error
	reconcileOnce(r, "fix-crash")
	p, _ := getProposal(r, "fix-crash")
	if agenticv1alpha1.DerivePhase(p.Status.Conditions) != agenticv1alpha1.ProposalPhaseVerifying {
		t.Fatalf("expected Verifying, got %s", agenticv1alpha1.DerivePhase(p.Status.Conditions))
	}

	// Verify RBAC exists before failure
	roleName := executionRoleName("fix-crash")
	var role rbacv1.Role
	if err := fc.Get(context.Background(), types.NamespacedName{Name: roleName, Namespace: "production"}, &role); err != nil {
		t.Fatalf("Role should exist before failure: %v", err)
	}

	// System failure during verification
	agent.verifyErr = fmt.Errorf("sandbox pod crashed")
	reconcileOnce(r, "fix-crash")
	p, _ = getProposal(r, "fix-crash")
	if agenticv1alpha1.DerivePhase(p.Status.Conditions) != agenticv1alpha1.ProposalPhaseFailed {
		t.Fatalf("expected Failed, got %s", agenticv1alpha1.DerivePhase(p.Status.Conditions))
	}

	// handleFailed should clean up RBAC
	reconcileOnce(r, "fix-crash")

	// Verify RBAC was cleaned up
	if err := fc.Get(context.Background(), types.NamespacedName{Name: roleName, Namespace: "production"}, &role); err == nil {
		t.Fatal("Role should be cleaned up after failure")
	}
	var bindingCheck rbacv1.RoleBinding
	if err := fc.Get(context.Background(), types.NamespacedName{Name: roleName, Namespace: "production"}, &bindingCheck); err == nil {
		t.Fatal("RoleBinding should be cleaned up after failure")
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
				Summary:    "Pod OOMKilled due to 256Mi memory limit",
				Confidence: "High",
				RootCause:  "Memory limit too low for workload",
			},
			Proposal: agenticv1alpha1.ProposalResult{
				Description: "Increase deployment memory limit to 512Mi",
				Actions:     []agenticv1alpha1.ProposedAction{{Type: "patch", Description: "Patch deployment memory limit"}},
				Risk:        "Low",
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
		Verification: &agenticv1alpha1.ExecutionVerification{
			ConditionOutcome: agenticv1alpha1.ConditionOutcomeImproved,
			Summary:          "Pod running with new memory limit",
		},
	})

	verificationJSON, _ := json.Marshal(verificationResponse{
		Success: true,
		Checks:  []agenticv1alpha1.VerifyCheck{{
			Name:   "pod-running",
			Source: "oc",
			Value:  "Running",
			Result: agenticv1alpha1.CheckResultPassed,
		}},
		Summary: "All verification checks passed",
	})

	sandboxAgent, sandbox := newMockSandboxAgent(string(analysisJSON), string(executionJSON), string(verificationJSON))

	scheme := testScheme()
	proposal := testProposal()

	objs := append([]client.Object{proposal}, defaultObjects()...)
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
		WithStatusSubresource(proposal, &agenticv1alpha1.AnalysisResult{}, &agenticv1alpha1.ExecutionResult{}, &agenticv1alpha1.VerificationResult{}, &agenticv1alpha1.EscalationResult{}).Build()

	r := &ProposalReconciler{Client: fc, Log: logr.Discard(), Agent: sandboxAgent}

	// Reconcile 1: Pending → Executing (via sandbox analysis)
	if _, err := reconcileOnce(r, "fix-crash"); err != nil {
		t.Fatalf("analysis reconcile: %v", err)
	}
	p, _ := getProposal(r, "fix-crash")
	if agenticv1alpha1.DerivePhase(p.Status.Conditions) != agenticv1alpha1.ProposalPhaseProposed {
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
	if ar.Status.Options[0].Diagnosis.Confidence != "High" {
		t.Errorf("confidence = %q", ar.Status.Options[0].Diagnosis.Confidence)
	}

	// Approve
	approveProposal(t, fc, "fix-crash")

	// Reconcile 2: Executing → Verifying (via sandbox execution)
	result, err := reconcileOnce(r, "fix-crash")
	if err != nil {
		t.Fatalf("execution reconcile: %v", err)
	}
	if result.Requeue {
		t.Error("should not requeue — watch event drives next reconcile")
	}
	p, _ = getProposal(r, "fix-crash")
	if agenticv1alpha1.DerivePhase(p.Status.Conditions) != agenticv1alpha1.ProposalPhaseVerifying {
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
	if er.Status.Verification.ConditionOutcome != agenticv1alpha1.ConditionOutcomeImproved {
		t.Errorf("inline verification = %q", er.Status.Verification.ConditionOutcome)
	}

	// Reconcile 3: Verifying → Completed (via sandbox verification)
	if _, err := reconcileOnce(r, "fix-crash"); err != nil {
		t.Fatalf("verification reconcile: %v", err)
	}
	p, _ = getProposal(r, "fix-crash")
	if agenticv1alpha1.DerivePhase(p.Status.Conditions) != agenticv1alpha1.ProposalPhaseCompleted {
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
	proposal := testProposal()

	objs := append([]client.Object{proposal}, defaultObjects()...)
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
		WithStatusSubresource(proposal, &agenticv1alpha1.AnalysisResult{}, &agenticv1alpha1.ExecutionResult{}, &agenticv1alpha1.VerificationResult{}, &agenticv1alpha1.EscalationResult{}).Build()

	agent := newTestAgentCaller()
	r := &ProposalReconciler{Client: fc, Log: logr.Discard(), Agent: agent}

	// Run analysis
	reconcileOnce(r, "fix-crash")

	// Approve → execution runs → phase should be Verifying
	approveProposal(t, fc, "fix-crash")
	reconcileOnce(r, "fix-crash")

	p, _ := getProposal(r, "fix-crash")
	if agenticv1alpha1.DerivePhase(p.Status.Conditions) != agenticv1alpha1.ProposalPhaseVerifying {
		t.Fatalf("expected Verifying after execution, got %s", agenticv1alpha1.DerivePhase(p.Status.Conditions))
	}

	// Simulate stale cache: set Executed back to Unknown (as if informer lagged)
	base := p.DeepCopy()
	meta.SetStatusCondition(&p.Status.Conditions, metav1.Condition{
		Type:   agenticv1alpha1.ProposalConditionExecuted,
		Status: metav1.ConditionUnknown,
		Reason: "ExecutionInProgress",
	})
	if err := fc.Status().Patch(context.Background(), p, client.MergeFrom(base)); err != nil {
		t.Fatalf("patch conditions to Executing: %v", err)
	}

	// Reconcile — should NOT re-execute (in-progress guard), stays Executing
	reconcileOnce(r, "fix-crash")

	p, _ = getProposal(r, "fix-crash")
	if agenticv1alpha1.DerivePhase(p.Status.Conditions) != agenticv1alpha1.ProposalPhaseExecuting {
		t.Fatalf("expected Executing (in-progress guard), got %s", agenticv1alpha1.DerivePhase(p.Status.Conditions))
	}
}

func TestReconcile_ExecutionOutcomeFailed_FailsStep(t *testing.T) {
	scheme := testScheme()
	proposal := testProposal()

	objs := append([]client.Object{proposal}, defaultObjects()...)
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
		WithStatusSubresource(proposal, &agenticv1alpha1.AnalysisResult{}, &agenticv1alpha1.ExecutionResult{}, &agenticv1alpha1.VerificationResult{}, &agenticv1alpha1.EscalationResult{}).Build()

	agent := newTestAgentCaller()
	agent.executeResult = &ExecutionOutput{
		Success:      false,
		ActionsTaken: []agenticv1alpha1.ExecutionAction{{Type: "patch", Description: "Failed patch", Outcome: agenticv1alpha1.ActionOutcomeFailed}},
	}
	r := &ProposalReconciler{Client: fc, Log: logr.Discard(), Agent: agent}

	// Analysis → Executing
	reconcileOnce(r, "fix-crash")
	// Approve
	approveProposal(t, fc, "fix-crash")
	// Execution with success=false → Failed
	reconcileOnce(r, "fix-crash")

	p, _ := getProposal(r, "fix-crash")
	if agenticv1alpha1.DerivePhase(p.Status.Conditions) != agenticv1alpha1.ProposalPhaseFailed {
		t.Fatalf("expected Failed when execution success=false, got %s", agenticv1alpha1.DerivePhase(p.Status.Conditions))
	}
}

func TestReconcile_VerificationOutcomeFailed_RetriesExecution(t *testing.T) {
	scheme := testScheme()
	proposal := testProposal()

	objs := append([]client.Object{proposal}, defaultObjectsWithMaxAttempts(3)...)
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
		WithStatusSubresource(proposal, &agenticv1alpha1.AnalysisResult{}, &agenticv1alpha1.ExecutionResult{}, &agenticv1alpha1.VerificationResult{}, &agenticv1alpha1.EscalationResult{}).Build()

	agent := newTestAgentCaller()
	agent.verifyResult = &VerificationOutput{
		Success: false,
		Checks:  []agenticv1alpha1.VerifyCheck{{Name: "health", Result: agenticv1alpha1.CheckResultFailed}},
		Summary: "Health check failed",
	}
	r := &ProposalReconciler{Client: fc, Log: logr.Discard(), Agent: agent}

	// Analysis → Executing → Approve → Execute → Verify (fail) → retry
	reconcileOnce(r, "fix-crash")
	approveProposal(t, fc, "fix-crash")
	reconcileOnce(r, "fix-crash") // execution
	reconcileOnce(r, "fix-crash") // verification → retry

	p, _ := getProposal(r, "fix-crash")
	if agenticv1alpha1.DerivePhase(p.Status.Conditions) != agenticv1alpha1.ProposalPhaseExecuting {
		t.Fatalf("expected Executing (retry) when verification success=false, got %s", agenticv1alpha1.DerivePhase(p.Status.Conditions))
	}
	if p.Status.Steps.Execution.RetryCount == nil || *p.Status.Steps.Execution.RetryCount != 1 {
		t.Fatal("retryCount should be 1")
	}
}

func TestReconcile_ExecutionSelectsOption(t *testing.T) {
	scheme := testScheme()
	proposal := testProposal()

	objs := append([]client.Object{proposal}, defaultObjects()...)
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
		WithStatusSubresource(proposal, &agenticv1alpha1.AnalysisResult{}, &agenticv1alpha1.ExecutionResult{}, &agenticv1alpha1.VerificationResult{}, &agenticv1alpha1.EscalationResult{}).Build()

	agent := newTestAgentCaller()
	agent.analyzeResult = &AnalysisOutput{
		Success: true,
		Options: []agenticv1alpha1.RemediationOption{
			{Title: "Option A", Diagnosis: agenticv1alpha1.DiagnosisResult{Summary: "diag-A"}},
			{Title: "Option B", Diagnosis: agenticv1alpha1.DiagnosisResult{Summary: "diag-B"}},
			{Title: "Option C", Diagnosis: agenticv1alpha1.DiagnosisResult{Summary: "diag-C"}},
		},
	}
	r := &ProposalReconciler{Client: fc, Log: logr.Discard(), Agent: agent}

	// Analysis
	reconcileOnce(r, "fix-crash")

	p, _ := getProposal(r, "fix-crash")
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
	approveProposalWithOption(t, fc, "fix-crash", 1)

	// Execution reconcile — should trim to just Option B
	reconcileOnce(r, "fix-crash")

	p, _ = getProposal(r, "fix-crash")
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
	proposal := testProposal()

	objs := append([]client.Object{proposal}, defaultObjects()...)
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
		WithStatusSubresource(proposal, &agenticv1alpha1.AnalysisResult{}, &agenticv1alpha1.ExecutionResult{}, &agenticv1alpha1.VerificationResult{}, &agenticv1alpha1.EscalationResult{}).Build()

	r := &ProposalReconciler{Client: fc, Log: logr.Discard(), Agent: newTestAgentCaller()}

	// Analysis (default stub returns 1 option)
	reconcileOnce(r, "fix-crash")

	// Approve option 0
	approveProposal(t, fc, "fix-crash")

	// Execution
	reconcileOnce(r, "fix-crash")

	p, _ := getProposal(r, "fix-crash")
	if len(p.Status.Steps.Analysis.Results) == 0 {
		t.Fatal("expected analysis results")
	}
}

func TestReconcile_TrimOptionsOnExecution(t *testing.T) {
	scheme := testScheme()
	proposal := testProposal()

	objs := append([]client.Object{proposal}, defaultObjectsWithMaxAttempts(3)...)
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
		WithStatusSubresource(proposal, &agenticv1alpha1.AnalysisResult{}, &agenticv1alpha1.ExecutionResult{}, &agenticv1alpha1.VerificationResult{}, &agenticv1alpha1.EscalationResult{}).Build()

	agent := newTestAgentCaller()
	agent.analyzeResult = &AnalysisOutput{
		Success: true,
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
	r := &ProposalReconciler{Client: fc, Log: logr.Discard(), Agent: agent}

	// Analysis
	reconcileOnce(r, "fix-crash")

	// Approve option 2 (Option C)
	approveProposalWithOption(t, fc, "fix-crash", 2)

	// Execution — should trim options to just Option C
	reconcileOnce(r, "fix-crash")

	p, _ := getProposal(r, "fix-crash")

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

	// Verification fails → triggers retry
	reconcileOnce(r, "fix-crash")

	p, _ = getProposal(r, "fix-crash")
	if agenticv1alpha1.DerivePhase(p.Status.Conditions) != agenticv1alpha1.ProposalPhaseExecuting {
		t.Fatalf("expected Executing (retry), got %s", agenticv1alpha1.DerivePhase(p.Status.Conditions))
	}

	// AnalysisResult should still have just 1 option after retry
	if err := fc.Get(context.Background(), types.NamespacedName{Name: p.Status.Steps.Analysis.Results[0].Name, Namespace: "default"}, &ar); err != nil {
		t.Fatalf("get AnalysisResult after retry: %v", err)
	}
	if len(ar.Status.Options) != 1 {
		t.Fatalf("expected 1 option after retry, got %d", len(ar.Status.Options))
	}
	if ar.Status.Options[0].Title != "Option C" {
		t.Errorf("expected option %q after retry, got %q", "Option C", ar.Status.Options[0].Title)
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
	proposal := testProposal()

	objs := append([]client.Object{proposal}, defaultObjects()...)
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
		WithStatusSubresource(proposal, &agenticv1alpha1.AnalysisResult{}, &agenticv1alpha1.ExecutionResult{}, &agenticv1alpha1.VerificationResult{}, &agenticv1alpha1.EscalationResult{}).Build()

	r := &ProposalReconciler{Client: fc, Log: logr.Discard(), Agent: agent}

	reconcileOnce(r, "fix-crash")
	p, _ := getProposal(r, "fix-crash")

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
