package agenticrun

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
)

func cleanupSpokeRunAccess(
	ctx context.Context,
	run *agenticv1alpha1.AgenticRun,
	spoke *SpokeAccess,
) (SuspensionCleanup, error) {
	var report SuspensionCleanup
	if run.Spec.TargetCluster == "" {
		return report, nil
	}
	if spoke == nil || spoke.Client == nil {
		report.Unverified = append(report.Unverified,
			fmt.Sprintf("spoke access for target %q is unavailable", run.Spec.TargetCluster))
		return report, nil
	}

	for _, step := range []string{"analysis", "execution", "verification", "escalation"} {
		deleteAndConfirmSpokeObject(ctx, spoke.Client, &corev1.ServiceAccount{
			ObjectMeta: metav1.ObjectMeta{Name: sandboxSAName(run, step), Namespace: spoke.Namespace},
		}, run.UID, &report)

		for index := range spokeReaderBindingNames {
			deleteAndConfirmSpokeObject(ctx, spoke.Client, &rbacv1.ClusterRoleBinding{
				ObjectMeta: metav1.ObjectMeta{Name: perRunCRBName(string(run.UID), step, index)},
			}, run.UID, &report)
		}
	}

	roleName := executionRoleName(string(run.UID))
	for _, namespace := range annotatedRBACNamespaces(run) {
		deleteAndConfirmSpokeObject(ctx, spoke.Client, &rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: roleName, Namespace: namespace},
		}, run.UID, &report)
		deleteAndConfirmSpokeObject(ctx, spoke.Client, &rbacv1.Role{
			ObjectMeta: metav1.ObjectMeta{Name: roleName, Namespace: namespace},
		}, run.UID, &report)
	}

	crName := clusterRoleName(string(run.UID))
	deleteAndConfirmSpokeObject(ctx, spoke.Client, &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: crName},
	}, run.UID, &report)
	deleteAndConfirmSpokeObject(ctx, spoke.Client, &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{Name: crName},
	}, run.UID, &report)

	return report, nil
}

func deleteAndConfirmSpokeObject(
	ctx context.Context,
	c client.Client,
	obj client.Object,
	runUID types.UID,
	report *SuspensionCleanup,
) {
	key := client.ObjectKeyFromObject(obj)
	description := fmt.Sprintf("%T %s/%s", obj, key.Namespace, key.Name)
	if err := c.Get(ctx, key, obj); err != nil {
		if apierrors.IsNotFound(err) {
			return
		}
		report.Unverified = append(report.Unverified, description+": GET: "+err.Error())
		return
	}
	if obj.GetName() != key.Name || obj.GetNamespace() != key.Namespace {
		report.Unverified = append(report.Unverified, description+": deterministic name mismatch")
		return
	}

	if obj.GetLabels()[LabelRun] != string(runUID) {
		report.Unverified = append(report.Unverified, description+": run UID label mismatch")
		return
	}
	uid := obj.GetUID()
	if uid == "" {
		report.Unverified = append(report.Unverified, description+": UID is missing")
		return
	}
	if err := c.Delete(ctx, obj, client.Preconditions{UID: &uid}); err != nil && !apierrors.IsNotFound(err) {
		report.Unverified = append(report.Unverified, description+": DELETE: "+err.Error())
		return
	}
	if err := c.Get(ctx, key, obj); err == nil {
		report.Pending = append(report.Pending, description)
	} else if !apierrors.IsNotFound(err) {
		report.Unverified = append(report.Unverified, description+": confirm absence: "+err.Error())
	}
}
