//go:build product_e2e

package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
)

func TestTroubleshooting_PhaseTransitions(t *testing.T) {
	scenariosDir := os.Getenv("E2E_SCENARIOS_DIR")
	if scenariosDir == "" {
		t.Fatal("E2E_SCENARIOS_DIR must be set (path to cloned rhobs/troubleshooting-scenarios)")
	}

	c := newClient(t)
	createTroubleshootingFixtures(t, c)
	stopWatcher := watchAndCaptureSandboxLogs(t)
	t.Cleanup(stopWatcher)

	scenarios := discoverScenarios(t, scenariosDir)

	scenarioTimeout := 20 * time.Minute
	if v := os.Getenv("E2E_SCENARIO_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			scenarioTimeout = d
		}
	}

	for _, sc := range scenarios {
		t.Run(sc.Name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), scenarioTimeout)
			defer cancel()

			setupScript := sc.Dir + "/setup.sh"
			cleanupScript := sc.Dir + "/cleanup.sh"

			t.Cleanup(func() {
				if _, err := os.Stat(cleanupScript); err != nil {
					t.Logf("Skipping cleanup: %s not found", cleanupScript)
					return
				}
				t.Logf("Running cleanup: %s", cleanupScript)
				cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 2*time.Minute)
				defer cleanupCancel()
				cmd := exec.CommandContext(cleanupCtx, "bash", cleanupScript)
				cmd.Stdout = os.Stdout
				cmd.Stderr = os.Stderr
				if err := cmd.Run(); err != nil {
					t.Errorf("cleanup.sh failed: %v", err)
				}
			})

			t.Logf("Running setup: %s", setupScript)
			setupCmd := exec.CommandContext(ctx, "bash", setupScript)
			setupCmd.Stdout = os.Stdout
			setupCmd.Stderr = os.Stderr
			if err := setupCmd.Run(); err != nil {
				t.Fatalf("setup.sh failed: %v", err)
			}
			t.Log("Scenario setup complete")

			var tools agenticv1alpha1.ToolsSpec
			for _, s := range sc.Spec.Tools.Skills {
				tools.Skills = append(tools.Skills, agenticv1alpha1.SkillsSource{
					Image: s.Image,
					Paths: s.Paths,
				})
			}

			runName := fmt.Sprintf("e2e-ts-%s", strings.ReplaceAll(sc.Name, "_", "-"))
			run := createTroubleshootingRun(t, c, runName, sc.Spec.Request, sc.Spec.TargetNamespaces, tools)
			// Registered after createTroubleshootingRun's cleanup, so this runs
			// first and exports templogs before the AgenticRun finalizer deletes
			// them from the collector.
			t.Cleanup(func() { archiveRunTemplogs(t, run) })

			deadline, _ := ctx.Deadline()
			remaining := time.Until(deadline)

			expectedPhase := sc.ExpectedPhase
			t.Logf("Waiting for phase: %s", expectedPhase)
			updated := waitForPhaseWithTimeout(t, c, run.Name, expectedPhase, remaining)
			t.Logf("Phase reached: %s", expectedPhase)

			if expectedPhase == agenticv1alpha1.AgenticRunPhaseCompleted {
				assertNoFailedConditions(t, updated)
				assertAnalysisResultExists(t, c, string(run.UID))
				assertExecutionResultExists(t, c, string(run.UID))
				assertVerificationResultExists(t, c, string(run.UID))
			} else {
				assertAnalysisResultExists(t, c, string(run.UID))
			}

			t.Logf("PASS: %s — phase transition to %s", sc.Name, expectedPhase)
		})
	}
}

func createTroubleshootingRun(t *testing.T, c client.Client, name, request string, targetNamespaces []string, tools agenticv1alpha1.ToolsSpec) *agenticv1alpha1.AgenticRun {
	t.Helper()
	ctx := context.Background()

	run := &agenticv1alpha1.AgenticRun{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNS},
		Spec: agenticv1alpha1.AgenticRunSpec{
			Request:          request,
			TargetNamespaces: targetNamespaces,
			Tools:            tools,
			Analysis:         agenticv1alpha1.AgenticRunStep{Agent: "e2e-agent"},
			Execution:        agenticv1alpha1.AgenticRunStep{Agent: "e2e-agent"},
			Verification:     agenticv1alpha1.AgenticRunStep{Agent: "e2e-agent"},
		},
	}

	cleanup(t, c, &agenticv1alpha1.AgenticRun{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNS}})
	cleanup(t, c, &agenticv1alpha1.AgenticRunApproval{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNS}})
	deleteSandboxClaim(t, c, "ls-analysis-"+name, testNS)
	deleteSandboxClaim(t, c, "ls-execution-"+name, testNS)
	deleteSandboxClaim(t, c, "ls-verification-"+name, testNS)
	deleteBarePod(t, c, "ls-analysis-"+name)
	deleteBarePod(t, c, "ls-execution-"+name)
	deleteBarePod(t, c, "ls-verification-"+name)

	if err := c.Create(ctx, run); err != nil {
		t.Fatalf("create AgenticRun %s: %v", name, err)
	}
	t.Cleanup(func() { cleanup(t, c, run) })
	t.Logf("AgenticRun created: %s/%s", testNS, name)

	return run
}

func assertNoFailedConditions(t *testing.T, run agenticv1alpha1.AgenticRun) {
	t.Helper()
	tracked := map[string]bool{
		agenticv1alpha1.AgenticRunConditionAnalyzed:  true,
		agenticv1alpha1.AgenticRunConditionExecuted:  true,
		agenticv1alpha1.AgenticRunConditionVerified:  true,
		agenticv1alpha1.AgenticRunConditionEscalated: true,
	}
	for _, cond := range run.Status.Conditions {
		if tracked[cond.Type] && cond.Status == metav1.ConditionFalse {
			t.Errorf("condition %s has status=False reason=%s message=%s", cond.Type, cond.Reason, cond.Message)
		}
	}
	t.Log("Verified: no failed workflow conditions")
}

func assertAnalysisResultExists(t *testing.T, c client.Client, runUID string) {
	t.Helper()
	var list agenticv1alpha1.AnalysisResultList
	if err := c.List(context.Background(), &list, client.InNamespace(testNS), client.MatchingLabels{"agentic.openshift.io/run": runUID}); err != nil {
		t.Fatalf("list AnalysisResult: %v", err)
	}
	if len(list.Items) == 0 {
		t.Fatal("no AnalysisResult found")
	}
	if len(list.Items[0].OwnerReferences) == 0 {
		t.Error("AnalysisResult has no owner references")
	}
	t.Logf("Verified: AnalysisResult %s exists with owner reference", list.Items[0].Name)
}

func assertExecutionResultExists(t *testing.T, c client.Client, runUID string) {
	t.Helper()
	var list agenticv1alpha1.ExecutionResultList
	if err := c.List(context.Background(), &list, client.InNamespace(testNS), client.MatchingLabels{"agentic.openshift.io/run": runUID}); err != nil {
		t.Fatalf("list ExecutionResult: %v", err)
	}
	if len(list.Items) == 0 {
		t.Fatal("no ExecutionResult found")
	}
	if len(list.Items[0].OwnerReferences) == 0 {
		t.Error("ExecutionResult has no owner references")
	}
	t.Logf("Verified: ExecutionResult %s exists with owner reference", list.Items[0].Name)
}

func assertVerificationResultExists(t *testing.T, c client.Client, runUID string) {
	t.Helper()
	var list agenticv1alpha1.VerificationResultList
	if err := c.List(context.Background(), &list, client.InNamespace(testNS), client.MatchingLabels{"agentic.openshift.io/run": runUID}); err != nil {
		t.Fatalf("list VerificationResult: %v", err)
	}
	if len(list.Items) == 0 {
		t.Fatal("no VerificationResult found")
	}
	if len(list.Items[0].OwnerReferences) == 0 {
		t.Error("VerificationResult has no owner references")
	}
	t.Logf("Verified: VerificationResult %s exists with owner reference", list.Items[0].Name)
}
