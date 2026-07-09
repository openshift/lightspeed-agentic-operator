package proposal

import (
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

	name, err := m.Claim(t.Context(), "my-proposal", "analysis", "")
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if name != "ls-analysis-my-proposal" {
		t.Errorf("name = %q, want ls-analysis-my-proposal", name)
	}

	var pod corev1.Pod
	if err := fc.Get(t.Context(), types.NamespacedName{Name: name, Namespace: "test-ns"}, &pod); err != nil {
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

func TestBarePodManager_Claim_UsesPerProposalSA(t *testing.T) {
	fc := newBarePodClient().Build()
	builder := &PodSpecBuilder{Image: "quay.io/test/sandbox:latest"}
	m := NewBarePodManager(fc, builder, "test-ns")
	m.SetStep(
		&agenticv1alpha1.Agent{Spec: agenticv1alpha1.AgentSpec{Model: "claude-opus-4-6"}},
		testLLMProvider(agenticv1alpha1.LLMProviderAnthropic),
		nil,
		"ls-exec-default-my-proposal",
	)

	name, err := m.Claim(t.Context(), "my-proposal", "execution", "")
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}

	var pod corev1.Pod
	if err := fc.Get(t.Context(), types.NamespacedName{Name: name, Namespace: "test-ns"}, &pod); err != nil {
		t.Fatalf("pod not created: %v", err)
	}
	if pod.Spec.ServiceAccountName != "ls-exec-default-my-proposal" {
		t.Errorf("serviceAccountName = %q, want %q", pod.Spec.ServiceAccountName, "ls-exec-default-my-proposal")
	}
}

func TestBarePodManager_Claim_TruncatesLongProposalNameInLabel(t *testing.T) {
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
	name, err := m.Claim(t.Context(), longName, "analysis", "")
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}

	var pod corev1.Pod
	if err := fc.Get(t.Context(), types.NamespacedName{Name: name, Namespace: "test-ns"}, &pod); err != nil {
		t.Fatalf("pod not created: %v", err)
	}
	if len(pod.Labels[LabelProposal]) > 63 {
		t.Fatalf("proposal label length %d exceeds 63", len(pod.Labels[LabelProposal]))
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
		defaultSandboxSA,
	)

	name, err := m.Claim(t.Context(), "my-proposal", "analysis", "")
	if err != nil {
		t.Fatalf("Claim should succeed for existing pod: %v", err)
	}
	if name != "ls-analysis-my-proposal" {
		t.Errorf("name = %q", name)
	}
}

func TestBarePodManager_Claim_TerminatingPodReplacedAfterDeletion(t *testing.T) {
	now := metav1.Now()
	existing := &corev1.Pod{}
	existing.Name = "ls-analysis-my-proposal"
	existing.Namespace = "test-ns"
	existing.DeletionTimestamp = &now
	existing.Finalizers = []string{"test-finalizer"}

	fc := newBarePodClient().WithObjects(existing).Build()
	builder := &PodSpecBuilder{Image: "quay.io/test/sandbox:latest"}
	m := NewBarePodManager(fc, builder, "test-ns")
	m.DeletionTimeout = 30 * time.Second
	m.SetStep(
		&agenticv1alpha1.Agent{Spec: agenticv1alpha1.AgentSpec{Model: "m"}},
		testLLMProvider(agenticv1alpha1.LLMProviderAnthropic),
		nil,
		defaultSandboxSA,
	)

	// Remove the finalizer after a short delay so the fake client
	// allows the pod to disappear. The generous DeletionTimeout above
	// avoids flakiness on slow CI nodes.
	go func() {
		time.Sleep(1 * time.Second)
		var pod corev1.Pod
		key := types.NamespacedName{Name: "ls-analysis-my-proposal", Namespace: "test-ns"}
		if err := fc.Get(t.Context(), key, &pod); err != nil {
			return
		}
		pod.Finalizers = nil
		_ = fc.Update(t.Context(), &pod)
		_ = fc.Delete(t.Context(), &pod)
	}()

	name, err := m.Claim(t.Context(), "my-proposal", "analysis", "")
	if err != nil {
		t.Fatalf("Claim should succeed after terminating pod disappears: %v", err)
	}
	if name != "ls-analysis-my-proposal" {
		t.Errorf("name = %q, want ls-analysis-my-proposal", name)
	}

	var pod corev1.Pod
	if err := fc.Get(t.Context(), types.NamespacedName{Name: name, Namespace: "test-ns"}, &pod); err != nil {
		t.Fatalf("new pod not created: %v", err)
	}
	if pod.Spec.Containers[0].Image != "quay.io/test/sandbox:latest" {
		t.Errorf("container image = %q", pod.Spec.Containers[0].Image)
	}
}

func TestBarePodManager_Claim_TerminatingPodTimeout(t *testing.T) {
	now := metav1.Now()
	existing := &corev1.Pod{}
	existing.Name = "ls-analysis-my-proposal"
	existing.Namespace = "test-ns"
	existing.DeletionTimestamp = &now
	existing.Finalizers = []string{"test-finalizer"}

	fc := newBarePodClient().WithObjects(existing).Build()
	builder := &PodSpecBuilder{Image: "img"}
	m := NewBarePodManager(fc, builder, "test-ns")
	m.DeletionTimeout = 3 * time.Second
	m.SetStep(
		&agenticv1alpha1.Agent{Spec: agenticv1alpha1.AgentSpec{Model: "m"}},
		testLLMProvider(agenticv1alpha1.LLMProviderAnthropic),
		nil,
		defaultSandboxSA,
	)

	_, err := m.Claim(t.Context(), "my-proposal", "analysis", "")
	if err == nil {
		t.Fatal("Claim should fail when terminating pod never disappears")
	}
	if !strings.Contains(err.Error(), "timeout waiting for pod") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestBarePodManager_Release(t *testing.T) {
	existing := &corev1.Pod{}
	existing.Name = "ls-analysis-my-proposal"
	existing.Namespace = "test-ns"

	fc := newBarePodClient().WithObjects(existing).Build()
	m := NewBarePodManager(fc, &PodSpecBuilder{Image: "img"}, "test-ns")

	if err := m.Release(t.Context(), "ls-analysis-my-proposal"); err != nil {
		t.Fatalf("Release: %v", err)
	}

	var pod corev1.Pod
	err := fc.Get(t.Context(), types.NamespacedName{Name: "ls-analysis-my-proposal", Namespace: "test-ns"}, &pod)
	if err == nil {
		t.Error("pod should be deleted")
	}
}

func TestBarePodManager_Release_NotFound(t *testing.T) {
	fc := newBarePodClient().Build()
	m := NewBarePodManager(fc, &PodSpecBuilder{Image: "img"}, "test-ns")

	if err := m.Release(t.Context(), "nonexistent"); err != nil {
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

	name, err := m.Claim(t.Context(), "my-proposal", "analysis", "")
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}

	var pod corev1.Pod
	if err := fc.Get(t.Context(), types.NamespacedName{Name: name, Namespace: "test-ns"}, &pod); err != nil {
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

func TestBarePodManager_Claim_AuditWithOTELEndpoint(t *testing.T) {
	config := &agenticv1alpha1.AgenticOLSConfig{}
	config.Name = "cluster"
	config.Spec.Audit = agenticv1alpha1.AuditConfig{
		Logging: agenticv1alpha1.AuditLoggingEnabled,
		OTEL:    agenticv1alpha1.AuditOTELConfig{Endpoint: "jaeger:4317"},
	}
	fc := newBarePodClient().WithObjects(config).Build()
	builder := &PodSpecBuilder{Image: "quay.io/test/sandbox:latest"}
	m := NewBarePodManager(fc, builder, "test-ns")
	m.SetStep(
		&agenticv1alpha1.Agent{Spec: agenticv1alpha1.AgentSpec{Model: "claude-opus-4-6"}},
		testLLMProvider(agenticv1alpha1.LLMProviderAnthropic),
		nil,
		defaultSandboxSA,
	)

	name, err := m.Claim(t.Context(), "my-proposal", "analysis", "")
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}

	var pod corev1.Pod
	if err := fc.Get(t.Context(), types.NamespacedName{Name: name, Namespace: "test-ns"}, &pod); err != nil {
		t.Fatalf("pod not created: %v", err)
	}
	env := envToMap(pod.Spec.Containers[0].Env)
	if env["LIGHTSPEED_AUDIT_ENABLED"] != "true" {
		t.Errorf("LIGHTSPEED_AUDIT_ENABLED = %q, want true", env["LIGHTSPEED_AUDIT_ENABLED"])
	}
	if env["OTEL_EXPORTER_OTLP_ENDPOINT"] != "jaeger:4317" {
		t.Errorf("OTEL_EXPORTER_OTLP_ENDPOINT = %q, want jaeger:4317", env["OTEL_EXPORTER_OTLP_ENDPOINT"])
	}
}

func TestBarePodManager_Claim_AuditDisabled(t *testing.T) {
	config := &agenticv1alpha1.AgenticOLSConfig{}
	config.Name = "cluster"
	config.Spec.Audit = agenticv1alpha1.AuditConfig{
		Logging: agenticv1alpha1.AuditLoggingDisabled,
	}
	fc := newBarePodClient().WithObjects(config).Build()
	builder := &PodSpecBuilder{Image: "quay.io/test/sandbox:latest"}
	m := NewBarePodManager(fc, builder, "test-ns")
	m.SetStep(
		&agenticv1alpha1.Agent{Spec: agenticv1alpha1.AgentSpec{Model: "claude-opus-4-6"}},
		testLLMProvider(agenticv1alpha1.LLMProviderAnthropic),
		nil,
		defaultSandboxSA,
	)

	name, err := m.Claim(t.Context(), "my-proposal", "analysis", "")
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}

	var pod corev1.Pod
	if err := fc.Get(t.Context(), types.NamespacedName{Name: name, Namespace: "test-ns"}, &pod); err != nil {
		t.Fatalf("pod not created: %v", err)
	}
	env := envToMap(pod.Spec.Containers[0].Env)
	if _, ok := env["LIGHTSPEED_AUDIT_ENABLED"]; ok {
		t.Error("LIGHTSPEED_AUDIT_ENABLED should not be set when audit logging is disabled")
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

	endpoint, err := m.WaitReady(t.Context(), "ls-analysis-my-proposal", 10*time.Second)
	if err != nil {
		t.Fatalf("WaitReady: %v", err)
	}
	if endpoint != "10.0.0.5" {
		t.Errorf("endpoint = %q, want 10.0.0.5", endpoint)
	}
}
