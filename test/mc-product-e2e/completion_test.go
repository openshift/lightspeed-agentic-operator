//go:build mc_product_e2e

package mcproducte2e

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
)

func (f *fixture) waitForCompletion(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), phaseTimeout)
	defer cancel()
	var run agenticv1alpha1.AgenticRun
	err := wait.PollUntilContextTimeout(ctx, pollInterval, phaseTimeout, true, func(ctx context.Context) (bool, error) {
		if err := f.clusters.hub.Get(ctx, client.ObjectKeyFromObject(f.run), &run); err != nil {
			return false, err
		}
		if run.UID != f.run.UID {
			return false, errors.New("AgenticRun identity changed")
		}
		phase := agenticv1alpha1.DerivePhase(run.Status.Conditions)
		if phase == agenticv1alpha1.AgenticRunPhaseCompleted {
			return true, nil
		}
		if phase == agenticv1alpha1.AgenticRunPhaseFailed || phase == agenticv1alpha1.AgenticRunPhaseDenied || phase == agenticv1alpha1.AgenticRunPhaseEscalated {
			return false, fmt.Errorf("run reached terminal phase %s before Completed", phase)
		}
		return false, nil
	})
	if err != nil {
		t.Fatalf("wait for completed run: %v", err)
	}
	// the result CRs already exist; read them under a fresh short timeout
	// independent of the completion budget consumed above
	readCtx, readCancel := context.WithTimeout(context.Background(), resultReadTimeout)
	defer readCancel()
	for _, step := range []struct {
		condition string
		ref       string
	}{
		{agenticv1alpha1.AgenticRunConditionAnalyzed, latestSucceededResult(run.Status.Steps.Analysis.Results)},
		{agenticv1alpha1.AgenticRunConditionExecuted, latestSucceededResult(run.Status.Steps.Execution.Results)},
		{agenticv1alpha1.AgenticRunConditionVerified, latestSucceededResult(run.Status.Steps.Verification.Results)},
	} {
		cond := meta.FindStatusCondition(run.Status.Conditions, step.condition)
		if cond == nil || cond.Status != metav1.ConditionTrue || strings.EqualFold(cond.Reason, "Skipped") || step.ref == "" {
			t.Fatalf("%s did not finish with a non-skipped result", step.condition)
		}
	}

	var analysis agenticv1alpha1.AnalysisResult
	mustGet(t, readCtx, f.clusters.hub, types.NamespacedName{Namespace: hubNamespace, Name: latestSucceededResult(run.Status.Steps.Analysis.Results)}, &analysis)
	if !ownedBy(&analysis, f.run) || analysis.Spec.AgenticRunName != f.run.Name {
		t.Fatal("analysis result does not belong to this run")
	}

	var execution agenticv1alpha1.ExecutionResult
	mustGet(t, readCtx, f.clusters.hub, types.NamespacedName{Namespace: hubNamespace, Name: latestSucceededResult(run.Status.Steps.Execution.Results)}, &execution)
	if !ownedBy(&execution, f.run) || execution.Spec.AgenticRunName != f.run.Name {
		t.Fatal("execution result does not belong to this run")
	}
	// A reported action alone is insufficient; the independent spoke proof below confirms the effect.
	succeeded := false
	for _, action := range execution.Status.ActionsTaken {
		if action.Outcome == agenticv1alpha1.ActionOutcomeSucceeded {
			succeeded = true
		}
	}
	if !succeeded {
		t.Fatal("execution result contains no successful action")
	}
	var verification agenticv1alpha1.VerificationResult
	mustGet(t, readCtx, f.clusters.hub, types.NamespacedName{Namespace: hubNamespace, Name: latestSucceededResult(run.Status.Steps.Verification.Results)}, &verification)
	if !ownedBy(&verification, f.run) || verification.Spec.AgenticRunName != f.run.Name || len(verification.Status.Checks) == 0 {
		t.Fatal("verification result is missing, empty or belongs to another run")
	}
	if !verificationChecksPassed(verification.Status.Checks) {
		t.Fatal("verification contains a non-passed check")
	}
	var proof corev1.ConfigMap
	mustGet(t, readCtx, f.clusters.spoke, types.NamespacedName{Namespace: f.namespace.Name, Name: f.proofName}, &proof)
	if proof.Labels[ownedLabel] != f.proofValue || proof.Data["proof"] != f.proofValue {
		t.Fatal("independent spoke client found no matching labeled proof ConfigMap")
	}
	var hubProof corev1.ConfigMap
	if err := f.clusters.hub.Get(readCtx, types.NamespacedName{Namespace: hubNamespace, Name: f.proofName}, &hubProof); !apierrors.IsNotFound(err) {
		t.Fatalf("proof ConfigMap must not exist in the hub operator namespace (get: %v)", err)
	}
}

func verificationChecksPassed(checks []agenticv1alpha1.VerifyCheck) bool {
	if len(checks) == 0 {
		return false
	}
	for _, check := range checks {
		if check.Result != agenticv1alpha1.CheckResultPassed {
			return false
		}
	}
	return true
}

// latestSucceededResult returns the name of the newest Succeeded result ref.
// Results are appended newest last, so a retried step has its successful attempt
// at the end; returns empty when no attempt succeeded.
func latestSucceededResult(refs []agenticv1alpha1.StepResultRef) string {
	for i := len(refs) - 1; i >= 0; i-- {
		if refs[i].Outcome == agenticv1alpha1.ActionOutcomeSucceeded {
			return refs[i].Name
		}
	}
	return ""
}
