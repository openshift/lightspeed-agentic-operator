//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
)

// TestSandboxCrash validates that the operator correctly handles a sandbox pod
// that crashes (exits non-zero) without producing a Result CR:
//
//  1. Create AgenticRun with MOCK_CRASH in the request text
//  2. Mock agent exits 1 immediately — no Result CR created
//  3. Operator detects pod failure → sets Analyzed=False/SandboxFailed
//  4. AgenticRun transitions to Failed
func TestSandboxCrash(t *testing.T) {
	t.Log("=== TestSandboxCrash: validates sandbox crash → Failed with SandboxFailed ===")
	c := newClient(t)

	prop := createAgenticRunWithRequest(t, c, "e2e-sandbox-crash", "MOCK_CRASH")
	t.Logf("AgenticRun created: %s/%s", testNS, prop.Name)

	t.Log("Waiting for phase: Failed (sandbox crash)")
	updated := waitForPhase(t, c, prop.Name, agenticv1alpha1.AgenticRunPhaseFailed)
	t.Log("Phase reached: Failed")

	assertStepCondition(t, updated.Status.Conditions, agenticv1alpha1.AgenticRunConditionAnalyzed,
		metav1.ConditionFalse, "SandboxFailed")

	if len(updated.Status.Steps.Analysis.Results) > 0 {
		t.Errorf("expected no analysis result refs (pod crashed), got %d", len(updated.Status.Steps.Analysis.Results))
	}

	t.Log("PASS: sandbox crash detected, phase=Failed, reason=SandboxFailed, no result ref")
}

// TestAgentFailure validates that the operator correctly handles an agent that
// reports failure via the Result CR's failureReason field:
//
//  1. Create AnalysisResult CR with failureReason and Completed=True
//  2. Create AgenticRun with MOCK_AGENT_FAIL (mock exits immediately)
//  3. Operator finds pre-created Result CR with failureReason
//  4. Operator sets Analyzed=False/SandboxFailed with the failure reason,
//     records the result ref
func TestAgentFailure(t *testing.T) {
	t.Log("=== TestAgentFailure: validates agent failure via Result CR failureReason ===")
	c := newClient(t)
	ctx := context.Background()

	runName := "e2e-agent-failure"
	resultName := fmt.Sprintf("%s-analysis-1", runName)
	failureMsg := "simulated agent error: tool execution failed"

	ar := &agenticv1alpha1.AnalysisResult{
		ObjectMeta: metav1.ObjectMeta{Name: resultName, Namespace: testNS},
		Spec:       agenticv1alpha1.AnalysisResultSpec{AgenticRunName: runName},
	}
	if err := c.Create(ctx, ar); err != nil {
		t.Fatalf("create AnalysisResult: %v", err)
	}
	t.Cleanup(func() { cleanup(t, c, ar) })

	ar.Status.FailureReason = failureMsg
	ar.Status.Conditions = []metav1.Condition{{
		Type:               "Completed",
		Status:             metav1.ConditionTrue,
		Reason:             "Failed",
		LastTransitionTime: metav1.Now(),
	}}
	if err := c.Status().Update(ctx, ar); err != nil {
		t.Fatalf("update AnalysisResult status: %v", err)
	}
	t.Logf("Created AnalysisResult %s with failureReason=%q", resultName, failureMsg)

	prop := createAgenticRunWithRequest(t, c, runName, "MOCK_AGENT_FAIL")
	t.Logf("AgenticRun created: %s/%s", testNS, prop.Name)

	t.Log("Waiting for phase: Failed (agent failure)")
	updated := waitForPhase(t, c, prop.Name, agenticv1alpha1.AgenticRunPhaseFailed)
	t.Log("Phase reached: Failed")

	assertStepCondition(t, updated.Status.Conditions, agenticv1alpha1.AgenticRunConditionAnalyzed,
		metav1.ConditionFalse, "SandboxFailed")

	if len(updated.Status.Steps.Analysis.Results) == 0 {
		t.Fatal("expected analysis result ref (agent failure), got none")
	}
	t.Logf("Verified: result ref recorded: %s", updated.Status.Steps.Analysis.Results[0].Name)

	t.Log("PASS: agent failure detected, phase=Failed, reason=SandboxFailed, result ref present")
}

// TestSandboxTimeout validates that the operator kills a sandbox pod that
// exceeds the per-step timeout and sets the SandboxTimeout condition:
//
//  1. Create AgenticRun with MOCK_TIMEOUT in the request text
//  2. Mock agent sleeps forever — never creates Result CR
//  3. Operator's timeout loop detects the pod exceeded the analysis timeout
//  4. Operator kills the pod and sets Analyzed=False/SandboxTimeout
//  5. AgenticRun transitions to Failed
func TestSandboxTimeout(t *testing.T) {
	t.Log("=== TestSandboxTimeout: validates sandbox timeout → Failed with SandboxTimeout ===")
	c := newClient(t)

	prop := createAgenticRunWithRequest(t, c, "e2e-sandbox-timeout", "MOCK_TIMEOUT")
	t.Logf("AgenticRun created: %s/%s", testNS, prop.Name)

	t.Log("Waiting for phase: Failed (sandbox timeout — this takes ~11 minutes)")
	updated := waitForPhaseWithTimeout(t, c, prop.Name, agenticv1alpha1.AgenticRunPhaseFailed, 15*time.Minute)
	t.Log("Phase reached: Failed")

	assertStepCondition(t, updated.Status.Conditions, agenticv1alpha1.AgenticRunConditionAnalyzed,
		metav1.ConditionFalse, "SandboxTimeout")

	if len(updated.Status.Steps.Analysis.Results) > 0 {
		t.Errorf("expected no analysis result refs (pod timed out), got %d", len(updated.Status.Steps.Analysis.Results))
	}

	t.Log("PASS: sandbox timeout detected, phase=Failed, reason=SandboxTimeout, no result ref")
}

// TestNoResultCR validates that the operator handles a sandbox pod that
// succeeds (exit 0) but writes a Result CR without patching its status
// (no Completed condition):
//
//  1. Create AgenticRun with MOCK_NO_STATUS
//  2. Mock agent creates Result CR without status, exits 0
//  3. Operator detects missing Completed condition → SandboxFailed
//  4. No result ref recorded (CR was not valid)
func TestNoResultCR(t *testing.T) {
	t.Log("=== TestNoResultCR: validates pod exit 0 without Completed status → Failed ===")
	c := newClient(t)

	prop := createAgenticRunWithRequest(t, c, "e2e-no-result-cr", "MOCK_NO_STATUS")
	t.Logf("AgenticRun created: %s/%s", testNS, prop.Name)

	t.Log("Waiting for phase: Failed (no valid result)")
	updated := waitForPhase(t, c, prop.Name, agenticv1alpha1.AgenticRunPhaseFailed)
	t.Log("Phase reached: Failed")

	assertStepCondition(t, updated.Status.Conditions, agenticv1alpha1.AgenticRunConditionAnalyzed,
		metav1.ConditionFalse, "SandboxFailed")

	if len(updated.Status.Steps.Analysis.Results) > 0 {
		t.Errorf("expected no analysis result refs (incomplete CR), got %d", len(updated.Status.Steps.Analysis.Results))
	}

	t.Log("PASS: incomplete result detected, phase=Failed, reason=SandboxFailed, no result ref")
}

// TestRapidDelete validates that deleting an AgenticRun immediately after
// creation cleans up orphaned sandbox resources (pods, RBAC):
//
//  1. Create AgenticRun, wait for Analyzing (sandbox pod starting)
//  2. Delete the AgenticRun immediately
//  3. Verify the AgenticRun is fully removed (finalizer processed)
//  4. Verify no orphaned sandbox pods remain
func TestRapidDelete(t *testing.T) {
	t.Log("=== TestRapidDelete: validates DELETE during analysis cleans up orphans ===")
	c := newClient(t)
	ctx := context.Background()

	prop := createAgenticRunWithRequest(t, c, "e2e-rapid-delete", "Pod crash-looping in staging namespace")
	t.Logf("AgenticRun created: %s/%s", testNS, prop.Name)

	t.Log("Waiting for phase: Analyzing (sandbox starting)")
	waitForPhase(t, c, prop.Name, agenticv1alpha1.AgenticRunPhaseAnalyzing)

	t.Log("Deleting AgenticRun immediately")
	if err := c.Delete(ctx, prop); err != nil {
		t.Fatalf("delete AgenticRun: %v", err)
	}

	t.Log("Waiting for AgenticRun to disappear (finalizer cleanup)")
	waitForDeletion(t, c, prop.Name)

	t.Log("Verifying no orphaned sandbox pods")
	var pods corev1.PodList
	if err := c.List(ctx, &pods, client.InNamespace(testNS),
		client.MatchingLabels{"agentic.openshift.io/run": "e2e-rapid-delete"}); err != nil {
		t.Fatalf("list sandbox pods: %v", err)
	}
	if len(pods.Items) > 0 {
		t.Errorf("expected no orphaned sandbox pods, found %d", len(pods.Items))
	}

	t.Log("PASS: rapid delete handled, no orphaned resources")
}

// TestPendingPodTimeout validates that the operator detects a sandbox pod
// stuck in Pending (cannot be scheduled) and times out via podStartTimeout.
// Uses a nodeSelector that matches no nodes to force Pending.
func TestPendingPodTimeout(t *testing.T) {
	t.Log("=== TestPendingPodTimeout: validates unschedulable pod → SandboxTimeout ===")
	c := newClient(t)
	ctx := context.Background()

	// Patch the sandbox-pod-spec to add an impossible nodeSelector.
	var cm corev1.ConfigMap
	cmKey := client.ObjectKey{Name: "lightspeed-agentic-configuration", Namespace: testNS}
	if err := c.Get(ctx, cmKey, &cm); err != nil {
		t.Fatalf("get config: %v", err)
	}
	originalSpec := cm.Data["sandbox-pod-spec"]

	// Inject nodeSelector at pod level (not container level).
	var podSpec map[string]interface{}
	if err := json.Unmarshal([]byte(originalSpec), &podSpec); err != nil {
		t.Fatalf("parse sandbox-pod-spec: %v", err)
	}
	podSpec["nodeSelector"] = map[string]string{"e2e-nonexistent-label": "true"}
	patchedBytes, err := json.Marshal(podSpec)
	if err != nil {
		t.Fatalf("marshal patched pod spec: %v", err)
	}
	cm.Data["sandbox-pod-spec"] = string(patchedBytes)
	if err := c.Update(ctx, &cm); err != nil {
		t.Fatalf("patch sandbox-pod-spec: %v", err)
	}
	t.Cleanup(func() {
		var restore corev1.ConfigMap
		if err := c.Get(ctx, cmKey, &restore); err == nil {
			restore.Data["sandbox-pod-spec"] = originalSpec
			_ = c.Update(ctx, &restore)
		}
	})
	t.Log("Patched sandbox-pod-spec with impossible nodeSelector")

	prop := createAgenticRunWithRequest(t, c, "e2e-pending-timeout", "Pod crash-looping in staging namespace")
	t.Logf("AgenticRun created: %s/%s", testNS, prop.Name)

	// podStartTimeout is 5 minutes, timeout check interval is 1 minute → ~6 min.
	t.Log("Waiting for phase: Failed (pod start timeout — this takes ~6 minutes)")
	updated := waitForPhaseWithTimeout(t, c, prop.Name, agenticv1alpha1.AgenticRunPhaseFailed, 8*time.Minute)
	t.Log("Phase reached: Failed")

	assertStepCondition(t, updated.Status.Conditions, agenticv1alpha1.AgenticRunConditionAnalyzed,
		metav1.ConditionFalse, "SandboxTimeout")

	t.Log("PASS: pending pod timeout detected, phase=Failed, reason=SandboxTimeout")
}

// assertStepCondition checks that a condition with the given type, status, and
// reason exists in the conditions list.
func assertStepCondition(t *testing.T, conditions []metav1.Condition, condType string, status metav1.ConditionStatus, reason string) {
	t.Helper()
	for _, cond := range conditions {
		if cond.Type == condType {
			if cond.Status != status {
				t.Errorf("%s condition status = %s, want %s", condType, cond.Status, status)
			}
			if cond.Reason != reason {
				t.Errorf("%s condition reason = %s, want %s", condType, cond.Reason, reason)
			}
			t.Logf("Verified: %s=%s reason=%s message=%q", condType, cond.Status, cond.Reason, cond.Message)
			return
		}
	}
	t.Errorf("%s condition not found", condType)
}
