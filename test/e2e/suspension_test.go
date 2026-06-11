//go:build e2e

package e2e

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
)

func TestSuspension(t *testing.T) {
	c := newClient(t)
	createFixtures(t, c)
	ctx := context.Background()

	t.Run("suspend terminates in-flight proposal", func(t *testing.T) {
		prop := createProposal(t, c, "suspend-inflight")

		// Wait for proposal to start analyzing (proves it's actively progressing).
		waitForPhase(t, c, prop.Name, agenticv1alpha1.ProposalPhaseAnalyzing)

		// Create AgenticOLSConfig with suspended=true.
		config := &agenticv1alpha1.AgenticOLSConfig{
			ObjectMeta: metav1.ObjectMeta{Name: "cluster"},
			Spec:       agenticv1alpha1.AgenticOLSConfigSpec{Suspended: true},
		}
		cleanup(t, c, config)
		if err := c.Create(ctx, config); err != nil {
			t.Fatalf("create AgenticOLSConfig: %v", err)
		}
		t.Cleanup(func() { cleanup(t, c, config) })

		// Proposal should reach EmergencyStopped.
		waitForPhase(t, c, prop.Name, agenticv1alpha1.ProposalPhaseEmergencyStopped)
		t.Log("proposal terminated by suspension")
	})

	t.Run("EmergencyStopped stays terminal after resume", func(t *testing.T) {
		// Config is still suspended from previous test — set suspended=false.
		var config agenticv1alpha1.AgenticOLSConfig
		if err := c.Get(ctx, client.ObjectKey{Name: "cluster"}, &config); err != nil {
			t.Fatalf("get config: %v", err)
		}
		base := config.DeepCopy()
		config.Spec.Suspended = false
		if err := c.Patch(ctx, &config, client.MergeFrom(base)); err != nil {
			t.Fatalf("patch config to resume: %v", err)
		}

		// Check that the stopped proposal from previous test is still EmergencyStopped.
		var prop agenticv1alpha1.Proposal
		if err := c.Get(ctx, client.ObjectKeyFromObject(&agenticv1alpha1.Proposal{
			ObjectMeta: metav1.ObjectMeta{Name: "suspend-inflight", Namespace: testNS},
		}), &prop); err != nil {
			t.Fatalf("get stopped proposal: %v", err)
		}
		phase := agenticv1alpha1.DerivePhase(prop.Status.Conditions)
		if phase != agenticv1alpha1.ProposalPhaseEmergencyStopped {
			t.Fatalf("expected EmergencyStopped after resume, got %s", phase)
		}
		t.Log("stopped proposal remains terminal after resume")
	})

	t.Run("new proposal proceeds after resume", func(t *testing.T) {
		// System is now resumed (suspended=false from previous test).
		prop := createProposal(t, c, "post-resume")

		// Should progress past Pending into Analyzing.
		waitForPhase(t, c, prop.Name, agenticv1alpha1.ProposalPhaseAnalyzing)
		t.Log("new proposal proceeds normally after resume")
	})
}
