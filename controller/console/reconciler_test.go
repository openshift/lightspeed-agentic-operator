package console

import (
	"context"
	"testing"

	consolev1 "github.com/openshift/api/console/v1"
	openshiftv1 "github.com/openshift/api/operator/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func testScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = corev1.AddToScheme(s)
	_ = appsv1.AddToScheme(s)
	_ = consolev1.AddToScheme(s)
	_ = openshiftv1.AddToScheme(s)
	return s
}

func testConfig() AgenticConsoleConfig {
	return AgenticConsoleConfig{
		Image:     "quay.io/test/agentic-console:latest",
		Namespace: "openshift-lightspeed",
	}
}

func TestEnsureAgenticConsole_CreatesAllResources(t *testing.T) {
	consoleCR := &openshiftv1.Console{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster"},
	}
	fc := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(consoleCR).Build()
	cfg := testConfig()

	if err := EnsureAgenticConsole(context.Background(), fc, cfg); err != nil {
		t.Fatalf("EnsureAgenticConsole: %v", err)
	}

	var cm corev1.ConfigMap
	if err := fc.Get(context.Background(), types.NamespacedName{Name: pluginName, Namespace: cfg.Namespace}, &cm); err != nil {
		t.Errorf("ConfigMap not created: %v", err)
	}
	if cm.Data["nginx.conf"] == "" {
		t.Error("ConfigMap missing nginx.conf")
	}

	var sa corev1.ServiceAccount
	if err := fc.Get(context.Background(), types.NamespacedName{Name: pluginName, Namespace: cfg.Namespace}, &sa); err != nil {
		t.Errorf("ServiceAccount not created: %v", err)
	}

	var svc corev1.Service
	if err := fc.Get(context.Background(), types.NamespacedName{Name: pluginName, Namespace: cfg.Namespace}, &svc); err != nil {
		t.Errorf("Service not created: %v", err)
	}
	if svc.Annotations[servingCertAnnotation] != certSecretName {
		t.Errorf("Service missing serving-cert annotation, got %q", svc.Annotations[servingCertAnnotation])
	}
	if svc.Spec.Ports[0].Port != pluginPort {
		t.Errorf("Service port = %d, want %d", svc.Spec.Ports[0].Port, pluginPort)
	}

	var dep appsv1.Deployment
	if err := fc.Get(context.Background(), types.NamespacedName{Name: pluginName, Namespace: cfg.Namespace}, &dep); err != nil {
		t.Errorf("Deployment not created: %v", err)
	}
	if dep.Spec.Template.Spec.Containers[0].Image != cfg.Image {
		t.Errorf("Deployment image = %q, want %q", dep.Spec.Template.Spec.Containers[0].Image, cfg.Image)
	}

	var plugin consolev1.ConsolePlugin
	if err := fc.Get(context.Background(), types.NamespacedName{Name: pluginName}, &plugin); err != nil {
		t.Errorf("ConsolePlugin not created: %v", err)
	}
	if plugin.Spec.Backend.Service.Port != pluginPort {
		t.Errorf("ConsolePlugin port = %d, want %d", plugin.Spec.Backend.Service.Port, pluginPort)
	}

	var console openshiftv1.Console
	if err := fc.Get(context.Background(), types.NamespacedName{Name: "cluster"}, &console); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, p := range console.Spec.Plugins {
		if p == pluginName {
			found = true
		}
	}
	if !found {
		t.Errorf("Console.spec.plugins does not contain %q: %v", pluginName, console.Spec.Plugins)
	}
}

func TestEnsureAgenticConsole_Idempotent(t *testing.T) {
	consoleCR := &openshiftv1.Console{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster"},
	}
	fc := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(consoleCR).Build()
	cfg := testConfig()

	if err := EnsureAgenticConsole(context.Background(), fc, cfg); err != nil {
		t.Fatalf("first call: %v", err)
	}
	if err := EnsureAgenticConsole(context.Background(), fc, cfg); err != nil {
		t.Fatalf("second call (idempotent): %v", err)
	}

	var console openshiftv1.Console
	if err := fc.Get(context.Background(), types.NamespacedName{Name: "cluster"}, &console); err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, p := range console.Spec.Plugins {
		if p == pluginName {
			count++
		}
	}
	if count != 1 {
		t.Errorf("plugin registered %d times, want 1", count)
	}
}

func TestEnsureAgenticConsole_UpdatesImage(t *testing.T) {
	consoleCR := &openshiftv1.Console{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster"},
	}
	fc := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(consoleCR).Build()

	cfg := testConfig()
	if err := EnsureAgenticConsole(context.Background(), fc, cfg); err != nil {
		t.Fatal(err)
	}

	cfg.Image = "quay.io/test/agentic-console:v2"
	if err := EnsureAgenticConsole(context.Background(), fc, cfg); err != nil {
		t.Fatal(err)
	}

	var dep appsv1.Deployment
	if err := fc.Get(context.Background(), types.NamespacedName{Name: pluginName, Namespace: cfg.Namespace}, &dep); err != nil {
		t.Fatal(err)
	}
	if dep.Spec.Template.Spec.Containers[0].Image != "quay.io/test/agentic-console:v2" {
		t.Errorf("image not updated: %q", dep.Spec.Template.Spec.Containers[0].Image)
	}
}

func TestEnsureAgenticConsole_SkipsWhenNoImage(t *testing.T) {
	fc := fake.NewClientBuilder().WithScheme(testScheme()).Build()
	cfg := AgenticConsoleConfig{Namespace: "openshift-lightspeed"}

	if err := EnsureAgenticConsole(context.Background(), fc, cfg); err != nil {
		t.Fatalf("should not error with empty image: %v", err)
	}
}

func TestEnsureAgenticConsole_DeploymentSecurityContext(t *testing.T) {
	consoleCR := &openshiftv1.Console{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster"},
	}
	fc := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(consoleCR).Build()

	if err := EnsureAgenticConsole(context.Background(), fc, testConfig()); err != nil {
		t.Fatal(err)
	}

	var dep appsv1.Deployment
	if err := fc.Get(context.Background(), types.NamespacedName{Name: pluginName, Namespace: "openshift-lightspeed"}, &dep); err != nil {
		t.Fatal(err)
	}

	pod := dep.Spec.Template.Spec
	if !*pod.SecurityContext.RunAsNonRoot {
		t.Error("pod should be RunAsNonRoot")
	}
	container := pod.Containers[0]
	if *container.SecurityContext.AllowPrivilegeEscalation {
		t.Error("container should not allow privilege escalation")
	}
}
