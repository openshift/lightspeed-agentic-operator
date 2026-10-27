package agenticrun

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var smWarmPoolGVK = schema.GroupVersionKind{
	Group: "extensions.agents.x-k8s.io", Version: "v1beta1", Kind: "SandboxWarmPool",
}

// SuspensionCleanup summarizes the authoritative outcome of one suspension cleanup pass.
type SuspensionCleanup struct {
	Pending    []string
	Failures   []string
	Unverified []string
}

// Confirmed reports whether the observed workload and access cleanup is complete.
func (c SuspensionCleanup) Confirmed() bool {
	return len(c.Pending) == 0 && len(c.Failures) == 0 && len(c.Unverified) == 0
}

func suspensionRunSelector(uid types.UID) client.ListOption {
	if uid == "" {
		return client.HasLabels{LabelRun}
	}
	return client.MatchingLabels{LabelRun: string(uid)}
}

func suspensionUIDMatches(labels map[string]string, uid types.UID) bool {
	value := labels[LabelRun]
	return value != "" && (uid == "" || value == string(uid))
}

func suspensionStepAllowed(step string) bool {
	switch step {
	case "analysis", "execution", "verification", "escalation":
		return true
	default:
		return false
	}
}

type suspensionClaimWork struct {
	object      *unstructured.Unstructured
	uid         types.UID
	runUID      types.UID
	step        string
	blocked     bool
	sandboxes   []*suspensionSandboxWork
	sandboxKeys map[types.NamespacedName]struct{}
}

type suspensionSandboxWork struct {
	object        *unstructured.Unstructured
	runUID        types.UID
	step          string
	claim         *suspensionClaimWork
	blocked       bool
	identitySaved bool
	podSafe       bool
	podCompleted  bool
}

// CleanupSuspension performs one deletion and authoritative-confirmation pass.
// An empty runUID selects all managed hub-local resources; a non-empty UID scopes
// the pass to one AgenticRun. It intentionally does not delete input ConfigMaps,
// Result CRs, shared reader bindings, or reusable SandboxTemplates.
func (m *SandboxManager) CleanupSuspension(ctx context.Context, runUID types.UID) (SuspensionCleanup, error) {
	var result SuspensionCleanup
	var operationErrors []error
	if m.reader == nil {
		err := errors.New("uncached API reader is required for suspension cleanup")
		recordSuspensionUnverified(&result, &operationErrors, "suspension inventory: uncached API reader is not configured", err)
		return result, err
	}

	var pods corev1.PodList
	podListErr := m.reader.List(ctx, &pods, client.InNamespace(m.namespace))
	if podListErr != nil {
		recordSuspensionUnverified(&result, &operationErrors, "Pod inventory", podListErr)
	}

	claims := &unstructured.UnstructuredList{}
	claims.SetGroupVersionKind(smClaimGVK.GroupVersion().WithKind(smClaimGVK.Kind + "List"))
	claimListErr := m.reader.List(ctx, claims, client.InNamespace(m.namespace))

	sandboxes := &unstructured.UnstructuredList{}
	sandboxes.SetGroupVersionKind(smSandboxGVK.GroupVersion().WithKind(smSandboxGVK.Kind + "List"))
	sandboxListErr := m.reader.List(ctx, sandboxes, client.InNamespace(m.namespace))

	warmPools := &unstructured.UnstructuredList{}
	warmPools.SetGroupVersionKind(smWarmPoolGVK.GroupVersion().WithKind(smWarmPoolGVK.Kind + "List"))
	warmPoolListErr := m.reader.List(ctx, warmPools, client.InNamespace(m.namespace), suspensionRunSelector(runUID))

	optionalErrors := []error{claimListErr, sandboxListErr, warmPoolListErr}
	if m.optionalSandboxAPIsAbsent(ctx, optionalErrors, &result, &operationErrors) {
		claimListErr, sandboxListErr, warmPoolListErr = nil, nil, nil
	} else {
		recordSuspensionUnverified(&result, &operationErrors, "SandboxClaim inventory", claimListErr)
		recordSuspensionUnverified(&result, &operationErrors, "Sandbox inventory", sandboxListErr)
		recordSuspensionUnverified(&result, &operationErrors, "SandboxWarmPool inventory", warmPoolListErr)
	}

	var accounts corev1.ServiceAccountList
	accountListErr := m.reader.List(ctx, &accounts, client.InNamespace(m.namespace), suspensionRunSelector(runUID))
	if accountListErr != nil {
		recordSuspensionUnverified(&result, &operationErrors, "ServiceAccount inventory", accountListErr)
	}

	var roles rbacv1.RoleList
	roleListErr := m.reader.List(ctx, &roles, suspensionRunSelector(runUID))
	if roleListErr != nil {
		recordSuspensionUnverified(&result, &operationErrors, "Role inventory", roleListErr)
	}

	var roleBindings rbacv1.RoleBindingList
	roleBindingListErr := m.reader.List(ctx, &roleBindings, suspensionRunSelector(runUID))
	if roleBindingListErr != nil {
		recordSuspensionUnverified(&result, &operationErrors, "RoleBinding inventory", roleBindingListErr)
	}

	var clusterRoles rbacv1.ClusterRoleList
	clusterRoleListErr := m.reader.List(ctx, &clusterRoles, suspensionRunSelector(runUID))
	if clusterRoleListErr != nil {
		recordSuspensionUnverified(&result, &operationErrors, "ClusterRole inventory", clusterRoleListErr)
	}

	var clusterRoleBindings rbacv1.ClusterRoleBindingList
	clusterRoleBindingListErr := m.reader.List(ctx, &clusterRoleBindings, suspensionRunSelector(runUID))
	if clusterRoleBindingListErr != nil {
		recordSuspensionUnverified(&result, &operationErrors, "ClusterRoleBinding inventory", clusterRoleBindingListErr)
	}

	var secrets corev1.SecretList
	secretListErr := m.reader.List(ctx, &secrets, client.InNamespace(m.namespace), suspensionRunSelector(runUID))
	if secretListErr != nil {
		recordSuspensionUnverified(&result, &operationErrors, "credential Secret inventory", secretListErr)
	}

	claimWorkByUID := make(map[types.UID]*suspensionClaimWork)
	claimObjectsByUID := make(map[types.UID]*unstructured.Unstructured, len(claims.Items))
	claimWork := make([]*suspensionClaimWork, 0, len(claims.Items))
	if claimListErr == nil {
		for i := range claims.Items {
			claim := &claims.Items[i]
			claim.SetGroupVersionKind(smClaimGVK)
			uid := claim.GetUID()
			if uid != "" {
				claimObjectsByUID[uid] = claim
			}
			labels := claim.GetLabels()
			if !suspensionUIDMatches(labels, runUID) {
				continue
			}
			step := labels[LabelStep]
			if !suspensionStepAllowed(step) {
				recordSuspensionUnverified(&result, &operationErrors,
					"managed SandboxClaim has no valid step label: "+suspensionObjectDescription(claim),
					fmt.Errorf("invalid step %q", step))
				continue
			}
			if uid == "" {
				recordSuspensionUnverified(&result, &operationErrors,
					"managed SandboxClaim has no UID: "+suspensionObjectDescription(claim),
					errors.New("refuse to remove claim without UID"))
				continue
			}
			work := &suspensionClaimWork{
				object:      claim,
				uid:         uid,
				runUID:      types.UID(labels[LabelRun]),
				step:        step,
				sandboxKeys: make(map[types.NamespacedName]struct{}),
			}
			claimWorkByUID[uid] = work
			claimWork = append(claimWork, work)
		}
	}

	sandboxByName := make(map[string]*unstructured.Unstructured, len(sandboxes.Items))
	if sandboxListErr == nil {
		for i := range sandboxes.Items {
			sandbox := &sandboxes.Items[i]
			sandbox.SetGroupVersionKind(smSandboxGVK)
			sandboxByName[sandbox.GetName()] = sandbox
		}
	}

	sandboxWorkByKey := make(map[types.NamespacedName]*suspensionSandboxWork)
	if sandboxListErr == nil {
		for i := range sandboxes.Items {
			sandbox := &sandboxes.Items[i]
			labels := sandbox.GetLabels()
			runLabel := labels[LabelRun]
			var ownerClaim *suspensionClaimWork
			for _, ref := range sandbox.GetOwnerReferences() {
				if ref.Controller == nil || !*ref.Controller || ref.Kind != smClaimGVK.Kind {
					continue
				}
				gv, err := schema.ParseGroupVersion(ref.APIVersion)
				if err != nil || gv.Group != smClaimGVK.Group {
					continue
				}
				candidate := claimWorkByUID[ref.UID]
				if candidate != nil && ref.Name == candidate.object.GetName() {
					ownerClaim = candidate
					break
				}
			}

			var identityUID types.UID
			var step string
			if ownerClaim != nil {
				identityUID, step = ownerClaim.runUID, ownerClaim.step
				if runLabel != "" && runLabel != string(identityUID) {
					ownerClaim.blocked = true
					recordSuspensionUnverified(&result, &operationErrors,
						"Sandbox run label conflicts with its validated claim owner: "+suspensionObjectDescription(sandbox),
						fmt.Errorf("Sandbox belongs to run UID %q, claim belongs to %q", runLabel, identityUID))
					continue
				}
				if labels[LabelStep] != "" && labels[LabelStep] != step {
					ownerClaim.blocked = true
					recordSuspensionUnverified(&result, &operationErrors,
						"Sandbox step label conflicts with its validated claim owner: "+suspensionObjectDescription(sandbox),
						fmt.Errorf("Sandbox step %q conflicts with claim step %q", labels[LabelStep], step))
					continue
				}
			} else {
				if !suspensionUIDMatches(labels, runUID) {
					continue
				}
				step = labels[LabelStep]
				if !suspensionStepAllowed(step) {
					recordSuspensionUnverified(&result, &operationErrors,
						"managed Sandbox has no valid step label: "+suspensionObjectDescription(sandbox),
						fmt.Errorf("invalid step %q", step))
					continue
				}
				identityUID = types.UID(runLabel)
				claimRef, hasClaimOwner := suspensionControllerOwnerReference(sandbox, smClaimGVK)
				if hasClaimOwner {
					if claimListErr != nil {
						recordSuspensionUnverified(&result, &operationErrors,
							"cannot validate SandboxClaim creation source for "+suspensionObjectDescription(sandbox),
							claimListErr)
						continue
					}
					if claimObject := claimObjectsByUID[claimRef.UID]; claimObject != nil {
						recordSuspensionUnverified(&result, &operationErrors,
							"managed Sandbox is still controlled by a claim without matching run/step identity: "+suspensionObjectDescription(sandbox),
							fmt.Errorf("claim UID %s is not a validated run-owned SandboxClaim", claimRef.UID))
						continue
					}
				} else if suspensionHasAnyControllerOwner(sandbox) || suspensionHasOwnerKind(sandbox, smClaimGVK.Kind) {
					recordSuspensionUnverified(&result, &operationErrors,
						"managed Sandbox has an unvalidated creation owner: "+suspensionObjectDescription(sandbox),
						errors.New("Sandbox controller owner is not a validated SandboxClaim"))
					continue
				}
			}

			key := types.NamespacedName{Name: sandbox.GetName(), Namespace: sandbox.GetNamespace()}
			work := sandboxWorkByKey[key]
			if work == nil {
				work = &suspensionSandboxWork{object: sandbox, runUID: identityUID, step: step, claim: ownerClaim}
				sandboxWorkByKey[key] = work
			} else if work.runUID != identityUID || work.step != step {
				work.blocked = true
				if work.claim != nil {
					work.claim.blocked = true
				}
				recordSuspensionUnverified(&result, &operationErrors,
					"Sandbox has conflicting managed identities: "+suspensionObjectDescription(sandbox),
					errors.New("conflicting run or step identity"))
				continue
			}
			if ownerClaim != nil {
				work.claim = ownerClaim
				if _, exists := ownerClaim.sandboxKeys[key]; !exists {
					ownerClaim.sandboxKeys[key] = struct{}{}
					ownerClaim.sandboxes = append(ownerClaim.sandboxes, work)
				}
			}
		}
	}

	if podListErr == nil && sandboxListErr == nil {
		for i := range pods.Items {
			pod := &pods.Items[i]
			labels := pod.Labels
			if !suspensionUIDMatches(labels, runUID) || !suspensionStepAllowed(labels[LabelStep]) {
				continue
			}
			sandboxRef, hasSandboxOwner := suspensionControllerOwnerReference(pod, smSandboxGVK)
			if !hasSandboxOwner {
				if suspensionHasControllerOwnerKind(pod, smSandboxGVK.Kind) {
					recordSuspensionUnverified(&result, &operationErrors,
						"managed Pod has an invalid Sandbox owner reference: "+suspensionObjectDescription(pod),
						errors.New("Sandbox owner reference API group or UID is invalid"))
				}
				continue
			}
			sandbox := sandboxByName[sandboxRef.Name]
			if sandbox == nil {
				continue
			}
			key := types.NamespacedName{Name: sandbox.GetName(), Namespace: sandbox.GetNamespace()}
			work := sandboxWorkByKey[key]
			if sandbox.GetUID() != sandboxRef.UID ||
				!suspensionHasControllerOwner(pod, smSandboxGVK, sandbox.GetName(), sandbox.GetUID()) {
				if work != nil {
					work.blocked = true
					if work.claim != nil {
						work.claim.blocked = true
					}
				}
				recordSuspensionUnverified(&result, &operationErrors,
					"managed Pod does not validate its Sandbox owner: "+suspensionObjectDescription(pod),
					fmt.Errorf("Sandbox owner UID changed or does not match Pod owner UID %s", sandboxRef.UID))
				continue
			}
			sandboxLabels := sandbox.GetLabels()
			if (sandboxLabels[LabelRun] != "" && sandboxLabels[LabelRun] != labels[LabelRun]) ||
				(sandboxLabels[LabelStep] != "" && sandboxLabels[LabelStep] != labels[LabelStep]) {
				if work != nil {
					work.blocked = true
					if work.claim != nil {
						work.claim.blocked = true
					}
				}
				recordSuspensionUnverified(&result, &operationErrors,
					"managed Pod identity conflicts with its Sandbox: "+suspensionObjectDescription(pod),
					errors.New("run UID or step labels conflict"))
				continue
			}

			claimRef, hasClaimOwner := suspensionControllerOwnerReference(sandbox, smClaimGVK)
			var ownerClaim *suspensionClaimWork
			if hasClaimOwner {
				if claimListErr != nil {
					if work != nil {
						work.blocked = true
						if work.claim != nil {
							work.claim.blocked = true
						}
					}
					recordSuspensionUnverified(&result, &operationErrors,
						"cannot validate SandboxClaim creation source for "+suspensionObjectDescription(sandbox),
						claimListErr)
					continue
				}
				if claimObject := claimObjectsByUID[claimRef.UID]; claimObject != nil {
					ownerClaim = claimWorkByUID[claimRef.UID]
					if ownerClaim == nil || ownerClaim.runUID != types.UID(labels[LabelRun]) ||
						ownerClaim.step != labels[LabelStep] || ownerClaim.object.GetName() != claimRef.Name {
						if work != nil {
							work.blocked = true
							if work.claim != nil {
								work.claim.blocked = true
							}
						}
						recordSuspensionUnverified(&result, &operationErrors,
							"managed Pod's Sandbox has an unvalidated claim owner: "+suspensionObjectDescription(sandbox),
							fmt.Errorf("claim owner UID %s has no matching run/step identity", claimRef.UID))
						continue
					}
				}
			} else if suspensionHasAnyControllerOwner(sandbox) || suspensionHasOwnerKind(sandbox, smClaimGVK.Kind) {
				if work != nil {
					work.blocked = true
					if work.claim != nil {
						work.claim.blocked = true
					}
				}
				recordSuspensionUnverified(&result, &operationErrors,
					"managed Sandbox has an unvalidated creation owner: "+suspensionObjectDescription(sandbox),
					errors.New("Sandbox controller owner is not a validated SandboxClaim"))
				continue
			}

			if work == nil {
				work = &suspensionSandboxWork{
					object: sandbox,
					runUID: types.UID(labels[LabelRun]),
					step:   labels[LabelStep],
					claim:  ownerClaim,
				}
				sandboxWorkByKey[key] = work
			} else if work.runUID != types.UID(labels[LabelRun]) || work.step != labels[LabelStep] {
				work.blocked = true
				if work.claim != nil {
					work.claim.blocked = true
				}
				recordSuspensionUnverified(&result, &operationErrors,
					"managed Pod identity conflicts with existing Sandbox identity: "+suspensionObjectDescription(pod),
					errors.New("run UID or step labels conflict"))
				continue
			}
			if ownerClaim != nil {
				if work.claim != nil && work.claim.uid != ownerClaim.uid {
					work.blocked = true
					work.claim.blocked = true
					ownerClaim.blocked = true
					recordSuspensionUnverified(&result, &operationErrors,
						"Sandbox has conflicting claim owners: "+suspensionObjectDescription(sandbox),
						errors.New("claim UID ownership conflict"))
					continue
				}
				work.claim = ownerClaim
				if _, exists := ownerClaim.sandboxKeys[key]; !exists {
					ownerClaim.sandboxKeys[key] = struct{}{}
					ownerClaim.sandboxes = append(ownerClaim.sandboxes, work)
				}
			}
		}
	}

	if claimListErr == nil {
		for _, claim := range claimWork {
			if sandboxListErr != nil {
				claim.blocked = true
				recordSuspensionUnverified(&result, &operationErrors,
					"cannot validate Sandbox descendants for "+suspensionObjectDescription(claim.object),
					fmt.Errorf("Sandbox inventory unavailable: %w", sandboxListErr))
				continue
			}
			sandboxName, found, err := unstructured.NestedString(claim.object.Object, "status", "sandbox", "name")
			if err != nil {
				claim.blocked = true
				recordSuspensionUnverified(&result, &operationErrors,
					"cannot read Sandbox reference from "+suspensionObjectDescription(claim.object), err)
				continue
			}
			if !found || sandboxName == "" {
				continue
			}
			statusSandbox := sandboxByName[sandboxName]
			if statusSandbox == nil {
				claim.blocked = true
				recordSuspensionUnverified(&result, &operationErrors,
					"SandboxClaim references a missing Sandbox: "+suspensionObjectDescription(claim.object),
					fmt.Errorf("referenced Sandbox %s/%s is absent from authoritative inventory", m.namespace, sandboxName))
				continue
			}
			if !suspensionHasControllerOwner(statusSandbox, smClaimGVK, claim.object.GetName(), claim.uid) {
				claim.blocked = true
				recordSuspensionUnverified(&result, &operationErrors,
					"SandboxClaim references a Sandbox with an unvalidated owner: "+suspensionObjectDescription(claim.object),
					fmt.Errorf("Sandbox %s/%s is not controlled by claim UID %s", statusSandbox.GetNamespace(), statusSandbox.GetName(), claim.uid))
				continue
			}
			key := types.NamespacedName{Name: statusSandbox.GetName(), Namespace: statusSandbox.GetNamespace()}
			if _, exists := claim.sandboxKeys[key]; !exists {
				claim.blocked = true
				recordSuspensionUnverified(&result, &operationErrors,
					"SandboxClaim's status Sandbox was not safely inventoried: "+suspensionObjectDescription(claim.object),
					errors.New("validated status Sandbox is not a managed cleanup candidate"))
			}
		}
	}

	handledPods := make(map[types.NamespacedName]struct{})
	if sandboxListErr == nil {
		sandboxWorkKeys := make([]types.NamespacedName, 0, len(sandboxWorkByKey))
		for key := range sandboxWorkByKey {
			sandboxWorkKeys = append(sandboxWorkKeys, key)
		}
		sort.Slice(sandboxWorkKeys, func(i, j int) bool {
			if sandboxWorkKeys[i].Namespace != sandboxWorkKeys[j].Namespace {
				return sandboxWorkKeys[i].Namespace < sandboxWorkKeys[j].Namespace
			}
			return sandboxWorkKeys[i].Name < sandboxWorkKeys[j].Name
		})
		for _, key := range sandboxWorkKeys {
			sandbox := sandboxWorkByKey[key]
			if sandbox.blocked {
				if sandbox.claim != nil {
					sandbox.claim.blocked = true
				}
				continue
			}
			if err := m.patchRunIdentityLabels(ctx, sandbox.object, sandbox.object.GetUID(), sandbox.runUID, sandbox.step); err != nil {
				sandbox.blocked = true
				if sandbox.claim != nil {
					sandbox.claim.blocked = true
				}
				recordSuspensionUnverified(&result, &operationErrors,
					"cannot persist validated Sandbox identity: "+suspensionObjectDescription(sandbox.object), err)
				continue
			}
			sandbox.identitySaved = true

			podName := sandbox.object.GetAnnotations()[sandboxPodNameAnnotation]
			if podName == "" {
				podName = sandbox.object.GetName()
			}
			podKey := types.NamespacedName{Name: podName, Namespace: m.namespace}
			handledPods[podKey] = struct{}{}
			sandbox.podSafe = m.deleteSuspensionPod(ctx, podName, sandbox.runUID, sandbox.step, sandbox, true, &sandbox.podCompleted, &result, &operationErrors)
			if !sandbox.podSafe {
				sandbox.blocked = true
				if sandbox.claim != nil {
					sandbox.claim.blocked = true
				}
				continue
			}
			if sandbox.podCompleted {
				if !m.deleteSuspensionObjectWithOptions(ctx, sandbox.object, &result, &operationErrors,
					client.PropagationPolicy(metav1.DeletePropagationOrphan)) {
					sandbox.blocked = true
					if sandbox.claim != nil {
						sandbox.claim.blocked = true
					}
				}
			} else {
				m.deleteSuspensionObject(ctx, sandbox.object, &result, &operationErrors)
			}
		}
	}

	if warmPoolListErr == nil {
		for i := range warmPools.Items {
			pool := &warmPools.Items[i]
			pool.SetGroupVersionKind(smWarmPoolGVK)
			labels := pool.GetLabels()
			if !suspensionUIDMatches(labels, runUID) {
				continue
			}
			step := labels[LabelStep]
			if !suspensionStepAllowed(step) {
				recordSuspensionUnverified(&result, &operationErrors,
					"managed SandboxWarmPool has no valid step label: "+suspensionObjectDescription(pool),
					fmt.Errorf("invalid step %q", step))
				continue
			}
			m.deleteSuspensionObject(ctx, pool, &result, &operationErrors)
		}
	}

	if podListErr == nil {
		for i := range pods.Items {
			pod := &pods.Items[i]
			labels := pod.Labels
			if !suspensionUIDMatches(labels, runUID) {
				continue
			}
			step := labels[LabelStep]
			if !suspensionStepAllowed(step) {
				recordSuspensionUnverified(&result, &operationErrors,
					"managed Pod has no valid step label: "+suspensionObjectDescription(pod),
					fmt.Errorf("invalid step %q", step))
				continue
			}
			key := client.ObjectKeyFromObject(pod)
			if _, handled := handledPods[key]; handled {
				continue
			}
			handledPods[key] = struct{}{}
			m.deleteSuspensionPod(ctx, pod.Name, types.UID(labels[LabelRun]), step, nil, false, nil, &result, &operationErrors)
		}
	}

	if accountListErr == nil {
		for i := range accounts.Items {
			account := &accounts.Items[i]
			_, managed, valid := suspensionServiceAccountStep(account.Labels, runUID)
			if !managed {
				continue
			}
			if !valid {
				recordSuspensionUnverified(&result, &operationErrors,
					"managed ServiceAccount has invalid component identity: "+suspensionObjectDescription(account),
					errors.New("ServiceAccount component does not identify an allowed step"))
				continue
			}
			m.deleteSuspensionObject(ctx, account, &result, &operationErrors)
		}
	}

	if roleListErr == nil {
		for i := range roles.Items {
			role := &roles.Items[i]
			_, managed, valid := suspensionRBACStep(role.Labels, role.Name, runUID)
			if !managed {
				continue
			}
			if !valid {
				recordSuspensionUnverified(&result, &operationErrors,
					"managed Role has invalid component identity: "+suspensionObjectDescription(role),
					errors.New("Role component does not identify an allowed step"))
				continue
			}
			if role.Labels[LabelComponent] == "result-rbac" && role.Namespace != m.namespace {
				recordSuspensionUnverified(&result, &operationErrors,
					"result Role is outside the operator namespace: "+suspensionObjectDescription(role),
					errors.New("refuse to delete result RBAC outside the operator namespace"))
				continue
			}
			m.deleteSuspensionObject(ctx, role, &result, &operationErrors)
		}
	}

	if roleBindingListErr == nil {
		for i := range roleBindings.Items {
			binding := &roleBindings.Items[i]
			_, managed, valid := suspensionRBACStep(binding.Labels, binding.Name, runUID)
			if !managed {
				continue
			}
			if !valid {
				recordSuspensionUnverified(&result, &operationErrors,
					"managed RoleBinding has invalid component identity: "+suspensionObjectDescription(binding),
					errors.New("RoleBinding component does not identify an allowed step"))
				continue
			}
			if binding.Labels[LabelComponent] == "result-rbac" && binding.Namespace != m.namespace {
				recordSuspensionUnverified(&result, &operationErrors,
					"result RoleBinding is outside the operator namespace: "+suspensionObjectDescription(binding),
					errors.New("refuse to delete result RBAC outside the operator namespace"))
				continue
			}
			m.deleteSuspensionObject(ctx, binding, &result, &operationErrors)
		}
	}

	if clusterRoleListErr == nil {
		for i := range clusterRoles.Items {
			role := &clusterRoles.Items[i]
			component := role.Labels[LabelComponent]
			if suspensionUIDMatches(role.Labels, runUID) && (component == "result-rbac" || component == "reader-rbac") {
				recordSuspensionUnverified(&result, &operationErrors,
					"namespace-scoped RBAC component found on ClusterRole: "+suspensionObjectDescription(role),
					fmt.Errorf("unexpected ClusterRole component %q", component))
				continue
			}
			_, managed, valid := suspensionRBACStep(role.Labels, role.Name, runUID)
			if !managed {
				continue
			}
			if !valid {
				recordSuspensionUnverified(&result, &operationErrors,
					"managed ClusterRole has invalid component identity: "+suspensionObjectDescription(role),
					errors.New("ClusterRole component does not identify an allowed step"))
				continue
			}
			m.deleteSuspensionObject(ctx, role, &result, &operationErrors)
		}
	}

	if clusterRoleBindingListErr == nil {
		for i := range clusterRoleBindings.Items {
			binding := &clusterRoleBindings.Items[i]
			if !suspensionUIDMatches(binding.Labels, runUID) {
				continue
			}
			component := binding.Labels[LabelComponent]
			if component == "reader-rbac" {
				if !suspensionStepAllowed(binding.Labels[LabelStep]) {
					recordSuspensionUnverified(&result, &operationErrors,
						"temporary reader ClusterRoleBinding has invalid step identity: "+suspensionObjectDescription(binding),
						fmt.Errorf("invalid step %q", binding.Labels[LabelStep]))
					continue
				}
			} else if component == "execution-rbac" {
				if binding.Labels[LabelStep] != "" && binding.Labels[LabelStep] != "execution" {
					recordSuspensionUnverified(&result, &operationErrors,
						"execution ClusterRoleBinding has conflicting step identity: "+suspensionObjectDescription(binding),
						fmt.Errorf("invalid step %q", binding.Labels[LabelStep]))
					continue
				}
			} else if component == "result-rbac" {
				recordSuspensionUnverified(&result, &operationErrors,
					"namespace-scoped result RBAC component found on ClusterRoleBinding: "+suspensionObjectDescription(binding),
					errors.New("refuse to delete result RBAC from cluster scope"))
				continue
			} else {
				continue
			}
			m.deleteSuspensionObject(ctx, binding, &result, &operationErrors)
		}
	}

	if secretListErr == nil {
		for i := range secrets.Items {
			secret := &secrets.Items[i]
			if !suspensionUIDMatches(secret.Labels, runUID) || secret.Labels[LabelComponent] != "sandbox-kubeconfig" {
				continue
			}
			step := suspensionCredentialStep(secret.Name, secret.Labels[LabelRun])
			if !suspensionStepAllowed(step) || (secret.Labels[LabelStep] != "" && secret.Labels[LabelStep] != step) {
				recordSuspensionUnverified(&result, &operationErrors,
					"sandbox credential Secret has invalid step identity: "+suspensionObjectDescription(secret),
					fmt.Errorf("invalid step identity %q", secret.Labels[LabelStep]))
				continue
			}
			m.deleteSuspensionObject(ctx, secret, &result, &operationErrors)
		}
	}

	for _, claim := range claimWork {
		if claim.blocked {
			continue
		}
		for _, sandbox := range claim.sandboxes {
			if sandbox.blocked || !sandbox.identitySaved || !sandbox.podSafe {
				claim.blocked = true
				break
			}
		}
		if !claim.blocked {
			m.deleteSuspensionObject(ctx, claim.object, &result, &operationErrors)
		}
	}

	m.confirmSuspensionInventory(ctx, runUID, &result, &operationErrors)
	return result, errors.Join(operationErrors...)
}

func (m *SandboxManager) optionalSandboxAPIsAbsent(ctx context.Context, errs []error, result *SuspensionCleanup, operationErrors *[]error) bool {
	if m.config == nil || !m.config.ForceBareMode || len(errs) != 3 {
		return false
	}
	if cfg := m.config.Get(); cfg != nil && cfg.Sandbox.Mode != "bare-pod" {
		return false
	}
	for _, err := range errs {
		if !suspensionOptionalListAbsence(err) {
			return false
		}
	}

	crdGVK := schema.GroupVersionKind{
		Group: "apiextensions.k8s.io", Version: "v1", Kind: "CustomResourceDefinition",
	}
	allAbsent := true
	for _, name := range []string{
		"sandboxclaims.extensions.agents.x-k8s.io",
		"sandboxes.agents.x-k8s.io",
		"sandboxwarmpools.extensions.agents.x-k8s.io",
	} {
		crd := &unstructured.Unstructured{}
		crd.SetGroupVersionKind(crdGVK)
		err := m.reader.Get(ctx, client.ObjectKey{Name: name}, crd)
		if apierrors.IsNotFound(err) {
			continue
		}
		allAbsent = false
		if err != nil {
			recordSuspensionUnverified(result, operationErrors, "confirm optional CRD absence "+name, err)
			continue
		}
		recordSuspensionUnverified(result, operationErrors,
			"optional CRD is present despite an unavailable inventory mapping: "+name,
			fmt.Errorf("CRD %s is installed", name))
	}
	return allAbsent
}

func suspensionOptionalListAbsence(err error) bool {
	if err == nil || apierrors.IsForbidden(err) || apierrors.IsTimeout(err) ||
		apierrors.IsServerTimeout(err) || errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if multi, ok := err.(interface{ Unwrap() []error }); ok && len(multi.Unwrap()) > 1 {
		return false
	}
	return apierrors.IsNotFound(err) || apimeta.IsNoMatchError(err)
}

func suspensionControllerOwnerReference(child metav1.Object, parentGVK schema.GroupVersionKind) (metav1.OwnerReference, bool) {
	for _, ref := range child.GetOwnerReferences() {
		if ref.Controller == nil || !*ref.Controller || ref.Kind != parentGVK.Kind || ref.Name == "" || ref.UID == "" {
			continue
		}
		gv, err := schema.ParseGroupVersion(ref.APIVersion)
		if err == nil && gv.Group == parentGVK.Group {
			return ref, true
		}
	}
	return metav1.OwnerReference{}, false
}

func suspensionHasControllerOwnerKind(child metav1.Object, kind string) bool {
	for _, ref := range child.GetOwnerReferences() {
		if ref.Controller != nil && *ref.Controller && ref.Kind == kind {
			return true
		}
	}
	return false
}
func suspensionHasOwnerKind(child metav1.Object, kind string) bool {
	for _, ref := range child.GetOwnerReferences() {
		if ref.Kind == kind {
			return true
		}
	}
	return false
}

func suspensionHasAnyControllerOwner(child metav1.Object) bool {
	for _, ref := range child.GetOwnerReferences() {
		if ref.Controller != nil && *ref.Controller {
			return true
		}
	}
	return false
}

func suspensionHasControllerOwner(child metav1.Object, parentGVK schema.GroupVersionKind, parentName string, parentUID types.UID) bool {
	ref, found := suspensionControllerOwnerReference(child, parentGVK)
	return found && ref.Name == parentName && ref.UID == parentUID
}

func (m *SandboxManager) deleteSuspensionPod(
	ctx context.Context,
	podName string,
	runUID types.UID,
	step string,
	parentSandbox *suspensionSandboxWork,
	persistIdentity bool,
	podCompleted *bool,
	result *SuspensionCleanup,
	operationErrors *[]error,
) bool {
	pod := &corev1.Pod{}
	key := types.NamespacedName{Name: podName, Namespace: m.namespace}
	if err := m.reader.Get(ctx, key, pod); err != nil {
		if apierrors.IsNotFound(err) {
			return true
		}
		recordSuspensionUnverified(result, operationErrors,
			fmt.Sprintf("cannot confirm backing Pod %s/%s", key.Namespace, key.Name), err)
		return false
	}
	if pod.UID == "" {
		recordSuspensionUnverified(result, operationErrors,
			"managed Pod has no UID: "+suspensionObjectDescription(pod), errors.New("refuse to remove Pod without UID"))
		return false
	}

	parentSafe := true
	if parentSandbox != nil && !suspensionHasControllerOwner(pod, smSandboxGVK, parentSandbox.object.GetName(), parentSandbox.object.GetUID()) {
		parentSafe = false
		ownerErr := fmt.Errorf("Pod %s/%s is not controlled by Sandbox UID %s", pod.Namespace, pod.Name, parentSandbox.object.GetUID())
		recordSuspensionUnverified(result, operationErrors,
			"backing Pod owner does not validate: "+suspensionObjectDescription(pod), ownerErr)
		if !suspensionPodLabelsMatch(pod, runUID, step) {
			return false
		}
	}

	if persistIdentity {
		if err := m.patchRunIdentityLabels(ctx, pod, pod.UID, runUID, step); err != nil {
			recordSuspensionUnverified(result, operationErrors,
				"cannot persist validated Pod identity: "+suspensionObjectDescription(pod), err)
			return false
		}
	} else if !suspensionPodLabelsMatch(pod, runUID, step) {
		recordSuspensionUnverified(result, operationErrors,
			"Pod identity changed before suspension deletion: "+suspensionObjectDescription(pod),
			fmt.Errorf("Pod labels do not identify run UID %s and step %s", runUID, step))
		return false
	}

	if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
		if podCompleted != nil {
			*podCompleted = true
		}
		if !parentSafe && suspensionPodLabelsMatch(pod, runUID, step) {
			return true
		}
		return parentSafe
	}

	expectedUID := pod.UID
	if err := m.deleteWithUIDPrecondition(ctx, pod, client.GracePeriodSeconds(0)); err != nil {
		recordSuspensionFailure(result, operationErrors,
			"delete active suspension Pod "+suspensionObjectDescription(pod), err)
		return false
	}
	gone, err := m.confirmObjectGone(ctx, pod)
	if err != nil {
		recordSuspensionUnverified(result, operationErrors,
			"cannot confirm suspension Pod removal "+suspensionObjectDescription(pod), err)
		return false
	}
	if !gone {
		if pod.UID != expectedUID {
			recordSuspensionPending(result,
				fmt.Sprintf("replacement Pod %s/%s UID %s requires independent validation", pod.Namespace, pod.Name, pod.UID))
		} else {
			recordSuspensionPending(result, "active Pod remains after deletion request: "+suspensionObjectDescription(pod))
		}
		return false
	}
	return parentSafe
}

func (m *SandboxManager) deleteSuspensionObject(ctx context.Context, object client.Object, result *SuspensionCleanup, operationErrors *[]error) bool {
	return m.deleteSuspensionObjectWithOptions(ctx, object, result, operationErrors)
}

func (m *SandboxManager) deleteSuspensionObjectWithOptions(ctx context.Context, object client.Object, result *SuspensionCleanup, operationErrors *[]error, extra ...client.DeleteOption) bool {
	if err := m.deleteWithUIDPrecondition(ctx, object, extra...); err != nil {
		recordSuspensionFailure(result, operationErrors, "delete suspension resource "+suspensionObjectDescription(object), err)
		return false
	}
	return true
}

func (m *SandboxManager) deleteWithUIDPrecondition(ctx context.Context, object client.Object, extra ...client.DeleteOption) error {
	uid := object.GetUID()
	if uid == "" {
		return fmt.Errorf("refuse suspension delete without UID: %T %s/%s", object, object.GetNamespace(), object.GetName())
	}
	options := append(extra, client.Preconditions{UID: &uid})
	if err := m.client.Delete(ctx, object, options...); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete %T %s/%s UID %s: %w", object, object.GetNamespace(), object.GetName(), uid, err)
	}
	return nil
}

func (m *SandboxManager) confirmObjectGone(ctx context.Context, object client.Object) (bool, error) {
	err := m.reader.Get(ctx, client.ObjectKeyFromObject(object), object)
	if apierrors.IsNotFound(err) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return false, nil
}

func (m *SandboxManager) patchRunIdentityLabels(ctx context.Context, object client.Object, expectedUID, runUID types.UID, step string) error {
	if expectedUID == "" || runUID == "" || !suspensionStepAllowed(step) {
		return errors.New("invalid suspension identity")
	}
	if m.reader == nil {
		return errors.New("uncached API reader is required to persist suspension identity")
	}
	if err := m.reader.Get(ctx, client.ObjectKeyFromObject(object), object); err != nil {
		return err
	}
	if object.GetUID() != expectedUID {
		return fmt.Errorf("object UID changed before suspension identity patch: expected %s, got %s", expectedUID, object.GetUID())
	}
	currentLabels := object.GetLabels()
	if value := currentLabels[LabelRun]; value != "" && value != string(runUID) {
		return fmt.Errorf("object belongs to another run UID %s", value)
	}
	if value := currentLabels[LabelStep]; value != "" && value != step {
		return fmt.Errorf("object belongs to another step %s", value)
	}
	if currentLabels[LabelRun] == string(runUID) && currentLabels[LabelStep] == step {
		return nil
	}
	if object.GetResourceVersion() == "" {
		return errors.New("object has no resourceVersion for suspension identity patch")
	}
	body := struct {
		Metadata struct {
			UID             types.UID         `json:"uid"`
			ResourceVersion string            `json:"resourceVersion"`
			Labels          map[string]string `json:"labels"`
		} `json:"metadata"`
	}{}
	body.Metadata.UID = expectedUID
	body.Metadata.ResourceVersion = object.GetResourceVersion()
	body.Metadata.Labels = map[string]string{LabelRun: string(runUID), LabelStep: step}
	patch, err := json.Marshal(body)
	if err != nil {
		return err
	}
	if err := m.client.Patch(ctx, object, client.RawPatch(types.MergePatchType, patch)); err != nil {
		return err
	}
	if err := m.reader.Get(ctx, client.ObjectKeyFromObject(object), object); err != nil {
		return fmt.Errorf("confirm persisted suspension identity: %w", err)
	}
	if object.GetUID() != expectedUID || object.GetLabels()[LabelRun] != string(runUID) || object.GetLabels()[LabelStep] != step {
		return errors.New("validated suspension identity was not persisted on the expected object")
	}
	return nil
}

func suspensionPodLabelsMatch(pod *corev1.Pod, runUID types.UID, step string) bool {
	return pod.Labels[LabelRun] == string(runUID) && pod.Labels[LabelStep] == step
}

func suspensionServiceAccountStep(labels map[string]string, uid types.UID) (string, bool, bool) {
	if !suspensionUIDMatches(labels, uid) {
		return "", false, false
	}
	component := labels[LabelComponent]
	if len(component) <= len("-sa") || component[len(component)-len("-sa"):] != "-sa" {
		return "", false, false
	}
	step := component[:len(component)-len("-sa")]
	if !suspensionStepAllowed(step) || (labels[LabelStep] != "" && labels[LabelStep] != step) {
		return step, true, false
	}
	return step, true, true
}

func suspensionRBACStep(labels map[string]string, name string, uid types.UID) (string, bool, bool) {
	if !suspensionUIDMatches(labels, uid) {
		return "", false, false
	}
	switch labels[LabelComponent] {
	case "execution-rbac":
		if labels[LabelStep] != "" && labels[LabelStep] != "execution" {
			return "execution", true, false
		}
		return "execution", true, true
	case "result-rbac":
		for _, step := range []string{"analysis", "execution", "verification", "escalation"} {
			if name == resultRoleName(labels[LabelRun], step) {
				if labels[LabelStep] != "" && labels[LabelStep] != step {
					return step, true, false
				}
				return step, true, true
			}
		}
		return "", true, false
	default:
		return "", false, false
	}
}

func suspensionCredentialStep(name, runUID string) string {
	for _, step := range []string{"analysis", "execution", "verification", "escalation"} {
		if name == sandboxKubeconfigSecretName(runUID, step) {
			return step
		}
	}
	return ""
}

func (m *SandboxManager) confirmSuspensionInventory(ctx context.Context, runUID types.UID, result *SuspensionCleanup, operationErrors *[]error) {
	var pods corev1.PodList
	if err := m.reader.List(ctx, &pods, client.InNamespace(m.namespace), suspensionRunSelector(runUID)); err != nil {
		recordSuspensionUnverified(result, operationErrors, "Pod confirmation inventory", err)
	} else {
		for i := range pods.Items {
			pod := &pods.Items[i]
			if !suspensionUIDMatches(pod.Labels, runUID) {
				continue
			}
			if !suspensionStepAllowed(pod.Labels[LabelStep]) {
				recordSuspensionUnverified(result, operationErrors,
					"retained managed Pod has no valid step label: "+suspensionObjectDescription(pod),
					fmt.Errorf("invalid step %q", pod.Labels[LabelStep]))
				continue
			}
			if pod.Status.Phase != corev1.PodSucceeded && pod.Status.Phase != corev1.PodFailed {
				recordSuspensionPending(result, "active Pod remains in authoritative inventory: "+suspensionObjectDescription(pod))
			}
		}
	}

	claims := &unstructured.UnstructuredList{}
	claims.SetGroupVersionKind(smClaimGVK.GroupVersion().WithKind(smClaimGVK.Kind + "List"))
	claimListErr := m.reader.List(ctx, claims, client.InNamespace(m.namespace), suspensionRunSelector(runUID))
	sandboxes := &unstructured.UnstructuredList{}
	sandboxes.SetGroupVersionKind(smSandboxGVK.GroupVersion().WithKind(smSandboxGVK.Kind + "List"))
	sandboxListErr := m.reader.List(ctx, sandboxes, client.InNamespace(m.namespace), suspensionRunSelector(runUID))
	warmPools := &unstructured.UnstructuredList{}
	warmPools.SetGroupVersionKind(smWarmPoolGVK.GroupVersion().WithKind(smWarmPoolGVK.Kind + "List"))
	warmPoolListErr := m.reader.List(ctx, warmPools, client.InNamespace(m.namespace), suspensionRunSelector(runUID))
	optionalErrors := []error{claimListErr, sandboxListErr, warmPoolListErr}
	if m.optionalSandboxAPIsAbsent(ctx, optionalErrors, result, operationErrors) {
		claimListErr, sandboxListErr, warmPoolListErr = nil, nil, nil
	} else {
		recordSuspensionUnverified(result, operationErrors, "SandboxClaim confirmation inventory", claimListErr)
		recordSuspensionUnverified(result, operationErrors, "Sandbox confirmation inventory", sandboxListErr)
		recordSuspensionUnverified(result, operationErrors, "SandboxWarmPool confirmation inventory", warmPoolListErr)
	}
	if claimListErr == nil {
		for i := range claims.Items {
			claim := &claims.Items[i]
			claim.SetGroupVersionKind(smClaimGVK)
			if !suspensionUIDMatches(claim.GetLabels(), runUID) {
				continue
			}
			if !suspensionStepAllowed(claim.GetLabels()[LabelStep]) {
				recordSuspensionUnverified(result, operationErrors,
					"retained SandboxClaim has no valid step label: "+suspensionObjectDescription(claim),
					fmt.Errorf("invalid step %q", claim.GetLabels()[LabelStep]))
				continue
			}
			recordSuspensionPending(result, "SandboxClaim remains in authoritative inventory: "+suspensionObjectDescription(claim))
		}
	}
	if sandboxListErr == nil {
		for i := range sandboxes.Items {
			sandbox := &sandboxes.Items[i]
			sandbox.SetGroupVersionKind(smSandboxGVK)
			if !suspensionUIDMatches(sandbox.GetLabels(), runUID) {
				continue
			}
			if !suspensionStepAllowed(sandbox.GetLabels()[LabelStep]) {
				recordSuspensionUnverified(result, operationErrors,
					"retained Sandbox has no valid step label: "+suspensionObjectDescription(sandbox),
					fmt.Errorf("invalid step %q", sandbox.GetLabels()[LabelStep]))
				continue
			}
			recordSuspensionPending(result, "Sandbox remains in authoritative inventory: "+suspensionObjectDescription(sandbox))
		}
	}
	if warmPoolListErr == nil {
		for i := range warmPools.Items {
			pool := &warmPools.Items[i]
			pool.SetGroupVersionKind(smWarmPoolGVK)
			if !suspensionUIDMatches(pool.GetLabels(), runUID) {
				continue
			}
			if !suspensionStepAllowed(pool.GetLabels()[LabelStep]) {
				recordSuspensionUnverified(result, operationErrors,
					"retained SandboxWarmPool has no valid step label: "+suspensionObjectDescription(pool),
					fmt.Errorf("invalid step %q", pool.GetLabels()[LabelStep]))
				continue
			}
			recordSuspensionPending(result, "SandboxWarmPool remains in authoritative inventory: "+suspensionObjectDescription(pool))
		}
	}

	var accounts corev1.ServiceAccountList
	if err := m.reader.List(ctx, &accounts, client.InNamespace(m.namespace), suspensionRunSelector(runUID)); err != nil {
		recordSuspensionUnverified(result, operationErrors, "ServiceAccount confirmation inventory", err)
	} else {
		for i := range accounts.Items {
			account := &accounts.Items[i]
			_, managed, valid := suspensionServiceAccountStep(account.Labels, runUID)
			if !managed {
				continue
			}
			if !valid {
				recordSuspensionUnverified(result, operationErrors,
					"retained ServiceAccount has invalid component identity: "+suspensionObjectDescription(account),
					errors.New("ServiceAccount component does not identify an allowed step"))
				continue
			}
			recordSuspensionPending(result, "ServiceAccount remains in authoritative inventory: "+suspensionObjectDescription(account))
		}
	}

	var roles rbacv1.RoleList
	if err := m.reader.List(ctx, &roles, suspensionRunSelector(runUID)); err != nil {
		recordSuspensionUnverified(result, operationErrors, "Role confirmation inventory", err)
	} else {
		for i := range roles.Items {
			role := &roles.Items[i]
			_, managed, valid := suspensionRBACStep(role.Labels, role.Name, runUID)
			if !managed {
				continue
			}
			if !valid {
				recordSuspensionUnverified(result, operationErrors,
					"retained Role has invalid component identity: "+suspensionObjectDescription(role),
					errors.New("Role component does not identify an allowed step"))
				continue
			}
			if role.Labels[LabelComponent] == "result-rbac" && role.Namespace != m.namespace {
				recordSuspensionUnverified(result, operationErrors,
					"retained result Role is outside the operator namespace: "+suspensionObjectDescription(role),
					errors.New("result RBAC was found outside the operator namespace"))
				continue
			}
			recordSuspensionPending(result, "managed Role remains in authoritative inventory: "+suspensionObjectDescription(role))
		}
	}

	var roleBindings rbacv1.RoleBindingList
	if err := m.reader.List(ctx, &roleBindings, suspensionRunSelector(runUID)); err != nil {
		recordSuspensionUnverified(result, operationErrors, "RoleBinding confirmation inventory", err)
	} else {
		for i := range roleBindings.Items {
			binding := &roleBindings.Items[i]
			_, managed, valid := suspensionRBACStep(binding.Labels, binding.Name, runUID)
			if !managed {
				continue
			}
			if !valid {
				recordSuspensionUnverified(result, operationErrors,
					"retained RoleBinding has invalid component identity: "+suspensionObjectDescription(binding),
					errors.New("RoleBinding component does not identify an allowed step"))
				continue
			}
			if binding.Labels[LabelComponent] == "result-rbac" && binding.Namespace != m.namespace {
				recordSuspensionUnverified(result, operationErrors,
					"retained result RoleBinding is outside the operator namespace: "+suspensionObjectDescription(binding),
					errors.New("result RBAC was found outside the operator namespace"))
				continue
			}
			recordSuspensionPending(result, "managed RoleBinding remains in authoritative inventory: "+suspensionObjectDescription(binding))
		}
	}

	var clusterRoles rbacv1.ClusterRoleList
	if err := m.reader.List(ctx, &clusterRoles, suspensionRunSelector(runUID)); err != nil {
		recordSuspensionUnverified(result, operationErrors, "ClusterRole confirmation inventory", err)
	} else {
		for i := range clusterRoles.Items {
			role := &clusterRoles.Items[i]
			component := role.Labels[LabelComponent]
			if suspensionUIDMatches(role.Labels, runUID) && (component == "result-rbac" || component == "reader-rbac") {
				recordSuspensionUnverified(result, operationErrors,
					"namespace-scoped RBAC component remains on ClusterRole: "+suspensionObjectDescription(role),
					fmt.Errorf("unexpected ClusterRole component %q", component))
				continue
			}
			_, managed, valid := suspensionRBACStep(role.Labels, role.Name, runUID)
			if !managed {
				continue
			}
			if !valid {
				recordSuspensionUnverified(result, operationErrors,
					"retained ClusterRole has invalid component identity: "+suspensionObjectDescription(role),
					errors.New("ClusterRole component does not identify an allowed step"))
				continue
			}
			recordSuspensionPending(result, "managed ClusterRole remains in authoritative inventory: "+suspensionObjectDescription(role))
		}
	}

	var clusterRoleBindings rbacv1.ClusterRoleBindingList
	if err := m.reader.List(ctx, &clusterRoleBindings, suspensionRunSelector(runUID)); err != nil {
		recordSuspensionUnverified(result, operationErrors, "ClusterRoleBinding confirmation inventory", err)
	} else {
		for i := range clusterRoleBindings.Items {
			binding := &clusterRoleBindings.Items[i]
			if !suspensionUIDMatches(binding.Labels, runUID) {
				continue
			}
			component := binding.Labels[LabelComponent]
			if component == "reader-rbac" {
				if !suspensionStepAllowed(binding.Labels[LabelStep]) {
					recordSuspensionUnverified(result, operationErrors,
						"retained temporary reader ClusterRoleBinding has invalid step identity: "+suspensionObjectDescription(binding),
						fmt.Errorf("invalid step %q", binding.Labels[LabelStep]))
					continue
				}
			} else if component == "execution-rbac" {
				if binding.Labels[LabelStep] != "" && binding.Labels[LabelStep] != "execution" {
					recordSuspensionUnverified(result, operationErrors,
						"retained execution ClusterRoleBinding has conflicting step identity: "+suspensionObjectDescription(binding),
						fmt.Errorf("invalid step %q", binding.Labels[LabelStep]))
					continue
				}
			} else if component == "result-rbac" {
				recordSuspensionUnverified(result, operationErrors,
					"namespace-scoped result RBAC component remains on ClusterRoleBinding: "+suspensionObjectDescription(binding),
					errors.New("result RBAC was found in cluster scope"))
				continue
			} else {
				continue
			}
			recordSuspensionPending(result, "managed ClusterRoleBinding remains in authoritative inventory: "+suspensionObjectDescription(binding))
		}
	}

	var secrets corev1.SecretList
	if err := m.reader.List(ctx, &secrets, client.InNamespace(m.namespace), suspensionRunSelector(runUID)); err != nil {
		recordSuspensionUnverified(result, operationErrors, "credential Secret confirmation inventory", err)
	} else {
		for i := range secrets.Items {
			secret := &secrets.Items[i]
			if !suspensionUIDMatches(secret.Labels, runUID) || secret.Labels[LabelComponent] != "sandbox-kubeconfig" {
				continue
			}
			step := suspensionCredentialStep(secret.Name, secret.Labels[LabelRun])
			if !suspensionStepAllowed(step) || (secret.Labels[LabelStep] != "" && secret.Labels[LabelStep] != step) {
				recordSuspensionUnverified(result, operationErrors,
					"retained sandbox credential Secret has invalid step identity: "+suspensionObjectDescription(secret),
					fmt.Errorf("invalid step identity %q", secret.Labels[LabelStep]))
				continue
			}
			recordSuspensionPending(result, "sandbox credential Secret remains in authoritative inventory: "+suspensionObjectDescription(secret))
		}
	}
}

func suspensionObjectDescription(object client.Object) string {
	return fmt.Sprintf("%T %s/%s UID %s", object, object.GetNamespace(), object.GetName(), object.GetUID())
}

func recordSuspensionPending(result *SuspensionCleanup, message string) {
	appendSuspensionMessage(&result.Pending, message)
}

func recordSuspensionFailure(result *SuspensionCleanup, operationErrors *[]error, message string, err error) {
	appendSuspensionMessage(&result.Failures, message+": "+err.Error())
	*operationErrors = append(*operationErrors, fmt.Errorf("%s: %w", message, err))
}

func recordSuspensionUnverified(result *SuspensionCleanup, operationErrors *[]error, message string, err error) {
	if err == nil {
		return
	}
	*operationErrors = append(*operationErrors, fmt.Errorf("%s: %w", message, err))
	appendSuspensionMessage(&result.Unverified, message+": "+err.Error())
}

func appendSuspensionMessage(messages *[]string, message string) {
	for _, existing := range *messages {
		if existing == message {
			return
		}
	}
	*messages = append(*messages, message)
}
