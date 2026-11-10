package agenticrun

import (
	"context"
	"os"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

func startCancellationEnvtest(t *testing.T) *rest.Config {
	t.Helper()
	assetsDir := os.Getenv("KUBEBUILDER_ASSETS")
	if assetsDir == "" {
		t.Skip("set KUBEBUILDER_ASSETS to Kubernetes 1.35+ envtest binaries")
		return nil
	}
	testEnv := &envtest.Environment{
		BinaryAssetsDirectory: assetsDir,
		CRDDirectoryPaths:     []string{"../../config/crd/bases"},
	}
	cfg, err := testEnv.Start()
	if err != nil {
		t.Fatalf("start envtest: %v", err)
	}
	t.Cleanup(func() {
		if err := testEnv.Stop(); err != nil {
			t.Errorf("stop envtest: %v", err)
		}
	})
	return cfg
}

func TestAgenticRunCancelledCEL(t *testing.T) {
	cfg := startCancellationEnvtest(t)
	dc, err := dynamic.NewForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	namespaces := dc.Resource(schema.GroupVersionResource{Version: "v1", Resource: "namespaces"})
	_, err = namespaces.Create(ctx, &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1", "kind": "Namespace",
		"metadata": map[string]interface{}{"name": "default"},
	}}, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatal(err)
	}
	runs := dc.Resource(schema.GroupVersionResource{
		Group: "agentic.openshift.io", Version: "v1alpha1", Resource: "agenticruns",
	}).Namespace("default")
	newRun := func(name string, cancelled *bool) *unstructured.Unstructured {
		spec := map[string]interface{}{
			"request": "CEL test", "analysis": map[string]interface{}{"agent": "default"},
		}
		if cancelled != nil {
			spec["cancelled"] = *cancelled
		}
		return &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "agentic.openshift.io/v1alpha1", "kind": "AgenticRun",
			"metadata": map[string]interface{}{"name": name}, "spec": spec,
		}}
	}
	created, err := runs.Create(ctx, newRun("cancel-absent", nil), metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, found, err := unstructured.NestedBool(created.Object, "spec", "cancelled"); err != nil || found {
		t.Fatalf("omitted cancelled field = found %v, err %v; want absent", found, err)
	}
	if err := unstructured.SetNestedField(created.Object, true, "spec", "cancelled"); err != nil {
		t.Fatal(err)
	}
	accepted, err := runs.Update(ctx, created, metav1.UpdateOptions{})
	if err != nil {
		t.Fatalf("absent to true: %v", err)
	}
	repeated := accepted.DeepCopy()
	if _, err := runs.Update(ctx, repeated, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("true to true: %v", err)
	}
	for _, remove := range []bool{false, true} {
		candidate := accepted.DeepCopy()
		if remove {
			unstructured.RemoveNestedField(candidate.Object, "spec", "cancelled")
		} else if err := unstructured.SetNestedField(candidate.Object, false, "spec", "cancelled"); err != nil {
			t.Fatal(err)
		}
		if _, err := runs.Update(ctx, candidate, metav1.UpdateOptions{}); !apierrors.IsInvalid(err) {
			t.Fatalf("true to false/remove (remove=%v) error = %v, want Invalid", remove, err)
		}
	}
	falseValue := false
	explicitFalse, err := runs.Create(ctx, newRun("cancel-false", &falseValue), metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	removeFalse := explicitFalse.DeepCopy()
	unstructured.RemoveNestedField(removeFalse.Object, "spec", "cancelled")
	if _, err := runs.Update(ctx, removeFalse, metav1.UpdateOptions{}); !apierrors.IsInvalid(err) {
		t.Fatalf("false to remove error = %v, want Invalid", err)
	}
	if err := unstructured.SetNestedField(explicitFalse.Object, true, "spec", "cancelled"); err != nil {
		t.Fatal(err)
	}
	if _, err := runs.Update(ctx, explicitFalse, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("false to true: %v", err)
	}
}
