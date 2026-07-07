package sandbox

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func testScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	utilruntime.Must(corev1.AddToScheme(s))
	return s
}

func testConfig() BootstrapConfig {
	return BootstrapConfig{
		Image:       "quay.io/test/agentic-sandbox:latest",
		Namespace:   "openshift-lightspeed",
		SandboxMode: "sandbox-claim",
	}
}

func TestEnsureBootstrapResources_CreatesResources(t *testing.T) {
	fc := fake.NewClientBuilder().WithScheme(testScheme()).Build()
	cfg := testConfig()

	if err := EnsureBootstrapResources(context.Background(), fc, cfg); err != nil {
		t.Fatalf("EnsureBootstrapResources: %v", err)
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
	foundHomeVolume := false
	foundWorkdirVolume := false
	for _, v := range volumes {
		vol := v.(map[string]any)
		if vol["name"] == "skills" {
			foundSkills = true
		}
		if vol["name"] == "home" {
			foundHomeVolume = true
			if vol["emptyDir"] == nil {
				t.Error("home volume should be emptyDir")
			}
		}
		if vol["name"] == "skills-workdir" {
			foundWorkdirVolume = true
			if vol["emptyDir"] == nil {
				t.Error("skills-workdir volume should be emptyDir")
			}
		}
	}
	if !foundSkills {
		t.Error("SandboxTemplate missing skills volume")
	}
	if !foundHomeVolume {
		t.Error("SandboxTemplate missing home volume")
	}
	if !foundWorkdirVolume {
		t.Error("SandboxTemplate missing skills-workdir volume")
	}

	mounts, _, _ := unstructured.NestedSlice(container, "volumeMounts")
	foundHomeMount := false
	foundWorkdirMount := false
	for _, m := range mounts {
		mount := m.(map[string]any)
		if mount["name"] == "home" && mount["mountPath"] == "/home/agent" {
			foundHomeMount = true
		}
		if mount["name"] == "skills-workdir" && mount["mountPath"] == "/app/skills/.agents" {
			foundWorkdirMount = true
		}
	}
	if !foundHomeMount {
		t.Error("SandboxTemplate missing home mount at /home/agent")
	}
	if !foundWorkdirMount {
		t.Error("SandboxTemplate missing skills-workdir mount at /app/skills/.agents")
	}

	lbls := tmpl.GetLabels()
	if lbls["app.kubernetes.io/managed-by"] != "lightspeed-operator" {
		t.Errorf("missing managed-by label, got %v", lbls)
	}
}

func TestEnsureBootstrapResources_Idempotent(t *testing.T) {
	fc := fake.NewClientBuilder().WithScheme(testScheme()).Build()
	cfg := testConfig()

	if err := EnsureBootstrapResources(context.Background(), fc, cfg); err != nil {
		t.Fatalf("first call: %v", err)
	}
	if err := EnsureBootstrapResources(context.Background(), fc, cfg); err != nil {
		t.Fatalf("second call (idempotent): %v", err)
	}
}

func TestEnsureBootstrapResources_SkipsWhenNoImage(t *testing.T) {
	fc := fake.NewClientBuilder().WithScheme(testScheme()).Build()
	cfg := BootstrapConfig{Namespace: "openshift-lightspeed"}

	if err := EnsureBootstrapResources(context.Background(), fc, cfg); err != nil {
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

func TestEnsureBootstrapResources_PartialExists(t *testing.T) {
	sa := &corev1.ServiceAccount{}
	sa.Name = templateName
	sa.Namespace = "openshift-lightspeed"

	fc := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(sa).Build()
	cfg := testConfig()

	if err := EnsureBootstrapResources(context.Background(), fc, cfg); err != nil {
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

func TestEnsureBootstrapResources_BarePod_SAOnly(t *testing.T) {
	fc := fake.NewClientBuilder().WithScheme(testScheme()).Build()
	cfg := BootstrapConfig{
		Image:       "quay.io/test/sandbox:latest",
		Namespace:   "openshift-lightspeed",
		SandboxMode: "bare-pod",
	}

	if err := EnsureBootstrapResources(context.Background(), fc, cfg); err != nil {
		t.Fatalf("EnsureBootstrapResources: %v", err)
	}

	var sa corev1.ServiceAccount
	if err := fc.Get(context.Background(), types.NamespacedName{
		Name: templateName, Namespace: cfg.Namespace,
	}, &sa); err != nil {
		t.Errorf("ServiceAccount not created: %v", err)
	}

	tmpl := &unstructured.Unstructured{}
	tmpl.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "extensions.agents.x-k8s.io", Version: "v1alpha1", Kind: "SandboxTemplate",
	})
	err := fc.Get(context.Background(), types.NamespacedName{
		Name: templateName, Namespace: cfg.Namespace,
	}, tmpl)
	if err == nil {
		t.Error("SandboxTemplate should not be created in bare-pod mode")
	}
}

func TestEnsureBootstrapResources_SandboxClaim_CreatesAll(t *testing.T) {
	fc := fake.NewClientBuilder().WithScheme(testScheme()).Build()
	cfg := BootstrapConfig{
		Image:       "quay.io/test/sandbox:latest",
		Namespace:   "openshift-lightspeed",
		SandboxMode: "sandbox-claim",
	}

	if err := EnsureBootstrapResources(context.Background(), fc, cfg); err != nil {
		t.Fatalf("EnsureBootstrapResources: %v", err)
	}

	var sa corev1.ServiceAccount
	if err := fc.Get(context.Background(), types.NamespacedName{
		Name: templateName, Namespace: cfg.Namespace,
	}, &sa); err != nil {
		t.Errorf("ServiceAccount not created: %v", err)
	}

	tmpl := &unstructured.Unstructured{}
	tmpl.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "extensions.agents.x-k8s.io", Version: "v1alpha1", Kind: "SandboxTemplate",
	})
	if err := fc.Get(context.Background(), types.NamespacedName{
		Name: templateName, Namespace: cfg.Namespace,
	}, tmpl); err != nil {
		t.Errorf("SandboxTemplate not created in sandbox-claim mode: %v", err)
	}
}

func TestEnsureBootstrapResources_DefaultMode_BarePod(t *testing.T) {
	fc := fake.NewClientBuilder().WithScheme(testScheme()).Build()
	cfg := BootstrapConfig{
		Image:     "quay.io/test/sandbox:latest",
		Namespace: "openshift-lightspeed",
	}

	if err := EnsureBootstrapResources(context.Background(), fc, cfg); err != nil {
		t.Fatalf("EnsureBootstrapResources: %v", err)
	}

	tmpl := &unstructured.Unstructured{}
	tmpl.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "extensions.agents.x-k8s.io", Version: "v1alpha1", Kind: "SandboxTemplate",
	})
	err := fc.Get(context.Background(), types.NamespacedName{
		Name: templateName, Namespace: cfg.Namespace,
	}, tmpl)
	if err == nil {
		t.Error("SandboxTemplate should not be created in default (bare-pod) mode")
	}
}
