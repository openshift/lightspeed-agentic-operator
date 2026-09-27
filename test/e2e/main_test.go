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
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
)

var suiteClient client.Client

func TestMain(m *testing.M) {
	var err error
	suiteClient, err = buildClient()
	if err != nil {
		fmt.Fprintf(os.Stderr, "FATAL: build client: %v\n", err)
		os.Exit(1)
	}

	if err := setupSuiteFixtures(suiteClient); err != nil {
		fmt.Fprintf(os.Stderr, "FATAL: setup fixtures: %v\n", err)
		os.Exit(1)
	}

	code := m.Run()

	ctx := context.Background()
	for _, obj := range fixtureStubs() {
		_ = suiteClient.Delete(ctx, obj)
	}

	os.Exit(code)
}

// fixtureStubs returns lightweight stubs (name/namespace only) for teardown.
func fixtureStubs() []client.Object {
	provider := os.Getenv("E2E_PROVIDER")
	if provider == "" {
		return []client.Object{
			&agenticv1alpha1.LLMProvider{ObjectMeta: metav1.ObjectMeta{Name: "e2e-llm"}},
			&agenticv1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "e2e-agent"}},
			&agenticv1alpha1.ApprovalPolicy{ObjectMeta: metav1.ObjectMeta{Name: "cluster"}},
			&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "e2e-llm-secret", Namespace: testNS}},
		}
	}
	llmName := fmt.Sprintf("e2e-%s-llm", provider)
	secretName := fmt.Sprintf("e2e-%s-secret", provider)
	return []client.Object{
		&agenticv1alpha1.LLMProvider{ObjectMeta: metav1.ObjectMeta{Name: llmName}},
		&agenticv1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "e2e-agent"}},
		&agenticv1alpha1.ApprovalPolicy{ObjectMeta: metav1.ObjectMeta{Name: "cluster"}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: testNS}},
	}
}

// --- Suite-level fixture setup ---

func setupSuiteFixtures(c client.Client) error {
	ctx := context.Background()

	stagingNS := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "staging"}}
	if err := c.Create(ctx, stagingNS); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create staging namespace: %w", err)
	}

	if os.Getenv("E2E_PROVIDER") != "" {
		return setupRealProviderFixtures(c)
	}
	return setupDefaultFixtures(c)
}

func deleteAndWait(ctx context.Context, c client.Client, obj client.Object) {
	key := types.NamespacedName{Name: obj.GetName(), Namespace: obj.GetNamespace()}
	if err := c.Get(ctx, key, obj); apierrors.IsNotFound(err) {
		return
	}
	_ = c.Delete(ctx, obj)
	_ = wait.PollUntilContextTimeout(ctx, 500*time.Millisecond, 30*time.Second, true, func(ctx context.Context) (bool, error) {
		return c.Get(ctx, key, obj) != nil, nil
	})
}

func setupDefaultFixtures(c client.Client) error {
	ctx := context.Background()

	fixtures := []client.Object{
		&agenticv1alpha1.LLMProvider{
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
		&agenticv1alpha1.Agent{
			ObjectMeta: metav1.ObjectMeta{Name: "e2e-agent"},
			Spec: agenticv1alpha1.AgentSpec{
				LLMProvider: agenticv1alpha1.LLMProviderReference{Name: "e2e-llm"},
				Model:       "claude-opus-4-6",
			},
		},
		&agenticv1alpha1.ApprovalPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: "cluster"},
			Spec: agenticv1alpha1.ApprovalPolicySpec{
				Stages: []agenticv1alpha1.ApprovalPolicyStage{
					{Name: agenticv1alpha1.SandboxStepAnalysis, Approval: agenticv1alpha1.ApprovalModeAutomatic},
				},
			},
		},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "e2e-llm-secret", Namespace: testNS},
			Data:       map[string][]byte{"credentials.json": []byte(`{"fake":"creds"}`)},
		},
	}

	for _, obj := range fixtures {
		deleteAndWait(ctx, c, obj)
	}
	for _, obj := range fixtures {
		obj.SetResourceVersion("")
		obj.SetUID("")
		if err := c.Create(ctx, obj); err != nil {
			return fmt.Errorf("create %T %s: %w", obj, obj.GetName(), err)
		}
		fmt.Fprintf(os.Stderr, "suite: created %T/%s\n", obj, obj.GetName())
	}
	return nil
}

func setupRealProviderFixtures(c client.Client) error {
	ctx := context.Background()

	provider := os.Getenv("E2E_PROVIDER")
	model := os.Getenv("E2E_MODEL")
	keyPath := os.Getenv("E2E_PROVIDER_KEY_PATH")
	if model == "" || keyPath == "" {
		return fmt.Errorf("E2E_PROVIDER=%s requires E2E_MODEL and E2E_PROVIDER_KEY_PATH", provider)
	}

	creds, err := os.ReadFile(keyPath)
	if err != nil {
		return fmt.Errorf("read credentials %s: %w", keyPath, err)
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
			return fmt.Errorf("VERTEX_PROJECT_ID must be set for %s provider", provider)
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
			return fmt.Errorf("cannot infer modelProvider from model %q", model)
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
		return fmt.Errorf("unsupported E2E_PROVIDER: %s", provider)
	}

	fixtures := []client.Object{
		&agenticv1alpha1.LLMProvider{
			ObjectMeta: metav1.ObjectMeta{Name: llmName},
			Spec:       llmSpec,
		},
		&agenticv1alpha1.Agent{
			ObjectMeta: metav1.ObjectMeta{Name: "e2e-agent"},
			Spec: agenticv1alpha1.AgentSpec{
				LLMProvider: agenticv1alpha1.LLMProviderReference{Name: llmName},
				Model:       model,
			},
		},
		&agenticv1alpha1.ApprovalPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: "cluster"},
			Spec: agenticv1alpha1.ApprovalPolicySpec{
				Stages: []agenticv1alpha1.ApprovalPolicyStage{
					{Name: agenticv1alpha1.SandboxStepAnalysis, Approval: agenticv1alpha1.ApprovalModeAutomatic},
				},
			},
		},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: testNS},
			Data:       secretData,
		},
	}

	for _, obj := range fixtures {
		deleteAndWait(ctx, c, obj)
	}
	for _, obj := range fixtures {
		obj.SetResourceVersion("")
		obj.SetUID("")
		if err := c.Create(ctx, obj); err != nil {
			return fmt.Errorf("create %T %s: %w", obj, obj.GetName(), err)
		}
		fmt.Fprintf(os.Stderr, "suite: created %T/%s\n", obj, obj.GetName())
	}
	fmt.Fprintf(os.Stderr, "suite: real provider fixtures created: provider=%s model=%s llm=%s\n", provider, model, llmName)
	return nil
}
