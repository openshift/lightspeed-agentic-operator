package agenticrun

import (
	"context"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
)

func TestEnsureAnalysisIdentityScopesReadAccessToRunNamespace(t *testing.T) {
	run := &agenticv1alpha1.AgenticRun{
		ObjectMeta: metav1.ObjectMeta{Name: "diagnose", Namespace: "team-a", UID: types.UID("run-uid")},
		Spec:       agenticv1alpha1.AgenticRunSpec{TargetNamespaces: []string{"team-b"}},
	}
	fc := fake.NewClientBuilder().WithScheme(testScheme()).Build()

	name, err := ensureAnalysisIdentity(context.Background(), fc, run, "operator-ns")
	if err != nil {
		t.Fatalf("ensureAnalysisIdentity: %v", err)
	}
	if len(name) > 63 || !strings.HasPrefix(name, "ls-analysis-team-a-diagnose-") {
		t.Fatalf("unexpected identity name %q", name)
	}
	// A repeated reconcile is idempotent.
	if _, err := ensureAnalysisIdentity(context.Background(), fc, run, "operator-ns"); err != nil {
		t.Fatalf("second ensureAnalysisIdentity: %v", err)
	}

	var sa corev1.ServiceAccount
	if err := fc.Get(context.Background(), client.ObjectKey{Name: name, Namespace: "operator-ns"}, &sa); err != nil {
		t.Fatalf("get ServiceAccount: %v", err)
	}
	if len(sa.OwnerReferences) != 0 {
		t.Fatal("cross-namespace ServiceAccount must not have an owner reference")
	}
	if sa.AutomountServiceAccountToken == nil || *sa.AutomountServiceAccountToken {
		t.Fatal("ServiceAccount should disable implicit token mounts")
	}

	var viewBinding rbacv1.RoleBinding
	if err := fc.Get(context.Background(), client.ObjectKey{Name: name + "-view", Namespace: "team-a"}, &viewBinding); err != nil {
		t.Fatalf("get view RoleBinding: %v", err)
	}
	if viewBinding.RoleRef.Kind != "ClusterRole" || viewBinding.RoleRef.Name != analysisViewClusterRole {
		t.Fatalf("unexpected view roleRef: %#v", viewBinding.RoleRef)
	}
	if len(viewBinding.Subjects) != 1 || viewBinding.Subjects[0].Name != name || viewBinding.Subjects[0].Namespace != "operator-ns" {
		t.Fatalf("unexpected view subjects: %#v", viewBinding.Subjects)
	}

	var logRole rbacv1.Role
	if err := fc.Get(context.Background(), client.ObjectKey{Name: name, Namespace: "team-a"}, &logRole); err != nil {
		t.Fatalf("get log Role: %v", err)
	}
	if len(logRole.Rules) != 1 || len(logRole.Rules[0].Resources) != 1 || logRole.Rules[0].Resources[0] != "pods/log" {
		t.Fatalf("supplemental analysis permissions must contain only pods/log: %#v", logRole.Rules)
	}

	var otherBindings rbacv1.RoleBindingList
	if err := fc.List(context.Background(), &otherBindings, client.InNamespace("team-b")); err != nil {
		t.Fatalf("list other namespace bindings: %v", err)
	}
	if len(otherBindings.Items) != 0 {
		t.Fatalf("targetNamespaces must not widen analysis access: %#v", otherBindings.Items)
	}

	if err := cleanupAnalysisIdentity(context.Background(), fc, run, "operator-ns"); err != nil {
		t.Fatalf("cleanupAnalysisIdentity: %v", err)
	}
	if err := fc.Get(context.Background(), client.ObjectKey{Name: name, Namespace: "operator-ns"}, &sa); !apierrors.IsNotFound(err) {
		t.Fatalf("ServiceAccount still exists after cleanup: %v", err)
	}
}

func TestAnalysisUsesScopedIdentityAndCleansItUp(t *testing.T) {
	run := testAgenticRun()
	run.UID = types.UID("analysis-run-uid")
	agent := newTestAgentCaller()
	objects := append([]client.Object{run}, defaultObjects()...)
	fc := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(objects...).
		WithStatusSubresource(run, &agenticv1alpha1.AnalysisResult{}, &agenticv1alpha1.ExecutionResult{}, &agenticv1alpha1.VerificationResult{}, &agenticv1alpha1.EscalationResult{}).Build()
	r := &AgenticRunReconciler{Client: fc, Agent: agent, Namespace: "operator-ns"}

	if _, err := reconcileOnce(r, run.Name); err != nil {
		t.Fatalf("reconcile analysis: %v", err)
	}
	want := analysisIdentityName(run)
	if agent.analysisServiceAccount != want {
		t.Fatalf("analysis ServiceAccount = %q, want %q", agent.analysisServiceAccount, want)
	}
	var sa corev1.ServiceAccount
	if err := fc.Get(context.Background(), client.ObjectKey{Name: want, Namespace: "operator-ns"}, &sa); err != nil {
		t.Fatalf("analysis ServiceAccount should remain until completion is observed: %v", err)
	}
	if _, err := reconcileOnce(r, run.Name); err != nil {
		t.Fatalf("reconcile post-analysis cleanup: %v", err)
	}
	if err := fc.Get(context.Background(), client.ObjectKey{Name: want, Namespace: "operator-ns"}, &sa); !apierrors.IsNotFound(err) {
		t.Fatalf("analysis ServiceAccount was not cleaned up: %v", err)
	}
}

func TestAnalysisIdentityNameIncludesRunUID(t *testing.T) {
	first := &agenticv1alpha1.AgenticRun{ObjectMeta: metav1.ObjectMeta{Name: strings.Repeat("long-name-", 10), Namespace: "team", UID: types.UID("first")}}
	second := first.DeepCopy()
	second.UID = types.UID("second")

	if analysisIdentityName(first) == analysisIdentityName(second) {
		t.Fatal("recreated runs must not share an analysis identity")
	}
	if len(analysisIdentityName(first)+"-logs") > 63 {
		t.Fatalf("derived log binding name is too long: %q", analysisIdentityName(first)+"-logs")
	}
}

func TestAnalysisIdentityCleanupFailureRequeues(t *testing.T) {
	run := testAgenticRun()
	run.UID = types.UID("analysis-run-uid")
	objects := append([]client.Object{run}, defaultObjects()...)
	fc := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(objects...).
		WithStatusSubresource(run, &agenticv1alpha1.AnalysisResult{}, &agenticv1alpha1.ExecutionResult{}, &agenticv1alpha1.VerificationResult{}, &agenticv1alpha1.EscalationResult{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				if _, ok := obj.(*rbacv1.RoleBinding); ok && strings.HasPrefix(obj.GetName(), "ls-analysis-") {
					return errors.New("transient delete failure")
				}
				return c.Delete(ctx, obj, opts...)
			},
		}).Build()
	r := &AgenticRunReconciler{Client: fc, Agent: newTestAgentCaller(), Namespace: "operator-ns"}

	if _, err := reconcileOnce(r, run.Name); err != nil {
		t.Fatalf("reconcile analysis: %v", err)
	}
	result, err := reconcileOnce(r, run.Name)
	if err != nil {
		t.Fatalf("reconcile cleanup: %v", err)
	}
	if result.RequeueAfter != rbacCleanupRequeueAfter {
		t.Fatalf("cleanup requeue = %v, want %v", result.RequeueAfter, rbacCleanupRequeueAfter)
	}
}
