package agenticrun

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
)

func newBarePodClient() *fake.ClientBuilder {
	s := runtime.NewScheme()
	utilruntime.Must(corev1.AddToScheme(s))
	utilruntime.Must(agenticv1alpha1.AddToScheme(s))
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
		defaultSandboxSA,
	)

	name, err := m.Claim(context.Background(), "my-run", "analysis", "")
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if name != "ls-analysis-my-run" {
		t.Errorf("name = %q, want ls-analysis-my-run", name)
	}

	var pod corev1.Pod
	if err := fc.Get(context.Background(), types.NamespacedName{Name: name, Namespace: "test-ns"}, &pod); err != nil {
		t.Fatalf("pod not created: %v", err)
	}
	if pod.Spec.Containers[0].Image != "quay.io/test/sandbox:latest" {
		t.Errorf("container image = %q", pod.Spec.Containers[0].Image)
	}
	if pod.Labels[LabelRun] != "my-run" {
		t.Errorf("run label = %q", pod.Labels[LabelRun])
	}
	if pod.Labels[LabelStep] != "analysis" {
		t.Errorf("step label = %q", pod.Labels[LabelStep])
	}
}

func TestBarePodManager_Claim_UsesPerAgenticRunSA(t *testing.T) {
	fc := newBarePodClient().Build()
	builder := &PodSpecBuilder{Image: "quay.io/test/sandbox:latest"}
	m := NewBarePodManager(fc, builder, "test-ns")
	m.SetStep(
		&agenticv1alpha1.Agent{Spec: agenticv1alpha1.AgentSpec{Model: "claude-opus-4-6"}},
		testLLMProvider(agenticv1alpha1.LLMProviderAnthropic),
		nil,
		"ls-exec-default-my-run",
	)

	name, err := m.Claim(context.Background(), "my-run", "execution", "")
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}

	var pod corev1.Pod
	if err := fc.Get(context.Background(), types.NamespacedName{Name: name, Namespace: "test-ns"}, &pod); err != nil {
		t.Fatalf("pod not created: %v", err)
	}
	if pod.Spec.ServiceAccountName != "ls-exec-default-my-run" {
		t.Errorf("serviceAccountName = %q, want %q", pod.Spec.ServiceAccountName, "ls-exec-default-my-run")
	}
}

func TestBarePodManager_Claim_TruncatesLongAgenticRunNameInLabel(t *testing.T) {
	fc := newBarePodClient().Build()
	builder := &PodSpecBuilder{Image: "quay.io/test/sandbox:latest"}
	m := NewBarePodManager(fc, builder, "test-ns")
	m.SetStep(
		&agenticv1alpha1.Agent{Spec: agenticv1alpha1.AgentSpec{Model: "claude-opus-4-6"}},
		testLLMProvider(agenticv1alpha1.LLMProviderAnthropic),
		nil,
		defaultSandboxSA,
	)

	longName := strings.Repeat("a", 80)
	name, err := m.Claim(context.Background(), longName, "analysis", "")
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}

	var pod corev1.Pod
	if err := fc.Get(context.Background(), types.NamespacedName{Name: name, Namespace: "test-ns"}, &pod); err != nil {
		t.Fatalf("pod not created: %v", err)
	}
	if len(pod.Labels[LabelRun]) > 63 {
		t.Fatalf("run label length %d exceeds 63", len(pod.Labels[LabelRun]))
	}
}

func TestBarePodManager_Claim_AlreadyExists(t *testing.T) {
	existing := &corev1.Pod{}
	existing.Name = "ls-analysis-my-run"
	existing.Namespace = "test-ns"

	fc := newBarePodClient().WithObjects(existing).Build()
	builder := &PodSpecBuilder{Image: "img"}
	m := NewBarePodManager(fc, builder, "test-ns")
	m.SetStep(
		&agenticv1alpha1.Agent{Spec: agenticv1alpha1.AgentSpec{Model: "m"}},
		testLLMProvider(agenticv1alpha1.LLMProviderAnthropic),
		nil,
		defaultSandboxSA,
	)

	name, err := m.Claim(context.Background(), "my-run", "analysis", "")
	if err != nil {
		t.Fatalf("Claim should succeed for existing pod: %v", err)
	}
	if name != "ls-analysis-my-run" {
		t.Errorf("name = %q", name)
	}
}

func TestBarePodManager_Release(t *testing.T) {
	existing := &corev1.Pod{}
	existing.Name = "ls-analysis-my-run"
	existing.Namespace = "test-ns"

	fc := newBarePodClient().WithObjects(existing).Build()
	m := NewBarePodManager(fc, &PodSpecBuilder{Image: "img"}, "test-ns")

	if err := m.Release(context.Background(), "ls-analysis-my-run"); err != nil {
		t.Fatalf("Release: %v", err)
	}

	var pod corev1.Pod
	err := fc.Get(context.Background(), types.NamespacedName{Name: "ls-analysis-my-run", Namespace: "test-ns"}, &pod)
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

func TestBarePodManager_Claim_AuditEnabled_DefaultsTrue(t *testing.T) {
	fc := newBarePodClient().Build()
	builder := &PodSpecBuilder{Image: "quay.io/test/sandbox:latest"}
	m := NewBarePodManager(fc, builder, "test-ns")
	m.SetStep(
		&agenticv1alpha1.Agent{Spec: agenticv1alpha1.AgentSpec{Model: "claude-opus-4-6"}},
		testLLMProvider(agenticv1alpha1.LLMProviderAnthropic),
		nil,
		defaultSandboxSA,
	)

	name, err := m.Claim(context.Background(), "my-run", "analysis", "")
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}

	var pod corev1.Pod
	if err := fc.Get(context.Background(), types.NamespacedName{Name: name, Namespace: "test-ns"}, &pod); err != nil {
		t.Fatalf("pod not created: %v", err)
	}
	env := envToMap(pod.Spec.Containers[0].Env)
	if env["LIGHTSPEED_AUDIT_ENABLED"] != "true" {
		t.Errorf("LIGHTSPEED_AUDIT_ENABLED = %q, want true", env["LIGHTSPEED_AUDIT_ENABLED"])
	}
	if _, ok := env["OTEL_EXPORTER_OTLP_ENDPOINT"]; ok {
		t.Error("OTEL_EXPORTER_OTLP_ENDPOINT should not be set when no config CR exists")
	}
}

func TestBarePodManager_Claim_AuditAlwaysEnabled(t *testing.T) {
	fc := newBarePodClient().Build()
	builder := &PodSpecBuilder{Image: "quay.io/test/sandbox:latest"}
	m := NewBarePodManager(fc, builder, "test-ns")
	m.SetStep(
		&agenticv1alpha1.Agent{Spec: agenticv1alpha1.AgentSpec{Model: "claude-opus-4-6"}},
		testLLMProvider(agenticv1alpha1.LLMProviderAnthropic),
		nil,
		defaultSandboxSA,
	)

	name, err := m.Claim(context.Background(), "my-run", "analysis", "")
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}

	var pod corev1.Pod
	if err := fc.Get(context.Background(), types.NamespacedName{Name: name, Namespace: "test-ns"}, &pod); err != nil {
		t.Fatalf("pod not created: %v", err)
	}
	env := envToMap(pod.Spec.Containers[0].Env)
	if env["LIGHTSPEED_AUDIT_ENABLED"] != "true" {
		t.Errorf("LIGHTSPEED_AUDIT_ENABLED = %q, want true (audit is always on)", env["LIGHTSPEED_AUDIT_ENABLED"])
	}
}

func TestBarePodManager_WaitReady_ImmediateReady(t *testing.T) {
	pod := &corev1.Pod{}
	pod.Name = "ls-analysis-my-run"
	pod.Namespace = "test-ns"
	pod.Status.Conditions = []corev1.PodCondition{{
		Type:   corev1.PodReady,
		Status: corev1.ConditionTrue,
	}}
	pod.Status.PodIP = "10.0.0.5"

	fc := newBarePodClient().WithObjects(pod).Build()
	m := NewBarePodManager(fc, &PodSpecBuilder{Image: "img"}, "test-ns")

	endpoint, err := m.WaitReady(context.Background(), "ls-analysis-my-run", 10*time.Second)
	if err != nil {
		t.Fatalf("WaitReady: %v", err)
	}
	if endpoint != "10.0.0.5" {
		t.Errorf("endpoint = %q, want 10.0.0.5", endpoint)
	}
}
