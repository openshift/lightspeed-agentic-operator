//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
)

const suspensionVAPName = "agentic.openshift.io-agenticrun-suspension"

// TestSuspension verifies the full suspension lifecycle: activating the kill
// switch emergency-stops an in-flight run, sets the Suspended=True status
// condition on AgenticOLSConfig, emits a SuspensionActivated event, and
// preserves the terminal state after the config is deleted (resume).
// New CREATE while suspended is covered by TestSuspension_AdmissionRejectsCreate.
func TestSuspension(t *testing.T) {
	c := newClient(t)
	createFixtures(t, c)
	ctx := context.Background()

	prop := createAgenticRun(t, c, "suspend-inflight")

	config := &agenticv1alpha1.AgenticOLSConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster"},
		Spec:       agenticv1alpha1.AgenticOLSConfigSpec{Suspended: true},
	}
	cleanup(t, c, config)
	t.Cleanup(func() { cleanup(t, c, config) })

	// Activate kill switch after the run exists — reset fields that cleanup may overwrite.
	config.SetResourceVersion("")
	config.SetUID("")
	config.Spec.Suspended = true
	if err := c.Create(ctx, config); err != nil {
		t.Fatalf("create AgenticOLSConfig: %v", err)
	}

	// AgenticRun should reach EmergencyStopped via the reconciler guard.
	waitForPhase(t, c, prop.Name, agenticv1alpha1.AgenticRunPhaseEmergencyStopped)
	t.Log("run terminated by suspension guard")

	waitForConfigSuspended(t, c, 1)
	waitForConfigEvent(t, c, "SuspensionActivated", "System suspended; 1 runs emergency-stopped")
	t.Log("config status and activation event verified")

	// Resume: delete config.
	if err := c.Delete(ctx, &agenticv1alpha1.AgenticOLSConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster"},
	}); err != nil {
		t.Fatalf("delete config to resume: %v", err)
	}
	time.Sleep(5 * time.Second)

	// Verify: stopped run stays EmergencyStopped after resume.
	var updated agenticv1alpha1.AgenticRun
	if err := c.Get(ctx, client.ObjectKeyFromObject(prop), &updated); err != nil {
		t.Fatalf("get stopped run: %v", err)
	}
	phase := agenticv1alpha1.DerivePhase(updated.Status.Conditions)
	if phase != agenticv1alpha1.AgenticRunPhaseEmergencyStopped {
		t.Fatalf("expected EmergencyStopped after resume, got %s", phase)
	}
	t.Log("stopped run remains terminal after resume")
}

// TestSuspension_AdmissionRejectsCreate verifies rules 11a–11b: while suspended,
// AgenticRun CREATE is rejected at admission and no CR is persisted. Also checks
// that the install path shipped the VAP and binding.
func TestSuspension_AdmissionRejectsCreate(t *testing.T) {
	c := newClient(t)
	createFixtures(t, c)
	ctx := context.Background()

	assertSuspensionVAPInstalled(t, c)

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

	name := "suspend-admission-blocked"
	waitForSuspendedAdmissionReject(t, c, name, agenticv1alpha1.AgenticRunSpec{
		Request:          "should be rejected by VAP",
		TargetNamespaces: []string{"staging"},
		Analysis:         agenticv1alpha1.AgenticRunStep{Agent: "e2e-agent"},
	})

	var got agenticv1alpha1.AgenticRun
	if getErr := c.Get(ctx, types.NamespacedName{Name: name, Namespace: testNS}, &got); !apierrors.IsNotFound(getErr) {
		t.Fatalf("expected AgenticRun not to exist after admission reject, get err=%v", getErr)
	}
}

// TestSuspension_AdmissionAllowsCreateWhenConfigAbsent verifies rule 11c:
// with no AgenticOLSConfig, VAP parameterNotFoundAction Allow permits CREATE.
func TestSuspension_AdmissionAllowsCreateWhenConfigAbsent(t *testing.T) {
	c := newClient(t)
	createFixtures(t, c)
	ctx := context.Background()

	assertSuspensionVAPInstalled(t, c)

	// Ensure the param object is absent (other e2e tests may leave it).
	if err := c.Delete(ctx, &agenticv1alpha1.AgenticOLSConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster"},
	}); err != nil && !apierrors.IsNotFound(err) {
		t.Fatalf("delete AgenticOLSConfig: %v", err)
	}
	err := wait.PollUntilContextTimeout(ctx, pollInterval, pollTimeout, true, func(ctx context.Context) (bool, error) {
		var cfg agenticv1alpha1.AgenticOLSConfig
		getErr := c.Get(ctx, types.NamespacedName{Name: "cluster"}, &cfg)
		if apierrors.IsNotFound(getErr) {
			return true, nil
		}
		if getErr != nil {
			return false, getErr
		}
		return false, nil
	})
	if err != nil {
		t.Fatalf("timed out waiting for AgenticOLSConfig absence: %v", err)
	}

	// Poll CREATE until VAP parameter cache observes the missing config
	// (parameterNotFoundAction Allow). A single-shot Create can still see a
	// stale suspended param and flake.
	waitForAdmissionAllow(t, c, "suspend-admission-absent-config", agenticv1alpha1.AgenticRunSpec{
		Request:          "should be allowed when config is absent",
		TargetNamespaces: []string{"staging"},
		Analysis:         agenticv1alpha1.AgenticRunStep{Agent: "e2e-agent"},
	})
	t.Log("CREATE succeeded with absent AgenticOLSConfig (parameterNotFoundAction Allow)")
}

// TestSuspension_InFlight verifies rule 6: a run that has already
// progressed past analysis (Proposed phase) is terminated when the kill
// switch activates.
func TestSuspension_InFlight(t *testing.T) {
	c := newClient(t)
	createFixtures(t, c)
	ctx := context.Background()

	prop := createAgenticRun(t, c, "suspend-inflight-proposed")

	// Wait for the run to reach Proposed (analysis complete, non-terminal).
	waitForPhase(t, c, prop.Name, agenticv1alpha1.AgenticRunPhaseProposed)
	t.Log("run reached Proposed — activating kill switch")

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
	waitForPhase(t, c, prop.Name, agenticv1alpha1.AgenticRunPhaseEmergencyStopped)
	t.Log("in-flight run terminated by suspension guard")
}

// TestSuspension_ResumeNewAgenticRun verifies rule 10: after resuming the
// system (suspended → false), new runs proceed normally. While suspended,
// CREATE is rejected by admission (rule 11a).
func TestSuspension_ResumeNewAgenticRun(t *testing.T) {
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

	// Verify admission blocking while suspended (poll until VAP param cache catches up).
	waitForSuspendedAdmissionReject(t, c, "suspend-before-resume", agenticv1alpha1.AgenticRunSpec{
		Request:  "should be rejected while suspended",
		Analysis: agenticv1alpha1.AgenticRunStep{Agent: "e2e-agent"},
	})
	t.Log("confirmed admission blocking while suspended")

	// Resume via raw JSON merge patch — avoids omitempty/omitzero serialization
	// issues with bool false, and sends a MODIFIED watch event that the informer
	// cache propagates faster than a DELETE.
	patch := client.RawPatch(types.MergePatchType, []byte(`{"spec":{"suspended":false}}`))
	if err := c.Patch(ctx, config, patch); err != nil {
		t.Fatalf("patch config to resume: %v", err)
	}

	waitForConfigDeactivated(t, c)
	waitForConfigEvent(t, c, "SuspensionDeactivated", "System resumed; agentic operations re-enabled")
	t.Log("config deactivation condition and event verified")

	// Poll CREATE until VAP observes suspended=false (replaces fixed sleep).
	waitForAdmissionAllow(t, c, "suspend-after-resume", agenticv1alpha1.AgenticRunSpec{
		Request:          "resume after suspend",
		TargetNamespaces: []string{"staging"},
		Analysis:         agenticv1alpha1.AgenticRunStep{Agent: "e2e-agent"},
	})
	waitForPhase(t, c, "suspend-after-resume", agenticv1alpha1.AgenticRunPhaseProposed)
	t.Log("new run proceeded normally after resume")
}

// assertSuspensionVAPInstalled fails if the suspension policy or binding is missing.
func assertSuspensionVAPInstalled(t *testing.T, c client.Client) {
	t.Helper()
	ctx := context.Background()

	var policy admissionregistrationv1.ValidatingAdmissionPolicy
	if err := c.Get(ctx, types.NamespacedName{Name: suspensionVAPName}, &policy); err != nil {
		t.Fatalf("ValidatingAdmissionPolicy %s not installed (deploy path must apply config/admission): %v", suspensionVAPName, err)
	}
	var binding admissionregistrationv1.ValidatingAdmissionPolicyBinding
	if err := c.Get(ctx, types.NamespacedName{Name: suspensionVAPName}, &binding); err != nil {
		t.Fatalf("ValidatingAdmissionPolicyBinding %s not installed: %v", suspensionVAPName, err)
	}
	if binding.Spec.ParamRef == nil || binding.Spec.ParamRef.ParameterNotFoundAction == nil ||
		*binding.Spec.ParamRef.ParameterNotFoundAction != admissionregistrationv1.AllowAction {
		t.Fatalf("expected binding parameterNotFoundAction=Allow, got %+v", binding.Spec.ParamRef)
	}
}

// waitForSuspendedAdmissionReject polls AgenticRun CREATE until the VAP denies
// it with Forbidden or Invalid and a "suspended" message. Retries when CREATE
// succeeds (param cache lag) by deleting the accidental object. Fatals on
// timeout; logs the expected rejection on success.
func waitForSuspendedAdmissionReject(t *testing.T, c client.Client, name string, spec agenticv1alpha1.AgenticRunSpec) {
	t.Helper()
	ctx := context.Background()

	var lastErr error
	err := wait.PollUntilContextTimeout(ctx, pollInterval, pollTimeout, true, func(ctx context.Context) (bool, error) {
		run := &agenticv1alpha1.AgenticRun{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNS},
			Spec:       spec,
		}
		createErr := c.Create(ctx, run)
		if createErr == nil {
			t.Logf("CREATE succeeded before VAP observed suspension — deleting and retrying")
			cleanup(t, c, run)
			return false, nil
		}
		lastErr = createErr
		if !apierrors.IsForbidden(createErr) && !apierrors.IsInvalid(createErr) {
			t.Logf("waiting for admission Forbidden/Invalid, got %T / %v", createErr, createErr)
			return false, nil
		}
		if !strings.Contains(strings.ToLower(createErr.Error()), "suspended") {
			return false, fmt.Errorf("admission error missing suspended mention: %w", createErr)
		}
		return true, nil
	})
	if err != nil {
		t.Fatalf("timed out waiting for suspended admission reject (last=%v): %v", lastErr, err)
	}
	t.Logf("CREATE rejected as expected: %v", lastErr)
}

// waitForAdmissionAllow polls AgenticRun CREATE until admission accepts it.
// Retries while the VAP still denies with a suspension error (stale param
// cache after delete/resume). The created object is registered for cleanup.
func waitForAdmissionAllow(t *testing.T, c client.Client, name string, spec agenticv1alpha1.AgenticRunSpec) {
	t.Helper()
	ctx := context.Background()

	var lastErr error
	err := wait.PollUntilContextTimeout(ctx, pollInterval, pollTimeout, true, func(ctx context.Context) (bool, error) {
		run := &agenticv1alpha1.AgenticRun{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNS},
			Spec:       spec,
		}
		createErr := c.Create(ctx, run)
		if createErr == nil {
			t.Cleanup(func() { cleanup(t, c, run) })
			return true, nil
		}
		lastErr = createErr
		if apierrors.IsAlreadyExists(createErr) {
			// A prior successful create from this poll left the object.
			t.Cleanup(func() { cleanup(t, c, run) })
			return true, nil
		}
		if (apierrors.IsForbidden(createErr) || apierrors.IsInvalid(createErr)) &&
			strings.Contains(strings.ToLower(createErr.Error()), "suspended") {
			t.Logf("CREATE still denied by stale VAP param cache — retrying: %v", createErr)
			return false, nil
		}
		t.Logf("waiting for admission allow, got %T / %v", createErr, createErr)
		return false, nil
	})
	if err != nil {
		t.Fatalf("timed out waiting for admission allow (last=%v): %v", lastErr, err)
	}
}

// waitForConfigSuspended polls AgenticOLSConfig until the Suspended condition
// is True with reason AdminActivated and a message matching the expected
// emergency-stopped run count.
func waitForConfigSuspended(t *testing.T, c client.Client, wantStopped int) {
	t.Helper()
	ctx := context.Background()
	wantMsg := "System suspended; " + strconv.Itoa(wantStopped) + " runs emergency-stopped"

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
