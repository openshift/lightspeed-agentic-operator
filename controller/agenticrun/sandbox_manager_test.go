package agenticrun

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
	"github.com/openshift/lightspeed-agentic-operator/pkg/configuration"
)

func testCache(t *testing.T, mode string) *configuration.Cache {
	t.Helper()
	return testCacheWithOTEL(t, mode, "", "", "")
}

func testCacheWithOTEL(t *testing.T, mode, otelEndpoint, otelAdmin, otelCA string) *configuration.Cache {
	t.Helper()
	c := &configuration.Cache{}
	data := map[string]string{
		configuration.KeySandboxMode:    mode,
		configuration.KeySandboxPodSpec: `{"containers":[{"name":"agent","image":"registry.example.com/agent:latest","ports":[{"containerPort":8080}]}]}`,
	}
	if otelEndpoint != "" {
		data[configuration.KeyOtelCollectorEndpoint] = otelEndpoint
	}
	if otelAdmin != "" {
		data[configuration.KeyOtelAdminEndpoint] = otelAdmin
	}
	if otelCA != "" {
		data[configuration.KeyOtelCASecret] = otelCA
	}
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: configuration.ConfigMapName},
		Data:       data,
	}
	if err := c.OnConfigMapChange(context.Background(), cm); err != nil {
		t.Fatalf("testCacheWithOTEL: OnConfigMapChange failed: %v", err)
	}
	return c
}

func testSMRun() *agenticv1alpha1.AgenticRun {
	return &agenticv1alpha1.AgenticRun{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-run",
			Namespace: "test-ns",
			UID:       types.UID("abc-123"),
		},
		Spec: agenticv1alpha1.AgenticRunSpec{
			Request: "fix it",
		},
	}
}

func testSMAgent() *agenticv1alpha1.Agent {
	return &agenticv1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "default"},
		Spec: agenticv1alpha1.AgentSpec{
			LLMProvider: agenticv1alpha1.LLMProviderReference{Name: "smart"},
			Model:       "test-model",
		},
	}
}

func testLLMForManager() *agenticv1alpha1.LLMProvider {
	return &agenticv1alpha1.LLMProvider{
		ObjectMeta: metav1.ObjectMeta{Name: "smart"},
		Spec: agenticv1alpha1.LLMProviderSpec{
			Type: agenticv1alpha1.LLMProviderOpenAI,
			OpenAI: agenticv1alpha1.OpenAIConfig{
				CredentialsSecret: agenticv1alpha1.SecretReference{Name: "llm-creds"},
			},
		},
	}
}

func testReaderCRB() *rbacv1.ClusterRoleBinding {
	return &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: defaultReaderClusterRoleBinding},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "cluster-reader"},
		Subjects: []rbacv1.Subject{{
			Kind:      rbacv1.ServiceAccountKind,
			Name:      defaultSandboxSA,
			Namespace: "test-ns",
		}},
	}
}

func newTestSandboxManager(fc client.Client, cache *configuration.Cache) *SandboxManager {
	resetReaderBindings()
	return &SandboxManager{
		client:          fc,
		config:          cache,
		builder:         &PodSpecBuilder{},
		namespace:       "test-ns",
		deletionTimeout: 1 * time.Second,
	}
}

// --- Create tests ---

func TestCreate_BarePod(t *testing.T) {
	cache := testCache(t, "bare-pod")
	fc := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(testReaderCRB()).Build()
	mgr := newTestSandboxManager(fc, cache)

	name, err := mgr.Create(context.Background(), testSMRun(), "analysis", testSMAgent(), testLLMForManager(), nil, 15*time.Minute, nil)
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}
	if name == "" {
		t.Fatal("expected non-empty name")
	}
	if name[0:3] != "ls-" {
		t.Fatalf("bare-pod name should start with 'ls-', got %q", name)
	}

	var pod corev1.Pod
	if err := fc.Get(context.Background(), types.NamespacedName{Name: name, Namespace: "test-ns"}, &pod); err != nil {
		t.Fatalf("pod not found: %v", err)
	}
	if len(pod.OwnerReferences) == 0 {
		t.Fatal("expected OwnerReferences on pod")
	}
	if pod.OwnerReferences[0].Name != "test-run" {
		t.Fatalf("expected owner name 'test-run', got %q", pod.OwnerReferences[0].Name)
	}
	if pod.Spec.ActiveDeadlineSeconds == nil {
		t.Fatal("expected ActiveDeadlineSeconds to be set on pod")
	}
	if *pod.Spec.ActiveDeadlineSeconds != int64(15*time.Minute/time.Second) {
		t.Fatalf("expected ActiveDeadlineSeconds=%d, got %d", int64(15*time.Minute/time.Second), *pod.Spec.ActiveDeadlineSeconds)
	}

	var cm corev1.ConfigMap
	run := testSMRun()
	if err := fc.Get(context.Background(), types.NamespacedName{Name: inputConfigMapName("analysis", string(run.UID)), Namespace: "test-ns"}, &cm); err != nil {
		t.Fatalf("input ConfigMap not found: %v", err)
	}
	if !strings.Contains(cm.Data[inputConfigMapKeyQuery], "fix it") {
		t.Errorf("ConfigMap query should contain request text, got %q", cm.Data[inputConfigMapKeyQuery])
	}
	if len(cm.OwnerReferences) == 0 {
		t.Fatal("expected OwnerReferences on input ConfigMap")
	}
	if cm.OwnerReferences[0].Kind != "Pod" || cm.OwnerReferences[0].Name != name {
		t.Fatalf("expected ConfigMap owned by Pod %q, got %s/%s", name, cm.OwnerReferences[0].Kind, cm.OwnerReferences[0].Name)
	}
	foundMount := false
	for _, m := range pod.Spec.Containers[0].VolumeMounts {
		if m.Name == inputConfigMapVolumeName && m.MountPath == inputConfigMapMountPath {
			foundMount = true
			break
		}
	}
	if !foundMount {
		t.Error("pod missing /input ConfigMap mount")
	}
}

func TestCreate_SandboxClaim(t *testing.T) {
	cache := testCache(t, "sandbox-claim")
	fc := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(testReaderCRB()).Build()
	mgr := newTestSandboxManager(fc, cache)

	name, err := mgr.Create(context.Background(), testSMRun(), "analysis", testSMAgent(), testLLMForManager(), nil, 15*time.Minute, nil)
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}
	if name[0:3] != "ls-" {
		t.Fatalf("sandbox-claim name should start with 'ls-', got %q", name)
	}

	tmpl := &unstructured.Unstructured{}
	tmpl.SetGroupVersionKind(smClaimGVK)
	// Actually check the template was created
	stmpl := &unstructured.Unstructured{}
	stmpl.SetGroupVersionKind(smClaimGVK)
	stmpl.SetGroupVersionKind(smClaimGVK)
	if err := fc.Get(context.Background(), types.NamespacedName{Name: name, Namespace: "test-ns"}, stmpl); err != nil {
		t.Fatalf("SandboxClaim not found: %v", err)
	}

	ownerRefs, found, _ := unstructured.NestedSlice(stmpl.Object, "metadata", "ownerReferences")
	if !found || len(ownerRefs) == 0 {
		t.Fatal("expected ownerReferences on SandboxClaim")
	}
}

func TestCreate_ConfigNotAvailable(t *testing.T) {
	cache := &configuration.Cache{} // empty — no ConfigMap loaded
	fc := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(testReaderCRB()).Build()
	mgr := newTestSandboxManager(fc, cache)

	_, err := mgr.Create(context.Background(), testSMRun(), "analysis", testSMAgent(), testLLMForManager(), nil, 15*time.Minute, nil)
	if err == nil {
		t.Fatal("expected error when config is not available")
	}
}

func TestCreate_Idempotent_BarePod(t *testing.T) {
	cache := testCache(t, "bare-pod")
	fc := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(testReaderCRB()).Build()
	mgr := newTestSandboxManager(fc, cache)

	name1, err := mgr.Create(context.Background(), testSMRun(), "analysis", testSMAgent(), testLLMForManager(), nil, 15*time.Minute, nil)
	if err != nil {
		t.Fatalf("first Create failed: %v", err)
	}
	name2, err := mgr.Create(context.Background(), testSMRun(), "analysis", testSMAgent(), testLLMForManager(), nil, 15*time.Minute, nil)
	if err != nil {
		t.Fatalf("second Create failed: %v", err)
	}
	if name1 != name2 {
		t.Fatalf("expected same name on idempotent create, got %q and %q", name1, name2)
	}
}

func TestCreate_Idempotent_SandboxClaim(t *testing.T) {
	cache := testCache(t, "sandbox-claim")
	fc := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(testReaderCRB()).Build()
	mgr := newTestSandboxManager(fc, cache)

	name1, err := mgr.Create(context.Background(), testSMRun(), "analysis", testSMAgent(), testLLMForManager(), nil, 15*time.Minute, nil)
	if err != nil {
		t.Fatalf("first Create failed: %v", err)
	}
	name2, err := mgr.Create(context.Background(), testSMRun(), "analysis", testSMAgent(), testLLMForManager(), nil, 15*time.Minute, nil)
	if err != nil {
		t.Fatalf("second Create failed: %v", err)
	}
	if name1 != name2 {
		t.Fatalf("expected same name on idempotent create, got %q and %q", name1, name2)
	}
}

func TestCreate_OTELEnvVars(t *testing.T) {
	cache := testCacheWithOTEL(t, "bare-pod", "dns:///otel-collector.ns.svc:4317", "https://otel-collector.ns.svc:8080", "otel-ca-secret")
	fc := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(testReaderCRB()).Build()
	mgr := newTestSandboxManager(fc, cache)

	run := testSMRun()
	name, err := mgr.Create(context.Background(), run, "analysis", testSMAgent(), testLLMForManager(), nil, 15*time.Minute, nil)
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}

	var pod corev1.Pod
	if err := fc.Get(context.Background(), types.NamespacedName{Name: name, Namespace: "test-ns"}, &pod); err != nil {
		t.Fatalf("pod not found: %v", err)
	}

	envMap := map[string]string{}
	for _, e := range pod.Spec.Containers[0].Env {
		envMap[e.Name] = e.Value
	}

	if v := envMap["OTEL_EXPORTER_OTLP_ENDPOINT"]; v != "dns:///otel-collector.ns.svc:4317" {
		t.Fatalf("expected OTEL endpoint, got %q", v)
	}
	if v := envMap["LIGHTSPEED_AGENTICRUN_UID"]; v != string(run.UID) {
		t.Fatalf("expected run UID %q, got %q", run.UID, v)
	}
	if v := envMap["LIGHTSPEED_AGENTICRUN_STEP"]; v != "analysis" {
		t.Fatalf("expected run step %q, got %q", "analysis", v)
	}
	hasVolume := false
	for _, v := range pod.Spec.Volumes {
		if v.Name == otelCAVolumeName {
			hasVolume = true
			if v.Secret.SecretName != "otel-ca-secret" {
				t.Fatalf("expected otel-ca-secret, got %q", v.Secret.SecretName)
			}
		}
	}
	if !hasVolume {
		t.Fatal("expected otel-ca volume")
	}

	hasMount := false
	for _, m := range pod.Spec.Containers[0].VolumeMounts {
		if m.Name == otelCAVolumeName {
			hasMount = true
			if m.MountPath != otelCAMountPath {
				t.Fatalf("expected mount path %q, got %q", otelCAMountPath, m.MountPath)
			}
		}
	}
	if !hasMount {
		t.Fatal("expected otel-ca volume mount")
	}
}

func TestCreate_NoOTEL_NoEnvVars(t *testing.T) {
	cache := testCache(t, "bare-pod")
	fc := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(testReaderCRB()).Build()
	mgr := newTestSandboxManager(fc, cache)

	name, err := mgr.Create(context.Background(), testSMRun(), "analysis", testSMAgent(), testLLMForManager(), nil, 15*time.Minute, nil)
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}

	var pod corev1.Pod
	if err := fc.Get(context.Background(), types.NamespacedName{Name: name, Namespace: "test-ns"}, &pod); err != nil {
		t.Fatalf("pod not found: %v", err)
	}

	for _, e := range pod.Spec.Containers[0].Env {
		switch e.Name {
		case "OTEL_EXPORTER_OTLP_ENDPOINT", "LIGHTSPEED_AGENTICRUN_UID", "LIGHTSPEED_AGENTICRUN_STEP":
			t.Fatalf("%s should not be present when the endpoint is empty", e.Name)
		}
	}
}

func TestCreate_OTELEnvVars_SandboxClaim(t *testing.T) {
	cache := testCacheWithOTEL(t, "sandbox-claim", "dns:///otel-collector.ns.svc:4317", "https://otel-collector.ns.svc:8080", "otel-ca-secret")
	fc := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(testReaderCRB()).Build()
	mgr := newTestSandboxManager(fc, cache)

	run := testSMRun()
	name, err := mgr.Create(context.Background(), run, "analysis", testSMAgent(), testLLMForManager(), nil, 15*time.Minute, nil)
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}
	if name[0:3] != "ls-" {
		t.Fatalf("expected 'ls-' prefix, got %q", name)
	}

	tmpl := &unstructured.Unstructured{}
	tmpl.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "extensions.agents.x-k8s.io", Version: "v1beta1", Kind: "SandboxTemplate",
	})
	if err := fc.Get(context.Background(), types.NamespacedName{Name: name, Namespace: "test-ns"}, tmpl); err != nil {
		t.Fatalf("SandboxTemplate not found: %v", err)
	}

	containers, found, _ := unstructured.NestedSlice(tmpl.Object, "spec", "podTemplate", "spec", "containers")
	if !found || len(containers) == 0 {
		t.Fatal("expected containers in SandboxTemplate podTemplate spec")
	}
	container := containers[0].(map[string]interface{})
	envList, _, _ := unstructured.NestedSlice(container, "env")

	envMap := map[string]string{}
	for _, e := range envList {
		em := e.(map[string]interface{})
		if n, ok := em["name"].(string); ok {
			if v, ok := em["value"].(string); ok {
				envMap[n] = v
			}
		}
	}

	if v := envMap["OTEL_EXPORTER_OTLP_ENDPOINT"]; v != "dns:///otel-collector.ns.svc:4317" {
		t.Fatalf("expected OTEL endpoint in SandboxTemplate, got %q", v)
	}
	if v := envMap["LIGHTSPEED_AGENTICRUN_UID"]; v != string(run.UID) {
		t.Fatalf("expected run UID %q in SandboxTemplate, got %q", run.UID, v)
	}
	if v := envMap["LIGHTSPEED_AGENTICRUN_STEP"]; v != "analysis" {
		t.Fatalf("expected run step %q in SandboxTemplate, got %q", "analysis", v)
	}
	deadline, found, _ := unstructured.NestedInt64(tmpl.Object, "spec", "podTemplate", "spec", "activeDeadlineSeconds")
	if !found {
		t.Fatal("expected activeDeadlineSeconds in SandboxTemplate podTemplate spec")
	}
	if deadline != int64(15*time.Minute/time.Second) {
		t.Fatalf("expected activeDeadlineSeconds=%d, got %d", int64(15*time.Minute/time.Second), deadline)
	}
}

// --- Release tests ---

func TestRelease_BarePod(t *testing.T) {
	cache := testCache(t, "bare-pod")
	run := testSMRun()
	run.Status.Steps.Analysis.Sandbox.ClaimName = "p-analysis-test-run"
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p-analysis-test-run", Namespace: "test-ns"},
	}
	fc := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(pod).Build()
	mgr := newTestSandboxManager(fc, cache)

	if err := mgr.Release(context.Background(), run, "analysis", nil); err != nil {
		t.Fatalf("Release failed: %v", err)
	}

	var check corev1.Pod
	err := fc.Get(context.Background(), types.NamespacedName{Name: "p-analysis-test-run", Namespace: "test-ns"}, &check)
	if err == nil {
		t.Fatal("expected pod to be deleted")
	}
}

func TestRelease_BarePod_Idempotent(t *testing.T) {
	cache := testCache(t, "bare-pod")
	run := testSMRun()
	fc := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(testReaderCRB()).Build()
	mgr := newTestSandboxManager(fc, cache)

	if err := mgr.Release(context.Background(), run, "analysis", nil); err != nil {
		t.Fatalf("Release of non-existent pod should succeed, got: %v", err)
	}
}

func TestRelease_SandboxClaim_Idempotent(t *testing.T) {
	cache := testCache(t, "sandbox-claim")
	run := testSMRun()
	fc := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(testReaderCRB()).Build()
	mgr := newTestSandboxManager(fc, cache)

	if err := mgr.Release(context.Background(), run, "analysis", nil); err != nil {
		t.Fatalf("Release of non-existent claim should succeed, got: %v", err)
	}
}

// --- Name prefix ---

func TestNamePrefix_LSPrefix(t *testing.T) {
	for _, mode := range []string{"bare-pod", "sandbox-claim", ""} {
		t.Run(mode, func(t *testing.T) {
			cache := testCache(t, mode)
			fc := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(testReaderCRB()).Build()
			mgr := newTestSandboxManager(fc, cache)

			name, err := mgr.Create(context.Background(), testSMRun(), "analysis", testSMAgent(), testLLMForManager(), nil, 15*time.Minute, nil)
			if err != nil {
				t.Fatalf("Create failed: %v", err)
			}
			if name[:3] != "ls-" {
				t.Fatalf("expected 'ls-' prefix, got %q (full name: %q)", name[:3], name)
			}
		})
	}
}

func TestNamePrefix_LongNameTruncated(t *testing.T) {
	cache := testCache(t, "bare-pod")
	fc := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(testReaderCRB()).Build()
	mgr := newTestSandboxManager(fc, cache)

	longRun := testSMRun()
	longRun.Name = "a-very-long-run-name-that-exceeds-sixty-three-characters-in-total-length"

	name, err := mgr.Create(context.Background(), longRun, "analysis", testSMAgent(), testLLMForManager(), nil, 15*time.Minute, nil)
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}
	if len(name) > 63 {
		t.Fatalf("name exceeds 63 chars: len=%d, name=%q", len(name), name)
	}
	if name[:3] != "ls-" {
		t.Fatalf("prefix lost after truncation: %q", name)
	}
}

// --- podSpecToUnstructured ---

func TestPodSpecToUnstructured(t *testing.T) {
	spec := &corev1.PodSpec{
		Containers: []corev1.Container{
			{Name: "agent", Image: "test:latest"},
		},
	}
	result, err := podSpecToUnstructured(spec)
	if err != nil {
		t.Fatalf("podSpecToUnstructured failed: %v", err)
	}
	containers, ok := result["containers"]
	if !ok {
		t.Fatal("expected 'containers' key in result")
	}
	arr, ok := containers.([]any)
	if !ok || len(arr) == 0 {
		t.Fatal("expected non-empty containers array")
	}
}

// --- Spoke tests ---

func testSpokeKubeconfigSecret() *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "spoke-kubeconfig-test-spoke",
			Namespace: "test-ns",
		},
		Data: map[string][]byte{
			"kubeconfig": []byte(`
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
`),
		},
	}
}

func testSpokeRun() *agenticv1alpha1.AgenticRun {
	return &agenticv1alpha1.AgenticRun{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "spoke-run",
			Namespace: "test-ns",
			UID:       types.UID("spoke-uid-123"),
		},
		Spec: agenticv1alpha1.AgenticRunSpec{
			Request:       "fix spoke issue",
			TargetCluster: "test-spoke",
		},
	}
}

// TestCreate_SpokeRun verifies that spoke runs create SA on spoke, request
// a token, use "default" SA for the pod, bind result RBAC to "default",
// and don't suppress automount.
func TestCreate_SpokeRun(t *testing.T) {
	origClient := NewClientFromConfig
	origClientset := NewClientsetFromConfig

	// Spoke fake client with source reader CRBs.
	srcBindings := spokeReaderBindings()
	spokeFC := fake.NewClientBuilder().WithScheme(testScheme()).
		WithObjects(srcBindings[0], srcBindings[1]).Build()
	NewClientFromConfig = func(cfg *rest.Config) (client.Client, error) {
		return spokeFC, nil
	}

	// Fake clientset for TokenRequest.
	cs := k8sfake.NewSimpleClientset()
	cs.PrependReactor("create", "serviceaccounts/token", func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, &authenticationv1.TokenRequest{
			Status: authenticationv1.TokenRequestStatus{Token: "spoke-token-xyz"},
		}, nil
	})
	NewClientsetFromConfig = func(cfg *rest.Config) (kubernetes.Interface, error) {
		return cs, nil
	}
	t.Cleanup(func() {
		NewClientFromConfig = origClient
		NewClientsetFromConfig = origClientset
	})

	cache := testCache(t, "bare-pod")
	hubFC := fake.NewClientBuilder().WithScheme(testScheme()).
		WithObjects(testReaderCRB(), testSpokeKubeconfigSecret()).Build()
	mgr := newTestSandboxManager(hubFC, cache)

	run := testSpokeRun()
	name, err := mgr.Create(context.Background(), run, "analysis", testSMAgent(), testLLMForManager(), nil, 15*time.Minute, nil)
	if err != nil {
		t.Fatalf("Create spoke run failed: %v", err)
	}
	if name == "" {
		t.Fatal("expected non-empty name")
	}

	// SA should exist on spoke.
	saName := sandboxSAName(run, "analysis")
	var sa corev1.ServiceAccount
	if err := spokeFC.Get(context.Background(), types.NamespacedName{Name: saName, Namespace: spokeManagedNamespace}, &sa); err != nil {
		t.Fatalf("spoke SA not found: %v", err)
	}
	// Spoke SA should have spoke labels.
	if sa.Labels[LabelSpokeCluster] == "" {
		t.Error("spoke SA missing spoke-cluster label")
	}

	// Per-run CRBs should exist on spoke.
	for i := range spokeReaderBindingNames {
		crbName := perRunCRBName(string(run.UID), "analysis", i)
		var crb rbacv1.ClusterRoleBinding
		if err := spokeFC.Get(context.Background(), types.NamespacedName{Name: crbName}, &crb); err != nil {
			t.Fatalf("per-run CRB %s not found on spoke: %v", crbName, err)
		}
	}

	// Pod should use "default" SA (not per-step SA).
	var pod corev1.Pod
	if err := hubFC.Get(context.Background(), types.NamespacedName{Name: name, Namespace: "test-ns"}, &pod); err != nil {
		t.Fatalf("pod not found: %v", err)
	}
	if pod.Spec.ServiceAccountName != "default" {
		t.Fatalf("spoke pod SA = %q, want \"default\"", pod.Spec.ServiceAccountName)
	}

	// automountServiceAccountToken should NOT be false — spoke pods need
	// hub API access for writing Result CRs via the default SA.
	if pod.Spec.AutomountServiceAccountToken != nil && !*pod.Spec.AutomountServiceAccountToken {
		t.Fatal("spoke pod should not have automountServiceAccountToken=false")
	}

	// Result RBAC should bind to "default" (podServiceAccount), not the per-step SA.
	roleName := resultRoleName(string(run.UID), "analysis")
	var binding rbacv1.RoleBinding
	if err := hubFC.Get(context.Background(), types.NamespacedName{Name: roleName, Namespace: "test-ns"}, &binding); err != nil {
		t.Fatalf("result RoleBinding not found: %v", err)
	}
	if binding.Subjects[0].Name != "default" {
		t.Fatalf("result RBAC subject = %q, want \"default\"", binding.Subjects[0].Name)
	}

	// Sandbox kubeconfig Secret should exist on hub with owner ref.
	kcSecretName := sandboxKubeconfigSecretName(string(run.UID), "analysis")
	var kcSecret corev1.Secret
	if err := hubFC.Get(context.Background(), types.NamespacedName{Name: kcSecretName, Namespace: "test-ns"}, &kcSecret); err != nil {
		t.Fatalf("sandbox kubeconfig Secret not found: %v", err)
	}
	if len(kcSecret.OwnerReferences) == 0 || kcSecret.OwnerReferences[0].Name != run.Name {
		t.Fatal("kubeconfig Secret should be owned by AgenticRun")
	}
	if _, ok := kcSecret.Data[spokeKubeconfigFileName]; !ok {
		t.Fatal("kubeconfig Secret missing kubeconfig data key")
	}
	if kcSecret.Labels[LabelRun] != string(run.UID) {
		t.Fatalf("kubeconfig Secret run label = %q, want %q", kcSecret.Labels[LabelRun], string(run.UID))
	}

	// Decode the kubeconfig and verify the spoke token actually landed.
	// This closes the ensureSA → token → buildSandboxKubeconfig chain.
	parsedKC, kcErr := clientcmd.Load(kcSecret.Data[spokeKubeconfigFileName])
	if kcErr != nil {
		t.Fatalf("failed to parse sandbox kubeconfig: %v", kcErr)
	}
	ctxName := parsedKC.CurrentContext
	kcCtx, ok := parsedKC.Contexts[ctxName]
	if !ok {
		t.Fatalf("current context %q not found in kubeconfig", ctxName)
	}
	authInfo, ok := parsedKC.AuthInfos[kcCtx.AuthInfo]
	if !ok {
		t.Fatalf("auth info %q not found in kubeconfig", kcCtx.AuthInfo)
	}
	if authInfo.Token != "spoke-token-xyz" {
		t.Fatalf("kubeconfig token = %q, want %q", authInfo.Token, "spoke-token-xyz")
	}

	// Pod should have spoke-kubeconfig volume, mount, and KUBECONFIG env var.
	foundVolume := false
	for _, v := range pod.Spec.Volumes {
		if v.Name == spokeKubeconfigVolume {
			foundVolume = true
			if v.Secret == nil || v.Secret.SecretName != kcSecretName {
				t.Fatalf("spoke-kubeconfig volume source = %v, want Secret %q", v.VolumeSource, kcSecretName)
			}
		}
	}
	if !foundVolume {
		t.Fatal("pod missing spoke-kubeconfig volume")
	}

	foundMount := false
	for _, m := range pod.Spec.Containers[0].VolumeMounts {
		if m.Name == spokeKubeconfigVolume {
			foundMount = true
			if m.MountPath != spokeKubeconfigMountPath {
				t.Fatalf("mount path = %q, want %q", m.MountPath, spokeKubeconfigMountPath)
			}
			if !m.ReadOnly {
				t.Fatal("spoke-kubeconfig mount should be read-only")
			}
		}
	}
	if !foundMount {
		t.Fatal("pod missing spoke-kubeconfig volume mount")
	}

	foundEnv := false
	for _, e := range pod.Spec.Containers[0].Env {
		if e.Name == "KUBECONFIG" {
			foundEnv = true
			wantPath := spokeKubeconfigMountPath + "/" + spokeKubeconfigFileName
			if e.Value != wantPath {
				t.Fatalf("KUBECONFIG = %q, want %q", e.Value, wantPath)
			}
		}
	}
	if !foundEnv {
		t.Fatal("pod missing KUBECONFIG env var")
	}
}

// TestCreate_SpokeRun_KubeconfigSecretAlreadyExists verifies that when the
// kubeconfig Secret already exists (retry scenario), Create refreshes its
// data with the new token and rejects Secrets owned by a different run.
func TestCreate_SpokeRun_KubeconfigSecretAlreadyExists(t *testing.T) {
	origClient := NewClientFromConfig
	origClientset := NewClientsetFromConfig

	srcBindings := spokeReaderBindings()
	spokeFC := fake.NewClientBuilder().WithScheme(testScheme()).
		WithObjects(srcBindings[0], srcBindings[1]).Build()
	NewClientFromConfig = func(cfg *rest.Config) (client.Client, error) {
		return spokeFC, nil
	}

	cs := k8sfake.NewSimpleClientset()
	cs.PrependReactor("create", "serviceaccounts/token", func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, &authenticationv1.TokenRequest{
			Status: authenticationv1.TokenRequestStatus{Token: "refreshed-token"},
		}, nil
	})
	NewClientsetFromConfig = func(cfg *rest.Config) (kubernetes.Interface, error) {
		return cs, nil
	}
	t.Cleanup(func() {
		NewClientFromConfig = origClient
		NewClientsetFromConfig = origClientset
	})

	run := testSpokeRun()
	kcSecretName := sandboxKubeconfigSecretName(string(run.UID), "analysis")

	// Pre-create the kubeconfig Secret with the correct run label (same run retry).
	existingSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      kcSecretName,
			Namespace: "test-ns",
			Labels:    map[string]string{LabelRun: string(run.UID)},
		},
		Data: map[string][]byte{spokeKubeconfigFileName: []byte("old-data")},
	}

	cache := testCache(t, "bare-pod")
	hubFC := fake.NewClientBuilder().WithScheme(testScheme()).
		WithObjects(testReaderCRB(), testSpokeKubeconfigSecret(), existingSecret).Build()
	mgr := newTestSandboxManager(hubFC, cache)

	// Should succeed — Secret data refreshed with new token.
	_, err := mgr.Create(context.Background(), run, "analysis", testSMAgent(), testLLMForManager(), nil, 15*time.Minute, nil)
	if err != nil {
		t.Fatalf("Create with existing Secret failed: %v", err)
	}

	// Verify the Secret was updated with the refreshed token.
	var kcSecret corev1.Secret
	if err := hubFC.Get(context.Background(), types.NamespacedName{Name: kcSecretName, Namespace: "test-ns"}, &kcSecret); err != nil {
		t.Fatalf("kubeconfig Secret not found: %v", err)
	}
	parsedKC, err := clientcmd.Load(kcSecret.Data[spokeKubeconfigFileName])
	if err != nil {
		t.Fatalf("failed to parse refreshed kubeconfig: %v", err)
	}
	ctxName := parsedKC.CurrentContext
	kcCtx, ok := parsedKC.Contexts[ctxName]
	if !ok {
		t.Fatalf("current context %q not found in kubeconfig", ctxName)
	}
	authInfo := parsedKC.AuthInfos[kcCtx.AuthInfo]
	if authInfo.Token != "refreshed-token" {
		t.Fatalf("token after refresh = %q, want %q", authInfo.Token, "refreshed-token")
	}

	// Ownership guard (defense-in-depth): pre-create a Secret whose name
	// matches what run2 would generate but with a DIFFERENT run's label.
	// With UID-based naming this can't happen organically, but the guard
	// protects against hypothetical hash/truncation collisions.
	run2 := testSpokeRun()
	run2.UID = "different-uid"
	collisionSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      sandboxKubeconfigSecretName(string(run2.UID), "analysis"),
			Namespace: "test-ns",
			Labels:    map[string]string{LabelRun: "someone-elses-uid"},
		},
		Data: map[string][]byte{spokeKubeconfigFileName: []byte("other-data")},
	}

	hubFC2 := fake.NewClientBuilder().WithScheme(testScheme()).
		WithObjects(testReaderCRB(), testSpokeKubeconfigSecret(), collisionSecret).Build()
	mgr2 := newTestSandboxManager(hubFC2, cache)

	_, err = mgr2.Create(context.Background(), run2, "analysis", testSMAgent(), testLLMForManager(), nil, 15*time.Minute, nil)
	if err == nil {
		t.Fatal("expected error when kubeconfig Secret belongs to a different run")
	}
	if !strings.Contains(err.Error(), "belongs to a different run") {
		t.Fatalf("expected ownership error, got: %v", err)
	}
}

func TestRelease_SpokeRun_CleansUpSpoke(t *testing.T) {
	origClient := NewClientFromConfig

	// Pre-populate the spoke with SA + per-run CRBs.
	run := testSpokeRun()
	saName := sandboxSAName(run, "analysis")
	spokeSA := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{Name: saName, Namespace: spokeManagedNamespace},
	}
	spokeCRBs := spokeReaderBindings()
	// Create per-run CRBs (as addReaderSubjectOnSpoke would have done).
	perRunCRB0 := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:   perRunCRBName(string(run.UID), "analysis", 0),
			Labels: rbacLabels(string(run.UID), "reader-rbac"),
		},
		RoleRef:  spokeCRBs[0].RoleRef,
		Subjects: []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: saName, Namespace: spokeManagedNamespace}},
	}
	perRunCRB1 := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:   perRunCRBName(string(run.UID), "analysis", 1),
			Labels: rbacLabels(string(run.UID), "reader-rbac"),
		},
		RoleRef:  spokeCRBs[1].RoleRef,
		Subjects: []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: saName, Namespace: spokeManagedNamespace}},
	}
	spokeFC := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(
		spokeCRBs[0], spokeCRBs[1], spokeSA, perRunCRB0, perRunCRB1,
	).Build()
	NewClientFromConfig = func(cfg *rest.Config) (client.Client, error) {
		return spokeFC, nil
	}
	t.Cleanup(func() { NewClientFromConfig = origClient })

	cache := testCache(t, "bare-pod")
	hubFC := fake.NewClientBuilder().WithScheme(testScheme()).
		WithObjects(testReaderCRB(), testSpokeKubeconfigSecret()).Build()
	mgr := newTestSandboxManager(hubFC, cache)

	run.Status.Steps.Analysis.Sandbox.ClaimName = "ls-analysis-" + string(run.UID)

	// Create the hub-side pod so releaseBarePod doesn't error.
	hubPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: run.Status.Steps.Analysis.Sandbox.ClaimName, Namespace: "test-ns"},
	}
	if err := hubFC.Create(context.Background(), hubPod); err != nil {
		t.Fatalf("create hub pod: %v", err)
	}

	spoke := &SpokeAccess{Client: spokeFC, Namespace: spokeManagedNamespace}
	if err := mgr.Release(context.Background(), run, "analysis", spoke); err != nil {
		t.Fatalf("Release failed: %v", err)
	}

	// SA should be deleted from spoke.
	var sa corev1.ServiceAccount
	if err := spokeFC.Get(context.Background(), types.NamespacedName{Name: saName, Namespace: spokeManagedNamespace}, &sa); err == nil {
		t.Fatal("spoke SA should be deleted after Release")
	}

	// Per-run CRBs should be deleted.
	for i := range spokeReaderBindingNames {
		crbName := perRunCRBName(string(run.UID), "analysis", i)
		var crb rbacv1.ClusterRoleBinding
		if err := spokeFC.Get(context.Background(), types.NamespacedName{Name: crbName}, &crb); err == nil {
			t.Fatalf("per-run CRB %s should be deleted after Release", crbName)
		}
	}

	// Source CRBs should be untouched.
	for _, name := range spokeReaderBindingNames {
		var crb rbacv1.ClusterRoleBinding
		if err := spokeFC.Get(context.Background(), types.NamespacedName{Name: name}, &crb); err != nil {
			t.Fatalf("source CRB %s should still exist: %v", name, err)
		}
	}
}

func TestRelease_SpokeUnreachable_HubCleanupContinues(t *testing.T) {
	// Spoke kubeconfig Secret is missing (spoke decommissioned).
	// Hub cleanup should still proceed.
	cache := testCache(t, "bare-pod")
	hubFC := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(testReaderCRB()).Build()
	mgr := newTestSandboxManager(hubFC, cache)

	run := testSpokeRun()
	run.Status.Steps.Analysis.Sandbox.ClaimName = "ls-analysis-" + string(run.UID)

	// Create the hub-side pod.
	hubPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: run.Status.Steps.Analysis.Sandbox.ClaimName, Namespace: "test-ns"},
	}
	if err := hubFC.Create(context.Background(), hubPod); err != nil {
		t.Fatalf("create hub pod: %v", err)
	}

	// Release should not error — spoke failure is logged, hub cleanup proceeds.
	if err := mgr.Release(context.Background(), run, "analysis", nil); err != nil {
		t.Fatalf("Release should succeed even when spoke is unreachable, got: %v", err)
	}

	// Hub pod should be deleted.
	var pod corev1.Pod
	if err := hubFC.Get(context.Background(), types.NamespacedName{Name: run.Status.Steps.Analysis.Sandbox.ClaimName, Namespace: "test-ns"}, &pod); err == nil {
		t.Fatal("hub pod should be deleted")
	}
}

func TestCreate_LocalRun_Unchanged(t *testing.T) {
	// Regression: local run (no targetCluster) still works as before.
	cache := testCache(t, "bare-pod")
	fc := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(testReaderCRB()).Build()
	mgr := newTestSandboxManager(fc, cache)

	run := testSMRun()
	name, err := mgr.Create(context.Background(), run, "analysis", testSMAgent(), testLLMForManager(), nil, 15*time.Minute, nil)
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}

	// SA should be on hub.
	saName := sandboxSAName(run, "analysis")
	var sa corev1.ServiceAccount
	if err := fc.Get(context.Background(), types.NamespacedName{Name: saName, Namespace: "test-ns"}, &sa); err != nil {
		t.Fatalf("SA not found on hub: %v", err)
	}
	// Hub SA should have owner ref (set by setSAOwner).
	if len(sa.OwnerReferences) == 0 {
		t.Fatal("hub SA should have owner refs")
	}
	// Hub SA should NOT have spoke labels.
	if _, ok := sa.Labels[LabelSpokeCluster]; ok {
		t.Fatal("hub SA should not have spoke-cluster label")
	}

	// Pod should exist.
	var pod corev1.Pod
	if err := fc.Get(context.Background(), types.NamespacedName{Name: name, Namespace: "test-ns"}, &pod); err != nil {
		t.Fatalf("pod not found: %v", err)
	}

	// Hub pod should NOT have spoke-kubeconfig volume or KUBECONFIG env var.
	for _, v := range pod.Spec.Volumes {
		if v.Name == spokeKubeconfigVolume {
			t.Fatal("hub pod should not have spoke-kubeconfig volume")
		}
	}
	for _, e := range pod.Spec.Containers[0].Env {
		if e.Name == "KUBECONFIG" {
			t.Fatal("hub pod should not have KUBECONFIG env var")
		}
	}

	// No sandbox kubeconfig Secret should exist.
	kcSecretName := sandboxKubeconfigSecretName(string(run.UID), "analysis")
	var kcSecret corev1.Secret
	if err := fc.Get(context.Background(), types.NamespacedName{Name: kcSecretName, Namespace: "test-ns"}, &kcSecret); err == nil {
		t.Fatal("hub run should NOT create sandbox kubeconfig Secret")
	}
}

// TestCreate_OwnerPatchFailure_DoesNotDeleteWorkload verifies that a
// post-workload owner-ref patch failure does NOT trigger
// cleanupOnCreateFailure. The pod should still exist, and its
// dependencies (ConfigMap, SA, RBAC) should remain intact.
func TestCreate_OwnerPatchFailure_DoesNotDeleteWorkload(t *testing.T) {
	fc := fake.NewClientBuilder().
		WithScheme(testScheme()).
		WithObjects(testReaderCRB()).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				// Fail the ConfigMap owner-ref patch (setInputConfigMapOwner).
				if _, ok := obj.(*corev1.ConfigMap); ok {
					return fmt.Errorf("simulated owner patch failure")
				}
				return c.Patch(ctx, obj, patch, opts...)
			},
		}).
		Build()

	cache := testCache(t, "bare-pod")
	mgr := newTestSandboxManager(fc, cache)
	run := testSMRun()

	_, err := mgr.Create(context.Background(), run, "analysis", testSMAgent(), testLLMForManager(), nil, 15*time.Minute, nil)
	if err == nil {
		t.Fatal("expected error from owner-ref patch failure")
	}

	// Pod should still exist — cleanup must not delete it.
	podName := fmt.Sprintf("ls-analysis-%s", run.UID)
	var pod corev1.Pod
	if err := fc.Get(context.Background(), types.NamespacedName{Name: podName, Namespace: "test-ns"}, &pod); err != nil {
		t.Fatalf("pod should still exist after owner-patch failure, got: %v", err)
	}

	// ConfigMap should still exist — cleanup must not delete it.
	cmName := inputConfigMapName("analysis", string(run.UID))
	var cm corev1.ConfigMap
	if err := fc.Get(context.Background(), types.NamespacedName{Name: cmName, Namespace: "test-ns"}, &cm); err != nil {
		t.Fatalf("ConfigMap should still exist after owner-patch failure, got: %v", err)
	}

	// SA should still exist — cleanup must not delete it.
	saName := sandboxSAName(run, "analysis")
	var sa corev1.ServiceAccount
	if err := fc.Get(context.Background(), types.NamespacedName{Name: saName, Namespace: "test-ns"}, &sa); err != nil {
		t.Fatalf("SA should still exist after owner-patch failure, got: %v", err)
	}
}
