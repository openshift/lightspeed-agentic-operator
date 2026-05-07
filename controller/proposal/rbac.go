package proposal

import (
	"context"
	"fmt"
	"strings"

	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
)

const (
	rbacNamespacesAnnotation = "agentic.openshift.io/rbac-namespaces"
)

// ensureExecutionRBAC creates Role+RoleBinding (namespace-scoped) and
// ClusterRole+ClusterRoleBinding (cluster-scoped) from the selected
// option's RBAC result. Idempotent — skips resources that already exist.
func ensureExecutionRBAC(
	ctx context.Context,
	c client.Client,
	proposal *agenticv1alpha1.Proposal,
	rbacResult *agenticv1alpha1.RBACResult,
	sandboxSA string,
	operatorNS string,
) error {
	if rbacResult == nil {
		return nil
	}

	roleName := executionRoleName(proposal.Name)
	labels := rbacLabels(proposal.Name, "execution-rbac")

	subjects := []rbacv1.Subject{{
		Kind:      rbacv1.ServiceAccountKind,
		Name:      sandboxSA,
		Namespace: operatorNS,
	}}

	if len(rbacResult.NamespaceScoped) > 0 {
		nsRules := rbacRulesToPolicyRules(rbacResult.NamespaceScoped)
		targetNS := rbacTargetNamespaces(proposal, rbacResult)

		if len(targetNS) > 0 {
			if proposal.Annotations == nil {
				proposal.Annotations = make(map[string]string)
			}
			proposal.Annotations[rbacNamespacesAnnotation] = strings.Join(targetNS, ",")
		}

		for _, ns := range targetNS {
			role := &rbacv1.Role{
				ObjectMeta: metav1.ObjectMeta{Name: roleName, Namespace: ns, Labels: labels},
				Rules:      nsRules,
			}
			if err := c.Create(ctx, role); err != nil && !apierrors.IsAlreadyExists(err) {
				return fmt.Errorf("create Role in %s: %w", ns, err)
			}
			binding := &rbacv1.RoleBinding{
				ObjectMeta: metav1.ObjectMeta{Name: roleName, Namespace: ns, Labels: labels},
				RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: roleName},
				Subjects:   subjects,
			}
			if err := c.Create(ctx, binding); err != nil && !apierrors.IsAlreadyExists(err) {
				return fmt.Errorf("create RoleBinding in %s: %w", ns, err)
			}
		}
	}

	if len(rbacResult.ClusterScoped) > 0 {
		crName := clusterRoleName(proposal.Name)
		clusterRules := rbacRulesToPolicyRules(rbacResult.ClusterScoped)
		cr := &rbacv1.ClusterRole{
			ObjectMeta: metav1.ObjectMeta{Name: crName, Labels: labels},
			Rules:      clusterRules,
		}
		if err := c.Create(ctx, cr); err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("create ClusterRole %s: %w", crName, err)
		}
		crb := &rbacv1.ClusterRoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: crName, Labels: labels},
			RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: crName},
			Subjects:   subjects,
		}
		if err := c.Create(ctx, crb); err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("create ClusterRoleBinding %s: %w", crName, err)
		}
	}

	return nil
}

// cleanupExecutionRBAC removes all RBAC resources created for a proposal's
// execution. Uses the annotation to find namespaces (survives retry clearing Steps).
func cleanupExecutionRBAC(ctx context.Context, c client.Client, proposal *agenticv1alpha1.Proposal) error {
	roleName := executionRoleName(proposal.Name)

	nsList := annotatedRBACNamespaces(proposal)

	for _, ns := range nsList {
		if err := deleteIfExists(ctx, c, &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: roleName, Namespace: ns}}); err != nil {
			return fmt.Errorf("delete RoleBinding in %s: %w", ns, err)
		}
		if err := deleteIfExists(ctx, c, &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: roleName, Namespace: ns}}); err != nil {
			return fmt.Errorf("delete Role in %s: %w", ns, err)
		}
	}

	crName := clusterRoleName(proposal.Name)
	if err := deleteIfExists(ctx, c, &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: crName}}); err != nil {
		return fmt.Errorf("delete ClusterRoleBinding %s: %w", crName, err)
	}
	if err := deleteIfExists(ctx, c, &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: crName}}); err != nil {
		return fmt.Errorf("delete ClusterRole %s: %w", crName, err)
	}
	return nil
}

func annotatedRBACNamespaces(proposal *agenticv1alpha1.Proposal) []string {
	if proposal.Annotations == nil {
		return nil
	}
	val := proposal.Annotations[rbacNamespacesAnnotation]
	if val == "" {
		return nil
	}
	return strings.Split(val, ",")
}

func deleteIfExists(ctx context.Context, c client.Client, obj client.Object) error {
	if err := c.Delete(ctx, obj); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

func rbacTargetNamespaces(proposal *agenticv1alpha1.Proposal, rbacResult *agenticv1alpha1.RBACResult) []string {
	if len(proposal.Spec.TargetNamespaces) > 0 {
		return proposal.Spec.TargetNamespaces
	}
	if rbacResult == nil {
		return nil
	}
	seen := make(map[string]bool)
	var nsList []string
	for _, rule := range rbacResult.NamespaceScoped {
		if rule.Namespace != "" && !seen[rule.Namespace] {
			nsList = append(nsList, rule.Namespace)
			seen[rule.Namespace] = true
		}
	}
	return nsList
}

func truncateK8sName(name string) string {
	if len(name) > 63 {
		name = strings.TrimRight(name[:63], "-")
	}
	return name
}

func executionRoleName(proposalName string) string {
	return truncateK8sName("ls-exec-" + proposalName)
}

func clusterRoleName(proposalName string) string {
	return truncateK8sName("ls-exec-cluster-" + proposalName)
}

func rbacLabels(proposalName, component string) map[string]string {
	return map[string]string{
		LabelProposal:  proposalName,
		LabelComponent: component,
	}
}

func rbacRulesToPolicyRules(rules []agenticv1alpha1.RBACRule) []rbacv1.PolicyRule {
	out := make([]rbacv1.PolicyRule, len(rules))
	for i, r := range rules {
		out[i] = rbacv1.PolicyRule{
			APIGroups:     normalizeCoreAPIGroup(r.APIGroups),
			Resources:     r.Resources,
			ResourceNames: r.ResourceNames,
			Verbs:         r.Verbs,
		}
	}
	return out
}

// normalizeCoreAPIGroup maps "core" to "" for the Kubernetes core API group.
// The output schema requires minLength=1 so the LLM uses "core" instead of "".
func normalizeCoreAPIGroup(groups []string) []string {
	out := make([]string, len(groups))
	for i, g := range groups {
		if g == "core" {
			out[i] = ""
		} else {
			out[i] = g
		}
	}
	return out
}
