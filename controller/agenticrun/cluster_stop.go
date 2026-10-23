package agenticrun

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
)

// SpokeStopErrorKind classifies a failure while stopping a run that targets a
// spoke. Only gone/unreachable failures are eligible for the config-level
// degraded completion; permission and teardown errors keep the config in
// Stopping until they are resolved.
type SpokeStopErrorKind string

const (
	SpokeStopGone        SpokeStopErrorKind = "Gone"
	SpokeStopUnreachable SpokeStopErrorKind = "Unreachable"
	SpokeStopFailed      SpokeStopErrorKind = "Failed"

	spokeSweepTimeout = 30 * time.Second

	// SpokeRetryWindow is the bounded window before a gone or unreachable
	// spoke is reported as unverified and a stopped AgenticRun's RBAC finalizer
	// may be released. It is shared by the config controller and the run
	// deletion path.
	SpokeRetryWindow = 60 * time.Second
)

// SpokeStopError records which spoke prevented confirmed teardown.
type SpokeStopError struct {
	Cluster string
	Kind    SpokeStopErrorKind
	Err     error
}

func (e *SpokeStopError) Error() string {
	return fmt.Sprintf("spoke %s %s: %v", e.Cluster, e.Kind, e.Err)
}

func (e *SpokeStopError) Unwrap() error { return e.Err }

// IsSpokeStopDegradeable reports whether err is (or contains) only failures
// that may be reported as AdminActivatedWithErrors after the retry window.
func IsSpokeStopDegradeable(err error) bool {
	if err == nil {
		return false
	}
	switch typed := err.(type) {
	case *SpokeStopError:
		return typed.Kind == SpokeStopGone || typed.Kind == SpokeStopUnreachable
	case interface{ Unwrap() []error }:
		children := typed.Unwrap()
		if len(children) == 0 {
			return false
		}
		for _, child := range children {
			if !IsSpokeStopDegradeable(child) {
				return false
			}
		}
		return true
	case interface{ Unwrap() error }:
		child := typed.Unwrap()
		return child != nil && IsSpokeStopDegradeable(child)
	default:
		return false
	}
}

// IsSpokeStopGone reports whether err is (or contains) only hard-gone spoke
// failures. A gone spoke may be reported as degraded without waiting for the
// unreachable-spoke retry window.
func IsSpokeStopGone(err error) bool {
	if err == nil {
		return false
	}
	switch typed := err.(type) {
	case *SpokeStopError:
		return typed.Kind == SpokeStopGone
	case interface{ Unwrap() []error }:
		children := typed.Unwrap()
		if len(children) == 0 {
			return false
		}
		for _, child := range children {
			if !IsSpokeStopGone(child) {
				return false
			}
		}
		return true
	case interface{ Unwrap() error }:
		child := typed.Unwrap()
		return child != nil && IsSpokeStopGone(child)
	default:
		return false
	}
}

// SpokeStopClusters returns the sorted set of spoke names in err.
func SpokeStopClusters(err error) []string {
	set := map[string]struct{}{}
	var collect func(error)
	collect = func(current error) {
		if current == nil {
			return
		}
		switch typed := current.(type) {
		case *SpokeStopError:
			if typed.Cluster != "" {
				set[typed.Cluster] = struct{}{}
			}
			collect(typed.Err)
		case interface{ Unwrap() []error }:
			for _, child := range typed.Unwrap() {
				collect(child)
			}
		case interface{ Unwrap() error }:
			collect(typed.Unwrap())
		}
	}
	collect(err)
	clusters := make([]string, 0, len(set))
	for cluster := range set {
		clusters = append(clusters, cluster)
	}
	sort.Strings(clusters)
	return clusters
}

func isUnreachableError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	if apierrors.IsTimeout(err) || apierrors.IsServerTimeout(err) || apierrors.IsServiceUnavailable(err) {
		return true
	}
	message := strings.ToLower(err.Error())
	for _, fragment := range []string{
		"connection refused",
		"connection reset",
		"no such host",
		"i/o timeout",
		"network is unreachable",
		"tls handshake timeout",
		"context deadline exceeded",
	} {
		if strings.Contains(message, fragment) {
			return true
		}
	}
	return false
}

func classifySpokeAccessError(err error) SpokeStopErrorKind {
	if apierrors.IsNotFound(err) || isGoneSpokeConfigError(err) {
		return SpokeStopGone
	}
	if isUnreachableError(err) {
		return SpokeStopUnreachable
	}
	return SpokeStopFailed
}

func isGoneSpokeConfigError(err error) bool {
	if err == nil {
		return false
	}
	message := err.Error()
	for _, fragment := range []string{
		ErrParseSpokeKubeconfig,
		ErrSpokeInsecureTLS,
		ErrCreateSpokeClient,
	} {
		if strings.Contains(message, fragment) {
			return true
		}
	}
	return false
}

// wrapSpokeOperationErrors names the spoke in every operation error. Transport
// failures are degradeable; permission and API validation errors stay
// non-degradeable and keep the config in Stopping.
func wrapSpokeOperationErrors(cluster string, errs []error) []error {
	wrapped := make([]error, 0, len(errs))
	for _, err := range errs {
		if err == nil {
			continue
		}
		kind := SpokeStopFailed
		if isUnreachableError(err) {
			kind = SpokeStopUnreachable
		}
		wrapped = append(wrapped, &SpokeStopError{Cluster: cluster, Kind: kind, Err: err})
	}
	return wrapped
}

// SweepCluster stops every run-associated workload/access and sweeps
// operator-managed resources that lost their AgenticRun reference. It returns
// true only when no active workload or sandbox access remains.
func (m *SandboxManager) SweepCluster(ctx context.Context, runs []agenticv1alpha1.AgenticRun) (bool, error) {
	var errs []error
	confirmed := true

	for i := range runs {
		ok, err := m.StopSandboxes(ctx, &runs[i])
		if err != nil {
			errs = append(errs, err)
		}
		if !ok {
			confirmed = false
		}
	}

	orphansOK, orphanErrs := m.sweepOrphanedHubResources(ctx)
	errs = append(errs, orphanErrs...)
	if !orphansOK {
		confirmed = false
	}

	spokesOK, spokeErrs := m.sweepSpokeRBAC(ctx)
	errs = append(errs, spokeErrs...)
	if !spokesOK {
		confirmed = false
	}

	return confirmed, errors.Join(errs...)
}

func (m *SandboxManager) sweepOrphanedHubResources(ctx context.Context) (bool, []error) {
	reader := m.apiReader()
	var errs []error

	var pods corev1.PodList
	if err := reader.List(ctx, &pods, client.InNamespace(m.namespace), client.HasLabels{LabelRun}); err != nil {
		errs = append(errs, fmt.Errorf("list labeled Pods for cluster sweep: %w", err))
	} else {
		for idx := range pods.Items {
			pod := &pods.Items[idx]
			if !isActivePod(pod) {
				continue
			}
			if err := m.client.Delete(ctx, pod, stopDeleteOptions(pod, client.GracePeriodSeconds(0))...); err != nil && !apierrors.IsNotFound(err) {
				errs = append(errs, fmt.Errorf("delete labeled Pod %s/%s with zero grace: %w", pod.Namespace, pod.Name, err))
			}
		}
	}

	for _, item := range []struct {
		gvk  schema.GroupVersionKind
		kind string
	}{
		{gvk: smClaimGVK, kind: "SandboxClaim"},
		{gvk: smSandboxGVK, kind: "Sandbox"},
		{gvk: sandboxWarmPoolGVK, kind: "SandboxWarmPool"},
	} {
		objects, err := listStopUnstructured(ctx, reader, item.gvk, m.namespace)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for idx := range objects {
			obj := &objects[idx]
			if obj.GetLabels()[LabelRun] == "" {
				continue
			}
			if err := deleteStopResource(ctx, m.client, obj, item.kind); err != nil {
				errs = append(errs, err)
			}
		}
	}

	var secrets corev1.SecretList
	if err := reader.List(ctx, &secrets, client.InNamespace(m.namespace), client.HasLabels{LabelRun}); err != nil {
		errs = append(errs, fmt.Errorf("list labeled Secrets for cluster sweep: %w", err))
	} else {
		for idx := range secrets.Items {
			secret := &secrets.Items[idx]
			if err := m.client.Delete(ctx, secret, stopDeleteOptions(secret)...); err != nil && !apierrors.IsNotFound(err) {
				errs = append(errs, fmt.Errorf("delete labeled Secret %s/%s: %w", secret.Namespace, secret.Name, err))
			}
		}
	}

	hubRBACOK, hubRBACErrs := sweepLabeledAccess(ctx, reader, m.client, "")
	errs = append(errs, hubRBACErrs...)

	remaining, confirmErrs := m.orphanedHubResourcesRemain(ctx, reader)
	errs = append(errs, confirmErrs...)
	return !remaining && hubRBACOK, errs
}

func (m *SandboxManager) orphanedHubResourcesRemain(ctx context.Context, reader client.Reader) (bool, []error) {
	var errs []error
	remains := false

	var pods corev1.PodList
	if err := reader.List(ctx, &pods, client.InNamespace(m.namespace), client.HasLabels{LabelRun}); err != nil {
		errs = append(errs, fmt.Errorf("confirm labeled Pods for cluster sweep: %w", err))
	} else {
		for idx := range pods.Items {
			if isActivePod(&pods.Items[idx]) {
				remains = true
				break
			}
		}
	}

	for _, item := range []struct {
		gvk schema.GroupVersionKind
	}{
		{gvk: smClaimGVK},
		{gvk: smSandboxGVK},
		{gvk: sandboxWarmPoolGVK},
	} {
		objects, err := listStopUnstructured(ctx, reader, item.gvk, m.namespace)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for idx := range objects {
			if objects[idx].GetLabels()[LabelRun] != "" {
				remains = true
				break
			}
		}
	}

	var secrets corev1.SecretList
	if err := reader.List(ctx, &secrets, client.InNamespace(m.namespace), client.HasLabels{LabelRun}); err != nil {
		errs = append(errs, fmt.Errorf("confirm labeled Secrets for cluster sweep: %w", err))
	} else if len(secrets.Items) > 0 {
		remains = true
	}

	return remains, errs
}

func (m *SandboxManager) sweepSpokeRBAC(ctx context.Context) (bool, []error) {
	var secrets corev1.SecretList
	if err := m.apiReader().List(ctx, &secrets, client.InNamespace(m.namespace)); err != nil {
		return false, []error{fmt.Errorf("list spoke kubeconfig Secrets for cluster sweep: %w", err)}
	}

	var errs []error
	confirmed := true
	for idx := range secrets.Items {
		secret := &secrets.Items[idx]
		if !strings.HasPrefix(secret.Name, spokeKubeconfigPrefix) {
			continue
		}
		cluster := strings.TrimPrefix(secret.Name, spokeKubeconfigPrefix)
		if cluster == "" {
			continue
		}
		spokeCtx, cancel := context.WithTimeout(ctx, spokeSweepTimeout)
		ok, spokeErrs := m.sweepSpokeAccess(spokeCtx, cluster)
		cancel()
		errs = append(errs, spokeErrs...)
		if !ok {
			confirmed = false
		}
	}
	return confirmed, errs
}

func (m *SandboxManager) sweepSpokeAccess(ctx context.Context, cluster string) (bool, []error) {
	spoke, err := spokeAccessForCluster(ctx, m.apiReader(), m.namespace, cluster)
	if err != nil {
		return false, []error{&SpokeStopError{Cluster: cluster, Kind: classifySpokeAccessError(err), Err: err}}
	}
	if spoke == nil {
		return true, nil
	}
	ok, errs := sweepLabeledAccess(ctx, spoke.Client, spoke.Client, spoke.Namespace)
	return ok, wrapSpokeOperationErrors(cluster, errs)
}

// sweepLabeledAccess deletes operator-managed ServiceAccounts and RBAC objects
// carrying LabelRun. An empty namespace lists all namespaces.
func sweepLabeledAccess(ctx context.Context, reader client.Reader, writer client.Client, namespace string) (bool, []error) {
	var errs []error
	remains := false
	var namespaced []client.ListOption
	if namespace != "" {
		namespaced = append(namespaced, client.InNamespace(namespace))
	}

	var serviceAccounts corev1.ServiceAccountList
	if err := reader.List(ctx, &serviceAccounts, namespaced...); err != nil {
		errs = append(errs, fmt.Errorf("list labeled ServiceAccounts for cluster sweep: %w", err))
	} else {
		for idx := range serviceAccounts.Items {
			sa := &serviceAccounts.Items[idx]
			if sa.Labels[LabelRun] == "" {
				continue
			}
			if err := deleteStopResource(ctx, writer, sa, "ServiceAccount"); err != nil {
				errs = append(errs, err)
			}
		}
	}

	var roleBindings rbacv1.RoleBindingList
	if err := reader.List(ctx, &roleBindings, namespaced...); err != nil {
		errs = append(errs, fmt.Errorf("list labeled RoleBindings for cluster sweep: %w", err))
	} else {
		for idx := range roleBindings.Items {
			binding := &roleBindings.Items[idx]
			if binding.Labels[LabelRun] == "" {
				continue
			}
			if err := deleteStopResource(ctx, writer, binding, "RoleBinding"); err != nil {
				errs = append(errs, err)
			}
		}
	}

	var roles rbacv1.RoleList
	if err := reader.List(ctx, &roles, namespaced...); err != nil {
		errs = append(errs, fmt.Errorf("list labeled Roles for cluster sweep: %w", err))
	} else {
		for idx := range roles.Items {
			role := &roles.Items[idx]
			if role.Labels[LabelRun] == "" {
				continue
			}
			if err := deleteStopResource(ctx, writer, role, "Role"); err != nil {
				errs = append(errs, err)
			}
		}
	}

	var clusterRoleBindings rbacv1.ClusterRoleBindingList
	if err := reader.List(ctx, &clusterRoleBindings); err != nil {
		errs = append(errs, fmt.Errorf("list labeled ClusterRoleBindings for cluster sweep: %w", err))
	} else {
		for idx := range clusterRoleBindings.Items {
			binding := &clusterRoleBindings.Items[idx]
			if binding.Labels[LabelRun] == "" {
				continue
			}
			if err := deleteStopResource(ctx, writer, binding, "ClusterRoleBinding"); err != nil {
				errs = append(errs, err)
			}
		}
	}

	var clusterRoles rbacv1.ClusterRoleList
	if err := reader.List(ctx, &clusterRoles); err != nil {
		errs = append(errs, fmt.Errorf("list labeled ClusterRoles for cluster sweep: %w", err))
	} else {
		for idx := range clusterRoles.Items {
			role := &clusterRoles.Items[idx]
			if role.Labels[LabelRun] == "" {
				continue
			}
			if err := deleteStopResource(ctx, writer, role, "ClusterRole"); err != nil {
				errs = append(errs, err)
			}
		}
	}

	// Confirmation: any labeled object that survived keeps the sweep pending.
	if err := reader.List(ctx, &serviceAccounts, namespaced...); err != nil {
		errs = append(errs, fmt.Errorf("confirm labeled ServiceAccounts for cluster sweep: %w", err))
	} else {
		for idx := range serviceAccounts.Items {
			if serviceAccounts.Items[idx].Labels[LabelRun] != "" {
				remains = true
				break
			}
		}
	}
	if err := reader.List(ctx, &roleBindings, namespaced...); err != nil {
		errs = append(errs, fmt.Errorf("confirm labeled RoleBindings for cluster sweep: %w", err))
	} else {
		for idx := range roleBindings.Items {
			if roleBindings.Items[idx].Labels[LabelRun] != "" {
				remains = true
				break
			}
		}
	}
	if err := reader.List(ctx, &roles, namespaced...); err != nil {
		errs = append(errs, fmt.Errorf("confirm labeled Roles for cluster sweep: %w", err))
	} else {
		for idx := range roles.Items {
			if roles.Items[idx].Labels[LabelRun] != "" {
				remains = true
				break
			}
		}
	}
	if err := reader.List(ctx, &clusterRoleBindings); err != nil {
		errs = append(errs, fmt.Errorf("confirm labeled ClusterRoleBindings for cluster sweep: %w", err))
	} else {
		for idx := range clusterRoleBindings.Items {
			if clusterRoleBindings.Items[idx].Labels[LabelRun] != "" {
				remains = true
				break
			}
		}
	}
	if err := reader.List(ctx, &clusterRoles); err != nil {
		errs = append(errs, fmt.Errorf("confirm labeled ClusterRoles for cluster sweep: %w", err))
	} else {
		for idx := range clusterRoles.Items {
			if clusterRoles.Items[idx].Labels[LabelRun] != "" {
				remains = true
				break
			}
		}
	}

	return !remains, errs
}

func isActivePod(pod *corev1.Pod) bool {
	if pod == nil {
		return false
	}
	return pod.Status.Phase != corev1.PodSucceeded && pod.Status.Phase != corev1.PodFailed
}
