package agenticrun

import (
	"context"
	"strings"
	"testing"

	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
)

const testOperatorNS = "openshift-lightspeed"

// fakeNewClient replaces NewClientFromConfig in tests so no real
// cluster connection is attempted. Returns a fresh fake client each time.
func fakeNewClient(cfg *rest.Config) (client.Client, error) {
	return fake.NewClientBuilder().WithScheme(testScheme()).Build(), nil
}

func TestSpokeAccessForRun_EmptyTargetCluster(t *testing.T) {
	run := &agenticv1alpha1.AgenticRun{
		Spec: agenticv1alpha1.AgenticRunSpec{},
	}
	hubClient := fake.NewClientBuilder().WithScheme(testScheme()).Build()

	sa, err := spokeAccessForRun(context.Background(), hubClient, run, testOperatorNS)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sa != nil {
		t.Fatal("expected nil SpokeAccess for empty targetCluster")
	}
}

func TestSpokeAccessForRun_SecretExists(t *testing.T) {
	orig := NewClientFromConfig
	NewClientFromConfig = fakeNewClient
	t.Cleanup(func() { NewClientFromConfig = orig })

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
		ObjectMeta: metav1.ObjectMeta{
			Name:      "spoke-kubeconfig-test-spoke",
			Namespace: testOperatorNS,
		},
		Data: map[string][]byte{
			kubeconfigKey: kubeconfig,
		},
	}

	hubClient := fake.NewClientBuilder().
		WithScheme(testScheme()).
		WithObjects(secret).
		Build()

	run := &agenticv1alpha1.AgenticRun{
		Spec: agenticv1alpha1.AgenticRunSpec{
			TargetCluster: "test-spoke",
		},
	}

	sa, err := spokeAccessForRun(context.Background(), hubClient, run, testOperatorNS)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sa == nil {
		t.Fatal("expected non-nil SpokeAccess")
	}
	if sa.Namespace != spokeManagedNamespace {
		t.Errorf("namespace = %q, want %q", sa.Namespace, spokeManagedNamespace)
	}
	if sa.Client == nil {
		t.Error("expected non-nil spoke client")
	}
	if sa.Config == nil {
		t.Error("expected non-nil rest.Config")
	}
	if sa.Config.Timeout != spokeDialTimeout {
		t.Errorf("timeout = %v, want %v", sa.Config.Timeout, spokeDialTimeout)
	}
}

func TestSpokeAccessForRun_SecretMissing(t *testing.T) {
	hubClient := fake.NewClientBuilder().WithScheme(testScheme()).Build()

	run := &agenticv1alpha1.AgenticRun{
		Spec: agenticv1alpha1.AgenticRunSpec{
			TargetCluster: "nonexistent",
		},
	}

	sa, err := spokeAccessForRun(context.Background(), hubClient, run, testOperatorNS)
	if err == nil {
		t.Fatal("expected error for missing Secret")
	}
	if sa != nil {
		t.Fatal("expected nil SpokeAccess on error")
	}

	// Error message should include the Secret name for debugging.
	want := "spoke-kubeconfig-nonexistent"
	if got := err.Error(); !contains(got, want) {
		t.Errorf("error = %q, want it to contain %q", got, want)
	}
}

func TestSpokeAccessForRun_MalformedKubeconfig(t *testing.T) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "spoke-kubeconfig-bad",
			Namespace: testOperatorNS,
		},
		Data: map[string][]byte{
			kubeconfigKey: []byte("not valid yaml {{{"),
		},
	}

	hubClient := fake.NewClientBuilder().
		WithScheme(testScheme()).
		WithObjects(secret).
		Build()

	run := &agenticv1alpha1.AgenticRun{
		Spec: agenticv1alpha1.AgenticRunSpec{
			TargetCluster: "bad",
		},
	}

	sa, err := spokeAccessForRun(context.Background(), hubClient, run, testOperatorNS)
	if err == nil {
		t.Fatal("expected error for malformed kubeconfig")
	}
	if sa != nil {
		t.Fatal("expected nil SpokeAccess on error")
	}
}

func TestSpokeAccessForRun_MissingKubeconfigKey(t *testing.T) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "spoke-kubeconfig-nokey",
			Namespace: testOperatorNS,
		},
		Data: map[string][]byte{
			"wrong-key": []byte("some data"),
		},
	}

	hubClient := fake.NewClientBuilder().
		WithScheme(testScheme()).
		WithObjects(secret).
		Build()

	run := &agenticv1alpha1.AgenticRun{
		Spec: agenticv1alpha1.AgenticRunSpec{
			TargetCluster: "nokey",
		},
	}

	sa, err := spokeAccessForRun(context.Background(), hubClient, run, testOperatorNS)
	if err == nil {
		t.Fatal("expected error for missing kubeconfig key")
	}
	if sa != nil {
		t.Fatal("expected nil SpokeAccess on error")
	}

	want := `missing "kubeconfig" key`
	if got := err.Error(); !contains(got, want) {
		t.Errorf("error = %q, want it to contain %q", got, want)
	}
}

func TestSpokeAccessForRun_RejectsHTTP(t *testing.T) {
	orig := NewClientFromConfig
	NewClientFromConfig = fakeNewClient
	t.Cleanup(func() { NewClientFromConfig = orig })

	kubeconfig := []byte(`
apiVersion: v1
kind: Config
clusters:
- cluster:
    server: http://api.spoke.example.com:6443
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
		ObjectMeta: metav1.ObjectMeta{
			Name:      "spoke-kubeconfig-http-spoke",
			Namespace: testOperatorNS,
		},
		Data: map[string][]byte{
			kubeconfigKey: kubeconfig,
		},
	}

	hubClient := fake.NewClientBuilder().
		WithScheme(testScheme()).
		WithObjects(secret).
		Build()

	run := &agenticv1alpha1.AgenticRun{
		Spec: agenticv1alpha1.AgenticRunSpec{
			TargetCluster: "http-spoke",
		},
	}

	sa, err := spokeAccessForRun(context.Background(), hubClient, run, testOperatorNS)
	if err == nil {
		t.Fatal("expected error for HTTP kubeconfig")
	}
	if sa != nil {
		t.Fatal("expected nil SpokeAccess on error")
	}
	if !contains(err.Error(), ErrSpokeInsecureTLS) {
		t.Errorf("error = %q, want it to contain %q", err.Error(), ErrSpokeInsecureTLS)
	}
}

func TestSpokeAccessForRun_RejectsInsecureTLS(t *testing.T) {
	orig := NewClientFromConfig
	NewClientFromConfig = fakeNewClient
	t.Cleanup(func() { NewClientFromConfig = orig })

	kubeconfig := []byte(`
apiVersion: v1
kind: Config
clusters:
- cluster:
    server: https://api.spoke.example.com:6443
    insecure-skip-tls-verify: true
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
		ObjectMeta: metav1.ObjectMeta{
			Name:      "spoke-kubeconfig-insecure-spoke",
			Namespace: testOperatorNS,
		},
		Data: map[string][]byte{
			kubeconfigKey: kubeconfig,
		},
	}

	hubClient := fake.NewClientBuilder().
		WithScheme(testScheme()).
		WithObjects(secret).
		Build()

	run := &agenticv1alpha1.AgenticRun{
		Spec: agenticv1alpha1.AgenticRunSpec{
			TargetCluster: "insecure-spoke",
		},
	}

	sa, err := spokeAccessForRun(context.Background(), hubClient, run, testOperatorNS)
	if err == nil {
		t.Fatal("expected error for insecure TLS kubeconfig")
	}
	if sa != nil {
		t.Fatal("expected nil SpokeAccess on error")
	}
	if !contains(err.Error(), ErrSpokeInsecureTLS) {
		t.Errorf("error = %q, want it to contain %q", err.Error(), ErrSpokeInsecureTLS)
	}
}

func TestSpokeAccessForRun_RejectsExecProvider(t *testing.T) {
	orig := NewClientFromConfig
	NewClientFromConfig = fakeNewClient
	t.Cleanup(func() { NewClientFromConfig = orig })

	// A kubeconfig that uses exec-based credential plugin.
	kubeconfig := []byte(`
apiVersion: v1
kind: Config
clusters:
- cluster:
    server: https://api.spoke.example.com:6443
    certificate-authority-data: LS0tLS1CRUdJTi0tLS0t
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
    exec:
      apiVersion: client.authentication.k8s.io/v1
      command: /malicious-binary
      interactiveMode: Never
`)

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "spoke-kubeconfig-exec-spoke",
			Namespace: testOperatorNS,
		},
		Data: map[string][]byte{
			kubeconfigKey: kubeconfig,
		},
	}

	hubClient := fake.NewClientBuilder().
		WithScheme(testScheme()).
		WithObjects(secret).
		Build()

	run := &agenticv1alpha1.AgenticRun{
		Spec: agenticv1alpha1.AgenticRunSpec{
			TargetCluster: "exec-spoke",
		},
	}

	sa, err := spokeAccessForRun(context.Background(), hubClient, run, testOperatorNS)
	if err == nil {
		t.Fatal("expected error for exec-provider kubeconfig")
	}
	if sa != nil {
		t.Fatal("expected nil SpokeAccess on error")
	}
	if !contains(err.Error(), "external credential providers") {
		t.Errorf("error = %q, want it to contain 'external credential providers'", err.Error())
	}
}

// ---------------------------------------------------------------------------
// spokeLabels
// ---------------------------------------------------------------------------

func TestSpokeLabels(t *testing.T) {
	labels := spokeLabels("uid-123", "my-run", "prod-spoke", "sandbox-sa")

	want := map[string]string{
		LabelRun:          "uid-123",
		LabelComponent:    "sandbox-sa",
		LabelSpokeCluster: "prod-spoke",
		LabelAgenticRun:   "my-run",
	}
	if len(labels) != len(want) {
		t.Fatalf("expected %d labels, got %d: %v", len(want), len(labels), labels)
	}
	for k, v := range want {
		if labels[k] != v {
			t.Errorf("label %q = %q, want %q", k, labels[k], v)
		}
	}
}

func TestSpokeLabels_LongValuesTruncated(t *testing.T) {
	// 80-char cluster name exceeds the 63-char label limit.
	longCluster := "my-very-long-spoke-cluster-name-that-definitely-exceeds-sixty-three-characters-x"
	longRun := "my-very-long-agentic-run-name-that-also-exceeds-sixty-three-characters-padding-y"

	labels := spokeLabels("uid-123", longRun, longCluster, "analysis-sa")

	for _, key := range []string{LabelSpokeCluster, LabelAgenticRun} {
		v := labels[key]
		if len(v) > maxLabelValueLen {
			t.Errorf("label %q value length %d exceeds %d: %q", key, len(v), maxLabelValueLen, v)
		}
	}
}

func TestTruncateLabelValue_ShortUnchanged(t *testing.T) {
	v := "short-value"
	if got := truncateLabelValue(v); got != v {
		t.Errorf("expected %q unchanged, got %q", v, got)
	}
}

func TestTruncateLabelValue_ExactlyMaxUnchanged(t *testing.T) {
	v := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" // 63 chars
	if len(v) != maxLabelValueLen {
		t.Fatalf("test setup: len = %d, want %d", len(v), maxLabelValueLen)
	}
	if got := truncateLabelValue(v); got != v {
		t.Errorf("expected %q unchanged, got %q", v, got)
	}
}

func TestTruncateLabelValue_LongTruncatedWithHash(t *testing.T) {
	v := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-overflow" // 72 chars
	got := truncateLabelValue(v)
	if len(got) != maxLabelValueLen {
		t.Errorf("expected length %d, got %d: %q", maxLabelValueLen, len(got), got)
	}
	// same input must produce same output (deterministic)
	if got2 := truncateLabelValue(v); got != got2 {
		t.Errorf("non-deterministic: %q vs %q", got, got2)
	}
	// different inputs must produce different outputs
	v2 := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb-overflow" // 72 chars
	if got2 := truncateLabelValue(v2); got == got2 {
		t.Errorf("collision: both %q", got)
	}
}

func TestSpokeLabels_HubResourcesNotLabeled(t *testing.T) {
	// Hub resources use rbacLabels() which has only LabelRun + LabelComponent.
	hubLabels := rbacLabels("uid-123", "execution-rbac")
	if _, ok := hubLabels[LabelSpokeCluster]; ok {
		t.Fatal("hub resources should NOT have spoke-cluster label")
	}
	if _, ok := hubLabels[LabelAgenticRun]; ok {
		t.Fatal("hub resources should NOT have agentic-run label")
	}
}

// ---------------------------------------------------------------------------
// cleanupStepRBAC (spoke path)
// ---------------------------------------------------------------------------

func TestCleanupStepRBAC_Spoke_DeletesSAAndPerRunCRBs(t *testing.T) {
	ctx := context.Background()
	runUID := "uid-123"
	step := "analysis"
	saName := "ls-anl-uid-123"

	sa := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{Name: saName, Namespace: spokeManagedNamespace},
	}

	// Create source CRBs and per-run CRBs.
	srcBindings := spokeReaderBindings()
	objs := []client.Object{sa, srcBindings[0], srcBindings[1]}
	for i := range spokeReaderBindingNames {
		objs = append(objs, &rbacv1.ClusterRoleBinding{
			ObjectMeta: metav1.ObjectMeta{
				Name:   perRunCRBName(runUID, step, i),
				Labels: rbacLabels(runUID, "reader-rbac"),
			},
			RoleRef:  srcBindings[i].RoleRef,
			Subjects: []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: saName, Namespace: spokeManagedNamespace}},
		})
	}

	fc := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(objs...).Build()
	run := &agenticv1alpha1.AgenticRun{
		ObjectMeta: metav1.ObjectMeta{UID: types.UID(runUID)},
		Spec:       agenticv1alpha1.AgenticRunSpec{TargetCluster: "test-spoke"},
	}
	spoke := &SpokeAccess{Client: fc, Namespace: spokeManagedNamespace}

	if err := cleanupStepRBAC(ctx, spoke, fc, spokeManagedNamespace, run, step); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// SA should be deleted.
	var got corev1.ServiceAccount
	if err := fc.Get(ctx, types.NamespacedName{Name: saName, Namespace: spokeManagedNamespace}, &got); err == nil {
		t.Error("expected SA to be deleted")
	}

	// Per-run CRBs should be deleted.
	for i := range spokeReaderBindingNames {
		crbName := perRunCRBName(runUID, step, i)
		var crb rbacv1.ClusterRoleBinding
		if err := fc.Get(ctx, types.NamespacedName{Name: crbName}, &crb); err == nil {
			t.Fatalf("per-run CRB %s should be deleted", crbName)
		}
	}

	// Source CRBs should still exist untouched.
	for _, name := range spokeReaderBindingNames {
		var crb rbacv1.ClusterRoleBinding
		if err := fc.Get(ctx, types.NamespacedName{Name: name}, &crb); err != nil {
			t.Fatalf("source CRB %s should still exist: %v", name, err)
		}
	}
}

func TestCleanupStepRBAC_Spoke_IdempotentWhenMissing(t *testing.T) {
	fc := fake.NewClientBuilder().WithScheme(testScheme()).Build()
	run := &agenticv1alpha1.AgenticRun{ObjectMeta: metav1.ObjectMeta{UID: "uid-gone"}}

	spoke := &SpokeAccess{Client: fc, Namespace: spokeManagedNamespace}

	if err := cleanupStepRBAC(context.Background(), spoke, fc, spokeManagedNamespace, run, "analysis"); err != nil {
		t.Fatalf("expected no error for missing resources, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// cleanupStepRBAC (hub path)
// ---------------------------------------------------------------------------

func TestCleanupStepRBAC_Hub_RemovesSubjectAndDeletesSA(t *testing.T) {
	ctx := context.Background()
	resetReaderBindings()

	saName := "ls-anl-uid-hub"
	sa := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{Name: saName, Namespace: "default"},
	}
	// Create per-run reader CRBs (Boris's per-run model).
	perRunCRB := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name: "ls-reader-anl-uid-hub-0",
			Labels: map[string]string{
				LabelRun:       "uid-hub",
				LabelStep:      "analysis",
				LabelComponent: "reader-rbac",
			},
		},
		RoleRef:  rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "cluster-reader"},
		Subjects: []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: saName, Namespace: "default"}},
	}

	fc := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(sa, perRunCRB).Build()
	run := &agenticv1alpha1.AgenticRun{ObjectMeta: metav1.ObjectMeta{UID: "uid-hub"}}

	if err := cleanupStepRBAC(ctx, nil, fc, "default", run, "analysis"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// SA should be deleted.
	var gotSA corev1.ServiceAccount
	if err := fc.Get(ctx, types.NamespacedName{Name: saName, Namespace: "default"}, &gotSA); err == nil {
		t.Error("expected SA to be deleted")
	}

	// Per-run reader CRB should be deleted.
	var crb rbacv1.ClusterRoleBinding
	if err := fc.Get(ctx, types.NamespacedName{Name: "ls-reader-anl-uid-hub-0"}, &crb); !apierrors.IsNotFound(err) {
		t.Fatalf("expected per-run CRB to be deleted, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// requestSpokeToken
// ---------------------------------------------------------------------------

func TestRequestSpokeToken(t *testing.T) {
	orig := NewClientsetFromConfig
	cs := k8sfake.NewSimpleClientset()
	cs.PrependReactor("create", "serviceaccounts/token", func(action k8stesting.Action) (bool, runtime.Object, error) {
		createAction := action.(k8stesting.CreateAction)
		treq := createAction.GetObject().(*authenticationv1.TokenRequest)
		// Verify 24h expiration was requested.
		if treq.Spec.ExpirationSeconds == nil || *treq.Spec.ExpirationSeconds != 86400 {
			t.Errorf("expiration = %v, want 86400", treq.Spec.ExpirationSeconds)
		}
		return true, &authenticationv1.TokenRequest{
			Status: authenticationv1.TokenRequestStatus{Token: "test-spoke-token-abc"},
		}, nil
	})
	NewClientsetFromConfig = func(cfg *rest.Config) (kubernetes.Interface, error) {
		return cs, nil
	}
	t.Cleanup(func() { NewClientsetFromConfig = orig })

	token, err := requestSpokeToken(context.Background(), &rest.Config{}, "ls-anl-uid1", spokeManagedNamespace)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if token != "test-spoke-token-abc" {
		t.Errorf("token = %q, want test-spoke-token-abc", token)
	}
}

func TestRequestSpokeToken_Error(t *testing.T) {
	orig := NewClientsetFromConfig
	cs := k8sfake.NewSimpleClientset()
	// No reactor — CreateToken will fail because SA doesn't exist in fake.
	NewClientsetFromConfig = func(cfg *rest.Config) (kubernetes.Interface, error) {
		return cs, nil
	}
	t.Cleanup(func() { NewClientsetFromConfig = orig })

	_, err := requestSpokeToken(context.Background(), &rest.Config{}, "nonexistent", spokeManagedNamespace)
	if err == nil {
		t.Fatal("expected error for TokenRequest on non-existent SA")
	}
}

// ---------------------------------------------------------------------------
// buildSandboxKubeconfig
// ---------------------------------------------------------------------------

func TestBuildSandboxKubeconfig_Basic(t *testing.T) {
	spoke := &SpokeAccess{
		Config: &rest.Config{
			Host: "https://api.spoke.example.com:6443",
			TLSClientConfig: rest.TLSClientConfig{
				CAData: []byte("ca-data-here"),
			},
		},
	}

	data, err := buildSandboxKubeconfig(spoke, "test-token-123")
	if err != nil {
		t.Fatalf("buildSandboxKubeconfig: %v", err)
	}

	// Parse the generated kubeconfig.
	kc, err := clientcmd.Load(data)
	if err != nil {
		t.Fatalf("parse kubeconfig: %v", err)
	}
	if kc.CurrentContext != "spoke" {
		t.Fatalf("current-context = %q, want spoke", kc.CurrentContext)
	}
	cluster := kc.Clusters["spoke"]
	if cluster == nil {
		t.Fatal("cluster 'spoke' not found")
	}
	if cluster.Server != "https://api.spoke.example.com:6443" {
		t.Fatalf("server = %q", cluster.Server)
	}
	if string(cluster.CertificateAuthorityData) != "ca-data-here" {
		t.Fatalf("CA data = %q", cluster.CertificateAuthorityData)
	}
	if cluster.ProxyURL != "" {
		t.Fatalf("proxy-url should be empty, got %q", cluster.ProxyURL)
	}
	authInfo := kc.AuthInfos["sandbox"]
	if authInfo == nil {
		t.Fatal("authInfo 'sandbox' not found")
	}
	if authInfo.Token != "test-token-123" {
		t.Fatalf("token = %q", authInfo.Token)
	}
}

func TestBuildSandboxKubeconfig_WithProxyURL(t *testing.T) {
	spoke := &SpokeAccess{
		Config: &rest.Config{
			Host: "https://api.spoke.example.com:6443",
			TLSClientConfig: rest.TLSClientConfig{
				CAData: []byte("ca-data"),
			},
		},
		ProxyURL: "http://cluster-proxy.hub.svc:8090",
	}

	data, err := buildSandboxKubeconfig(spoke, "token-abc")
	if err != nil {
		t.Fatalf("buildSandboxKubeconfig: %v", err)
	}

	kc, err := clientcmd.Load(data)
	if err != nil {
		t.Fatalf("parse kubeconfig: %v", err)
	}
	cluster := kc.Clusters["spoke"]
	if cluster.ProxyURL != "http://cluster-proxy.hub.svc:8090" {
		t.Fatalf("proxy-url = %q, want http://cluster-proxy.hub.svc:8090", cluster.ProxyURL)
	}
}

// ---------------------------------------------------------------------------
// sandboxKubeconfigSecretName
// ---------------------------------------------------------------------------

func TestSandboxKubeconfigSecretName(t *testing.T) {
	name := sandboxKubeconfigSecretName("abc-123-uid", "analysis")
	// Step abbreviation comes before run UID: prefix + stepAbbrev + "-" + runUID
	if name != "ls-sandbox-kubeconfig-anl-abc-123-uid" {
		t.Fatalf("name = %q, want ls-sandbox-kubeconfig-anl-abc-123-uid", name)
	}

	// UIDs are fixed-length (36 chars) so truncation shouldn't happen in
	// practice, but verify it's still safe.
	longUID := strings.Repeat("x", 50)
	longName := sandboxKubeconfigSecretName(longUID, "execution")
	if len(longName) > 63 {
		t.Fatalf("name exceeds 63 chars: %d", len(longName))
	}
	// Step discriminator must survive truncation.
	if !strings.Contains(longName, "-exe-") {
		t.Fatalf("step discriminator lost after truncation: %q", longName)
	}

	// Two steps of the same run must produce different names.
	analysisName := sandboxKubeconfigSecretName(longUID, "analysis")
	executionName := sandboxKubeconfigSecretName(longUID, "execution")
	if analysisName == executionName {
		t.Fatalf("name collision between steps: both = %q", analysisName)
	}
}

// ---------------------------------------------------------------------------
// spokeAccessForRun — ProxyURL extraction
// ---------------------------------------------------------------------------

func TestSpokeAccessForRun_ExtractsProxyURL(t *testing.T) {
	orig := NewClientFromConfig
	NewClientFromConfig = fakeNewClient
	t.Cleanup(func() { NewClientFromConfig = orig })

	kubeconfig := []byte(`
apiVersion: v1
kind: Config
clusters:
- cluster:
    server: https://api.spoke.example.com:6443
    proxy-url: http://cluster-proxy.hub.svc:8090
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
		ObjectMeta: metav1.ObjectMeta{
			Name:      "spoke-kubeconfig-mce-spoke",
			Namespace: testOperatorNS,
		},
		Data: map[string][]byte{kubeconfigKey: kubeconfig},
	}
	hubClient := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(secret).Build()

	run := &agenticv1alpha1.AgenticRun{
		Spec: agenticv1alpha1.AgenticRunSpec{TargetCluster: "mce-spoke"},
	}

	sa, err := spokeAccessForRun(context.Background(), hubClient, run, testOperatorNS)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sa.ProxyURL != "http://cluster-proxy.hub.svc:8090" {
		t.Fatalf("ProxyURL = %q, want http://cluster-proxy.hub.svc:8090", sa.ProxyURL)
	}
}

func TestSpokeAccessForRun_NoProxyURL(t *testing.T) {
	orig := NewClientFromConfig
	NewClientFromConfig = fakeNewClient
	t.Cleanup(func() { NewClientFromConfig = orig })

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
		ObjectMeta: metav1.ObjectMeta{
			Name:      "spoke-kubeconfig-direct-spoke",
			Namespace: testOperatorNS,
		},
		Data: map[string][]byte{kubeconfigKey: kubeconfig},
	}
	hubClient := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(secret).Build()

	run := &agenticv1alpha1.AgenticRun{
		Spec: agenticv1alpha1.AgenticRunSpec{TargetCluster: "direct-spoke"},
	}

	sa, err := spokeAccessForRun(context.Background(), hubClient, run, testOperatorNS)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sa.ProxyURL != "" {
		t.Fatalf("ProxyURL = %q, want empty", sa.ProxyURL)
	}
}

// contains is a simple helper.
func contains(s, substr string) bool {
	return len(s) >= len(substr) && searchString(s, substr)
}

func searchString(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
