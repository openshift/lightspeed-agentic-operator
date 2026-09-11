package agenticrun

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"

	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
)

// +kubebuilder:rbac:groups="",resources=serviceaccounts,verbs=create;delete;get

const (
	rbacNamespacesAnnotation = "agentic.openshift.io/rbac-namespaces"

	ErrCreateSandboxSA          = "create sandbox SA"
	ErrCreateRole               = "create Role in"
	ErrCreateRoleBinding        = "create RoleBinding in"
	ErrCreateClusterRole        = "create ClusterRole"
	ErrCreateClusterRoleBinding = "create ClusterRoleBinding"
	ErrAddReaderSubject         = "add subject to reader ClusterRoleBinding"
	ErrRemoveReaderSubject      = "remove subject from reader ClusterRoleBinding"
	ErrDeleteRoleBinding        = "delete RoleBinding in"
	ErrDeleteRole               = "delete Role in"
	ErrDeleteClusterRoleBinding = "delete ClusterRoleBinding"
	ErrDeleteClusterRole        = "delete ClusterRole"
)

var readerBindings atomic.Value // []string — cached CRB names; nil until first discovery

func init() {
	readerBindings.Store([]string(nil))
}

// Spoke reader CRB names — created by the hub operator's provisioner during
// spoke registration (lightspeed-hub/internal/provisioner/spoke.go). Known
// and stable, so we hardcode them instead of using the process-global
// discovery cache (which is hub-only). If the hub operator renames these,
// this must be updated to match.
var spokeReaderBindingNames = []string{
	"lightspeed-hub:cluster-reader",
	"lightspeed-hub:cluster-monitoring-view",
}

// addReaderSubjectOnSpoke adds the SA to the known spoke reader
// ClusterRoleBindings. Uses hardcoded CRB names (no global cache).
func addReaderSubjectOnSpoke(ctx context.Context, spokeClient client.Client, saName, spokeNS string) error {
	subject := rbacv1.Subject{
		Kind:      rbacv1.ServiceAccountKind,
		Name:      saName,
		Namespace: spokeNS,
	}
	for _, name := range spokeReaderBindingNames {
		if err := addSubjectToBinding(ctx, spokeClient, name, subject); err != nil {
			return err
		}
	}
	return nil
}

// removeReaderSubjectOnSpoke removes the SA from the known spoke reader
// ClusterRoleBindings. Processes all bindings even if one fails (best-effort
// cleanup). Uses hardcoded CRB names (no global cache).
func removeReaderSubjectOnSpoke(ctx context.Context, spokeClient client.Client, saName, spokeNS string) error {
	var firstErr error
	for _, name := range spokeReaderBindingNames {
		if err := removeSubjectFromBinding(ctx, spokeClient, name, saName, spokeNS); err != nil {
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

// resolveReaderBindings returns all ClusterRoleBindings that list the
// lightspeed-agent SA as a subject.  Results are discovered once and cached
// for the lifetime of the process (CRBs are static infrastructure).
// If discovery has not yet succeeded, it is triggered on demand.
func resolveReaderBindings(ctx context.Context, c client.Client, operatorNS string) ([]string, error) {
	if names := readerBindings.Load().([]string); len(names) > 0 {
		return names, nil
	}

	log := logf.FromContext(ctx)
	log.Info("discovering reader ClusterRoleBindings by SA subject")

	crbList := &rbacv1.ClusterRoleBindingList{}
	if err := c.List(ctx, crbList); err != nil {
		return nil, fmt.Errorf("list ClusterRoleBindings for reader discovery: %w", err)
	}

	var names []string
	for i := range crbList.Items {
		for _, s := range crbList.Items[i].Subjects {
			if s.Kind == rbacv1.ServiceAccountKind && s.Name == defaultSandboxSA && s.Namespace == operatorNS {
				names = append(names, crbList.Items[i].Name)
				break
			}
		}
	}

	if len(names) == 0 {
		return nil, fmt.Errorf("no ClusterRoleBinding found with subject %s/%s — ensure reader RBAC is configured", operatorNS, defaultSandboxSA)
	}

	log.Info("resolved reader ClusterRoleBindings", "bindings", names)
	readerBindings.Store(names)
	return names, nil
}

// sandboxSAName returns the per-step ServiceAccount name for RBAC isolation.
// Each step gets its own SA so it can only create its specific Result CRD.
// Uses the run UID to guarantee uniqueness without truncation collisions.
func sandboxSAName(run *agenticv1alpha1.AgenticRun, step string) string {
	return fmt.Sprintf("ls-%s-%s", stepAbbrev(step), run.UID)
}

func stepAbbrev(step string) string {
	switch step {
	case "analysis":
		return "anl"
	case "execution":
		return "exe"
	case "verification":
		return "ver"
	case "escalation":
		return "esc"
	default:
		return step
	}
}

// addReaderSubject adds the SA as a subject to every ClusterRoleBinding that
// references the lightspeed-agent SA (e.g. cluster-reader, cluster-monitoring-view).
// Idempotent — skips bindings where the subject is already present.
// Retries each binding independently on conflict (optimistic locking).
func addReaderSubject(ctx context.Context, c client.Client, saName, namespace string) error {
	names, err := resolveReaderBindings(ctx, c, namespace)
	if err != nil {
		return fmt.Errorf("%s: %w", ErrAddReaderSubject, err)
	}

	subject := rbacv1.Subject{
		Kind:      rbacv1.ServiceAccountKind,
		Name:      saName,
		Namespace: namespace,
	}

	for _, bindingName := range names {
		if err := addSubjectToBinding(ctx, c, bindingName, subject); err != nil {
			return err
		}
	}
	return nil
}

func addSubjectToBinding(ctx context.Context, c client.Client, bindingName string, subject rbacv1.Subject) error {
	for attempts := 0; attempts < 3; attempts++ {
		crb := &rbacv1.ClusterRoleBinding{}
		if err := c.Get(ctx, client.ObjectKey{Name: bindingName}, crb); err != nil {
			return fmt.Errorf("%s %s: %w", ErrAddReaderSubject, bindingName, err)
		}

		for _, s := range crb.Subjects {
			if s.Kind == subject.Kind && s.Name == subject.Name && s.Namespace == subject.Namespace {
				return nil
			}
		}

		crb.Subjects = append(crb.Subjects, subject)
		err := c.Update(ctx, crb)
		if err == nil {
			return nil
		}
		if !apierrors.IsConflict(err) {
			return fmt.Errorf("%s %s: %w", ErrAddReaderSubject, bindingName, err)
		}
	}
	return fmt.Errorf("%s: conflict after retries", ErrAddReaderSubject)
}

// removeReaderSubject removes the SA from every cached reader ClusterRoleBinding.
// Idempotent — no-op if the subject is not present. Returns nil when no bindings
// are found (they may have been deleted during teardown); propagates all other errors.
func removeReaderSubject(ctx context.Context, c client.Client, saName, namespace string) error {
	names, err := resolveReaderBindings(ctx, c, namespace)
	if err != nil {
		if strings.Contains(err.Error(), "no ClusterRoleBinding found") {
			return nil
		}
		return fmt.Errorf("%s: %w", ErrRemoveReaderSubject, err)
	}

	for _, bindingName := range names {
		if err := removeSubjectFromBinding(ctx, c, bindingName, saName, namespace); err != nil {
			return err
		}
	}
	return nil
}

func removeSubjectFromBinding(ctx context.Context, c client.Client, bindingName, saName, namespace string) error {
	for attempts := 0; attempts < 3; attempts++ {
		crb := &rbacv1.ClusterRoleBinding{}
		if err := c.Get(ctx, client.ObjectKey{Name: bindingName}, crb); err != nil {
			if apierrors.IsNotFound(err) {
				return nil // binding gone, nothing to remove
			}
			return fmt.Errorf("%s %s: %w", ErrRemoveReaderSubject, bindingName, err)
		}

		filtered := make([]rbacv1.Subject, 0, len(crb.Subjects))
		for _, s := range crb.Subjects {
			if s.Kind == rbacv1.ServiceAccountKind && s.Name == saName && s.Namespace == namespace {
				continue
			}
			filtered = append(filtered, s)
		}

		if len(filtered) == len(crb.Subjects) {
			return nil
		}

		crb.Subjects = filtered
		err := c.Update(ctx, crb)
		if err == nil {
			return nil
		}
		if !apierrors.IsConflict(err) {
			return fmt.Errorf("%s %s: %w", ErrRemoveReaderSubject, bindingName, err)
		}
	}
	return fmt.Errorf("%s: conflict after retries", ErrRemoveReaderSubject)
}

// ensureExecutionRBAC creates Role+RoleBinding (namespace-scoped) and
// ClusterRole+ClusterRoleBinding (cluster-scoped) from the selected option's RBAC result.
// All bindings reference the per-run SA for isolation between concurrent AgenticRuns.
// SA creation is handled by SandboxManager.Create. Idempotent.
func ensureExecutionRBAC(
	ctx context.Context,
	c client.Client,
	run *agenticv1alpha1.AgenticRun,
	rbacResult *agenticv1alpha1.RBACResult,
	operatorNS string,
) error {
	if rbacResult == nil {
		return nil
	}

	saName := sandboxSAName(run, "execution")
	roleName := executionRoleName(string(run.UID))
	labels := rbacLabels(string(run.UID), "execution-rbac")

	subjects := []rbacv1.Subject{{
		Kind:      rbacv1.ServiceAccountKind,
		Name:      saName,
		Namespace: operatorNS,
	}}

	if len(rbacResult.NamespaceScoped) > 0 {
		nsRules := rbacRulesToPolicyRules(rbacResult.NamespaceScoped)
		targetNS := rbacTargetNamespaces(run, rbacResult)

		if len(targetNS) > 0 {
			if run.Annotations == nil {
				run.Annotations = make(map[string]string)
			}
			run.Annotations[rbacNamespacesAnnotation] = strings.Join(targetNS, ",")
		}

		for _, ns := range targetNS {
			role := &rbacv1.Role{
				ObjectMeta: metav1.ObjectMeta{Name: roleName, Namespace: ns, Labels: labels},
				Rules:      nsRules,
			}
			if err := c.Create(ctx, role); err != nil && !apierrors.IsAlreadyExists(err) {
				return fmt.Errorf("%s %s: %w", ErrCreateRole, ns, err)
			}
			binding := &rbacv1.RoleBinding{
				ObjectMeta: metav1.ObjectMeta{Name: roleName, Namespace: ns, Labels: labels},
				RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: roleName},
				Subjects:   subjects,
			}
			if err := c.Create(ctx, binding); err != nil && !apierrors.IsAlreadyExists(err) {
				return fmt.Errorf("%s %s: %w", ErrCreateRoleBinding, ns, err)
			}
		}
	}

	if len(rbacResult.ClusterScoped) > 0 {
		crName := clusterRoleName(string(run.UID))
		clusterRules := rbacRulesToPolicyRules(rbacResult.ClusterScoped)
		cr := &rbacv1.ClusterRole{
			ObjectMeta: metav1.ObjectMeta{Name: crName, Labels: labels},
			Rules:      clusterRules,
		}
		if err := c.Create(ctx, cr); err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("%s %s: %w", ErrCreateClusterRole, crName, err)
		}
		crb := &rbacv1.ClusterRoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: crName, Labels: labels},
			RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: crName},
			Subjects:   subjects,
		}
		if err := c.Create(ctx, crb); err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("%s %s: %w", ErrCreateClusterRoleBinding, crName, err)
		}
	}

	return nil
}

// cleanupExecutionRBAC removes all RBAC resources and the per-run SA created for
// a run's execution. Uses the annotation to find namespaces (survives retry clearing Steps).
func cleanupExecutionRBAC(ctx context.Context, c client.Client, run *agenticv1alpha1.AgenticRun) error {
	roleName := executionRoleName(string(run.UID))

	nsList := annotatedRBACNamespaces(run)

	for _, ns := range nsList {
		if err := deleteIfExists(ctx, c, &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: roleName, Namespace: ns}}); err != nil {
			return fmt.Errorf("%s %s: %w", ErrDeleteRoleBinding, ns, err)
		}
		if err := deleteIfExists(ctx, c, &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: roleName, Namespace: ns}}); err != nil {
			return fmt.Errorf("%s %s: %w", ErrDeleteRole, ns, err)
		}
	}

	crName := clusterRoleName(string(run.UID))
	if err := deleteIfExists(ctx, c, &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: crName}}); err != nil {
		return fmt.Errorf("%s %s: %w", ErrDeleteClusterRoleBinding, crName, err)
	}
	if err := deleteIfExists(ctx, c, &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: crName}}); err != nil {
		return fmt.Errorf("%s %s: %w", ErrDeleteClusterRole, crName, err)
	}

	return nil
}

func annotatedRBACNamespaces(run *agenticv1alpha1.AgenticRun) []string {
	if run.Annotations == nil {
		return nil
	}
	val := run.Annotations[rbacNamespacesAnnotation]
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

func rbacTargetNamespaces(run *agenticv1alpha1.AgenticRun, rbacResult *agenticv1alpha1.RBACResult) []string {
	if len(run.Spec.TargetNamespaces) > 0 {
		return run.Spec.TargetNamespaces
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

// stepResultResource maps a sandbox step name to its Result CRD resource name.
var stepResultResource = map[string]string{
	"analysis":     "analysisresults",
	"execution":    "executionresults",
	"verification": "verificationresults",
	"escalation":   "escalationresults",
}

// resultRoleName uses the run UID for fixed-length, collision-free names.
func resultRoleName(runUID, step string) string {
	return fmt.Sprintf("ls-result-%s-%s", step, runUID)
}

// ensureResultRBAC creates a per-run, per-step Role+RoleBinding in the operator
// namespace granting the sandbox SA permission to create the specific Result CR
// for this step and patch its status. Idempotent.
func ensureResultRBAC(ctx context.Context, c client.Client, run *agenticv1alpha1.AgenticRun, step, serviceAccount, operatorNS string) error {
	resource, ok := stepResultResource[step]
	if !ok {
		return fmt.Errorf("unknown step %q for result RBAC", step)
	}

	roleName := resultRoleName(string(run.UID), step)
	labels := rbacLabels(string(run.UID), "result-rbac")
	resultName := resultCRName(run.Name, step, nextResultIndex(run, step))

	rules := []rbacv1.PolicyRule{
		{
			// create cannot be name-scoped — the object doesn't exist at authz time.
			APIGroups: []string{"agentic.openshift.io"},
			Resources: []string{resource},
			Verbs:     []string{"create"},
		},
		{
			APIGroups:     []string{"agentic.openshift.io"},
			Resources:     []string{resource, resource + "/status"},
			ResourceNames: []string{resultName},
			Verbs:         []string{"get", "patch", "update"},
		},
	}
	if step == "escalation" {
		rules = append(rules, rbacv1.PolicyRule{
			APIGroups: []string{"agentic.openshift.io"},
			Resources: []string{"analysisresults", "executionresults", "verificationresults"},
			Verbs:     []string{"get", "list"},
		})
	}

	role := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{Name: roleName, Namespace: operatorNS, Labels: labels},
		Rules:      rules,
	}
	if err := c.Create(ctx, role); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create result Role %s: %w", roleName, err)
	}

	binding := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: roleName, Namespace: operatorNS, Labels: labels},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: roleName},
		Subjects: []rbacv1.Subject{{
			Kind:      rbacv1.ServiceAccountKind,
			Name:      serviceAccount,
			Namespace: operatorNS,
		}},
	}
	if err := c.Create(ctx, binding); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create result RoleBinding %s: %w", roleName, err)
	}

	return nil
}

// setResultRBACOwner sets the pod/claim as owner on the per-step result RBAC
// Role and RoleBinding so Kubernetes GC cleans them up automatically.
func setResultRBACOwner(ctx context.Context, c client.Client, runUID, step string, owner metav1.OwnerReference, operatorNS string) error {
	roleName := resultRoleName(runUID, step)

	role := &rbacv1.Role{}
	if err := c.Get(ctx, client.ObjectKey{Name: roleName, Namespace: operatorNS}, role); err != nil {
		return fmt.Errorf("get result Role %s: %w", roleName, err)
	}
	baseRole := role.DeepCopy()
	role.OwnerReferences = append(role.OwnerReferences, owner)
	if err := c.Patch(ctx, role, client.MergeFrom(baseRole)); err != nil {
		return fmt.Errorf("set owner on result Role %s: %w", roleName, err)
	}

	binding := &rbacv1.RoleBinding{}
	if err := c.Get(ctx, client.ObjectKey{Name: roleName, Namespace: operatorNS}, binding); err != nil {
		return fmt.Errorf("get result RoleBinding %s: %w", roleName, err)
	}
	baseBinding := binding.DeepCopy()
	binding.OwnerReferences = append(binding.OwnerReferences, owner)
	if err := c.Patch(ctx, binding, client.MergeFrom(baseBinding)); err != nil {
		return fmt.Errorf("set owner on result RoleBinding %s: %w", roleName, err)
	}

	return nil
}

func truncateK8sName(name string) string {
	if len(name) > 63 {
		name = strings.TrimRight(name[:63], "-._")
	}
	return name
}

func executionRoleName(agenticRunName string) string {
	return truncateK8sName("ls-exec-" + agenticRunName)
}

func clusterRoleName(agenticRunName string) string {
	return truncateK8sName("ls-exec-cluster-" + agenticRunName)
}

func rbacLabels(runUID, component string) map[string]string {
	return map[string]string{
		LabelRun:       runUID,
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
