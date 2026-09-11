// Mock agent for e2e and local testing (batch mode).
//
// Contract: the operator mounts an input ConfigMap at /input/ with four keys:
//
//	query          — the user's request text
//	output-schema  — JSON schema identifying the step (analysis/execution/verification/escalation)
//	context        — JSON with targetNamespaces, approvedOption, etc.
//	result-template — JSON for the Result CR (apiVersion, kind, metadata, spec); sandbox fills status
//
// The mock reads /input/, creates the Result CR via the Kubernetes API, patches
// its status subresource with a canned response + Completed=True condition,
// and exits 0. For execution/verification steps it sleeps first so e2e tests
// can observe in-flight RBAC.
//
// Build:  make -C test/agent docker-build
// Image:  quay.io/openshift-lightspeed/ols-qe:lightspeed-mock-agent1
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
	agenticrun "github.com/openshift/lightspeed-agentic-operator/controller/agenticrun"
)

const inputDir = "/input"

// Mock behavior keywords — embed in the AgenticRun request text to
// trigger failure modes in e2e tests.
const (
	MockTimeout   = "MOCK_TIMEOUT"    // sleep forever, never create Result CR → SandboxTimeout
	MockCrash     = "MOCK_CRASH"      // exit 1 immediately, no Result CR → SandboxFailed
	MockNoStatus  = "MOCK_NO_STATUS"  // create Result CR but don't patch status, exit 0 → SandboxFailed (partial write)
	MockAgentFail = "MOCK_AGENT_FAIL" // exit 0, Result CR with failureReason + Completed=True → AgentFailed
	// MockVerifyFail makes ONLY the verification step report an objective
	// failure (VerificationResult Completed reason=Failed with a failing check);
	// analysis and execution still succeed. Drives the escalate-on-verification-
	// failure path (OLS-3817): Verified=False → Escalating.
	//
	// Propagation: the operator only embeds the request text in the ANALYSIS
	// query (buildAnalysisQuery); the verification query is built from the
	// approved option + execution result (buildVerificationQuery), NOT the
	// request. So the keyword cannot reach the verification step directly. The
	// analysis mock therefore stamps this marker into the RemediationOption it
	// emits; the operator serializes that option into the verification query
	// (OptionJSON), which is how the verification step sees it. See
	// setStatus/AnalysisResult below.
	MockVerifyFail = "MOCK_VERIFY_FAIL"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	log.Println("mock agent starting (batch mode)")

	query := mustReadFile(inputDir + "/query")
	schemaRaw := mustReadFile(inputDir + "/output-schema")
	ctxRaw := mustReadFile(inputDir + "/context")
	tmplRaw := mustReadFile(inputDir + "/result-template")

	step := stepFromSchema(schemaRaw)
	targetNS := pickNamespace(ctxRaw)
	queryStr := string(query)
	verifyFail := strings.Contains(queryStr, MockVerifyFail)
	// Build stamp: if this line is absent from a sandbox pod's logs, the cluster
	// is running a STALE mock image and the fix under test never ran. See OLS-3817.
	log.Printf("mock-agent build=OLS-3817-verifyfail-via-option step=%s target_ns=%s query_len=%d verify_fail=%v", step, targetNS, len(query), verifyFail)

	// Failure modes triggered by keywords in the query text.
	switch {
	case strings.Contains(queryStr, MockTimeout):
		log.Println("MOCK_TIMEOUT: sleeping forever")
		time.Sleep(24 * time.Hour)
	case strings.Contains(queryStr, MockCrash):
		log.Fatalf("MOCK_CRASH: exiting without creating Result CR")
	case strings.Contains(queryStr, MockAgentFail):
		log.Println("MOCK_AGENT_FAIL: exiting 0 without creating Result CR")
		return
	}

	if d := stepDelay(step); d > 0 {
		log.Printf("delaying %s for step=%s", d, step)
		time.Sleep(d)
	}

	c := mustNewClient()
	ctx := context.Background()
	cr := newResultCR(step, tmplRaw)
	if err := c.Create(ctx, cr); err != nil {
		log.Fatalf("create %s %s/%s: %v", cr.GetObjectKind().GroupVersionKind().Kind, cr.GetNamespace(), cr.GetName(), err)
	}
	log.Printf("created %s %s/%s", cr.GetObjectKind().GroupVersionKind().Kind, cr.GetNamespace(), cr.GetName())

	if strings.Contains(queryStr, MockNoStatus) {
		log.Println("MOCK_NO_STATUS: CR created without status patch — exiting 0")
		return
	}

	setStatus(cr, targetNS, verifyFail)
	if err := c.Status().Update(ctx, cr); err != nil {
		log.Fatalf("update status: %v", err)
	}
	log.Printf("status updated, step=%s — exiting 0", step)
}

// ---------------------------------------------------------------------------
// Input helpers
// ---------------------------------------------------------------------------

func mustReadFile(path string) []byte {
	data, err := os.ReadFile(path)
	if err != nil {
		log.Fatalf("read %s: %v", path, err)
	}
	return data
}

func stepFromSchema(raw []byte) string {
	compact := compactJSON(raw)
	switch {
	case bytes.Equal(compact, compactJSON(agenticrun.ExecutionOutputSchema)):
		return "execution"
	case bytes.Equal(compact, compactJSON(agenticrun.VerificationOutputSchema)):
		return "verification"
	case bytes.Equal(compact, compactJSON(agenticrun.EscalationOutputSchema)):
		return "escalation"
	default:
		return "analysis"
	}
}

func compactJSON(raw json.RawMessage) []byte {
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return raw
	}
	return buf.Bytes()
}

func stepDelay(step string) time.Duration {
	switch step {
	case "execution", "verification":
		return 60 * time.Second
	default:
		return 0
	}
}

func pickNamespace(ctxRaw []byte) string {
	var c struct {
		TargetNamespaces []string `json:"targetNamespaces"`
	}
	if err := json.Unmarshal(ctxRaw, &c); err == nil && len(c.TargetNamespaces) > 0 && c.TargetNamespaces[0] != "" {
		return c.TargetNamespaces[0]
	}
	return "default"
}

// ---------------------------------------------------------------------------
// Kubernetes client
// ---------------------------------------------------------------------------

func mustNewClient() client.Client {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		log.Printf("not in cluster (%v), falling back to kubeconfig", err)
		loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
		cfg, err = clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, nil).ClientConfig()
		if err != nil {
			log.Fatalf("kubeconfig: %v", err)
		}
	}

	s := scheme.Scheme
	utilruntime.Must(agenticv1alpha1.AddToScheme(s))

	c, err := client.New(cfg, client.Options{Scheme: s})
	if err != nil {
		log.Fatalf("create client: %v", err)
	}
	return c
}

// ---------------------------------------------------------------------------
// Result CR: create from template, then fill status
// ---------------------------------------------------------------------------

func newResultCR(step string, tmplRaw []byte) client.Object {
	switch step {
	case "execution":
		var cr agenticv1alpha1.ExecutionResult
		must(json.Unmarshal(tmplRaw, &cr))
		return &cr
	case "verification":
		var cr agenticv1alpha1.VerificationResult
		must(json.Unmarshal(tmplRaw, &cr))
		return &cr
	case "escalation":
		var cr agenticv1alpha1.EscalationResult
		must(json.Unmarshal(tmplRaw, &cr))
		return &cr
	default:
		var cr agenticv1alpha1.AnalysisResult
		must(json.Unmarshal(tmplRaw, &cr))
		return &cr
	}
}

// mockTokenUsage returns deterministic values for the operator E2E tests.
// These values model non-zero usage from a sandbox without depending on an LLM.
func mockTokenUsage(step string) agenticv1alpha1.TokenUsage {
	var input, output int64
	switch step {
	case "analysis":
		input, output = 100, 200
	case "execution":
		input, output = 110, 210
	case "verification":
		input, output = 120, 220
	case "escalation":
		input, output = 130, 230
	}
	return agenticv1alpha1.TokenUsage{InputTokens: &input, OutputTokens: &output}
}

// setStatus fills a Result CR with the canned outcome for its workflow step.
func setStatus(obj client.Object, targetNS string, verifyFail bool) {
	now := metav1.Now()
	completed := []metav1.Condition{{
		Type:               "Completed",
		Status:             metav1.ConditionTrue,
		Reason:             "Succeeded",
		LastTransitionTime: now,
	}}

	switch cr := obj.(type) {
	case *agenticv1alpha1.AnalysisResult:
		cr.Status.TokenUsage = mockTokenUsage("analysis")
		cr.Status.Conditions = completed
		cr.Status.ActionRequired = agenticv1alpha1.ActionRequiredTrue
		cr.Status.Diagnosis = agenticv1alpha1.DiagnosisResult{
			Summary:   "mock diagnosis",
			RootCause: "mock root cause",
		}
		// When the request asked for a verification failure, stamp the marker
		// into the option summary so it survives into the verification query
		// (OptionJSON) — the request text itself never reaches that step.
		optionSummary := "mock option summary"
		if verifyFail {
			optionSummary = "mock option summary " + MockVerifyFail
		}
		cr.Status.Options = []agenticv1alpha1.RemediationOption{{
			Title:   "mock-remediation",
			Summary: optionSummary,
			Diagnosis: agenticv1alpha1.DiagnosisResult{
				Summary:   "mock diagnosis",
				RootCause: "mock root cause",
			},
			RemediationPlan: agenticv1alpha1.RemediationPlan{
				Description: "mock proposal description",
				Actions: []agenticv1alpha1.ProposedAction{
					{Command: fmt.Sprintf("kubectl get configmap -n %s", targetNS), Type: "pre-check", Description: "Check current configmap state"},
					{Command: fmt.Sprintf("kubectl patch configmap mock-cm -n %s -p '{\"data\":{\"key\":\"value\"}}'", targetNS), Type: "mutation", Description: "Patch configmap with fix"},
					{Command: fmt.Sprintf("kubectl get configmap mock-cm -n %s -o jsonpath='{.data.key}'", targetNS), Type: "post-check", Description: "Verify configmap was patched"},
				},
				Reversible: agenticv1alpha1.ReversibilityReversible,
			},
			Verification: agenticv1alpha1.VerificationPlan{
				Description: "mock verification plan",
				Steps: []agenticv1alpha1.VerificationStep{
					{Name: "mock-step", Command: "true", Expected: "ok", Type: "command"},
				},
			},
			RBAC: agenticv1alpha1.RBACResult{
				NamespaceScoped: []agenticv1alpha1.RBACRule{{
					Namespace:     targetNS,
					APIGroups:     []string{""},
					Resources:     []string{"configmaps"},
					Verbs:         []string{"get", "list", "patch"},
					Justification: "Read and patch configmaps for mock remediation",
				}},
			},
		}}

	case *agenticv1alpha1.ExecutionResult:
		cr.Status.TokenUsage = mockTokenUsage("execution")
		cr.Status.Conditions = completed
		cr.Status.ActionsTaken = []agenticv1alpha1.ExecutionAction{{
			Type:        "mock",
			Description: "mock execution action",
			Outcome:     "Succeeded",
		}}

	case *agenticv1alpha1.VerificationResult:
		cr.Status.TokenUsage = mockTokenUsage("verification")
		if verifyFail {
			// Objective verification failure (OLS-3817): the agent ran and
			// reports the remediation did not work. Signal it via the
			// Completed condition reason=Failed so the operator escalates.
			cr.Status.Conditions = []metav1.Condition{{
				Type:               "Completed",
				Status:             metav1.ConditionTrue,
				Reason:             agenticv1alpha1.ResultReasonFailed,
				LastTransitionTime: now,
			}}
			cr.Status.Checks = []agenticv1alpha1.VerifyCheck{{
				Name:   "mock-check",
				Source: "mock",
				Value:  "not-ok",
				Result: "Failed",
			}}
			cr.Status.Summary = "mock verification failure (MOCK_VERIFY_FAIL)"
			break
		}
		cr.Status.Conditions = completed
		cr.Status.Checks = []agenticv1alpha1.VerifyCheck{{
			Name:   "mock-check",
			Source: "mock",
			Value:  "ok",
			Result: "Passed",
		}}
		cr.Status.Summary = "mock verification summary"

	case *agenticv1alpha1.EscalationResult:
		cr.Status.TokenUsage = mockTokenUsage("escalation")
		cr.Status.Conditions = completed
		cr.Status.Summary = "mock escalation summary"
		cr.Status.Content = "mock escalation content"
	}
}

func must(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
