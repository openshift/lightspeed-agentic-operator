//go:build e2e

package e2e

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
)

// TestDenialFlow_ProposedToDenied validates that denying execution terminates the proposal:
//
//  1. Create Proposal, wait for Proposed (analysis complete)
//  2. Deny execution on ProposalApproval
//  3. Wait for phase = Denied (terminal)
//  4. Assert: Denied condition present, sandboxes released on deletion
func TestDenialFlow_ProposedToDenied(t *testing.T) {
	t.Log("=== TestDenialFlow_ProposedToDenied: validates execution denial → Denied terminal ===")
	c := newClient(t)
	ctx := context.Background()

	t.Log("Creating fixtures (LLMProvider, Agent, ApprovalPolicy, Secret)")
	createFixtures(t, c)
	prop := createProposal(t, c, "e2e-denial-flow")
	t.Logf("Proposal created: %s/%s", testNS, prop.Name)

	t.Log("Waiting for phase: Proposed (analysis complete)")
	waitForPhase(t, c, prop.Name, agenticv1alpha1.ProposalPhaseProposed)
	t.Log("Phase reached: Proposed")

	t.Log("Denying execution stage")
	denyStage(t, c, prop.Name, agenticv1alpha1.ApprovalStageExecution)

	t.Log("Waiting for phase: Denied (terminal)")
	updated := waitForPhase(t, c, prop.Name, agenticv1alpha1.ProposalPhaseDenied)
	t.Log("Phase reached: Denied")

	// --- Verify: Denied condition ---
	var deniedFound bool
	for _, cond := range updated.Status.Conditions {
		if cond.Type == agenticv1alpha1.ProposalConditionDenied {
			deniedFound = true
			if cond.Status != metav1.ConditionTrue {
				t.Errorf("Denied condition status = %s, want True", cond.Status)
			}
		}
	}
	if !deniedFound {
		t.Error("Denied condition not found")
	}
	t.Log("Verified: Denied=True condition present")

	// --- Cleanup ---
	t.Log("Deleting Proposal — verifying finalizer cleanup")
	if err := c.Delete(ctx, prop); err != nil {
		t.Fatalf("delete Proposal: %v", err)
	}
	waitForDeletion(t, c, prop.Name)
	t.Log("Verified: Proposal deleted successfully")

	t.Logf("PASS: execution denied, phase=Denied (terminal)")
}
