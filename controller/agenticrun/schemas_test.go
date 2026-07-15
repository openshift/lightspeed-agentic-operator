package agenticrun

import (
	"encoding/json"
	"testing"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

func TestDefaultOutputSchemas_AllPhasesPresent(t *testing.T) {
	tests := []struct {
		phase       string
		requiredKey string
	}{
		{"analysis", "options"},
		{"execution", "actionsTaken"},
		{"verification", "checks"},
		{"escalation", "content"},
	}

	for _, tt := range tests {
		t.Run(tt.phase, func(t *testing.T) {
			schema := defaultOutputSchemas[tt.phase]
			if schema == nil {
				t.Fatal("schema is nil")
			}

			var parsed map[string]any
			if err := json.Unmarshal(schema, &parsed); err != nil {
				t.Fatalf("schema is not valid JSON: %v", err)
			}

			props, ok := parsed["properties"].(map[string]any)
			if !ok {
				t.Fatal("schema has no properties")
			}
			if _, ok := props[tt.requiredKey]; !ok {
				t.Errorf("schema missing required key %q", tt.requiredKey)
			}
		})
	}
}

func TestDefaultOutputSchemas_UnknownPhaseReturnsNil(t *testing.T) {
	schema := defaultOutputSchemas["unknown"]
	if schema != nil {
		t.Error("unknown phase should return nil")
	}
}

func TestAnalysisOutputSchema_ValidJSON(t *testing.T) {
	var parsed map[string]any
	if err := json.Unmarshal(AnalysisOutputSchema, &parsed); err != nil {
		t.Fatalf("AnalysisOutputSchema is not valid JSON: %v", err)
	}
	if parsed["type"] != "object" {
		t.Errorf("type = %v, want object", parsed["type"])
	}
}

func TestExecutionOutputSchema_ValidJSON(t *testing.T) {
	var parsed map[string]any
	if err := json.Unmarshal(ExecutionOutputSchema, &parsed); err != nil {
		t.Fatalf("ExecutionOutputSchema is not valid JSON: %v", err)
	}
	required, ok := parsed["required"].([]any)
	if !ok {
		t.Fatal("missing required field")
	}
	requiredSet := map[string]bool{}
	for _, r := range required {
		requiredSet[r.(string)] = true
	}
	if !requiredSet["actionsTaken"] {
		t.Error("ExecutionOutputSchema should require actionsTaken")
	}
}

func TestEscalationOutputSchema_ValidJSON(t *testing.T) {
	var parsed map[string]any
	if err := json.Unmarshal(EscalationOutputSchema, &parsed); err != nil {
		t.Fatalf("EscalationOutputSchema is not valid JSON: %v", err)
	}
	required, ok := parsed["required"].([]any)
	if !ok {
		t.Fatal("missing required field")
	}
	requiredSet := map[string]bool{}
	for _, r := range required {
		requiredSet[r.(string)] = true
	}
	for _, key := range []string{"success", "summary", "content"} {
		if !requiredSet[key] {
			t.Errorf("EscalationOutputSchema should require %q", key)
		}
	}
}

func TestVerificationOutputSchema_ValidJSON(t *testing.T) {
	var parsed map[string]any
	if err := json.Unmarshal(VerificationOutputSchema, &parsed); err != nil {
		t.Fatalf("VerificationOutputSchema is not valid JSON: %v", err)
	}
	required, ok := parsed["required"].([]any)
	if !ok {
		t.Fatal("missing required field")
	}
	requiredSet := map[string]bool{}
	for _, r := range required {
		requiredSet[r.(string)] = true
	}
	if !requiredSet["checks"] {
		t.Error("VerificationOutputSchema should require checks")
	}
}

func TestAnalysisOutputSchema_OptionsStructure(t *testing.T) {
	var parsed map[string]any
	if err := json.Unmarshal(AnalysisOutputSchema, &parsed); err != nil {
		t.Fatal(err)
	}

	props := parsed["properties"].(map[string]any)
	options := props["options"].(map[string]any)

	if options["type"] != "array" {
		t.Error("options should be an array")
	}

	items := options["items"].(map[string]any)
	itemProps := items["properties"].(map[string]any)

	for _, key := range []string{"title", "diagnosis", "remediationPlan", "rbac", "verification"} {
		if _, ok := itemProps[key]; !ok {
			t.Errorf("options items missing required property %q", key)
		}
	}

	required := items["required"].([]any)
	requiredSet := map[string]bool{}
	for _, r := range required {
		requiredSet[r.(string)] = true
	}
	for _, key := range []string{"title", "diagnosis", "remediationPlan", "verification"} {
		if !requiredSet[key] {
			t.Errorf("%q should be required in options items", key)
		}
	}
	if requiredSet["rbac"] {
		t.Error("rbac should not be required — advisory runs may omit it")
	}

	remediationPlan := itemProps["remediationPlan"].(map[string]any)
	planProps := remediationPlan["properties"].(map[string]any)
	actions := planProps["actions"].(map[string]any)
	actionItems := actions["items"].(map[string]any)
	actionProps := actionItems["properties"].(map[string]any)
	if _, ok := actionProps["command"]; !ok {
		t.Error("actions items should have 'command' property")
	}
	actionRequired := actionItems["required"].([]any)
	actionRequiredSet := map[string]bool{}
	for _, r := range actionRequired {
		actionRequiredSet[r.(string)] = true
	}
	if !actionRequiredSet["command"] {
		t.Error("'command' should be required in actions items")
	}
}

func TestVerificationOutputSchema_ChecksUseResultNotPassed(t *testing.T) {
	var parsed map[string]any
	if err := json.Unmarshal(VerificationOutputSchema, &parsed); err != nil {
		t.Fatal(err)
	}

	props := parsed["properties"].(map[string]any)
	checks := props["checks"].(map[string]any)
	items := checks["items"].(map[string]any)
	itemProps := items["properties"].(map[string]any)

	if _, ok := itemProps["result"]; !ok {
		t.Error("checks items should have 'result' field (not 'passed')")
	}
	if _, ok := itemProps["passed"]; ok {
		t.Error("checks items should NOT have 'passed' field — use 'result' to match Go type")
	}

	resultField := itemProps["result"].(map[string]any)
	if resultField["type"] != "string" {
		t.Errorf("result type = %v, want string", resultField["type"])
	}

	required := items["required"].([]any)
	requiredSet := map[string]bool{}
	for _, r := range required {
		requiredSet[r.(string)] = true
	}
	if !requiredSet["result"] {
		t.Error("'result' should be required in checks items")
	}
}

func TestOutputSchemaForStep_FullAgenticRun_RequiresRBACAndVerification(t *testing.T) {
	run := &agenticv1alpha1.AgenticRun{}
	run.Spec.Execution = agenticv1alpha1.AgenticRunStep{Agent: "default"}
	run.Spec.Verification = agenticv1alpha1.AgenticRunStep{Agent: "default"}

	required := extractOptionRequired(t, outputSchemaForStep("analysis", run))
	if !required["rbac"] {
		t.Error("should require rbac when run has execution")
	}
	if !required["verification"] {
		t.Error("should require verification when run has verification step")
	}
}

func TestOutputSchemaForStep_ExecutionOnly_RequiresRBACNotVerification(t *testing.T) {
	run := &agenticv1alpha1.AgenticRun{}
	run.Spec.Execution = agenticv1alpha1.AgenticRunStep{Agent: "default"}

	required := extractOptionRequired(t, outputSchemaForStep("analysis", run))
	if !required["rbac"] {
		t.Error("should require rbac when run has execution")
	}
	if required["verification"] {
		t.Error("should not require verification when run has no verification step")
	}
}

func TestOutputSchemaForStep_Advisory_OmitsRBACAndVerification(t *testing.T) {
	run := &agenticv1alpha1.AgenticRun{}

	required := extractOptionRequired(t, outputSchemaForStep("analysis", run))
	if required["rbac"] {
		t.Error("should not require rbac for advisory runs")
	}
	if required["verification"] {
		t.Error("should not require verification for advisory runs")
	}
	if !required["title"] || !required["diagnosis"] || !required["remediationPlan"] {
		t.Error("should always require title, diagnosis, run")
	}
}

func TestOutputSchemaForStep_NonAnalysis_ReturnsDefault(t *testing.T) {
	run := &agenticv1alpha1.AgenticRun{}
	run.Spec.Execution = agenticv1alpha1.AgenticRunStep{Agent: "default"}

	for _, step := range []string{"execution", "verification", "escalation"} {
		schema := outputSchemaForStep(step, run)
		if string(schema) != string(defaultOutputSchemas[step]) {
			t.Errorf("outputSchemaForStep(%q) should return default schema", step)
		}
	}
}

func extractOptionItems(t *testing.T, schema json.RawMessage) map[string]any {
	t.Helper()
	var parsed map[string]any
	if err := json.Unmarshal(schema, &parsed); err != nil {
		t.Fatalf("invalid schema JSON: %v", err)
	}
	options := parsed["properties"].(map[string]any)["options"].(map[string]any)
	return options["items"].(map[string]any)
}

func extractOptionRequired(t *testing.T, schema json.RawMessage) map[string]bool {
	t.Helper()
	items := extractOptionItems(t, schema)
	required := items["required"].([]any)
	set := map[string]bool{}
	for _, r := range required {
		set[r.(string)] = true
	}
	return set
}

func TestExecutionOutputSchema_ActionsUseOutcomeNotSuccess(t *testing.T) {
	var parsed map[string]any
	if err := json.Unmarshal(ExecutionOutputSchema, &parsed); err != nil {
		t.Fatal(err)
	}

	props := parsed["properties"].(map[string]any)
	actions := props["actionsTaken"].(map[string]any)
	items := actions["items"].(map[string]any)
	itemProps := items["properties"].(map[string]any)

	if _, ok := itemProps["outcome"]; !ok {
		t.Error("actionsTaken items should have 'outcome' field (not 'success')")
	}
	if _, ok := itemProps["success"]; ok {
		t.Error("actionsTaken items should NOT have 'success' field — use 'outcome' to match Go type")
	}

	required := items["required"].([]any)
	requiredSet := map[string]bool{}
	for _, r := range required {
		requiredSet[r.(string)] = true
	}
	if !requiredSet["outcome"] {
		t.Error("'outcome' should be required in actionsTaken items")
	}
}

func TestOutputSchemaForStep_WithOutputSchema_InjectsComponents(t *testing.T) {
	run := &agenticv1alpha1.AgenticRun{}
	run.Spec.AnalysisOutput = agenticv1alpha1.AnalysisOutput{
		Schema: &apiextensionsv1.JSONSchemaProps{
			Type: "object",
			Properties: map[string]apiextensionsv1.JSONSchemaProps{
				"foo": {Type: "string"},
			},
		},
	}

	schema := outputSchemaForStep("analysis", run)
	var parsed map[string]any
	if err := json.Unmarshal(schema, &parsed); err != nil {
		t.Fatalf("invalid schema JSON: %v", err)
	}

	options := parsed["properties"].(map[string]any)["options"].(map[string]any)
	items := options["items"].(map[string]any)
	props := items["properties"].(map[string]any)
	components, ok := props["components"]
	if !ok {
		t.Fatal("expected 'components' property in option items")
	}
	compMap := components.(map[string]any)
	if compMap["type"] != "object" {
		t.Errorf("components type = %v, want object", compMap["type"])
	}
	compProps := compMap["properties"].(map[string]any)
	if _, ok := compProps["foo"]; !ok {
		t.Error("components should contain 'foo' from outputSchema")
	}

	required := extractOptionRequired(t, schema)
	if !required["components"] {
		t.Error("'components' should be required when outputSchema is set")
	}
}

func TestOutputSchemaForStep_WithoutOutputSchema_NoComponents(t *testing.T) {
	run := &agenticv1alpha1.AgenticRun{}

	schema := outputSchemaForStep("analysis", run)
	var parsed map[string]any
	if err := json.Unmarshal(schema, &parsed); err != nil {
		t.Fatalf("invalid schema JSON: %v", err)
	}

	options := parsed["properties"].(map[string]any)["options"].(map[string]any)
	items := options["items"].(map[string]any)
	props := items["properties"].(map[string]any)
	if _, ok := props["components"]; ok {
		t.Error("should not have 'components' property when outputSchema is nil")
	}

	required := extractOptionRequired(t, schema)
	if required["components"] {
		t.Error("'components' should not be required when analysisOutput.schema is nil")
	}
}

func extractOptionProperties(t *testing.T, schema json.RawMessage) map[string]any {
	t.Helper()
	return extractOptionItems(t, schema)["properties"].(map[string]any)
}

func TestAnalysisOutputSchema_ActionRequiredAndDiagnosis(t *testing.T) {
	var parsed map[string]any
	if err := json.Unmarshal(AnalysisOutputSchema, &parsed); err != nil {
		t.Fatal(err)
	}
	props := parsed["properties"].(map[string]any)

	ar, ok := props["actionRequired"]
	if !ok {
		t.Fatal("schema missing actionRequired property")
	}
	if ar.(map[string]any)["type"] != "boolean" {
		t.Error("actionRequired should be boolean")
	}

	diag, ok := props["diagnosis"]
	if !ok {
		t.Fatal("schema missing diagnosis property")
	}
	diagProps := diag.(map[string]any)["properties"].(map[string]any)
	for _, key := range []string{"summary", "confidence", "rootCause"} {
		if _, ok := diagProps[key]; !ok {
			t.Errorf("diagnosis missing property %q", key)
		}
	}

	required := parsed["required"].([]any)
	requiredSet := map[string]bool{}
	for _, r := range required {
		requiredSet[r.(string)] = true
	}
	if !requiredSet["actionRequired"] {
		t.Error("actionRequired should be required")
	}

	options := props["options"].(map[string]any)
	minItems := options["minItems"].(float64)
	if minItems != 0 {
		t.Errorf("options minItems = %v, want 0", minItems)
	}
}

func TestMinimalAnalysisOutputSchema_ActionRequiredAndDiagnosis(t *testing.T) {
	var parsed map[string]any
	if err := json.Unmarshal(MinimalAnalysisOutputSchema, &parsed); err != nil {
		t.Fatal(err)
	}
	props := parsed["properties"].(map[string]any)

	if _, ok := props["actionRequired"]; !ok {
		t.Fatal("MinimalAnalysisOutputSchema missing actionRequired property")
	}
	if _, ok := props["diagnosis"]; !ok {
		t.Fatal("MinimalAnalysisOutputSchema missing diagnosis property")
	}

	required := parsed["required"].([]any)
	requiredSet := map[string]bool{}
	for _, r := range required {
		requiredSet[r.(string)] = true
	}
	if !requiredSet["actionRequired"] {
		t.Error("actionRequired should be required in minimal schema")
	}

	options := props["options"].(map[string]any)
	minItems := options["minItems"].(float64)
	if minItems != 0 {
		t.Errorf("minimal options minItems = %v, want 0", minItems)
	}
}

func TestMinimalAnalysisOutputSchema_ValidJSON(t *testing.T) {
	var parsed map[string]any
	if err := json.Unmarshal(MinimalAnalysisOutputSchema, &parsed); err != nil {
		t.Fatalf("MinimalAnalysisOutputSchema is not valid JSON: %v", err)
	}
	if parsed["type"] != "object" {
		t.Errorf("type = %v, want object", parsed["type"])
	}

	props := parsed["properties"].(map[string]any)
	options := props["options"].(map[string]any)
	items := options["items"].(map[string]any)
	itemProps := items["properties"].(map[string]any)

	if _, ok := itemProps["title"]; !ok {
		t.Error("MinimalAnalysisOutputSchema should have 'title' in option items")
	}
	if len(itemProps) != 1 {
		t.Errorf("MinimalAnalysisOutputSchema should have exactly 1 property (title), got %d", len(itemProps))
	}
}

func TestOutputSchemaForStep_MinimalMode_StripsBuiltInProperties(t *testing.T) {
	run := &agenticv1alpha1.AgenticRun{}
	run.Spec.AnalysisOutput = agenticv1alpha1.AnalysisOutput{
		Mode: agenticv1alpha1.AnalysisOutputModeMinimal,
	}

	schema := outputSchemaForStep("analysis", run)
	props := extractOptionProperties(t, schema)

	if _, ok := props["title"]; !ok {
		t.Error("Minimal mode should have 'title' property")
	}
	for _, key := range []string{"diagnosis", "remediationPlan", "rbac", "verification", "summary"} {
		if _, ok := props[key]; ok {
			t.Errorf("Minimal mode should not have '%s' property", key)
		}
	}

	required := extractOptionRequired(t, schema)
	if !required["title"] {
		t.Error("Minimal mode should require 'title'")
	}
	if required["diagnosis"] || required["remediationPlan"] {
		t.Error("Minimal mode should not require diagnosis or run")
	}
}

func TestOutputSchemaForStep_MinimalMode_WithCustomSchema(t *testing.T) {
	run := &agenticv1alpha1.AgenticRun{}
	run.Spec.AnalysisOutput = agenticv1alpha1.AnalysisOutput{
		Mode: agenticv1alpha1.AnalysisOutputModeMinimal,
		Schema: &apiextensionsv1.JSONSchemaProps{
			Type: "object",
			Properties: map[string]apiextensionsv1.JSONSchemaProps{
				"severity": {Type: "string"},
			},
		},
	}

	schema := outputSchemaForStep("analysis", run)
	props := extractOptionProperties(t, schema)

	if _, ok := props["components"]; !ok {
		t.Fatal("Minimal + custom schema should have 'components' property")
	}
	if _, ok := props["diagnosis"]; ok {
		t.Error("Minimal + custom schema should not have 'diagnosis'")
	}

	required := extractOptionRequired(t, schema)
	if !required["components"] {
		t.Error("'components' should be required")
	}
	if !required["title"] {
		t.Error("'title' should be required")
	}
}

func TestOutputSchemaForStep_MinimalMode_WithExecution_InjectsRBACAndAgenticRun(t *testing.T) {
	run := &agenticv1alpha1.AgenticRun{}
	run.Spec.AnalysisOutput = agenticv1alpha1.AnalysisOutput{
		Mode: agenticv1alpha1.AnalysisOutputModeMinimal,
	}
	run.Spec.Execution = agenticv1alpha1.AgenticRunStep{Agent: "default"}

	schema := outputSchemaForStep("analysis", run)
	props := extractOptionProperties(t, schema)

	if _, ok := props["rbac"]; !ok {
		t.Error("Minimal + execution should inject 'rbac' property")
	}
	if _, ok := props["remediationPlan"]; !ok {
		t.Error("Minimal + execution should inject 'run' property")
	}
	if _, ok := props["diagnosis"]; ok {
		t.Error("Minimal + execution should not inject 'diagnosis'")
	}

	required := extractOptionRequired(t, schema)
	if !required["rbac"] {
		t.Error("rbac should be required when execution exists")
	}
	if !required["remediationPlan"] {
		t.Error("run should be required when execution exists")
	}
}

func TestOutputSchemaForStep_MinimalMode_WithVerification_InjectsVerification(t *testing.T) {
	run := &agenticv1alpha1.AgenticRun{}
	run.Spec.AnalysisOutput = agenticv1alpha1.AnalysisOutput{
		Mode: agenticv1alpha1.AnalysisOutputModeMinimal,
	}
	run.Spec.Verification = agenticv1alpha1.AgenticRunStep{Agent: "default"}

	schema := outputSchemaForStep("analysis", run)
	props := extractOptionProperties(t, schema)

	if _, ok := props["verification"]; !ok {
		t.Error("Minimal + verification should inject 'verification' property")
	}

	required := extractOptionRequired(t, schema)
	if !required["verification"] {
		t.Error("verification should be required when verification step exists")
	}
}

func TestOutputSchemaForStep_DefaultMode_Unchanged(t *testing.T) {
	run := &agenticv1alpha1.AgenticRun{}
	run.Spec.AnalysisOutput = agenticv1alpha1.AnalysisOutput{
		Mode: agenticv1alpha1.AnalysisOutputModeDefault,
	}

	schema := outputSchemaForStep("analysis", run)
	props := extractOptionProperties(t, schema)

	for _, key := range []string{"title", "diagnosis", "remediationPlan", "rbac", "verification"} {
		if _, ok := props[key]; !ok {
			t.Errorf("Default mode should have '%s' property", key)
		}
	}

	required := extractOptionRequired(t, schema)
	if !required["title"] || !required["diagnosis"] || !required["remediationPlan"] {
		t.Error("Default mode should require title, diagnosis, run")
	}
}
