//go:build e2e

package e2e

import (
	"context"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
)

// TestVerificationFlow_VerifyingToCompleted validates the verification phase:
//
//  1. Create Proposal, drive through analysis + execution to Verifying
//  2. Wait for phase = Completed (verification auto-approved, runs, passes)
//  3. Assert: VerificationResult exists, Verified=True, terminal state
//  4. Delete Proposal, verify RBAC cleaned up
func TestVerificationFlow_VerifyingToCompleted(t *testing.T) {
	t.Log("=== TestVerificationFlow_VerifyingToCompleted: validates full lifecycle → Completed ===")
	c := newClient(t)
	ctx := context.Background()

	t.Log("Creating fixtures (LLMProvider, Agent, ApprovalPolicy, Secret)")
	createFixtures(t, c)
	prop := createProposal(t, c, "e2e-verification-flow")
	t.Logf("Proposal created: %s/%s", testNS, prop.Name)

	t.Log("Waiting for phase: Proposed (analysis complete)")
	waitForPhase(t, c, prop.Name, agenticv1alpha1.ProposalPhaseProposed)
	t.Log("Phase reached: Proposed")

	t.Log("Approving execution with option 0")
	approveExecution(t, c, prop.Name, 0)

	t.Log("Waiting for phase: Verifying (execution complete)")
	waitForPhase(t, c, prop.Name, agenticv1alpha1.ProposalPhaseVerifying)
	t.Log("Phase reached: Verifying")

	t.Log("Approving verification")
	approveVerification(t, c, prop.Name)

	t.Log("Waiting for phase: Completed (verification complete)")
	updated := waitForPhase(t, c, prop.Name, agenticv1alpha1.ProposalPhaseCompleted)
	t.Log("Phase reached: Completed")

	// --- Verify: Verified condition ---
	var verifiedFound bool
	for _, cond := range updated.Status.Conditions {
		if cond.Type == agenticv1alpha1.ProposalConditionVerified {
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
	if err := c.List(ctx, &verifyList, client.InNamespace(testNS), client.MatchingLabels{"agentic.openshift.io/proposal": prop.Name}); err != nil {
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

	// --- Cleanup and verify RBAC removed ---
	roleName := "ls-exec-" + prop.Name
	t.Log("Deleting Proposal — verifying RBAC cleanup")
	if err := c.Delete(ctx, prop); err != nil {
		t.Fatalf("delete Proposal: %v", err)
	}
	waitForDeletion(t, c, prop.Name)

	var role rbacv1.Role
	if err := c.Get(ctx, types.NamespacedName{Name: roleName, Namespace: "staging"}, &role); err == nil {
		t.Errorf("Role %s still exists after deletion — RBAC not cleaned up", roleName)
	}
	t.Log("Verified: RBAC cleaned up after deletion")
	t.Log("PASS: verification complete, phase=Completed, RBAC cleaned")
}
