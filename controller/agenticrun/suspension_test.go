package agenticrun

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
)

func suspensionReportMentions(report SuspensionCleanup, text string) bool {
	for _, message := range report.Unverified {
		if strings.Contains(message, text) {
			return true
		}
	}
	return false
}

func TestCleanupSuspensionTreatsAllMissingOptionalAPIsAsEmptyInBareMode(t *testing.T) {
	fc := fake.NewClientBuilder().WithScheme(testScheme()).Build()
	crdNames := []string{
		"sandboxclaims.extensions.agents.x-k8s.io",
		"sandboxes.agents.x-k8s.io",
		"sandboxwarmpools.extensions.agents.x-k8s.io",
	}
	crdResource := schema.GroupResource{Group: "apiextensions.k8s.io", Resource: "customresourcedefinitions"}
	getErrors := make(map[string]error, len(crdNames))
	for _, name := range crdNames {
		getErrors[name] = apierrors.NewNotFound(crdResource, name)
	}
	reader := &suspensionListErrorReader{
		Reader:          fc,
		listErrorsByGVK: suspensionOptionalListNoMatchErrors(),
		getErrors:       getErrors,
	}
	cache := testCache(t, "bare-pod")
	cache.ForceBareMode = true
	manager := NewSandboxManager(fc, reader, cache, "test-ns", nil)

	got, err := manager.CleanupSuspension(context.Background(), "")
	if err != nil {
		t.Fatalf("definitive optional API absence was not accepted in bare mode: %v", err)
	}
	if !got.Confirmed() {
		t.Fatalf("all absent optional APIs did not produce an empty inventory: %+v", got)
	}
}

func TestCleanupSuspensionRejectsPartialOptionalCRDPresence(t *testing.T) {
	fc := fake.NewClientBuilder().WithScheme(testScheme()).Build()
	crdNames := []string{
		"sandboxclaims.extensions.agents.x-k8s.io",
		"sandboxes.agents.x-k8s.io",
		"sandboxwarmpools.extensions.agents.x-k8s.io",
	}
	crdResource := schema.GroupResource{Group: "apiextensions.k8s.io", Resource: "customresourcedefinitions"}
	reader := &suspensionListErrorReader{
		Reader:          fc,
		listErrorsByGVK: suspensionOptionalListNoMatchErrors(),
		getErrors: map[string]error{
			crdNames[1]: apierrors.NewNotFound(crdResource, crdNames[1]),
			crdNames[2]: apierrors.NewNotFound(crdResource, crdNames[2]),
		},
		existingCRDs: map[string]bool{crdNames[0]: true},
	}
	cache := testCache(t, "bare-pod")
	cache.ForceBareMode = true
	manager := NewSandboxManager(fc, reader, cache, "test-ns", nil)

	got, err := manager.CleanupSuspension(context.Background(), "")
	if err == nil || got.Confirmed() ||
		!suspensionReportMentions(got, "optional CRD is present despite an unavailable inventory mapping: "+crdNames[0]) {
		t.Fatalf("partial optional API presence was not identified as unverified: report=%+v err=%v", got, err)
	}
}

func TestCleanupSuspensionRejectsOptionalCRDProbeFailure(t *testing.T) {
	fc := fake.NewClientBuilder().WithScheme(testScheme()).Build()
	crdNames := []string{
		"sandboxclaims.extensions.agents.x-k8s.io",
		"sandboxes.agents.x-k8s.io",
		"sandboxwarmpools.extensions.agents.x-k8s.io",
	}
	crdResource := schema.GroupResource{Group: "apiextensions.k8s.io", Resource: "customresourcedefinitions"}
	reader := &suspensionListErrorReader{
		Reader:          fc,
		listErrorsByGVK: suspensionOptionalListNoMatchErrors(),
		getErrors: map[string]error{
			crdNames[0]: apierrors.NewForbidden(crdResource, crdNames[0], errors.New("injected CRD probe denial")),
			crdNames[1]: apierrors.NewNotFound(crdResource, crdNames[1]),
			crdNames[2]: apierrors.NewNotFound(crdResource, crdNames[2]),
		},
	}
	cache := testCache(t, "bare-pod")
	cache.ForceBareMode = true
	manager := NewSandboxManager(fc, reader, cache, "test-ns", nil)

	got, err := manager.CleanupSuspension(context.Background(), "")
	if err == nil || got.Confirmed() ||
		!suspensionReportMentions(got, "confirm optional CRD absence "+crdNames[0]) {
		t.Fatalf("CRD probe failure was not identified as unverified: report=%+v err=%v", got, err)
	}
}

func TestCleanupSuspensionKeepsMixedOptionalDiscoveryErrorsUnverified(t *testing.T) {
	for _, injected := range []struct {
		name string
		err  error
	}{
		{name: "Forbidden", err: apierrors.NewForbidden(schema.GroupResource{Group: smSandboxGVK.Group, Resource: "sandboxes"}, "", errors.New("injected Forbidden"))},
		{name: "timeout", err: context.DeadlineExceeded},
		{name: "discovery failure", err: fmt.Errorf("injected RESTMapper discovery failure")},
	} {
		t.Run(injected.name, func(t *testing.T) {
			fc := fake.NewClientBuilder().WithScheme(testScheme()).Build()
			sandboxListGVK := smSandboxGVK.GroupVersion().WithKind(smSandboxGVK.Kind + "List")
			listErrors := suspensionOptionalListNoMatchErrors()
			listErrors[sandboxListGVK] = injected.err
			reader := &suspensionListErrorReader{
				Reader:          fc,
				listErrorsByGVK: listErrors,
			}
			cache := testCache(t, "bare-pod")
			cache.ForceBareMode = true
			manager := NewSandboxManager(fc, reader, cache, "test-ns", nil)

			got, err := manager.CleanupSuspension(context.Background(), "")
			if err == nil || got.Confirmed() || !suspensionReportMentions(got, "Sandbox inventory") {
				t.Fatalf("mixed optional discovery errors were not identified as unverified: report=%+v err=%v", got, err)
			}
		})
	}
}

func suspensionTestScheme() *runtime.Scheme {
	s := testScheme()
	for _, gvk := range []schema.GroupVersionKind{
		smClaimGVK,
		smSandboxGVK,
		smWarmPoolGVK,
		{Group: "extensions.agents.x-k8s.io", Version: "v1beta1", Kind: "SandboxTemplate"},
	} {
		s.AddKnownTypeWithName(gvk, &unstructured.Unstructured{})
		s.AddKnownTypeWithName(gvk.GroupVersion().WithKind(gvk.Kind+"List"), &unstructured.UnstructuredList{})
	}
	return s
}

func suspensionTestObject(gvk schema.GroupVersionKind, name, namespace string, uid types.UID, labels map[string]string) *unstructured.Unstructured {
	object := &unstructured.Unstructured{}
	object.SetGroupVersionKind(gvk)
	object.SetName(name)
	object.SetNamespace(namespace)
	object.SetUID(uid)
	object.SetResourceVersion("1")
	object.SetLabels(labels)
	return object
}

func suspensionOwnerRef(gvk schema.GroupVersionKind, name string, uid types.UID) metav1.OwnerReference {
	controller := true
	return metav1.OwnerReference{
		APIVersion: gvk.GroupVersion().String(),
		Kind:       gvk.Kind,
		Name:       name,
		UID:        uid,
		Controller: &controller,
	}
}

func suspensionManagedPod(name string, uid types.UID, runUID types.UID, step string, phase corev1.PodPhase) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:            name,
			Namespace:       "test-ns",
			UID:             uid,
			ResourceVersion: "1",
			Labels:          map[string]string{LabelRun: string(runUID), LabelStep: step},
		},
		Status: corev1.PodStatus{Phase: phase},
	}
}

type suspensionFaultClient struct {
	client.Client
	failPodName        string
	holdSAName         string
	replacePod         string
	replacement        *corev1.Pod
	replaced           bool
	precondition       types.UID
	grace              *int64
	podPrecondition    types.UID
	podGrace           *int64
	sandboxPropagation *metav1.DeletionPropagation
}

func (c *suspensionFaultClient) Delete(ctx context.Context, object client.Object, options ...client.DeleteOption) error {
	deleteOptions := (&client.DeleteOptions{}).ApplyOptions(options)
	if deleteOptions.Preconditions != nil && deleteOptions.Preconditions.UID != nil {
		c.precondition = *deleteOptions.Preconditions.UID
	}
	if deleteOptions.GracePeriodSeconds != nil {
		value := *deleteOptions.GracePeriodSeconds
		c.grace = &value
	}
	if sandbox, ok := object.(*unstructured.Unstructured); ok && sandbox.GetKind() == smSandboxGVK.Kind {
		if deleteOptions.PropagationPolicy != nil {
			value := *deleteOptions.PropagationPolicy
			c.sandboxPropagation = &value
		}
	}
	if pod, ok := object.(*corev1.Pod); ok {
		if deleteOptions.Preconditions != nil && deleteOptions.Preconditions.UID != nil {
			c.podPrecondition = *deleteOptions.Preconditions.UID
		}
		if deleteOptions.GracePeriodSeconds != nil {
			value := *deleteOptions.GracePeriodSeconds
			c.podGrace = &value
		}
		if c.failPodName == pod.Name {
			return apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, pod.Name, errors.New("injected Pod deletion failure"))
		}
		if c.replacePod == pod.Name && !c.replaced {
			c.replaced = true
			var existing corev1.Pod
			if err := c.Client.Get(ctx, client.ObjectKeyFromObject(pod), &existing); err != nil {
				return err
			}
			if err := c.Client.Delete(ctx, &existing); err != nil {
				return err
			}
			replacement := c.replacement.DeepCopy()
			if err := c.Client.Create(ctx, replacement); err != nil {
				return err
			}
			if c.precondition != existing.UID {
				return fmt.Errorf("deletion did not carry the old Pod UID precondition: got %q, want %q", c.precondition, existing.UID)
			}
			return apierrors.NewConflict(schema.GroupResource{Resource: "pods"}, pod.Name, errors.New("UID precondition rejected replacement"))
		}
	}
	if account, ok := object.(*corev1.ServiceAccount); ok && c.holdSAName == account.Name {
		return nil
	}
	return c.Client.Delete(ctx, object, options...)
}

type suspensionListErrorReader struct {
	client.Reader
	listErrors      map[string]error
	listErrorsByGVK map[schema.GroupVersionKind]error
	getErrors       map[string]error
	existingCRDs    map[string]bool
}

func (r *suspensionListErrorReader) Get(ctx context.Context, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
	if err, ok := r.getErrors[key.Name]; ok {
		return err
	}
	if r.existingCRDs[key.Name] {
		return nil
	}
	return r.Reader.Get(ctx, key, object, options...)
}

func (r *suspensionListErrorReader) List(ctx context.Context, list client.ObjectList, options ...client.ListOption) error {
	if err, ok := r.listErrorsByGVK[list.GetObjectKind().GroupVersionKind()]; ok {
		return err
	}
	if err := r.listErrors[fmt.Sprintf("%T", list)]; err != nil {
		return err
	}
	return r.Reader.List(ctx, list, options...)
}

func suspensionOptionalListNoMatchErrors() map[schema.GroupVersionKind]error {
	claimList := smClaimGVK.GroupVersion().WithKind(smClaimGVK.Kind + "List")
	sandboxList := smSandboxGVK.GroupVersion().WithKind(smSandboxGVK.Kind + "List")
	warmPoolList := smWarmPoolGVK.GroupVersion().WithKind(smWarmPoolGVK.Kind + "List")
	return map[schema.GroupVersionKind]error{
		claimList: &meta.NoKindMatchError{
			GroupKind:        schema.GroupKind{Group: claimList.Group, Kind: claimList.Kind},
			SearchedVersions: []string{claimList.Version},
		},
		sandboxList: &meta.NoResourceMatchError{
			PartialResource: schema.GroupVersionResource{
				Group: sandboxList.Group, Version: sandboxList.Version, Resource: "sandboxes",
			},
		},
		warmPoolList: &meta.NoKindMatchError{
			GroupKind:        schema.GroupKind{Group: warmPoolList.Group, Kind: warmPoolList.Kind},
			SearchedVersions: []string{warmPoolList.Version},
		},
	}
}
func TestSuspensionDoesNotConfirmWriterOnlyDeletion(t *testing.T) {
	run := testSMRun()
	pod := suspensionManagedPod("held-analysis", "held-pod-uid", run.UID, "analysis", corev1.PodRunning)
	writerState := fake.NewClientBuilder().WithScheme(suspensionTestScheme()).WithObjects(run, pod).Build()
	readerState := fake.NewClientBuilder().WithScheme(suspensionTestScheme()).WithObjects(run.DeepCopy(), pod.DeepCopy()).Build()
	writer := &podDeleteOptionsClient{Client: writerState}
	cache := testCache(t, "bare-pod")
	cache.ForceBareMode = true
	manager := NewSandboxManager(writer, readerState, cache, "test-ns", nil)

	got, err := manager.CleanupSuspension(context.Background(), run.UID)
	if err != nil {
		t.Fatalf("CleanupSuspension() error = %v", err)
	}
	if got.Confirmed() {
		t.Fatal("reported removal while the authoritative reader retained the active Pod")
	}
	if len(got.Pending) == 0 {
		t.Fatalf("Pending = %v, want retained active Pod", got.Pending)
	}
	if len(writer.podDeletes) != 1 || writer.podDeletes[0].grace == nil || *writer.podDeletes[0].grace != 0 {
		t.Fatalf("suspension Pod delete grace = %#v, want zero", writer.podDeletes)
	}
	var retained corev1.Pod
	if err := readerState.Get(context.Background(), client.ObjectKeyFromObject(pod), &retained); err != nil {
		t.Fatalf("authoritative Pod fixture disappeared: %v", err)
	}
}

func TestCleanupSuspensionRemovesManagedAccessAndPreservesUnrelatedObjects(t *testing.T) {
	const runA types.UID = "uid-suspension-run-a"
	const runB types.UID = "uid-suspension-run-b"
	scheme := suspensionTestScheme()
	managedPod := suspensionManagedPod("managed-analysis", "pod-a", runA, "analysis", corev1.PodRunning)
	completedPod := suspensionManagedPod("completed-analysis", "pod-complete", runA, "analysis", corev1.PodSucceeded)
	otherRunPod := suspensionManagedPod("other-run-analysis", "pod-b", runB, "analysis", corev1.PodRunning)
	unrelatedPod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "unrelated", Namespace: "test-ns", UID: "pod-unrelated"}, Status: corev1.PodStatus{Phase: corev1.PodRunning}}
	standingSA := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: defaultSandboxSA, Namespace: "test-ns", UID: "sa-standing"}}
	managedSA := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "analysis-sa-a", Namespace: "test-ns", UID: "sa-a", Labels: rbacLabels(string(runA), "analysis-sa")}}
	otherRunSA := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "analysis-sa-b", Namespace: "test-ns", UID: "sa-b", Labels: rbacLabels(string(runB), "analysis-sa")}}
	managedSecret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name: sandboxKubeconfigSecretName(string(runA), "analysis"), Namespace: "test-ns", UID: "secret-a",
		Labels: rbacLabels(string(runA), "sandbox-kubeconfig"),
	}}
	otherRunSecret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name: sandboxKubeconfigSecretName(string(runB), "analysis"), Namespace: "test-ns", UID: "secret-b",
		Labels: rbacLabels(string(runB), "sandbox-kubeconfig"),
	}}
	managedReaderBinding := &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{
		Name: "temporary-reader-a", UID: "crb-reader-a",
		Labels: map[string]string{LabelRun: string(runA), LabelStep: "analysis", LabelComponent: "reader-rbac"},
	}}
	sourceReaderBinding := testReaderCRB()
	managedExecutionRole := &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{
		Name: executionRoleName(string(runA)), Namespace: "target-ns", UID: "role-exec-a",
		Labels: rbacLabels(string(runA), "execution-rbac"),
	}}
	managedExecutionRoleBinding := &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{
		Name: executionRoleName(string(runA)), Namespace: "target-ns", UID: "rb-exec-a",
		Labels: rbacLabels(string(runA), "execution-rbac"),
	}}
	managedResultRole := &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{
		Name: resultRoleName(string(runA), "analysis"), Namespace: "test-ns", UID: "role-result-a",
		Labels: rbacLabels(string(runA), "result-rbac"),
	}}
	managedResultRoleBinding := &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{
		Name: resultRoleName(string(runA), "analysis"), Namespace: "test-ns", UID: "rb-result-a",
		Labels: rbacLabels(string(runA), "result-rbac"),
	}}
	managedClusterRole := &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{
		Name: clusterRoleName(string(runA)), UID: "cluster-role-a", Labels: rbacLabels(string(runA), "execution-rbac"),
	}}
	managedClusterRoleBinding := &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{
		Name: clusterRoleName(string(runA)), UID: "cluster-binding-a", Labels: rbacLabels(string(runA), "execution-rbac"),
	}}
	upstreamSandbox := suspensionTestObject(smSandboxGVK, "upstream-sandbox", "test-ns", "sandbox-upstream", nil)
	templateGVK := schema.GroupVersionKind{Group: "extensions.agents.x-k8s.io", Version: "v1beta1", Kind: "SandboxTemplate"}
	sharedTemplate := suspensionTestObject(templateGVK, "shared-template", "test-ns", "template-shared", map[string]string{LabelRun: string(runA), LabelStep: "analysis"})
	input := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "input-a", Namespace: "test-ns", UID: "cm-a", Labels: map[string]string{LabelRun: string(runA), LabelStep: "analysis"}}}
	resultCR := &agenticv1alpha1.AnalysisResult{ObjectMeta: metav1.ObjectMeta{Name: "result-a", Namespace: "test-ns", UID: "result-cr-a", Labels: map[string]string{LabelRun: string(runA), LabelStep: "analysis"}}}

	objects := []client.Object{
		managedPod, completedPod, otherRunPod, unrelatedPod,
		standingSA, managedSA, otherRunSA, managedSecret, otherRunSecret,
		managedReaderBinding, sourceReaderBinding,
		managedExecutionRole, managedExecutionRoleBinding, managedResultRole, managedResultRoleBinding,
		managedClusterRole, managedClusterRoleBinding,
		upstreamSandbox, sharedTemplate, input, resultCR,
	}
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
	manager := NewSandboxManager(fc, fc, testCache(t, "bare-pod"), "test-ns", nil)

	got, err := manager.CleanupSuspension(context.Background(), runA)
	if err != nil {
		t.Fatalf("CleanupSuspension() error = %v", err)
	}
	if !got.Confirmed() {
		t.Fatalf("cleanup not confirmed: pending=%v failures=%v unverified=%v", got.Pending, got.Failures, got.Unverified)
	}
	for _, tc := range []struct {
		name string
		obj  client.Object
	}{
		{name: "managed Pod", obj: managedPod},
		{name: "managed ServiceAccount", obj: managedSA},
		{name: "managed Secret", obj: managedSecret},
		{name: "temporary reader binding", obj: managedReaderBinding},
		{name: "execution Role", obj: managedExecutionRole},
		{name: "execution RoleBinding", obj: managedExecutionRoleBinding},
		{name: "result Role", obj: managedResultRole},
		{name: "result RoleBinding", obj: managedResultRoleBinding},
		{name: "execution ClusterRole", obj: managedClusterRole},
		{name: "execution ClusterRoleBinding", obj: managedClusterRoleBinding},
	} {
		if err := fc.Get(context.Background(), client.ObjectKeyFromObject(tc.obj), tc.obj.DeepCopyObject().(client.Object)); !apierrors.IsNotFound(err) {
			t.Errorf("%s remains or read failed: %v", tc.name, err)
		}
	}
	for _, tc := range []struct {
		name string
		obj  client.Object
	}{
		{name: "completed Pod", obj: completedPod},
		{name: "other run Pod", obj: otherRunPod},
		{name: "unrelated Pod", obj: unrelatedPod},
		{name: "standing ServiceAccount", obj: standingSA},
		{name: "other run ServiceAccount", obj: otherRunSA},
		{name: "other run Secret", obj: otherRunSecret},
		{name: "source reader binding", obj: sourceReaderBinding},
		{name: "upstream Sandbox", obj: upstreamSandbox},
		{name: "shared template", obj: sharedTemplate},
		{name: "input ConfigMap", obj: input},
		{name: "Result CR", obj: resultCR},
	} {
		if err := fc.Get(context.Background(), client.ObjectKeyFromObject(tc.obj), tc.obj.DeepCopyObject().(client.Object)); err != nil {
			t.Errorf("%s was not preserved: %v", tc.name, err)
		}
	}
}

func TestCleanupSuspensionEmptyUIDSweepsEveryManagedRun(t *testing.T) {
	scheme := suspensionTestScheme()
	podA := suspensionManagedPod("run-a-pod", "pod-a", "uid-run-a", "analysis", corev1.PodRunning)
	podB := suspensionManagedPod("run-b-pod", "pod-b", "uid-run-b", "execution", corev1.PodRunning)
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(podA, podB).Build()
	manager := NewSandboxManager(fc, fc, testCache(t, "bare-pod"), "test-ns", nil)

	got, err := manager.CleanupSuspension(context.Background(), "")
	if err != nil {
		t.Fatalf("CleanupSuspension() error = %v", err)
	}
	if !got.Confirmed() {
		t.Fatalf("global cleanup not confirmed: pending=%v failures=%v unverified=%v", got.Pending, got.Failures, got.Unverified)
	}
	for _, pod := range []*corev1.Pod{podA, podB} {
		var retained corev1.Pod
		if err := fc.Get(context.Background(), client.ObjectKeyFromObject(pod), &retained); !apierrors.IsNotFound(err) {
			t.Errorf("managed Pod %s remains or read failed: %v", pod.Name, err)
		}
	}
}

func TestCleanupSuspensionPreservesClaimDiscoveryUntilBackingPodAbsent(t *testing.T) {
	const runUID types.UID = "uid-claim-run"
	const claimUID types.UID = "claim-object-uid"
	const sandboxUID types.UID = "sandbox-object-uid"
	const podUID types.UID = "pod-object-uid"
	scheme := suspensionTestScheme()
	claim := suspensionTestObject(smClaimGVK, "claim-analysis", "test-ns", claimUID,
		map[string]string{LabelRun: string(runUID), LabelStep: "analysis"})
	claim.SetOwnerReferences([]metav1.OwnerReference{
		agenticRunOwnerRef(&agenticv1alpha1.AgenticRun{ObjectMeta: metav1.ObjectMeta{Name: "run", UID: runUID}}),
	})
	if err := unstructured.SetNestedField(claim.Object, "sandbox-analysis", "status", "sandbox", "name"); err != nil {
		t.Fatal(err)
	}
	sandbox := suspensionTestObject(smSandboxGVK, "sandbox-analysis", "test-ns", sandboxUID, nil)
	sandbox.SetOwnerReferences([]metav1.OwnerReference{suspensionOwnerRef(smClaimGVK, claim.GetName(), claimUID)})
	sandbox.SetAnnotations(map[string]string{sandboxPodNameAnnotation: "backing-analysis"})
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "backing-analysis", Namespace: "test-ns", UID: podUID, ResourceVersion: "1",
		OwnerReferences: []metav1.OwnerReference{suspensionOwnerRef(smSandboxGVK, sandbox.GetName(), sandboxUID)},
	}, Status: corev1.PodStatus{Phase: corev1.PodRunning}}
	pool := suspensionTestObject(smWarmPoolGVK, "pool-analysis", "test-ns", "pool-object-uid",
		map[string]string{LabelRun: string(runUID), LabelStep: "analysis"})
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(claim, sandbox, pod, pool).Build()
	faultClient := &suspensionFaultClient{Client: fc, failPodName: pod.Name}
	manager := NewSandboxManager(faultClient, fc, testCache(t, "sandbox-claim"), "test-ns", nil)

	first, err := manager.CleanupSuspension(context.Background(), runUID)
	if err == nil {
		t.Fatal("expected injected backing Pod deletion failure")
	}
	if first.Confirmed() {
		t.Fatal("reported confirmation after backing Pod deletion failed")
	}
	if faultClient.podPrecondition != pod.UID || faultClient.podGrace == nil || *faultClient.podGrace != 0 {
		t.Fatalf("backing Pod delete options = UID %q grace %v, want UID %q and zero grace", faultClient.podPrecondition, faultClient.podGrace, pod.UID)
	}
	var retainedClaim unstructured.Unstructured
	retainedClaim.SetGroupVersionKind(smClaimGVK)
	if err := fc.Get(context.Background(), client.ObjectKeyFromObject(claim), &retainedClaim); err != nil {
		t.Fatalf("claim discovery path was removed before Pod absence: %v", err)
	}
	var retainedSandbox unstructured.Unstructured
	retainedSandbox.SetGroupVersionKind(smSandboxGVK)
	if err := fc.Get(context.Background(), client.ObjectKeyFromObject(sandbox), &retainedSandbox); err != nil {
		t.Fatalf("Sandbox discovery path was removed before Pod absence: %v", err)
	}
	if retainedSandbox.GetLabels()[LabelRun] != string(runUID) || retainedSandbox.GetLabels()[LabelStep] != "analysis" {
		t.Fatalf("Sandbox identity was not persisted: %v", retainedSandbox.GetLabels())
	}
	var retainedPod corev1.Pod
	if err := fc.Get(context.Background(), client.ObjectKeyFromObject(pod), &retainedPod); err != nil {
		t.Fatalf("Pod unexpectedly disappeared: %v", err)
	}
	if !suspensionPodLabelsMatch(&retainedPod, runUID, "analysis") {
		t.Fatalf("Pod identity was not persisted: %v", retainedPod.Labels)
	}
	var deletedPool unstructured.Unstructured
	deletedPool.SetGroupVersionKind(smWarmPoolGVK)
	if err := fc.Get(context.Background(), client.ObjectKeyFromObject(pool), &deletedPool); !apierrors.IsNotFound(err) {
		t.Fatalf("independent creation source was not removed: %v", err)
	}

	manager = NewSandboxManager(fc, fc, testCache(t, "sandbox-claim"), "test-ns", nil)
	second, err := manager.CleanupSuspension(context.Background(), runUID)
	if err != nil {
		t.Fatalf("second CleanupSuspension() error = %v", err)
	}
	if !second.Confirmed() {
		t.Fatalf("retry did not confirm cleanup: pending=%v failures=%v unverified=%v", second.Pending, second.Failures, second.Unverified)
	}
	for _, tc := range []struct {
		name string
		obj  client.Object
	}{
		{name: "Pod", obj: pod},
		{name: "Sandbox", obj: sandbox},
		{name: "SandboxClaim", obj: claim},
	} {
		check := tc.obj.DeepCopyObject().(client.Object)
		if err := fc.Get(context.Background(), client.ObjectKeyFromObject(tc.obj), check); !apierrors.IsNotFound(err) {
			t.Errorf("%s remains after confirmed cleanup or read failed: %v", tc.name, err)
		}
	}

	latePod := suspensionManagedPod("late-after-claim", "late-pod-uid", runUID, "analysis", corev1.PodRunning)
	latePod.ResourceVersion = ""
	if err := fc.Create(context.Background(), latePod); err != nil {
		t.Fatalf("create late replacement Pod after claim deletion: %v", err)
	}
	third, err := manager.CleanupSuspension(context.Background(), runUID)
	if err != nil {
		t.Fatalf("late replacement cleanup error = %v", err)
	}
	if !third.Confirmed() {
		t.Fatalf("late managed Pod was not discovered after claim deletion: %+v", third)
	}
	var retainedLatePod corev1.Pod
	if err := fc.Get(context.Background(), client.ObjectKeyFromObject(latePod), &retainedLatePod); !apierrors.IsNotFound(err) {
		t.Fatalf("late managed Pod remains or read failed: %v", err)
	}
}

func TestCleanupSuspensionRetainsClaimWhenReferencedSandboxIsMissing(t *testing.T) {
	const runUID types.UID = "uid-missing-sandbox-run"
	const claimUID types.UID = "missing-sandbox-claim-uid"
	const staleSandboxUID types.UID = "missing-sandbox-stale-owner-uid"
	claim := suspensionTestObject(smClaimGVK, "managed-claim", "test-ns", claimUID,
		map[string]string{LabelRun: string(runUID), LabelStep: "analysis"})
	claim.SetAnnotations(map[string]string{AnnotationRunName: "managed-run"})
	claim.SetOwnerReferences([]metav1.OwnerReference{
		agenticRunOwnerRef(&agenticv1alpha1.AgenticRun{ObjectMeta: metav1.ObjectMeta{Name: "managed-run", UID: runUID}}),
	})
	if err := unstructured.SetNestedField(claim.Object, "missing-sandbox", "status", "sandbox", "name"); err != nil {
		t.Fatal(err)
	}
	activePod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "orphaned-active-pod",
			Namespace:       "test-ns",
			UID:             "orphaned-active-pod-uid",
			ResourceVersion: "1",
			OwnerReferences: []metav1.OwnerReference{
				suspensionOwnerRef(smSandboxGVK, "missing-sandbox", staleSandboxUID),
			},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	fc := fake.NewClientBuilder().WithScheme(suspensionTestScheme()).WithObjects(claim, activePod).Build()
	manager := NewSandboxManager(fc, fc, testCache(t, "sandbox-claim"), "test-ns", nil)

	report, err := manager.CleanupSuspension(context.Background(), runUID)
	if report.Confirmed() || len(report.Unverified) == 0 {
		t.Fatalf("missing Sandbox was treated as confirmed absence: report=%+v err=%v", report, err)
	}
	var retainedClaim unstructured.Unstructured
	retainedClaim.SetGroupVersionKind(smClaimGVK)
	if err := fc.Get(context.Background(), client.ObjectKeyFromObject(claim), &retainedClaim); err != nil {
		t.Fatalf("managed claim was deleted without backing-Sandbox confirmation: %v", err)
	}
	var retainedPod corev1.Pod
	if err := fc.Get(context.Background(), client.ObjectKeyFromObject(activePod), &retainedPod); err != nil {
		t.Fatalf("active unlabeled Pod disappeared from the fake inventory: %v", err)
	}
	if retainedPod.Status.Phase != corev1.PodRunning {
		t.Fatalf("orphaned Pod phase = %s, want Running", retainedPod.Status.Phase)
	}
}

func TestCleanupSuspensionDiscoversUnlabeledSandboxFromManagedPodOwner(t *testing.T) {
	const runUID types.UID = "uid-owner-discovery-run"
	const sandboxUID types.UID = "owner-discovery-sandbox-uid"
	const podUID types.UID = "owner-discovery-pod-uid"
	sandbox := suspensionTestObject(smSandboxGVK, "owner-discovery-sandbox", "test-ns", sandboxUID, nil)
	sandbox.SetAnnotations(map[string]string{sandboxPodNameAnnotation: "owner-discovery-pod"})
	pod := suspensionManagedPod("owner-discovery-pod", podUID, runUID, "analysis", corev1.PodRunning)
	pod.OwnerReferences = []metav1.OwnerReference{suspensionOwnerRef(smSandboxGVK, sandbox.GetName(), sandboxUID)}
	fc := fake.NewClientBuilder().WithScheme(suspensionTestScheme()).WithObjects(sandbox, pod).Build()
	manager := NewSandboxManager(fc, fc, testCache(t, "sandbox-claim"), "test-ns", nil)

	got, err := manager.CleanupSuspension(context.Background(), runUID)
	if err != nil {
		t.Fatalf("CleanupSuspension() error = %v", err)
	}
	if !got.Confirmed() {
		t.Fatalf("validated Pod owner did not lead to Sandbox cleanup: %+v", got)
	}
	for _, object := range []client.Object{pod, sandbox} {
		check := object.DeepCopyObject().(client.Object)
		if err := fc.Get(context.Background(), client.ObjectKeyFromObject(object), check); !apierrors.IsNotFound(err) {
			t.Errorf("managed descendant %s remains or read failed: %v", object.GetName(), err)
		}
	}
}

func TestCleanupSuspensionPreservesCompletedClaimPod(t *testing.T) {
	const runUID types.UID = "uid-completed-claim-run"
	const claimUID types.UID = "completed-claim-uid"
	const sandboxUID types.UID = "completed-sandbox-uid"
	const podUID types.UID = "completed-pod-uid"
	claim := suspensionTestObject(smClaimGVK, "completed-claim", "test-ns", claimUID,
		map[string]string{LabelRun: string(runUID), LabelStep: "analysis"})
	if err := unstructured.SetNestedField(claim.Object, "completed-sandbox", "status", "sandbox", "name"); err != nil {
		t.Fatal(err)
	}
	sandbox := suspensionTestObject(smSandboxGVK, "completed-sandbox", "test-ns", sandboxUID, nil)
	sandbox.SetOwnerReferences([]metav1.OwnerReference{suspensionOwnerRef(smClaimGVK, claim.GetName(), claimUID)})
	sandbox.SetAnnotations(map[string]string{sandboxPodNameAnnotation: "completed-pod"})
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "completed-pod", Namespace: "test-ns", UID: podUID, ResourceVersion: "1",
			OwnerReferences: []metav1.OwnerReference{suspensionOwnerRef(smSandboxGVK, sandbox.GetName(), sandboxUID)},
		},
		Status: corev1.PodStatus{Phase: corev1.PodSucceeded},
	}
	fc := fake.NewClientBuilder().WithScheme(suspensionTestScheme()).WithObjects(claim, sandbox, pod).Build()
	writer := &suspensionFaultClient{Client: fc}
	manager := NewSandboxManager(writer, fc, testCache(t, "sandbox-claim"), "test-ns", nil)

	got, err := manager.CleanupSuspension(context.Background(), runUID)
	if err != nil {
		t.Fatalf("CleanupSuspension() error = %v", err)
	}
	if !got.Confirmed() {
		t.Fatalf("completed run cleanup not confirmed: %+v", got)
	}
	if writer.sandboxPropagation == nil || *writer.sandboxPropagation != metav1.DeletePropagationOrphan {
		t.Fatalf("completed Pod owner deletion policy = %v, want orphan", writer.sandboxPropagation)
	}
	var retained corev1.Pod
	if err := fc.Get(context.Background(), client.ObjectKeyFromObject(pod), &retained); err != nil {
		t.Fatalf("completed Pod was not preserved: %v", err)
	}
	if retained.UID != podUID || retained.Status.Phase != corev1.PodSucceeded ||
		!suspensionPodLabelsMatch(&retained, runUID, "analysis") {
		t.Fatalf("completed Pod changed unexpectedly: uid=%s phase=%s labels=%v", retained.UID, retained.Status.Phase, retained.Labels)
	}
}

func TestCleanupSuspensionRetainedAccessKeepsConfirmationFalse(t *testing.T) {
	const runUID types.UID = "uid-held-sa-run"
	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{
		Name: "held-analysis", Namespace: "test-ns", UID: "held-sa-uid",
		Labels: rbacLabels(string(runUID), "analysis-sa"),
	}}
	fc := fake.NewClientBuilder().WithScheme(suspensionTestScheme()).WithObjects(sa).Build()
	holdingClient := &suspensionFaultClient{Client: fc, holdSAName: sa.Name}
	manager := NewSandboxManager(holdingClient, fc, testCache(t, "bare-pod"), "test-ns", nil)

	got, err := manager.CleanupSuspension(context.Background(), runUID)
	if err != nil {
		t.Fatalf("CleanupSuspension() error = %v", err)
	}
	if got.Confirmed() || len(got.Pending) == 0 {
		t.Fatalf("retained temporary access was confirmed: %+v", got)
	}
	var retained corev1.ServiceAccount
	if err := fc.Get(context.Background(), client.ObjectKeyFromObject(sa), &retained); err != nil {
		t.Fatalf("held ServiceAccount fixture disappeared: %v", err)
	}
}

func TestCleanupSuspensionContinuesAfterForbiddenInventory(t *testing.T) {
	const runUID types.UID = "uid-forbidden-run"
	pod := suspensionManagedPod("pod-before-forbidden", "pod-forbidden", runUID, "analysis", corev1.PodRunning)
	role := &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{
		Name: "execution-grant", Namespace: "target-ns", UID: "role-forbidden",
		Labels: rbacLabels(string(runUID), "execution-rbac"),
	}}
	fc := fake.NewClientBuilder().WithScheme(suspensionTestScheme()).WithObjects(pod, role).Build()
	reader := &suspensionListErrorReader{
		Reader: fc,
		listErrors: map[string]error{
			"*v1.RoleList": apierrors.NewForbidden(schema.GroupResource{Group: rbacv1.GroupName, Resource: "roles"}, "", errors.New("injected Role inventory denial")),
		},
	}
	manager := NewSandboxManager(fc, reader, testCache(t, "bare-pod"), "test-ns", nil)

	got, err := manager.CleanupSuspension(context.Background(), runUID)
	if err == nil {
		t.Fatal("expected Forbidden Role inventory to be returned")
	}
	if got.Confirmed() || len(got.Unverified) == 0 {
		t.Fatalf("Forbidden inventory was treated as confirmation: %+v", got)
	}
	var removed corev1.Pod
	if err := fc.Get(context.Background(), client.ObjectKeyFromObject(pod), &removed); !apierrors.IsNotFound(err) {
		t.Fatalf("independent Pod cleanup did not continue: %v", err)
	}
	var retained rbacv1.Role
	if err := fc.Get(context.Background(), client.ObjectKeyFromObject(role), &retained); err != nil {
		t.Fatalf("unreadable Role fixture unexpectedly disappeared: %v", err)
	}
}

func TestCleanupSuspensionDoesNotTreatOptionalAPIForbiddenAsBareMode(t *testing.T) {
	const runUID types.UID = "uid-optional-forbidden"
	fc := fake.NewClientBuilder().WithScheme(suspensionTestScheme()).Build()
	listErrors := suspensionOptionalListNoMatchErrors()
	claimListGVK := smClaimGVK.GroupVersion().WithKind(smClaimGVK.Kind + "List")
	listErrors[claimListGVK] = apierrors.NewForbidden(schema.GroupResource{Group: smClaimGVK.Group, Resource: "sandboxclaims"}, "", errors.New("injected claim inventory denial"))
	reader := &suspensionListErrorReader{
		Reader:          fc,
		listErrorsByGVK: listErrors,
	}
	cache := testCache(t, "bare-pod")
	cache.ForceBareMode = true
	manager := NewSandboxManager(fc, reader, cache, "test-ns", nil)

	got, err := manager.CleanupSuspension(context.Background(), runUID)
	if err == nil || got.Confirmed() || !suspensionReportMentions(got, "SandboxClaim inventory") {
		t.Fatalf("Forbidden optional API inventory was not identified as unverified: result=%+v err=%v", got, err)
	}
}

func TestCleanupSuspensionProtectsSameNamePodReplacementWithUIDPrecondition(t *testing.T) {
	const runUID types.UID = "uid-replacement-run"
	pod := suspensionManagedPod("replace-me", "old-pod-uid", runUID, "analysis", corev1.PodRunning)
	replacement := suspensionManagedPod("replace-me", "replacement-pod-uid", runUID, "analysis", corev1.PodRunning)
	replacement.ResourceVersion = ""
	fc := fake.NewClientBuilder().WithScheme(suspensionTestScheme()).WithObjects(pod).Build()
	writer := &suspensionFaultClient{Client: fc, replacePod: pod.Name, replacement: replacement}
	manager := NewSandboxManager(writer, fc, testCache(t, "bare-pod"), "test-ns", nil)

	got, err := manager.CleanupSuspension(context.Background(), runUID)
	if err == nil {
		t.Fatal("expected the replacement to reject the old UID-preconditioned delete")
	}
	if got.Confirmed() || len(got.Failures) == 0 {
		t.Fatalf("replacement Pod was treated as removed: %+v", got)
	}
	if writer.precondition != pod.UID || writer.grace == nil || *writer.grace != 0 {
		t.Fatalf("Pod delete options = UID %q grace %v, want UID %q and zero grace", writer.precondition, writer.grace, pod.UID)
	}
	var retained corev1.Pod
	if err := fc.Get(context.Background(), client.ObjectKeyFromObject(replacement), &retained); err != nil {
		t.Fatalf("same-name replacement was deleted: %v", err)
	}
	if retained.UID != replacement.UID {
		t.Fatalf("replacement UID = %q, want %q", retained.UID, replacement.UID)
	}
}

func TestCleanupSuspensionRejectsUnverifiedReader(t *testing.T) {
	manager := NewSandboxManager(fake.NewClientBuilder().WithScheme(testScheme()).Build(), nil, testCache(t, "bare-pod"), "test-ns", nil)
	got, err := manager.CleanupSuspension(context.Background(), "uid-without-reader")
	if err == nil || got.Confirmed() || len(got.Unverified) == 0 {
		t.Fatalf("missing uncached reader was treated as success: result=%+v err=%v", got, err)
	}
}
