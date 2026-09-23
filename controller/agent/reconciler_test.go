package agent

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
)

const operatorNS = "openshift-lightspeed"

func testScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	utilruntime.Must(agenticv1alpha1.AddToScheme(s))
	utilruntime.Must(corev1.AddToScheme(s))
	return s
}

func testAgent() *agenticv1alpha1.Agent {
	return &agenticv1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "default", Generation: 1},
		Spec: agenticv1alpha1.AgentSpec{
			LLMProvider: agenticv1alpha1.LLMProviderReference{Name: "openai"},
			Model:       "gpt-4",
		},
	}
}

func testProvider() *agenticv1alpha1.LLMProvider {
	return &agenticv1alpha1.LLMProvider{
		ObjectMeta: metav1.ObjectMeta{Name: "openai"},
		Spec: agenticv1alpha1.LLMProviderSpec{
			Type: agenticv1alpha1.LLMProviderOpenAI,
			OpenAI: agenticv1alpha1.OpenAIConfig{
				CredentialsSecret: agenticv1alpha1.SecretReference{Name: "llm-creds"},
			},
		},
	}
}

func testSecret() *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "llm-creds", Namespace: operatorNS},
		Data:       map[string][]byte{"OPENAI_API_KEY": []byte("sk-test")},
	}
}

func newReconciler(objs ...client.Object) (*Reconciler, client.Client) {
	builder := fake.NewClientBuilder().WithScheme(testScheme()).
		WithStatusSubresource(&agenticv1alpha1.Agent{})
	if len(objs) > 0 {
		builder = builder.WithObjects(objs...)
	}
	fc := builder.Build()
	return &Reconciler{Client: fc, Namespace: operatorNS}, fc
}

func reconcileDefault(r *Reconciler) (ctrl.Result, error) {
	return r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "default"},
	})
}

func getAgent(t *testing.T, c client.Client) *agenticv1alpha1.Agent {
	t.Helper()
	got := &agenticv1alpha1.Agent{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "default"}, got); err != nil {
		t.Fatalf("get Agent: %v", err)
	}
	return got
}

func TestReconcile_NotFound(t *testing.T) {
	r, _ := newReconciler()
	if _, err := reconcileDefault(r); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
}

func TestReconcile_ReadyTrue(t *testing.T) {
	r, fc := newReconciler(testAgent(), testProvider(), testSecret())
	result, err := reconcileDefault(r)
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if result.RequeueAfter != 0 {
		t.Fatalf("Ready=True should not requeue, got %s", result.RequeueAfter)
	}
	got := getAgent(t, fc)
	if got.Status.Ready != string(metav1.ConditionTrue) {
		t.Fatalf("status.ready = %q, want True", got.Status.Ready)
	}
	cond := meta.FindStatusCondition(got.Status.Conditions, agenticv1alpha1.AgentConditionReady)
	if cond == nil {
		t.Fatal("Ready condition missing")
	}
	if cond.Status != metav1.ConditionTrue || cond.Reason != reasonReady {
		t.Fatalf("Ready condition = %+v", cond)
	}
}

func TestReconcile_MissingProvider(t *testing.T) {
	r, fc := newReconciler(testAgent())
	result, err := reconcileDefault(r)
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if result.RequeueAfter != requeueWhenNotReady {
		t.Fatalf("Ready=False should requeue, got %s", result.RequeueAfter)
	}
	got := getAgent(t, fc)
	if got.Status.Ready != string(metav1.ConditionFalse) {
		t.Fatalf("status.ready = %q, want False", got.Status.Ready)
	}
	cond := meta.FindStatusCondition(got.Status.Conditions, agenticv1alpha1.AgentConditionReady)
	if cond == nil || cond.Reason != reasonLLMProviderNotFound {
		t.Fatalf("Ready condition = %+v, want reason %s", cond, reasonLLMProviderNotFound)
	}
}

func TestReconcile_MissingSecret(t *testing.T) {
	r, fc := newReconciler(testAgent(), testProvider())
	if _, err := reconcileDefault(r); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	got := getAgent(t, fc)
	cond := meta.FindStatusCondition(got.Status.Conditions, agenticv1alpha1.AgentConditionReady)
	if cond == nil || cond.Reason != reasonSecretNotFound {
		t.Fatalf("Ready condition = %+v, want reason %s", cond, reasonSecretNotFound)
	}
	if got.Status.Ready != string(metav1.ConditionFalse) {
		t.Fatalf("status.ready = %q, want False", got.Status.Ready)
	}
}

func TestReconcile_MissingSecretKey(t *testing.T) {
	secret := testSecret()
	secret.Data = map[string][]byte{"wrong": []byte("x")}
	r, fc := newReconciler(testAgent(), testProvider(), secret)
	if _, err := reconcileDefault(r); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	got := getAgent(t, fc)
	cond := meta.FindStatusCondition(got.Status.Conditions, agenticv1alpha1.AgentConditionReady)
	if cond == nil || cond.Reason != reasonSecretKeysMissing {
		t.Fatalf("Ready condition = %+v, want reason %s", cond, reasonSecretKeysMissing)
	}
}

func TestReconcile_SecretRestoreBecomesReady(t *testing.T) {
	r, fc := newReconciler(testAgent(), testProvider())
	if _, err := reconcileDefault(r); err != nil {
		t.Fatalf("first Reconcile: %v", err)
	}
	if err := fc.Create(context.Background(), testSecret()); err != nil {
		t.Fatalf("create secret: %v", err)
	}
	if _, err := reconcileDefault(r); err != nil {
		t.Fatalf("second Reconcile: %v", err)
	}
	got := getAgent(t, fc)
	if got.Status.Ready != string(metav1.ConditionTrue) {
		t.Fatalf("status.ready = %q, want True after secret restore", got.Status.Ready)
	}
}

func TestReconcile_Idempotent(t *testing.T) {
	r, fc := newReconciler(testAgent(), testProvider(), testSecret())
	if _, err := reconcileDefault(r); err != nil {
		t.Fatalf("first: %v", err)
	}
	before := getAgent(t, fc)
	if _, err := reconcileDefault(r); err != nil {
		t.Fatalf("second: %v", err)
	}
	after := getAgent(t, fc)
	if before.ResourceVersion != after.ResourceVersion {
		t.Fatalf("second reconcile should be a no-op patch, resourceVersion %s -> %s", before.ResourceVersion, after.ResourceVersion)
	}
}

func TestMapSecret_IgnoresOtherNamespaces(t *testing.T) {
	r, _ := newReconciler(testAgent(), testProvider(), testSecret())
	reqs := r.mapSecret(context.Background(), &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "llm-creds", Namespace: "other"},
	})
	if len(reqs) != 0 {
		t.Fatalf("expected no requests for other namespace, got %v", reqs)
	}
}

func TestMapLLMProvider_EnqueuesReferencingAgents(t *testing.T) {
	r, _ := newReconciler(testAgent(), testProvider())
	reqs := r.mapLLMProvider(context.Background(), testProvider())
	if len(reqs) != 1 || reqs[0].Name != "default" {
		t.Fatalf("requests = %v, want default", reqs)
	}
}
