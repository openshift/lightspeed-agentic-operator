//go:build e2e

package e2e

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
)

func TestSuspension(t *testing.T) {
	c := newClient(t)
	createFixtures(t, c)
	ctx := context.Background()

	config := &agenticv1alpha1.AgenticOLSConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster"},
		Spec:       agenticv1alpha1.AgenticOLSConfigSpec{Suspended: true},
	}
	cleanup(t, c, config)
	t.Cleanup(func() { cleanup(t, c, config) })

	// Activate kill switch — reset fields that cleanup's c.Get may have overwritten.
	config.SetResourceVersion("")
	config.SetUID("")
	config.Spec.Suspended = true
	if err := c.Create(ctx, config); err != nil {
		t.Fatalf("create AgenticOLSConfig: %v", err)
	}

	// Let the controller cache sync.
	time.Sleep(5 * time.Second)

	prop := createProposal(t, c, "suspend-inflight")

	// Proposal should reach EmergencyStopped on its first reconcile.
	waitForPhase(t, c, prop.Name, agenticv1alpha1.ProposalPhaseEmergencyStopped)
	t.Log("proposal terminated by suspension guard")

	// Resume: delete config.
	if err := c.Delete(ctx, &agenticv1alpha1.AgenticOLSConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster"},
	}); err != nil {
		t.Fatalf("delete config to resume: %v", err)
	}
	time.Sleep(5 * time.Second)

	// Verify: stopped proposal stays EmergencyStopped after resume.
	var updated agenticv1alpha1.Proposal
	if err := c.Get(ctx, client.ObjectKeyFromObject(prop), &updated); err != nil {
		t.Fatalf("get stopped proposal: %v", err)
	}
	phase := agenticv1alpha1.DerivePhase(updated.Status.Conditions)
	if phase != agenticv1alpha1.ProposalPhaseEmergencyStopped {
		t.Fatalf("expected EmergencyStopped after resume, got %s", phase)
	}
	t.Log("stopped proposal remains terminal after resume")
}

// TestSuspension_InFlight verifies rule 6: a proposal that has already
// progressed past analysis (Proposed phase) is terminated when the kill
// switch activates.
func TestSuspension_InFlight(t *testing.T) {
	c := newClient(t)
	createFixtures(t, c)
	ctx := context.Background()

	prop := createProposal(t, c, "suspend-inflight-proposed")

	// Wait for the proposal to reach Proposed (analysis complete, non-terminal).
	waitForPhase(t, c, prop.Name, agenticv1alpha1.ProposalPhaseProposed)
	t.Log("proposal reached Proposed — activating kill switch")

	config := &agenticv1alpha1.AgenticOLSConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster"},
		Spec:       agenticv1alpha1.AgenticOLSConfigSpec{Suspended: true},
	}
	cleanup(t, c, config)
	t.Cleanup(func() { cleanup(t, c, config) })

	config.SetResourceVersion("")
	config.SetUID("")
	config.Spec.Suspended = true
	if err := c.Create(ctx, config); err != nil {
		t.Fatalf("create AgenticOLSConfig: %v", err)
	}

	// The AgenticOLSConfig watch re-queues all non-terminal proposals.
	waitForPhase(t, c, prop.Name, agenticv1alpha1.ProposalPhaseEmergencyStopped)
	t.Log("in-flight proposal terminated by suspension guard")
}

// TestSuspension_ResumeNewProposal verifies rule 10: after resuming the
// system (suspended → false), new proposals proceed normally.
func TestSuspension_ResumeNewProposal(t *testing.T) {
	c := newClient(t)
	createFixtures(t, c)
	ctx := context.Background()

	config := &agenticv1alpha1.AgenticOLSConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster"},
		Spec:       agenticv1alpha1.AgenticOLSConfigSpec{Suspended: true},
	}
	cleanup(t, c, config)
	t.Cleanup(func() { cleanup(t, c, config) })

	config.SetResourceVersion("")
	config.SetUID("")
	config.Spec.Suspended = true
	if err := c.Create(ctx, config); err != nil {
		t.Fatalf("create AgenticOLSConfig: %v", err)
	}
	time.Sleep(5 * time.Second)

	// Verify suspension works.
	stopped := createProposal(t, c, "suspend-before-resume")
	waitForPhase(t, c, stopped.Name, agenticv1alpha1.ProposalPhaseEmergencyStopped)
	t.Log("confirmed suspension is active")

	// Resume via raw JSON merge patch — avoids omitempty/omitzero serialization
	// issues with bool false, and sends a MODIFIED watch event that the informer
	// cache propagates faster than a DELETE.
	patch := client.RawPatch(types.MergePatchType, []byte(`{"spec":{"suspended":false}}`))
	if err := c.Patch(ctx, config, patch); err != nil {
		t.Fatalf("patch config to resume: %v", err)
	}
	time.Sleep(5 * time.Second)

	// New proposal should proceed past Pending.
	resumed := createProposal(t, c, "suspend-after-resume")
	waitForPhase(t, c, resumed.Name, agenticv1alpha1.ProposalPhaseProposed)
	t.Log("new proposal proceeded normally after resume")
}
