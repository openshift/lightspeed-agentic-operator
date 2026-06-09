package proposal

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
)

const (
	rbacNamespacesAnnotation = "agentic.openshift.io/rbac-namespaces"
)

// +kubebuilder:rbac:groups="",resources=serviceaccounts,verbs=create;delete;get

// executionSAName returns the per-proposal ServiceAccount name for execution RBAC isolation.
// Uses the same truncation pattern as executionRoleName. Collision is theoretically possible
// for very long namespace+name combinations (>55 chars) that share the same prefix after
// truncation, but is near-impossible in practice with typical naming conventions.
func executionSAName(proposal *agenticv1alpha1.Proposal) string {
	return truncateK8sName(fmt.Sprintf("ls-exec-%s-%s", proposal.Namespace, proposal.Name))
}

// ensureExecutionSA creates a per-proposal ServiceAccount for execution RBAC isolation.
// No owner reference — cross-namespace owner refs are unsupported by Kubernetes GC.
// Cleanup is handled explicitly by cleanupExecutionRBAC (via finalizer on Proposal deletion).
func ensureExecutionSA(ctx context.Context, c client.Client, proposal *agenticv1alpha1.Proposal, operatorNS string) (string, error) {
	saName := executionSAName(proposal)
	sa := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      saName,
			Namespace: operatorNS,
			Labels:    rbacLabels(proposal.Name, "execution-sa"),
		},
	}
	if err := c.Create(ctx, sa); err != nil && !apierrors.IsAlreadyExists(err) {
		return "", fmt.Errorf("create execution SA %s: %w", saName, err)
	}
	return saName, nil
}

// deleteExecutionSA explicitly deletes the per-proposal ServiceAccount after execution completes.
func deleteExecutionSA(ctx context.Context, c client.Client, proposal *agenticv1alpha1.Proposal, operatorNS string) error {
	saName := executionSAName(proposal)
	return deleteIfExists(ctx, c, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: saName, Namespace: operatorNS}})
}

// ensureExecutionRBAC creates a per-proposal SA, then Role+RoleBinding (namespace-scoped) and
// ClusterRole+ClusterRoleBinding (cluster-scoped) from the selected option's RBAC result.
// All bindings reference the per-proposal SA for isolation between concurrent Proposals.
// Idempotent — skips resources that already exist.
func ensureExecutionRBAC(
	ctx context.Context,
	c client.Client,
	proposal *agenticv1alpha1.Proposal,
	rbacResult *agenticv1alpha1.RBACResult,
	operatorNS string,
) error {
	if rbacResult == nil {
		return nil
	}

	saName, err := ensureExecutionSA(ctx, c, proposal, operatorNS)
	if err != nil {
		return err
	}

	roleName := executionRoleName(proposal.Name)
	labels := rbacLabels(proposal.Name, "execution-rbac")

	subjects := []rbacv1.Subject{{
		Kind:      rbacv1.ServiceAccountKind,
		Name:      saName,
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

// cleanupExecutionRBAC removes all RBAC resources and the per-proposal SA created for
// a proposal's execution. Uses the annotation to find namespaces (survives retry clearing Steps).
func cleanupExecutionRBAC(ctx context.Context, c client.Client, proposal *agenticv1alpha1.Proposal, operatorNS string) error {
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

	if err := deleteExecutionSA(ctx, c, proposal, operatorNS); err != nil {
		return fmt.Errorf("delete execution SA: %w", err)
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
