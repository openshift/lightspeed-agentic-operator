package agenticrun

// Tests for sandbox-execution.md rule 21d (OLS-4237): execution RBAC for a
// namespace that is terminating.

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
)

// namespaceTerminatingError builds the error the API server's
// NamespaceLifecycle admission plugin returns for a create in a terminating
// namespace: Forbidden, with a NamespaceTerminating status cause.
func namespaceTerminatingError(resource, name, ns string) error {
	err := apierrors.NewForbidden(
		schema.GroupResource{Group: rbacv1.GroupName, Resource: resource},
		name,
		fmt.Errorf("unable to create new content in namespace %s because it is being terminated", ns),
	)
	err.ErrStatus.Details.Causes = append(err.ErrStatus.Details.Causes, metav1.StatusCause{
		Type:    corev1.NamespaceTerminatingCause,
		Message: fmt.Sprintf("namespace %s is being terminated", ns),
		Field:   "metadata.namespace",
	})
	return err
}

func terminatingNSRun() *agenticv1alpha1.AgenticRun {
	return &agenticv1alpha1.AgenticRun{
		ObjectMeta: metav1.ObjectMeta{Name: "fix-stuck-ns", Namespace: "openshift-lightspeed", UID: "uid-ols-4237"},
	}
}

func configMapRuleIn(ns string) agenticv1alpha1.RBACRule {
	return agenticv1alpha1.RBACRule{
		Namespace:     ns,
		APIGroups:     []string{""},
		Resources:     []string{"configmaps"},
		Verbs:         []string{"get", "patch"},
		Justification: "Clear blocking ConfigMap finalizer",
	}
}

// assertTerminatingNamespaceError checks the rule 21d error contract: the
// error names the namespace and states that namespace-scoped execution
// access cannot be granted because the namespace is terminating.
func assertTerminatingNamespaceError(t *testing.T, err error, ns string) {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error for a terminating namespace, got nil")
	}
	want := fmt.Sprintf("namespace-scoped execution RBAC cannot be granted: namespace %s is terminating", ns)
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("error does not state the terminating-namespace reason\n got: %v\nwant substring: %q", err, want)
	}
}

// assertNoClusterScopedExecutionRBAC checks that the operator did not widen
// the approved namespace-scoped rules to cluster scope.
func assertNoClusterScopedExecutionRBAC(t *testing.T, c client.Client, runUID string) {
	t.Helper()
	ctx := context.Background()
	crName := clusterRoleName(runUID)
	if err := c.Get(ctx, types.NamespacedName{Name: crName}, &rbacv1.ClusterRole{}); !apierrors.IsNotFound(err) {
		t.Fatalf("ClusterRole %s must not exist (no widening to cluster scope); get err = %v", crName, err)
	}
	if err := c.Get(ctx, types.NamespacedName{Name: crName}, &rbacv1.ClusterRoleBinding{}); !apierrors.IsNotFound(err) {
		t.Fatalf("ClusterRoleBinding %s must not exist (no widening to cluster scope); get err = %v", crName, err)
	}
}

// Break caught: Role create rejected by a terminating namespace surfaces as the
// raw admission 403 instead of the rule 21d error.
func TestEnsureExecutionRBAC_TerminatingNamespace_RoleCreateRejected(t *testing.T) {
	ctx := context.Background()
	run := terminatingNSRun()
	fc := fake.NewClientBuilder().WithScheme(testScheme()).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if obj.GetNamespace() == "mock-stuck-ns" {
					return namespaceTerminatingError("roles", obj.GetName(), "mock-stuck-ns")
				}
				return c.Create(ctx, obj, opts...)
			},
		}).Build()

	rbacResult := &agenticv1alpha1.RBACResult{
		NamespaceScoped: []agenticv1alpha1.RBACRule{configMapRuleIn("mock-stuck-ns")},
	}

	err := ensureExecutionRBAC(ctx, fc, run, rbacResult, "openshift-lightspeed", nil)

	assertTerminatingNamespaceError(t, err, "mock-stuck-ns")
	assertNoClusterScopedExecutionRBAC(t, fc, string(run.UID))
}

// Break caught: the namespace starts terminating between the Role create and
// the RoleBinding create, and the RoleBinding rejection surfaces as the raw 403.
func TestEnsureExecutionRBAC_TerminatingNamespace_RoleBindingCreateRejected(t *testing.T) {
	ctx := context.Background()
	run := terminatingNSRun()
	fc := fake.NewClientBuilder().WithScheme(testScheme()).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if _, ok := obj.(*rbacv1.RoleBinding); ok && obj.GetNamespace() == "mock-stuck-ns" {
					return namespaceTerminatingError("rolebindings", obj.GetName(), "mock-stuck-ns")
				}
				return c.Create(ctx, obj, opts...)
			},
		}).Build()

	rbacResult := &agenticv1alpha1.RBACResult{
		NamespaceScoped: []agenticv1alpha1.RBACRule{configMapRuleIn("mock-stuck-ns")},
	}

	err := ensureExecutionRBAC(ctx, fc, run, rbacResult, "openshift-lightspeed", nil)

	assertTerminatingNamespaceError(t, err, "mock-stuck-ns")
	assertNoClusterScopedExecutionRBAC(t, fc, string(run.UID))
}

// Break caught: the terminating-namespace handling is keyed on any Forbidden
// error, so a policy or RBAC-escalation denial is wrongly reported as
// "namespace is terminating". This guards the detection boundary; it passes
// on the unfixed code and must keep passing after the fix.
func TestEnsureExecutionRBAC_ForbiddenWithoutTerminatingCause_KeepsOriginalError(t *testing.T) {
	ctx := context.Background()
	run := terminatingNSRun()
	denial := apierrors.NewForbidden(
		schema.GroupResource{Group: rbacv1.GroupName, Resource: "roles"},
		executionRoleName(string(run.UID)),
		fmt.Errorf("user \"system:serviceaccount:openshift-lightspeed:controller-manager\" is attempting to grant RBAC permissions not currently held"),
	)
	fc := fake.NewClientBuilder().WithScheme(testScheme()).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if _, ok := obj.(*rbacv1.Role); ok {
					return denial
				}
				return c.Create(ctx, obj, opts...)
			},
		}).Build()

	rbacResult := &agenticv1alpha1.RBACResult{
		NamespaceScoped: []agenticv1alpha1.RBACRule{configMapRuleIn("production")},
	}

	err := ensureExecutionRBAC(ctx, fc, run, rbacResult, "openshift-lightspeed", nil)
	if err == nil {
		t.Fatal("expected the policy denial to be returned, got nil")
	}
	if !apierrors.IsForbidden(err) {
		t.Fatalf("policy denial must stay a Forbidden error, got: %v", err)
	}
	if !strings.Contains(err.Error(), "attempting to grant RBAC permissions not currently held") {
		t.Fatalf("policy denial message lost, got: %v", err)
	}
	if strings.Contains(err.Error(), "terminating") {
		t.Fatalf("policy denial must not be reported as a terminating namespace, got: %v", err)
	}
}

// TestEnsureExecutionRBAC_TerminatingNamespace_RealAPIServer checks rule 21d
// against a real kube-apiserver, so the NamespaceLifecycle rejection is the
// real one and not a test double. Skips unless KUBEBUILDER_ASSETS is set.
func TestEnsureExecutionRBAC_TerminatingNamespace_RealAPIServer(t *testing.T) {
	assetsDir := os.Getenv("KUBEBUILDER_ASSETS")
	if assetsDir == "" {
		t.Skip("set KUBEBUILDER_ASSETS to the directory containing envtest binaries to run this test")
	}

	testEnv := &envtest.Environment{BinaryAssetsDirectory: assetsDir, Scheme: testScheme()}
	cfg, err := testEnv.Start()
	if err != nil {
		t.Fatalf("start envtest: %v", err)
	}
	defer func() {
		if err := testEnv.Stop(); err != nil {
			t.Errorf("stop envtest: %v", err)
		}
	}()

	c, err := client.New(cfg, client.Options{Scheme: testScheme()})
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// A finalizer keeps the namespace in Terminating after Delete.
	stuck := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:       "mock-stuck-ns",
		Finalizers: []string{"ols-4237.example.com/block"},
	}}
	if err := c.Create(ctx, stuck); err != nil {
		t.Fatalf("create namespace: %v", err)
	}
	if err := c.Delete(ctx, stuck); err != nil {
		t.Fatalf("delete namespace: %v", err)
	}
	var ns corev1.Namespace
	if err := c.Get(ctx, types.NamespacedName{Name: "mock-stuck-ns"}, &ns); err != nil {
		t.Fatalf("get namespace: %v", err)
	}
	if ns.DeletionTimestamp == nil {
		t.Fatal("precondition: namespace must have a deletionTimestamp")
	}

	run := terminatingNSRun()
	rbacResult := &agenticv1alpha1.RBACResult{
		NamespaceScoped: []agenticv1alpha1.RBACRule{configMapRuleIn("mock-stuck-ns")},
	}

	err = ensureExecutionRBAC(ctx, c, run, rbacResult, "openshift-lightspeed", nil)

	assertTerminatingNamespaceError(t, err, "mock-stuck-ns")
	assertNoClusterScopedExecutionRBAC(t, c, string(run.UID))
}

// TestSandboxManagerCreate_TerminatingNamespace exercises the full
// SandboxManager.Create path for rule 21d [OLS-4237]: the approved option
// targets two namespaces, the healthy one first. When the Role create in the
// terminating namespace is rejected, Create must fail with the rule 21d
// error, and the failure cleanup must remove the partial RBAC (healthy-ns
// Role/RoleBinding), the reader subject, and the per-step SA.
func TestSandboxManagerCreate_TerminatingNamespace(t *testing.T) {
	ctx := context.Background()
	resetReaderBindings()
	fc := fake.NewClientBuilder().WithScheme(testScheme()).
		WithObjects(testReaderCRB()).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if obj.GetNamespace() == "mock-stuck-ns" {
					return namespaceTerminatingError("roles", obj.GetName(), "mock-stuck-ns")
				}
				return c.Create(ctx, obj, opts...)
			},
		}).Build()
	mgr := newTestSandboxManager(fc, testCache(t, "bare-pod"))

	run := testSMRun()
	run = seedSandboxRun(t, mgr, run)
	runUID := string(run.UID)

	agentCtx := &agentContext{
		ApprovedOption: &agenticv1alpha1.RemediationOption{
			Title:   "mock-remediation",
			Summary: "mock option summary",
			RBAC: agenticv1alpha1.RBACResult{
				NamespaceScoped: []agenticv1alpha1.RBACRule{
					configMapRuleIn("healthy-ns"),
					configMapRuleIn("mock-stuck-ns"),
				},
			},
		},
	}

	name, err := mgr.Create(ctx, run, "execution", testSMAgent(), testLLMForManager(), nil, 15*time.Minute, agentCtx)
	if err == nil {
		t.Fatalf("expected Create to fail for a terminating namespace, got name %q", name)
	}
	if !strings.Contains(err.Error(), "namespace-scoped execution RBAC cannot be granted") ||
		!strings.Contains(err.Error(), "mock-stuck-ns is terminating") {
		t.Fatalf("Create error does not carry the rule 21d message: %v", err)
	}
	if name != "" {
		t.Fatalf("Create must not return a sandbox name on failure, got %q", name)
	}

	// No sandbox pod may exist for the run.
	var pods corev1.PodList
	if err := fc.List(ctx, &pods, client.InNamespace("test-ns")); err != nil {
		t.Fatalf("list pods: %v", err)
	}
	for _, pod := range pods.Items {
		if pod.Labels[LabelRun] == runUID {
			t.Fatalf("sandbox pod %s must not be created for a failed Create", pod.Name)
		}
	}

	// Partial execution RBAC from the healthy namespace must be cleaned up.
	roleName := executionRoleName(runUID)
	for _, ns := range []string{"healthy-ns", "mock-stuck-ns"} {
		if err := fc.Get(ctx, types.NamespacedName{Name: roleName, Namespace: ns}, &rbacv1.Role{}); !apierrors.IsNotFound(err) {
			t.Errorf("Role in %s must be cleaned up after Create failure; get err = %v", ns, err)
		}
		if err := fc.Get(ctx, types.NamespacedName{Name: roleName, Namespace: ns}, &rbacv1.RoleBinding{}); !apierrors.IsNotFound(err) {
			t.Errorf("RoleBinding in %s must be cleaned up after Create failure; get err = %v", ns, err)
		}
	}

	// The per-step SA must be cleaned up.
	saName := sandboxSAName(run, "execution")
	if err := fc.Get(ctx, types.NamespacedName{Name: saName, Namespace: "test-ns"}, &corev1.ServiceAccount{}); !apierrors.IsNotFound(err) {
		t.Errorf("per-step SA %s must be cleaned up after Create failure; get err = %v", saName, err)
	}

	// The shared reader binding must keep its original subject.
	crb := &rbacv1.ClusterRoleBinding{}
	if err := fc.Get(ctx, types.NamespacedName{Name: defaultReaderClusterRoleBinding}, crb); err != nil {
		t.Fatalf("get shared reader ClusterRoleBinding: %v", err)
	}
	for _, s := range crb.Subjects {
		if s.Name == sandboxSAName(run, "execution") {
			t.Errorf("per-step SA subject must be removed from %s on Create failure", defaultReaderClusterRoleBinding)
		}
	}
}
