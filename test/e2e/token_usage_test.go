//go:build e2e

package e2e

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
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
	var reasoningIn, toolIn, peakIn []*int64
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
		reasoningIn = append(reasoningIn, result.usage.ReasoningTokens)
		toolIn = append(toolIn, result.usage.ToolTokens)
		peakIn = append(peakIn, result.usage.PeakInputTokens)
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

	// Optional fields follow the additive rule: the run sums what the Result CRs
	// reported, and a field no CR reported stays absent. This assertion derives the
	// expectation from the Result CRs, so it holds for both the mock lane and real
	// providers (where reasoning may legitimately be absent).
	assertOptionalAggregate(t, "reasoningTokens", updated.Status.TokenUsage.ReasoningTokens, sumReported(reasoningIn))
	assertOptionalAggregate(t, "toolTokens", updated.Status.TokenUsage.ToolTokens, sumReported(toolIn))
	assertOptionalAggregate(t, "peakInputTokens", updated.Status.TokenUsage.PeakInputTokens, maxReported(peakIn))

	if updated.Status.TokenUsage.Model != "" {
		t.Errorf("AgenticRun status.tokenUsage.model = %q, want empty (model is Result-CR-only)", updated.Status.TokenUsage.Model)
	}

	// The mock lane has deterministic values. Keep an exact assertion there so
	// a stale or incorrectly configured mock image cannot make this test pass
	// with arbitrary non-zero values. Product E2E uses real provider counts and
	// therefore only asserts the aggregate relationship above.
	if os.Getenv("E2E_PROVIDER") == "" {
		if inputTokens != 330 || outputTokens != 630 {
			t.Errorf("mock token usage total = input=%d output=%d, want input=330 output=630", inputTokens, outputTokens)
		}
		if got := updated.Status.TokenUsage; got.ReasoningTokens == nil || *got.ReasoningTokens != 63 {
			t.Errorf("mock AgenticRun reasoningTokens = %v, want 63", got.ReasoningTokens)
		}
		if got := updated.Status.TokenUsage; got.ToolTokens == nil || *got.ToolTokens != 93 {
			t.Errorf("mock AgenticRun toolTokens = %v, want 93", got.ToolTokens)
		}
		if got := updated.Status.TokenUsage; got.PeakInputTokens == nil || *got.PeakInputTokens != 120 {
			t.Errorf("mock AgenticRun peakInputTokens = %v, want max 120", got.PeakInputTokens)
		}
		// The mock reports every required field, so completeness must be recorded
		// as True/Complete. The condition is advisory: it never drives phase.
		cond := meta.FindStatusCondition(updated.Status.Conditions, agenticv1alpha1.AgenticRunConditionTokenUsageReported)
		if cond == nil {
			t.Fatalf("AgenticRun has no %s condition", agenticv1alpha1.AgenticRunConditionTokenUsageReported)
		}
		if cond.Status != metav1.ConditionTrue || cond.Reason != agenticv1alpha1.ReasonTokenUsageReportedComplete {
			t.Errorf("mock %s = %s/%s, want True/%s", agenticv1alpha1.AgenticRunConditionTokenUsageReported,
				cond.Status, cond.Reason, agenticv1alpha1.ReasonTokenUsageReportedComplete)
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

// sumReported adds the reported values: an unreported value contributes nothing,
// and the result is absent when nothing was reported at all. This mirrors the
// operator's aggregation rule.
func sumReported(values []*int64) *int64 {
	var total *int64
	for _, v := range values {
		if v == nil {
			continue
		}
		if total == nil {
			total = new(int64)
		}
		*total += *v
	}
	return total
}

// maxReported is sumReported's peak counterpart: the largest reported value, or
// absent when nothing was reported.
func maxReported(values []*int64) *int64 {
	var max *int64
	for _, v := range values {
		if v == nil {
			continue
		}
		if max == nil {
			max = new(int64)
			*max = *v
			continue
		}
		if *v > *max {
			*max = *v
		}
	}
	return max
}

// assertOptionalAggregate compares an optional run-level count against the value
// derived from the Result CRs, treating nil as "not reported".
func assertOptionalAggregate(t *testing.T, name string, got, want *int64) {
	t.Helper()
	switch {
	case want == nil && got != nil:
		t.Errorf("AgenticRun %s = %d, want absent (no Result CR reported it)", name, *got)
	case want != nil && got == nil:
		t.Errorf("AgenticRun %s absent, want %d", name, *want)
	case want != nil && got != nil && *got != *want:
		t.Errorf("AgenticRun %s = %d, want %d", name, *got, *want)
	}
}

// TestTokenUsageFlow_MissingRequiredFieldWarns covers the advisory path that the
// happy-path test cannot reach: a sandbox that under-reports. It asks the mock
// agent to omit peakInputTokens via the MOCK_OMIT_PEAK request keyword.
//
// Three contracts are checked together:
//  1. The API server accepts the Result CR with the field absent. Unit tests use
//     a fake client that ignores the CRD schema, so this is the only place the
//     relaxed tokenUsage schema is proven — if a required field came back, this
//     patch would be rejected and the run would lose its diagnosis and options.
//  2. The operator reports without failing: tokenUsageReported=False/Incomplete
//     on the run names the missing field, while logs and metrics carry detail.
//  3. The run still completes with correct totals. Every step omits peak (the
//     marker rides in the option summary into the downstream queries), so a field
//     no CR reported leaves the run-level peak absent.
func TestTokenUsageFlow_MissingRequiredFieldWarns(t *testing.T) {
	t.Log("=== TestTokenUsageFlow_MissingRequiredFieldWarns: validates incomplete token reporting is advisory ===")
	c := newClient(t)

	run := createAgenticRunWithRequest(t, c, "e2e-token-usage-peak-missing", "MOCK_OMIT_PEAK")
	t.Logf("AgenticRun created: %s/%s (MOCK_OMIT_PEAK — analysis reports no peakInputTokens)", testNS, run.Name)

	proposed := waitForPhase(t, c, run.Name, agenticv1alpha1.AgenticRunPhaseProposed)
	runUID := string(proposed.UID)

	analysis := singleAnalysisResult(t, c, runUID)
	usage := analysis.Status.TokenUsage
	if usage.PeakInputTokens != nil {
		t.Fatalf("AnalysisResult peakInputTokens = %d, want absent: the mock omits it (MOCK_OMIT_PEAK)", *usage.PeakInputTokens)
	}
	if usage.InputTokens == nil || usage.OutputTokens == nil {
		t.Fatal("AnalysisResult must still report inputTokens and outputTokens")
	}
	t.Logf("API server accepted a partial tokenUsage: input=%d output=%d peak=absent",
		*usage.InputTokens, *usage.OutputTokens)

	cond := meta.FindStatusCondition(proposed.Status.Conditions, agenticv1alpha1.AgenticRunConditionTokenUsageReported)
	if cond == nil {
		t.Fatalf("AgenticRun has no %s condition", agenticv1alpha1.AgenticRunConditionTokenUsageReported)
	}
	if cond.Status != metav1.ConditionFalse || cond.Reason != agenticv1alpha1.ReasonTokenUsageReportedIncomplete {
		t.Errorf("%s = %s/%s, want False/%s", agenticv1alpha1.AgenticRunConditionTokenUsageReported,
			cond.Status, cond.Reason, agenticv1alpha1.ReasonTokenUsageReportedIncomplete)
	}
	if !strings.Contains(cond.Message, "peakInputTokens") {
		t.Errorf("%s message = %q, want it to name peakInputTokens", agenticv1alpha1.AgenticRunConditionTokenUsageReported, cond.Message)
	}
	if phase := agenticv1alpha1.DerivePhase(proposed.Status.Conditions); phase != agenticv1alpha1.AgenticRunPhaseProposed {
		t.Fatalf("incomplete token reporting must not affect phase: got %s, want %s", phase, agenticv1alpha1.AgenticRunPhaseProposed)
	}

	t.Log("Approving execution and verification to prove the run still completes")
	approveExecution(t, c, run.Name, 0)
	waitForPhase(t, c, run.Name, agenticv1alpha1.AgenticRunPhaseVerifying)
	approveVerification(t, c, run.Name)
	updated := waitForPhase(t, c, run.Name, agenticv1alpha1.AgenticRunPhaseCompleted)

	if updated.Status.TokenUsage.InputTokens == nil || updated.Status.TokenUsage.OutputTokens == nil {
		t.Fatalf("completed run lost its token totals: %+v", updated.Status.TokenUsage)
	}
	wantInput, wantOutput := tokenTotals(t, resultUsages(t, c, runUID))
	if got := *updated.Status.TokenUsage.InputTokens; got != wantInput {
		t.Errorf("AgenticRun input total = %d, want %d", got, wantInput)
	}
	if got := *updated.Status.TokenUsage.OutputTokens; got != wantOutput {
		t.Errorf("AgenticRun output total = %d, want %d", got, wantOutput)
	}
	if updated.Status.TokenUsage.PeakInputTokens != nil {
		t.Errorf("AgenticRun peakInputTokens = %d, want absent: no step reported it",
			*updated.Status.TokenUsage.PeakInputTokens)
	}
	if updated.Status.TokenUsage.ReasoningTokens == nil || updated.Status.TokenUsage.ToolTokens == nil {
		t.Errorf("AgenticRun reasoning/tool tokens = %v/%v, want both reported (every step reports them)",
			updated.Status.TokenUsage.ReasoningTokens, updated.Status.TokenUsage.ToolTokens)
	}

	// The run condition keeps the first offense; a later complete step must not
	// clear it.
	final := meta.FindStatusCondition(updated.Status.Conditions, agenticv1alpha1.AgenticRunConditionTokenUsageReported)
	if final == nil || final.Status != metav1.ConditionFalse {
		t.Errorf("%s after completion = %v, want False (sticky)", agenticv1alpha1.AgenticRunConditionTokenUsageReported, final)
	}

	t.Log("PASS: partial reporting was accepted, recorded on the run, and did not block completion")
}

// TestTokenUsageFlow_MissingOptionalFieldIsSilent covers the other half of the
// tier split: reasoningTokens and toolTokens are optional, so a step that does
// not report them must not degrade the run condition or produce a warning-oriented
// log. Only the counter moves. The run-level values stay absent because no
// step reported them, while peakInputTokens stays present because every step does.
func TestTokenUsageFlow_MissingOptionalFieldIsSilent(t *testing.T) {
	t.Log("=== TestTokenUsageFlow_MissingOptionalFieldIsSilent: validates optional fields are silent when absent ===")
	c := newClient(t)

	run := createAgenticRunWithRequest(t, c, "e2e-token-usage-optional-missing", "MOCK_OMIT_OPTIONAL")
	t.Logf("AgenticRun created: %s/%s (MOCK_OMIT_OPTIONAL — steps report no reasoning/tool tokens)", testNS, run.Name)

	proposed := waitForPhase(t, c, run.Name, agenticv1alpha1.AgenticRunPhaseProposed)
	runUID := string(proposed.UID)

	analysis := singleAnalysisResult(t, c, runUID)
	usage := analysis.Status.TokenUsage
	if usage.ReasoningTokens != nil || usage.ToolTokens != nil {
		t.Fatalf("AnalysisResult reasoning/tool = %v/%v, want both absent (MOCK_OMIT_OPTIONAL)",
			usage.ReasoningTokens, usage.ToolTokens)
	}
	if usage.PeakInputTokens == nil {
		t.Fatal("AnalysisResult must still report peakInputTokens")
	}

	cond := meta.FindStatusCondition(proposed.Status.Conditions, agenticv1alpha1.AgenticRunConditionTokenUsageReported)
	if cond == nil {
		t.Fatalf("AgenticRun has no %s condition", agenticv1alpha1.AgenticRunConditionTokenUsageReported)
	}
	if cond.Status != metav1.ConditionTrue || cond.Reason != agenticv1alpha1.ReasonTokenUsageReportedComplete {
		t.Errorf("%s = %s/%s, want True/%s: optional fields are silent",
			agenticv1alpha1.AgenticRunConditionTokenUsageReported, cond.Status, cond.Reason,
			agenticv1alpha1.ReasonTokenUsageReportedComplete)
	}

	approveExecution(t, c, run.Name, 0)
	waitForPhase(t, c, run.Name, agenticv1alpha1.AgenticRunPhaseVerifying)
	approveVerification(t, c, run.Name)
	updated := waitForPhase(t, c, run.Name, agenticv1alpha1.AgenticRunPhaseCompleted)

	usages := resultUsages(t, c, runUID)
	wantInput, wantOutput := tokenTotals(t, usages)
	if got := updated.Status.TokenUsage; got.InputTokens == nil || *got.InputTokens != wantInput {
		t.Errorf("AgenticRun input total = %v, want %d", got.InputTokens, wantInput)
	}
	if got := updated.Status.TokenUsage; got.OutputTokens == nil || *got.OutputTokens != wantOutput {
		t.Errorf("AgenticRun output total = %v, want %d", got.OutputTokens, wantOutput)
	}
	if updated.Status.TokenUsage.ReasoningTokens != nil || updated.Status.TokenUsage.ToolTokens != nil {
		t.Errorf("AgenticRun reasoning/tool = %v/%v, want both absent (no step reported them)",
			updated.Status.TokenUsage.ReasoningTokens, updated.Status.TokenUsage.ToolTokens)
	}
	assertOptionalAggregate(t, "peakInputTokens", updated.Status.TokenUsage.PeakInputTokens, tokenPeak(usages))

	final := meta.FindStatusCondition(updated.Status.Conditions, agenticv1alpha1.AgenticRunConditionTokenUsageReported)
	if final == nil || final.Status != metav1.ConditionTrue {
		t.Errorf("%s after completion = %v, want True", agenticv1alpha1.AgenticRunConditionTokenUsageReported, final)
	}

	t.Log("PASS: absent optional fields left the run condition Complete")
}

// TestTokenUsageFlow_FailedStepIsSilent covers the succeeded-only gate. A step
// that fails may legitimately have no complete usage — the run is already being
// escalated for the failure, and an alarm about telemetry would be noise. Here
// the verification step reports a failing check AND omits peakInputTokens: the
// operator escalates without token-usage reporting for that failed step.
func TestTokenUsageFlow_FailedStepIsSilent(t *testing.T) {
	t.Log("=== TestTokenUsageFlow_FailedStepIsSilent: validates failed steps raise no token warning ===")
	c := newClient(t)

	run := createAgenticRunWithRequest(t, c, "e2e-token-usage-failed-step-silent", "MOCK_VERIFY_FAIL MOCK_OMIT_PEAK")
	t.Logf("AgenticRun created: %s/%s (verification fails and omits peakInputTokens)", testNS, run.Name)

	proposed := waitForPhase(t, c, run.Name, agenticv1alpha1.AgenticRunPhaseProposed)
	runUID := string(proposed.UID)

	// Control: the succeeded analysis step omits the same field and marks the run incomplete.
	_ = singleAnalysisResult(t, c, runUID)

	approveExecution(t, c, run.Name, 0)
	waitForPhase(t, c, run.Name, agenticv1alpha1.AgenticRunPhaseVerifying)
	approveVerification(t, c, run.Name)
	waitForPhase(t, c, run.Name, agenticv1alpha1.AgenticRunPhaseEscalated)

	verification := singleVerificationResult(t, c, runUID)
	if verification.Status.TokenUsage.PeakInputTokens != nil {
		t.Fatalf("VerificationResult peakInputTokens = %d, want absent", *verification.Status.TokenUsage.PeakInputTokens)
	}
	failed := false
	for _, check := range verification.Status.Checks {
		if check.Result == agenticv1alpha1.CheckResultFailed {
			failed = true
			break
		}
	}
	if !failed {
		t.Fatalf("VerificationResult has no failing check: %+v", verification.Status.Checks)
	}

	t.Log("PASS: VerificationResult omits peakInputTokens and records a failed check")
}

// singleAnalysisResult returns the only AnalysisResult belonging to the run.
func singleAnalysisResult(t *testing.T, c client.Client, runUID string) agenticv1alpha1.AnalysisResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var list agenticv1alpha1.AnalysisResultList
	if err := c.List(ctx, &list, client.InNamespace(testNS), client.MatchingLabels{"agentic.openshift.io/run": runUID}); err != nil {
		t.Fatalf("list AnalysisResult: %v", err)
	}
	if len(list.Items) != 1 {
		t.Fatalf("expected exactly 1 AnalysisResult, got %d", len(list.Items))
	}
	return list.Items[0]
}

// resultUsage pairs a Result CR with its reported token usage.
type resultUsage struct {
	name  string
	usage agenticv1alpha1.TokenUsage
}

// resultUsages returns the token usage of every Result CR belonging to the run.
func resultUsages(t *testing.T, c client.Client, runUID string) []resultUsage {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	label := client.MatchingLabels{"agentic.openshift.io/run": runUID}

	var analysisList agenticv1alpha1.AnalysisResultList
	if err := c.List(ctx, &analysisList, client.InNamespace(testNS), label); err != nil {
		t.Fatalf("list AnalysisResult: %v", err)
	}
	var executionList agenticv1alpha1.ExecutionResultList
	if err := c.List(ctx, &executionList, client.InNamespace(testNS), label); err != nil {
		t.Fatalf("list ExecutionResult: %v", err)
	}
	var verificationList agenticv1alpha1.VerificationResultList
	if err := c.List(ctx, &verificationList, client.InNamespace(testNS), label); err != nil {
		t.Fatalf("list VerificationResult: %v", err)
	}

	var usages []resultUsage
	for i := range analysisList.Items {
		usages = append(usages, resultUsage{"AnalysisResult", analysisList.Items[i].Status.TokenUsage})
	}
	for i := range executionList.Items {
		usages = append(usages, resultUsage{"ExecutionResult", executionList.Items[i].Status.TokenUsage})
	}
	for i := range verificationList.Items {
		usages = append(usages, resultUsage{"VerificationResult", verificationList.Items[i].Status.TokenUsage})
	}
	if len(usages) != 3 {
		t.Fatalf("expected 3 Result CRs for run %s, got %d", runUID, len(usages))
	}
	return usages
}

// tokenTotals sums inputTokens/outputTokens across the Result CRs.
func tokenTotals(t *testing.T, usages []resultUsage) (int64, int64) {
	t.Helper()
	var input, output int64
	for _, u := range usages {
		i, o := requireTokenUsage(t, u.name, u.usage)
		input += i
		output += o
	}
	return input, output
}

// tokenPeak derives the run-level peak the operator should hold.
func tokenPeak(usages []resultUsage) *int64 {
	peaks := make([]*int64, 0, len(usages))
	for _, u := range usages {
		peaks = append(peaks, u.usage.PeakInputTokens)
	}
	return maxReported(peaks)
}

// singleVerificationResult returns the only VerificationResult of the run.
func singleVerificationResult(t *testing.T, c client.Client, runUID string) agenticv1alpha1.VerificationResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var list agenticv1alpha1.VerificationResultList
	if err := c.List(ctx, &list, client.InNamespace(testNS), client.MatchingLabels{"agentic.openshift.io/run": runUID}); err != nil {
		t.Fatalf("list VerificationResult: %v", err)
	}
	if len(list.Items) != 1 {
		t.Fatalf("expected exactly 1 VerificationResult, got %d", len(list.Items))
	}
	return list.Items[0]
}
