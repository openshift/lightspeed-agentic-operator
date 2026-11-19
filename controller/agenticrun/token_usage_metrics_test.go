package agenticrun

import (
	dto "github.com/prometheus/client_model/go"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
	"slices"
	"strings"
	"testing"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"k8s.io/utils/ptr"
)

func tokenUsageMetricValue(namespace, step, model, tokenType string) float64 {
	return testutil.ToFloat64(tokenUsageTotal.WithLabelValues(namespace, step, model, tokenType))
}

func TestRecordTokenUsageReportsEachConsumptionField(t *testing.T) {
	const (
		namespace = "metrics-all-fields"
		step      = "analysis"
		model     = "test-model"
	)
	usage := agenticv1alpha1.TokenUsage{
		InputTokens:     ptr.To[int64](100),
		OutputTokens:    ptr.To[int64](40),
		ReasoningTokens: ptr.To[int64](10),
		ToolTokens:      ptr.To[int64](25),
		PeakInputTokens: ptr.To[int64](80),
		Model:           model,
	}

	before := map[string]float64{}
	for _, tokenType := range []string{"input", "output", "reasoning", "tool"} {
		before[tokenType] = tokenUsageMetricValue(namespace, step, model, tokenType)
	}

	recordTokenUsage(namespace, step, usage)

	want := map[string]float64{
		"input":     100,
		"output":    40,
		"reasoning": 10,
		"tool":      25,
	}
	for tokenType, increment := range want {
		got := tokenUsageMetricValue(namespace, step, model, tokenType) - before[tokenType]
		if got != increment {
			t.Errorf("%s increment = %v, want %v", tokenType, got, increment)
		}
	}
}

func TestRecordTokenUsageHandlesPartialUsage(t *testing.T) {
	const (
		namespace = "metrics-partial"
		step      = "execution"
		model     = "partial-model"
	)
	t.Cleanup(func() { tokenUsageTotal.DeletePartialMatch(prometheus.Labels{"namespace": namespace}) })
	recordTokenUsage(namespace, step, agenticv1alpha1.TokenUsage{
		InputTokens:     ptr.To[int64](12),
		OutputTokens:    ptr.To[int64](0),
		PeakInputTokens: ptr.To[int64](900),
		Model:           model,
	})

	// Compare the actual series without creating any through label lookups.
	want := `
 # HELP agenticrun_token_usage_total Token usage reported by processed agentic workflow steps. Input and output are provider-reported totals; reasoning is a subset of output, and tool is an estimated subset of input. Token types must not be summed together.
 # TYPE agenticrun_token_usage_total counter
 agenticrun_token_usage_total{model="partial-model",namespace="metrics-partial",step_type="execution",token_type="input"} 12
 agenticrun_token_usage_total{model="partial-model",namespace="metrics-partial",step_type="execution",token_type="output"} 0
 `
	gather := prometheus.GathererFunc(func() ([]*dto.MetricFamily, error) {
		families, err := metrics.Registry.Gather()
		if err != nil {
			return nil, err
		}
		for _, family := range families {
			if family.GetName() == "agenticrun_token_usage_total" {
				family.Metric = slices.DeleteFunc(family.Metric, func(metric *dto.Metric) bool {
					return !slices.ContainsFunc(metric.Label, func(label *dto.LabelPair) bool {
						return label.GetName() == "namespace" && label.GetValue() == namespace
					})
				})
				return []*dto.MetricFamily{family}, nil
			}
		}
		return nil, nil
	})
	if err := testutil.GatherAndCompare(gather, strings.NewReader(want)); err != nil {
		t.Fatal(err)
	}

}

func TestRecordTokenUsageKeepsModelsSeparate(t *testing.T) {
	const (
		namespace = "metrics-models"
		step      = "verification"
		modelA    = "model-a"
		modelB    = "model-b"
	)
	beforeA := tokenUsageMetricValue(namespace, step, modelA, "input")
	beforeB := tokenUsageMetricValue(namespace, step, modelB, "input")

	recordTokenUsage(namespace, step, agenticv1alpha1.TokenUsage{InputTokens: ptr.To[int64](11), Model: modelA})
	recordTokenUsage(namespace, step, agenticv1alpha1.TokenUsage{InputTokens: ptr.To[int64](17), Model: modelB})

	if increment := tokenUsageMetricValue(namespace, step, modelA, "input") - beforeA; increment != 11 {
		t.Errorf("model A increment = %v, want 11", increment)
	}
	if increment := tokenUsageMetricValue(namespace, step, modelB, "input") - beforeB; increment != 17 {
		t.Errorf("model B increment = %v, want 17", increment)
	}
}
