package agenticrun

import (
	"context"
	"errors"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
)

// TestFailStepRecordsFailureAfterSandboxCreationAdvancesResourceVersion covers
// a setup failure that happens after SandboxManager.Create patched the run.
// The handler's copy is stale at that point; recording the failure from it must
// still succeed, otherwise the step stays Unknown with no Pod to time out.
func TestFailStepRecordsFailureAfterSandboxCreationAdvancesResourceVersion(t *testing.T) {
	ctx := context.Background()
	run := testAgenticRun()
	run.UID = "run-uid"
	fc := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(run).
		WithStatusSubresource(&agenticv1alpha1.AgenticRun{}, &agenticv1alpha1.ExecutionResult{}).Build()

	// The handler's copy, as seen before SandboxManager.Create ran.
	handlerRun := run.DeepCopy()
	if handlerRun.ResourceVersion == "" {
		t.Fatal("persisted run has no resourceVersion")
	}

	// Create patches the run's RBAC metadata on a separately fetched copy and
	// thereby advances the persisted resourceVersion.
	persisted := &agenticv1alpha1.AgenticRun{}
	if err := fc.Get(ctx, client.ObjectKeyFromObject(run), persisted); err != nil {
		t.Fatalf("get persisted run: %v", err)
	}
	base := persisted.DeepCopy()
	if persisted.Annotations == nil {
		persisted.Annotations = map[string]string{}
	}
	persisted.Annotations["agents.x-k8s.io/test-rbac-patched"] = "true"
	if err := fc.Patch(ctx, persisted, client.MergeFrom(base)); err != nil {
		t.Fatalf("patch run annotation: %v", err)
	}

	r := &AgenticRunReconciler{Client: fc, APIReader: fc, Namespace: run.Namespace}
	result, err := r.failStep(ctx, handlerRun, agenticv1alpha1.AgenticRunConditionExecuted, errors.New("pods \"agent\" is forbidden: exceeded quota"))
	if err != nil {
		t.Fatalf("failStep() error = %v, want nil", err)
	}
	if !result.IsZero() {
		t.Fatalf("failStep() result = %+v, want empty", result)
	}

	updated := &agenticv1alpha1.AgenticRun{}
	if err := fc.Get(ctx, client.ObjectKeyFromObject(run), updated); err != nil {
		t.Fatalf("get updated run: %v", err)
	}
	condition := findStopLifecycleCondition(updated.Status.Conditions, agenticv1alpha1.AgenticRunConditionExecuted)
	if condition == nil {
		t.Fatal("Executed condition missing")
	}
	if condition.Status != metav1.ConditionFalse || condition.Reason != reasonFailed {
		t.Fatalf("Executed condition = %s/%s, want False/%s", condition.Status, condition.Reason, reasonFailed)
	}
	if len(updated.Status.Steps.Execution.Results) != 1 {
		t.Fatalf("execution result refs = %d, want 1", len(updated.Status.Steps.Execution.Results))
	}
	if updated.Annotations["agents.x-k8s.io/test-rbac-patched"] != "true" {
		t.Fatal("RBAC annotation recorded by sandbox creation was lost")
	}
}
