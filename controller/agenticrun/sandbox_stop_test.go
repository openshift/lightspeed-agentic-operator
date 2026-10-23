package agenticrun

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
)

var stopTestTemplateGVK = schema.GroupVersionKind{
	Group: "extensions.agents.x-k8s.io", Version: "v1beta1", Kind: "SandboxTemplate",
}

func stopTestScheme() *runtime.Scheme {
	scheme := testScheme()
	for _, gvk := range []schema.GroupVersionKind{smClaimGVK, smSandboxGVK, sandboxWarmPoolGVK, stopTestTemplateGVK} {
		scheme.AddKnownTypeWithName(gvk, &unstructured.Unstructured{})
		scheme.AddKnownTypeWithName(gvk.GroupVersion().WithKind(gvk.Kind+"List"), &unstructured.UnstructuredList{})
		metav1.AddToGroupVersion(scheme, gvk.GroupVersion())
	}
	return scheme
}

func sandboxStopTestObjects(run *agenticv1alpha1.AgenticRun, podFinalizer, claimFinalizer bool) (*corev1.Pod, *unstructured.Unstructured, *unstructured.Unstructured, *unstructured.Unstructured, *unstructured.Unstructured) {
	claimName := "ls-analysis-" + string(run.UID)
	runOwner := metav1.OwnerReference{
		APIVersion: agenticv1alpha1.GroupVersion.String(),
		Kind:       "AgenticRun",
		Name:       run.Name,
		UID:        run.UID,
	}
	claim := &unstructured.Unstructured{Object: map[string]any{}}
	claim.SetGroupVersionKind(smClaimGVK)
	claim.SetName(claimName)
	claim.SetNamespace("test-ns")
	claim.SetUID(types.UID("claim-" + string(run.UID)))
	claim.SetOwnerReferences([]metav1.OwnerReference{runOwner})
	_ = unstructured.SetNestedField(claim.Object, claimName, "spec", "warmPoolRef", "name")
	_ = unstructured.SetNestedField(claim.Object, claimName, "status", "sandbox", "name")
	if claimFinalizer {
		claim.SetFinalizers([]string{"test.agentic.openshift.io/claim-cleanup"})
	}

	sandbox := &unstructured.Unstructured{Object: map[string]any{}}
	sandbox.SetGroupVersionKind(smSandboxGVK)
	sandbox.SetName(claimName)
	sandbox.SetNamespace("test-ns")
	sandbox.SetUID(types.UID("sandbox-" + string(run.UID)))
	sandbox.SetOwnerReferences([]metav1.OwnerReference{{
		APIVersion: smClaimGVK.GroupVersion().String(),
		Kind:       smClaimGVK.Kind,
		Name:       claimName,
		UID:        claim.GetUID(),
	}})
	sandbox.SetAnnotations(map[string]string{sandboxPodNameAnnotation: "backing-analysis-pod"})

	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name:      "backing-analysis-pod",
		Namespace: "test-ns",
		UID:       types.UID("pod-" + string(run.UID)),
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: smSandboxGVK.GroupVersion().String(),
			Kind:       smSandboxGVK.Kind,
			Name:       claimName,
			UID:        sandbox.GetUID(),
		}},
	}}
	if podFinalizer {
		pod.Finalizers = []string{"test.agentic.openshift.io/pod-cleanup"}
	}

	pool := &unstructured.Unstructured{Object: map[string]any{}}
	pool.SetGroupVersionKind(sandboxWarmPoolGVK)
	pool.SetName(claimName)
	pool.SetNamespace("test-ns")
	pool.SetUID(types.UID("pool-" + string(run.UID)))
	pool.SetLabels(map[string]string{LabelRun: string(run.UID)})
	pool.SetOwnerReferences([]metav1.OwnerReference{runOwner})
	_ = unstructured.SetNestedField(pool.Object, int64(0), "spec", "replicas")
	_ = unstructured.SetNestedField(pool.Object, claimName, "spec", "sandboxTemplateRef", "name")

	// This template identity may be reused; per-run cleanup must never remove it.
	template := &unstructured.Unstructured{Object: map[string]any{}}
	template.SetGroupVersionKind(stopTestTemplateGVK)
	template.SetName(claimName)
	template.SetNamespace("test-ns")
	return pod, claim, sandbox, pool, template
}

func newSandboxStopManager(t *testing.T, objects ...client.Object) (*SandboxManager, client.Client) {
	t.Helper()
	fc := fake.NewClientBuilder().WithScheme(stopTestScheme()).WithObjects(objects...).Build()
	return &SandboxManager{client: fc, APIReader: fc, namespace: "test-ns"}, fc
}

func getUnstructuredForStopTest(t *testing.T, c client.Client, gvk schema.GroupVersionKind, key types.NamespacedName) *unstructured.Unstructured {
	t.Helper()
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gvk)
	if err := c.Get(context.Background(), key, obj); err != nil {
		t.Fatalf("get %s %s/%s: %v", gvk.Kind, key.Namespace, key.Name, err)
	}
	return obj
}

func TestStopSandboxesDiscoversOwnerChainWithoutStatusAndRetainsTemplate(t *testing.T) {
	ctx := context.Background()
	run := testSMRun()
	pod, claim, sandbox, pool, template := sandboxStopTestObjects(run, false, false)
	manager, fc := newSandboxStopManager(t, run, pod, claim, sandbox, pool, template)

	complete, err := manager.StopSandboxes(ctx, run)
	if err != nil {
		t.Fatalf("StopSandboxes() error = %v", err)
	}
	if !complete {
		t.Fatal("StopSandboxes() confirmed=false, want true")
	}

	for _, obj := range []struct {
		kind schema.GroupVersionKind
		key  types.NamespacedName
	}{
		{kind: smClaimGVK, key: types.NamespacedName{Namespace: "test-ns", Name: claim.GetName()}},
		{kind: smSandboxGVK, key: types.NamespacedName{Namespace: "test-ns", Name: sandbox.GetName()}},
		{kind: sandboxWarmPoolGVK, key: types.NamespacedName{Namespace: "test-ns", Name: pool.GetName()}},
	} {
		resource := &unstructured.Unstructured{}
		resource.SetGroupVersionKind(obj.kind)
		if err := fc.Get(ctx, obj.key, resource); !apierrors.IsNotFound(err) {
			t.Errorf("%s remains after confirmed stop: Get error = %v", obj.kind.Kind, err)
		}
	}
	var remainingPod corev1.Pod
	if err := fc.Get(ctx, client.ObjectKeyFromObject(pod), &remainingPod); !apierrors.IsNotFound(err) {
		t.Errorf("backing Pod remains after confirmed stop: Get error = %v", err)
	}
	getUnstructuredForStopTest(t, fc, stopTestTemplateGVK, types.NamespacedName{Namespace: "test-ns", Name: template.GetName()})
}
func TestStopSandboxesUsesStatusReferencesForAllSteps(t *testing.T) {
	ctx := context.Background()
	run := testSMRun()
	infos := []*agenticv1alpha1.SandboxInfo{
		&run.Status.Steps.Analysis.Sandbox,
		&run.Status.Steps.Execution.Sandbox,
		&run.Status.Steps.Verification.Sandbox,
		&run.Status.Steps.Escalation.Sandbox,
	}
	objects := []client.Object{run}
	names := make([]string, len(sandboxStopSteps))
	for idx, step := range sandboxStopSteps {
		name := "ls-" + step + "-" + string(run.UID)
		names[idx] = name
		*infos[idx] = agenticv1alpha1.SandboxInfo{ClaimName: name, Namespace: "test-ns"}
		objects = append(objects, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "test-ns",
			UID:       types.UID("pod-" + step + "-" + string(run.UID)),
		}})
	}
	manager, fc := newSandboxStopManager(t, objects...)

	complete, err := manager.StopSandboxes(ctx, run)
	if err != nil {
		t.Fatalf("StopSandboxes() error = %v", err)
	}
	if !complete {
		t.Fatal("StopSandboxes() confirmed=false, want true")
	}
	for idx, name := range names {
		var pod corev1.Pod
		if err := fc.Get(ctx, types.NamespacedName{Namespace: "test-ns", Name: name}, &pod); !apierrors.IsNotFound(err) {
			t.Errorf("%s Pod remains after confirmed stop: Get error = %v", sandboxStopSteps[idx], err)
		}
		if infos[idx].ClaimName != name {
			t.Errorf("StopSandboxes cleared %s status reference before lifecycle confirmation", sandboxStopSteps[idx])
		}
	}
}
func TestStopSandboxesWaitsForPodAndClaimFinalizers(t *testing.T) {

	ctx := context.Background()
	run := testSMRun()
	pod, claim, sandbox, pool, template := sandboxStopTestObjects(run, true, true)
	manager, fc := newSandboxStopManager(t, run, pod, claim, sandbox, pool, template)

	complete, err := manager.StopSandboxes(ctx, run)
	if err != nil {
		t.Fatalf("StopSandboxes() error = %v", err)
	}
	if complete {
		t.Fatal("StopSandboxes() confirmed teardown while Pod and claim finalizers remain")
	}

	var remainingPod corev1.Pod
	if err := fc.Get(ctx, client.ObjectKeyFromObject(pod), &remainingPod); err != nil {
		t.Fatalf("get finalizing backing Pod: %v", err)
	}
	if remainingPod.DeletionTimestamp.IsZero() {
		t.Fatal("backing Pod Delete was not persisted")
	}
	remainingClaim := getUnstructuredForStopTest(t, fc, smClaimGVK, types.NamespacedName{Namespace: "test-ns", Name: claim.GetName()})
	if remainingClaim.GetDeletionTimestamp().IsZero() {
		t.Fatal("SandboxClaim Delete was not persisted")
	}
	getUnstructuredForStopTest(t, fc, stopTestTemplateGVK, types.NamespacedName{Namespace: "test-ns", Name: template.GetName()})
}

type recordingStopClient struct {
	client.Client
	deleteErrors map[string]error
	deleteKinds  []string
	podGrace     []int64
}

func (c *recordingStopClient) Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
	kind := stopTestObjectKind(obj)
	c.deleteKinds = append(c.deleteKinds, kind)
	deleteOptions := &client.DeleteOptions{}
	for _, opt := range opts {
		opt.ApplyToDelete(deleteOptions)
	}
	if _, ok := obj.(*corev1.Pod); ok {
		if deleteOptions.GracePeriodSeconds == nil {
			c.podGrace = append(c.podGrace, -1)
		} else {
			c.podGrace = append(c.podGrace, *deleteOptions.GracePeriodSeconds)
		}
	}
	if err := c.deleteErrors[kind+"/"+obj.GetName()]; err != nil {
		return err
	}
	return c.Client.Delete(ctx, obj, opts...)
}

func stopTestObjectKind(obj client.Object) string {
	switch value := obj.(type) {
	case *corev1.Pod:
		return "Pod"
	case *corev1.ConfigMap:
		return "ConfigMap"
	case *corev1.Secret:
		return "Secret"
	case *corev1.ServiceAccount:
		return "ServiceAccount"
	case *rbacv1.Role:
		return "Role"
	case *rbacv1.RoleBinding:
		return "RoleBinding"
	case *rbacv1.ClusterRole:
		return "ClusterRole"
	case *rbacv1.ClusterRoleBinding:
		return "ClusterRoleBinding"
	case *unstructured.Unstructured:
		return value.GetKind()
	default:
		return obj.GetObjectKind().GroupVersionKind().Kind
	}
}

func TestStopSandboxesAggregatesDeleteErrorsAndAttemptsEveryWorkload(t *testing.T) {
	ctx := context.Background()
	run := testSMRun()
	pod, claim, sandbox, pool, template := sandboxStopTestObjects(run, false, false)
	podErr := errors.New("Pod delete denied")
	claimErr := errors.New("claim delete denied")
	fc := fake.NewClientBuilder().WithScheme(stopTestScheme()).WithObjects(run, pod, claim, sandbox, pool, template).Build()
	recording := &recordingStopClient{
		Client: fc,
		deleteErrors: map[string]error{
			"Pod/" + pod.Name:                 podErr,
			"SandboxClaim/" + claim.GetName(): claimErr,
		},
	}
	manager := &SandboxManager{client: recording, APIReader: fc, namespace: "test-ns"}

	complete, err := manager.StopSandboxes(ctx, run)
	if complete {
		t.Fatal("StopSandboxes() confirmed teardown after delete errors")
	}
	if !errors.Is(err, podErr) || !errors.Is(err, claimErr) {
		t.Fatalf("StopSandboxes() error = %v, want both Pod and claim errors", err)
	}
	seen := map[string]bool{}
	for _, kind := range recording.deleteKinds {
		seen[kind] = true
	}
	for _, kind := range []string{"Pod", "Sandbox", "SandboxClaim", "SandboxWarmPool"} {
		if !seen[kind] {
			t.Errorf("cleanup did not attempt %s deletion after earlier errors", kind)
		}
	}
	if len(recording.podGrace) != 1 || recording.podGrace[0] != 0 {
		t.Fatalf("Pod grace periods = %v, want [0]", recording.podGrace)
	}
	getUnstructuredForStopTest(t, fc, stopTestTemplateGVK, types.NamespacedName{Namespace: "test-ns", Name: template.GetName()})
}

func TestStopSandboxesRevokesUnlabelledReaderSubjectsAndUIDOwnedExecutionRBAC(t *testing.T) {
	ctx := context.Background()
	run := testSMRun()
	saName := sandboxSAName(run, "analysis")
	readerRole := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "unlabelled-reader", Namespace: "target"},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: "reader"},
		Subjects:   []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: saName, Namespace: "test-ns"}},
	}
	sharedReader := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "shared-reader"},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "reader"},
		Subjects: []rbacv1.Subject{
			{Kind: rbacv1.ServiceAccountKind, Name: saName, Namespace: "test-ns"},
			{Kind: rbacv1.ServiceAccountKind, Name: "other", Namespace: "target"},
		},
	}
	role := &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{
		Name:      executionRoleName(string(run.UID)),
		Namespace: "target",
		Labels:    rbacLabels(string(run.UID), "execution-rbac"),
	}}
	roleBinding := &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{
		Name:      executionRoleName(string(run.UID)),
		Namespace: "target",
		Labels:    rbacLabels(string(run.UID), "execution-rbac"),
	}}
	clusterRole := &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{
		Name:   clusterRoleName(string(run.UID)),
		Labels: rbacLabels(string(run.UID), "execution-rbac"),
	}}
	clusterRoleBinding := &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{
		Name:   clusterRoleName(string(run.UID)),
		Labels: rbacLabels(string(run.UID), "execution-rbac"),
	}}
	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: saName, Namespace: "test-ns"}}
	manager, fc := newSandboxStopManager(t, run, readerRole, sharedReader, role, roleBinding, clusterRole, clusterRoleBinding, sa)

	complete, err := manager.StopSandboxes(ctx, run)
	if err != nil {
		t.Fatalf("StopSandboxes() error = %v", err)
	}
	if !complete {
		t.Fatal("StopSandboxes() confirmed=false, want true")
	}
	for _, obj := range []client.Object{readerRole, role, roleBinding, clusterRole, clusterRoleBinding, sa} {
		if err := fc.Get(ctx, client.ObjectKeyFromObject(obj), obj); !apierrors.IsNotFound(err) {
			t.Errorf("run-owned %T remains: Get error = %v", obj, err)
		}
	}
	var remainingShared rbacv1.ClusterRoleBinding
	if err := fc.Get(ctx, client.ObjectKeyFromObject(sharedReader), &remainingShared); err != nil {
		t.Fatalf("get shared reader binding: %v", err)
	}
	if len(remainingShared.Subjects) != 1 || remainingShared.Subjects[0].Name != "other" {
		t.Fatalf("shared binding subjects = %+v, want only unrelated subject", remainingShared.Subjects)
	}
}

func TestReleaseSandboxClaimRetainsReusableTemplate(t *testing.T) {
	ctx := context.Background()
	run := testSMRun()
	pod, claim, _, pool, template := sandboxStopTestObjects(run, false, false)
	run.Status.Steps.Analysis.Sandbox = agenticv1alpha1.SandboxInfo{ClaimName: claim.GetName(), Namespace: "test-ns"}
	cache := testCache(t, sandboxModeSandboxClaim)
	fc := fake.NewClientBuilder().WithScheme(stopTestScheme()).WithObjects(testReaderCRB(), pod, claim, pool, template).Build()
	manager := newTestSandboxManager(fc, cache)

	if err := manager.Release(ctx, run, "analysis", nil); err != nil {
		t.Fatalf("Release() error = %v", err)
	}
	getUnstructuredForStopTest(t, fc, stopTestTemplateGVK, types.NamespacedName{Namespace: "test-ns", Name: template.GetName()})
}

func TestStopSandboxesRetainsRefsAcrossRetries(t *testing.T) {
	ctx := context.Background()
	run := testSMRun()
	pod, claim, sandbox, pool, template := sandboxStopTestObjects(run, true, false)
	manager, fc := newSandboxStopManager(t, run, pod, claim, sandbox, pool, template)

	complete, err := manager.StopSandboxes(ctx, run)
	if err != nil {
		t.Fatalf("StopSandboxes() error = %v", err)
	}
	if complete {
		t.Fatal("StopSandboxes() confirmed teardown while the backing Pod is finalizing")
	}

	stored := &agenticv1alpha1.AgenticRun{}
	if err := fc.Get(ctx, client.ObjectKeyFromObject(run), stored); err != nil {
		t.Fatalf("get run: %v", err)
	}
	var refs stopPendingRefs
	if err := json.Unmarshal([]byte(stored.Annotations[stopPendingRefsAnnotation]), &refs); err != nil {
		t.Fatalf("parse %s annotation %q: %v", stopPendingRefsAnnotation, stored.Annotations[stopPendingRefsAnnotation], err)
	}
	podRef := "test-ns/" + pod.Name
	if !slices.Contains(refs.Pods, podRef) {
		t.Fatalf("pending pod refs = %v, want %q", refs.Pods, podRef)
	}

	// The Sandbox and Claim owner chain is gone, so only the recorded identity
	// can still see the Pod. A retry must report pending, not confirm teardown.
	complete, err = manager.StopSandboxes(ctx, run)
	if err != nil {
		t.Fatalf("StopSandboxes() retry error = %v", err)
	}
	if complete {
		t.Fatal("StopSandboxes() confirmed teardown on retry while the finalizing Pod still exists")
	}

	finalizing := &corev1.Pod{}
	if err := fc.Get(ctx, client.ObjectKeyFromObject(pod), finalizing); err != nil {
		t.Fatalf("get finalizing Pod: %v", err)
	}
	finalizing.Finalizers = nil
	if err := fc.Update(ctx, finalizing); err != nil {
		t.Fatalf("remove Pod finalizer: %v", err)
	}
	if err := fc.Delete(ctx, finalizing); err != nil && !apierrors.IsNotFound(err) {
		t.Fatalf("delete retained Pod: %v", err)
	}

	complete, err = manager.StopSandboxes(ctx, run)
	if err != nil {
		t.Fatalf("StopSandboxes() after Pod removal error = %v", err)
	}
	if !complete {
		t.Fatal("StopSandboxes() confirmed=false after the retained Pod was removed")
	}
	if err := fc.Get(ctx, client.ObjectKeyFromObject(run), stored); err != nil {
		t.Fatalf("get run after confirmation: %v", err)
	}
	if value, found := stored.Annotations[stopPendingRefsAnnotation]; found {
		t.Fatalf("pending stop refs annotation not cleared after confirmation: %q", value)
	}
}

func TestStopSandboxesUsesAPIReaderForConfirmation(t *testing.T) {
	ctx := context.Background()
	run := testSMRun()
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name:      "run-owned-pod",
		Namespace: "test-ns",
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: agenticv1alpha1.GroupVersion.String(),
			Kind:       "AgenticRun",
			Name:       run.Name,
			UID:        run.UID,
		}},
	}}
	cached := fake.NewClientBuilder().WithScheme(stopTestScheme()).WithObjects(run).Build()
	live := fake.NewClientBuilder().WithScheme(stopTestScheme()).WithObjects(run, pod).Build()
	manager := &SandboxManager{client: cached, APIReader: live, namespace: "test-ns"}

	complete, err := manager.StopSandboxes(ctx, run)
	if err != nil {
		t.Fatalf("StopSandboxes() error = %v", err)
	}
	if complete {
		t.Fatal("StopSandboxes() confirmed from the cached client while the authoritative reader still sees the Pod")
	}

	if err := live.Delete(ctx, pod); err != nil {
		t.Fatalf("delete Pod from authoritative reader: %v", err)
	}
	complete, err = manager.StopSandboxes(ctx, run)
	if err != nil {
		t.Fatalf("StopSandboxes() error = %v", err)
	}
	if !complete {
		t.Fatal("StopSandboxes() confirmed=false after the Pod was removed from the authoritative reader")
	}
}

func TestStopSandboxesCleansSpokeAccess(t *testing.T) {
	ctx := context.Background()
	run := testSMRun()
	run.Spec.TargetCluster = "spoke-a"

	kubeconfig := []byte(`
apiVersion: v1
kind: Config
clusters:
- cluster:
    server: https://api.spoke.example.com:6443
  name: spoke
contexts:
- context:
    cluster: spoke
    user: spoke-user
  name: spoke
current-context: spoke
users:
- name: spoke-user
  user:
    token: test-token
`)
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: spokeKubeconfigPrefix + "spoke-a", Namespace: "test-ns"},
		Data:       map[string][]byte{kubeconfigKey: kubeconfig},
	}

	saName := sandboxSAName(run, "execution")
	roleName := executionRoleName(string(run.UID))
	clusterRole := clusterRoleName(string(run.UID))
	const unrelatedSA = "unrelated-service-account"
	spokeObjects := []client.Object{
		&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: saName, Namespace: spokeManagedNamespace}},
		&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: unrelatedSA, Namespace: spokeManagedNamespace}},
		&rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: roleName, Namespace: spokeManagedNamespace}},
		&rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: roleName, Namespace: spokeManagedNamespace},
			Subjects:   []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: saName, Namespace: spokeManagedNamespace}},
		},
		&rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: clusterRole}},
		&rbacv1.ClusterRoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: clusterRole},
			Subjects:   []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: saName, Namespace: spokeManagedNamespace}},
		},
	}
	spokeClient := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(spokeObjects...).Build()
	originalNewClient := NewClientFromConfig
	NewClientFromConfig = func(*rest.Config) (client.Client, error) { return spokeClient, nil }
	t.Cleanup(func() { NewClientFromConfig = originalNewClient })

	manager, _ := newSandboxStopManager(t, run, secret)
	complete, err := manager.StopSandboxes(ctx, run)
	if err != nil {
		t.Fatalf("StopSandboxes() error = %v", err)
	}
	if !complete {
		t.Fatal("StopSandboxes() confirmed=false, want true")
	}

	for _, obj := range []client.Object{
		&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: saName, Namespace: spokeManagedNamespace}},
		&rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: roleName, Namespace: spokeManagedNamespace}},
		&rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: roleName, Namespace: spokeManagedNamespace}},
		&rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: clusterRole}},
		&rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: clusterRole}},
	} {
		if err := spokeClient.Get(ctx, client.ObjectKeyFromObject(obj), obj); !apierrors.IsNotFound(err) {
			t.Errorf("%T %s remains on the spoke after confirmed stop: Get error = %v", obj, obj.GetName(), err)
		}
	}
	unrelated := &corev1.ServiceAccount{}
	if err := spokeClient.Get(ctx, types.NamespacedName{Namespace: spokeManagedNamespace, Name: unrelatedSA}, unrelated); err != nil {
		t.Errorf("unrelated ServiceAccount was removed: %v", err)
	}
}

func TestStopSandboxesDeletesLabelOnlyPod(t *testing.T) {
	ctx := context.Background()
	run := testSMRun()
	labelled := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name:      "labelled-pod",
		Namespace: "test-ns",
		Labels:    map[string]string{LabelRun: string(run.UID)},
	}}
	unrelated := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name:      "unrelated-pod",
		Namespace: "test-ns",
		Labels:    map[string]string{LabelRun: "someone-else"},
	}}
	manager, fc := newSandboxStopManager(t, run, labelled, unrelated)

	complete, err := manager.StopSandboxes(ctx, run)
	if err != nil {
		t.Fatalf("StopSandboxes() error = %v", err)
	}
	if !complete {
		t.Fatal("StopSandboxes() confirmed=false, want true")
	}
	var pod corev1.Pod
	if err := fc.Get(ctx, client.ObjectKeyFromObject(labelled), &pod); !apierrors.IsNotFound(err) {
		t.Fatalf("label-only Pod remains after confirmed stop: Get error = %v", err)
	}
	if err := fc.Get(ctx, client.ObjectKeyFromObject(unrelated), &pod); err != nil {
		t.Fatalf("unrelated Pod was removed: %v", err)
	}
}

func TestStopSandboxesRetainedLabelOnlyPodBlocksConfirmation(t *testing.T) {
	ctx := context.Background()
	run := testSMRun()
	labelled := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name:       "labelled-pod",
		Namespace:  "test-ns",
		Labels:     map[string]string{LabelRun: string(run.UID)},
		Finalizers: []string{"test.agentic.openshift.io/pod-cleanup"},
	}}
	manager, fc := newSandboxStopManager(t, run, labelled)

	complete, err := manager.StopSandboxes(ctx, run)
	if err != nil {
		t.Fatalf("StopSandboxes() error = %v", err)
	}
	if complete {
		t.Fatal("StopSandboxes() confirmed teardown while the label-matched Pod is finalizing")
	}
	var pod corev1.Pod
	if err := fc.Get(ctx, client.ObjectKeyFromObject(labelled), &pod); err != nil {
		t.Fatalf("get label-matched Pod: %v", err)
	}
	if pod.DeletionTimestamp.IsZero() {
		t.Fatal("label-matched Pod Delete was not issued")
	}
}
