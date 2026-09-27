//go:build e2e

package e2e

import (
	"context"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
)

// NOTE: verification-failure behavior lives in the mock-agent image, keyed on
// the MOCK_VERIFY_FAIL request keyword (test/agent/main.go). The e2e
// SandboxTemplate pulls quay.io/openshift-lightspeed/ols-qe:lightspeed-mock-agent1,
// so that image must be rebuilt+pushed (make -C test/agent docker-build
// docker-push) after changing the mock, or the failing test's run will just
// Complete.

// TestVerificationFlow_VerifyingToCompleted validates the verification phase:
//
//  1. Create AgenticRun, drive through analysis + execution to Verifying
//  2. Wait for phase = Completed (verification auto-approved, runs, passes)
//  3. Assert: VerificationResult exists, Verified=True, terminal state
//  4. Delete AgenticRun, verify RBAC cleaned up
func TestVerificationFlow_VerifyingToCompleted(t *testing.T) {
	t.Log("=== TestVerificationFlow_VerifyingToCompleted: validates full lifecycle → Completed ===")
	c := newClient(t)
	ctx := context.Background()

	prop := createAgenticRun(t, c, "e2e-verification-flow")
	t.Logf("AgenticRun created: %s/%s", testNS, prop.Name)

	t.Log("Waiting for phase: Proposed (analysis complete)")
	proposed := waitForPhase(t, c, prop.Name, agenticv1alpha1.AgenticRunPhaseProposed)
	runUID := string(proposed.UID)
	t.Log("Phase reached: Proposed")

	t.Log("Approving execution with option 0")
	approveExecution(t, c, prop.Name, 0)

	t.Log("Waiting for phase: Verifying (execution complete)")
	waitForPhase(t, c, prop.Name, agenticv1alpha1.AgenticRunPhaseVerifying)
	t.Log("Phase reached: Verifying")

	t.Log("Approving verification")
	approveVerification(t, c, prop.Name)

	t.Log("Waiting for phase: Completed (verification complete)")
	updated := waitForPhase(t, c, prop.Name, agenticv1alpha1.AgenticRunPhaseCompleted)
	t.Log("Phase reached: Completed")

	// --- Verify: Verified condition ---
	var verifiedFound bool
	for _, cond := range updated.Status.Conditions {
		if cond.Type == agenticv1alpha1.AgenticRunConditionVerified {
			verifiedFound = true
			if cond.Status != metav1.ConditionTrue {
				t.Errorf("Verified condition status = %s, want True", cond.Status)
			}
		}
	}
	if !verifiedFound {
		t.Error("Verified condition not found")
	}
	t.Log("Verified: Verified=True condition present")

	// --- Verify: VerificationResult exists ---
	var verifyList agenticv1alpha1.VerificationResultList
	if err := c.List(ctx, &verifyList, client.InNamespace(testNS), client.MatchingLabels{"agentic.openshift.io/run": runUID}); err != nil {
		t.Fatalf("list VerificationResult: %v", err)
	}
	if len(verifyList.Items) == 0 {
		t.Fatal("no VerificationResult found")
	}
	if len(verifyList.Items[0].OwnerReferences) == 0 {
		t.Error("VerificationResult has no owner references")
	}
	t.Logf("Verified: VerificationResult %s exists with owner reference", verifyList.Items[0].Name)

	// --- Verify: verification sandbox info ---
	if updated.Status.Steps.Verification.Sandbox.ClaimName == "" {
		t.Error("status.steps.verification.sandbox.claimName is empty")
	}
	t.Logf("Verified: verification sandbox info recorded, claimName=%s", updated.Status.Steps.Verification.Sandbox.ClaimName)

	// --- Verify: OTEL traces exported for all steps ---
	assertTracesExported(t, &updated, []string{
		"agenticrun.analyze",
		"agenticrun.human_approval",
		"agenticrun.execute",
		"agenticrun.verify",
		"agenticrun.terminal",
	})

	// --- Cleanup and verify RBAC removed ---
	roleName := "ls-exec-" + runUID
	t.Log("Deleting AgenticRun — verifying RBAC cleanup")
	if err := c.Delete(ctx, prop); err != nil {
		t.Fatalf("delete AgenticRun: %v", err)
	}
	waitForDeletion(t, c, prop.Name)

	var role rbacv1.Role
	if err := c.Get(ctx, types.NamespacedName{Name: roleName, Namespace: "staging"}, &role); err == nil {
		t.Errorf("Role %s still exists after deletion — RBAC not cleaned up", roleName)
	}
	t.Log("Verified: RBAC cleaned up after deletion")
	t.Log("PASS: verification complete, phase=Completed, RBAC cleaned")
}

// TestVerificationFlow_FailureEscalatesSingleExecution validates that a
// verification failure escalates directly, without re-executing:
//
//  1. Create AgenticRun with the MOCK_VERIFY_FAIL keyword, drive through
//     analysis + execution to Verifying
//  2. Approve verification — the mock agent reports a FAILING check for the
//     verification step (analysis and execution still succeed)
//  3. Assert: escalation is raised (phase Escalating, or Escalated if the
//     controller auto-advances before the poll observes Escalating),
//     Verified=False/VerificationFailed, Escalated condition present
//  4. Assert: exactly one ExecutionResult exists for the run — proof
//     verification failure never triggers re-execution
func TestVerificationFlow_FailureEscalatesSingleExecution(t *testing.T) {
	t.Log("=== TestVerificationFlow_FailureEscalatesSingleExecution: validates verification failure -> Escalating, single execution ===")
	c := newClient(t)
	ctx := context.Background()

	prop := createAgenticRunWithRequest(t, c, "e2e-verification-fail-escalates", "MOCK_VERIFY_FAIL")
	t.Logf("AgenticRun created: %s/%s (MOCK_VERIFY_FAIL — verification will report failure)", testNS, prop.Name)

	t.Log("Waiting for phase: Proposed (analysis complete)")
	proposed := waitForPhase(t, c, prop.Name, agenticv1alpha1.AgenticRunPhaseProposed)
	runUID := string(proposed.UID)
	t.Log("Phase reached: Proposed")

	t.Log("Approving execution with option 0")
	approveExecution(t, c, prop.Name, 0)

	t.Log("Waiting for phase: Verifying (execution complete)")
	waitForPhase(t, c, prop.Name, agenticv1alpha1.AgenticRunPhaseVerifying)
	t.Log("Phase reached: Verifying")

	t.Log("Approving verification (mock agent will report a FAILING check — MOCK_VERIFY_FAIL)")
	approveVerification(t, c, prop.Name)

	t.Log("Waiting for escalation to complete")
	updated := waitForPhase(t, c, prop.Name, agenticv1alpha1.AgenticRunPhaseEscalated)
	t.Logf("Escalation completed: phase=%s", agenticv1alpha1.DerivePhase(updated.Status.Conditions))

	// --- Verify: Verified=False/VerificationFailed ---
	verified := meta.FindStatusCondition(updated.Status.Conditions, agenticv1alpha1.AgenticRunConditionVerified)
	if verified == nil || verified.Status != metav1.ConditionFalse || verified.Reason != agenticv1alpha1.ReasonVerificationFailed {
		t.Fatalf("expected Verified=False/%s, got %+v", agenticv1alpha1.ReasonVerificationFailed, verified)
	}
	t.Log("Verified: Verified=False/VerificationFailed condition present")

	// --- Verify: Escalated=True/Succeeded ---
	escalated := meta.FindStatusCondition(updated.Status.Conditions, agenticv1alpha1.AgenticRunConditionEscalated)
	if escalated == nil || escalated.Status != metav1.ConditionTrue || escalated.Reason != "Succeeded" {
		t.Fatalf("expected Escalated=True/Succeeded, got %+v", escalated)
	}
	t.Log("Verified: Escalated=True/Succeeded")

	// --- Verify: exactly ONE ExecutionResult — proof of no re-execution ---
	// Result CRs are labelled with the run UID (LabelRun = string(run.UID)),
	// not the run name — see controller/agenticrun/results.go.
	var execList agenticv1alpha1.ExecutionResultList
	if err := c.List(ctx, &execList, client.InNamespace(testNS), client.MatchingLabels{"agentic.openshift.io/run": runUID}); err != nil {
		t.Fatalf("list ExecutionResult: %v", err)
	}
	if len(execList.Items) != 1 {
		t.Fatalf("expected exactly 1 ExecutionResult, got %d", len(execList.Items))
	}
	t.Logf("Verified: exactly 1 ExecutionResult %s exists — no re-execution occurred", execList.Items[0].Name)

	t.Log("PASS: verification failure escalated with a single execution")
}
