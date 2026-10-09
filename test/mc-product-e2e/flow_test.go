//go:build mc_product_e2e

package mcproducte2e

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
)

const (
	pollInterval      = 2 * time.Second
	phaseTimeout      = 12 * time.Minute
	deleteTimeout     = 2 * time.Minute
	resultReadTimeout = 30 * time.Second
	ownedLabel        = "agentic.openshift.io/mc-e2e"
)

type fixture struct {
	clusters   clusters
	run        *agenticv1alpha1.AgenticRun
	namespace  *corev1.Namespace
	proofName  string
	proofValue string
	observer   *observer
}

func createFixture(t *testing.T, c clusters, o *observer) *fixture {
	t.Helper()

	id := makeID(t)
	f := &fixture{
		clusters:   c,
		namespace:  &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "mc-e2e-" + id, Labels: map[string]string{ownedLabel: id}}},
		proofName:  "mc-e2e-proof-" + id,
		proofValue: id,
	}
	f.run = &agenticv1alpha1.AgenticRun{
		ObjectMeta: metav1.ObjectMeta{
			Name: "mc-e2e-" + id, Namespace: hubNamespace, Labels: map[string]string{ownedLabel: id},
		},
		Spec: agenticv1alpha1.AgenticRunSpec{
			TargetCluster:    c.name,
			TargetNamespaces: []string{f.namespace.Name},
			Request:          f.request(),
			Analysis:         agenticv1alpha1.AgenticRunStep{Agent: "default"},
			Execution:        agenticv1alpha1.AgenticRunStep{Agent: "default"},
			Verification:     agenticv1alpha1.AgenticRunStep{Agent: "default"},
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	mustBeAbsent(t, ctx, c.spoke, f.namespace)
	mustBeAbsent(t, ctx, c.hub, f.run)
	mustBeAbsent(t, ctx, c.spoke, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: f.proofName, Namespace: f.namespace.Name}})
	mustBeAbsent(t, ctx, c.hub, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: f.proofName, Namespace: hubNamespace}})
	if err := c.spoke.Create(ctx, f.namespace); err != nil {
		t.Fatalf("create owned spoke namespace: %v", err)
	}
	// Register cleanup immediately, even if the run cannot be created.
	t.Cleanup(func() { f.cleanup(t) })
	err := wait.PollUntilContextTimeout(ctx, pollInterval, 30*time.Second, true, func(ctx context.Context) (bool, error) {
		var ns corev1.Namespace
		if err := c.spoke.Get(ctx, client.ObjectKeyFromObject(f.namespace), &ns); err != nil {
			return false, err
		}
		return ns.UID == f.namespace.UID && ns.Status.Phase == corev1.NamespaceActive, nil
	})
	if err != nil {
		t.Fatalf("wait for owned spoke namespace to become Active: %v", err)
	}
	// Start all watches before an automatically approved run starts.
	o.bind(t, f)
	if err := c.hub.Create(ctx, f.run); err != nil {
		t.Fatalf("create owned AgenticRun: %v", err)
	}
	if f.run.UID == "" || f.namespace.UID == "" {
		t.Fatal("created fixture resources must have UIDs")
	}
	o.activate()
	return f
}

func makeID(t *testing.T) string {
	t.Helper()
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		t.Fatalf("generate fixture ID: %v", err)
	}
	return hex.EncodeToString(raw[:])
}

func mustBeAbsent(t *testing.T, ctx context.Context, c client.Client, obj client.Object) {
	t.Helper()
	err := c.Get(ctx, client.ObjectKeyFromObject(obj), obj)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("expected %T %s to be absent before creating fixture (get: %v)", obj, client.ObjectKeyFromObject(obj), err)
	}
}

func (f *fixture) cleanup(t *testing.T) {
	t.Helper()
	// never delete a colliding object or strip a finalizer, leave leaks visible
	if !deleteOwned(t, f.clusters.hub, f.run, deleteTimeout) {
		t.Errorf("retaining spoke namespace %s while owned AgenticRun cleanup is unconfirmed", f.namespace.Name)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), deleteTimeout)
	defer cancel()
	if err := f.cleanupNamespace(t, ctx); err != nil {
		t.Errorf("run artifacts remain after hub deletion: %v", err)
	}
}

// cleanupNamespace always attempts owned namespace deletion after the hub run is gone
func (f *fixture) cleanupNamespace(t *testing.T, ctx context.Context) error {
	t.Helper()
	var artifactErr error
	if f.observer != nil {
		artifactErr = f.observer.artifactsReleased(ctx)
	}
	deleteOwned(t, f.clusters.spoke, f.namespace, deleteTimeout)
	return artifactErr
}

func deleteOwned(t *testing.T, c client.Client, owned client.Object, timeout time.Duration) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	obj := owned.DeepCopyObject().(client.Object)
	key := client.ObjectKeyFromObject(owned)
	if err := c.Get(ctx, key, obj); err != nil {
		if apierrors.IsNotFound(err) {
			return true
		}
		t.Errorf("get owned resource %s before cleanup: %v", key, err)
		return false
	}
	if owned.GetUID() == "" || obj.GetUID() != owned.GetUID() || obj.GetLabels()[ownedLabel] != owned.GetLabels()[ownedLabel] {
		t.Errorf("resource %s identity changed, refusing cleanup", key)
		return false
	}
	if err := c.Delete(ctx, obj, client.Preconditions{UID: ptrUID(owned.GetUID())}); err != nil && !apierrors.IsNotFound(err) {
		t.Errorf("delete owned resource %s: %v", key, err)
		return false
	}
	err := wait.PollUntilContextTimeout(ctx, pollInterval, timeout, true, func(ctx context.Context) (bool, error) {
		err := c.Get(ctx, key, obj)
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		return false, err
	})
	if err != nil {
		t.Errorf("owned resource %s still exists after cleanup deadline: %v", key, err)
		return false
	}
	return true
}

func ptrUID(uid types.UID) *types.UID { return &uid }

func ownedBy(obj client.Object, run *agenticv1alpha1.AgenticRun) bool {
	for _, ref := range obj.GetOwnerReferences() {
		if ref.UID == run.UID && ref.Name == run.Name {
			return true
		}
	}
	return false
}

func (f *fixture) request() string {
	return fmt.Sprintf("ConfigMap %s is missing from the spoke namespace %s. Investigate and create it there with data.proof=%s and label %s=%s, then verify that value. Do not change other resources.", f.proofName, f.namespace.Name, f.proofValue, ownedLabel, f.proofValue)
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
