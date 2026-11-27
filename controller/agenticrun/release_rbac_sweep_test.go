package agenticrun

import (
	"context"
	"errors"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
)

// Regression tests for OLS-3266: deleting a run must remove its reader and
// execution RBAC even when the step claimName or the rbac-namespaces
// annotation was never recorded. These objects have no ownerReference, so
// garbage collection cannot remove them.

const (
	sweepRunUID   = "uid-sweep-run"
	sweepOtherUID = "uid-other-run"
	sweepTargetNS = "target-ns"
	sweepOpNS     = "default"
)

// sweepRun returns a run with no recorded sandbox claims.
func sweepRun(uid types.UID, deleting bool) *agenticv1alpha1.AgenticRun {
	run := testSandboxAgenticRun()
	run.UID = uid
	if deleting {
		now := metav1.Now()
		run.DeletionTimestamp = &now
		run.Finalizers = []string{rbacCleanupFinalizer}
	}
	return run
}

// sweepCaller returns a SandboxAgentCaller over fc with a no-op sandbox.
func sweepCaller(fc client.Client) *SandboxAgentCaller {
	return &SandboxAgentCaller{
		Sandbox:   &mockSandboxProvider{},
		K8sClient: fc,
		APIReader: fc,
		Namespace: sweepOpNS,
	}
}

// addSweepReaderBindings creates per-run reader bindings for step, as
// SandboxManager.Create does.
func addSweepReaderBindings(t *testing.T, fc client.Client, uid, step string) {
	t.Helper()
	if err := addReaderSubject(context.Background(), fc, uid, step, "ls-"+step+"-sa", sweepOpNS); err != nil {
		t.Fatalf("addReaderSubject(%s): %v", step, err)
	}
}

// addSweepExecutionRBAC creates namespace- and cluster-scoped execution RBAC
// for run, as SandboxManager.Create does. It sets the annotation on run.
func addSweepExecutionRBAC(t *testing.T, fc client.Client, run *agenticv1alpha1.AgenticRun) {
	t.Helper()
	rbacResult := &agenticv1alpha1.RBACResult{
		NamespaceScoped: []agenticv1alpha1.RBACRule{{
			Namespace: sweepTargetNS, APIGroups: []string{"apps"}, Resources: []string{"deployments"},
			ResourceNames: []string{"broken-app"}, Verbs: []string{"patch"}, Justification: "fix image",
		}},
		ClusterScoped: []agenticv1alpha1.RBACRule{{
			APIGroups: []string{""}, Resources: []string{"nodes"}, Verbs: []string{"get"}, Justification: "read nodes",
		}},
	}
	if err := ensureExecutionRBAC(context.Background(), fc, run, rbacResult, sweepOpNS, nil); err != nil {
		t.Fatalf("ensureExecutionRBAC: %v", err)
	}
}

func listReaderBindings(t *testing.T, fc client.Client, uid string) []rbacv1.ClusterRoleBinding {
	t.Helper()
	var list rbacv1.ClusterRoleBindingList
	if err := fc.List(context.Background(), &list, client.MatchingLabels{LabelRun: uid, LabelComponent: "reader-rbac"}); err != nil {
		t.Fatalf("list reader bindings: %v", err)
	}
	return list.Items
}

func assertGone(t *testing.T, fc client.Client, obj client.Object, key types.NamespacedName) {
	t.Helper()
	if err := fc.Get(context.Background(), key, obj); !apierrors.IsNotFound(err) {
		t.Errorf("%T %s still exists (err=%v)", obj, key, err)
	}
}

func assertPresent(t *testing.T, fc client.Client, obj client.Object, key types.NamespacedName) {
	t.Helper()
	if err := fc.Get(context.Background(), key, obj); err != nil {
		t.Errorf("%T %s should still exist: %v", obj, key, err)
	}
}

// Mode 1: the analysis and verification claimNames were never recorded.
func TestReleaseSandboxes_Deletion_RemovesReaderBindingsWithoutClaim(t *testing.T) {
	resetReaderBindings()
	fc := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(readerBinding()).Build()
	addSweepReaderBindings(t, fc, sweepRunUID, "analysis")
	addSweepReaderBindings(t, fc, sweepRunUID, "verification")
	if got := listReaderBindings(t, fc, sweepRunUID); len(got) != 2 {
		t.Fatalf("setup: want 2 per-run reader bindings, got %d", len(got))
	}

	if err := sweepCaller(fc).ReleaseSandboxes(context.Background(), sweepRun(sweepRunUID, true), false); err != nil {
		t.Fatalf("ReleaseSandboxes: %v", err)
	}

	if got := listReaderBindings(t, fc, sweepRunUID); len(got) != 0 {
		names := make([]string, 0, len(got))
		for _, b := range got {
			names = append(names, b.Name)
		}
		t.Errorf("reader bindings left behind after deletion: %v", names)
	}
	assertPresent(t, fc, &rbacv1.ClusterRoleBinding{}, types.NamespacedName{Name: defaultReaderClusterRoleBinding})
}

// Mode 2: the rbac-namespaces annotation was lost before deletion.
func TestReleaseSandboxes_Deletion_RemovesExecutionRBACWithoutAnnotation(t *testing.T) {
	resetReaderBindings()
	fc := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(readerBinding()).Build()
	run := sweepRun(sweepRunUID, true)
	addSweepExecutionRBAC(t, fc, run)
	run.Annotations = nil

	if err := sweepCaller(fc).ReleaseSandboxes(context.Background(), run, false); err != nil {
		t.Fatalf("ReleaseSandboxes: %v", err)
	}

	roleKey := types.NamespacedName{Namespace: sweepTargetNS, Name: executionRoleName(sweepRunUID)}
	assertGone(t, fc, &rbacv1.Role{}, roleKey)
	assertGone(t, fc, &rbacv1.RoleBinding{}, roleKey)
	clusterKey := types.NamespacedName{Name: clusterRoleName(sweepRunUID)}
	assertGone(t, fc, &rbacv1.ClusterRole{}, clusterKey)
	assertGone(t, fc, &rbacv1.ClusterRoleBinding{}, clusterKey)
}

// The sweep must not touch shared source bindings, other runs' RBAC, or this
// run's result RBAC (GC removes result RBAC through its ownerReference).
func TestReleaseSandboxes_Deletion_SweepIsScopedToRun(t *testing.T) {
	resetReaderBindings()
	fc := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(readerBinding()).Build()

	run := sweepRun(sweepRunUID, true)
	addSweepReaderBindings(t, fc, sweepRunUID, "analysis")
	addSweepExecutionRBAC(t, fc, run)
	run.Annotations = nil

	other := sweepRun(sweepOtherUID, false)
	addSweepReaderBindings(t, fc, sweepOtherUID, "analysis")
	addSweepExecutionRBAC(t, fc, other)

	resultRole := &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{
		Name: resultRoleName(sweepRunUID, "analysis"), Namespace: sweepOpNS,
		Labels: rbacLabels(sweepRunUID, "result-rbac"),
	}}
	if err := fc.Create(context.Background(), resultRole); err != nil {
		t.Fatalf("create result Role: %v", err)
	}

	if err := sweepCaller(fc).ReleaseSandboxes(context.Background(), run, false); err != nil {
		t.Fatalf("ReleaseSandboxes: %v", err)
	}

	// This run's reader and execution RBAC is gone.
	if got := listReaderBindings(t, fc, sweepRunUID); len(got) != 0 {
		t.Errorf("this run's reader bindings left behind: %d", len(got))
	}
	assertGone(t, fc, &rbacv1.Role{}, types.NamespacedName{Namespace: sweepTargetNS, Name: executionRoleName(sweepRunUID)})

	// Everything else is untouched.
	assertPresent(t, fc, &rbacv1.ClusterRoleBinding{}, types.NamespacedName{Name: defaultReaderClusterRoleBinding})
	if got := listReaderBindings(t, fc, sweepOtherUID); len(got) != 1 {
		t.Errorf("other run's reader bindings: want 1, got %d", len(got))
	}
	otherKey := types.NamespacedName{Namespace: sweepTargetNS, Name: executionRoleName(sweepOtherUID)}
	assertPresent(t, fc, &rbacv1.Role{}, otherKey)
	assertPresent(t, fc, &rbacv1.RoleBinding{}, otherKey)
	otherClusterKey := types.NamespacedName{Name: clusterRoleName(sweepOtherUID)}
	assertPresent(t, fc, &rbacv1.ClusterRole{}, otherClusterKey)
	assertPresent(t, fc, &rbacv1.ClusterRoleBinding{}, otherClusterKey)
	assertPresent(t, fc, &rbacv1.Role{}, types.NamespacedName{Namespace: sweepOpNS, Name: resultRole.Name})
}

// Outside deletion (terminal cleanup), unrecorded RBAC is left alone.
func TestReleaseSandboxes_NotDeleting_KeepsUnrecordedRBAC(t *testing.T) {
	resetReaderBindings()
	fc := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(readerBinding()).Build()
	run := sweepRun(sweepRunUID, false)
	addSweepReaderBindings(t, fc, sweepRunUID, "analysis")
	addSweepExecutionRBAC(t, fc, run)
	run.Annotations = nil

	if err := sweepCaller(fc).ReleaseSandboxes(context.Background(), run, false); err != nil {
		t.Fatalf("ReleaseSandboxes: %v", err)
	}

	if got := listReaderBindings(t, fc, sweepRunUID); len(got) != 1 {
		t.Errorf("analysis reader bindings: want 1 (not deleting), got %d", len(got))
	}
	assertPresent(t, fc, &rbacv1.Role{}, types.NamespacedName{Namespace: sweepTargetNS, Name: executionRoleName(sweepRunUID)})
}

// A sweep failure must be returned so the deletion finalizer stays.
func TestReleaseSandboxes_Deletion_SweepErrorIsReturned(t *testing.T) {
	resetReaderBindings()
	listErr := errors.New("injected Role list failure")
	fc := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(readerBinding()).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if _, ok := list.(*rbacv1.RoleList); ok {
					return listErr
				}
				return c.List(ctx, list, opts...)
			},
		}).Build()

	err := sweepCaller(fc).ReleaseSandboxes(context.Background(), sweepRun(sweepRunUID, true), false)
	if !errors.Is(err, listErr) {
		t.Fatalf("ReleaseSandboxes error = %v, want wrapped %v", err, listErr)
	}
}
