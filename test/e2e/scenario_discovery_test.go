//go:build product_e2e

package e2e

import (
	"os"
	"path/filepath"
	"testing"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
)

func TestScenarioDiscovery(t *testing.T) {
	const scenario = `- conversation_group_id: test_alert
  tag: [alert]
  turns:
    - openshift_agentic_run_spec:
        request: Investigate the alert
        tools:
          skills:
            - image: example.com/skills:latest
              paths: [/skills/investigate-alert]
      expected_openshift_agentic_run_status:
        phase: Completed
`
	for _, tc := range []struct {
		name  string
		files map[string]string
	}{
		{
			name: "agentic filename",
			files: map[string]string{
				"evals-ols-agentic.yaml": scenario,
				"evals-ols-classic.yaml": "invalid classic data",
			},
		},
		{
			name:  "legacy filename",
			files: map[string]string{"evals.yaml": scenario},
		},
		{
			name: "prefer agentic filename",
			files: map[string]string{
				"evals-ols-agentic.yaml": scenario,
				"evals.yaml":             "invalid legacy data",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("E2E_SCENARIO_TAGS", "alert")
			t.Setenv("E2E_SKIP_SCENARIOS", "")
			t.Setenv("E2E_PROVIDER", "openai")
			root := t.TempDir()
			dir := filepath.Join(root, "evals", "scenarios", "test_alert")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			tc.files["setup.sh"] = "#!/bin/bash\n"
			for name, data := range tc.files {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0o644); err != nil {
					t.Fatal(err)
				}
			}

			scenarios := discoverScenarios(t, root)
			if len(scenarios) != 1 {
				t.Fatalf("got %d scenarios, want 1", len(scenarios))
			}
			got := scenarios[0]
			if got.Name != "test_alert" || got.Dir != dir || got.Spec.Request != "Investigate the alert" {
				t.Fatalf("unexpected discovered scenario: %+v", got)
			}
			if got.ExpectedPhase != agenticv1alpha1.AgenticRunPhaseCompleted {
				t.Errorf("got phase %s, want Completed", got.ExpectedPhase)
			}
			if len(got.Spec.Tools.Skills) != 1 || got.Spec.Tools.Skills[0].Image != "example.com/skills:latest" {
				t.Errorf("unexpected skills: %+v", got.Spec.Tools.Skills)
			}
		})
	}
}
