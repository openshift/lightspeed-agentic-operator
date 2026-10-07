//go:build product_e2e

package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	watchtools "k8s.io/client-go/tools/watch"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
	"github.com/openshift/lightspeed-agentic-operator/test/disconnected"
)

func TestDisconnectedWatchFailures(t *testing.T) {
	for _, tc := range []struct {
		name     string
		event    *watch.Event
		checkErr error
		want     string
	}{
		{name: "unexpected closure", want: "closed unexpectedly"},
		{name: "unrecoverable error", event: &watch.Event{Type: watch.Error, Object: &metav1.Status{Code: 410, Reason: metav1.StatusReasonExpired}}, want: "watch error"},
		{name: "invalid sandbox", event: &watch.Event{Type: watch.Added, Object: &corev1.Pod{}}, checkErr: errors.New("invalid sandbox boundary"), want: "invalid sandbox boundary"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancelCause(context.Background())
			defer cancel(nil)
			events := make(chan watch.Event, 1)
			if tc.event != nil {
				events <- *tc.event
			}
			close(events)
			err := consumeDisconnectedSandboxEvents(ctx, events, func(*corev1.Pod) error { return tc.checkErr })
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("watch failure = %v, want %q", err, tc.want)
			}
			cancel(err)
			if context.Cause(ctx) != err {
				t.Fatalf("scenario cancellation lost watch failure: %v", context.Cause(ctx))
			}
		})
	}
}

func TestDisconnectedWatchIntentionalShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	events := make(chan watch.Event)
	close(events)
	if err := consumeDisconnectedSandboxEvents(ctx, events, func(*corev1.Pod) error {
		t.Fatal("validated a pod after shutdown")
		return nil
	}); err != nil {
		t.Fatalf("intentional shutdown must not fail: %v", err)
	}
}

func TestDisconnectedWatchReconnect(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	streams := make(chan *watch.RaceFreeFakeWatcher, 2)
	versions := make(chan string, 2)
	watcher, err := watchtools.NewRetryWatcherWithContext(ctx, "1", &cache.ListWatch{WatchFuncWithContext: func(ctx context.Context, options metav1.ListOptions) (watch.Interface, error) {
		stream := watch.NewRaceFreeFake()
		versions <- options.ResourceVersion
		streams <- stream
		return stream, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer watcher.Stop()
	checked := make(chan string, 2)
	done := make(chan error, 1)
	go func() {
		done <- consumeDisconnectedSandboxEvents(ctx, watcher.ResultChan(), func(p *corev1.Pod) error {
			checked <- p.ResourceVersion
			return nil
		})
	}()
	for _, version := range []string{"2", "3"} {
		var stream *watch.RaceFreeFakeWatcher
		select {
		case stream = <-streams:
		case <-ctx.Done():
			t.Fatal("watch did not reconnect")
		}
		stream.Add(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{ResourceVersion: version}})
		select {
		case got := <-checked:
			if got != version {
				t.Fatalf("checked version %s, want %s", got, version)
			}
		case <-ctx.Done():
			t.Fatal("watch stopped validating after reconnect")
		}
		stream.Stop()
	}
	if first, second := <-versions, <-versions; first != "1" || second != "2" {
		t.Fatalf("watch resumed from versions %s, %s", first, second)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("normal watch reconnection failed: %v", err)
	}
}

// Exercise Fatal in the test goroutine without making the regression test itself
// fail. The child must stop active phase polling, run cleanup and abort the next
// scenario instead of waiting for the scenario timeout.
func TestDisconnectedWatchFailureAbortsScenarios(t *testing.T) {
	if os.Getenv("E2E_WATCH_FAILURE_UNIT_CHILD") == "1" {
		failWatch := make(chan struct{})
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if r.URL.Query().Get("watch") != "true" {
				_ = json.NewEncoder(w).Encode(&corev1.PodList{TypeMeta: metav1.TypeMeta{Kind: "PodList", APIVersion: "v1"}, ListMeta: metav1.ListMeta{ResourceVersion: "1"}})
				return
			}
			select {
			case <-failWatch:
			case <-r.Context().Done():
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"type": "ERROR", "object": &metav1.Status{
				TypeMeta: metav1.TypeMeta{Kind: "Status", APIVersion: "v1"},
				Code:     410, Reason: metav1.StatusReasonExpired, Message: "unit watch history expired",
			}})
		}))
		defer server.Close()
		kubeconfig := filepath.Join(t.TempDir(), "kubeconfig")
		if err := clientcmd.WriteToFile(clientcmdapi.Config{
			Clusters:       map[string]*clientcmdapi.Cluster{"unit": {Server: server.URL}},
			Contexts:       map[string]*clientcmdapi.Context{"unit": {Cluster: "unit"}},
			CurrentContext: "unit",
		}, kubeconfig); err != nil {
			t.Fatal(err)
		}
		t.Setenv("KUBECONFIG", kubeconfig)
		ctx := watchDisconnectedSandboxes(t, disconnected.Config{})
		var once sync.Once
		scheme := runtime.NewScheme()
		_ = agenticv1alpha1.AddToScheme(scheme)
		c := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				once.Do(func() { close(failWatch) })
				return c.Get(ctx, key, obj, opts...)
			},
		}).Build()
		t.Cleanup(func() { t.Log("suite cleanup retained") })
		for _, name := range []string{"active", "must-not-start"} {
			requireDisconnectedWatch(t, ctx)
			t.Run(name, func(t *testing.T) {
				t.Cleanup(func() { t.Log("scenario cleanup retained") })
				waitForPhaseWithContext(ctx, t, c, "pending", agenticv1alpha1.AgenticRunPhaseCompleted, time.Hour)
				t.Fatal("phase polling continued after watch failure")
			})
		}
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestDisconnectedWatchFailureAbortsScenarios$", "-test.v")
	cmd.Env = append(os.Environ(), "E2E_WATCH_FAILURE_UNIT_CHILD=1")
	output, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("watch failure did not interrupt phase polling: %s", output)
	}
	if err == nil {
		t.Fatalf("watch failure did not fail the suite: %s", output)
	}
	text := string(output)
	for _, want := range []string{"waiting for phase Completed failed: sandbox boundary watch error", "unit watch history expired", "scenario cleanup retained", "suite cleanup retained"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in child output: %s", want, output)
		}
	}
	if strings.Contains(text, "must-not-start") || strings.Contains(text, "phase polling continued") || strings.Contains(text, "panic:") {
		t.Fatalf("continued scenarios after losing boundary monitoring: %s", output)
	}
}
