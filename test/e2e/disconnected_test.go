//go:build product_e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	watchtools "k8s.io/client-go/tools/watch"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
	"github.com/openshift/lightspeed-agentic-operator/test/disconnected"
)

func TestOpenAIFixtureCustomURL(t *testing.T) {
	t.Setenv("E2E_PROVIDER", "openai")
	t.Setenv("E2E_MODEL", "google/gemma-4-31B-it")
	t.Setenv("E2E_OPENAI_URL", "http://vllm.models.svc:8000/v1")
	path := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(path, []byte("test-key"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("E2E_PROVIDER_KEY_PATH", path)
	s := runtime.NewScheme()
	_ = corev1.AddToScheme(s)
	_ = agenticv1alpha1.AddToScheme(s)
	c := fake.NewClientBuilder().WithScheme(s).Build()
	fixture := createRealProviderFixtures(t, c)
	if fixture.LLM.Spec.OpenAI.URL != os.Getenv("E2E_OPENAI_URL") {
		t.Fatal("OpenAI custom URL was not passed to fixture")
	}
	if fixture.Agent.Spec.Model != os.Getenv("E2E_MODEL") {
		t.Fatal("selected model not preserved")
	}
}

func prepareDisconnected(t *testing.T, c client.Client, scenarios []discoveredScenario) context.Context {
	t.Helper()
	cfg, err := disconnected.FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if os.Getenv("E2E_SCENARIO_TAGS") != "core" || os.Getenv("E2E_SKIP_SCENARIOS") != "" {
		t.Fatal("disconnected tests require core tags without scenario exclusions")
	}
	if os.Getenv("E2E_PROVIDER") != "openai" || os.Getenv("E2E_OPENAI_URL") != cfg.BaseURL || os.Getenv("E2E_MODEL") != cfg.Model {
		t.Fatal("disconnected provider fixture must match provisioning handoff")
	}
	var cm corev1.ConfigMap
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: testNS, Name: "lightspeed-agentic-configuration"}, &cm); err != nil {
		t.Fatal(err)
	}
	if cm.Data["sandbox-mode"] != "bare-pod" {
		t.Fatal("disconnected tests require bare-pod sandbox mode")
	}
	for _, key := range []string{"otel-collector-endpoint", "mcp-endpoint", "rhokp-endpoint"} {
		if cm.Data[key] != "" {
			t.Fatalf("%s requires an explicit policy extension and is unsupported by this variant", key)
		}
	}
	var spec corev1.PodSpec
	if err := json.Unmarshal([]byte(cm.Data["sandbox-pod-spec"]), &spec); err != nil {
		t.Fatal(err)
	}
	if len(spec.Containers) == 0 {
		t.Fatal("sandbox PodSpec has no containers")
	}
	if err := cfg.ValidatePod(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{disconnected.RunLabel: "validation"}}, Spec: spec}); err != nil {
		t.Fatal(err)
	}
	// Validate every selected scenario before any setup script or AgenticRun is created.
	for i := range scenarios {
		for j := range scenarios[i].Spec.Tools.Skills {
			skill := &scenarios[i].Spec.Tools.Skills[j]
			image, err := cfg.LocalImage(skill.Image)
			if err != nil {
				t.Fatalf("scenario %s: %v", scenarios[i].Name, err)
			}
			skill.Image = image
		}
	}
	ctx := watchDisconnectedSandboxes(t, cfg)
	cfg.Start(ctx, t, c, testNS, spec.Containers[0].Image)
	requireDisconnectedWatch(t, ctx)
	return ctx
}

// Only the test goroutine may abort the suite; Fatal in the watcher goroutine
// would leave the scenario loop running without boundary monitoring.
func requireDisconnectedWatch(t *testing.T, ctx context.Context) {
	t.Helper()
	if err := context.Cause(ctx); err != nil {
		t.Fatalf("disconnected sandbox monitoring failed; aborting scenario execution: %v", err)
	}
}

func watchDisconnectedSandboxes(t *testing.T, cfg disconnected.Config) context.Context {
	t.Helper()
	kubeconfig := os.Getenv("KUBECONFIG")
	if kubeconfig == "" {
		home, _ := os.UserHomeDir()
		kubeconfig = filepath.Join(home, ".kube/config")
	}
	restCfg, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		t.Fatal(err)
	}
	cs, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancelCause(context.Background())
	pods, err := cs.CoreV1().Pods(testNS).List(ctx, metav1.ListOptions{})
	if err != nil {
		cancel(nil)
		t.Fatal(err)
	}
	check := func(p *corev1.Pod) error {
		if !strings.HasPrefix(p.Name, "ls-") {
			return nil
		}
		disconnected.ArchivePod(t, p)
		if err := cfg.ValidatePod(p); err != nil {
			return fmt.Errorf("disconnected sandbox boundary: %w", err)
		}
		for _, status := range append(p.Status.ContainerStatuses, p.Status.InitContainerStatuses...) {
			if status.State.Waiting != nil && (status.State.Waiting.Reason == "ErrImagePull" || status.State.Waiting.Reason == "ImagePullBackOff") {
				return fmt.Errorf("disconnected sandbox image pull failed: %s/%s", p.Name, status.Name)
			}
		}
		return nil
	}
	for i := range pods.Items {
		if err := check(&pods.Items[i]); err != nil {
			cancel(nil)
			t.Fatalf("pre-existing sandbox violates disconnected boundary: %v", err)
		}
	}
	// Resume from the last resourceVersion after normal API watch timeouts;
	// an expired history (410 Gone) still fails instead of silently relisting.
	watcher, err := watchtools.NewRetryWatcherWithContext(ctx, pods.ResourceVersion, &cache.ListWatch{WatchFuncWithContext: func(ctx context.Context, options metav1.ListOptions) (watch.Interface, error) {
		return cs.CoreV1().Pods(testNS).Watch(ctx, options)
	}})
	if err != nil {
		cancel(nil)
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		err := consumeDisconnectedSandboxEvents(ctx, watcher.ResultChan(), check)
		if err != nil {
			cancel(err)
		}
		done <- err
	}()
	t.Cleanup(func() {
		cancel(nil)
		watcher.Stop()
		// Retain late failures even if teardown cancellation wins the race to
		// set the context's cause after the last scenario's context check.
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	return ctx
}

func consumeDisconnectedSandboxEvents(ctx context.Context, events <-chan watch.Event, check func(*corev1.Pod) error) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case event, ok := <-events:
			if ctx.Err() != nil {
				return nil
			}
			if !ok {
				return fmt.Errorf("sandbox boundary watch closed unexpectedly")
			}
			pod, ok := event.Object.(*corev1.Pod)
			if !ok || event.Type == watch.Error {
				return fmt.Errorf("sandbox boundary watch error: %v", event.Object)
			}
			if err := check(pod); err != nil {
				return err
			}
		}
	}
}
