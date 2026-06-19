//go:build e2e

package e2e

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
)

// TestSuspension verifies the full suspension lifecycle: activating the kill
// switch emergency-stops in-flight proposals, sets the Suspended=True status
// condition on AgenticOLSConfig, emits a SuspensionActivated event, and
// preserves the terminal state after the config is deleted (resume).
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

	waitForConfigSuspended(t, c, 1)
	waitForConfigEvent(t, c, "SuspensionActivated", "System suspended; 1 proposals emergency-stopped")
	t.Log("config status and activation event verified")

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

	waitForConfigDeactivated(t, c)
	waitForConfigEvent(t, c, "SuspensionDeactivated", "System resumed; agentic operations re-enabled")
	t.Log("config deactivation condition and event verified")

	// New proposal should proceed past Pending.
	resumed := createProposal(t, c, "suspend-after-resume")
	waitForPhase(t, c, resumed.Name, agenticv1alpha1.ProposalPhaseProposed)
	t.Log("new proposal proceeded normally after resume")
}

// waitForConfigSuspended polls AgenticOLSConfig until the Suspended condition
// is True with reason AdminActivated and a message matching the expected
// emergency-stopped proposal count.
func waitForConfigSuspended(t *testing.T, c client.Client, wantStopped int) {
	t.Helper()
	ctx := context.Background()
	wantMsg := "System suspended; " + strconv.Itoa(wantStopped) + " proposals emergency-stopped"

	err := wait.PollUntilContextTimeout(ctx, pollInterval, pollTimeout, true, func(ctx context.Context) (bool, error) {
		var cfg agenticv1alpha1.AgenticOLSConfig
		if err := c.Get(ctx, types.NamespacedName{Name: "cluster"}, &cfg); err != nil {
			return false, err
		}
		cond := meta.FindStatusCondition(cfg.Status.Conditions, agenticv1alpha1.AgenticOLSConfigConditionSuspended)
		if cond == nil || cond.Status != metav1.ConditionTrue {
			t.Logf("polling config status: condition=%v", cond)
			return false, nil
		}
		if cond.Reason != "AdminActivated" {
			t.Logf("polling config status: reason=%q", cond.Reason)
			return false, nil
		}
		if cond.Message != wantMsg {
			t.Logf("polling config status: message=%q want=%q", cond.Message, wantMsg)
			return false, nil
		}
		return true, nil
	})
	if err != nil {
		t.Fatalf("timed out waiting for Suspended=True on AgenticOLSConfig: %v", err)
	}
}

// waitForConfigDeactivated polls AgenticOLSConfig until spec.suspended is
// false and the Suspended condition transitions to False/AdminDeactivated.
func waitForConfigDeactivated(t *testing.T, c client.Client) {
	t.Helper()
	ctx := context.Background()

	err := wait.PollUntilContextTimeout(ctx, pollInterval, pollTimeout, true, func(ctx context.Context) (bool, error) {
		var cfg agenticv1alpha1.AgenticOLSConfig
		if err := c.Get(ctx, types.NamespacedName{Name: "cluster"}, &cfg); err != nil {
			return false, err
		}
		if cfg.Spec.Suspended {
			t.Log("polling config status: spec still suspended")
			return false, nil
		}
		cond := meta.FindStatusCondition(cfg.Status.Conditions, agenticv1alpha1.AgenticOLSConfigConditionSuspended)
		if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != "AdminDeactivated" {
			t.Logf("polling config status: condition=%v", cond)
			return false, nil
		}
		return true, nil
	})
	if err != nil {
		t.Fatalf("timed out waiting for Suspended=False/AdminDeactivated on AgenticOLSConfig: %v", err)
	}
}

// waitForConfigEvent polls cluster Events until one is found on the "cluster"
// AgenticOLSConfig object matching the given reason and message substring.
func waitForConfigEvent(t *testing.T, c client.Client, reason, message string) {
	t.Helper()
	ctx := context.Background()

	err := wait.PollUntilContextTimeout(ctx, pollInterval, pollTimeout, true, func(ctx context.Context) (bool, error) {
		var events corev1.EventList
		if err := c.List(ctx, &events); err != nil {
			return false, err
		}
		for _, event := range events.Items {
			if event.InvolvedObject.Kind != "AgenticOLSConfig" || event.InvolvedObject.Name != "cluster" {
				continue
			}
			if event.Reason == reason && strings.Contains(event.Message, message) {
				return true, nil
			}
		}
		t.Logf("polling events: reason=%q message=%q (seen %d events)", reason, message, len(events.Items))
		return false, nil
	})
	if err != nil {
		t.Fatalf("timed out waiting for event reason=%q message=%q: %v", reason, message, err)
	}
}
