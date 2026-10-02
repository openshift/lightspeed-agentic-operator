package disconnected

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"testing"
)

func TestProbeLabelsPreservesIntersectingRequirements(t *testing.T) {
	selector := metav1.LabelSelector{MatchLabels: map[string]string{"app": "vllm"}, MatchExpressions: []metav1.LabelSelectorRequirement{{Key: "app", Operator: metav1.LabelSelectorOpIn, Values: []string{"other", "vllm"}}, {Key: "role", Operator: metav1.LabelSelectorOpExists}, {Key: "role", Operator: metav1.LabelSelectorOpNotIn, Values: []string{"probe"}}}}
	result, err := probeLabels(selector)
	if err != nil || result["app"] != "vllm" || result["role"] == "probe" {
		t.Fatalf("valid intersecting selector rejected: %v %v", result, err)
	}
}

func TestProbeLabels(t *testing.T) {
	selector := metav1.LabelSelector{MatchLabels: map[string]string{"app": "vllm"}, MatchExpressions: []metav1.LabelSelectorRequirement{{Key: "role", Operator: metav1.LabelSelectorOpIn, Values: []string{"inference"}}, {Key: "present", Operator: metav1.LabelSelectorOpExists}, {Key: "absent", Operator: metav1.LabelSelectorOpDoesNotExist}}}
	labels, err := probeLabels(selector)
	if err != nil {
		t.Fatal(err)
	}
	if labels["app"] != "vllm" || labels["role"] != "inference" || labels["present"] == "" {
		t.Fatalf("wrong labels: %v", labels)
	}
	selector.MatchExpressions = append(selector.MatchExpressions, metav1.LabelSelectorRequirement{Key: "app", Operator: metav1.LabelSelectorOpNotIn, Values: []string{"vllm"}})
	if _, err := probeLabels(selector); err == nil {
		t.Fatal("accepted impossible selector")
	}
}
