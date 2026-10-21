//go:build e2e

package configwatch_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
	"github.com/openshift/lightspeed-agentic-operator/controller/agenticrun"
	"github.com/openshift/lightspeed-agentic-operator/pkg/configuration"
	"github.com/openshift/lightspeed-agentic-operator/pkg/configwatch"
)

// TestStandbyConfigurationDeletion exercises real informer delivery and leader
// election against a cluster; no production workflow resources are created.
func TestStandbyConfigurationDeletion(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cfg, err := config.GetConfig()
	if err != nil {
		t.Fatal(err)
	}
	s := runtime.NewScheme()
	_ = scheme.AddToScheme(s)
	c, err := client.New(cfg, client.Options{Scheme: s})
	if err != nil {
		t.Fatal(err)
	}
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "configwatch-e2e-"}}
	if err := c.Create(ctx, ns); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupCtx, stop := context.WithTimeout(context.Background(), 15*time.Second)
		defer stop()
		if err := c.Delete(cleanupCtx, ns); err != nil {
			t.Error(err)
		}
	})
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: configuration.ConfigMapName, Namespace: ns.Name}, Data: map[string]string{configuration.KeySandboxMode: "bare-pod"}}
	if err := c.Create(ctx, cm); err != nil {
		t.Fatal(err)
	}
	type replica struct {
		manager ctrl.Manager
		config  *configuration.Cache
		cancel  context.CancelFunc
		done    chan error
		started chan struct{}
	}
	replicaNumber := 0
	start := func() *replica {
		t.Helper()
		mgr, err := ctrl.NewManager(cfg, ctrl.Options{Scheme: s, Cache: cache.Options{DefaultNamespaces: map[string]cache.Config{ns.Name: {}}}, Metrics: metricsserver.Options{BindAddress: "0"}, HealthProbeBindAddress: "0", LeaderElection: true, LeaderElectionNamespace: ns.Name, LeaderElectionID: "configwatch-e2e", LeaderElectionReleaseOnCancel: true, LeaseDuration: duration(4 * time.Second), RenewDeadline: duration(3 * time.Second), RetryPeriod: duration(time.Second)})
		if err != nil {
			t.Fatal(err)
		}
		local := &configuration.Cache{}
		watcher := configwatch.New(mgr.GetClient(), ns.Name, configwatch.Registration{Name: configuration.ConfigMapName, Handler: local.OnConfigMapChange})
		replicaNumber++
		watcher.Name = fmt.Sprintf("%s-%d", ns.Name, replicaNumber)
		if err := watcher.SetupWithManager(mgr); err != nil {
			t.Fatal(err)
		}
		r := &replica{manager: mgr, config: local, done: make(chan error, 1), started: make(chan struct{})}
		if err := mgr.Add(manager.RunnableFunc(func(ctx context.Context) error { close(r.started); <-ctx.Done(); return nil })); err != nil {
			t.Fatal(err)
		}
		runCtx, stop := context.WithCancel(ctx)
		r.cancel = stop
		go func() { r.done <- mgr.Start(runCtx) }()
		t.Cleanup(func() {
			stop()
			select {
			case err := <-r.done:
				if err != nil {
					t.Error(err)
				}
			case <-time.After(15 * time.Second):
				t.Error("manager did not stop")
			}
		})
		return r
	}
	first := start()
	select {
	case <-first.started:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	standby := start()
	if !standby.manager.GetCache().WaitForCacheSync(ctx) {
		t.Fatal("standby cache did not synchronize")
	}
	select {
	case <-standby.started:
		t.Fatal("second manager unexpectedly acquired leadership")
	default:
	}
	if err := wait.PollUntilContextTimeout(ctx, 50*time.Millisecond, 15*time.Second, true, func(context.Context) (bool, error) {
		return standby.config.Available(), nil
	}); err != nil {
		t.Fatal("standby did not load the initial ConfigMap:", err)
	}
	cm.Data[configuration.KeySandboxMode] = "sandbox-claim"
	if err := c.Update(ctx, cm); err != nil {
		t.Fatal(err)
	}
	if err := wait.PollUntilContextTimeout(ctx, 50*time.Millisecond, 15*time.Second, true, func(context.Context) (bool, error) {
		v := standby.config.Get()
		return v != nil && v.Sandbox.Mode == "sandbox-claim", nil
	}); err != nil {
		t.Fatal("standby did not process configuration update:", err)
	}
	if err := c.Delete(ctx, cm); err != nil {
		t.Fatal(err)
	}
	if err := wait.PollUntilContextTimeout(ctx, 50*time.Millisecond, 15*time.Second, true, func(context.Context) (bool, error) { return standby.config.Get() == nil, nil }); err != nil {
		t.Fatal("standby retained deleted ConfigMap:", err)
	}
	first.cancel()
	select {
	case <-standby.started:
	case <-ctx.Done():
		t.Fatal("standby not promoted:", ctx.Err())
	}
	if standby.config.Available() {
		t.Fatal("promoted standby retained deleted configuration")
	}
	sandbox := agenticrun.NewSandboxManager(standby.manager.GetClient(), standby.config, ns.Name, nil)
	if _, err := sandbox.Create(ctx, &agenticv1alpha1.AgenticRun{}, "analysis", nil, nil, nil, time.Minute, nil); err == nil || !strings.Contains(err.Error(), "configuration not available") {
		t.Fatalf("sandbox creation was not blocked by missing configuration: %v", err)
	}
	pods := &corev1.PodList{}
	if err := c.List(ctx, pods, client.InNamespace(ns.Name)); err != nil {
		t.Fatal(err)
	}
	if len(pods.Items) != 0 {
		t.Fatal("sandbox Pod created without configuration")
	}
}

func duration(d time.Duration) *time.Duration { return &d }
