//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
)

const (
	pollInterval = 2 * time.Second
	pollTimeout  = 10 * time.Minute
	testNS       = "default"
)

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
	_ = agenticv1alpha1.AddToScheme(s)

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

// --- Fixture builders ---

// e2eFixtures holds the prerequisite CRs needed for any proposal flow.
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
					{Name: agenticv1alpha1.SandboxStepVerification, Approval: agenticv1alpha1.ApprovalModeAutomatic},
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
	_ = c.Create(ctx, stagingNS) // ignore AlreadyExists

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

// createProposal creates a Proposal + pre-created ProposalApproval (CEL workaround).
// Cleans up leftovers from previous runs. Returns the created Proposal.
func createProposal(t *testing.T, c client.Client, name string) *agenticv1alpha1.Proposal {
	t.Helper()
	ctx := context.Background()

	prop := &agenticv1alpha1.Proposal{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNS},
		Spec: agenticv1alpha1.ProposalSpec{
			Request:          "Pod crash-looping in staging namespace",
			TargetNamespaces: []string{"staging"},
			Tools:            agenticv1alpha1.ToolsSpec{Skills: []agenticv1alpha1.SkillsSource{{Image: "quay.io/openshift-lightspeed/ols-qe:lightspeed-mock-agent", Paths: []string{"/skills"}}}},
			Analysis:         agenticv1alpha1.ProposalStep{Agent: "e2e-agent"},
			Execution:        agenticv1alpha1.ProposalStep{Agent: "e2e-agent"},
			Verification:     agenticv1alpha1.ProposalStep{Agent: "e2e-agent"},
		},
	}

	// Clean leftovers.
	cleanup(t, c, &agenticv1alpha1.Proposal{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNS}})
	cleanup(t, c, &agenticv1alpha1.ProposalApproval{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNS}})
	deleteSandboxClaim(t, c, "ls-analysis-"+name, testNS)
	deleteSandboxClaim(t, c, "ls-execution-"+name, testNS)
	deleteSandboxClaim(t, c, "ls-verification-"+name, testNS)

	if err := c.Create(ctx, prop); err != nil {
		t.Fatalf("create Proposal: %v", err)
	}
	t.Cleanup(func() { cleanup(t, c, prop) })

	return prop
}

// waitForPhase polls until the Proposal reaches the target phase or times out.
func waitForPhase(t *testing.T, c client.Client, name string, target agenticv1alpha1.ProposalPhase) agenticv1alpha1.Proposal {
	t.Helper()
	ctx := context.Background()
	var updated agenticv1alpha1.Proposal

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

// waitForDeletion polls until the Proposal is gone (finalizer completed).
func waitForDeletion(t *testing.T, c client.Client, name string) {
	t.Helper()
	ctx := context.Background()

	err := wait.PollUntilContextTimeout(ctx, pollInterval, pollTimeout, true, func(ctx context.Context) (bool, error) {
		var gone agenticv1alpha1.Proposal
		if err := c.Get(ctx, types.NamespacedName{Name: name, Namespace: testNS}, &gone); err != nil {
			return true, nil
		}
		return false, nil
	})
	if err != nil {
		t.Fatalf("timed out waiting for Proposal %s deletion (finalizer may be stuck)", name)
	}
}

// denyStage patches the ProposalApproval to deny the given stage.
func denyStage(t *testing.T, c client.Client, name string, stageType agenticv1alpha1.ApprovalStageType) {
	t.Helper()
	ctx := context.Background()

	var approval agenticv1alpha1.ProposalApproval
	if err := c.Get(ctx, types.NamespacedName{Name: name, Namespace: testNS}, &approval); err != nil {
		t.Fatalf("get ProposalApproval for denial: %v", err)
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
		case agenticv1alpha1.ApprovalStageExecution:
			stage.Execution = agenticv1alpha1.ExecutionApproval{Agent: "e2e-agent"}
		case agenticv1alpha1.ApprovalStageVerification:
			stage.Verification = agenticv1alpha1.VerificationApproval{Agent: "e2e-agent"}
		case agenticv1alpha1.ApprovalStageEscalation:
			stage.Escalation = agenticv1alpha1.EscalationApproval{Agent: "e2e-agent"}
		}
		approval.Spec.Stages = append(approval.Spec.Stages, stage)
	}
	if err := c.Patch(ctx, &approval, client.MergeFrom(base)); err != nil {
		t.Fatalf("deny stage %s: %v", stageType, err)
	}
	t.Logf("denied stage %s", stageType)
}

// approveExecution patches the ProposalApproval to approve execution with the given option index.
func approveExecution(t *testing.T, c client.Client, name string, optionIdx int32) {
	t.Helper()
	ctx := context.Background()

	var approval agenticv1alpha1.ProposalApproval
	if err := c.Get(ctx, types.NamespacedName{Name: name, Namespace: testNS}, &approval); err != nil {
		t.Fatalf("get ProposalApproval for execution approval: %v", err)
	}

	base := approval.DeepCopy()
	found := false
	for i, s := range approval.Spec.Stages {
		if s.Type == agenticv1alpha1.ApprovalStageExecution {
			approval.Spec.Stages[i].Execution = agenticv1alpha1.ExecutionApproval{
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
			Execution: agenticv1alpha1.ExecutionApproval{Agent: "e2e-agent", Option: ptrInt32(optionIdx)},
		})
	}
	if err := c.Patch(ctx, &approval, client.MergeFrom(base)); err != nil {
		t.Fatalf("approve execution: %v", err)
	}
	t.Logf("approved execution with option %d", optionIdx)
}
