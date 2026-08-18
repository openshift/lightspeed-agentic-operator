package agenticrun

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
)

const (
	analysisViewClusterRole = "view"

	ErrCreateAnalysisSA          = "create analysis SA"
	ErrCreateAnalysisViewBinding = "create analysis view RoleBinding"
	ErrCreateAnalysisLogRole     = "create analysis log Role"
	ErrCreateAnalysisLogBinding  = "create analysis log RoleBinding"
	ErrDeleteAnalysisViewBinding = "delete analysis view RoleBinding"
	ErrDeleteAnalysisLogBinding  = "delete analysis log RoleBinding"
	ErrDeleteAnalysisLogRole     = "delete analysis log Role"
	ErrDeleteAnalysisSA          = "delete analysis SA"
)

func analysisIdentityName(run *agenticv1alpha1.AgenticRun) string {
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte(run.Namespace+"\x00"+run.Name+"\x00"+string(run.UID))))[:8]
	prefix := strings.TrimRight(fmt.Sprintf("ls-analysis-%s-%s", run.Namespace, run.Name), "-._")
	const maxPrefixLength = 47 // 47 + '-' + 8 leaves room for RoleBinding suffixes.
	if len(prefix) > maxPrefixLength {
		prefix = strings.TrimRight(prefix[:maxPrefixLength], "-._")
	}
	return prefix + "-" + digest
}

// ensureAnalysisIdentity creates a per-run ServiceAccount in the operator
// namespace. It can read only the AgenticRun namespace through the built-in
// view ClusterRole, plus failed-step pod logs.
func ensureAnalysisIdentity(ctx context.Context, c client.Client, run *agenticv1alpha1.AgenticRun, operatorNS string) (string, error) {
	name := analysisIdentityName(run)
	labels := rbacLabels(run.Name, "analysis-identity")
	subjects := []rbacv1.Subject{{
		Kind:      rbacv1.ServiceAccountKind,
		Name:      name,
		Namespace: operatorNS,
	}}

	resources := []struct {
		object client.Object
		errMsg string
	}{
		{
			object: &corev1.ServiceAccount{
				ObjectMeta:                   metav1.ObjectMeta{Name: name, Namespace: operatorNS, Labels: labels},
				AutomountServiceAccountToken: ptr.To(false),
			},
			errMsg: ErrCreateAnalysisSA,
		},
		{
			object: &rbacv1.RoleBinding{
				ObjectMeta: metav1.ObjectMeta{Name: name + "-view", Namespace: run.Namespace, Labels: labels},
				RoleRef: rbacv1.RoleRef{
					APIGroup: rbacv1.GroupName,
					Kind:     "ClusterRole",
					Name:     analysisViewClusterRole,
				},
				Subjects: subjects,
			},
			errMsg: ErrCreateAnalysisViewBinding,
		},
		{
			object: &rbacv1.Role{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: run.Namespace, Labels: labels},
				Rules: []rbacv1.PolicyRule{
					{APIGroups: []string{""}, Resources: []string{"pods/log"}, Verbs: []string{"get"}},
				},
			},
			errMsg: ErrCreateAnalysisLogRole,
		},
		{
			object: &rbacv1.RoleBinding{
				ObjectMeta: metav1.ObjectMeta{Name: name + "-logs", Namespace: run.Namespace, Labels: labels},
				RoleRef: rbacv1.RoleRef{
					APIGroup: rbacv1.GroupName,
					Kind:     "Role",
					Name:     name,
				},
				Subjects: subjects,
			},
			errMsg: ErrCreateAnalysisLogBinding,
		},
	}

	for _, resource := range resources {
		if err := c.Create(ctx, resource.object); err != nil && !apierrors.IsAlreadyExists(err) {
			return "", fmt.Errorf("%s %s: %w", resource.errMsg, name, err)
		}
	}
	return name, nil
}

func cleanupAnalysisIdentity(ctx context.Context, c client.Client, run *agenticv1alpha1.AgenticRun, operatorNS string) error {
	name := analysisIdentityName(run)
	resources := []struct {
		object client.Object
		errMsg string
	}{
		{&rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: name + "-view", Namespace: run.Namespace}}, ErrDeleteAnalysisViewBinding},
		{&rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: name + "-logs", Namespace: run.Namespace}}, ErrDeleteAnalysisLogBinding},
		{&rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: run.Namespace}}, ErrDeleteAnalysisLogRole},
		{&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: operatorNS}}, ErrDeleteAnalysisSA},
	}
	for _, resource := range resources {
		if err := deleteIfExists(ctx, c, resource.object); err != nil {
			return fmt.Errorf("%s %s: %w", resource.errMsg, name, err)
		}
	}
	return nil
}
