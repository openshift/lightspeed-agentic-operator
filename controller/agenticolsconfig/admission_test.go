package agenticolsconfig

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
)

func TestAdmission_ConfigSafetyRequiresTerminalCurrentSafety(t *testing.T) {
	assetsDir := os.Getenv("KUBEBUILDER_ASSETS")
	if assetsDir == "" {
		t.Skip("set KUBEBUILDER_ASSETS to the directory containing Kubernetes 1.35+ envtest binaries to run this admission envtest")
	}

	var crd apiextensionsv1.CustomResourceDefinition
	readAdmissionTestManifest(t, filepath.Join("..", "..", "config", "crd", "bases", "agentic.openshift.io_agenticolsconfigs.yaml"), &crd)

	testEnv := &envtest.Environment{
		BinaryAssetsDirectory: assetsDir,
		CRDs:                  []*apiextensionsv1.CustomResourceDefinition{&crd},
	}
	apiConfig, err := testEnv.Start()
	if err != nil {
		t.Fatalf("start envtest: %v", err)
	}
	t.Cleanup(func() {
		if err := testEnv.Stop(); err != nil {
			t.Errorf("stop envtest: %v", err)
		}
	})

	scheme := testScheme()
	if err := admissionregistrationv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add admission API types to scheme: %v", err)
	}
	apiClient, err := client.New(apiConfig, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("create API client: %v", err)
	}
	dynamicClient, err := dynamic.NewForConfig(apiConfig)
	if err != nil {
		t.Fatalf("create dynamic client: %v", err)
	}
	configs := dynamicClient.Resource(schema.GroupVersionResource{
		Group:    "agentic.openshift.io",
		Version:  "v1alpha1",
		Resource: "agenticolsconfigs",
	})
	ctx := context.Background()

	var policy admissionregistrationv1.ValidatingAdmissionPolicy
	readAdmissionTestManifest(t, filepath.Join("..", "..", "config", "admission", "agenticolsconfig-safety-policy.yaml"), &policy)
	if err := apiClient.Create(ctx, &policy); err != nil {
		t.Fatalf("create shipped config-safety ValidatingAdmissionPolicy: %v", err)
	}
	var binding admissionregistrationv1.ValidatingAdmissionPolicyBinding
	readAdmissionTestManifest(t, filepath.Join("..", "..", "config", "admission", "agenticolsconfig-safety-binding.yaml"), &binding)
	if err := apiClient.Create(ctx, &binding); err != nil {
		t.Fatalf("create shipped config-safety ValidatingAdmissionPolicyBinding: %v", err)
	}

	config := &agenticv1alpha1.AgenticOLSConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster"},
		Spec:       agenticv1alpha1.AgenticOLSConfigSpec{Suspended: true},
	}
	if err := apiClient.Create(ctx, config); err != nil {
		t.Fatalf("create suspended config: %v", err)
	}
	setAdmissionTestStatus(t, ctx, apiClient, "Stopping")

	t.Run("Stopping with safe condition denies resume and deletion", func(t *testing.T) {
		var lastErr error
		err := wait.PollUntilContextTimeout(ctx, 100*time.Millisecond, 30*time.Second, true, func(ctx context.Context) (bool, error) {
			lastErr = dryRunResumeUpdate(ctx, configs, false)
			if isConfigSafetyDenial(lastErr) {
				return true, nil
			}
			if lastErr == nil {
				return false, nil
			}
			return false, fmt.Errorf("baseline resume dry-run failed without the config-safety denial: %w", lastErr)
		})
		if err != nil {
			t.Fatalf("waiting for baseline config-safety denial (last error: %v): %v", lastErr, err)
		}

		if err := dryRunResumeUpdate(ctx, configs, true); !isConfigSafetyDenial(err) {
			t.Fatalf("resume with suspended omitted: expected config-safety denial, got %v", err)
		}
		if err := dryRunDeleteConfig(ctx, configs); !isConfigSafetyDenial(err) {
			t.Fatalf("DELETE while stopping: expected config-safety denial, got %v", err)
		}
	})

	for _, reason := range []string{"AdminActivated", "AdminActivatedWithErrors"} {
		t.Run(reason+" with current safety allows resume and deletion", func(t *testing.T) {
			setAdmissionTestStatus(t, ctx, apiClient, reason)

			if err := dryRunResumeUpdate(ctx, configs, false); err != nil {
				t.Fatalf("resume with suspended=false: %v", err)
			}
			if err := dryRunResumeUpdate(ctx, configs, true); err != nil {
				t.Fatalf("resume with suspended omitted: %v", err)
			}
			if err := dryRunDeleteConfig(ctx, configs); err != nil {
				t.Fatalf("DELETE after safe terminal outcome: %v", err)
			}
		})
	}

}

func readAdmissionTestManifest(t *testing.T, path string, target any) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read manifest %s: %v", path, err)
	}
	if err := yaml.Unmarshal(data, target); err != nil {
		t.Fatalf("decode manifest %s: %v", path, err)
	}
}

func setAdmissionTestStatus(t *testing.T, ctx context.Context, c client.Client, suspendedReason string) {
	t.Helper()
	var config agenticv1alpha1.AgenticOLSConfig
	if err := c.Get(ctx, client.ObjectKey{Name: "cluster"}, &config); err != nil {
		t.Fatalf("get config before status update: %v", err)
	}
	transition := metav1.Now()
	config.Status.Conditions = []metav1.Condition{
		{
			Type:               agenticv1alpha1.AgenticOLSConfigConditionSuspended,
			Status:             metav1.ConditionTrue,
			Reason:             suspendedReason,
			Message:            "suspension status for admission test",
			ObservedGeneration: config.Generation,
			LastTransitionTime: transition,
		},
		{
			Type:               agenticv1alpha1.AgenticOLSConfigConditionSafeToResume,
			Status:             metav1.ConditionTrue,
			Reason:             "RunsStopped",
			Message:            "all affected runs have accepted stop intent",
			ObservedGeneration: config.Generation,
			LastTransitionTime: transition,
		},
	}
	if err := c.Status().Update(ctx, &config); err != nil {
		t.Fatalf("update config conditions: %v", err)
	}
}

func dryRunResumeUpdate(ctx context.Context, configs dynamic.ResourceInterface, omitSuspended bool) error {
	config, err := configs.Get(ctx, "cluster", metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get config for dry-run resume: %w", err)
	}
	if omitSuspended {
		unstructured.RemoveNestedField(config.Object, "spec", "suspended")
	} else if err := unstructured.SetNestedField(config.Object, false, "spec", "suspended"); err != nil {
		return fmt.Errorf("set suspended=false in dry-run request: %w", err)
	}
	_, err = configs.Update(ctx, config, metav1.UpdateOptions{DryRun: []string{metav1.DryRunAll}})
	return err
}

func dryRunDeleteConfig(ctx context.Context, configs dynamic.ResourceInterface) error {
	return configs.Delete(ctx, "cluster", metav1.DeleteOptions{DryRun: []string{metav1.DryRunAll}})
}

func isConfigSafetyDenial(err error) bool {
	return err != nil &&
		(apierrors.IsForbidden(err) || apierrors.IsInvalid(err)) &&
		strings.Contains(err.Error(), "current terminal Suspended and SafeToResume=True conditions are required")
}
