package proposal

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
)

func newBarePodClient() *fake.ClientBuilder {
	s := runtime.NewScheme()
	_ = corev1.AddToScheme(s)
	return fake.NewClientBuilder().WithScheme(s)
}

func TestBarePodManager_Claim_Creates(t *testing.T) {
	fc := newBarePodClient().Build()
	builder := &PodSpecBuilder{Image: "quay.io/test/sandbox:latest"}
	m := NewBarePodManager(fc, builder, "test-ns")
	m.SetStep(
		&agenticv1alpha1.Agent{Spec: agenticv1alpha1.AgentSpec{Model: "claude-opus-4-6"}},
		testLLMProvider(agenticv1alpha1.LLMProviderAnthropic),
		nil,
	)

	name, err := m.Claim(context.Background(), "my-proposal", "analysis", "")
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if name != "ls-analysis-my-proposal" {
		t.Errorf("name = %q, want ls-analysis-my-proposal", name)
	}

	var pod corev1.Pod
	if err := fc.Get(context.Background(), types.NamespacedName{Name: name, Namespace: "test-ns"}, &pod); err != nil {
		t.Fatalf("pod not created: %v", err)
	}
	if pod.Spec.Containers[0].Image != "quay.io/test/sandbox:latest" {
		t.Errorf("container image = %q", pod.Spec.Containers[0].Image)
	}
	if pod.Labels[LabelProposal] != "my-proposal" {
		t.Errorf("proposal label = %q", pod.Labels[LabelProposal])
	}
	if pod.Labels[LabelStep] != "analysis" {
		t.Errorf("step label = %q", pod.Labels[LabelStep])
	}
}

func TestBarePodManager_Claim_AlreadyExists(t *testing.T) {
	existing := &corev1.Pod{}
	existing.Name = "ls-analysis-my-proposal"
	existing.Namespace = "test-ns"

	fc := newBarePodClient().WithObjects(existing).Build()
	builder := &PodSpecBuilder{Image: "img"}
	m := NewBarePodManager(fc, builder, "test-ns")
	m.SetStep(
		&agenticv1alpha1.Agent{Spec: agenticv1alpha1.AgentSpec{Model: "m"}},
		testLLMProvider(agenticv1alpha1.LLMProviderAnthropic),
		nil,
	)

	name, err := m.Claim(context.Background(), "my-proposal", "analysis", "")
	if err != nil {
		t.Fatalf("Claim should succeed for existing pod: %v", err)
	}
	if name != "ls-analysis-my-proposal" {
		t.Errorf("name = %q", name)
	}
}

func TestBarePodManager_Release(t *testing.T) {
	existing := &corev1.Pod{}
	existing.Name = "ls-analysis-my-proposal"
	existing.Namespace = "test-ns"

	fc := newBarePodClient().WithObjects(existing).Build()
	m := NewBarePodManager(fc, &PodSpecBuilder{Image: "img"}, "test-ns")

	if err := m.Release(context.Background(), "ls-analysis-my-proposal"); err != nil {
		t.Fatalf("Release: %v", err)
	}

	var pod corev1.Pod
	err := fc.Get(context.Background(), types.NamespacedName{Name: "ls-analysis-my-proposal", Namespace: "test-ns"}, &pod)
	if err == nil {
		t.Error("pod should be deleted")
	}
}

func TestBarePodManager_Release_NotFound(t *testing.T) {
	fc := newBarePodClient().Build()
	m := NewBarePodManager(fc, &PodSpecBuilder{Image: "img"}, "test-ns")

	if err := m.Release(context.Background(), "nonexistent"); err != nil {
		t.Fatalf("Release should not error for not-found: %v", err)
	}
}

func TestBarePodManager_WaitReady_ImmediateReady(t *testing.T) {
	pod := &corev1.Pod{}
	pod.Name = "ls-analysis-my-proposal"
	pod.Namespace = "test-ns"
	pod.Status.Conditions = []corev1.PodCondition{{
		Type:   corev1.PodReady,
		Status: corev1.ConditionTrue,
	}}
	pod.Status.PodIP = "10.0.0.5"

	fc := newBarePodClient().WithObjects(pod).Build()
	m := NewBarePodManager(fc, &PodSpecBuilder{Image: "img"}, "test-ns")

	endpoint, err := m.WaitReady(context.Background(), "ls-analysis-my-proposal", 10*time.Second)
	if err != nil {
		t.Fatalf("WaitReady: %v", err)
	}
	if endpoint != "10.0.0.5" {
		t.Errorf("endpoint = %q, want 10.0.0.5", endpoint)
	}
}
