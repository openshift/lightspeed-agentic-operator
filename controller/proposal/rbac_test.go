package proposal

import (
	"context"
	"strings"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
)

// ---------------------------------------------------------------------------
// ensureExecutionRBAC
// ---------------------------------------------------------------------------

func TestEnsureExecutionRBAC_NamespaceScopedOnly(t *testing.T) {
	ctx := context.Background()
	fc := fake.NewClientBuilder().WithScheme(testScheme()).Build()

	proposal := &agenticv1alpha1.Proposal{
		ObjectMeta: metav1.ObjectMeta{Name: "fix-oom", Namespace: "default"},
		Spec:       agenticv1alpha1.ProposalSpec{TargetNamespaces: []string{"production"}},
	}
	rbacResult := &agenticv1alpha1.RBACResult{
		NamespaceScoped: []agenticv1alpha1.RBACRule{{
			APIGroups:     []string{"apps"},
			Resources:     []string{"deployments"},
			Verbs:         []string{"get", "patch"},
			Justification: "Patch deployment memory",
		}},
	}

	if err := ensureExecutionRBAC(ctx, fc, proposal, rbacResult, "lightspeed-agent", "default"); err != nil {
		t.Fatalf("ensureExecutionRBAC: %v", err)
	}

	roleName := executionRoleName("fix-oom")

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
	if role.Labels[LabelProposal] != "fix-oom" {
		t.Fatalf("missing proposal label")
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
	if binding.Subjects[0].Name != "lightspeed-agent" {
		t.Fatalf("unexpected subject: %s", binding.Subjects[0].Name)
	}
	if binding.Subjects[0].Namespace != "default" {
		t.Fatalf("subject namespace should be operator ns, got %s", binding.Subjects[0].Namespace)
	}
	if binding.RoleRef.Kind != "Role" || binding.RoleRef.Name != roleName {
		t.Fatalf("unexpected roleRef: %+v", binding.RoleRef)
	}

	// Verify annotation
	if proposal.Annotations[rbacNamespacesAnnotation] != "production" {
		t.Fatalf("expected rbac-namespaces annotation, got %q", proposal.Annotations[rbacNamespacesAnnotation])
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
	fc := fake.NewClientBuilder().WithScheme(testScheme()).Build()

	proposal := &agenticv1alpha1.Proposal{
		ObjectMeta: metav1.ObjectMeta{Name: "check-nodes", Namespace: "default"},
	}
	rbacResult := &agenticv1alpha1.RBACResult{
		ClusterScoped: []agenticv1alpha1.RBACRule{{
			APIGroups:     []string{""},
			Resources:     []string{"nodes"},
			Verbs:         []string{"get", "list"},
			Justification: "Read node status",
		}},
	}

	if err := ensureExecutionRBAC(ctx, fc, proposal, rbacResult, "lightspeed-agent", "default"); err != nil {
		t.Fatalf("ensureExecutionRBAC: %v", err)
	}

	crName := clusterRoleName("check-nodes")

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
	if crb.Subjects[0].Name != "lightspeed-agent" {
		t.Fatalf("unexpected subject: %s", crb.Subjects[0].Name)
	}
}

func TestEnsureExecutionRBAC_BothScopes(t *testing.T) {
	ctx := context.Background()
	fc := fake.NewClientBuilder().WithScheme(testScheme()).Build()

	proposal := &agenticv1alpha1.Proposal{
		ObjectMeta: metav1.ObjectMeta{Name: "full-fix", Namespace: "default"},
		Spec:       agenticv1alpha1.ProposalSpec{TargetNamespaces: []string{"staging"}},
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

	if err := ensureExecutionRBAC(ctx, fc, proposal, rbacResult, "lightspeed-agent", "default"); err != nil {
		t.Fatalf("ensureExecutionRBAC: %v", err)
	}

	// Role in staging
	var role rbacv1.Role
	if err := fc.Get(ctx, types.NamespacedName{Name: executionRoleName("full-fix"), Namespace: "staging"}, &role); err != nil {
		t.Fatalf("Role not found: %v", err)
	}

	// ClusterRole
	var cr rbacv1.ClusterRole
	if err := fc.Get(ctx, types.NamespacedName{Name: clusterRoleName("full-fix")}, &cr); err != nil {
		t.Fatalf("ClusterRole not found: %v", err)
	}
}

func TestEnsureExecutionRBAC_MultipleNamespaces(t *testing.T) {
	ctx := context.Background()
	fc := fake.NewClientBuilder().WithScheme(testScheme()).Build()

	proposal := &agenticv1alpha1.Proposal{
		ObjectMeta: metav1.ObjectMeta{Name: "multi-ns", Namespace: "default"},
		Spec:       agenticv1alpha1.ProposalSpec{TargetNamespaces: []string{"ns-a", "ns-b", "ns-c"}},
	}
	rbacResult := &agenticv1alpha1.RBACResult{
		NamespaceScoped: []agenticv1alpha1.RBACRule{{
			APIGroups: []string{""}, Resources: []string{"pods"},
			Verbs: []string{"get", "delete"}, Justification: "Restart pod",
		}},
	}

	if err := ensureExecutionRBAC(ctx, fc, proposal, rbacResult, "lightspeed-agent", "default"); err != nil {
		t.Fatalf("ensureExecutionRBAC: %v", err)
	}

	roleName := executionRoleName("multi-ns")
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
	got := proposal.Annotations[rbacNamespacesAnnotation]
	if got != "ns-a,ns-b,ns-c" {
		t.Fatalf("expected annotation 'ns-a,ns-b,ns-c', got %q", got)
	}
}

func TestEnsureExecutionRBAC_Idempotent(t *testing.T) {
	ctx := context.Background()
	fc := fake.NewClientBuilder().WithScheme(testScheme()).Build()

	proposal := &agenticv1alpha1.Proposal{
		ObjectMeta: metav1.ObjectMeta{Name: "idem", Namespace: "default"},
		Spec:       agenticv1alpha1.ProposalSpec{TargetNamespaces: []string{"prod"}},
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

	if err := ensureExecutionRBAC(ctx, fc, proposal, rbacResult, "lightspeed-agent", "default"); err != nil {
		t.Fatalf("first call: %v", err)
	}
	if err := ensureExecutionRBAC(ctx, fc, proposal, rbacResult, "lightspeed-agent", "default"); err != nil {
		t.Fatalf("idempotent second call should not error: %v", err)
	}
}

func TestEnsureExecutionRBAC_NilResult(t *testing.T) {
	ctx := context.Background()
	fc := fake.NewClientBuilder().WithScheme(testScheme()).Build()

	proposal := &agenticv1alpha1.Proposal{
		ObjectMeta: metav1.ObjectMeta{Name: "no-rbac", Namespace: "default"},
	}

	if err := ensureExecutionRBAC(ctx, fc, proposal, nil, "lightspeed-agent", "default"); err != nil {
		t.Fatalf("nil RBACResult should be no-op: %v", err)
	}
}

func TestEnsureExecutionRBAC_EmptyRules(t *testing.T) {
	ctx := context.Background()
	fc := fake.NewClientBuilder().WithScheme(testScheme()).Build()

	proposal := &agenticv1alpha1.Proposal{
		ObjectMeta: metav1.ObjectMeta{Name: "empty-rules", Namespace: "default"},
		Spec:       agenticv1alpha1.ProposalSpec{TargetNamespaces: []string{"prod"}},
	}
	rbacResult := &agenticv1alpha1.RBACResult{}

	if err := ensureExecutionRBAC(ctx, fc, proposal, rbacResult, "lightspeed-agent", "default"); err != nil {
		t.Fatalf("empty RBACResult should be no-op: %v", err)
	}

	// No Role should exist
	var role rbacv1.Role
	if err := fc.Get(ctx, types.NamespacedName{Name: executionRoleName("empty-rules"), Namespace: "prod"}, &role); err == nil {
		t.Fatal("Role should not exist for empty rules")
	}
}

func TestEnsureExecutionRBAC_NamespacesFromRBACRules(t *testing.T) {
	ctx := context.Background()
	fc := fake.NewClientBuilder().WithScheme(testScheme()).Build()

	ns1 := "app-ns"
	ns2 := "data-ns"
	proposal := &agenticv1alpha1.Proposal{
		ObjectMeta: metav1.ObjectMeta{Name: "ns-from-rules", Namespace: "default"},
	}
	rbacResult := &agenticv1alpha1.RBACResult{
		NamespaceScoped: []agenticv1alpha1.RBACRule{
			{Namespace: ns1, APIGroups: []string{""}, Resources: []string{"pods"}, Verbs: []string{"get"}, Justification: "Read pods"},
			{Namespace: ns2, APIGroups: []string{""}, Resources: []string{"services"}, Verbs: []string{"get"}, Justification: "Read services"},
		},
	}

	if err := ensureExecutionRBAC(ctx, fc, proposal, rbacResult, "lightspeed-agent", "default"); err != nil {
		t.Fatalf("ensureExecutionRBAC: %v", err)
	}

	roleName := executionRoleName("ns-from-rules")
	for _, ns := range []string{"app-ns", "data-ns"} {
		var role rbacv1.Role
		if err := fc.Get(ctx, types.NamespacedName{Name: roleName, Namespace: ns}, &role); err != nil {
			t.Fatalf("Role not found in %s: %v", ns, err)
		}
	}
}

func TestEnsureExecutionRBAC_ResourceNames(t *testing.T) {
	ctx := context.Background()
	fc := fake.NewClientBuilder().WithScheme(testScheme()).Build()

	proposal := &agenticv1alpha1.Proposal{
		ObjectMeta: metav1.ObjectMeta{Name: "with-names", Namespace: "default"},
		Spec:       agenticv1alpha1.ProposalSpec{TargetNamespaces: []string{"prod"}},
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

	if err := ensureExecutionRBAC(ctx, fc, proposal, rbacResult, "lightspeed-agent", "default"); err != nil {
		t.Fatalf("ensureExecutionRBAC: %v", err)
	}

	var role rbacv1.Role
	if err := fc.Get(ctx, types.NamespacedName{Name: executionRoleName("with-names"), Namespace: "prod"}, &role); err != nil {
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
	fc := fake.NewClientBuilder().WithScheme(testScheme()).Build()

	proposal := &agenticv1alpha1.Proposal{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "cleanup-test",
			Namespace:   "default",
			Annotations: map[string]string{rbacNamespacesAnnotation: "ns-a,ns-b"},
		},
		Spec: agenticv1alpha1.ProposalSpec{TargetNamespaces: []string{"ns-a", "ns-b"}},
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
	if err := ensureExecutionRBAC(ctx, fc, proposal, rbacResult, "lightspeed-agent", "default"); err != nil {
		t.Fatalf("ensure: %v", err)
	}

	// Verify resources exist
	roleName := executionRoleName("cleanup-test")
	crName := clusterRoleName("cleanup-test")
	var role rbacv1.Role
	if err := fc.Get(ctx, types.NamespacedName{Name: roleName, Namespace: "ns-a"}, &role); err != nil {
		t.Fatalf("Role not created: %v", err)
	}
	var cr rbacv1.ClusterRole
	if err := fc.Get(ctx, types.NamespacedName{Name: crName}, &cr); err != nil {
		t.Fatalf("ClusterRole not created: %v", err)
	}

	// Cleanup
	if err := cleanupExecutionRBAC(ctx, fc, proposal); err != nil {
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
	fc := fake.NewClientBuilder().WithScheme(testScheme()).Build()

	proposal := &agenticv1alpha1.Proposal{
		ObjectMeta: metav1.ObjectMeta{Name: "no-annot", Namespace: "default"},
	}

	// Create cluster-scoped only
	rbacResult := &agenticv1alpha1.RBACResult{
		ClusterScoped: []agenticv1alpha1.RBACRule{{
			APIGroups: []string{""}, Resources: []string{"nodes"},
			Verbs: []string{"get"}, Justification: "Read nodes",
		}},
	}
	if err := ensureExecutionRBAC(ctx, fc, proposal, rbacResult, "lightspeed-agent", "default"); err != nil {
		t.Fatalf("ensure: %v", err)
	}

	// Cleanup with no namespace annotation — should still clean cluster resources
	if err := cleanupExecutionRBAC(ctx, fc, proposal); err != nil {
		t.Fatalf("cleanupExecutionRBAC: %v", err)
	}

	crName := clusterRoleName("no-annot")
	var cr rbacv1.ClusterRole
	if err := fc.Get(ctx, types.NamespacedName{Name: crName}, &cr); err == nil {
		t.Fatal("ClusterRole should be deleted")
	}
}

func TestCleanupExecutionRBAC_MissingResources(t *testing.T) {
	ctx := context.Background()
	fc := fake.NewClientBuilder().WithScheme(testScheme()).Build()

	proposal := &agenticv1alpha1.Proposal{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "already-gone",
			Namespace:   "default",
			Annotations: map[string]string{rbacNamespacesAnnotation: "ghost-ns"},
		},
	}

	// Nothing created — cleanup should tolerate NotFound
	if err := cleanupExecutionRBAC(ctx, fc, proposal); err != nil {
		t.Fatalf("cleanup of missing resources should succeed: %v", err)
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
		proposal := &agenticv1alpha1.Proposal{
			Spec: agenticv1alpha1.ProposalSpec{TargetNamespaces: []string{"prod", "staging"}},
		}
		got := rbacTargetNamespaces(proposal, &agenticv1alpha1.RBACResult{
			NamespaceScoped: []agenticv1alpha1.RBACRule{{Namespace: ns1}},
		})
		if len(got) != 2 || got[0] != "prod" || got[1] != "staging" {
			t.Fatalf("spec namespaces should take precedence: %v", got)
		}
	})

	t.Run("from_rbac_rules", func(t *testing.T) {
		proposal := &agenticv1alpha1.Proposal{}
		got := rbacTargetNamespaces(proposal, &agenticv1alpha1.RBACResult{
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
		proposal := &agenticv1alpha1.Proposal{}
		got := rbacTargetNamespaces(proposal, &agenticv1alpha1.RBACResult{
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
		proposal := &agenticv1alpha1.Proposal{}
		got := rbacTargetNamespaces(proposal, nil)
		if got != nil {
			t.Fatalf("should be nil for nil rbac: %v", got)
		}
	})

	t.Run("nil_namespace_in_rule", func(t *testing.T) {
		proposal := &agenticv1alpha1.Proposal{}
		got := rbacTargetNamespaces(proposal, &agenticv1alpha1.RBACResult{
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
		proposal := &agenticv1alpha1.Proposal{}
		got := rbacTargetNamespaces(proposal, &agenticv1alpha1.RBACResult{
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
		p := &agenticv1alpha1.Proposal{ObjectMeta: metav1.ObjectMeta{Name: "test"}}
		got := annotatedRBACNamespaces(p)
		if got != nil {
			t.Fatalf("expected nil, got %v", got)
		}
	})

	t.Run("empty_value", func(t *testing.T) {
		p := &agenticv1alpha1.Proposal{
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
		p := &agenticv1alpha1.Proposal{
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
		p := &agenticv1alpha1.Proposal{
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
	labels := rbacLabels("fix-oom", "execution-rbac")
	if labels[LabelProposal] != "fix-oom" {
		t.Fatalf("proposal label: %s", labels[LabelProposal])
	}
	if labels[LabelComponent] != "execution-rbac" {
		t.Fatalf("component label: %s", labels[LabelComponent])
	}
	if len(labels) != 2 {
		t.Fatalf("expected 2 labels, got %d", len(labels))
	}
}
