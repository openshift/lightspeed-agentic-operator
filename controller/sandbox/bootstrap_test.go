package sandbox

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func testScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = corev1.AddToScheme(s)
	return s
}

func testConfig() BaseSandboxConfig {
	return BaseSandboxConfig{
		Image:     "quay.io/test/agentic-sandbox:latest",
		Namespace: "openshift-lightspeed",
	}
}

func TestEnsureBaseSandboxTemplate_CreatesResources(t *testing.T) {
	fc := fake.NewClientBuilder().WithScheme(testScheme()).Build()
	cfg := testConfig()

	if err := EnsureBaseSandboxTemplate(context.Background(), fc, cfg); err != nil {
		t.Fatalf("EnsureBaseSandboxTemplate: %v", err)
	}

	var sa corev1.ServiceAccount
	if err := fc.Get(context.Background(), types.NamespacedName{
		Name: templateName, Namespace: cfg.Namespace,
	}, &sa); err != nil {
		t.Errorf("ServiceAccount not created: %v", err)
	}
	if sa.AutomountServiceAccountToken != nil && *sa.AutomountServiceAccountToken {
		t.Error("ServiceAccount should not automount token")
	}

	tmpl := &unstructured.Unstructured{}
	tmpl.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "extensions.agents.x-k8s.io", Version: "v1alpha1", Kind: "SandboxTemplate",
	})
	if err := fc.Get(context.Background(), types.NamespacedName{
		Name: templateName, Namespace: cfg.Namespace,
	}, tmpl); err != nil {
		t.Fatalf("SandboxTemplate not created: %v", err)
	}

	containers, _, _ := unstructured.NestedSlice(tmpl.Object, "spec", "podTemplate", "spec", "containers")
	if len(containers) == 0 {
		t.Fatal("SandboxTemplate has no containers")
	}
	container := containers[0].(map[string]any)
	if container["image"] != cfg.Image {
		t.Errorf("container image = %q, want %q", container["image"], cfg.Image)
	}

	saName, _, _ := unstructured.NestedString(tmpl.Object, "spec", "podTemplate", "spec", "serviceAccountName")
	if saName != templateName {
		t.Errorf("serviceAccountName = %q, want %q", saName, templateName)
	}

	volumes, _, _ := unstructured.NestedSlice(tmpl.Object, "spec", "podTemplate", "spec", "volumes")
	foundSkills := false
	for _, v := range volumes {
		vol := v.(map[string]any)
		if vol["name"] == "skills" {
			foundSkills = true
		}
	}
	if !foundSkills {
		t.Error("SandboxTemplate missing skills volume")
	}

	lbls := tmpl.GetLabels()
	if lbls["app.kubernetes.io/managed-by"] != "lightspeed-operator" {
		t.Errorf("missing managed-by label, got %v", lbls)
	}
}

func TestEnsureBaseSandboxTemplate_Idempotent(t *testing.T) {
	fc := fake.NewClientBuilder().WithScheme(testScheme()).Build()
	cfg := testConfig()

	if err := EnsureBaseSandboxTemplate(context.Background(), fc, cfg); err != nil {
		t.Fatalf("first call: %v", err)
	}
	if err := EnsureBaseSandboxTemplate(context.Background(), fc, cfg); err != nil {
		t.Fatalf("second call (idempotent): %v", err)
	}
}

func TestEnsureBaseSandboxTemplate_SkipsWhenNoImage(t *testing.T) {
	fc := fake.NewClientBuilder().WithScheme(testScheme()).Build()
	cfg := BaseSandboxConfig{Namespace: "openshift-lightspeed"}

	if err := EnsureBaseSandboxTemplate(context.Background(), fc, cfg); err != nil {
		t.Fatalf("should not error with empty image: %v", err)
	}

	tmpl := &unstructured.Unstructured{}
	tmpl.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "extensions.agents.x-k8s.io", Version: "v1alpha1", Kind: "SandboxTemplate",
	})
	err := fc.Get(context.Background(), types.NamespacedName{
		Name: templateName, Namespace: "openshift-lightspeed",
	}, tmpl)
	if err == nil {
		t.Error("SandboxTemplate should not be created when image is empty")
	}
}

func TestEnsureBaseSandboxTemplate_PartialExists(t *testing.T) {
	sa := &corev1.ServiceAccount{}
	sa.Name = templateName
	sa.Namespace = "openshift-lightspeed"

	fc := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(sa).Build()
	cfg := testConfig()

	if err := EnsureBaseSandboxTemplate(context.Background(), fc, cfg); err != nil {
		t.Fatalf("should succeed when SA exists: %v", err)
	}

	tmpl := &unstructured.Unstructured{}
	tmpl.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "extensions.agents.x-k8s.io", Version: "v1alpha1", Kind: "SandboxTemplate",
	})
	if err := fc.Get(context.Background(), types.NamespacedName{
		Name: templateName, Namespace: cfg.Namespace,
	}, tmpl); err != nil {
		t.Errorf("SandboxTemplate not created: %v", err)
	}
}
