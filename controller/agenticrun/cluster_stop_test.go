package agenticrun

import (
	"context"
	"errors"
	"fmt"
	"testing"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
)

func clusterStopScheme() *runtime.Scheme {
	s := testScheme()
	utilruntime.Must(rbacv1.AddToScheme(s))
	return s
}

func clusterStopRun() *agenticv1alpha1.AgenticRun {
	return &agenticv1alpha1.AgenticRun{
		ObjectMeta: metav1.ObjectMeta{Name: "run", Namespace: "default", UID: types.UID("run-uid")},
		Spec:       agenticv1alpha1.AgenticRunSpec{Request: "test"},
	}
}

func TestSweepClusterStopsActivePodsAndOrphanedRBAC(t *testing.T) {
	run := clusterStopRun()
	active := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "active", Namespace: "default", UID: types.UID("active-uid"),
			Labels: map[string]string{LabelRun: "run-uid", LabelStep: "analysis"},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	completed := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "completed", Namespace: "default", UID: types.UID("completed-uid"),
			Labels: map[string]string{LabelRun: "run-uid", LabelStep: "analysis"},
		},
		Status: corev1.PodStatus{Phase: corev1.PodSucceeded},
	}
	configMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name: inputConfigMapName("analysis", "run-uid"), Namespace: "default",
			Labels: map[string]string{LabelRun: "run-uid"},
		},
	}
	binding := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name: executionRoleName("run-uid"), Namespace: "default",
			Labels: map[string]string{LabelRun: "run-uid"},
		},
		RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "admin"},
	}

	fc := fake.NewClientBuilder().WithScheme(clusterStopScheme()).
		WithObjects(run, active, completed, configMap, binding).Build()
	m := &SandboxManager{client: fc, APIReader: fc, namespace: "default"}

	confirmed, err := m.SweepCluster(context.Background(), []agenticv1alpha1.AgenticRun{*run})
	if err != nil {
		t.Fatalf("SweepCluster() error = %v", err)
	}
	if !confirmed {
		t.Fatal("SweepCluster() confirmed=false, want true")
	}

	if err := fc.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "active"}, &corev1.Pod{}); !apierrors.IsNotFound(err) {
		t.Errorf("active Pod still exists after sweep: %v", err)
	}
	if err := fc.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: inputConfigMapName("analysis", "run-uid")}, &corev1.ConfigMap{}); err != nil {
		t.Errorf("input ConfigMap was removed by the sweep: %v", err)
	}
	if err := fc.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: executionRoleName("run-uid")}, &rbacv1.RoleBinding{}); !apierrors.IsNotFound(err) {
		t.Errorf("execution RoleBinding still exists after sweep: %v", err)
	}
	if err := fc.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "completed"}, &corev1.Pod{}); err != nil {
		t.Fatalf("completed Pod was removed: %v", err)
	}
}

func TestSweepClusterRemovesOrphanedLabeledRBAC(t *testing.T) {
	binding := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name: "orphan-binding", Namespace: "default",
			Labels: map[string]string{LabelRun: "gone-run"},
		},
		RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "admin"},
	}
	standing := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "standing-binding", Namespace: "default"},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "admin"},
	}
	clusterBinding := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "orphan-cluster-binding",
			Labels: map[string]string{LabelRun: "gone-run"},
		},
		RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "admin"},
	}
	orphanSA := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name: "orphan-sa", Namespace: "default",
			Labels: map[string]string{LabelRun: "gone-run"},
		},
	}
	standingSA := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{Name: "standing-sa", Namespace: "default"},
	}
	fc := fake.NewClientBuilder().WithScheme(clusterStopScheme()).
		WithObjects(binding, standing, clusterBinding, orphanSA, standingSA).Build()
	m := &SandboxManager{client: fc, APIReader: fc, namespace: "default"}

	confirmed, err := m.SweepCluster(context.Background(), nil)
	if err != nil {
		t.Fatalf("SweepCluster() error = %v", err)
	}
	if !confirmed {
		t.Fatal("SweepCluster() confirmed=false, want true")
	}
	if err := fc.Get(context.Background(), client.ObjectKeyFromObject(binding), &rbacv1.RoleBinding{}); !apierrors.IsNotFound(err) {
		t.Errorf("orphaned RoleBinding still exists: %v", err)
	}
	if err := fc.Get(context.Background(), client.ObjectKeyFromObject(clusterBinding), &rbacv1.ClusterRoleBinding{}); !apierrors.IsNotFound(err) {
		t.Errorf("orphaned ClusterRoleBinding still exists: %v", err)
	}
	if err := fc.Get(context.Background(), client.ObjectKeyFromObject(orphanSA), &corev1.ServiceAccount{}); !apierrors.IsNotFound(err) {
		t.Errorf("orphaned ServiceAccount still exists: %v", err)
	}
	if err := fc.Get(context.Background(), client.ObjectKeyFromObject(standingSA), &corev1.ServiceAccount{}); err != nil {
		t.Fatalf("standing ServiceAccount was removed: %v", err)
	}
	if err := fc.Get(context.Background(), client.ObjectKeyFromObject(standing), &rbacv1.RoleBinding{}); err != nil {
		t.Fatalf("standing RoleBinding was removed: %v", err)
	}
}

func TestSpokeStopClassification(t *testing.T) {
	gone := &SpokeStopError{Cluster: "spoke-a", Kind: SpokeStopGone, Err: errors.New("missing kubeconfig")}
	unreachable := &SpokeStopError{Cluster: "spoke-b", Kind: SpokeStopUnreachable, Err: errors.New("i/o timeout")}
	if !IsSpokeStopGone(gone) || !IsSpokeStopDegradeable(gone) {
		t.Fatal("gone spoke error must be degradeable immediately")
	}
	if IsSpokeStopGone(unreachable) || !IsSpokeStopDegradeable(unreachable) {
		t.Fatal("unreachable spoke error must be degradeable but not immediately")
	}
	if IsSpokeStopGone(errors.Join(gone, unreachable)) {
		t.Fatal("mixed spoke errors must not be classified as immediately gone")
	}
	if !IsSpokeStopDegradeable(errors.Join(gone, unreachable)) {
		t.Fatal("mixed spoke errors must remain degradeable")
	}
	if IsSpokeStopDegradeable(errors.New("forbidden")) {
		t.Fatal("non-spoke error must not be degradeable")
	}
	failed := &SpokeStopError{Cluster: "spoke-c", Kind: SpokeStopFailed, Err: errors.New("forbidden")}
	if IsSpokeStopDegradeable(failed) {
		t.Fatal("permission failure must not be degradeable")
	}
	wrapped := fmt.Errorf("sweep: %w", errors.Join(gone, unreachable))
	if IsSpokeStopGone(wrapped) || !IsSpokeStopDegradeable(wrapped) {
		t.Fatal("wrapped mixed spoke errors must stay degradeable, not immediately gone")
	}
	outer := &SpokeStopError{Cluster: "spoke-d", Kind: SpokeStopGone, Err: errors.Join(gone, unreachable)}
	if !IsSpokeStopGone(outer) || !IsSpokeStopDegradeable(outer) {
		t.Fatal("outer spoke error kind must take precedence over its wrapped errors")
	}
	clusters := SpokeStopClusters(outer)
	if len(clusters) != 3 || clusters[0] != "spoke-a" || clusters[1] != "spoke-b" || clusters[2] != "spoke-d" {
		t.Fatalf("SpokeStopClusters() = %v, want [spoke-a spoke-b spoke-d]", clusters)
	}
}

func TestDeleteRunArtifactsKeepsInputConfigMaps(t *testing.T) {
	run := clusterStopRun()
	run.Spec.TargetCluster = "spoke-a"
	configMap := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Name: inputConfigMapName("analysis", "run-uid"), Namespace: "default",
		Labels: map[string]string{LabelRun: "run-uid"},
	}}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name: sandboxKubeconfigSecretName("run-uid", "analysis"), Namespace: "default",
		Labels: map[string]string{LabelRun: "run-uid"},
	}}
	fc := fake.NewClientBuilder().WithScheme(clusterStopScheme()).WithObjects(configMap, secret).Build()
	m := &SandboxManager{client: fc, APIReader: fc, namespace: "default"}

	if errs := m.deleteRunArtifacts(context.Background(), fc, run); len(errs) != 0 {
		t.Fatalf("deleteRunArtifacts() errors = %v", errs)
	}
	if err := fc.Get(context.Background(), client.ObjectKeyFromObject(secret), &corev1.Secret{}); !apierrors.IsNotFound(err) {
		t.Fatalf("sandbox kubeconfig Secret still exists: %v", err)
	}
	if err := fc.Get(context.Background(), client.ObjectKeyFromObject(configMap), &corev1.ConfigMap{}); err != nil {
		t.Fatalf("input ConfigMap was removed: %v", err)
	}
	remains, errs := m.runArtifactsRemain(context.Background(), fc, run)
	if len(errs) != 0 {
		t.Fatalf("runArtifactsRemain() errors = %v", errs)
	}
	if remains {
		t.Fatal("runArtifactsRemain() = true, want false after Secret removal")
	}
}

func TestClassifySpokeAccessError(t *testing.T) {
	notFound := apierrors.NewNotFound(schema.GroupResource{Resource: "secrets"}, "spoke-kubeconfig-a")
	if got := classifySpokeAccessError(notFound); got != SpokeStopGone {
		t.Fatalf("not-found classification = %s, want Gone", got)
	}
	if got := classifySpokeAccessError(fmt.Errorf("get secret: %w", notFound)); got != SpokeStopGone {
		t.Fatalf("wrapped not-found classification = %s, want Gone", got)
	}
	invalid := fmt.Errorf("%s: %w", ErrParseSpokeKubeconfig, errors.New("bad kubeconfig"))
	if got := classifySpokeAccessError(invalid); got != SpokeStopGone {
		t.Fatalf("invalid kubeconfig classification = %s, want Gone", got)
	}
	if got := classifySpokeAccessError(context.DeadlineExceeded); got != SpokeStopUnreachable {
		t.Fatalf("deadline classification = %s, want Unreachable", got)
	}
	if got := classifySpokeAccessError(errors.New("forbidden")); got != SpokeStopFailed {
		t.Fatalf("permission classification = %s, want Failed", got)
	}
}

func TestSweepClusterReportsMissingSpokeAsDegradeable(t *testing.T) {
	run := clusterStopRun()
	run.Spec.TargetCluster = "spoke-a"
	fc := fake.NewClientBuilder().WithScheme(clusterStopScheme()).WithObjects(run).Build()
	m := &SandboxManager{client: fc, APIReader: fc, namespace: "default"}

	confirmed, err := m.SweepCluster(context.Background(), []agenticv1alpha1.AgenticRun{*run})
	if confirmed {
		t.Fatal("SweepCluster() confirmed=true, want false for missing spoke")
	}
	if err == nil {
		t.Fatal("SweepCluster() error = nil, want spoke error")
	}
	if !IsSpokeStopDegradeable(err) {
		t.Fatalf("IsSpokeStopDegradeable(%v) = false, want true", err)
	}
	clusters := SpokeStopClusters(err)
	if len(clusters) != 1 || clusters[0] != "spoke-a" {
		t.Fatalf("SpokeStopClusters() = %v, want [spoke-a]", clusters)
	}
}

func TestSweepClusterPreservesCompletedOrphanPod(t *testing.T) {
	completed := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "completed", Namespace: "default", UID: types.UID("completed-uid"),
			Labels: map[string]string{LabelRun: "gone-run"},
		},
		Status: corev1.PodStatus{Phase: corev1.PodSucceeded},
	}
	fc := fake.NewClientBuilder().WithScheme(clusterStopScheme()).WithObjects(completed).Build()
	m := &SandboxManager{client: fc, APIReader: fc, namespace: "default"}

	confirmed, err := m.SweepCluster(context.Background(), nil)
	if err != nil {
		t.Fatalf("SweepCluster() error = %v", err)
	}
	if !confirmed {
		t.Fatal("SweepCluster() confirmed=false, want true")
	}
	if err := fc.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "completed"}, &corev1.Pod{}); err != nil {
		t.Fatalf("completed orphan Pod was removed: %v", err)
	}
}
