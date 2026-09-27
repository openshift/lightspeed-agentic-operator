package agenticrun

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	toolscache "k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
)

const defaultReaderClusterRoleBinding = "lightspeed-agent-cluster-reader"

func resetReaderBindings() {
	readerBindings.Store([]string(nil))
}

// readerBinding returns the pre-existing cluster-reader ClusterRoleBinding fixture
// that must exist for execution SA setup to succeed.
func readerBinding() *rbacv1.ClusterRoleBinding {
	return &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: defaultReaderClusterRoleBinding},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "cluster-reader"},
		Subjects: []rbacv1.Subject{{
			Kind:      rbacv1.ServiceAccountKind,
			Name:      "lightspeed-agent",
			Namespace: "default",
		}},
	}
}

// ---------------------------------------------------------------------------
// ensureExecutionRBAC
// ---------------------------------------------------------------------------

func TestEnsureExecutionRBAC_NamespaceScopedOnly(t *testing.T) {
	ctx := context.Background()
	resetReaderBindings()
	fc := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(readerBinding()).Build()

	run := &agenticv1alpha1.AgenticRun{
		ObjectMeta: metav1.ObjectMeta{Name: "fix-oom", Namespace: "app-ns", UID: "uid-fix-oom"},
		Spec:       agenticv1alpha1.AgenticRunSpec{TargetNamespaces: []string{"production"}},
	}
	rbacResult := &agenticv1alpha1.RBACResult{
		NamespaceScoped: []agenticv1alpha1.RBACRule{{
			APIGroups:     []string{"apps"},
			Resources:     []string{"deployments"},
			Verbs:         []string{"get", "patch"},
			Justification: "Patch deployment memory",
		}},
	}

	if err := ensureExecutionRBAC(ctx, fc, run, rbacResult, "default", nil); err != nil {
		t.Fatalf("ensureExecutionRBAC: %v", err)
	}

	roleName := executionRoleName("uid-fix-oom")

	// Verify Role
	var role rbacv1.Role
	if err := fc.Get(ctx, types.NamespacedName{Name: roleName, Namespace: "production"}, &role); err != nil {
		t.Fatalf("Role not found in production: %v", err)
	}
	if len(role.Rules) != 1 {
		t.Fatalf("expected 1 rule, got %d", len(role.Rules))
	}
	if role.Rules[0].APIGroups[0] != "apps" {
		t.Fatalf("unexpected apiGroup: %s", role.Rules[0].APIGroups[0])
	}
	if role.Rules[0].Resources[0] != "deployments" {
		t.Fatalf("unexpected resource: %s", role.Rules[0].Resources[0])
	}
	if role.Labels[LabelRun] != "uid-fix-oom" {
		t.Fatalf("run label = %q, want uid-fix-oom", role.Labels[LabelRun])
	}
	if role.Labels[LabelComponent] != "execution-rbac" {
		t.Fatalf("missing component label")
	}

	// Verify RoleBinding
	var binding rbacv1.RoleBinding
	if err := fc.Get(ctx, types.NamespacedName{Name: roleName, Namespace: "production"}, &binding); err != nil {
		t.Fatalf("RoleBinding not found: %v", err)
	}
	if len(binding.Subjects) != 1 {
		t.Fatalf("expected 1 subject, got %d", len(binding.Subjects))
	}
	if binding.Subjects[0].Name != sandboxSAName(run, "execution") {
		t.Fatalf("unexpected subject: %s", binding.Subjects[0].Name)
	}
	if binding.Subjects[0].Namespace != "default" {
		t.Fatalf("subject namespace should be operator ns, got %s", binding.Subjects[0].Namespace)
	}
	if binding.RoleRef.Kind != "Role" || binding.RoleRef.Name != roleName {
		t.Fatalf("unexpected roleRef: %+v", binding.RoleRef)
	}

	// Verify annotation
	if run.Annotations[rbacNamespacesAnnotation] != "production" {
		t.Fatalf("expected rbac-namespaces annotation, got %q", run.Annotations[rbacNamespacesAnnotation])
	}

	// No ClusterRole should exist
	crName := clusterRoleName("fix-oom")
	var cr rbacv1.ClusterRole
	if err := fc.Get(ctx, types.NamespacedName{Name: crName}, &cr); err == nil {
		t.Fatal("ClusterRole should not exist for namespace-only RBAC")
	}
}

func TestEnsureExecutionRBAC_ClusterScopedOnly(t *testing.T) {
	ctx := context.Background()
	resetReaderBindings()
	fc := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(readerBinding()).Build()

	run := &agenticv1alpha1.AgenticRun{
		ObjectMeta: metav1.ObjectMeta{Name: "check-nodes", Namespace: "default", UID: "uid-check-nodes"},
	}
	rbacResult := &agenticv1alpha1.RBACResult{
		ClusterScoped: []agenticv1alpha1.RBACRule{{
			APIGroups:     []string{""},
			Resources:     []string{"nodes"},
			Verbs:         []string{"get", "list"},
			Justification: "Read node status",
		}},
	}

	if err := ensureExecutionRBAC(ctx, fc, run, rbacResult, "default", nil); err != nil {
		t.Fatalf("ensureExecutionRBAC: %v", err)
	}

	crName := clusterRoleName("uid-check-nodes")

	// Verify ClusterRole
	var cr rbacv1.ClusterRole
	if err := fc.Get(ctx, types.NamespacedName{Name: crName}, &cr); err != nil {
		t.Fatalf("ClusterRole not found: %v", err)
	}
	if len(cr.Rules) != 1 || cr.Rules[0].Resources[0] != "nodes" {
		t.Fatalf("unexpected ClusterRole rules: %+v", cr.Rules)
	}

	// Verify ClusterRoleBinding
	var crb rbacv1.ClusterRoleBinding
	if err := fc.Get(ctx, types.NamespacedName{Name: crName}, &crb); err != nil {
		t.Fatalf("ClusterRoleBinding not found: %v", err)
	}
	if crb.RoleRef.Kind != "ClusterRole" || crb.RoleRef.Name != crName {
		t.Fatalf("unexpected roleRef: %+v", crb.RoleRef)
	}
	if crb.Subjects[0].Name != sandboxSAName(run, "execution") {
		t.Fatalf("unexpected subject: %s", crb.Subjects[0].Name)
	}
}

func TestEnsureExecutionRBAC_BothScopes(t *testing.T) {
	ctx := context.Background()
	resetReaderBindings()
	fc := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(readerBinding()).Build()

	run := &agenticv1alpha1.AgenticRun{
		ObjectMeta: metav1.ObjectMeta{Name: "full-fix", Namespace: "default", UID: "uid-full-fix"},
		Spec:       agenticv1alpha1.AgenticRunSpec{TargetNamespaces: []string{"staging"}},
	}
	rbacResult := &agenticv1alpha1.RBACResult{
		NamespaceScoped: []agenticv1alpha1.RBACRule{{
			APIGroups: []string{"apps"}, Resources: []string{"deployments"},
			Verbs: []string{"get", "patch"}, Justification: "Patch deploy",
		}},
		ClusterScoped: []agenticv1alpha1.RBACRule{{
			APIGroups: []string{""}, Resources: []string{"nodes"},
			Verbs: []string{"get"}, Justification: "Read nodes",
		}},
	}

	if err := ensureExecutionRBAC(ctx, fc, run, rbacResult, "default", nil); err != nil {
		t.Fatalf("ensureExecutionRBAC: %v", err)
	}

	// Role in staging
	var role rbacv1.Role
	if err := fc.Get(ctx, types.NamespacedName{Name: executionRoleName("uid-full-fix"), Namespace: "staging"}, &role); err != nil {
		t.Fatalf("Role not found: %v", err)
	}

	// ClusterRole
	var cr rbacv1.ClusterRole
	if err := fc.Get(ctx, types.NamespacedName{Name: clusterRoleName("uid-full-fix")}, &cr); err != nil {
		t.Fatalf("ClusterRole not found: %v", err)
	}
}

func TestEnsureExecutionRBAC_MultipleNamespaces(t *testing.T) {
	ctx := context.Background()
	resetReaderBindings()
	fc := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(readerBinding()).Build()

	run := &agenticv1alpha1.AgenticRun{
		ObjectMeta: metav1.ObjectMeta{Name: "multi-ns", Namespace: "default", UID: "uid-multi-ns"},
		Spec:       agenticv1alpha1.AgenticRunSpec{TargetNamespaces: []string{"ns-a", "ns-b", "ns-c"}},
	}
	rbacResult := &agenticv1alpha1.RBACResult{
		NamespaceScoped: []agenticv1alpha1.RBACRule{{
			APIGroups: []string{""}, Resources: []string{"pods"},
			Verbs: []string{"get", "delete"}, Justification: "Restart pod",
		}},
	}

	if err := ensureExecutionRBAC(ctx, fc, run, rbacResult, "default", nil); err != nil {
		t.Fatalf("ensureExecutionRBAC: %v", err)
	}

	roleName := executionRoleName("uid-multi-ns")
	for _, ns := range []string{"ns-a", "ns-b", "ns-c"} {
		var role rbacv1.Role
		if err := fc.Get(ctx, types.NamespacedName{Name: roleName, Namespace: ns}, &role); err != nil {
			t.Fatalf("Role not found in %s: %v", ns, err)
		}
		var binding rbacv1.RoleBinding
		if err := fc.Get(ctx, types.NamespacedName{Name: roleName, Namespace: ns}, &binding); err != nil {
			t.Fatalf("RoleBinding not found in %s: %v", ns, err)
		}
	}

	// Annotation should contain all namespaces
	got := run.Annotations[rbacNamespacesAnnotation]
	if got != "ns-a,ns-b,ns-c" {
		t.Fatalf("expected annotation 'ns-a,ns-b,ns-c', got %q", got)
	}
}

func TestEnsureExecutionRBAC_Idempotent(t *testing.T) {
	ctx := context.Background()
	resetReaderBindings()
	fc := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(readerBinding()).Build()

	run := &agenticv1alpha1.AgenticRun{
		ObjectMeta: metav1.ObjectMeta{Name: "idem", Namespace: "default", UID: "uid-idem"},
		Spec:       agenticv1alpha1.AgenticRunSpec{TargetNamespaces: []string{"prod"}},
	}
	rbacResult := &agenticv1alpha1.RBACResult{
		NamespaceScoped: []agenticv1alpha1.RBACRule{{
			APIGroups: []string{"apps"}, Resources: []string{"deployments"},
			Verbs: []string{"get"}, Justification: "Read deploy",
		}},
		ClusterScoped: []agenticv1alpha1.RBACRule{{
			APIGroups: []string{""}, Resources: []string{"nodes"},
			Verbs: []string{"get"}, Justification: "Read nodes",
		}},
	}

	if err := ensureExecutionRBAC(ctx, fc, run, rbacResult, "default", nil); err != nil {
		t.Fatalf("first call: %v", err)
	}
	if err := ensureExecutionRBAC(ctx, fc, run, rbacResult, "default", nil); err != nil {
		t.Fatalf("idempotent second call should not error: %v", err)
	}
}

func TestEnsureExecutionRBAC_NilResult(t *testing.T) {
	ctx := context.Background()
	resetReaderBindings()
	fc := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(readerBinding()).Build()

	run := &agenticv1alpha1.AgenticRun{
		ObjectMeta: metav1.ObjectMeta{Name: "no-rbac", Namespace: "default", UID: "uid-no-rbac"},
	}

	if err := ensureExecutionRBAC(ctx, fc, run, nil, "default", nil); err != nil {
		t.Fatalf("nil RBACResult should be no-op: %v", err)
	}
}

func TestEnsureExecutionRBAC_EmptyRules(t *testing.T) {
	ctx := context.Background()
	resetReaderBindings()
	fc := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(readerBinding()).Build()

	run := &agenticv1alpha1.AgenticRun{
		ObjectMeta: metav1.ObjectMeta{Name: "empty-rules", Namespace: "default", UID: "uid-empty-rules"},
		Spec:       agenticv1alpha1.AgenticRunSpec{TargetNamespaces: []string{"prod"}},
	}
	rbacResult := &agenticv1alpha1.RBACResult{}

	if err := ensureExecutionRBAC(ctx, fc, run, rbacResult, "default", nil); err != nil {
		t.Fatalf("empty RBACResult should be no-op: %v", err)
	}

	// No Role should exist
	var role rbacv1.Role
	if err := fc.Get(ctx, types.NamespacedName{Name: executionRoleName("uid-empty-rules"), Namespace: "prod"}, &role); err == nil {
		t.Fatal("Role should not exist for empty rules")
	}
}

func TestEnsureExecutionRBAC_NamespacesFromRBACRules(t *testing.T) {
	ctx := context.Background()
	resetReaderBindings()
	fc := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(readerBinding()).Build()

	ns1 := "app-ns"
	ns2 := "data-ns"
	run := &agenticv1alpha1.AgenticRun{
		ObjectMeta: metav1.ObjectMeta{Name: "ns-from-rules", Namespace: "default", UID: "uid-ns-from-rules"},
	}
	rbacResult := &agenticv1alpha1.RBACResult{
		NamespaceScoped: []agenticv1alpha1.RBACRule{
			{Namespace: ns1, APIGroups: []string{""}, Resources: []string{"pods"}, Verbs: []string{"get"}, Justification: "Read pods"},
			{Namespace: ns2, APIGroups: []string{""}, Resources: []string{"services"}, Verbs: []string{"get"}, Justification: "Read services"},
		},
	}

	if err := ensureExecutionRBAC(ctx, fc, run, rbacResult, "default", nil); err != nil {
		t.Fatalf("ensureExecutionRBAC: %v", err)
	}

	roleName := executionRoleName("uid-ns-from-rules")
	for _, ns := range []string{"app-ns", "data-ns"} {
		var role rbacv1.Role
		if err := fc.Get(ctx, types.NamespacedName{Name: roleName, Namespace: ns}, &role); err != nil {
			t.Fatalf("Role not found in %s: %v", ns, err)
		}
	}
}

func TestEnsureExecutionRBAC_ResourceNames(t *testing.T) {
	ctx := context.Background()
	resetReaderBindings()
	fc := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(readerBinding()).Build()

	run := &agenticv1alpha1.AgenticRun{
		ObjectMeta: metav1.ObjectMeta{Name: "with-names", Namespace: "default", UID: "uid-with-names"},
		Spec:       agenticv1alpha1.AgenticRunSpec{TargetNamespaces: []string{"prod"}},
	}
	rbacResult := &agenticv1alpha1.RBACResult{
		NamespaceScoped: []agenticv1alpha1.RBACRule{{
			APIGroups:     []string{"apps"},
			Resources:     []string{"deployments"},
			ResourceNames: []string{"web-frontend"},
			Verbs:         []string{"get", "patch"},
			Justification: "Patch specific deployment",
		}},
	}

	if err := ensureExecutionRBAC(ctx, fc, run, rbacResult, "default", nil); err != nil {
		t.Fatalf("ensureExecutionRBAC: %v", err)
	}

	var role rbacv1.Role
	if err := fc.Get(ctx, types.NamespacedName{Name: executionRoleName("uid-with-names"), Namespace: "prod"}, &role); err != nil {
		t.Fatalf("Role not found: %v", err)
	}
	if len(role.Rules[0].ResourceNames) != 1 || role.Rules[0].ResourceNames[0] != "web-frontend" {
		t.Fatalf("ResourceNames not preserved: %v", role.Rules[0].ResourceNames)
	}
}

// ---------------------------------------------------------------------------
// cleanupExecutionRBAC
// ---------------------------------------------------------------------------

func TestCleanupExecutionRBAC_NamespaceAndCluster(t *testing.T) {
	ctx := context.Background()
	resetReaderBindings()
	fc := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(readerBinding()).Build()

	run := &agenticv1alpha1.AgenticRun{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "cleanup-test",
			Namespace:   "default",
			UID:         "uid-cleanup-test",
			Annotations: map[string]string{rbacNamespacesAnnotation: "ns-a,ns-b"},
		},
		Spec: agenticv1alpha1.AgenticRunSpec{TargetNamespaces: []string{"ns-a", "ns-b"}},
	}
	rbacResult := &agenticv1alpha1.RBACResult{
		NamespaceScoped: []agenticv1alpha1.RBACRule{{
			APIGroups: []string{"apps"}, Resources: []string{"deployments"},
			Verbs: []string{"get"}, Justification: "Read",
		}},
		ClusterScoped: []agenticv1alpha1.RBACRule{{
			APIGroups: []string{""}, Resources: []string{"nodes"},
			Verbs: []string{"get"}, Justification: "Read nodes",
		}},
	}

	// Create RBAC
	if err := ensureExecutionRBAC(ctx, fc, run, rbacResult, "default", nil); err != nil {
		t.Fatalf("ensure: %v", err)
	}

	// Verify resources exist
	roleName := executionRoleName("uid-cleanup-test")
	crName := clusterRoleName("uid-cleanup-test")
	var role rbacv1.Role
	if err := fc.Get(ctx, types.NamespacedName{Name: roleName, Namespace: "ns-a"}, &role); err != nil {
		t.Fatalf("Role not created: %v", err)
	}
	var cr rbacv1.ClusterRole
	if err := fc.Get(ctx, types.NamespacedName{Name: crName}, &cr); err != nil {
		t.Fatalf("ClusterRole not created: %v", err)
	}

	// Cleanup
	if err := cleanupExecutionRBAC(ctx, fc, run); err != nil {
		t.Fatalf("cleanupExecutionRBAC: %v", err)
	}

	// Verify all deleted
	if err := fc.Get(ctx, types.NamespacedName{Name: roleName, Namespace: "ns-a"}, &role); err == nil {
		t.Fatal("Role in ns-a should be deleted")
	}
	if err := fc.Get(ctx, types.NamespacedName{Name: roleName, Namespace: "ns-b"}, &role); err == nil {
		t.Fatal("Role in ns-b should be deleted")
	}
	var binding rbacv1.RoleBinding
	if err := fc.Get(ctx, types.NamespacedName{Name: roleName, Namespace: "ns-a"}, &binding); err == nil {
		t.Fatal("RoleBinding in ns-a should be deleted")
	}
	if err := fc.Get(ctx, types.NamespacedName{Name: crName}, &cr); err == nil {
		t.Fatal("ClusterRole should be deleted")
	}
	var crb rbacv1.ClusterRoleBinding
	if err := fc.Get(ctx, types.NamespacedName{Name: crName}, &crb); err == nil {
		t.Fatal("ClusterRoleBinding should be deleted")
	}
}

func TestCleanupExecutionRBAC_NoAnnotation(t *testing.T) {
	ctx := context.Background()
	resetReaderBindings()
	fc := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(readerBinding()).Build()

	run := &agenticv1alpha1.AgenticRun{
		ObjectMeta: metav1.ObjectMeta{Name: "no-annot", Namespace: "default", UID: "uid-no-annot"},
	}

	// Create cluster-scoped only
	rbacResult := &agenticv1alpha1.RBACResult{
		ClusterScoped: []agenticv1alpha1.RBACRule{{
			APIGroups: []string{""}, Resources: []string{"nodes"},
			Verbs: []string{"get"}, Justification: "Read nodes",
		}},
	}
	if err := ensureExecutionRBAC(ctx, fc, run, rbacResult, "default", nil); err != nil {
		t.Fatalf("ensure: %v", err)
	}

	// Cleanup with no namespace annotation — should still clean cluster resources
	if err := cleanupExecutionRBAC(ctx, fc, run); err != nil {
		t.Fatalf("cleanupExecutionRBAC: %v", err)
	}

	crName := clusterRoleName("uid-no-annot")
	var cr rbacv1.ClusterRole
	if err := fc.Get(ctx, types.NamespacedName{Name: crName}, &cr); err == nil {
		t.Fatal("ClusterRole should be deleted")
	}
}

func TestCleanupExecutionRBAC_MissingResources(t *testing.T) {
	ctx := context.Background()
	resetReaderBindings()
	fc := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(readerBinding()).Build()

	run := &agenticv1alpha1.AgenticRun{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "already-gone",
			Namespace:   "default",
			UID:         "uid-already-gone",
			Annotations: map[string]string{rbacNamespacesAnnotation: "ghost-ns"},
		},
	}

	// Nothing created — cleanup should tolerate NotFound
	if err := cleanupExecutionRBAC(ctx, fc, run); err != nil {
		t.Fatalf("cleanup of missing resources should succeed: %v", err)
	}
}

// ---------------------------------------------------------------------------
// ensureResultRBAC / resultRoleName / setResultRBACOwner
// ---------------------------------------------------------------------------

func TestResultRoleName(t *testing.T) {
	name := resultRoleName("uid-abc", "analysis")
	if name != "ls-result-analysis-uid-abc" {
		t.Fatalf("resultRoleName = %q", name)
	}
}

func TestEnsureResultRBAC_CreatesRoleAndBinding(t *testing.T) {
	ctx := context.Background()
	fc := fake.NewClientBuilder().WithScheme(testScheme()).Build()

	run := &agenticv1alpha1.AgenticRun{
		ObjectMeta: metav1.ObjectMeta{Name: "my-run", Namespace: "app-ns", UID: "uid-my-run"},
	}

	if err := ensureResultRBAC(ctx, fc, run, "analysis", "sa-analysis", "operator-ns"); err != nil {
		t.Fatalf("ensureResultRBAC: %v", err)
	}

	roleName := resultRoleName("uid-my-run", "analysis")

	var role rbacv1.Role
	if err := fc.Get(ctx, types.NamespacedName{Name: roleName, Namespace: "operator-ns"}, &role); err != nil {
		t.Fatalf("Role not found: %v", err)
	}
	if len(role.Rules) != 2 {
		t.Fatalf("expected 2 rules, got %d", len(role.Rules))
	}
	if role.Rules[0].Verbs[0] != "create" {
		t.Fatalf("first rule should be create, got %v", role.Rules[0].Verbs)
	}
	if len(role.Rules[0].ResourceNames) != 0 {
		t.Fatal("create rule must not have resourceNames")
	}
	if role.Rules[1].ResourceNames[0] != resultCRName("my-run", "analysis", 1) {
		t.Fatalf("resourceNames = %v, want %s", role.Rules[1].ResourceNames, resultCRName("my-run", "analysis", 1))
	}
	if role.Rules[0].Resources[0] != "analysisresults" {
		t.Fatalf("resource = %q, want analysisresults", role.Rules[0].Resources[0])
	}
	if role.Labels[LabelRun] != "uid-my-run" {
		t.Fatalf("LabelRun = %q, want uid-my-run", role.Labels[LabelRun])
	}

	var binding rbacv1.RoleBinding
	if err := fc.Get(ctx, types.NamespacedName{Name: roleName, Namespace: "operator-ns"}, &binding); err != nil {
		t.Fatalf("RoleBinding not found: %v", err)
	}
	if binding.Subjects[0].Name != "sa-analysis" {
		t.Fatalf("subject = %q, want sa-analysis", binding.Subjects[0].Name)
	}
}

func TestEnsureResultRBAC_AllSteps(t *testing.T) {
	ctx := context.Background()

	for step, resource := range stepResultResource {
		fc := fake.NewClientBuilder().WithScheme(testScheme()).Build()
		run := &agenticv1alpha1.AgenticRun{
			ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "ns", UID: "uid-r"},
		}
		if err := ensureResultRBAC(ctx, fc, run, step, "sa", "op"); err != nil {
			t.Fatalf("step %s: %v", step, err)
		}
		var role rbacv1.Role
		if err := fc.Get(ctx, types.NamespacedName{Name: resultRoleName("uid-r", step), Namespace: "op"}, &role); err != nil {
			t.Fatalf("step %s: Role not found: %v", step, err)
		}
		if role.Rules[0].Resources[0] != resource {
			t.Fatalf("step %s: resource = %q, want %q", step, role.Rules[0].Resources[0], resource)
		}
	}
}

func TestEnsureResultRBAC_EscalationReadsOtherResults(t *testing.T) {
	ctx := context.Background()
	fc := fake.NewClientBuilder().WithScheme(testScheme()).Build()
	run := &agenticv1alpha1.AgenticRun{
		ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "ns", UID: "uid-r"},
	}
	if err := ensureResultRBAC(ctx, fc, run, "escalation", "sa-esc", "op"); err != nil {
		t.Fatalf("ensureResultRBAC: %v", err)
	}
	var role rbacv1.Role
	if err := fc.Get(ctx, types.NamespacedName{Name: resultRoleName("uid-r", "escalation"), Namespace: "op"}, &role); err != nil {
		t.Fatalf("Role not found: %v", err)
	}
	if len(role.Rules) != 3 {
		t.Fatalf("expected 3 rules for escalation (create + get/patch + read prior results), got %d", len(role.Rules))
	}
	readRule := role.Rules[2]
	wantResources := []string{"analysisresults", "executionresults", "verificationresults"}
	if len(readRule.Resources) != len(wantResources) {
		t.Fatalf("read rule resources = %v, want %v", readRule.Resources, wantResources)
	}
	for i, r := range wantResources {
		if readRule.Resources[i] != r {
			t.Errorf("read rule resource[%d] = %q, want %q", i, readRule.Resources[i], r)
		}
	}
	if len(readRule.Verbs) != 2 || readRule.Verbs[0] != "get" || readRule.Verbs[1] != "list" {
		t.Errorf("read rule verbs = %v, want [get list]", readRule.Verbs)
	}
}

func TestEnsureResultRBAC_NonEscalationNoReadRule(t *testing.T) {
	ctx := context.Background()
	for _, step := range []string{"analysis", "execution", "verification"} {
		fc := fake.NewClientBuilder().WithScheme(testScheme()).Build()
		run := &agenticv1alpha1.AgenticRun{
			ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "ns", UID: "uid-r"},
		}
		if err := ensureResultRBAC(ctx, fc, run, step, "sa", "op"); err != nil {
			t.Fatalf("step %s: %v", step, err)
		}
		var role rbacv1.Role
		if err := fc.Get(ctx, types.NamespacedName{Name: resultRoleName("uid-r", step), Namespace: "op"}, &role); err != nil {
			t.Fatalf("step %s: Role not found: %v", step, err)
		}
		if len(role.Rules) != 2 {
			t.Fatalf("step %s: expected 2 rules, got %d", step, len(role.Rules))
		}
	}
}

func TestEnsureResultRBAC_UnknownStep(t *testing.T) {
	ctx := context.Background()
	fc := fake.NewClientBuilder().WithScheme(testScheme()).Build()
	run := &agenticv1alpha1.AgenticRun{
		ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "ns", UID: "uid-r"},
	}
	err := ensureResultRBAC(ctx, fc, run, "bogus", "sa", "op")
	if err == nil || !strings.Contains(err.Error(), "unknown step") {
		t.Fatalf("expected unknown step error, got: %v", err)
	}
}

func TestEnsureResultRBAC_Idempotent(t *testing.T) {
	ctx := context.Background()
	fc := fake.NewClientBuilder().WithScheme(testScheme()).Build()
	run := &agenticv1alpha1.AgenticRun{
		ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "ns", UID: "uid-r"},
	}
	if err := ensureResultRBAC(ctx, fc, run, "analysis", "sa", "op"); err != nil {
		t.Fatalf("first call: %v", err)
	}
	if err := ensureResultRBAC(ctx, fc, run, "analysis", "sa", "op"); err != nil {
		t.Fatalf("second call (idempotent): %v", err)
	}
}

func TestSetResultRBACOwner(t *testing.T) {
	ctx := context.Background()
	fc := fake.NewClientBuilder().WithScheme(testScheme()).Build()
	run := &agenticv1alpha1.AgenticRun{
		ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "ns", UID: "uid-r"},
	}
	if err := ensureResultRBAC(ctx, fc, run, "execution", "sa", "op"); err != nil {
		t.Fatalf("setup: %v", err)
	}

	owner := metav1.OwnerReference{
		APIVersion: "v1", Kind: "Pod", Name: "my-pod", UID: "pod-uid",
	}
	if err := setResultRBACOwner(ctx, fc, "uid-r", "execution", owner, "op"); err != nil {
		t.Fatalf("setResultRBACOwner: %v", err)
	}

	roleName := resultRoleName("uid-r", "execution")
	var role rbacv1.Role
	if err := fc.Get(ctx, types.NamespacedName{Name: roleName, Namespace: "op"}, &role); err != nil {
		t.Fatalf("get Role: %v", err)
	}
	if len(role.OwnerReferences) != 1 || role.OwnerReferences[0].Name != "my-pod" {
		t.Fatalf("Role owner = %v", role.OwnerReferences)
	}

	var binding rbacv1.RoleBinding
	if err := fc.Get(ctx, types.NamespacedName{Name: roleName, Namespace: "op"}, &binding); err != nil {
		t.Fatalf("get RoleBinding: %v", err)
	}
	if len(binding.OwnerReferences) != 1 || binding.OwnerReferences[0].Name != "my-pod" {
		t.Fatalf("RoleBinding owner = %v", binding.OwnerReferences)
	}
}

// ---------------------------------------------------------------------------
// truncateK8sName
// ---------------------------------------------------------------------------

func TestTruncateK8sName(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"short", "ls-exec-fix-oom", "ls-exec-fix-oom"},
		{"exactly_63", strings.Repeat("a", 63), strings.Repeat("a", 63)},
		{"over_63", strings.Repeat("a", 70), strings.Repeat("a", 63)},
		{"trailing_dash_trimmed", strings.Repeat("a", 60) + "---" + strings.Repeat("b", 5), strings.Repeat("a", 60)},
		{"trailing_dot_trimmed", strings.Repeat("a", 60) + "..." + strings.Repeat("b", 5), strings.Repeat("a", 60)},
		{"trailing_underscore_trimmed", strings.Repeat("a", 60) + "___" + strings.Repeat("b", 5), strings.Repeat("a", 60)},
		{"trailing_mixed_trimmed", strings.Repeat("a", 58) + "-._.-" + strings.Repeat("b", 5), strings.Repeat("a", 58)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := truncateK8sName(tt.input)
			if len(got) > 63 {
				t.Fatalf("result exceeds 63 chars: %d", len(got))
			}
			if got != tt.want {
				t.Fatalf("truncateK8sName(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestTruncateK8sName_TrailingDashTrimmed(t *testing.T) {
	// 64 chars where truncation to 63 leaves a trailing dash
	input := strings.Repeat("a", 62) + "-x"
	got := truncateK8sName(input)
	if len(got) > 63 {
		t.Fatalf("exceeds 63: %d", len(got))
	}
	// Trailing dash is trimmed: "aaa...(62)-" → "aaa...(62)"
	if got != strings.Repeat("a", 62) {
		t.Fatalf("unexpected: %q (len %d)", got, len(got))
	}

	// Multiple trailing dashes after truncation
	input2 := strings.Repeat("a", 60) + "----"
	got2 := truncateK8sName(input2)
	if strings.HasSuffix(got2, "-") {
		t.Fatalf("trailing dash not trimmed: %q", got2)
	}
	if got2 != strings.Repeat("a", 60) {
		t.Fatalf("unexpected: %q", got2)
	}
}

// ---------------------------------------------------------------------------
// rbacTargetNamespaces
// ---------------------------------------------------------------------------

func TestRBACTargetNamespaces(t *testing.T) {
	ns1 := "ns-alpha"
	ns2 := "ns-beta"

	t.Run("from_spec", func(t *testing.T) {
		run := &agenticv1alpha1.AgenticRun{
			Spec: agenticv1alpha1.AgenticRunSpec{TargetNamespaces: []string{"prod", "staging"}},
		}
		got := rbacTargetNamespaces(run, &agenticv1alpha1.RBACResult{
			NamespaceScoped: []agenticv1alpha1.RBACRule{{Namespace: ns1}},
		})
		if len(got) != 2 || got[0] != "prod" || got[1] != "staging" {
			t.Fatalf("spec namespaces should take precedence: %v", got)
		}
	})

	t.Run("from_rbac_rules", func(t *testing.T) {
		run := &agenticv1alpha1.AgenticRun{}
		got := rbacTargetNamespaces(run, &agenticv1alpha1.RBACResult{
			NamespaceScoped: []agenticv1alpha1.RBACRule{
				{Namespace: ns1},
				{Namespace: ns2},
			},
		})
		if len(got) != 2 || got[0] != ns1 || got[1] != ns2 {
			t.Fatalf("should extract from rules: %v", got)
		}
	})

	t.Run("dedup", func(t *testing.T) {
		run := &agenticv1alpha1.AgenticRun{}
		got := rbacTargetNamespaces(run, &agenticv1alpha1.RBACResult{
			NamespaceScoped: []agenticv1alpha1.RBACRule{
				{Namespace: ns1},
				{Namespace: ns1},
				{Namespace: ns2},
			},
		})
		if len(got) != 2 {
			t.Fatalf("should dedup: got %v", got)
		}
	})

	t.Run("nil_rbac", func(t *testing.T) {
		run := &agenticv1alpha1.AgenticRun{}
		got := rbacTargetNamespaces(run, nil)
		if got != nil {
			t.Fatalf("should be nil for nil rbac: %v", got)
		}
	})

	t.Run("nil_namespace_in_rule", func(t *testing.T) {
		run := &agenticv1alpha1.AgenticRun{}
		got := rbacTargetNamespaces(run, &agenticv1alpha1.RBACResult{
			NamespaceScoped: []agenticv1alpha1.RBACRule{
				{APIGroups: []string{""}, Resources: []string{"pods"}, Verbs: []string{"get"}},
			},
		})
		if len(got) != 0 {
			t.Fatalf("rules with nil namespace should produce no namespaces: %v", got)
		}
	})

	t.Run("empty_namespace_in_rule", func(t *testing.T) {
		empty := ""
		run := &agenticv1alpha1.AgenticRun{}
		got := rbacTargetNamespaces(run, &agenticv1alpha1.RBACResult{
			NamespaceScoped: []agenticv1alpha1.RBACRule{
				{Namespace: empty},
			},
		})
		if len(got) != 0 {
			t.Fatalf("empty namespace should be skipped: %v", got)
		}
	})
}

// ---------------------------------------------------------------------------
// rbacRulesToPolicyRules
// ---------------------------------------------------------------------------

func TestRBACRulesToPolicyRules(t *testing.T) {
	t.Run("converts_all_fields", func(t *testing.T) {
		rules := []agenticv1alpha1.RBACRule{{
			APIGroups:     []string{"apps", "core"},
			Resources:     []string{"deployments", "pods"},
			ResourceNames: []string{"web-frontend"},
			Verbs:         []string{"get", "patch", "delete"},
			Justification: "should be stripped",
		}}
		got := rbacRulesToPolicyRules(rules)
		if len(got) != 1 {
			t.Fatalf("expected 1 rule, got %d", len(got))
		}
		r := got[0]
		if len(r.APIGroups) != 2 || r.APIGroups[0] != "apps" || r.APIGroups[1] != "" {
			t.Fatalf("APIGroups: %v, want [apps, \"\"] (core mapped to empty)", r.APIGroups)
		}
		if len(r.Resources) != 2 {
			t.Fatalf("Resources: %v", r.Resources)
		}
		if len(r.ResourceNames) != 1 || r.ResourceNames[0] != "web-frontend" {
			t.Fatalf("ResourceNames: %v", r.ResourceNames)
		}
		if len(r.Verbs) != 3 {
			t.Fatalf("Verbs: %v", r.Verbs)
		}
	})

	t.Run("empty_input", func(t *testing.T) {
		got := rbacRulesToPolicyRules(nil)
		if len(got) != 0 {
			t.Fatalf("expected empty, got %d", len(got))
		}
	})

	t.Run("multiple_rules", func(t *testing.T) {
		rules := []agenticv1alpha1.RBACRule{
			{APIGroups: []string{"apps"}, Resources: []string{"deployments"}, Verbs: []string{"get"}},
			{APIGroups: []string{"core"}, Resources: []string{"pods"}, Verbs: []string{"delete"}},
		}
		got := rbacRulesToPolicyRules(rules)
		if len(got) != 2 {
			t.Fatalf("expected 2 rules, got %d", len(got))
		}
		if got[1].APIGroups[0] != "" {
			t.Errorf("core should be mapped to empty string, got %q", got[1].APIGroups[0])
		}
	})

	t.Run("core_api_group_normalization", func(t *testing.T) {
		rules := []agenticv1alpha1.RBACRule{
			{APIGroups: []string{"core"}, Resources: []string{"pods"}, Verbs: []string{"get"}},
			{APIGroups: []string{"apps"}, Resources: []string{"deployments"}, Verbs: []string{"get"}},
			{APIGroups: []string{"core", "batch"}, Resources: []string{"pods", "jobs"}, Verbs: []string{"list"}},
		}
		got := rbacRulesToPolicyRules(rules)
		if got[0].APIGroups[0] != "" {
			t.Errorf("rule[0] core → \"\", got %q", got[0].APIGroups[0])
		}
		if got[1].APIGroups[0] != "apps" {
			t.Errorf("rule[1] apps should be unchanged, got %q", got[1].APIGroups[0])
		}
		if got[2].APIGroups[0] != "" || got[2].APIGroups[1] != "batch" {
			t.Errorf("rule[2] got %v, want [\"\", \"batch\"]", got[2].APIGroups)
		}
	})
}

// ---------------------------------------------------------------------------
// annotatedRBACNamespaces
// ---------------------------------------------------------------------------

func TestAnnotatedRBACNamespaces(t *testing.T) {
	t.Run("nil_annotations", func(t *testing.T) {
		p := &agenticv1alpha1.AgenticRun{ObjectMeta: metav1.ObjectMeta{Name: "test"}}
		got := annotatedRBACNamespaces(p)
		if got != nil {
			t.Fatalf("expected nil, got %v", got)
		}
	})

	t.Run("empty_value", func(t *testing.T) {
		p := &agenticv1alpha1.AgenticRun{
			ObjectMeta: metav1.ObjectMeta{
				Name:        "test",
				Annotations: map[string]string{rbacNamespacesAnnotation: ""},
			},
		}
		got := annotatedRBACNamespaces(p)
		if got != nil {
			t.Fatalf("expected nil for empty, got %v", got)
		}
	})

	t.Run("single_namespace", func(t *testing.T) {
		p := &agenticv1alpha1.AgenticRun{
			ObjectMeta: metav1.ObjectMeta{
				Name:        "test",
				Annotations: map[string]string{rbacNamespacesAnnotation: "production"},
			},
		}
		got := annotatedRBACNamespaces(p)
		if len(got) != 1 || got[0] != "production" {
			t.Fatalf("expected [production], got %v", got)
		}
	})

	t.Run("multiple_namespaces", func(t *testing.T) {
		p := &agenticv1alpha1.AgenticRun{
			ObjectMeta: metav1.ObjectMeta{
				Name:        "test",
				Annotations: map[string]string{rbacNamespacesAnnotation: "ns-a,ns-b,ns-c"},
			},
		}
		got := annotatedRBACNamespaces(p)
		if len(got) != 3 || got[0] != "ns-a" || got[2] != "ns-c" {
			t.Fatalf("expected [ns-a ns-b ns-c], got %v", got)
		}
	})
}

// ---------------------------------------------------------------------------
// Role name generators
// ---------------------------------------------------------------------------

func TestRoleNameGenerators(t *testing.T) {
	t.Run("executionRoleName", func(t *testing.T) {
		got := executionRoleName("fix-oom")
		if got != "ls-exec-fix-oom" {
			t.Fatalf("expected ls-exec-fix-oom, got %s", got)
		}
	})

	t.Run("clusterRoleName", func(t *testing.T) {
		got := clusterRoleName("fix-oom")
		if got != "ls-exec-cluster-fix-oom" {
			t.Fatalf("expected ls-exec-cluster-fix-oom, got %s", got)
		}
	})

	t.Run("executionRoleName_long", func(t *testing.T) {
		longName := strings.Repeat("x", 60)
		got := executionRoleName(longName)
		if len(got) > 63 {
			t.Fatalf("exceeds 63 chars: %d", len(got))
		}
		if !strings.HasPrefix(got, "ls-exec-") {
			t.Fatalf("missing prefix: %s", got)
		}
	})

	t.Run("clusterRoleName_long", func(t *testing.T) {
		longName := strings.Repeat("y", 60)
		got := clusterRoleName(longName)
		if len(got) > 63 {
			t.Fatalf("exceeds 63 chars: %d", len(got))
		}
		if !strings.HasPrefix(got, "ls-exec-cluster-") {
			t.Fatalf("missing prefix: %s", got)
		}
	})
}

// ---------------------------------------------------------------------------
// rbacLabels
// ---------------------------------------------------------------------------

func TestRBACLabels(t *testing.T) {
	uid := "a1b2c3d4-e5f6-7890-1234-567890abcdef"
	labels := rbacLabels(uid, "execution-rbac")
	if labels[LabelRun] != uid {
		t.Fatalf("run label: %s", labels[LabelRun])
	}
	if labels[LabelComponent] != "execution-rbac" {
		t.Fatalf("component label: %s", labels[LabelComponent])
	}
	if len(labels) != 2 {
		t.Fatalf("expected 2 labels, got %d", len(labels))
	}
}

// ---------------------------------------------------------------------------
// addReaderSubject / removeReaderSubject / resolveReaderBindings
// ---------------------------------------------------------------------------

func TestAddReaderSubject_ConcurrentRunsUseIndependentBindings(t *testing.T) {
	ctx := context.Background()
	resetReaderBindings()
	fc := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(readerBinding()).Build()

	const runs = 10
	errs := make(chan error, runs)
	var wg sync.WaitGroup
	for i := 0; i < runs; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- addReaderSubject(ctx, fc, fmt.Sprintf("uid-%02d", i), "analysis", fmt.Sprintf("ls-anl-uid-%02d", i), "default")
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent reader binding creation failed: %v", err)
		}
	}

	var shared rbacv1.ClusterRoleBinding
	if err := fc.Get(ctx, types.NamespacedName{Name: defaultReaderClusterRoleBinding}, &shared); err != nil {
		t.Fatalf("get shared binding: %v", err)
	}
	if len(shared.Subjects) != 1 || shared.Subjects[0].Name != defaultSandboxSA {
		t.Fatalf("shared binding was modified: %+v", shared.Subjects)
	}
}

func TestAddReaderSubject_Idempotent(t *testing.T) {
	ctx := context.Background()
	resetReaderBindings()
	fc := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(readerBinding()).Build()

	if err := addReaderSubject(ctx, fc, "uid-test", "execution", "ls-exec-test", "default"); err != nil {
		t.Fatalf("first add: %v", err)
	}
	if err := addReaderSubject(ctx, fc, "uid-test", "execution", "ls-exec-test", "default"); err != nil {
		t.Fatalf("second add: %v", err)
	}

	var shared rbacv1.ClusterRoleBinding
	if err := fc.Get(ctx, types.NamespacedName{Name: defaultReaderClusterRoleBinding}, &shared); err != nil {
		t.Fatalf("get shared binding: %v", err)
	}
	if len(shared.Subjects) != 1 || shared.Subjects[0].Name != defaultSandboxSA {
		t.Fatalf("shared binding was modified: %+v", shared.Subjects)
	}
	var binding rbacv1.ClusterRoleBinding
	if err := fc.Get(ctx, types.NamespacedName{Name: readerBindingName("uid-test", "execution", defaultReaderClusterRoleBinding)}, &binding); err != nil {
		t.Fatalf("get per-run binding: %v", err)
	}
	if len(binding.Subjects) != 1 || binding.Subjects[0].Name != "ls-exec-test" {
		t.Fatalf("unexpected per-run subjects: %+v", binding.Subjects)
	}
}

func TestAddReaderSubject_MultipleBindings(t *testing.T) {
	ctx := context.Background()
	resetReaderBindings()

	monitoringBinding := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "lightspeed-agent-monitoring-view"},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "cluster-monitoring-view"},
		Subjects: []rbacv1.Subject{{
			Kind:      rbacv1.ServiceAccountKind,
			Name:      defaultSandboxSA,
			Namespace: "default",
		}},
	}
	fc := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(readerBinding(), monitoringBinding).Build()

	if err := addReaderSubject(ctx, fc, "uid-test", "execution", "ls-exec-test", "default"); err != nil {
		t.Fatalf("addReaderSubject: %v", err)
	}

	for _, sourceName := range []string{defaultReaderClusterRoleBinding, "lightspeed-agent-monitoring-view"} {
		var source rbacv1.ClusterRoleBinding
		if err := fc.Get(ctx, types.NamespacedName{Name: sourceName}, &source); err != nil {
			t.Fatalf("get %s: %v", sourceName, err)
		}
		if len(source.Subjects) != 1 || source.Subjects[0].Name != defaultSandboxSA {
			t.Fatalf("shared binding %s was modified: %+v", sourceName, source.Subjects)
		}
		var binding rbacv1.ClusterRoleBinding
		name := readerBindingName("uid-test", "execution", sourceName)
		if err := fc.Get(ctx, types.NamespacedName{Name: name}, &binding); err != nil {
			t.Fatalf("get per-run binding %s: %v", name, err)
		}
		if len(binding.Subjects) != 1 || binding.Subjects[0].Name != "ls-exec-test" {
			t.Fatalf("unexpected subjects in %s: %+v", name, binding.Subjects)
		}
	}
}

func TestAddReaderSubject_CleansUpAfterPartialFailure(t *testing.T) {
	ctx := context.Background()
	resetReaderBindings()

	monitoringBinding := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "lightspeed-agent-monitoring-view"},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "cluster-monitoring-view"},
		Subjects:   []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: defaultSandboxSA, Namespace: "default"}},
	}
	conflictingBinding := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: readerBindingName("uid-partial-failure", "execution", monitoringBinding.Name)},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "unexpected"},
	}
	fc := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(readerBinding(), monitoringBinding, conflictingBinding).Build()

	err := addReaderSubject(ctx, fc, "uid-partial-failure", "execution", "ls-exec-partial-failure", "default")
	if err == nil {
		t.Fatal("expected second binding creation to fail")
	}

	firstName := readerBindingName("uid-partial-failure", "execution", defaultReaderClusterRoleBinding)
	var firstBinding rbacv1.ClusterRoleBinding
	if getErr := fc.Get(ctx, types.NamespacedName{Name: firstName}, &firstBinding); !apierrors.IsNotFound(getErr) {
		t.Fatalf("partially created binding should be cleaned up, got error: %v", getErr)
	}

	var shared rbacv1.ClusterRoleBinding
	if getErr := fc.Get(ctx, types.NamespacedName{Name: defaultReaderClusterRoleBinding}, &shared); getErr != nil {
		t.Fatalf("get shared reader binding: %v", getErr)
	}
	if len(shared.Subjects) != 1 || shared.Subjects[0].Name != defaultSandboxSA {
		t.Fatalf("shared reader binding was modified: %+v", shared.Subjects)
	}
}

func TestAddReaderSubject_CleansUpWithCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	resetReaderBindings()

	monitoringBinding := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "lightspeed-agent-monitoring-view"},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "cluster-monitoring-view"},
		Subjects:   []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: defaultSandboxSA, Namespace: "default"}},
	}
	conflictingBinding := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: readerBindingName("uid-cancelled-cleanup", "execution", monitoringBinding.Name)},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "unexpected"},
	}
	fc := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(readerBinding(), monitoringBinding, conflictingBinding).Build()

	if err := addReaderSubject(ctx, fc, "uid-cancelled-cleanup", "execution", "ls-exec-cancelled-cleanup", "default"); err == nil {
		t.Fatal("expected second binding creation to fail")
	}

	firstName := readerBindingName("uid-cancelled-cleanup", "execution", defaultReaderClusterRoleBinding)
	var firstBinding rbacv1.ClusterRoleBinding
	if getErr := fc.Get(context.Background(), types.NamespacedName{Name: firstName}, &firstBinding); !apierrors.IsNotFound(getErr) {
		t.Fatalf("partially created binding should be cleaned up with cancelled context, got error: %v", getErr)
	}
}

func TestRemoveReaderSubject_NotPresent(t *testing.T) {
	ctx := context.Background()
	resetReaderBindings()
	fc := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(readerBinding()).Build()

	if err := removeReaderSubject(ctx, fc, "uid-nonexistent", "execution", "default"); err != nil {
		t.Fatalf("remove non-existent subject should no-op, got: %v", err)
	}
}

func TestRemoveReaderSubject_MultipleBindings(t *testing.T) {
	ctx := context.Background()
	resetReaderBindings()

	saName := "ls-exec-remove-test"
	monitoringBinding := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "lightspeed-agent-monitoring-view"},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "cluster-monitoring-view"},
		Subjects:   []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: defaultSandboxSA, Namespace: "default"}},
	}
	fc := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(readerBinding(), monitoringBinding).Build()

	if err := addReaderSubject(ctx, fc, "uid-remove-test", "execution", saName, "default"); err != nil {
		t.Fatalf("addReaderSubject: %v", err)
	}
	if err := removeReaderSubject(ctx, fc, "uid-remove-test", "execution", "default"); err != nil {
		t.Fatalf("removeReaderSubject: %v", err)
	}

	for _, sourceName := range []string{defaultReaderClusterRoleBinding, "lightspeed-agent-monitoring-view"} {
		var crb rbacv1.ClusterRoleBinding
		if err := fc.Get(ctx, types.NamespacedName{Name: sourceName}, &crb); err != nil {
			t.Fatalf("get %s: %v", sourceName, err)
		}
		if len(crb.Subjects) != 1 || crb.Subjects[0].Name != defaultSandboxSA {
			t.Fatalf("shared binding %s was modified: %+v", sourceName, crb.Subjects)
		}
		var binding rbacv1.ClusterRoleBinding
		if err := fc.Get(ctx, types.NamespacedName{Name: readerBindingName("uid-remove-test", "execution", sourceName)}, &binding); !apierrors.IsNotFound(err) {
			t.Fatalf("per-run binding %s still exists: err=%v", sourceName, err)
		}
	}
}

func TestResolveReaderBindings_NoMatches(t *testing.T) {
	ctx := context.Background()
	resetReaderBindings()

	unrelatedBinding := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "unrelated-binding"},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "admin"},
		Subjects: []rbacv1.Subject{{
			Kind:      rbacv1.ServiceAccountKind,
			Name:      "other-sa",
			Namespace: "other-ns",
		}},
	}
	fc := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(unrelatedBinding).Build()

	_, err := resolveReaderBindings(ctx, fc, "default")
	if err == nil {
		t.Fatal("expected error when no matching bindings found")
	}
	if !strings.Contains(err.Error(), "no ClusterRoleBinding found") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestResolveReaderBindings_MultipleMatches(t *testing.T) {
	ctx := context.Background()
	resetReaderBindings()

	binding1 := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "custom-reader-1"},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "cluster-reader"},
		Subjects: []rbacv1.Subject{{
			Kind:      rbacv1.ServiceAccountKind,
			Name:      defaultSandboxSA,
			Namespace: "default",
		}},
	}
	binding2 := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "custom-reader-2"},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "cluster-monitoring-view"},
		Subjects: []rbacv1.Subject{{
			Kind:      rbacv1.ServiceAccountKind,
			Name:      defaultSandboxSA,
			Namespace: "default",
		}},
	}
	fc := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(binding1, binding2).Build()

	resolved, err := resolveReaderBindings(ctx, fc, "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(resolved) != 2 {
		t.Fatalf("expected 2 bindings, got %d: %v", len(resolved), resolved)
	}

	found := map[string]bool{}
	for _, name := range resolved {
		found[name] = true
	}
	if !found["custom-reader-1"] || !found["custom-reader-2"] {
		t.Fatalf("expected both custom bindings, got: %v", resolved)
	}
}

func TestAddReaderSubject_RefreshesStaleReaderCache(t *testing.T) {
	ctx := context.Background()
	resetReaderBindings()

	oldBinding := readerBinding()
	fc := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(oldBinding).Build()
	if _, err := resolveReaderBindings(ctx, fc, "default"); err != nil {
		t.Fatalf("initial resolve: %v", err)
	}
	if err := fc.Delete(ctx, oldBinding); err != nil {
		t.Fatalf("delete stale binding: %v", err)
	}
	replacement := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "replacement-reader"},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "replacement-reader-role"},
		Subjects:   []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: defaultSandboxSA, Namespace: "default"}},
	}
	if err := fc.Create(ctx, replacement); err != nil {
		t.Fatalf("create replacement binding: %v", err)
	}
	if err := addReaderSubject(ctx, fc, "uid-refresh-cache", "analysis", "ls-anl-refresh-cache", "default"); err != nil {
		t.Fatalf("addReaderSubject should refresh stale cache: %v", err)
	}
	var perRun rbacv1.ClusterRoleBinding
	if err := fc.Get(ctx, types.NamespacedName{Name: readerBindingName("uid-refresh-cache", "analysis", replacement.Name)}, &perRun); err != nil {
		t.Fatalf("get replacement per-run binding: %v", err)
	}
	if perRun.RoleRef.Name != replacement.RoleRef.Name {
		t.Fatalf("per-run binding used role %q, want %q", perRun.RoleRef.Name, replacement.RoleRef.Name)
	}
}

func TestReaderBindingReferencesServiceAccount(t *testing.T) {
	relevant := readerBinding()
	unrelated := relevant.DeepCopy()
	unrelated.Subjects = []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: "other-sa", Namespace: "default"}}
	deleted := toolscache.DeletedFinalStateUnknown{Key: relevant.Name, Obj: relevant}

	tests := []struct {
		name string
		obj  interface{}
		want bool
	}{
		{name: "relevant binding", obj: relevant, want: true},
		{name: "unrelated binding", obj: unrelated, want: false},
		{name: "value tombstone", obj: deleted, want: true},
		{name: "pointer tombstone", obj: &deleted, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := readerBindingReferencesServiceAccount(tt.obj, "default"); got != tt.want {
				t.Fatalf("readerBindingReferencesServiceAccount() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestInvalidateReaderBindings(t *testing.T) {
	readerBindings.Store([]string{"cached-reader"})
	invalidateReaderBindings()
	if got := readerBindings.Load().([]string); len(got) != 0 {
		t.Fatalf("cache was not invalidated: %v", got)
	}
}

func TestReaderBindingEventHandlersInvalidateCache(t *testing.T) {
	relevant := readerBinding()
	unrelated := relevant.DeepCopy()
	unrelated.Subjects = []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: "other-sa", Namespace: "default"}}
	handlers := readerBindingEventHandlers("default")

	t.Run("unrelated add does not invalidate", func(t *testing.T) {
		readerBindings.Store([]string{"cached-reader"})
		handlers.AddFunc(unrelated)
		if got := readerBindings.Load().([]string); len(got) != 1 {
			t.Fatalf("unrelated add invalidated cache: %v", got)
		}
	})
	t.Run("relevant update invalidates", func(t *testing.T) {
		readerBindings.Store([]string{"cached-reader"})
		handlers.UpdateFunc(relevant, unrelated)
		if got := readerBindings.Load().([]string); len(got) != 0 {
			t.Fatalf("relevant update did not invalidate cache: %v", got)
		}
	})
	t.Run("relevant delete tombstone invalidates", func(t *testing.T) {
		readerBindings.Store([]string{"cached-reader"})
		handlers.DeleteFunc(toolscache.DeletedFinalStateUnknown{Key: relevant.Name, Obj: relevant})
		if got := readerBindings.Load().([]string); len(got) != 0 {
			t.Fatalf("relevant delete did not invalidate cache: %v", got)
		}
	})
}

func TestResolveReaderBindings_Cached(t *testing.T) {
	ctx := context.Background()
	resetReaderBindings()
	fc := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(readerBinding()).Build()

	first, err := resolveReaderBindings(ctx, fc, "default")
	if err != nil {
		t.Fatalf("first resolve: %v", err)
	}
	second, err := resolveReaderBindings(ctx, fc, "default")
	if err != nil {
		t.Fatalf("second resolve: %v", err)
	}
	if len(first) != len(second) || first[0] != second[0] {
		t.Fatalf("cached result should match: %v vs %v", first, second)
	}
}

// ---------------------------------------------------------------------------
// addReaderSubjectOnSpoke / removeReaderSubjectOnSpoke
// ---------------------------------------------------------------------------

func spokeReaderBindings() []*rbacv1.ClusterRoleBinding {
	return []*rbacv1.ClusterRoleBinding{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "lightspeed-hub:cluster-reader"},
			RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "cluster-reader"},
			Subjects: []rbacv1.Subject{{
				Kind: rbacv1.ServiceAccountKind, Name: "lightspeed-agent", Namespace: "openshift-lightspeed-managed",
			}},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "lightspeed-hub:cluster-monitoring-view"},
			RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "cluster-monitoring-view"},
			Subjects: []rbacv1.Subject{{
				Kind: rbacv1.ServiceAccountKind, Name: "lightspeed-agent", Namespace: "openshift-lightspeed-managed",
			}},
		},
	}
}

func TestAddReaderSubjectOnSpoke_CreatesPerRunCRBs(t *testing.T) {
	ctx := context.Background()
	bindings := spokeReaderBindings()
	fc := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(bindings[0], bindings[1]).Build()

	runUID := "uid-spoke-1"
	step := "analysis"
	extraLabels := map[string]string{LabelSpokeCluster: "prod-spoke", LabelAgenticRun: "my-run"}

	if err := addReaderSubjectOnSpoke(ctx, fc, runUID, step, "ls-anl-uid-spoke-1", spokeManagedNamespace, extraLabels); err != nil {
		t.Fatalf("addReaderSubjectOnSpoke: %v", err)
	}

	// Verify per-run CRBs created, one per source.
	for i, sourceName := range spokeReaderBindingNames {
		crbName := perRunCRBName(runUID, step, i)
		var crb rbacv1.ClusterRoleBinding
		if err := fc.Get(ctx, types.NamespacedName{Name: crbName}, &crb); err != nil {
			t.Fatalf("per-run CRB %s not found: %v", crbName, err)
		}
		// Single subject.
		if len(crb.Subjects) != 1 {
			t.Fatalf("CRB %s: expected 1 subject, got %d", crbName, len(crb.Subjects))
		}
		if crb.Subjects[0].Name != "ls-anl-uid-spoke-1" {
			t.Fatalf("CRB %s: subject = %q, want ls-anl-uid-spoke-1", crbName, crb.Subjects[0].Name)
		}
		if crb.Subjects[0].Namespace != spokeManagedNamespace {
			t.Fatalf("CRB %s: subject ns = %q, want %q", crbName, crb.Subjects[0].Namespace, spokeManagedNamespace)
		}
		// RoleRef copied from source.
		var source rbacv1.ClusterRoleBinding
		if err := fc.Get(ctx, types.NamespacedName{Name: sourceName}, &source); err != nil {
			t.Fatalf("get source %s: %v", sourceName, err)
		}
		if crb.RoleRef != source.RoleRef {
			t.Fatalf("CRB %s: RoleRef mismatch: got %+v, want %+v", crbName, crb.RoleRef, source.RoleRef)
		}
		// Labels include run + component + spoke extras.
		if crb.Labels[LabelRun] != runUID {
			t.Errorf("CRB %s: run label = %q, want %q", crbName, crb.Labels[LabelRun], runUID)
		}
		if crb.Labels[LabelComponent] != "reader-rbac" {
			t.Errorf("CRB %s: component label = %q, want reader-rbac", crbName, crb.Labels[LabelComponent])
		}
		if crb.Labels[LabelSpokeCluster] != "prod-spoke" {
			t.Errorf("CRB %s: spoke-cluster label = %q, want prod-spoke", crbName, crb.Labels[LabelSpokeCluster])
		}
	}

	// Source CRBs must NOT be modified.
	for _, sourceName := range spokeReaderBindingNames {
		var source rbacv1.ClusterRoleBinding
		if err := fc.Get(ctx, types.NamespacedName{Name: sourceName}, &source); err != nil {
			t.Fatalf("get source %s: %v", sourceName, err)
		}
		for _, s := range source.Subjects {
			if s.Name == "ls-anl-uid-spoke-1" {
				t.Fatalf("source CRB %s was modified — subject added", sourceName)
			}
		}
	}
}

func TestAddReaderSubjectOnSpoke_Idempotent(t *testing.T) {
	ctx := context.Background()
	bindings := spokeReaderBindings()
	fc := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(bindings[0], bindings[1]).Build()

	extraLabels := map[string]string{LabelSpokeCluster: "s"}
	if err := addReaderSubjectOnSpoke(ctx, fc, "uid1", "analysis", "sa1", spokeManagedNamespace, extraLabels); err != nil {
		t.Fatalf("first call: %v", err)
	}
	if err := addReaderSubjectOnSpoke(ctx, fc, "uid1", "analysis", "sa1", spokeManagedNamespace, extraLabels); err != nil {
		t.Fatalf("second call (idempotent): %v", err)
	}
}

func TestAddReaderSubjectOnSpoke_DoesNotPolluteHubCache(t *testing.T) {
	ctx := context.Background()
	resetReaderBindings()

	// Set up a hub client with hub CRBs.
	hubFC := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(readerBinding()).Build()
	_, err := resolveReaderBindings(ctx, hubFC, "default")
	if err != nil {
		t.Fatalf("hub resolve: %v", err)
	}
	cachedBefore := readerBindings.Load().([]string)

	// Set up a spoke client with spoke CRBs.
	bindings := spokeReaderBindings()
	spokeFC := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(bindings[0], bindings[1]).Build()
	if err := addReaderSubjectOnSpoke(ctx, spokeFC, "uid1", "analysis", "ls-anl-uid1", spokeManagedNamespace, nil); err != nil {
		t.Fatalf("spoke add: %v", err)
	}

	// Verify hub cache was not changed.
	cachedAfter := readerBindings.Load().([]string)
	if len(cachedBefore) != len(cachedAfter) {
		t.Fatalf("hub cache was modified: before=%v after=%v", cachedBefore, cachedAfter)
	}
	for i := range cachedBefore {
		if cachedBefore[i] != cachedAfter[i] {
			t.Fatalf("hub cache entry changed: %q → %q", cachedBefore[i], cachedAfter[i])
		}
	}
}

func TestAddReaderSubjectOnSpoke_SourceCRBNotFound(t *testing.T) {
	ctx := context.Background()
	// No source CRBs exist.
	fc := fake.NewClientBuilder().WithScheme(testScheme()).Build()

	err := addReaderSubjectOnSpoke(ctx, fc, "uid1", "analysis", "sa1", spokeManagedNamespace, nil)
	if err == nil {
		t.Fatal("expected error when source CRB is missing")
	}
	if !strings.Contains(err.Error(), ErrAddReaderSubject) {
		t.Fatalf("error should contain %q, got: %v", ErrAddReaderSubject, err)
	}
}

func TestAddReaderSubjectOnSpoke_RollbackOnPartialFailure(t *testing.T) {
	ctx := context.Background()
	bindings := spokeReaderBindings()
	// Only first source CRB exists — second is missing, which triggers rollback.
	fc := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(bindings[0]).Build()

	err := addReaderSubjectOnSpoke(ctx, fc, "uid1", "analysis", "sa1", spokeManagedNamespace, nil)
	if err == nil {
		t.Fatal("expected error when second source CRB is missing")
	}

	// First per-run CRB should have been rolled back.
	crbName := perRunCRBName("uid1", "analysis", 0)
	var crb rbacv1.ClusterRoleBinding
	if err := fc.Get(ctx, types.NamespacedName{Name: crbName}, &crb); err == nil {
		t.Fatalf("per-run CRB %s should have been cleaned up on rollback", crbName)
	}
}

func TestRemoveReaderSubjectOnSpoke_DeletesPerRunCRBs(t *testing.T) {
	ctx := context.Background()
	bindings := spokeReaderBindings()
	fc := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(bindings[0], bindings[1]).Build()

	// Create per-run CRBs first.
	if err := addReaderSubjectOnSpoke(ctx, fc, "uid1", "analysis", "sa1", spokeManagedNamespace, nil); err != nil {
		t.Fatalf("setup: %v", err)
	}

	// Remove them.
	if err := removeReaderSubjectOnSpoke(ctx, fc, "uid1", "analysis"); err != nil {
		t.Fatalf("removeReaderSubjectOnSpoke: %v", err)
	}

	// Per-run CRBs should be gone.
	for i := range spokeReaderBindingNames {
		crbName := perRunCRBName("uid1", "analysis", i)
		var crb rbacv1.ClusterRoleBinding
		if err := fc.Get(ctx, types.NamespacedName{Name: crbName}, &crb); err == nil {
			t.Fatalf("per-run CRB %s should be deleted", crbName)
		}
	}

	// Source CRBs should still exist.
	for _, name := range spokeReaderBindingNames {
		var crb rbacv1.ClusterRoleBinding
		if err := fc.Get(ctx, types.NamespacedName{Name: name}, &crb); err != nil {
			t.Fatalf("source CRB %s should still exist: %v", name, err)
		}
	}
}

func TestRemoveReaderSubjectOnSpoke_IdempotentWhenGone(t *testing.T) {
	ctx := context.Background()
	// No per-run CRBs exist.
	fc := fake.NewClientBuilder().WithScheme(testScheme()).Build()

	// Should not error — NotFound is tolerated.
	if err := removeReaderSubjectOnSpoke(ctx, fc, "uid1", "analysis"); err != nil {
		t.Fatalf("expected no error when CRBs are gone, got: %v", err)
	}
}

func TestPerRunCRBName(t *testing.T) {
	name := perRunCRBName("abc-123", "analysis", 0)
	if name != "ls-reader-anl-abc-123-0" {
		t.Fatalf("perRunCRBName = %q, want ls-reader-anl-abc-123-0", name)
	}
	// Long UID should be truncated.
	longUID := strings.Repeat("x", 60)
	longName := perRunCRBName(longUID, "execution", 1)
	if len(longName) > 63 {
		t.Fatalf("name exceeds 63 chars: %d", len(longName))
	}
}

func TestAddReaderSubject_DoesNotUpdateSharedBinding(t *testing.T) {
	ctx := context.Background()
	resetReaderBindings()

	callCount := 0
	fc := fake.NewClientBuilder().
		WithScheme(testScheme()).
		WithObjects(readerBinding()).
		WithInterceptorFuncs(interceptor.Funcs{
			Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				if _, ok := obj.(*rbacv1.ClusterRoleBinding); ok {
					callCount++
					return apierrors.NewConflict(
						schema.GroupResource{Group: "rbac.authorization.k8s.io", Resource: "clusterrolebindings"},
						defaultReaderClusterRoleBinding,
						fmt.Errorf("modified concurrently"),
					)
				}
				return c.Update(ctx, obj, opts...)
			},
		}).Build()

	err := addReaderSubject(ctx, fc, "uid-conflict-test", "execution", "ls-exec-conflict-test", "default")
	if err != nil {
		t.Fatalf("per-run binding creation should not update shared binding: %v", err)
	}
	if callCount != 0 {
		t.Fatalf("expected no shared binding updates, got %d", callCount)
	}
}

func TestEnsureExecutionRBAC_SpokeLabels(t *testing.T) {
	ctx := context.Background()
	fc := fake.NewClientBuilder().WithScheme(testScheme()).Build()

	run := &agenticv1alpha1.AgenticRun{
		ObjectMeta: metav1.ObjectMeta{Name: "spoke-run", Namespace: "ns", UID: "uid-spoke"},
		Spec: agenticv1alpha1.AgenticRunSpec{
			TargetNamespaces: []string{"prod"},
			TargetCluster:    "prod-spoke",
		},
	}
	rbacResult := &agenticv1alpha1.RBACResult{
		NamespaceScoped: []agenticv1alpha1.RBACRule{{
			APIGroups: []string{"apps"},
			Resources: []string{"deployments"},
			Verbs:     []string{"get"},
		}},
	}
	extraLabels := map[string]string{
		LabelSpokeCluster: "prod-spoke",
		LabelAgenticRun:   "spoke-run",
	}

	if err := ensureExecutionRBAC(ctx, fc, run, rbacResult, "default", extraLabels); err != nil {
		t.Fatalf("ensureExecutionRBAC: %v", err)
	}

	roleName := executionRoleName("uid-spoke")
	var role rbacv1.Role
	if err := fc.Get(ctx, types.NamespacedName{Name: roleName, Namespace: "prod"}, &role); err != nil {
		t.Fatalf("Role not found: %v", err)
	}
	if role.Labels[LabelSpokeCluster] != "prod-spoke" {
		t.Errorf("spoke-cluster label = %q, want %q", role.Labels[LabelSpokeCluster], "prod-spoke")
	}
	if role.Labels[LabelAgenticRun] != "spoke-run" {
		t.Errorf("agentic-run label = %q, want %q", role.Labels[LabelAgenticRun], "spoke-run")
	}
	if role.Labels[LabelRun] != "uid-spoke" {
		t.Errorf("run label = %q, want %q", role.Labels[LabelRun], "uid-spoke")
	}
}

func TestEnsureExecutionRBAC_NilExtraLabels(t *testing.T) {
	ctx := context.Background()
	fc := fake.NewClientBuilder().WithScheme(testScheme()).Build()

	run := &agenticv1alpha1.AgenticRun{
		ObjectMeta: metav1.ObjectMeta{Name: "hub-run", Namespace: "ns", UID: "uid-hub"},
		Spec: agenticv1alpha1.AgenticRunSpec{
			TargetNamespaces: []string{"prod"},
		},
	}
	rbacResult := &agenticv1alpha1.RBACResult{
		NamespaceScoped: []agenticv1alpha1.RBACRule{{
			APIGroups: []string{"apps"},
			Resources: []string{"deployments"},
			Verbs:     []string{"get"},
		}},
	}

	if err := ensureExecutionRBAC(ctx, fc, run, rbacResult, "default", nil); err != nil {
		t.Fatalf("ensureExecutionRBAC: %v", err)
	}

	roleName := executionRoleName("uid-hub")
	var role rbacv1.Role
	if err := fc.Get(ctx, types.NamespacedName{Name: roleName, Namespace: "prod"}, &role); err != nil {
		t.Fatalf("Role not found: %v", err)
	}
	if _, ok := role.Labels[LabelSpokeCluster]; ok {
		t.Error("hub-path Role should NOT have spoke-cluster label")
	}
	if _, ok := role.Labels[LabelAgenticRun]; ok {
		t.Error("hub-path Role should NOT have agentic-run label")
	}
}

func TestEnsureExecutionRBAC_SpokeLabels_ClusterScoped(t *testing.T) {
	ctx := context.Background()
	fc := fake.NewClientBuilder().WithScheme(testScheme()).Build()

	run := &agenticv1alpha1.AgenticRun{
		ObjectMeta: metav1.ObjectMeta{Name: "spoke-run-cs", Namespace: "ns", UID: "uid-spoke-cs"},
		Spec: agenticv1alpha1.AgenticRunSpec{
			TargetCluster: "prod-spoke",
		},
	}
	rbacResult := &agenticv1alpha1.RBACResult{
		ClusterScoped: []agenticv1alpha1.RBACRule{{
			APIGroups: []string{""},
			Resources: []string{"nodes"},
			Verbs:     []string{"get", "list"},
		}},
	}
	extraLabels := map[string]string{
		LabelSpokeCluster: "prod-spoke",
		LabelAgenticRun:   "spoke-run-cs",
	}

	if err := ensureExecutionRBAC(ctx, fc, run, rbacResult, "default", extraLabels); err != nil {
		t.Fatalf("ensureExecutionRBAC: %v", err)
	}

	crName := clusterRoleName("uid-spoke-cs")
	var cr rbacv1.ClusterRole
	if err := fc.Get(ctx, types.NamespacedName{Name: crName}, &cr); err != nil {
		t.Fatalf("ClusterRole not found: %v", err)
	}
	if cr.Labels[LabelSpokeCluster] != "prod-spoke" {
		t.Errorf("ClusterRole spoke-cluster label = %q, want %q", cr.Labels[LabelSpokeCluster], "prod-spoke")
	}
	if cr.Labels[LabelAgenticRun] != "spoke-run-cs" {
		t.Errorf("ClusterRole agentic-run label = %q, want %q", cr.Labels[LabelAgenticRun], "spoke-run-cs")
	}

	var crb rbacv1.ClusterRoleBinding
	if err := fc.Get(ctx, types.NamespacedName{Name: crName}, &crb); err != nil {
		t.Fatalf("ClusterRoleBinding not found: %v", err)
	}
	if crb.Labels[LabelSpokeCluster] != "prod-spoke" {
		t.Errorf("CRB spoke-cluster label = %q, want %q", crb.Labels[LabelSpokeCluster], "prod-spoke")
	}
	if crb.Labels[LabelAgenticRun] != "spoke-run-cs" {
		t.Errorf("CRB agentic-run label = %q, want %q", crb.Labels[LabelAgenticRun], "spoke-run-cs")
	}
}
