//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
)

const pollInterval = 2 * time.Second

var pollTimeout = func() time.Duration {
	if v := os.Getenv("E2E_POLL_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err == nil {
			return d
		}
	}
	return 10 * time.Minute
}()

var testNS = func() string {
	if ns := os.Getenv("TEST_NAMESPACE"); ns != "" {
		return ns
	}
	return "openshift-lightspeed"
}()

// --- Client ---

func newClient(t *testing.T) client.Client {
	t.Helper()

	kubeconfig := os.Getenv("KUBECONFIG")
	if kubeconfig == "" {
		home, _ := os.UserHomeDir()
		kubeconfig = home + "/.kube/config"
	}

	cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		t.Fatalf("build kubeconfig: %v", err)
	}

	s := scheme.Scheme
	utilruntime.Must(agenticv1alpha1.AddToScheme(s))

	c, err := client.New(cfg, client.Options{Scheme: s})
	if err != nil {
		t.Fatalf("create client: %v", err)
	}
	return c
}

// --- Pointer helpers ---

func ptrBool(v bool) *bool    { return &v }
func ptrInt32(v int32) *int32 { return &v }

// --- Cleanup ---

// cleanup deletes objects, stripping finalizers if needed. Order: delete (sets DeletionTimestamp
// so the operator won't re-add finalizers), then patch finalizers to nil so deletion completes.
// Logs each action and verifies the object is gone.
func cleanup(t *testing.T, c client.Client, objs ...client.Object) {
	t.Helper()
	ctx := context.Background()
	for _, obj := range objs {
		kind := obj.GetObjectKind().GroupVersionKind().Kind
		if kind == "" {
			kind = fmt.Sprintf("%T", obj)
		}
		name := obj.GetName()
		ns := obj.GetNamespace()
		key := types.NamespacedName{Name: name, Namespace: ns}

		if err := c.Get(ctx, key, obj); err != nil {
			t.Logf("cleanup: %s/%s not found (already clean)", kind, name)
			continue
		}
		_ = c.Delete(ctx, obj)
		if err := c.Get(ctx, key, obj); err != nil {
			t.Logf("cleanup: %s/%s deleted", kind, name)
			continue
		}
		if len(obj.GetFinalizers()) > 0 {
			t.Logf("cleanup: %s/%s stripping finalizers %v", kind, name, obj.GetFinalizers())
			obj.SetFinalizers(nil)
			_ = c.Update(ctx, obj)
		}
		if err := c.Get(ctx, key, obj); err != nil {
			t.Logf("cleanup: %s/%s deleted", kind, name)
		} else {
			t.Logf("cleanup: WARNING %s/%s still exists after cleanup", kind, name)
		}
	}
}

// deleteSandboxClaim removes a SandboxClaim by name (no typed Go struct in this repo).
func deleteSandboxClaim(t *testing.T, c client.Client, name, namespace string) {
	t.Helper()
	obj := &metav1.PartialObjectMetadata{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "extensions.agents.x-k8s.io/v1alpha1",
			Kind:       "SandboxClaim",
		},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
	}
	_ = c.Delete(context.Background(), obj)
}

// deleteBarePod removes a bare pod by name from the operator namespace (same as testNS).
func deleteBarePod(t *testing.T, c client.Client, name string) {
	t.Helper()
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNS}}
	_ = c.Delete(context.Background(), pod)
}

// --- Fixture builders ---

// e2eFixtures holds the prerequisite CRs needed for any run flow.
type e2eFixtures struct {
	LLM       *agenticv1alpha1.LLMProvider
	Agent     *agenticv1alpha1.Agent
	Policy    *agenticv1alpha1.ApprovalPolicy
	LLMSecret *corev1.Secret
}

// createFixtures creates the prerequisite chain (LLMProvider, Agent, ApprovalPolicy, Secret)
// and registers cleanup. Cleans up leftovers from previous failed runs first.
func createFixtures(t *testing.T, c client.Client) *e2eFixtures {
	t.Helper()

	if os.Getenv("E2E_PROVIDER") != "" {
		return createRealProviderFixtures(t, c)
	}

	ctx := context.Background()

	f := &e2eFixtures{
		LLM: &agenticv1alpha1.LLMProvider{
			ObjectMeta: metav1.ObjectMeta{Name: "e2e-llm"},
			Spec: agenticv1alpha1.LLMProviderSpec{
				Type: agenticv1alpha1.LLMProviderGoogleCloudVertex,
				GoogleCloudVertex: agenticv1alpha1.GoogleCloudVertexConfig{
					CredentialsSecret: agenticv1alpha1.SecretReference{Name: "e2e-llm-secret"},
					ProjectID:         "e2e-project",
					Region:            "us-central1",
					ModelProvider:     agenticv1alpha1.GoogleCloudVertexModelProviderAnthropic,
				},
			},
		},
		Agent: &agenticv1alpha1.Agent{
			ObjectMeta: metav1.ObjectMeta{Name: "e2e-agent"},
			Spec: agenticv1alpha1.AgentSpec{
				LLMProvider: agenticv1alpha1.LLMProviderReference{Name: "e2e-llm"},
				Model:       "claude-opus-4-6",
			},
		},
		Policy: &agenticv1alpha1.ApprovalPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: "cluster"},
			Spec: agenticv1alpha1.ApprovalPolicySpec{
				Stages: []agenticv1alpha1.ApprovalPolicyStage{
					{Name: agenticv1alpha1.SandboxStepAnalysis, Approval: agenticv1alpha1.ApprovalModeAutomatic},
				},
			},
		},
		LLMSecret: &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "e2e-llm-secret", Namespace: testNS},
			Data:       map[string][]byte{"credentials.json": []byte(`{"fake":"creds"}`)},
		},
	}

	// Ensure target namespace exists (used by RBAC tests).
	stagingNS := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "staging"}}
	if err := c.Create(ctx, stagingNS); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create staging namespace: %v", err)
	}

	all := []client.Object{f.LLM, f.Agent, f.Policy, f.LLMSecret}
	cleanup(t, c, all...)
	for _, obj := range all {
		obj.SetResourceVersion("")
		obj.SetUID("")
		if err := c.Create(ctx, obj); err != nil {
			t.Fatalf("create %T %s: %v", obj, obj.GetName(), err)
		}
	}
	t.Cleanup(func() { cleanup(t, c, all...) })
	return f
}

func createRealProviderFixtures(t *testing.T, c client.Client) *e2eFixtures {
	t.Helper()
	ctx := context.Background()

	provider := os.Getenv("E2E_PROVIDER")
	model := os.Getenv("E2E_MODEL")
	keyPath := os.Getenv("E2E_PROVIDER_KEY_PATH")

	if model == "" || keyPath == "" {
		t.Fatalf("E2E_PROVIDER=%s requires E2E_MODEL and E2E_PROVIDER_KEY_PATH", provider)
	}

	creds, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatalf("read credentials file %s: %v", keyPath, err)
	}

	secretName := fmt.Sprintf("e2e-%s-secret", provider)
	llmName := fmt.Sprintf("e2e-%s-llm", provider)

	var llmSpec agenticv1alpha1.LLMProviderSpec
	var secretData map[string][]byte

	switch provider {
	case "claude", "gemini":
		projectID := os.Getenv("VERTEX_PROJECT_ID")
		region := os.Getenv("VERTEX_REGION")
		if projectID == "" {
			t.Fatal("VERTEX_PROJECT_ID must be set for claude/gemini providers")
		}
		if region == "" {
			region = "us-central1"
		}

		var modelProvider agenticv1alpha1.GoogleCloudVertexModelProvider
		switch {
		case strings.HasPrefix(model, "claude"):
			modelProvider = agenticv1alpha1.GoogleCloudVertexModelProviderAnthropic
		case strings.HasPrefix(model, "gemini"):
			modelProvider = agenticv1alpha1.GoogleCloudVertexModelProviderGoogle
		default:
			t.Fatalf("cannot infer modelProvider from model %q", model)
		}

		secretData = map[string][]byte{"GOOGLE_APPLICATION_CREDENTIALS": creds}
		llmSpec = agenticv1alpha1.LLMProviderSpec{
			Type: agenticv1alpha1.LLMProviderGoogleCloudVertex,
			GoogleCloudVertex: agenticv1alpha1.GoogleCloudVertexConfig{
				CredentialsSecret: agenticv1alpha1.SecretReference{Name: secretName},
				ProjectID:         projectID,
				Region:            region,
				ModelProvider:     modelProvider,
			},
		}

	case "openai":
		secretData = map[string][]byte{"OPENAI_API_KEY": creds}
		llmSpec = agenticv1alpha1.LLMProviderSpec{
			Type: agenticv1alpha1.LLMProviderOpenAI,
			OpenAI: agenticv1alpha1.OpenAIConfig{
				CredentialsSecret: agenticv1alpha1.SecretReference{Name: secretName},
			},
		}

	default:
		t.Fatalf("unsupported E2E_PROVIDER: %s", provider)
	}

	f := &e2eFixtures{
		LLM: &agenticv1alpha1.LLMProvider{
			ObjectMeta: metav1.ObjectMeta{Name: llmName},
			Spec:       llmSpec,
		},
		Agent: &agenticv1alpha1.Agent{
			ObjectMeta: metav1.ObjectMeta{Name: "e2e-agent"},
			Spec: agenticv1alpha1.AgentSpec{
				LLMProvider: agenticv1alpha1.LLMProviderReference{Name: llmName},
				Model:       model,
			},
		},
		Policy: &agenticv1alpha1.ApprovalPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: "cluster"},
			Spec: agenticv1alpha1.ApprovalPolicySpec{
				Stages: []agenticv1alpha1.ApprovalPolicyStage{
					{Name: agenticv1alpha1.SandboxStepAnalysis, Approval: agenticv1alpha1.ApprovalModeAutomatic},
				},
			},
		},
		LLMSecret: &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: testNS},
			Data:       secretData,
		},
	}

	stagingNS := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "staging"}}
	if err := c.Create(ctx, stagingNS); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create staging namespace: %v", err)
	}

	all := []client.Object{f.LLM, f.Agent, f.Policy, f.LLMSecret}
	cleanup(t, c, all...)
	for _, obj := range all {
		obj.SetResourceVersion("")
		obj.SetUID("")
		if err := c.Create(ctx, obj); err != nil {
			t.Fatalf("create %T %s: %v", obj, obj.GetName(), err)
		}
	}
	t.Cleanup(func() { cleanup(t, c, all...) })

	t.Logf("Real provider fixtures created: provider=%s model=%s llm=%s", provider, model, llmName)
	return f
}

// createAgenticRun creates a AgenticRun + pre-created AgenticRunApproval (CEL workaround).
// Cleans up leftovers from previous runs. Returns the created AgenticRun.
func createAgenticRun(t *testing.T, c client.Client, name string) *agenticv1alpha1.AgenticRun {
	t.Helper()
	ctx := context.Background()

	prop := &agenticv1alpha1.AgenticRun{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNS},
		Spec: agenticv1alpha1.AgenticRunSpec{
			Request:          "Pod crash-looping in staging namespace",
			TargetNamespaces: []string{"staging"},
			Tools:            agenticv1alpha1.ToolsSpec{Skills: []agenticv1alpha1.SkillsSource{{Image: "quay.io/openshift-lightspeed/ols-qe:lightspeed-mock-agent", Paths: []string{"/skills"}}}},
			Analysis:         agenticv1alpha1.AgenticRunStep{Agent: "e2e-agent"},
			Execution:        agenticv1alpha1.AgenticRunStep{Agent: "e2e-agent"},
			Verification:     agenticv1alpha1.AgenticRunStep{Agent: "e2e-agent"},
		},
	}

	// Clean leftovers from previous runs (runs, approvals, sandbox claims, bare pods).
	cleanup(t, c, &agenticv1alpha1.AgenticRun{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNS}})
	cleanup(t, c, &agenticv1alpha1.AgenticRunApproval{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNS}})
	deleteSandboxClaim(t, c, "ls-analysis-"+name, testNS)
	deleteSandboxClaim(t, c, "ls-execution-"+name, testNS)
	deleteSandboxClaim(t, c, "ls-verification-"+name, testNS)
	deleteBarePod(t, c, "ls-analysis-"+name)
	deleteBarePod(t, c, "ls-execution-"+name)
	deleteBarePod(t, c, "ls-verification-"+name)

	if err := c.Create(ctx, prop); err != nil {
		t.Fatalf("create AgenticRun: %v", err)
	}
	t.Cleanup(func() { cleanup(t, c, prop) })

	return prop
}

// waitForPhase polls until the AgenticRun reaches the target phase or times out.
func waitForPhase(t *testing.T, c client.Client, name string, target agenticv1alpha1.AgenticRunPhase) agenticv1alpha1.AgenticRun {
	t.Helper()
	ctx := context.Background()
	var updated agenticv1alpha1.AgenticRun

	err := wait.PollUntilContextTimeout(ctx, pollInterval, pollTimeout, true, func(ctx context.Context) (bool, error) {
		if err := c.Get(ctx, types.NamespacedName{Name: name, Namespace: testNS}, &updated); err != nil {
			return false, nil
		}
		phase := agenticv1alpha1.DerivePhase(updated.Status.Conditions)
		t.Logf("polling %s: phase=%s conditions=%d", name, phase, len(updated.Status.Conditions))
		return phase == target, nil
	})
	if err != nil {
		phase := agenticv1alpha1.DerivePhase(updated.Status.Conditions)
		t.Fatalf("timed out waiting for phase %s; current=%s conditions=%v", target, phase, updated.Status.Conditions)
	}
	return updated
}

// waitForDeletion polls until the AgenticRun is gone (finalizer completed).
func waitForDeletion(t *testing.T, c client.Client, name string) {
	t.Helper()
	ctx := context.Background()

	err := wait.PollUntilContextTimeout(ctx, pollInterval, pollTimeout, true, func(ctx context.Context) (bool, error) {
		var gone agenticv1alpha1.AgenticRun
		if err := c.Get(ctx, types.NamespacedName{Name: name, Namespace: testNS}, &gone); err != nil {
			return true, nil
		}
		return false, nil
	})
	if err != nil {
		t.Fatalf("timed out waiting for AgenticRun %s deletion (finalizer may be stuck)", name)
	}
}

// denyStage patches the AgenticRunApproval to deny the given stage.
func denyStage(t *testing.T, c client.Client, name string, stageType agenticv1alpha1.ApprovalStageType) {
	t.Helper()
	ctx := context.Background()

	var approval agenticv1alpha1.AgenticRunApproval
	if err := c.Get(ctx, types.NamespacedName{Name: name, Namespace: testNS}, &approval); err != nil {
		t.Fatalf("get AgenticRunApproval for denial: %v", err)
	}

	base := approval.DeepCopy()
	found := false
	for i, s := range approval.Spec.Stages {
		if s.Type == stageType {
			approval.Spec.Stages[i].Decision = agenticv1alpha1.ApprovalDecisionDenied
			found = true
			break
		}
	}
	if !found {
		stage := agenticv1alpha1.ApprovalStage{
			Type:     stageType,
			Decision: agenticv1alpha1.ApprovalDecisionDenied,
		}
		switch stageType {
		case agenticv1alpha1.ApprovalStageAnalysis:
			stage.Analysis = &agenticv1alpha1.AnalysisApproval{Agent: "e2e-agent"}
		case agenticv1alpha1.ApprovalStageExecution:
			stage.Execution = &agenticv1alpha1.ExecutionApproval{Agent: "e2e-agent"}
		case agenticv1alpha1.ApprovalStageVerification:
			stage.Verification = &agenticv1alpha1.VerificationApproval{Agent: "e2e-agent"}
		case agenticv1alpha1.ApprovalStageEscalation:
			stage.Escalation = &agenticv1alpha1.EscalationApproval{Agent: "e2e-agent"}
		}
		approval.Spec.Stages = append(approval.Spec.Stages, stage)
	}
	if err := c.Patch(ctx, &approval, client.MergeFrom(base)); err != nil {
		t.Fatalf("deny stage %s: %v", stageType, err)
	}
	t.Logf("denied stage %s", stageType)
}

// approveExecution patches the AgenticRunApproval to approve execution with the given option index.
func approveExecution(t *testing.T, c client.Client, name string, optionIdx int32) {
	t.Helper()
	ctx := context.Background()

	var approval agenticv1alpha1.AgenticRunApproval
	if err := c.Get(ctx, types.NamespacedName{Name: name, Namespace: testNS}, &approval); err != nil {
		t.Fatalf("get AgenticRunApproval for execution approval: %v", err)
	}

	base := approval.DeepCopy()
	found := false
	for i, s := range approval.Spec.Stages {
		if s.Type == agenticv1alpha1.ApprovalStageExecution {
			approval.Spec.Stages[i].Execution = &agenticv1alpha1.ExecutionApproval{
				Agent:  "e2e-agent",
				Option: ptrInt32(optionIdx),
			}
			found = true
			break
		}
	}
	if !found {
		approval.Spec.Stages = append(approval.Spec.Stages, agenticv1alpha1.ApprovalStage{
			Type:      agenticv1alpha1.ApprovalStageExecution,
			Execution: &agenticv1alpha1.ExecutionApproval{Agent: "e2e-agent", Option: ptrInt32(optionIdx)},
		})
	}
	if err := c.Patch(ctx, &approval, client.MergeFrom(base)); err != nil {
		t.Fatalf("approve execution: %v", err)
	}
	t.Logf("approved execution with option %d", optionIdx)
}

// approveVerification patches the AgenticRunApproval to approve verification.
func approveVerification(t *testing.T, c client.Client, name string) {
	t.Helper()
	ctx := context.Background()

	var approval agenticv1alpha1.AgenticRunApproval
	if err := c.Get(ctx, types.NamespacedName{Name: name, Namespace: testNS}, &approval); err != nil {
		t.Fatalf("get AgenticRunApproval for verification approval: %v", err)
	}

	base := approval.DeepCopy()
	found := false
	for i, s := range approval.Spec.Stages {
		if s.Type == agenticv1alpha1.ApprovalStageVerification {
			approval.Spec.Stages[i].Verification = &agenticv1alpha1.VerificationApproval{Agent: "e2e-agent"}
			found = true
			break
		}
	}
	if !found {
		approval.Spec.Stages = append(approval.Spec.Stages, agenticv1alpha1.ApprovalStage{
			Type:         agenticv1alpha1.ApprovalStageVerification,
			Verification: &agenticv1alpha1.VerificationApproval{Agent: "e2e-agent"},
		})
	}
	if err := c.Patch(ctx, &approval, client.MergeFrom(base)); err != nil {
		t.Fatalf("approve verification: %v", err)
	}
	t.Logf("approved verification")
}
