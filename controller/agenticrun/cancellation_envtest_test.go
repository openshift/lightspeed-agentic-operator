package agenticrun

import (
	"context"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
)

func startCancellationEnvtest(t *testing.T) *rest.Config {
	t.Helper()
	assetsDir := os.Getenv("KUBEBUILDER_ASSETS")
	if assetsDir == "" {
		t.Skip("set KUBEBUILDER_ASSETS to Kubernetes 1.35+ envtest binaries")
		return nil
	}
	testEnv := &envtest.Environment{
		BinaryAssetsDirectory: assetsDir,
		CRDDirectoryPaths:     []string{"../../config/crd/bases"},
	}
	cfg, err := testEnv.Start()
	if err != nil {
		t.Fatalf("start envtest: %v", err)
	}
	t.Cleanup(func() {
		if err := testEnv.Stop(); err != nil {
			t.Errorf("stop envtest: %v", err)
		}
	})
	return cfg
}

func TestAgenticRunCancelledCEL(t *testing.T) {
	cfg := startCancellationEnvtest(t)
	dc, err := dynamic.NewForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	namespaces := dc.Resource(schema.GroupVersionResource{Version: "v1", Resource: "namespaces"})
	_, err = namespaces.Create(ctx, &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1", "kind": "Namespace",
		"metadata": map[string]interface{}{"name": "default"},
	}}, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatal(err)
	}
	runs := dc.Resource(schema.GroupVersionResource{
		Group: "agentic.openshift.io", Version: "v1alpha1", Resource: "agenticruns",
	}).Namespace("default")
	newRun := func(name string, cancelled *bool) *unstructured.Unstructured {
		spec := map[string]interface{}{
			"request": "CEL test", "analysis": map[string]interface{}{"agent": "default"},
		}
		if cancelled != nil {
			spec["cancelled"] = *cancelled
		}
		return &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "agentic.openshift.io/v1alpha1", "kind": "AgenticRun",
			"metadata": map[string]interface{}{"name": name}, "spec": spec,
		}}
	}
	created, err := runs.Create(ctx, newRun("cancel-absent", nil), metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, found, err := unstructured.NestedBool(created.Object, "spec", "cancelled"); err != nil || found {
		t.Fatalf("omitted cancelled field = found %v, err %v; want absent", found, err)
	}
	if err := unstructured.SetNestedField(created.Object, true, "spec", "cancelled"); err != nil {
		t.Fatal(err)
	}
	accepted, err := runs.Update(ctx, created, metav1.UpdateOptions{})
	if err != nil {
		t.Fatalf("absent to true: %v", err)
	}
	repeated := accepted.DeepCopy()
	if _, err := runs.Update(ctx, repeated, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("true to true: %v", err)
	}
	for _, remove := range []bool{false, true} {
		candidate := accepted.DeepCopy()
		if remove {
			unstructured.RemoveNestedField(candidate.Object, "spec", "cancelled")
		} else if err := unstructured.SetNestedField(candidate.Object, false, "spec", "cancelled"); err != nil {
			t.Fatal(err)
		}
		if _, err := runs.Update(ctx, candidate, metav1.UpdateOptions{}); !apierrors.IsInvalid(err) {
			t.Fatalf("true to false/remove (remove=%v) error = %v, want Invalid", remove, err)
		}
	}
	falseValue := false
	explicitFalse, err := runs.Create(ctx, newRun("cancel-false", &falseValue), metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	removeFalse := explicitFalse.DeepCopy()
	unstructured.RemoveNestedField(removeFalse.Object, "spec", "cancelled")
	if _, err := runs.Update(ctx, removeFalse, metav1.UpdateOptions{}); !apierrors.IsInvalid(err) {
		t.Fatalf("false to remove error = %v, want Invalid", err)
	}
	if err := unstructured.SetNestedField(explicitFalse.Object, true, "spec", "cancelled"); err != nil {
		t.Fatal(err)
	}
	if _, err := runs.Update(ctx, explicitFalse, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("false to true: %v", err)
	}
}

func newCancellationEnvtestReconciler(
	t *testing.T,
	start time.Time,
) (client.Client, *AgenticRunReconciler) {
	t.Helper()
	cfg := startCancellationEnvtest(t)
	c, err := client.New(cfg, client.Options{Scheme: testScheme()})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default"}}); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create default namespace: %v", err)
	}
	cache := testCache(t, "bare-pod")
	// Envtest does not install the optional agent-sandbox CRDs.
	cache.ForceBareMode = true
	r := &AgenticRunReconciler{
		Client:    c,
		APIReader: c,
		Agent:     newTestAgentCaller().withClient(t, c, "default"),
		Sandbox:   NewSandboxManager(c, c, cache, "default", nil),
		Config:    cache,
		Namespace: "default",
		Now:       func() time.Time { return start },
	}
	return c, r
}

func reconcileCancellationAt(
	t *testing.T,
	r *AgenticRunReconciler,
	run *agenticv1alpha1.AgenticRun,
	at time.Time,
) ctrl.Result {
	t.Helper()
	r.Now = func() time.Time { return at }
	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKeyFromObject(run),
	})
	if err != nil {
		t.Fatalf("Reconcile at %s: %v", at, err)
	}
	return result
}

func newExecutingCancellationEnvtestRun(
	t *testing.T,
	c client.Client,
	start time.Time,
	targetCluster string,
	holdPod bool,
) *agenticv1alpha1.AgenticRun {
	t.Helper()
	ctx := context.Background()
	run := testAgenticRun()
	run.Spec.Tools.Skills[0].Paths = []string{"/skills/smoke"}
	run.Name = "cancel-executing"
	cancelled := true
	run.Spec.Cancelled = &cancelled
	run.Spec.TargetCluster = targetCluster
	if err := c.Create(ctx, run); err != nil {
		t.Fatalf("create executing AgenticRun: %v", err)
	}

	persisted := &agenticv1alpha1.AgenticRun{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(run), persisted); err != nil {
		t.Fatalf("get executing AgenticRun: %v", err)
	}
	meta.SetStatusCondition(&persisted.Status.Conditions, metav1.Condition{
		Type:               agenticv1alpha1.AgenticRunConditionAnalyzed,
		Status:             metav1.ConditionTrue,
		Reason:             reasonComplete,
		Message:            "Analysis complete",
		ObservedGeneration: persisted.Generation,
		LastTransitionTime: metav1.NewTime(start),
	})
	meta.SetStatusCondition(&persisted.Status.Conditions, metav1.Condition{
		Type:               agenticv1alpha1.AgenticRunConditionExecuted,
		Status:             metav1.ConditionUnknown,
		Reason:             reasonInProgress,
		Message:            "Execution agent is running",
		ObservedGeneration: persisted.Generation,
		LastTransitionTime: metav1.NewTime(start),
	})
	persisted.Status.Steps.Execution.Sandbox = agenticv1alpha1.SandboxInfo{
		ClaimName: "claim-smoke",
		Namespace: "default",
	}
	if err := c.Status().Update(ctx, persisted); err != nil {
		t.Fatalf("persist executing AgenticRun status: %v", err)
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cancel-executing-pod",
			Namespace: "default",
			Labels: map[string]string{
				LabelRun:  string(persisted.UID),
				LabelStep: "execution",
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "agent", Image: "registry.example.com/agent:latest"}},
		},
	}
	if holdPod {
		pod.Finalizers = []string{"smoke.agentic.openshift.io/hold"}
	}
	if err := c.Create(ctx, pod); err != nil {
		t.Fatalf("create execution sandbox Pod: %v", err)
	}
	return persisted
}

func readCancellationEnvtestRun(
	t *testing.T,
	c client.Client,
	run *agenticv1alpha1.AgenticRun,
) *agenticv1alpha1.AgenticRun {
	t.Helper()
	got := &agenticv1alpha1.AgenticRun{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(run), got); err != nil {
		t.Fatalf("get AgenticRun status: %v", err)
	}
	return got
}

func requireCancellationEnvtestCondition(
	t *testing.T,
	run *agenticv1alpha1.AgenticRun,
	conditionType string,
	status metav1.ConditionStatus,
	reason string,
) *metav1.Condition {
	t.Helper()
	condition := meta.FindStatusCondition(run.Status.Conditions, conditionType)
	if condition == nil || condition.Status != status || condition.Reason != reason {
		t.Fatalf("condition %q = %+v, want status %q reason %q", conditionType, condition, status, reason)
	}
	return condition
}

func assertCancellationEnvtestFailed(
	t *testing.T,
	run *agenticv1alpha1.AgenticRun,
	reason string,
) *metav1.Condition {
	t.Helper()
	if phase := agenticv1alpha1.DerivePhase(run.Status.Conditions); phase != agenticv1alpha1.AgenticRunPhaseFailed {
		t.Fatalf("AgenticRun phase = %q, want Failed; conditions=%+v", phase, run.Status.Conditions)
	}
	return requireCancellationEnvtestCondition(
		t,
		run,
		agenticv1alpha1.AgenticRunConditionExecuted,
		metav1.ConditionFalse,
		reason,
	)
}

func TestCancellationEnvtestExecutingRun(t *testing.T) {
	start := time.Now().UTC().Truncate(time.Second)
	c, r := newCancellationEnvtestReconciler(t, start)
	run := newExecutingCancellationEnvtestRun(t, c, start, "", false)

	reconcileCancellationAt(t, r, run, start)

	got := readCancellationEnvtestRun(t, c, run)
	executed := assertCancellationEnvtestFailed(t, got, reasonCancelledByUser)
	if len(got.Status.Steps.Execution.Results) != 0 {
		t.Fatalf("execution phase dispatched before cancellation: results=%+v", got.Status.Steps.Execution.Results)
	}
	if executed.Message != "Run stopped by user" {
		t.Fatalf("Executed message = %q, want Run stopped by user", executed.Message)
	}
	requireCancellationEnvtestCondition(
		t,
		got,
		agenticv1alpha1.AgenticRunConditionStopping,
		metav1.ConditionFalse,
		reasonTeardownComplete,
	)
	if hasRunSandboxInfo(got) {
		t.Fatalf("confirmed cancellation retained sandbox refs: %+v", got.Status.Steps.Execution.Sandbox)
	}
}

func TestCancellationEnvtestDeadlineFailureLaterCleanup(t *testing.T) {
	start := time.Now().UTC().Truncate(time.Second)
	c, r := newCancellationEnvtestReconciler(t, start)
	run := newExecutingCancellationEnvtestRun(t, c, start, "", true)

	reconcileCancellationAt(t, r, run, start)
	acceptedRun := readCancellationEnvtestRun(t, c, run)
	accepted := requireCancellationEnvtestCondition(
		t,
		acceptedRun,
		agenticv1alpha1.AgenticRunConditionStopping,
		metav1.ConditionTrue,
		reasonCancelledByUser,
	)
	acceptedAt := accepted.LastTransitionTime.Time
	if acceptedAt.IsZero() || !acceptedAt.Equal(start) {
		t.Fatalf("Stopping acceptance time = %s, want %s", acceptedAt, start)
	}

	reconcileCancellationAt(t, r, run, acceptedAt.Add(SuspensionStopWindow))
	failed := readCancellationEnvtestRun(t, c, run)
	assertCancellationEnvtestFailed(t, failed, reasonCancellationFailed)
	requireCancellationEnvtestCondition(
		t,
		failed,
		agenticv1alpha1.AgenticRunConditionStopping,
		metav1.ConditionFalse,
		reasonTeardownFailed,
	)
	if sandbox := failed.Status.Steps.Execution.Sandbox; sandbox.ClaimName != "claim-smoke" || sandbox.Namespace != "default" {
		t.Fatalf("deadline failure sandbox refs = %+v, want retained claim-smoke/default", sandbox)
	}

	pod := &corev1.Pod{}
	podKey := client.ObjectKey{Namespace: "default", Name: "cancel-executing-pod"}
	if err := c.Get(context.Background(), podKey, pod); err != nil {
		t.Fatalf("get held execution sandbox Pod: %v", err)
	}
	pod.Finalizers = nil
	if err := c.Update(context.Background(), pod); err != nil {
		t.Fatalf("remove execution sandbox Pod finalizer: %v", err)
	}

	reconcileCancellationAt(t, r, run, acceptedAt.Add(SuspensionStopWindow))
	cleaned := readCancellationEnvtestRun(t, c, run)
	assertCancellationEnvtestFailed(t, cleaned, reasonCancellationFailed)
	requireCancellationEnvtestCondition(
		t,
		cleaned,
		agenticv1alpha1.AgenticRunConditionStopping,
		metav1.ConditionFalse,
		reasonTeardownComplete,
	)
	if hasRunSandboxInfo(cleaned) {
		t.Fatalf("later cleanup retained sandbox refs: %+v", cleaned.Status.Steps.Execution.Sandbox)
	}
}

func TestCancellationEnvtestUnreachableSpoke(t *testing.T) {
	start := time.Now().UTC().Truncate(time.Second)
	c, r := newCancellationEnvtestReconciler(t, start)
	run := newExecutingCancellationEnvtestRun(t, c, start, "smoke-spoke", true)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	spokeServer := "https://" + listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	kubeconfig := []byte(`apiVersion: v1
kind: Config
clusters:
- cluster:
    server: ` + spokeServer + `
  name: smoke-spoke
contexts:
- context:
    cluster: smoke-spoke
    user: smoke-user
  name: smoke-spoke
current-context: smoke-spoke
users:
- name: smoke-user
  user:
    token: dummy-token
`)
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      spokeKubeconfigPrefix + run.Spec.TargetCluster,
			Namespace: "default",
		},
		Data: map[string][]byte{spokeKubeconfigFileName: kubeconfig},
	}
	if err := c.Create(context.Background(), secret); err != nil {
		t.Fatalf("create unreachable spoke kubeconfig Secret: %v", err)
	}

	spoke, err := spokeAccessForRun(context.Background(), c, run, r.Namespace)
	if err != nil {
		t.Fatalf("resolve unreachable spoke client: %v", err)
	}
	report, err := cleanupSpokeRunAccess(context.Background(), run, spoke)
	if err != nil {
		t.Fatalf("cleanup unreachable spoke access: %v", err)
	}
	if report.Confirmed() {
		t.Fatalf("unreachable spoke cleanup was confirmed: %+v", report)
	}
	var getFailure string
	for _, item := range report.Unverified {
		if strings.Contains(item, "GET:") {
			getFailure = item
			break
		}
	}
	if getFailure == "" || !strings.Contains(getFailure, "127.0.0.1") {
		t.Fatalf("unreachable spoke GET failure = %q, want recorded loopback GET error; report=%+v", getFailure, report)
	}

	reconcileCancellationAt(t, r, run, start)
	acceptedRun := readCancellationEnvtestRun(t, c, run)
	accepted := requireCancellationEnvtestCondition(
		t,
		acceptedRun,
		agenticv1alpha1.AgenticRunConditionStopping,
		metav1.ConditionTrue,
		reasonCancelledByUser,
	)
	acceptedAt := accepted.LastTransitionTime.Time
	if acceptedAt.IsZero() || !acceptedAt.Equal(start) {
		t.Fatalf("Stopping acceptance time = %s, want %s", acceptedAt, start)
	}

	reconcileCancellationAt(t, r, run, acceptedAt.Add(SuspensionStopWindow))
	failed := readCancellationEnvtestRun(t, c, run)
	assertCancellationEnvtestFailed(t, failed, reasonCancellationFailed)
	requireCancellationEnvtestCondition(
		t,
		failed,
		agenticv1alpha1.AgenticRunConditionStopping,
		metav1.ConditionFalse,
		reasonTeardownFailed,
	)
	if sandbox := failed.Status.Steps.Execution.Sandbox; sandbox.ClaimName != "claim-smoke" || sandbox.Namespace != "default" {
		t.Fatalf("unreachable-spoke sandbox refs = %+v, want retained claim-smoke/default", sandbox)
	}
}
