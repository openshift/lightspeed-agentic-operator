//go:build e2e

package e2e

import (
	"context"
	"os"
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
)

// TestTokenUsageFlow_AggregatesResultCRs verifies token usage across the full
// analysis -> execution -> verification workflow. The test intentionally reads
// usage from the Result CRs and compares the sum with AgenticRun.status rather
// than relying only on a hard-coded total. This also runs against each real
// provider in the product E2E pipeline.
func TestTokenUsageFlow_AggregatesResultCRs(t *testing.T) {
	t.Log("=== TestTokenUsageFlow_AggregatesResultCRs: validates Result CR token usage and AgenticRun aggregation ===")
	c := newClient(t)
	ctx := context.Background()

	run := createAgenticRun(t, c, "e2e-token-usage")
	t.Logf("AgenticRun created: %s/%s", testNS, run.Name)

	t.Log("Waiting for phase: Proposed (analysis complete)")
	proposed := waitForPhase(t, c, run.Name, agenticv1alpha1.AgenticRunPhaseProposed)
	runUID := string(proposed.UID)

	t.Log("Approving execution with option 0")
	approveExecution(t, c, run.Name, 0)

	t.Log("Waiting for phase: Verifying (execution complete)")
	waitForPhase(t, c, run.Name, agenticv1alpha1.AgenticRunPhaseVerifying)

	t.Log("Approving verification")
	approveVerification(t, c, run.Name)

	t.Log("Waiting for phase: Completed (verification complete)")
	updated := waitForPhase(t, c, run.Name, agenticv1alpha1.AgenticRunPhaseCompleted)

	listCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	var analysisList agenticv1alpha1.AnalysisResultList
	if err := c.List(listCtx, &analysisList, client.InNamespace(testNS), client.MatchingLabels{"agentic.openshift.io/run": runUID}); err != nil {
		t.Fatalf("list AnalysisResult: %v", err)
	}
	if len(analysisList.Items) != 1 {
		t.Fatalf("expected exactly 1 AnalysisResult, got %d", len(analysisList.Items))
	}

	var executionList agenticv1alpha1.ExecutionResultList
	if err := c.List(listCtx, &executionList, client.InNamespace(testNS), client.MatchingLabels{"agentic.openshift.io/run": runUID}); err != nil {
		t.Fatalf("list ExecutionResult: %v", err)
	}
	if len(executionList.Items) != 1 {
		t.Fatalf("expected exactly 1 ExecutionResult, got %d", len(executionList.Items))
	}

	var verificationList agenticv1alpha1.VerificationResultList
	if err := c.List(listCtx, &verificationList, client.InNamespace(testNS), client.MatchingLabels{"agentic.openshift.io/run": runUID}); err != nil {
		t.Fatalf("list VerificationResult: %v", err)
	}
	if len(verificationList.Items) != 1 {
		t.Fatalf("expected exactly 1 VerificationResult, got %d", len(verificationList.Items))
	}

	inputTokens := int64(0)
	outputTokens := int64(0)
	for _, result := range []struct {
		name  string
		usage agenticv1alpha1.TokenUsage
	}{
		{name: "AnalysisResult", usage: analysisList.Items[0].Status.TokenUsage},
		{name: "ExecutionResult", usage: executionList.Items[0].Status.TokenUsage},
		{name: "VerificationResult", usage: verificationList.Items[0].Status.TokenUsage},
	} {
		input, output := requireTokenUsage(t, result.name, result.usage)
		inputTokens += input
		outputTokens += output
	}

	if updated.Status.TokenUsage.InputTokens == nil {
		t.Fatal("AgenticRun status.tokenUsage.inputTokens is nil")
	}
	if updated.Status.TokenUsage.OutputTokens == nil {
		t.Fatal("AgenticRun status.tokenUsage.outputTokens is nil")
	}
	if *updated.Status.TokenUsage.InputTokens != inputTokens {
		t.Errorf("AgenticRun input token total = %d, want %d", *updated.Status.TokenUsage.InputTokens, inputTokens)
	}
	if *updated.Status.TokenUsage.OutputTokens != outputTokens {
		t.Errorf("AgenticRun output token total = %d, want %d", *updated.Status.TokenUsage.OutputTokens, outputTokens)
	}
	t.Logf("Verified: AgenticRun token usage totals input=%d output=%d", inputTokens, outputTokens)

	// The mock lane has deterministic values. Keep an exact assertion there so
	// a stale or incorrectly configured mock image cannot make this test pass
	// with arbitrary non-zero values. Product E2E uses real provider counts and
	// therefore only asserts the aggregate relationship above.
	if os.Getenv("E2E_PROVIDER") == "" {
		if inputTokens != 330 || outputTokens != 630 {
			t.Errorf("mock token usage total = input=%d output=%d, want input=330 output=630", inputTokens, outputTokens)
		}
	}

	t.Log("PASS: all Result CRs contain non-zero token usage and AgenticRun contains their exact aggregate")
}

// requireTokenUsage verifies that a Result CR reports positive input and output usage.
func requireTokenUsage(t *testing.T, resultName string, usage agenticv1alpha1.TokenUsage) (int64, int64) {
	t.Helper()
	if usage.InputTokens == nil {
		t.Fatalf("%s status.tokenUsage.inputTokens is nil", resultName)
	}
	if usage.OutputTokens == nil {
		t.Fatalf("%s status.tokenUsage.outputTokens is nil", resultName)
	}
	if *usage.InputTokens <= 0 {
		t.Errorf("%s input token count = %d, want > 0", resultName, *usage.InputTokens)
	}
	if *usage.OutputTokens <= 0 {
		t.Errorf("%s output token count = %d, want > 0", resultName, *usage.OutputTokens)
	}
	return *usage.InputTokens, *usage.OutputTokens
}
