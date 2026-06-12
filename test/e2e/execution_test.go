//go:build e2e

package e2e

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
)

// TestExecutionFlow_ProposedToVerifying validates the execution phase:
//
//  1. Create Proposal, wait for Proposed (analysis complete)
//  2. Approve execution (select option 0)
//  3. Wait for phase = Executing — assert RBAC exists (mock has 60s delay)
//  4. Wait for phase = Verifying (execution complete)
//  5. Assert: ExecutionResult exists, Executed=True, sandbox info, RBAC annotation
//  6. Delete Proposal, verify RBAC cleaned up
func TestExecutionFlow_ProposedToVerifying(t *testing.T) {
	t.Log("=== TestExecutionFlow_ProposedToVerifying: validates Proposed → Executing → Verifying with RBAC + SA ===")
	c := newClient(t)
	ctx := context.Background()

	t.Log("Creating fixtures (LLMProvider, Agent, ApprovalPolicy, Secret)")
	createFixtures(t, c)
	prop := createProposal(t, c, "e2e-execution-flow")
	t.Logf("Proposal created: %s/%s", testNS, prop.Name)

	t.Log("Waiting for phase: Proposed (analysis complete)")
	waitForPhase(t, c, prop.Name, agenticv1alpha1.ProposalPhaseProposed)
	t.Log("Phase reached: Proposed")

	t.Log("Approving execution with option 0")
	approveExecution(t, c, prop.Name, 0)

	t.Log("Waiting for phase: Executing")
	waitForPhase(t, c, prop.Name, agenticv1alpha1.ProposalPhaseExecuting)
	t.Log("Phase reached: Executing — checking RBAC")

	// --- Verify: RBAC created ---
	roleName := "ls-exec-" + prop.Name
	var role rbacv1.Role
	if err := c.Get(ctx, types.NamespacedName{Name: roleName, Namespace: "staging"}, &role); err != nil {
		t.Fatalf("get Role %s in staging: %v", roleName, err)
	}
	t.Logf("RBAC Role %s exists in staging namespace", roleName)

	var binding rbacv1.RoleBinding
	if err := c.Get(ctx, types.NamespacedName{Name: roleName, Namespace: "staging"}, &binding); err != nil {
		t.Fatalf("get RoleBinding %s in staging: %v", roleName, err)
	}
	t.Logf("Verified: RoleBinding %s exists in staging", roleName)

	// Verify: per-proposal execution SA created.
	saName := "ls-exec-" + testNS + "-" + prop.Name
	var sa corev1.ServiceAccount
	if err := c.Get(ctx, types.NamespacedName{Name: saName, Namespace: testNS}, &sa); err != nil {
		t.Fatalf("get execution SA %s: %v", saName, err)
	}
	t.Logf("Execution SA %s exists", saName)

	// Verify: RoleBinding references the per-proposal SA.
	if len(binding.Subjects) == 0 || binding.Subjects[0].Name != saName {
		t.Errorf("RoleBinding subject = %v, want SA %s", binding.Subjects, saName)
	}
	t.Log("Verified: RoleBinding subjects correct SA")

	// Verify annotation on Proposal.
	var current agenticv1alpha1.Proposal
	if err := c.Get(ctx, types.NamespacedName{Name: prop.Name, Namespace: testNS}, &current); err != nil {
		t.Fatalf("get Proposal: %v", err)
	}
	if current.Annotations["agentic.openshift.io/rbac-namespaces"] == "" {
		t.Error("rbac-namespaces annotation is empty")
	}
	t.Log("Verified: RBAC annotation present on Proposal")

	t.Log("Waiting for phase: Verifying (execution complete)")
	updated := waitForPhase(t, c, prop.Name, agenticv1alpha1.ProposalPhaseVerifying)
	t.Log("Phase reached: Verifying")

	// --- Verify: Executed condition ---
	var executedFound bool
	for _, cond := range updated.Status.Conditions {
		if cond.Type == agenticv1alpha1.ProposalConditionExecuted {
			executedFound = true
			if cond.Status != metav1.ConditionTrue {
				t.Errorf("Executed condition status = %s, want True", cond.Status)
			}
		}
	}
	if !executedFound {
		t.Error("Executed condition not found")
	}
	t.Log("Verified: Executed=True condition present")

	// --- Verify: ExecutionResult exists ---
	var execList agenticv1alpha1.ExecutionResultList
	if err := c.List(ctx, &execList, client.InNamespace(testNS), client.MatchingLabels{"agentic.openshift.io/proposal": prop.Name}); err != nil {
		t.Fatalf("list ExecutionResult: %v", err)
	}
	if len(execList.Items) == 0 {
		t.Fatal("no ExecutionResult found")
	}
	if len(execList.Items[0].OwnerReferences) == 0 {
		t.Error("ExecutionResult has no owner references")
	}
	t.Logf("Verified: ExecutionResult %s exists with owner reference", execList.Items[0].Name)

	// --- Verify: execution sandbox info ---
	if updated.Status.Steps.Execution.Sandbox.ClaimName == "" {
		t.Error("status.steps.execution.sandbox.claimName is empty")
	}
	t.Logf("Verified: execution sandbox info recorded, claimName=%s", updated.Status.Steps.Execution.Sandbox.ClaimName)

	// --- Cleanup and verify RBAC removed ---
	t.Log("Deleting Proposal — verifying RBAC + SA cleanup")
	if err := c.Delete(ctx, prop); err != nil {
		t.Fatalf("delete Proposal: %v", err)
	}
	waitForDeletion(t, c, prop.Name)

	if err := c.Get(ctx, types.NamespacedName{Name: roleName, Namespace: "staging"}, &role); err == nil {
		t.Errorf("Role %s still exists after Proposal deletion — RBAC not cleaned up", roleName)
	}
	if err := c.Get(ctx, types.NamespacedName{Name: saName, Namespace: testNS}, &sa); err == nil {
		t.Errorf("SA %s still exists after Proposal deletion — not cleaned up", saName)
	}
	t.Log("Verified: RBAC Role + SA cleaned up after deletion")

	t.Logf("PASS: execution complete, RBAC + SA created and cleaned up")
}
