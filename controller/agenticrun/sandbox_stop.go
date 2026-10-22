package agenticrun

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
)

// ErrRunStopAccepted indicates persisted termination prevented sandbox creation.
var ErrRunStopAccepted = errors.New("run stop accepted; sandbox creation aborted")

// ErrRunReplaced indicates the AgenticRun was deleted and recreated under the
// same name while a sandbox launch was in flight. The replacement is a
// different object with its own approvals and must not inherit this launch.
var ErrRunReplaced = errors.New("run was replaced during sandbox creation")

var sandboxStopSteps = [...]string{"analysis", "execution", "verification", "escalation"}

var sandboxWarmPoolGVK = schema.GroupVersionKind{
	Group: "extensions.agents.x-k8s.io", Version: "v1beta1", Kind: "SandboxWarmPool",
}

const sandboxPodNameAnnotation = "agents.x-k8s.io/pod-name"

// stopPendingRefsAnnotation records the namespace/name of Pods and Sandboxes
// that teardown discovered but could not yet confirm removed. Deleting a
// Sandbox or Claim destroys the owner chain that identifies a
// finalizer-retained backing Pod, so the identities are retained on the run and
// seeded back into discovery on the next attempt. Without this, a retry can
// find nothing and wrongly confirm teardown while a workload still exists.
const stopPendingRefsAnnotation = "agentic.openshift.io/stop-pending-refs"

type stopPendingRefs struct {
	Pods      []string `json:"pods,omitempty"`
	Sandboxes []string `json:"sandboxes,omitempty"`
}

func (i *sandboxStopInventory) pendingRefs() stopPendingRefs {
	refs := stopPendingRefs{}
	for key := range i.seenPods {
		refs.Pods = append(refs.Pods, key.String())
	}
	for key := range i.seenSandboxes {
		refs.Sandboxes = append(refs.Sandboxes, key.String())
	}
	sort.Strings(refs.Pods)
	sort.Strings(refs.Sandboxes)
	return refs
}

func (refs stopPendingRefs) empty() bool {
	return len(refs.Pods) == 0 && len(refs.Sandboxes) == 0
}

func pendingStopRefsFromRun(run *agenticv1alpha1.AgenticRun) (pods, sandboxes map[types.NamespacedName]struct{}) {
	pods = map[types.NamespacedName]struct{}{}
	sandboxes = map[types.NamespacedName]struct{}{}
	var refs stopPendingRefs
	if err := json.Unmarshal([]byte(run.Annotations[stopPendingRefsAnnotation]), &refs); err != nil {
		return pods, sandboxes
	}
	add := func(values []string, target map[types.NamespacedName]struct{}) {
		for _, value := range values {
			namespace, name, found := strings.Cut(value, "/")
			if found && namespace != "" && name != "" {
				target[types.NamespacedName{Namespace: namespace, Name: name}] = struct{}{}
			}
		}
	}
	add(refs.Pods, pods)
	add(refs.Sandboxes, sandboxes)
	return pods, sandboxes
}

// runMustNotAdvance guards sandbox creation against termination written by a
// different reconciler or a concurrent API update. Terminal runs are never
// eligible to start another step.
func runMustNotAdvance(run *agenticv1alpha1.AgenticRun) bool {
	if run == nil || run.Spec.Cancelled {
		return true
	}
	stopping := meta.FindStatusCondition(run.Status.Conditions, agenticv1alpha1.AgenticRunConditionStopping)
	if stopping != nil && stopping.Status == metav1.ConditionTrue {
		return true
	}
	return isTerminal(agenticv1alpha1.DerivePhase(run.Status.Conditions))
}

// StopSandboxes independently discovers and terminates all resources associated
// with a run. It never changes the run status; the lifecycle controller clears
// sandbox references only after this method confirms teardown. Descendant
// identities needed to observe finalizer-retained workloads are recorded on the
// run as metadata until teardown is confirmed.
func (m *SandboxManager) StopSandboxes(ctx context.Context, run *agenticv1alpha1.AgenticRun) (bool, error) {
	if run == nil {
		return false, errors.New("stop sandboxes: AgenticRun is nil")
	}
	if run.UID == "" {
		return false, errors.New("stop sandboxes: AgenticRun UID is empty")
	}

	reader := m.apiReader()
	// Work from the latest metadata so refs recorded by an earlier attempt are
	// seeded into discovery even if the caller's copy is stale.
	latest := &agenticv1alpha1.AgenticRun{}
	if err := reader.Get(ctx, client.ObjectKeyFromObject(run), latest); err == nil {
		run = latest
	}
	inventory, stopErrs := m.discoverRunResources(ctx, reader, run, nil, nil, nil, nil)
	// Record identities before deleting the owner chain (see
	// stopPendingRefsAnnotation). A failure here must prevent confirmation.
	if err := m.syncPendingStopRefs(ctx, run, inventory.pendingRefs()); err != nil {
		stopErrs = append(stopErrs, err)
	}

	spoke, spokeErr := spokeAccessForRun(ctx, m.client, run, m.namespace)
	if spokeErr != nil {
		stopErrs = append(stopErrs, fmt.Errorf("resolve spoke access: %w", spokeErr))
		spoke = nil
	}

	saNames := sandboxServiceAccountNames(run)
	rbacNames := sandboxRBACNamesForRun(run)
	hubSANamespaces := []string{m.namespace}
	if spoke != nil {
		hubSANamespaces = append(hubSANamespaces, spoke.Namespace)
	}
	stopErrs = append(stopErrs, m.revokeSandboxAccess(ctx, reader, m.client, run, inventory.ownerUIDs, rbacNames, saNames, hubSANamespaces)...)
	if spoke != nil {
		stopErrs = append(stopErrs, m.revokeSandboxAccess(ctx, spoke.Client, spoke.Client, run, inventory.ownerUIDs, rbacNames, saNames, []string{spoke.Namespace})...)
	}

	// Stop replenishment before releasing claims. The current upstream pool
	// controller treats an explicit replicas=0 as the desired size; deleting
	// the run-owned pool also prevents a later reconcile from recreating work.
	for _, pool := range inventory.pools {
		if err := m.scaleWarmPoolToZero(ctx, pool); err != nil {
			stopErrs = append(stopErrs, err)
		}
	}

	for _, pod := range inventory.pods {
		if err := m.client.Delete(ctx, pod, stopDeleteOptions(pod, client.GracePeriodSeconds(0))...); err != nil && !apierrors.IsNotFound(err) {
			stopErrs = append(stopErrs, fmt.Errorf("delete Pod %s/%s with zero grace: %w", pod.Namespace, pod.Name, err))
		}
	}
	for _, sandbox := range inventory.sandboxes {
		if err := deleteStopResource(ctx, m.client, sandbox, "Sandbox"); err != nil {
			stopErrs = append(stopErrs, err)
		}
	}
	for _, claim := range inventory.claims {
		if err := deleteStopResource(ctx, m.client, claim, "SandboxClaim"); err != nil {
			stopErrs = append(stopErrs, err)
		}
	}
	for _, pool := range inventory.pools {
		if err := deleteStopResource(ctx, m.client, pool, "SandboxWarmPool"); err != nil {
			stopErrs = append(stopErrs, err)
		}
	}
	stopErrs = append(stopErrs, m.deleteRunArtifacts(ctx, reader, run)...)

	// Confirm with authoritative reads. A successful Delete request is not
	// completion: resources with finalizers remain in this inventory until
	// the API server removes them.
	remaining, confirmErrs := m.discoverRunResources(ctx, reader, run, inventory.ownerUIDs, inventory.podRefs, inventory.sandboxRefs, inventory.poolRefs)
	stopErrs = append(stopErrs, confirmErrs...)
	pending := remaining.hasWorkloads()

	artifactsRemain, artifactErrs := m.runArtifactsRemain(ctx, reader, run)
	stopErrs = append(stopErrs, artifactErrs...)
	pending = pending || artifactsRemain

	hubAccessRemains, hubAccessErrs := sandboxAccessRemains(ctx, reader, run, inventory.ownerUIDs, rbacNames, saNames, hubSANamespaces)
	stopErrs = append(stopErrs, hubAccessErrs...)
	pending = pending || hubAccessRemains

	if run.Spec.TargetCluster != "" {
		if spoke == nil {
			pending = true
		} else {
			spokeAccessRemains, spokeAccessErrs := sandboxAccessRemains(ctx, spoke.Client, run, inventory.ownerUIDs, rbacNames, saNames, []string{spoke.Namespace})
			stopErrs = append(stopErrs, spokeAccessErrs...)
			pending = pending || spokeAccessRemains
		}
	}

	if err := errors.Join(stopErrs...); err != nil {
		return false, err
	}
	if pending {
		return false, nil
	}
	if err := m.syncPendingStopRefs(ctx, run, stopPendingRefs{}); err != nil {
		return false, err
	}
	return true, nil
}

func (m *SandboxManager) apiReader() client.Reader {
	if m.APIReader != nil {
		return m.APIReader
	}
	return m.client
}

// syncPendingStopRefs records the discovered descendant identities on the run.
// It is called before owners are deleted so a later attempt can still observe a
// finalizer-retained Pod. With no discovered refs it removes the annotation.
func (m *SandboxManager) syncPendingStopRefs(ctx context.Context, run *agenticv1alpha1.AgenticRun, refs stopPendingRefs) error {
	desired := ""
	if !refs.empty() {
		encoded, marshalErr := json.Marshal(refs)
		if marshalErr != nil {
			return fmt.Errorf("encode pending stop refs: %w", marshalErr)
		}
		desired = string(encoded)
	}

	current := &agenticv1alpha1.AgenticRun{}
	if err := m.apiReader().Get(ctx, client.ObjectKeyFromObject(run), current); err != nil {
		return fmt.Errorf("read AgenticRun for pending stop refs: %w", err)
	}
	if current.Annotations[stopPendingRefsAnnotation] == desired {
		return nil
	}
	base := current.DeepCopy()
	if desired == "" {
		delete(current.Annotations, stopPendingRefsAnnotation)
	} else {
		if current.Annotations == nil {
			current.Annotations = map[string]string{}
		}
		current.Annotations[stopPendingRefsAnnotation] = desired
	}
	if err := m.client.Patch(ctx, current, client.MergeFrom(base)); err != nil {
		return fmt.Errorf("record pending stop refs: %w", err)
	}
	return nil
}

type sandboxStopInventory struct {
	pods          []*corev1.Pod
	claims        []*unstructured.Unstructured
	sandboxes     []*unstructured.Unstructured
	pools         []*unstructured.Unstructured
	ownerUIDs     map[types.UID]struct{}
	statusRefs    map[types.NamespacedName]struct{}
	podRefs       map[types.NamespacedName]struct{}
	sandboxRefs   map[types.NamespacedName]struct{}
	poolRefs      map[types.NamespacedName]struct{}
	seenPods      map[types.NamespacedName]struct{}
	seenClaims    map[types.NamespacedName]struct{}
	seenSandboxes map[types.NamespacedName]struct{}
	seenPools     map[types.NamespacedName]struct{}
}

func newSandboxStopInventory(run *agenticv1alpha1.AgenticRun, defaultNamespace string, seedOwners map[types.UID]struct{}, seedPods, seedSandboxes, seedPools map[types.NamespacedName]struct{}) *sandboxStopInventory {
	inventory := &sandboxStopInventory{
		ownerUIDs:     make(map[types.UID]struct{}, len(seedOwners)+1),
		statusRefs:    sandboxStatusReferences(run, defaultNamespace),
		podRefs:       make(map[types.NamespacedName]struct{}, len(seedPods)),
		sandboxRefs:   make(map[types.NamespacedName]struct{}, len(seedSandboxes)),
		poolRefs:      make(map[types.NamespacedName]struct{}, len(seedPools)),
		seenPods:      make(map[types.NamespacedName]struct{}),
		seenClaims:    make(map[types.NamespacedName]struct{}),
		seenSandboxes: make(map[types.NamespacedName]struct{}),
		seenPools:     make(map[types.NamespacedName]struct{}),
	}
	for uid := range seedOwners {
		inventory.addOwnerUID(uid)
	}
	inventory.addOwnerUID(run.UID)
	for ref := range seedPods {
		inventory.podRefs[ref] = struct{}{}
	}
	for ref := range seedSandboxes {
		inventory.sandboxRefs[ref] = struct{}{}
	}
	for ref := range seedPools {
		inventory.poolRefs[ref] = struct{}{}
	}
	for ref := range inventory.statusRefs {
		inventory.podRefs[ref] = struct{}{}
		inventory.sandboxRefs[ref] = struct{}{}
	}
	retainedPods, retainedSandboxes := pendingStopRefsFromRun(run)
	for ref := range retainedPods {
		inventory.podRefs[ref] = struct{}{}
	}
	for ref := range retainedSandboxes {
		inventory.sandboxRefs[ref] = struct{}{}
	}
	return inventory
}

func (i *sandboxStopInventory) addOwnerUID(uid types.UID) {
	if uid != "" {
		i.ownerUIDs[uid] = struct{}{}
	}
}

func (i *sandboxStopInventory) addPod(pod *corev1.Pod) {
	if pod == nil {
		return
	}
	key := types.NamespacedName{Namespace: pod.Namespace, Name: pod.Name}
	if _, found := i.seenPods[key]; found {
		return
	}
	i.seenPods[key] = struct{}{}
	i.pods = append(i.pods, pod.DeepCopy())
	i.addOwnerUID(pod.UID)
}

func (i *sandboxStopInventory) addClaim(claim *unstructured.Unstructured) {
	if claim == nil {
		return
	}
	key := types.NamespacedName{Namespace: claim.GetNamespace(), Name: claim.GetName()}
	if _, found := i.seenClaims[key]; found {
		return
	}
	i.seenClaims[key] = struct{}{}
	copy := claim.DeepCopy()
	i.claims = append(i.claims, copy)
	i.addOwnerUID(claim.GetUID())
	if name := sandboxNameFromClaim(claim); name != "" {
		i.sandboxRefs[types.NamespacedName{Namespace: claim.GetNamespace(), Name: name}] = struct{}{}
	}
	if name := warmPoolNameFromClaim(claim); name != "" {
		i.poolRefs[types.NamespacedName{Namespace: claim.GetNamespace(), Name: name}] = struct{}{}
	}
}

func (i *sandboxStopInventory) addSandbox(sandbox *unstructured.Unstructured) {
	if sandbox == nil {
		return
	}
	key := types.NamespacedName{Namespace: sandbox.GetNamespace(), Name: sandbox.GetName()}
	if _, found := i.seenSandboxes[key]; found {
		return
	}
	i.seenSandboxes[key] = struct{}{}
	copy := sandbox.DeepCopy()
	i.sandboxes = append(i.sandboxes, copy)
	i.addOwnerUID(sandbox.GetUID())
	podName := sandbox.GetName()
	if annotation := sandbox.GetAnnotations()[sandboxPodNameAnnotation]; annotation != "" {
		podName = annotation
	}
	i.podRefs[types.NamespacedName{Namespace: sandbox.GetNamespace(), Name: podName}] = struct{}{}
}

func (i *sandboxStopInventory) addPool(pool *unstructured.Unstructured) {
	if pool == nil {
		return
	}
	key := types.NamespacedName{Namespace: pool.GetNamespace(), Name: pool.GetName()}
	if _, found := i.seenPools[key]; found {
		return
	}
	i.seenPools[key] = struct{}{}
	i.pools = append(i.pools, pool.DeepCopy())
	i.addOwnerUID(pool.GetUID())
	i.poolRefs[key] = struct{}{}
}

func (i *sandboxStopInventory) hasWorkloads() bool {
	return len(i.pods) != 0 || len(i.claims) != 0 || len(i.sandboxes) != 0 || len(i.pools) != 0
}

func sandboxStatusReferences(run *agenticv1alpha1.AgenticRun, defaultNamespace string) map[types.NamespacedName]struct{} {
	refs := make(map[types.NamespacedName]struct{}, 4)
	infos := [...]agenticv1alpha1.SandboxInfo{
		run.Status.Steps.Analysis.Sandbox,
		run.Status.Steps.Execution.Sandbox,
		run.Status.Steps.Verification.Sandbox,
		run.Status.Steps.Escalation.Sandbox,
	}
	for _, info := range infos {
		if info.ClaimName == "" {
			continue
		}
		namespace := info.Namespace
		if namespace == "" {
			namespace = defaultNamespace
		}
		refs[types.NamespacedName{Namespace: namespace, Name: info.ClaimName}] = struct{}{}
	}
	return refs
}

func (m *SandboxManager) discoverRunResources(ctx context.Context, reader client.Reader, run *agenticv1alpha1.AgenticRun, seedOwners map[types.UID]struct{}, seedPods, seedSandboxes, seedPools map[types.NamespacedName]struct{}) (*sandboxStopInventory, []error) {
	inventory := newSandboxStopInventory(run, m.namespace, seedOwners, seedPods, seedSandboxes, seedPools)
	var errs []error

	// Status references are a second discovery path. A reference can identify
	// a bare Pod or a SandboxClaim and remains useful when labels were lost.
	for ref := range inventory.statusRefs {
		pod := &corev1.Pod{}
		if err := reader.Get(ctx, ref, pod); err != nil {
			if !apierrors.IsNotFound(err) {
				errs = append(errs, fmt.Errorf("get status-referenced Pod %s/%s: %w", ref.Namespace, ref.Name, err))
			}
		} else {
			inventory.addPod(pod)
		}
		claim, err := getStopUnstructured(ctx, reader, smClaimGVK, ref)
		if err != nil {
			errs = append(errs, fmt.Errorf("get status-referenced SandboxClaim %s/%s: %w", ref.Namespace, ref.Name, err))
		} else {
			inventory.addClaim(claim)
		}
		sandbox, err := getStopUnstructured(ctx, reader, smSandboxGVK, ref)
		if err != nil {
			errs = append(errs, fmt.Errorf("get status-referenced Sandbox %s/%s: %w", ref.Namespace, ref.Name, err))
		} else {
			inventory.addSandbox(sandbox)
		}
	}

	namespaces := inventory.namespaces(m.namespace)
	for _, namespace := range namespaces {
		claims, err := listStopUnstructured(ctx, reader, smClaimGVK, namespace)
		if err != nil {
			errs = append(errs, err)
		} else {
			for idx := range claims {
				claim := &claims[idx]
				if stopObjectMatchesRun(claim, string(run.UID), inventory.ownerUIDs, inventory.statusRefs) {
					inventory.addClaim(claim)
				}
			}
		}
	}

	for _, namespace := range namespaces {
		pools, err := listStopUnstructured(ctx, reader, sandboxWarmPoolGVK, namespace)
		if err != nil {
			errs = append(errs, err)
		} else {
			for idx := range pools {
				pool := &pools[idx]
				if stopObjectMatchesRun(pool, string(run.UID), inventory.ownerUIDs, nil) {
					inventory.addPool(pool)
				}
			}
		}
	}

	for _, namespace := range namespaces {
		sandboxes, err := listStopUnstructured(ctx, reader, smSandboxGVK, namespace)
		if err != nil {
			errs = append(errs, err)
		} else {
			for idx := range sandboxes {
				sandbox := &sandboxes[idx]
				if stopObjectMatchesRun(sandbox, string(run.UID), inventory.ownerUIDs, inventory.sandboxRefs) {
					inventory.addSandbox(sandbox)
				}
			}
		}
	}

	// An upstream Sandbox can be owned by a SandboxClaim, and its Pod is
	// separately owned by the Sandbox. Resolve recorded Sandbox names before
	// deleting any Claim so the backing Pod's owner UID is not lost.
	for ref := range inventory.sandboxRefs {
		sandbox, err := getStopUnstructured(ctx, reader, smSandboxGVK, ref)
		if err != nil {
			errs = append(errs, fmt.Errorf("get referenced Sandbox %s/%s: %w", ref.Namespace, ref.Name, err))
		} else {
			inventory.addSandbox(sandbox)
		}
	}

	for _, namespace := range namespaces {
		var pods corev1.PodList
		if err := reader.List(ctx, &pods, client.InNamespace(namespace)); err != nil {
			errs = append(errs, fmt.Errorf("list Pods in %s: %w", namespace, err))
		} else {
			for idx := range pods.Items {
				pod := &pods.Items[idx]
				if stopObjectMatchesRun(pod, string(run.UID), inventory.ownerUIDs, inventory.podRefs) {
					inventory.addPod(pod)
				}
			}
		}
	}

	// Direct reads cover a just-created or informer-lagged backing Pod even if
	// its Sandbox or Claim has already started deletion.
	for ref := range inventory.podRefs {
		pod := &corev1.Pod{}
		if err := reader.Get(ctx, ref, pod); err != nil {
			if !apierrors.IsNotFound(err) {
				errs = append(errs, fmt.Errorf("get backing Pod %s/%s: %w", ref.Namespace, ref.Name, err))
			}
		} else {
			inventory.addPod(pod)
		}
	}

	return inventory, errs
}

func (i *sandboxStopInventory) namespaces(defaultNamespace string) []string {
	set := map[string]struct{}{defaultNamespace: {}}
	for ref := range i.statusRefs {
		set[ref.Namespace] = struct{}{}
	}
	for ref := range i.podRefs {
		set[ref.Namespace] = struct{}{}
	}
	for ref := range i.sandboxRefs {
		set[ref.Namespace] = struct{}{}
	}
	for ref := range i.poolRefs {
		set[ref.Namespace] = struct{}{}
	}
	namespaces := make([]string, 0, len(set))
	for namespace := range set {
		if namespace != "" {
			namespaces = append(namespaces, namespace)
		}
	}
	sort.Strings(namespaces)
	return namespaces
}

func stopObjectMatchesRun(obj metav1.Object, runUID string, ownerUIDs map[types.UID]struct{}, refs map[types.NamespacedName]struct{}) bool {
	if obj == nil {
		return false
	}
	if runUID != "" && obj.GetLabels()[LabelRun] == runUID {
		return true
	}
	for _, owner := range obj.GetOwnerReferences() {
		if _, found := ownerUIDs[owner.UID]; found && owner.UID != "" {
			return true
		}
	}
	_, found := refs[types.NamespacedName{Namespace: obj.GetNamespace(), Name: obj.GetName()}]
	return found
}

func listStopUnstructured(ctx context.Context, reader client.Reader, gvk schema.GroupVersionKind, namespace string) ([]unstructured.Unstructured, error) {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(gvk.GroupVersion().WithKind(gvk.Kind + "List"))
	if err := reader.List(ctx, list, client.InNamespace(namespace)); err != nil {
		if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("list %s in %s: %w", gvk.Kind, namespace, err)
	}
	for idx := range list.Items {
		list.Items[idx].SetGroupVersionKind(gvk)
	}
	return list.Items, nil
}

func getStopUnstructured(ctx context.Context, reader client.Reader, gvk schema.GroupVersionKind, key types.NamespacedName) (*unstructured.Unstructured, error) {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gvk)
	if err := reader.Get(ctx, key, obj); err != nil {
		if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
			return nil, nil
		}
		return nil, err
	}
	return obj, nil
}

func sandboxNameFromClaim(claim *unstructured.Unstructured) string {
	for _, path := range [][]string{{"status", "sandbox", "name"}, {"status", "sandboxStatus", "name"}} {
		name, found, err := unstructured.NestedString(claim.Object, path...)
		if err == nil && found && name != "" {
			return name
		}
	}
	return ""
}

func warmPoolNameFromClaim(claim *unstructured.Unstructured) string {
	name, found, err := unstructured.NestedString(claim.Object, "spec", "warmPoolRef", "name")
	if err == nil && found {
		return name
	}
	return ""
}

func deleteStopResource(ctx context.Context, c client.Client, obj client.Object, kind string) error {
	if err := c.Delete(ctx, obj, stopDeleteOptions(obj)...); err != nil && !apierrors.IsNotFound(err) && !meta.IsNoMatchError(err) {
		return fmt.Errorf("delete %s %s/%s: %w", kind, obj.GetNamespace(), obj.GetName(), err)
	}
	return nil
}

func stopDeleteOptions(obj client.Object, extra ...client.DeleteOption) []client.DeleteOption {
	options := make([]client.DeleteOption, 0, len(extra)+1)
	if uid := obj.GetUID(); uid != "" {
		options = append(options, client.Preconditions(metav1.Preconditions{UID: &uid}))
	}
	return append(options, extra...)
}

func (m *SandboxManager) scaleWarmPoolToZero(ctx context.Context, pool *unstructured.Unstructured) error {
	if deletionTimestamp := pool.GetDeletionTimestamp(); deletionTimestamp != nil && !deletionTimestamp.IsZero() {
		return nil
	}
	replicas, found, err := unstructured.NestedInt64(pool.Object, "spec", "replicas")
	if err != nil {
		return fmt.Errorf("read SandboxWarmPool %s/%s replicas: %w", pool.GetNamespace(), pool.GetName(), err)
	}
	if found && replicas == 0 {
		return nil
	}
	base := pool.DeepCopy()
	if err := unstructured.SetNestedField(pool.Object, int64(0), "spec", "replicas"); err != nil {
		return fmt.Errorf("set SandboxWarmPool %s/%s replicas to zero: %w", pool.GetNamespace(), pool.GetName(), err)
	}
	if err := m.client.Patch(ctx, pool, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
		return fmt.Errorf("scale SandboxWarmPool %s/%s to zero: %w", pool.GetNamespace(), pool.GetName(), err)
	}
	return nil
}

func sandboxServiceAccountNames(run *agenticv1alpha1.AgenticRun) map[string]struct{} {
	names := make(map[string]struct{}, len(sandboxStopSteps))
	for _, step := range sandboxStopSteps {
		names[sandboxSAName(run, step)] = struct{}{}
	}
	return names
}

type sandboxRBACNames struct {
	roles               map[string]struct{}
	roleBindings        map[string]struct{}
	clusterRoles        map[string]struct{}
	clusterRoleBindings map[string]struct{}
}

func sandboxRBACNamesForRun(run *agenticv1alpha1.AgenticRun) sandboxRBACNames {
	uid := string(run.UID)
	names := sandboxRBACNames{
		roles:               map[string]struct{}{executionRoleName(uid): {}},
		roleBindings:        map[string]struct{}{executionRoleName(uid): {}},
		clusterRoles:        map[string]struct{}{clusterRoleName(uid): {}},
		clusterRoleBindings: map[string]struct{}{clusterRoleName(uid): {}},
	}
	for _, step := range sandboxStopSteps {
		resultName := resultRoleName(uid, step)
		names.roles[resultName] = struct{}{}
		names.roleBindings[resultName] = struct{}{}
		for idx := range spokeReaderBindingNames {
			names.clusterRoleBindings[perRunCRBName(uid, step, idx)] = struct{}{}
		}
	}
	return names
}

func managedRBACObject(obj metav1.Object, run *agenticv1alpha1.AgenticRun, ownerUIDs map[types.UID]struct{}, names map[string]struct{}) bool {
	if stopObjectMatchesRun(obj, string(run.UID), ownerUIDs, nil) {
		return true
	}
	_, found := names[obj.GetName()]
	return found
}

func isRunServiceAccountSubject(subject rbacv1.Subject, saNames map[string]struct{}) bool {
	if subject.Kind != rbacv1.ServiceAccountKind {
		return false
	}
	_, found := saNames[subject.Name]
	return found
}

func removeRunServiceAccountSubjects(subjects []rbacv1.Subject, saNames map[string]struct{}) ([]rbacv1.Subject, bool) {
	remaining := make([]rbacv1.Subject, 0, len(subjects))
	changed := false
	for _, subject := range subjects {
		if isRunServiceAccountSubject(subject, saNames) {
			changed = true
			continue
		}
		remaining = append(remaining, subject)
	}
	return remaining, changed
}

func (m *SandboxManager) revokeSandboxAccess(ctx context.Context, reader client.Reader, writer client.Client, run *agenticv1alpha1.AgenticRun, ownerUIDs map[types.UID]struct{}, names sandboxRBACNames, saNames map[string]struct{}, saNamespaces []string) []error {
	var errs []error

	var roleBindings rbacv1.RoleBindingList
	if err := reader.List(ctx, &roleBindings); err != nil {
		errs = append(errs, fmt.Errorf("list RoleBindings while stopping run %s: %w", run.Name, err))
	} else {
		for idx := range roleBindings.Items {
			binding := &roleBindings.Items[idx]
			owned := managedRBACObject(binding, run, ownerUIDs, names.roleBindings)
			subjects, subjectRemoved := removeRunServiceAccountSubjects(binding.Subjects, saNames)
			if !owned && !subjectRemoved {
				continue
			}
			if owned || len(subjects) == 0 {
				if err := deleteStopResource(ctx, writer, binding, "RoleBinding"); err != nil {
					errs = append(errs, err)
				}
				continue
			}
			base := binding.DeepCopy()
			binding.Subjects = subjects
			if err := writer.Patch(ctx, binding, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
				errs = append(errs, fmt.Errorf("remove run ServiceAccount subjects from RoleBinding %s/%s: %w", binding.Namespace, binding.Name, err))
			}
		}
	}

	var clusterRoleBindings rbacv1.ClusterRoleBindingList
	if err := reader.List(ctx, &clusterRoleBindings); err != nil {
		errs = append(errs, fmt.Errorf("list ClusterRoleBindings while stopping run %s: %w", run.Name, err))
	} else {
		for idx := range clusterRoleBindings.Items {
			binding := &clusterRoleBindings.Items[idx]
			owned := managedRBACObject(binding, run, ownerUIDs, names.clusterRoleBindings)
			subjects, subjectRemoved := removeRunServiceAccountSubjects(binding.Subjects, saNames)
			if !owned && !subjectRemoved {
				continue
			}
			if owned || len(subjects) == 0 {
				if err := deleteStopResource(ctx, writer, binding, "ClusterRoleBinding"); err != nil {
					errs = append(errs, err)
				}
				continue
			}
			base := binding.DeepCopy()
			binding.Subjects = subjects
			if err := writer.Patch(ctx, binding, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
				errs = append(errs, fmt.Errorf("remove run ServiceAccount subjects from ClusterRoleBinding %s: %w", binding.Name, err))
			}
		}
	}

	var roles rbacv1.RoleList
	if err := reader.List(ctx, &roles); err != nil {
		errs = append(errs, fmt.Errorf("list Roles while stopping run %s: %w", run.Name, err))
	} else {
		for idx := range roles.Items {
			role := &roles.Items[idx]
			if managedRBACObject(role, run, ownerUIDs, names.roles) {
				if err := deleteStopResource(ctx, writer, role, "Role"); err != nil {
					errs = append(errs, err)
				}
			}
		}
	}

	var clusterRoles rbacv1.ClusterRoleList
	if err := reader.List(ctx, &clusterRoles); err != nil {
		errs = append(errs, fmt.Errorf("list ClusterRoles while stopping run %s: %w", run.Name, err))
	} else {
		for idx := range clusterRoles.Items {
			role := &clusterRoles.Items[idx]
			if managedRBACObject(role, run, ownerUIDs, names.clusterRoles) {
				if err := deleteStopResource(ctx, writer, role, "ClusterRole"); err != nil {
					errs = append(errs, err)
				}
			}
		}
	}

	for _, namespace := range uniqueStrings(saNamespaces) {
		for saName := range saNames {
			sa := &corev1.ServiceAccount{}
			key := types.NamespacedName{Namespace: namespace, Name: saName}
			if err := reader.Get(ctx, key, sa); err != nil {
				if !apierrors.IsNotFound(err) {
					errs = append(errs, fmt.Errorf("get sandbox ServiceAccount %s/%s: %w", key.Namespace, key.Name, err))
				}
				continue
			}
			if err := writer.Delete(ctx, sa, stopDeleteOptions(sa)...); err != nil && !apierrors.IsNotFound(err) {
				errs = append(errs, fmt.Errorf("delete sandbox ServiceAccount %s/%s: %w", key.Namespace, key.Name, err))
			}
		}
	}
	return errs
}

func sandboxAccessRemains(ctx context.Context, reader client.Reader, run *agenticv1alpha1.AgenticRun, ownerUIDs map[types.UID]struct{}, names sandboxRBACNames, saNames map[string]struct{}, saNamespaces []string) (bool, []error) {
	var errs []error
	remains := false

	var roleBindings rbacv1.RoleBindingList
	if err := reader.List(ctx, &roleBindings); err != nil {
		errs = append(errs, fmt.Errorf("confirm RoleBindings for run %s: %w", run.Name, err))
	} else {
		for idx := range roleBindings.Items {
			binding := &roleBindings.Items[idx]
			if managedRBACObject(binding, run, ownerUIDs, names.roleBindings) {
				remains = true
				break
			}
			for _, subject := range binding.Subjects {
				if isRunServiceAccountSubject(subject, saNames) {
					remains = true
					break
				}
			}
			if remains {
				break
			}
		}
	}

	var clusterRoleBindings rbacv1.ClusterRoleBindingList
	if err := reader.List(ctx, &clusterRoleBindings); err != nil {
		errs = append(errs, fmt.Errorf("confirm ClusterRoleBindings for run %s: %w", run.Name, err))
	} else {
		for idx := range clusterRoleBindings.Items {
			binding := &clusterRoleBindings.Items[idx]
			if managedRBACObject(binding, run, ownerUIDs, names.clusterRoleBindings) {
				remains = true
				break
			}
			for _, subject := range binding.Subjects {
				if isRunServiceAccountSubject(subject, saNames) {
					remains = true
					break
				}
			}
			if remains {
				break
			}
		}
	}

	var roles rbacv1.RoleList
	if err := reader.List(ctx, &roles); err != nil {
		errs = append(errs, fmt.Errorf("confirm Roles for run %s: %w", run.Name, err))
	} else {
		for idx := range roles.Items {
			if managedRBACObject(&roles.Items[idx], run, ownerUIDs, names.roles) {
				remains = true
				break
			}
		}
	}

	var clusterRoles rbacv1.ClusterRoleList
	if err := reader.List(ctx, &clusterRoles); err != nil {
		errs = append(errs, fmt.Errorf("confirm ClusterRoles for run %s: %w", run.Name, err))
	} else {
		for idx := range clusterRoles.Items {
			if managedRBACObject(&clusterRoles.Items[idx], run, ownerUIDs, names.clusterRoles) {
				remains = true
				break
			}
		}
	}

	for _, namespace := range uniqueStrings(saNamespaces) {
		for saName := range saNames {
			key := types.NamespacedName{Namespace: namespace, Name: saName}
			sa := &corev1.ServiceAccount{}
			if err := reader.Get(ctx, key, sa); err != nil {
				if !apierrors.IsNotFound(err) {
					errs = append(errs, fmt.Errorf("confirm sandbox ServiceAccount %s/%s: %w", key.Namespace, key.Name, err))
				}
				continue
			}
			remains = true
		}
	}
	return remains, errs
}

func uniqueStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	unique := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" {
			continue
		}
		if _, found := seen[value]; found {
			continue
		}
		seen[value] = struct{}{}
		unique = append(unique, value)
	}
	return unique
}

func (m *SandboxManager) deleteRunArtifacts(ctx context.Context, reader client.Reader, run *agenticv1alpha1.AgenticRun) []error {
	var errs []error
	for _, step := range sandboxStopSteps {
		configMap := &corev1.ConfigMap{}
		key := types.NamespacedName{Namespace: m.namespace, Name: inputConfigMapName(step, string(run.UID))}
		if err := reader.Get(ctx, key, configMap); err != nil {
			if !apierrors.IsNotFound(err) {
				errs = append(errs, fmt.Errorf("get input ConfigMap %s/%s: %w", key.Namespace, key.Name, err))
			}
		} else if !stopObjectMatchesRun(configMap, string(run.UID), map[types.UID]struct{}{run.UID: {}}, nil) {
			errs = append(errs, fmt.Errorf("input ConfigMap %s/%s is not owned by run UID %s", key.Namespace, key.Name, run.UID))
		} else if err := m.client.Delete(ctx, configMap, stopDeleteOptions(configMap)...); err != nil && !apierrors.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("delete input ConfigMap %s/%s: %w", key.Namespace, key.Name, err))
		}

		if run.Spec.TargetCluster == "" {
			continue
		}
		secret := &corev1.Secret{}
		secretKey := types.NamespacedName{Namespace: m.namespace, Name: sandboxKubeconfigSecretName(string(run.UID), step)}
		if err := reader.Get(ctx, secretKey, secret); err != nil {
			if !apierrors.IsNotFound(err) {
				errs = append(errs, fmt.Errorf("get sandbox kubeconfig Secret %s/%s: %w", secretKey.Namespace, secretKey.Name, err))
			}
		} else if !stopObjectMatchesRun(secret, string(run.UID), map[types.UID]struct{}{run.UID: {}}, nil) {
			errs = append(errs, fmt.Errorf("sandbox kubeconfig Secret %s/%s is not owned by run UID %s", secretKey.Namespace, secretKey.Name, run.UID))
		} else if err := m.client.Delete(ctx, secret, stopDeleteOptions(secret)...); err != nil && !apierrors.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("delete sandbox kubeconfig Secret %s/%s: %w", secretKey.Namespace, secretKey.Name, err))
		}
	}
	return errs
}

func (m *SandboxManager) runArtifactsRemain(ctx context.Context, reader client.Reader, run *agenticv1alpha1.AgenticRun) (bool, []error) {
	var errs []error
	remains := false
	for _, step := range sandboxStopSteps {
		key := types.NamespacedName{Namespace: m.namespace, Name: inputConfigMapName(step, string(run.UID))}
		var configMap corev1.ConfigMap
		if err := reader.Get(ctx, key, &configMap); err != nil {
			if !apierrors.IsNotFound(err) {
				errs = append(errs, fmt.Errorf("confirm input ConfigMap %s/%s: %w", key.Namespace, key.Name, err))
			}
		} else {
			remains = true
		}

		if run.Spec.TargetCluster == "" {
			continue
		}
		secretKey := types.NamespacedName{Namespace: m.namespace, Name: sandboxKubeconfigSecretName(string(run.UID), step)}
		var secret corev1.Secret
		if err := reader.Get(ctx, secretKey, &secret); err != nil {
			if !apierrors.IsNotFound(err) {
				errs = append(errs, fmt.Errorf("confirm sandbox kubeconfig Secret %s/%s: %w", secretKey.Namespace, secretKey.Name, err))
			}
		} else {
			remains = true
		}
	}
	return remains, errs
}
