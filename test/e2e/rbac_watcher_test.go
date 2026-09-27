//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"testing"
	"time"

	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// TestReaderBindingWatcherRefreshesCache verifies that adding a shared reader
// binding invalidates the operator's cached reader-binding discovery. A later
// run must receive a per-run binding for the newly added shared binding too.
func TestReaderBindingWatcherRefreshesCache(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()

	const sourceName = "e2e-reader-watcher-monitoring"
	source := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: sourceName},
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName,
			Kind:     "ClusterRole",
			Name:     "cluster-monitoring-view",
		},
		Subjects: []rbacv1.Subject{{
			Kind:      rbacv1.ServiceAccountKind,
			Name:      "lightspeed-agent",
			Namespace: testNS,
		}},
	}
	existing := &rbacv1.ClusterRoleBinding{}
	if err := c.Get(ctx, client.ObjectKey{Name: sourceName}, existing); err == nil {
		if !equality.Semantic.DeepEqual(existing.RoleRef, source.RoleRef) ||
			!equality.Semantic.DeepEqual(existing.Subjects, source.Subjects) {
			t.Fatalf("existing reader binding %q does not match the test binding", sourceName)
		}
		if existing.Annotations == nil {
			existing.Annotations = map[string]string{}
		}
		existing.Annotations["e2e.agentic.openshift.io/watcher-probe"] = time.Now().UTC().Format(time.RFC3339Nano)
		if err := c.Update(ctx, existing); err != nil {
			t.Fatalf("update existing reader binding: %v", err)
		}
	} else if apierrors.IsNotFound(err) {
		if err := c.Create(ctx, source); err != nil {
			t.Fatalf("create additional reader binding: %v", err)
		}
	} else {
		t.Fatalf("check existing reader binding: %v", err)
	}

	// Allow the controller's ClusterRoleBinding informer to observe the add.
	time.Sleep(2 * time.Second)
	shared := findReaderBindings(t, c)
	found := false
	for name := range shared {
		if name == sourceName {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("reader binding %q was not discovered", sourceName)
	}

	run := createAgenticRunWithSkills(
		t, c, "e2e-reader-watcher", "MOCK_TIMEOUT",
		"quay.io/openshiftanalytics/agentic-skills:latest",
		"/skills/cluster-troubleshoot/investigate-alert",
	)
	var bindings rbacv1.ClusterRoleBindingList
	if err := wait.PollUntilContextTimeout(ctx, pollInterval, pollTimeout, true, func(ctx context.Context) (bool, error) {
		bindings = rbacv1.ClusterRoleBindingList{}
		if err := c.List(ctx, &bindings, client.MatchingLabels{
			runLabel:       string(run.UID),
			componentLabel: readerRBACComponent,
		}); err != nil {
			return false, err
		}
		return len(bindings.Items) == len(shared), nil
	}); err != nil {
		t.Fatalf("waiting for per-run reader bindings: %v", err)
	}
	if len(bindings.Items) != len(shared) {
		t.Fatalf("per-run reader binding count = %d, want %d (%v)", len(bindings.Items), len(shared), fmt.Sprint(shared))
	}
}
