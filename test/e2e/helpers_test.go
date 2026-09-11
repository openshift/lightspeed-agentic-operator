//go:build e2e || product_e2e

package e2e

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
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

func buildClient() (client.Client, error) {
	kubeconfig := os.Getenv("KUBECONFIG")
	if kubeconfig == "" {
		home, _ := os.UserHomeDir()
		kubeconfig = home + "/.kube/config"
	}

	cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("build kubeconfig: %w", err)
	}

	s := scheme.Scheme
	utilruntime.Must(agenticv1alpha1.AddToScheme(s))
	utilruntime.Must(admissionregistrationv1.AddToScheme(s))

	c, err := client.New(cfg, client.Options{Scheme: s})
	if err != nil {
		return nil, fmt.Errorf("create client: %w", err)
	}
	return c, nil
}

func newClient(t *testing.T) client.Client {
	t.Helper()
	c, err := buildClient()
	if err != nil {
		t.Fatalf("build client: %v", err)
	}
	return c
}

// --- Pointer helpers ---

func ptrBool(v bool) *bool    { return &v }
func ptrInt32(v int32) *int32 { return &v }

// --- Cleanup ---

// cleanupTimeout is how long cleanup waits for operator finalizers before
// force-stripping. Long enough for normal finalizer processing, short enough
// to not stall the suite if a finalizer is genuinely stuck.
const cleanupTimeout = 2 * time.Minute

// cleanup deletes objects, letting the operator's finalizers run naturally.
// It polls until the object is fully gone (using cleanupTimeout), so the
// next test never races against a lingering finalizer. Only force-strips
// finalizers as a last resort when the operator appears stuck.
func cleanup(t *testing.T, c client.Client, objs ...client.Object) {
	t.Helper()
	ctx := context.Background()

	for _, obj := range objs {
		kind := obj.GetObjectKind().GroupVersionKind().Kind
		if kind == "" {
			kind = fmt.Sprintf("%T", obj)
		}
		name := obj.GetName()
		key := types.NamespacedName{Name: name, Namespace: obj.GetNamespace()}

		if err := c.Get(ctx, key, obj); err != nil {
			t.Logf("cleanup: %s/%s not found (already clean)", kind, name)
			continue
		}

		_ = c.Delete(ctx, obj)

		err := wait.PollUntilContextTimeout(ctx, 1*time.Second, cleanupTimeout, true, func(ctx context.Context) (bool, error) {
			if err := c.Get(ctx, key, obj); err != nil {
				return true, nil
			}
			if obj.GetDeletionTimestamp() != nil && len(obj.GetFinalizers()) > 0 {
				t.Logf("cleanup: %s/%s waiting for finalizer %v", kind, name, obj.GetFinalizers())
			}
			return false, nil
		})
		if err == nil {
			t.Logf("cleanup: %s/%s deleted", kind, name)
			continue
		}

		if err := c.Get(ctx, key, obj); err != nil {
			t.Logf("cleanup: %s/%s deleted", kind, name)
			continue
		}
		if len(obj.GetFinalizers()) > 0 {
			t.Logf("cleanup: %s/%s force-stripping finalizers %v (operator did not process within %s)", kind, name, obj.GetFinalizers(), cleanupTimeout)
			obj.SetFinalizers(nil)
			_ = c.Update(ctx, obj)
		}
	}
}

// --- AgenticRun builder ---

// createAgenticRun creates a AgenticRun, cleans up leftovers from previous
// runs, and registers cleanup. Returns the created AgenticRun.
func createAgenticRun(t *testing.T, c client.Client, name string) *agenticv1alpha1.AgenticRun {
	t.Helper()
	return createAgenticRunWithRequest(t, c, name, "Pod crash-looping in staging namespace")
}

// createAgenticRunWithRequest is like createAgenticRun but allows a custom
// request string. Embed mock failure keywords (MOCK_CRASH, MOCK_TIMEOUT, etc.)
// in the request to trigger failure modes.
func createAgenticRunWithRequest(t *testing.T, c client.Client, name, request string) *agenticv1alpha1.AgenticRun {
	t.Helper()
	ctx := context.Background()

	prop := &agenticv1alpha1.AgenticRun{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNS},
		Spec: agenticv1alpha1.AgenticRunSpec{
			Request:          request,
			TargetNamespaces: []string{"staging"},
			Tools:            agenticv1alpha1.ToolsSpec{Skills: []agenticv1alpha1.SkillsSource{{Image: "quay.io/openshift-lightspeed/ols-qe:lightspeed-mock-agent", Paths: []string{"/skills"}}}},
			Analysis:         agenticv1alpha1.AgenticRunStep{Agent: "e2e-agent"},
			Execution:        agenticv1alpha1.AgenticRunStep{Agent: "e2e-agent"},
			Verification:     agenticv1alpha1.AgenticRunStep{Agent: "e2e-agent"},
		},
	}

	cleanup(t, c, &agenticv1alpha1.AgenticRun{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNS}})
	cleanup(t, c, &agenticv1alpha1.AgenticRunApproval{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNS}})

	if err := c.Create(ctx, prop); err != nil {
		t.Fatalf("create AgenticRun: %v", err)
	}
	t.Cleanup(func() { cleanup(t, c, prop) })

	return prop
}

// terminalPhases lists phases that will never transition further.
var terminalPhases = map[agenticv1alpha1.AgenticRunPhase]bool{
	agenticv1alpha1.AgenticRunPhaseCompleted:        true,
	agenticv1alpha1.AgenticRunPhaseFailed:           true,
	agenticv1alpha1.AgenticRunPhaseDenied:           true,
	agenticv1alpha1.AgenticRunPhaseEscalated:        true,
	agenticv1alpha1.AgenticRunPhaseEmergencyStopped: true,
}

// waitForPhase polls until the AgenticRun reaches the target phase. It fails
// immediately if the run reaches a different terminal phase, since terminal
// phases never transition further.
func waitForPhase(t *testing.T, c client.Client, name string, target agenticv1alpha1.AgenticRunPhase) agenticv1alpha1.AgenticRun {
	t.Helper()
	return waitForPhaseWithTimeout(t, c, name, target, pollTimeout)
}

func waitForPhaseWithTimeout(t *testing.T, c client.Client, name string, target agenticv1alpha1.AgenticRunPhase, timeout time.Duration) agenticv1alpha1.AgenticRun {
	t.Helper()
	ctx := context.Background()
	var updated agenticv1alpha1.AgenticRun

	err := wait.PollUntilContextTimeout(ctx, pollInterval, timeout, true, func(ctx context.Context) (bool, error) {
		if err := c.Get(ctx, types.NamespacedName{Name: name, Namespace: testNS}, &updated); err != nil {
			return false, nil
		}
		phase := agenticv1alpha1.DerivePhase(updated.Status.Conditions)
		t.Logf("polling %s: phase=%s conditions=%d", name, phase, len(updated.Status.Conditions))
		if phase == target {
			return true, nil
		}
		if terminalPhases[phase] {
			return false, fmt.Errorf("reached terminal phase %s while waiting for %s", phase, target)
		}
		return false, nil
	})
	if err != nil {
		phase := agenticv1alpha1.DerivePhase(updated.Status.Conditions)
		t.Fatalf("waiting for phase %s failed: %v; current=%s conditions=%v", target, err, phase, updated.Status.Conditions)
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

// --- OTEL trace verification ---

const otelCollectorLabelSelector = "app=lightspeed-otel-collector"

// collectorLogs returns the OTEL collector pod logs, or empty string
// (with t.Skip) if the collector is not deployed.
func collectorLogs(t *testing.T) string {
	t.Helper()

	kubeconfig := os.Getenv("KUBECONFIG")
	if kubeconfig == "" {
		home, _ := os.UserHomeDir()
		kubeconfig = home + "/.kube/config"
	}
	cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		t.Fatalf("build kubeconfig for pod logs: %v", err)
	}
	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatalf("create kubernetes clientset: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pods, err := clientset.CoreV1().Pods(testNS).List(ctx, metav1.ListOptions{
		LabelSelector: otelCollectorLabelSelector,
	})
	if err != nil {
		t.Fatalf("list OTEL collector pods: %v", err)
	}
	var podName string
	for _, p := range pods.Items {
		if p.Status.Phase == corev1.PodRunning && p.DeletionTimestamp == nil {
			podName = p.Name
			break
		}
	}
	if podName == "" {
		t.Skip("OTEL collector not deployed, skipping trace assertion")
		return ""
	}
	req := clientset.CoreV1().Pods(testNS).GetLogs(podName, &corev1.PodLogOptions{})
	stream, err := req.Stream(ctx)
	if err != nil {
		t.Fatalf("stream collector logs: %v", err)
	}
	defer stream.Close()

	var buf bytes.Buffer
	if _, err := buf.ReadFrom(stream); err != nil {
		t.Fatalf("read collector logs: %v", err)
	}
	return buf.String()
}

// assertTracesExported verifies that the OTEL collector received spans
// for the given AgenticRun (matched by UID) and that each of the
// expected span names appears in a span block that also carries this
// run's UID attribute. Retries for up to 30 seconds to allow the batch
// span processor to flush recently completed spans.
func assertTracesExported(t *testing.T, run *agenticv1alpha1.AgenticRun, expectedSpans []string) {
	t.Helper()

	uid := string(run.UID)
	if uid == "" {
		t.Fatal("AgenticRun has no UID, cannot verify traces")
	}

	uidNeedle := fmt.Sprintf("agenticrun.uid: Str(%s)", uid)
	var missing []string

	err := wait.PollUntilContextTimeout(context.Background(), 5*time.Second, 30*time.Second, true, func(_ context.Context) (bool, error) {
		logs := collectorLogs(t)
		if !strings.Contains(logs, uidNeedle) {
			return false, nil
		}

		blocks := strings.Split(logs, "Span #")
		missing = nil
		for _, span := range expectedSpans {
			nameNeedle := fmt.Sprintf("Name           : %s", span)
			found := false
			for _, block := range blocks {
				if strings.Contains(block, uidNeedle) && strings.Contains(block, nameNeedle) {
					found = true
					break
				}
			}
			if !found {
				missing = append(missing, span)
			}
		}
		return len(missing) == 0, nil
	})

	if err != nil {
		if !strings.Contains(collectorLogs(t), uidNeedle) {
			t.Errorf("OTEL collector logs do not contain any spans for run UID %s", uid)
			return
		}
		for _, span := range missing {
			t.Errorf("OTEL collector logs missing span %q for run %s (UID %s)", span, run.Name, uid)
		}
	}

	for _, span := range expectedSpans {
		found := true
		for _, m := range missing {
			if m == span {
				found = false
				break
			}
		}
		if found {
			t.Logf("Verified: span %q present for UID %s", span, uid)
		}
	}
}

// --- Evals YAML parsing ---

type evalsEntry struct {
	ConversationGroupID string      `yaml:"conversation_group_id"`
	Tag                 []string    `yaml:"tag"`
	Turns               []evalsTurn `yaml:"turns"`
}

type evalsTurn struct {
	ProposalSpec   evalsProposalSpec   `yaml:"openshift_agentic_run_spec"`
	ExpectedStatus evalsExpectedStatus `yaml:"expected_openshift_agentic_run_status"`
}

type evalsExpectedStatus struct {
	Phase string `yaml:"phase"`
}

type evalsProposalSpec struct {
	Request          string         `yaml:"request"`
	TargetNamespaces []string       `yaml:"targetNamespaces"`
	Tools            evalsToolsSpec `yaml:"tools"`
}

type evalsToolsSpec struct {
	Skills []evalsSkill `yaml:"skills"`
}

type evalsSkill struct {
	Image string   `yaml:"image"`
	Paths []string `yaml:"paths"`
}

var providerSuffix = map[string]string{
	"claude": "_anthropic",
	"gemini": "_gemini",
	"openai": "_openai",
}

// discoveredScenario is a scenario discovered from the troubleshooting-scenarios repo.
type discoveredScenario struct {
	Name          string
	Dir           string
	Spec          evalsProposalSpec
	ExpectedPhase agenticv1alpha1.AgenticRunPhase
}

// discoverScenarios walks E2E_SCENARIOS_DIR/agentic/ and returns scenarios
// whose evals.yaml contains all the required tags. Tags default to ["core"]
// and can be overridden via E2E_SCENARIO_TAGS (comma-separated).
//
// Known tags in rhobs/troubleshooting-scenarios (see its README for updates):
//   - agentic: all agentic scenarios
//   - core: curated subset for benchmarking (~17 scenarios)
//   - alert: scenarios triggered by alerts
//   - difficulty_normal, difficulty_medium, difficulty_hard
func discoverScenarios(t *testing.T, scenariosDir string) []discoveredScenario {
	t.Helper()

	tags := []string{"core"}
	if v := os.Getenv("E2E_SCENARIO_TAGS"); v != "" {
		tags = splitCSVTrim(v)
	}

	skipSet := make(map[string]bool)
	if v := os.Getenv("E2E_SKIP_SCENARIOS"); v != "" {
		for _, s := range splitCSVTrim(v) {
			skipSet[s] = true
		}
	}

	agenticDir := filepath.Join(scenariosDir, "agentic")
	entries, err := os.ReadDir(agenticDir)
	if err != nil {
		t.Fatalf("read scenarios dir %s: %v", agenticDir, err)
	}

	var scenarios []discoveredScenario
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), "_") || entry.Name() == "results" {
			continue
		}
		if skipSet[entry.Name()] {
			t.Logf("skip %s: listed in E2E_SKIP_SCENARIOS", entry.Name())
			continue
		}
		dir := filepath.Join(agenticDir, entry.Name())

		evalsPath := filepath.Join(dir, "evals.yaml")
		if _, err := os.Stat(evalsPath); err != nil {
			continue
		}
		if _, err := os.Stat(filepath.Join(dir, "setup.sh")); err != nil {
			continue
		}

		data, err := os.ReadFile(evalsPath)
		if err != nil {
			t.Logf("skip %s: read evals.yaml: %v", entry.Name(), err)
			continue
		}
		var evals []evalsEntry
		if err := yaml.Unmarshal(data, &evals); err != nil {
			t.Logf("skip %s: parse evals.yaml: %v", entry.Name(), err)
			continue
		}
		if len(evals) == 0 || len(evals[0].Turns) == 0 {
			continue
		}

		if !hasAllTags(evals[0].Tag, tags) {
			continue
		}

		turn := selectEvalsTurn(t, evals)
		expectedPhase := agenticv1alpha1.AgenticRunPhase(turn.ExpectedStatus.Phase)
		if expectedPhase == "" {
			expectedPhase = agenticv1alpha1.AgenticRunPhaseCompleted
		}
		scenarios = append(scenarios, discoveredScenario{
			Name:          entry.Name(),
			Dir:           dir,
			Spec:          turn.ProposalSpec,
			ExpectedPhase: expectedPhase,
		})
	}

	if len(scenarios) == 0 {
		t.Fatalf("no scenarios found matching tags %v in %s", tags, agenticDir)
	}
	t.Logf("Discovered %d scenarios matching tags %v", len(scenarios), tags)
	return scenarios
}

func splitCSVTrim(v string) []string {
	var out []string
	for _, item := range strings.Split(v, ",") {
		item = strings.TrimSpace(item)
		if item != "" {
			out = append(out, item)
		}
	}
	return out
}

func hasAllTags(scenarioTags, required []string) bool {
	tagSet := make(map[string]bool, len(scenarioTags))
	for _, t := range scenarioTags {
		tagSet[t] = true
	}
	for _, r := range required {
		if !tagSet[r] {
			return false
		}
	}
	return true
}

func selectEvalsTurn(t *testing.T, entries []evalsEntry) evalsTurn {
	t.Helper()
	provider := os.Getenv("E2E_PROVIDER")
	suffix := providerSuffix[provider]
	for _, e := range entries {
		if suffix != "" && strings.HasSuffix(e.ConversationGroupID, suffix) && len(e.Turns) > 0 {
			return e.Turns[0]
		}
	}
	for _, e := range entries {
		if len(e.Turns) > 0 {
			return e.Turns[0]
		}
	}
	t.Fatalf("evals.yaml contains no turns")
	return evalsTurn{}
}

// e2eFixtures holds the per-test fixture objects so callers can patch or reference them.
type e2eFixtures struct {
	LLM    *agenticv1alpha1.LLMProvider
	Agent  *agenticv1alpha1.Agent
	Policy *agenticv1alpha1.ApprovalPolicy
	Secret *corev1.Secret
}

// createRealProviderFixtures creates LLMProvider, Agent, ApprovalPolicy, and Secret
// for a real provider (E2E_PROVIDER) with t.Cleanup teardown.
func createRealProviderFixtures(t *testing.T, c client.Client) *e2eFixtures {
	t.Helper()
	ctx := context.Background()

	provider := os.Getenv("E2E_PROVIDER")
	model := os.Getenv("E2E_MODEL")
	keyPath := os.Getenv("E2E_PROVIDER_KEY_PATH")
	if provider == "" || model == "" || keyPath == "" {
		t.Fatalf("E2E_PROVIDER, E2E_MODEL, E2E_PROVIDER_KEY_PATH must all be set")
	}

	creds, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatalf("read credentials %s: %v", keyPath, err)
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
			t.Fatalf("VERTEX_PROJECT_ID must be set for %s provider", provider)
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
		secretData = map[string][]byte{"OPENAI_API_KEY": []byte(strings.TrimSpace(string(creds)))}
		llmSpec = agenticv1alpha1.LLMProviderSpec{
			Type: agenticv1alpha1.LLMProviderOpenAI,
			OpenAI: agenticv1alpha1.OpenAIConfig{
				CredentialsSecret: agenticv1alpha1.SecretReference{Name: secretName},
			},
		}
	default:
		t.Fatalf("unsupported E2E_PROVIDER: %s", provider)
	}

	llm := &agenticv1alpha1.LLMProvider{
		ObjectMeta: metav1.ObjectMeta{Name: llmName},
		Spec:       llmSpec,
	}
	agent := &agenticv1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "e2e-agent"},
		Spec: agenticv1alpha1.AgentSpec{
			LLMProvider: agenticv1alpha1.LLMProviderReference{Name: llmName},
			Model:       model,
		},
	}
	policy := &agenticv1alpha1.ApprovalPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster"},
		Spec: agenticv1alpha1.ApprovalPolicySpec{
			Stages: []agenticv1alpha1.ApprovalPolicyStage{
				{Name: agenticv1alpha1.SandboxStepAnalysis, Approval: agenticv1alpha1.ApprovalModeAutomatic},
			},
		},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: testNS},
		Data:       secretData,
	}

	objs := []client.Object{llm, agent, policy, secret}
	for _, obj := range objs {
		cleanup(t, c, obj.DeepCopyObject().(client.Object))
	}
	for _, obj := range objs {
		obj.SetResourceVersion("")
		obj.SetUID("")
		if err := c.Create(ctx, obj); err != nil {
			t.Fatalf("create %T/%s: %v", obj, obj.GetName(), err)
		}
	}
	t.Cleanup(func() {
		for _, obj := range objs {
			cleanup(t, c, obj)
		}
	})

	t.Logf("Real provider fixtures created: provider=%s model=%s llm=%s", provider, model, llmName)
	return &e2eFixtures{LLM: llm, Agent: agent, Policy: policy, Secret: secret}
}

// ensureCrashLoopPod creates the crash-loop pod if it doesn't already exist.
func ensureCrashLoopPod(t *testing.T, c client.Client) {
	t.Helper()
	ctx := context.Background()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "e2e-crasher", Namespace: "staging", Labels: map[string]string{"app": "e2e-crasher"}},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyAlways,
			Containers:    []corev1.Container{{Name: "crasher", Image: "busybox:latest", Command: []string{"sh", "-c", "exit 1"}}},
		},
	}
	if err := c.Create(ctx, pod); err != nil {
		var existing corev1.Pod
		if getErr := c.Get(ctx, types.NamespacedName{Name: pod.Name, Namespace: pod.Namespace}, &existing); getErr != nil {
			t.Fatalf("create crash-loop pod: %v", err)
		}
	}
	err := wait.PollUntilContextTimeout(ctx, 2*time.Second, 2*time.Minute, true, func(ctx context.Context) (bool, error) {
		var p corev1.Pod
		if err := c.Get(ctx, types.NamespacedName{Name: pod.Name, Namespace: pod.Namespace}, &p); err != nil {
			return false, nil
		}
		for _, cs := range p.Status.ContainerStatuses {
			if cs.RestartCount > 0 {
				return true, nil
			}
		}
		return false, nil
	})
	if err != nil {
		t.Fatalf("crash-loop pod never restarted: %v", err)
	}
	t.Log("crash-loop pod is CrashLoopBackOff in staging namespace")
}

func deleteSandboxClaim(t *testing.T, c client.Client, name, ns string) {
	t.Helper()
	t.Logf("deleteSandboxClaim %s/%s: no-op (bare-pod mode)", ns, name)
}

func deleteBarePod(t *testing.T, c client.Client, name string) {
	t.Helper()
	ctx := context.Background()
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNS}}
	if err := c.Delete(ctx, pod); err != nil {
		return
	}
	_ = wait.PollUntilContextTimeout(ctx, 500*time.Millisecond, 30*time.Second, true, func(ctx context.Context) (bool, error) {
		return c.Get(ctx, types.NamespacedName{Name: name, Namespace: testNS}, &corev1.Pod{}) != nil, nil
	})
}

// watchAndCaptureSandboxLogs starts a background goroutine that watches for
// ls-* pods reaching a terminal phase (Succeeded/Failed) in testNS and
// captures their logs to ARTIFACT_DIR before the operator deletes them.
// Returns a cancel function that stops the watcher.
func watchAndCaptureSandboxLogs(t *testing.T) context.CancelFunc {
	t.Helper()
	baseDir := os.Getenv("ARTIFACT_DIR")
	if baseDir == "" {
		return func() {}
	}
	provider := os.Getenv("E2E_PROVIDER")
	if provider == "" {
		provider = "unknown"
	}
	artifactDir := filepath.Join(baseDir, provider, "podlogs")
	_ = os.MkdirAll(artifactDir, 0o755)

	kubeconfig := os.Getenv("KUBECONFIG")
	if kubeconfig == "" {
		if home, _ := os.UserHomeDir(); home != "" {
			kubeconfig = home + "/.kube/config"
		}
	}
	restCfg, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		t.Logf("watchAndCaptureSandboxLogs: build rest config: %v", err)
		return func() {}
	}
	clientset, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		t.Logf("watchAndCaptureSandboxLogs: create clientset: %v", err)
		return func() {}
	}

	ctx, cancel := context.WithCancel(context.Background())
	captured := make(map[string]bool)
	watcherDone := make(chan struct{})

	go func() {
		defer close(watcherDone)
		for {
			watcher, err := clientset.CoreV1().Pods(testNS).Watch(ctx, metav1.ListOptions{
				LabelSelector: "agentic.openshift.io/run",
			})
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				time.Sleep(2 * time.Second)
				continue
			}
			for event := range watcher.ResultChan() {
				pod, ok := event.Object.(*corev1.Pod)
				if !ok || captured[pod.Name] {
					continue
				}
				if pod.Status.Phase != corev1.PodSucceeded && pod.Status.Phase != corev1.PodFailed {
					continue
				}
				captured[pod.Name] = true
				savePodLogs(ctx, clientset, pod.Name, artifactDir, t)
			}
			if ctx.Err() != nil {
				return
			}
		}
	}()

	return func() {
		cancel()
		<-watcherDone
	}
}

// archiveRunTemplogs exports all persisted collector records for one run. It is
// best-effort and deliberately runs before the AgenticRun cleanup finalizer.
func archiveRunTemplogs(t *testing.T, run *agenticv1alpha1.AgenticRun) {
	t.Helper()
	baseDir, provider := os.Getenv("ARTIFACT_DIR"), os.Getenv("E2E_PROVIDER")
	if baseDir == "" || run == nil || run.UID == "" {
		return
	}
	if provider == "" {
		provider = "unknown"
	}
	dir := filepath.Join(baseDir, provider, "runs", run.Name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Logf("archive templogs: create directory: %v", err)
		return
	}
	writeErr := func(err error) {
		_ = os.WriteFile(filepath.Join(dir, "otel-sandbox.error.txt"), []byte(err.Error()+"\n"), 0o644)
		t.Logf("archive templogs: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	cfg, err := e2eRESTConfig()
	if err != nil {
		writeErr(fmt.Errorf("build REST config: %w", err))
		return
	}
	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		writeErr(fmt.Errorf("create Kubernetes client: %w", err))
		return
	}
	cm, err := clientset.CoreV1().ConfigMaps(testNS).Get(ctx, "lightspeed-agentic-configuration", metav1.GetOptions{})
	if err != nil {
		writeErr(fmt.Errorf("read agentic configuration: %w", err))
		return
	}
	adminEndpoint, caSecretName := cm.Data["otel-admin-endpoint"], cm.Data["otel-ca-secret"]
	if adminEndpoint == "" || caSecretName == "" {
		_ = os.WriteFile(filepath.Join(dir, "otel-sandbox.skipped.txt"), []byte("OTEL admin endpoint or CA secret is not configured\n"), 0o644)
		return
	}
	endpoint, err := url.Parse(adminEndpoint)
	if err != nil || endpoint.Scheme != "https" || endpoint.Hostname() == "" || endpoint.Port() == "" {
		writeErr(fmt.Errorf("invalid OTEL admin endpoint %q", adminEndpoint))
		return
	}
	servicePort, err := strconv.Atoi(endpoint.Port())
	if err != nil {
		writeErr(fmt.Errorf("invalid OTEL admin port in %q: %w", adminEndpoint, err))
		return
	}
	secret, err := clientset.CoreV1().Secrets(testNS).Get(ctx, caSecretName, metav1.GetOptions{})
	if err != nil {
		writeErr(fmt.Errorf("read OTEL CA secret %s: %w", caSecretName, err))
		return
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(secret.Data["otel-ca.crt"]) {
		writeErr(fmt.Errorf("OTEL CA secret %s has no valid otel-ca.crt", caSecretName))
		return
	}
	collectorPods, err := clientset.CoreV1().Pods(testNS).List(ctx, metav1.ListOptions{LabelSelector: otelCollectorLabelSelector})
	if err != nil {
		writeErr(fmt.Errorf("list collector pods: %w", err))
		return
	}
	var collectorPod string
	for _, pod := range collectorPods.Items {
		if pod.Status.Phase == corev1.PodRunning && pod.DeletionTimestamp == nil {
			collectorPod = pod.Name
			break
		}
	}
	if collectorPod == "" {
		writeErr(fmt.Errorf("no running OTEL collector pod"))
		return
	}

	// The endpoint port is a Service port; resolve its target port for the
	// pod port-forward (the two are not necessarily equal).
	collectorService := strings.Split(endpoint.Hostname(), ".")[0]
	service, err := clientset.CoreV1().Services(testNS).Get(ctx, collectorService, metav1.GetOptions{})
	if err != nil {
		writeErr(fmt.Errorf("get collector service %s: %w", collectorService, err))
		return
	}
	remotePort := servicePort
	for _, port := range service.Spec.Ports {
		if int(port.Port) == servicePort && port.TargetPort.IntVal != 0 {
			remotePort = int(port.TargetPort.IntVal)
			break
		}
	}
	req := clientset.CoreV1().RESTClient().Post().Resource("pods").Namespace(testNS).Name(collectorPod).SubResource("portforward")
	transport, upgrader, err := spdy.RoundTripperFor(cfg)
	if err != nil {
		writeErr(fmt.Errorf("create port-forward transport: %w", err))
		return
	}
	stop, ready := make(chan struct{}), make(chan struct{})
	forwarder, err := portforward.New(spdy.NewDialer(upgrader, &http.Client{Transport: transport}, http.MethodPost, req.URL()), []string{fmt.Sprintf("0:%d", remotePort)}, stop, ready, io.Discard, io.Discard)
	if err != nil {
		writeErr(fmt.Errorf("create port-forward: %w", err))
		return
	}
	forwardErr := make(chan error, 1)
	go func() { forwardErr <- forwarder.ForwardPorts() }()
	select {
	case <-ready:
	case err := <-forwardErr:
		writeErr(fmt.Errorf("start port-forward: %w", err))
		return
	case <-ctx.Done():
		writeErr(fmt.Errorf("start port-forward: %w", ctx.Err()))
		return
	}
	defer close(stop)
	ports, err := forwarder.GetPorts()
	if err != nil || len(ports) == 0 {
		writeErr(fmt.Errorf("get forwarded port: %w", err))
		return
	}
	serverName := endpoint.Hostname()
	endpoint.Host = net.JoinHostPort("127.0.0.1", strconv.Itoa(int(ports[0].Local)))
	endpoint.Path = "/api/v1/logs"
	endpoint.RawQuery = ""
	endpoint.RawPath = ""

	type response struct {
		Records []json.RawMessage `json:"records"`
		HasMore bool              `json:"has_more"`
	}
	var records []json.RawMessage
	var after int64
	httpClient := &http.Client{Timeout: 20 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, ServerName: serverName}}}
	for {
		query := endpoint.Query()
		query.Set("agentic_run_id", string(run.UID))
		query.Set("limit", "1000")
		query.Set("after", strconv.FormatInt(after, 10))
		endpoint.RawQuery = query.Encode()
		resp, err := httpClient.Get(endpoint.String())
		if err != nil {
			writeErr(fmt.Errorf("GET collector records: %w", err))
			return
		}
		var page response
		decodeErr := json.NewDecoder(resp.Body).Decode(&page)
		resp.Body.Close()
		if decodeErr != nil || resp.StatusCode != http.StatusOK {
			writeErr(fmt.Errorf("GET collector records: status=%s decode=%v", resp.Status, decodeErr))
			return
		}
		if len(page.Records) == 0 {
			break
		}
		records = append(records, page.Records...)
		var last struct {
			ID int64 `json:"id"`
		}
		if err := json.Unmarshal(page.Records[len(page.Records)-1], &last); err != nil || last.ID <= after {
			writeErr(fmt.Errorf("invalid collector pagination cursor: %v", err))
			return
		}
		after = last.ID
		if !page.HasMore {
			break
		}
	}
	data, err := json.MarshalIndent(records, "", "  ")
	if err != nil {
		writeErr(fmt.Errorf("marshal collector records: %w", err))
		return
	}
	if err := os.WriteFile(filepath.Join(dir, "otel-sandbox.json"), data, 0o644); err != nil {
		t.Logf("archive templogs: write records: %v", err)
	}
}

func e2eRESTConfig() (*rest.Config, error) {
	kubeconfig := os.Getenv("KUBECONFIG")
	if kubeconfig == "" {
		home, _ := os.UserHomeDir()
		kubeconfig = filepath.Join(home, ".kube", "config")
	}
	return clientcmd.BuildConfigFromFlags("", kubeconfig)
}

func savePodLogs(ctx context.Context, clientset kubernetes.Interface, podName, artifactDir string, t *testing.T) {
	logCtx, logCancel := context.WithTimeout(ctx, 15*time.Second)
	defer logCancel()

	stream, err := clientset.CoreV1().Pods(testNS).GetLogs(podName, &corev1.PodLogOptions{}).Stream(logCtx)
	if err != nil {
		return
	}
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(stream)
	stream.Close()
	if buf.Len() == 0 {
		return
	}
	outPath := filepath.Join(artifactDir, fmt.Sprintf("sandbox-%s.log", podName))
	if err := os.WriteFile(outPath, buf.Bytes(), 0o644); err != nil {
		t.Logf("savePodLogs: write %s: %v", outPath, err)
	} else {
		t.Logf("savePodLogs: saved %s (%d bytes)", outPath, buf.Len())
	}
}

// createFixtures creates real provider fixtures and ensures the crash-loop pod exists.
func createFixtures(t *testing.T, c client.Client) *e2eFixtures {
	t.Helper()
	f := createRealProviderFixtures(t, c)
	ensureCrashLoopPod(t, c)
	return f
}

// createTroubleshootingFixtures creates real provider fixtures with a fully
// automatic approval policy (all stages auto-approved) for unattended CI runs.
func createTroubleshootingFixtures(t *testing.T, c client.Client) *e2eFixtures {
	t.Helper()

	f := createRealProviderFixtures(t, c)

	ctx := context.Background()
	base := f.Policy.DeepCopy()
	f.Policy.Spec.Stages = []agenticv1alpha1.ApprovalPolicyStage{
		{Name: agenticv1alpha1.SandboxStepAnalysis, Approval: agenticv1alpha1.ApprovalModeAutomatic},
		{Name: agenticv1alpha1.SandboxStepExecution, Approval: agenticv1alpha1.ApprovalModeAutomatic},
		{Name: agenticv1alpha1.SandboxStepVerification, Approval: agenticv1alpha1.ApprovalModeAutomatic},
	}
	if err := c.Patch(ctx, f.Policy, client.MergeFrom(base)); err != nil {
		t.Fatalf("patch approval policy for full auto-approve: %v", err)
	}
	t.Log("Approval policy patched: all stages auto-approved")

	return f
}
