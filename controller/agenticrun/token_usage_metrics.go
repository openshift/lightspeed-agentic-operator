package agenticrun

import (
	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	tokenUsageTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "agenticrun_token_usage_total",
			Help: "Token usage reported by processed agentic workflow steps. Input and output are provider-reported totals; reasoning is a subset of output, and tool is an estimated subset of input. Token types must not be summed together.",
		},
		[]string{"namespace", "step_type", "model", "token_type"},
	)

	// tokenUsageMissingTotal counts observations of Result CRs that omitted a
	// token usage field, labelled by field name.
	tokenUsageMissingTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "agenticrun_token_usage_missing_total",
			Help: "Number of observations of Result CRs that omitted a token usage field, by field.",
		},
		[]string{"field"},
	)
)

func init() {
	metrics.Registry.MustRegister(tokenUsageTotal, tokenUsageMissingTotal)
}

func recordTokenUsage(namespace, step string, usage agenticv1alpha1.TokenUsage) {
	counts := []struct {
		tokenType string
		value     *int64
	}{
		{tokenType: "input", value: usage.InputTokens},
		{tokenType: "output", value: usage.OutputTokens},
		{tokenType: "reasoning", value: usage.ReasoningTokens},
		{tokenType: "tool", value: usage.ToolTokens},
	}
	for _, count := range counts {
		if count.value != nil {
			model := usage.Model
			if model == "" {
				model = "unknown"
			}
			tokenUsageTotal.WithLabelValues(namespace, step, model, count.tokenType).Add(float64(*count.value))
		}
	}
}

func recordMissingTokenUsage(fields []string) {
	for _, field := range fields {
		tokenUsageMissingTotal.WithLabelValues(field).Inc()
	}
}
