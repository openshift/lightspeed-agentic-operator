//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
)

const (
	suspensionVAPName       = "agentic.openshift.io-agenticrun-suspension"
	suspensionHoldFinalizer = "e2e.agentic.openshift.io/suspension-hold"
)

// TestSuspension verifies the full suspension lifecycle: activating the kill
// switch emergency-stops an in-flight run, sets the Suspended=True status
// condition on AgenticOLSConfig, emits a SuspensionActivated event, and
// preserves the terminal state after the config is deleted (resume).
// New CREATE while suspended is covered by TestSuspension_AdmissionRejectsCreate.
func TestSuspension(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()

	prop := createAgenticRun(t, c, "suspend-inflight")

	config := &agenticv1alpha1.AgenticOLSConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster"},
		Spec:       agenticv1alpha1.AgenticOLSConfigSpec{Suspended: true},
	}
	if err := deleteSuspensionConfig(ctx, c); err != nil {
		t.Fatalf("prepare AgenticOLSConfig: %v", err)
	}
	t.Cleanup(func() { cleanupSuspensionConfig(t, c) })

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

	waitForConfigSuspended(t, c)
	waitForConfigEvent(t, c, "SuspensionActivated")
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
	ctx := context.Background()

	assertSuspensionVAPInstalled(t, c)

	config := &agenticv1alpha1.AgenticOLSConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster"},
		Spec:       agenticv1alpha1.AgenticOLSConfigSpec{Suspended: true},
	}
	if err := deleteSuspensionConfig(ctx, c); err != nil {
		t.Fatalf("prepare AgenticOLSConfig: %v", err)
	}
	t.Cleanup(func() { cleanupSuspensionConfig(t, c) })

	config.SetResourceVersion("")
	config.SetUID("")
	config.Spec.Suspended = true
	if err := c.Create(ctx, config); err != nil {
		t.Fatalf("create AgenticOLSConfig: %v", err)
	}

	name := "suspend-admission-blocked"
	waitForSuspendedAdmissionReject(t, c, name, agenticv1alpha1.AgenticRunSpec{
		Request:  "should be rejected by VAP",
		Analysis: agenticv1alpha1.AgenticRunStep{Agent: "e2e-agent"},
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
	ctx := context.Background()

	assertSuspensionVAPInstalled(t, c)
	if err := deleteSuspensionConfig(ctx, c); err != nil {
		t.Fatalf("prepare absent AgenticOLSConfig: %v", err)
	}
	waitForConfigAbsent(t, c)

	// Poll CREATE until VAP parameter cache observes the missing config
	// (parameterNotFoundAction Allow). A single-shot Create can still see a
	// stale suspended param and flake.
	waitForAdmissionAllow(t, c, "suspend-admission-absent-config", agenticv1alpha1.AgenticRunSpec{
		Request:  "should be allowed when config is absent",
		Analysis: agenticv1alpha1.AgenticRunStep{Agent: "e2e-agent"},
	})
	t.Log("CREATE succeeded with absent AgenticOLSConfig (parameterNotFoundAction Allow)")
}

// TestSuspension_InFlight verifies rule 6: a run that has already
// progressed past analysis (Proposed phase) is terminated when the kill
// switch activates.
func TestSuspension_InFlight(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()

	prop := createAgenticRun(t, c, "suspend-inflight-proposed")

	// Wait for the run to reach Proposed (analysis complete, non-terminal).
	waitForPhase(t, c, prop.Name, agenticv1alpha1.AgenticRunPhaseProposed)
	t.Log("run reached Proposed — activating kill switch")

	config := &agenticv1alpha1.AgenticOLSConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster"},
		Spec:       agenticv1alpha1.AgenticOLSConfigSpec{Suspended: true},
	}
	if err := deleteSuspensionConfig(ctx, c); err != nil {
		t.Fatalf("prepare AgenticOLSConfig: %v", err)
	}
	t.Cleanup(func() { cleanupSuspensionConfig(t, c) })

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
	ctx := context.Background()

	config := &agenticv1alpha1.AgenticOLSConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster"},
		Spec:       agenticv1alpha1.AgenticOLSConfigSpec{Suspended: true},
	}
	if err := deleteSuspensionConfig(ctx, c); err != nil {
		t.Fatalf("prepare AgenticOLSConfig: %v", err)
	}
	t.Cleanup(func() { cleanupSuspensionConfig(t, c) })

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

	waitForConfigSuspended(t, c)

	// Resume via raw JSON merge patch — avoids omitempty/omitzero serialization
	// issues with bool false, and sends a MODIFIED watch event that the informer
	// cache propagates faster than a DELETE.
	patch := client.RawPatch(types.MergePatchType, []byte(`{"spec":{"suspended":false}}`))
	if err := c.Patch(ctx, config, patch); err != nil {
		t.Fatalf("patch config to resume: %v", err)
	}

	waitForConfigDeactivated(t, c)
	waitForConfigEvent(t, c, "SuspensionDeactivated")
	t.Log("config deactivation condition and event verified")

	// Poll CREATE until VAP observes suspended=false (replaces fixed sleep).
	// Execution must be configured so the run waits for approval at Proposed
	// rather than auto-completing (analysis-only runs skip directly to Completed).
	waitForAdmissionAllow(t, c, "suspend-after-resume", agenticv1alpha1.AgenticRunSpec{
		Request:   "resume after suspend",
		Analysis:  agenticv1alpha1.AgenticRunStep{Agent: "e2e-agent"},
		Execution: agenticv1alpha1.AgenticRunStep{Agent: "e2e-agent"},
	})
	waitForPhase(t, c, "suspend-after-resume", agenticv1alpha1.AgenticRunPhaseProposed)
	t.Log("new run proceeded normally after resume")
}

// TestSuspension_AdmissionRequiresCurrentCleanupSafety exercises real API
// UPDATE and DELETE admission while a managed operator-namespace Pod is held
// by a test-owned finalizer.
func TestSuspension_AdmissionRequiresCurrentCleanupSafety(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()
	if err := deleteSuspensionConfig(ctx, c); err != nil {
		t.Fatalf("prepare AgenticOLSConfig: %v", err)
	}
	t.Cleanup(func() { cleanupSuspensionConfig(t, c) })

	runUID := newSuspensionRunUID()
	pod := createSuspensionHeldPod(t, c, runUID)
	// Keep the required spec present when all typed spec fields are zero.
	activeConfig := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "agentic.openshift.io/v1alpha1",
		"kind":       "AgenticOLSConfig",
		"metadata":   map[string]interface{}{"name": "cluster"},
		"spec":       map[string]interface{}{"suspended": false},
	}}
	if err := c.Create(ctx, activeConfig); err != nil {
		t.Fatalf("create AgenticOLSConfig: %v", err)
	}

	if err := patchSuspensionValue(ctx, c, true); err != nil {
		t.Fatalf("active-config transition to suspended was denied: %v", err)
	}

	config := waitForConfigOutcome(t, c, "AdminActivatedWithErrors", metav1.ConditionFalse)
	assertSuspensionDeletionStarted(t, c, pod)
	if err := patchSuspensionValue(ctx, c, false); err == nil {
		t.Fatal("unsafe resume unexpectedly succeeded")
	} else {
		assertConfigSafetyDenial(t, err, "cannot resume while suspension cleanup")
	}
	if err := removeSuspendedField(ctx, c); err == nil {
		t.Fatal("removing spec.suspended unexpectedly succeeded")
	} else {
		assertConfigSafetyDenial(t, err, "cannot resume while suspension cleanup")
	}
	if err := c.Delete(ctx, &agenticv1alpha1.AgenticOLSConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster"},
	}); err == nil {
		t.Fatal("unsafe config deletion unexpectedly succeeded")
	} else {
		assertConfigSafetyDenial(t, err, "cannot delete a suspended config")
	}

	cfg, err := getSuspensionConfig(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	base := cfg.DeepCopy()
	cfg.Annotations = map[string]string{"agentic.openshift.io/e2e-admission-probe": "retained-suspended"}
	if err := c.Patch(ctx, cfg, client.MergeFrom(base)); err != nil {
		t.Fatalf("annotation update retaining suspended=true was denied: %v", err)
	}

	cfg, err = getSuspensionConfig(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Status().Patch(ctx, cfg, client.RawPatch(types.MergePatchType, []byte(`{"status":{}}`))); err != nil {
		t.Fatalf("/status write was denied by main-resource admission: %v", err)
	}

	if err := releaseSuspensionObject(ctx, c, pod); err != nil {
		t.Fatalf("release held Pod: %v", err)
	}
	config = waitForConfigOutcome(t, c, "AdminActivatedWithErrors", metav1.ConditionTrue)
	suspended := meta.FindStatusCondition(config.Status.Conditions, agenticv1alpha1.AgenticOLSConfigConditionSuspended)
	if suspended == nil || suspended.Reason != "AdminActivatedWithErrors" {
		t.Fatalf("cleanup recovery changed terminal suspension outcome: condition=%v", suspended)
	}
	if err := patchSuspensionValue(ctx, c, false); err != nil {
		t.Fatalf("resume after current safety confirmation: %v", err)
	}
	waitForConfigDeactivated(t, c)

	if err := c.Delete(ctx, &agenticv1alpha1.AgenticOLSConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster"},
	}); err != nil {
		t.Fatalf("delete active config after safe resumption: %v", err)
	}
	waitForConfigAbsent(t, c)
}

// TestSuspension_AdmissionKeepsAccessOnlyBlocker proves that deleting the
// managed Pod is insufficient while a run-owned identity remains.
func TestSuspension_AdmissionKeepsAccessOnlyBlocker(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()
	if err := deleteSuspensionConfig(ctx, c); err != nil {
		t.Fatalf("prepare AgenticOLSConfig: %v", err)
	}
	t.Cleanup(func() { cleanupSuspensionConfig(t, c) })

	runUID := newSuspensionRunUID()
	account := createSuspensionHeldServiceAccount(t, c, runUID)
	var pods corev1.PodList
	if err := c.List(ctx, &pods, client.InNamespace(suspensionOperatorNamespace()),
		client.MatchingLabels{"agentic.openshift.io/run": string(runUID)}); err != nil {
		t.Fatalf("list Pods for access-only fixture: %v", err)
	}
	if len(pods.Items) != 0 {
		t.Fatalf("access-only fixture has %d managed Pods before activation", len(pods.Items))
	}

	config := &agenticv1alpha1.AgenticOLSConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster"},
		Spec:       agenticv1alpha1.AgenticOLSConfigSpec{Suspended: true},
	}
	if err := c.Create(ctx, config); err != nil {
		t.Fatalf("create AgenticOLSConfig: %v", err)
	}
	waitForConfigOutcome(t, c, "AdminActivatedWithErrors", metav1.ConditionFalse)
	assertSuspensionDeletionStarted(t, c, account)
	if err := c.Delete(ctx, &agenticv1alpha1.AgenticOLSConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster"},
	}); err == nil {
		t.Fatal("config deletion succeeded while temporary access remained")
	} else {
		assertConfigSafetyDenial(t, err, "cannot delete a suspended config")
	}

	if err := releaseSuspensionObject(ctx, c, account); err != nil {
		t.Fatalf("release held ServiceAccount: %v", err)
	}
	config = waitForConfigOutcome(t, c, "AdminActivatedWithErrors", metav1.ConditionTrue)
	suspended := meta.FindStatusCondition(config.Status.Conditions, agenticv1alpha1.AgenticOLSConfigConditionSuspended)
	if suspended == nil || suspended.Reason != "AdminActivatedWithErrors" {
		t.Fatalf("access recovery changed terminal suspension outcome: condition=%v", suspended)
	}
	if err := c.Delete(ctx, config); err != nil {
		t.Fatalf("safe config deletion after access cleanup: %v", err)
	}
	waitForConfigAbsent(t, c)
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

// waitForConfigSuspended waits for current-generation terminal completion and
// independent current-generation resumption safety.
func waitForConfigSuspended(t *testing.T, c client.Client) {
	t.Helper()
	waitForConfigOutcome(t, c, "AdminActivated", metav1.ConditionTrue)
}

// waitForConfigDeactivated polls until the current config generation is active.
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
		if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != "AdminDeactivated" ||
			cond.ObservedGeneration != cfg.Generation {
			t.Logf("polling config status: condition=%v generation=%d", cond, cfg.Generation)
			return false, nil
		}
		return true, nil
	})
	if err != nil {
		t.Fatalf("timed out waiting for current-generation Suspended=False/AdminDeactivated: %v", err)
	}
}

// waitForConfigEvent polls cluster Events until one is found on the "cluster"
// AgenticOLSConfig object with the requested reason.
func waitForConfigEvent(t *testing.T, c client.Client, reason string) {
	t.Helper()
	ctx := context.Background()

	err := wait.PollUntilContextTimeout(ctx, pollInterval, pollTimeout, true, func(ctx context.Context) (bool, error) {
		var events corev1.EventList
		if err := c.List(ctx, &events); err != nil {
			return false, err
		}
		for _, event := range events.Items {
			if event.InvolvedObject.Kind == "AgenticOLSConfig" &&
				event.InvolvedObject.Name == "cluster" && event.Reason == reason {
				return true, nil
			}
		}
		t.Logf("polling events: reason=%q (seen %d events)", reason, len(events.Items))
		return false, nil
	})
	if err != nil {
		t.Fatalf("timed out waiting for event reason=%q: %v", reason, err)
	}
}
func waitForConfigOutcome(t *testing.T, c client.Client, reason string, safetyStatus metav1.ConditionStatus) *agenticv1alpha1.AgenticOLSConfig {
	t.Helper()
	ctx := context.Background()

	var observed *agenticv1alpha1.AgenticOLSConfig
	err := wait.PollUntilContextTimeout(ctx, pollInterval, pollTimeout, true, func(ctx context.Context) (bool, error) {
		var cfg agenticv1alpha1.AgenticOLSConfig
		if err := c.Get(ctx, types.NamespacedName{Name: "cluster"}, &cfg); err != nil {
			return false, err
		}
		suspended := meta.FindStatusCondition(cfg.Status.Conditions, agenticv1alpha1.AgenticOLSConfigConditionSuspended)
		safe := meta.FindStatusCondition(cfg.Status.Conditions, agenticv1alpha1.AgenticOLSConfigConditionSafeToResume)
		if !cfg.Spec.Suspended || suspended == nil || suspended.Status != metav1.ConditionTrue ||
			suspended.Reason != reason || suspended.ObservedGeneration != cfg.Generation ||
			safe == nil || safe.Status != safetyStatus || safe.ObservedGeneration != cfg.Generation {
			t.Logf("polling config outcome: generation=%d Suspended=%v SafeToResume=%v",
				cfg.Generation, suspended, safe)
			return false, nil
		}
		observed = cfg.DeepCopy()
		return true, nil
	})
	if err != nil {
		t.Fatalf("timed out waiting for Suspended=%s and SafeToResume=%s at the current generation: %v",
			reason, safetyStatus, err)
	}
	return observed
}

func getSuspensionConfig(ctx context.Context, c client.Client) (*agenticv1alpha1.AgenticOLSConfig, error) {
	var cfg agenticv1alpha1.AgenticOLSConfig
	if err := c.Get(ctx, types.NamespacedName{Name: "cluster"}, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func deleteSuspensionConfig(ctx context.Context, c client.Client) error {
	cfg, err := getSuspensionConfig(ctx, c)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("get AgenticOLSConfig before deletion: %w", err)
	}
	if cfg.Spec.Suspended {
		err = wait.PollUntilContextTimeout(ctx, pollInterval, cleanupTimeout, true, func(ctx context.Context) (bool, error) {
			current, err := getSuspensionConfig(ctx, c)
			if apierrors.IsNotFound(err) {
				return true, nil
			}
			if err != nil {
				return false, err
			}
			suspended := meta.FindStatusCondition(current.Status.Conditions, agenticv1alpha1.AgenticOLSConfigConditionSuspended)
			safe := meta.FindStatusCondition(current.Status.Conditions, agenticv1alpha1.AgenticOLSConfigConditionSafeToResume)
			return suspended != nil && suspended.Status == metav1.ConditionTrue &&
				(suspended.Reason == "AdminActivated" || suspended.Reason == "AdminActivatedWithErrors") &&
				suspended.ObservedGeneration == current.Generation &&
				safe != nil && safe.Status == metav1.ConditionTrue &&
				safe.ObservedGeneration == current.Generation, nil
		})
		if err != nil {
			return fmt.Errorf("wait for current-generation terminal completion and SafeToResume=True before config deletion: %w", err)
		}
	}

	cfg, err = getSuspensionConfig(ctx, c)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("get AgenticOLSConfig before deletion: %w", err)
	}
	if err := c.Delete(ctx, cfg); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete AgenticOLSConfig (admission may deny unsafe cleanup): %w", err)
	}
	return wait.PollUntilContextTimeout(ctx, pollInterval, cleanupTimeout, true, func(ctx context.Context) (bool, error) {
		_, err := getSuspensionConfig(ctx, c)
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		if err != nil {
			return false, err
		}
		return false, nil
	})
}

func cleanupSuspensionConfig(t *testing.T, c client.Client) {
	t.Helper()
	if err := deleteSuspensionConfig(context.Background(), c); err != nil {
		t.Errorf("AgenticOLSConfig cleanup failed; admission denial is not ignored: %v", err)
	}
}

func waitForConfigAbsent(t *testing.T, c client.Client) {
	t.Helper()
	ctx := context.Background()
	err := wait.PollUntilContextTimeout(ctx, pollInterval, pollTimeout, true, func(ctx context.Context) (bool, error) {
		_, err := getSuspensionConfig(ctx, c)
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		if err != nil {
			return false, err
		}
		return false, nil
	})
	if err != nil {
		t.Fatalf("timed out waiting for AgenticOLSConfig absence: %v", err)
	}
}

func patchSuspensionValue(ctx context.Context, c client.Client, suspended bool) error {
	cfg, err := getSuspensionConfig(ctx, c)
	if err != nil {
		return err
	}
	patch := client.RawPatch(types.MergePatchType, []byte(fmt.Sprintf(`{"spec":{"suspended":%t}}`, suspended)))
	return c.Patch(ctx, cfg, patch)
}

func removeSuspendedField(ctx context.Context, c client.Client) error {
	cfg, err := getSuspensionConfig(ctx, c)
	if err != nil {
		return err
	}
	patch := client.RawPatch(types.JSONPatchType, []byte(`[{"op":"remove","path":"/spec/suspended"}]`))
	return c.Patch(ctx, cfg, patch)
}

func assertConfigSafetyDenial(t *testing.T, err error, message string) {
	t.Helper()
	if !apierrors.IsForbidden(err) && !apierrors.IsInvalid(err) {
		t.Fatalf("request was not denied by admission: %v", err)
	}
	if !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(message)) {
		t.Fatalf("admission error does not identify the AgenticOLSConfig safety policy: %v", err)
	}
}

func assertSuspensionDeletionStarted(t *testing.T, c client.Client, obj client.Object) {
	t.Helper()
	ctx := context.Background()
	key := client.ObjectKeyFromObject(obj)
	if err := c.Get(ctx, key, obj); err != nil {
		t.Fatalf("get managed %T %s/%s after suspension completion: %v", obj, key.Namespace, key.Name, err)
	}
	if obj.GetDeletionTimestamp() == nil {
		t.Fatalf("suspension sweep did not start deleting managed %T %s/%s", obj, key.Namespace, key.Name)
	}
}

func newSuspensionRunUID() types.UID {
	return types.UID(fmt.Sprintf("e2e-%d", time.Now().UnixNano()))
}

func suspensionOperatorNamespace() string {
	if namespace := os.Getenv("OPERATOR_NAMESPACE"); namespace != "" {
		return namespace
	}
	return "openshift-lightspeed"
}

func createSuspensionHeldPod(t *testing.T, c client.Client, runUID types.UID) *corev1.Pod {
	t.Helper()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("suspension-held-%d", time.Now().UnixNano()),
			Namespace: suspensionOperatorNamespace(),
			Labels: map[string]string{
				"agentic.openshift.io/run":  string(runUID),
				"agentic.openshift.io/step": "analysis",
			},
			Finalizers: []string{suspensionHoldFinalizer},
		},
		Spec: corev1.PodSpec{
			Containers:    []corev1.Container{{Name: "hold", Image: "registry.k8s.io/pause:3.9"}},
			RestartPolicy: corev1.RestartPolicyNever,
		},
	}
	t.Cleanup(func() { cleanupSuspensionObject(t, c, pod) })
	if err := c.Create(context.Background(), pod); err != nil {
		t.Fatalf("create managed held Pod in operator namespace %q: %v", pod.Namespace, err)
	}
	return pod
}

func createSuspensionHeldServiceAccount(t *testing.T, c client.Client, runUID types.UID) *corev1.ServiceAccount {
	t.Helper()
	account := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("suspension-held-sa-%d", time.Now().UnixNano()),
			Namespace: suspensionOperatorNamespace(),
			Labels: map[string]string{
				"agentic.openshift.io/run":       string(runUID),
				"agentic.openshift.io/component": "analysis-sa",
			},
			Finalizers: []string{suspensionHoldFinalizer},
		},
	}
	t.Cleanup(func() { cleanupSuspensionObject(t, c, account) })
	if err := c.Create(context.Background(), account); err != nil {
		t.Fatalf("create managed held ServiceAccount in operator namespace %q: %v", account.Namespace, err)
	}
	return account
}

func releaseSuspensionObject(ctx context.Context, c client.Client, obj client.Object) error {
	key := client.ObjectKeyFromObject(obj)
	if err := c.Get(ctx, key, obj); apierrors.IsNotFound(err) {
		return nil
	} else if err != nil {
		return fmt.Errorf("get held %T %s/%s: %w", obj, key.Namespace, key.Name, err)
	}
	if obj.GetDeletionTimestamp() == nil {
		if err := c.Delete(ctx, obj); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete held %T %s/%s: %w", obj, key.Namespace, key.Name, err)
		}
		if err := c.Get(ctx, key, obj); apierrors.IsNotFound(err) {
			return nil
		} else if err != nil {
			return fmt.Errorf("reload held %T %s/%s: %w", obj, key.Namespace, key.Name, err)
		}
	}

	var remaining []string
	hasTestFinalizer := false
	for _, finalizer := range obj.GetFinalizers() {
		if finalizer == suspensionHoldFinalizer {
			hasTestFinalizer = true
			continue
		}
		remaining = append(remaining, finalizer)
	}
	if hasTestFinalizer {
		base := obj.DeepCopyObject().(client.Object)
		obj.SetFinalizers(remaining)
		if err := c.Patch(ctx, obj, client.MergeFrom(base)); err != nil {
			return fmt.Errorf("remove test finalizer from %T %s/%s: %w", obj, key.Namespace, key.Name, err)
		}
	}

	err := wait.PollUntilContextTimeout(ctx, pollInterval, cleanupTimeout, true, func(ctx context.Context) (bool, error) {
		err := c.Get(ctx, key, obj)
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		if err != nil {
			return false, err
		}
		return false, nil
	})
	if err != nil {
		return fmt.Errorf("wait for held %T %s/%s to disappear: %w", obj, key.Namespace, key.Name, err)
	}
	return nil
}

func cleanupSuspensionObject(t *testing.T, c client.Client, obj client.Object) {
	t.Helper()
	if err := releaseSuspensionObject(context.Background(), c, obj); err != nil {
		t.Errorf("suspension fixture cleanup failed: %v", err)
	}
}
