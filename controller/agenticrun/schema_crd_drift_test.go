package agenticrun

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/yaml"
)

// crdBasesDir resolves config/crd/bases relative to this source file rather
// than the test's working directory, so the lookup survives changes to how or
// from where the tests are invoked.
func crdBasesDir(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot resolve test source path via runtime.Caller")
	}
	return filepath.Join(filepath.Dir(thisFile), "..", "..", "config", "crd", "bases")
}

// TestAgentCRDHasNoUnreportedReadyColumn ensures `oc get agents` does not
// advertise readiness that the operator never writes to Agent status.
func TestAgentCRDHasNoUnreportedReadyColumn(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(crdBasesDir(t), "agentic.openshift.io_agents.yaml"))
	if err != nil {
		t.Fatalf("read Agent CRD: %v", err)
	}
	var crd apiextensionsv1.CustomResourceDefinition
	if err := yaml.Unmarshal(raw, &crd); err != nil {
		t.Fatalf("unmarshal Agent CRD: %v", err)
	}
	for _, version := range crd.Spec.Versions {
		for _, column := range version.AdditionalPrinterColumns {
			if column.Name == "Ready" {
				t.Errorf("Agent CRD version %s advertises a Ready column without a status writer", version.Name)
			}
		}
	}
}

// TestAgentCRDHasNoStatus ensures the configuration-only Agent CRD does not
// advertise a status field or status subresource that no controller maintains.
func TestAgentCRDHasNoStatus(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(crdBasesDir(t), "agentic.openshift.io_agents.yaml"))
	if err != nil {
		t.Fatalf("read Agent CRD: %v", err)
	}
	var crd apiextensionsv1.CustomResourceDefinition
	if err := yaml.Unmarshal(raw, &crd); err != nil {
		t.Fatalf("unmarshal Agent CRD: %v", err)
	}
	for _, version := range crd.Spec.Versions {
		if _, ok := version.Schema.OpenAPIV3Schema.Properties["status"]; ok {
			t.Errorf("Agent CRD version %s declares an unused status field", version.Name)
		}
		if version.Subresources != nil && version.Subresources.Status != nil {
			t.Errorf("Agent CRD version %s declares an unused status subresource", version.Name)
		}
	}
}

// TestSchemasCoverCRDRequiredFields guards against the schema/CRD contract
// drift described in lightspeed-agentic-operator#162: the JSON schema sent to
// the LLM for structured output must mark as required every field the result
// CRD requires. When the two disagree, a valid LLM response can be rejected by
// CRD validation at status-patch time, which fails the step and orphans the
// sandbox pod.
//
// The generated CRDs (which encode the +required markers) are the source of
// truth. For every object node a schema pair shares, the CRD's required set
// must be a subset of the LLM schema's required set. This covers all four LLM
// output schemas, catching the original estimatedImpact incident as well as
// the latent verification (checks source/value) gaps.
func TestSchemasCoverCRDRequiredFields(t *testing.T) {
	cases := []struct {
		name      string
		crdFile   string
		llmSchema json.RawMessage
		// crdNode selects the CRD schema node to start the comparison from,
		// given the decoded status schema for a CRD version.
		crdNode func(status *apiextensionsv1.JSONSchemaProps) (*apiextensionsv1.JSONSchemaProps, bool)
		// llmNode selects the matching LLM schema node, given the decoded root.
		llmNode func(root map[string]any) (map[string]any, bool)
		// path labels the comparison root in failure messages.
		path string
	}{
		{
			// Analysis emits an array of options; the per-option shape that
			// maps onto the CRD lives at properties.options.items.
			name:      "analysis",
			crdFile:   "agentic.openshift.io_analysisresults.yaml",
			llmSchema: AnalysisOutputSchema,
			crdNode: func(status *apiextensionsv1.JSONSchemaProps) (*apiextensionsv1.JSONSchemaProps, bool) {
				options, ok := status.Properties["options"]
				if !ok || options.Items == nil || options.Items.Schema == nil {
					return nil, false
				}
				return options.Items.Schema, true
			},
			llmNode: func(root map[string]any) (map[string]any, bool) {
				return digObject(root, "properties", "options", "items")
			},
			path: "options[]",
		},
		{
			// Execution, verification, and escalation each emit a single
			// object whose shape maps directly onto the CRD status.
			name:      "execution",
			crdFile:   "agentic.openshift.io_executionresults.yaml",
			llmSchema: ExecutionOutputSchema,
			crdNode: func(status *apiextensionsv1.JSONSchemaProps) (*apiextensionsv1.JSONSchemaProps, bool) {
				return status, true
			},
			llmNode: func(root map[string]any) (map[string]any, bool) { return root, true },
			path:    "status",
		},
		{
			name:      "verification",
			crdFile:   "agentic.openshift.io_verificationresults.yaml",
			llmSchema: VerificationOutputSchema,
			crdNode: func(status *apiextensionsv1.JSONSchemaProps) (*apiextensionsv1.JSONSchemaProps, bool) {
				return status, true
			},
			llmNode: func(root map[string]any) (map[string]any, bool) { return root, true },
			path:    "status",
		},
		{
			name:      "escalation",
			crdFile:   "agentic.openshift.io_escalationresults.yaml",
			llmSchema: EscalationOutputSchema,
			crdNode: func(status *apiextensionsv1.JSONSchemaProps) (*apiextensionsv1.JSONSchemaProps, bool) {
				return status, true
			},
			llmNode: func(root map[string]any) (map[string]any, bool) { return root, true },
			path:    "status",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			crdPath := filepath.Join(crdBasesDir(t), tc.crdFile)
			raw, err := os.ReadFile(crdPath)
			if err != nil {
				t.Fatalf("read CRD: %v", err)
			}

			var crd apiextensionsv1.CustomResourceDefinition
			if err := yaml.Unmarshal(raw, &crd); err != nil {
				t.Fatalf("unmarshal CRD: %v", err)
			}
			if len(crd.Spec.Versions) == 0 {
				t.Fatal("CRD declares no versions")
			}

			var llm map[string]any
			if err := json.Unmarshal(tc.llmSchema, &llm); err != nil {
				t.Fatalf("unmarshal LLM schema: %v", err)
			}
			llmStart, ok := tc.llmNode(llm)
			if !ok {
				t.Fatal("LLM schema is missing the expected comparison node")
			}

			checked := false
			for _, v := range crd.Spec.Versions {
				if v.Schema == nil || v.Schema.OpenAPIV3Schema == nil {
					continue
				}
				status, ok := v.Schema.OpenAPIV3Schema.Properties["status"]
				if !ok {
					continue
				}
				checked = true
				crdStart, ok := tc.crdNode(&status)
				if !ok {
					t.Fatalf("CRD version %s is missing the expected status schema node", v.Name)
				}
				assertRequiredCoverage(t, tc.path, crdStart, llmStart)
			}
			if !checked {
				t.Fatal("no CRD version contained a comparable status schema")
			}
		})
	}
}

// TestSchemasCoverCRDMaxLength guards against maxLength drift: every string
// field in the CRD with a maxLength constraint must have a matching maxLength
// in the LLM output schema so the LLM is told the limit. Without this, the
// LLM can produce strings that pass schema validation but get rejected by CRD
// validation at status-patch time (OLS-3865).
func TestSchemasCoverCRDMaxLength(t *testing.T) {
	crdPath := filepath.Join(crdBasesDir(t), "agentic.openshift.io_analysisresults.yaml")
	raw, err := os.ReadFile(crdPath)
	if err != nil {
		t.Fatalf("read CRD: %v", err)
	}

	var crd apiextensionsv1.CustomResourceDefinition
	if err := yaml.Unmarshal(raw, &crd); err != nil {
		t.Fatalf("unmarshal CRD: %v", err)
	}

	var llm map[string]any
	if err := json.Unmarshal(AnalysisOutputSchema, &llm); err != nil {
		t.Fatalf("unmarshal LLM schema: %v", err)
	}
	llmStart, ok := digObject(llm, "properties", "options", "items")
	if !ok {
		t.Fatal("LLM schema is missing options[].items")
	}

	for _, v := range crd.Spec.Versions {
		if v.Schema == nil || v.Schema.OpenAPIV3Schema == nil {
			continue
		}
		status, ok := v.Schema.OpenAPIV3Schema.Properties["status"]
		if !ok {
			continue
		}
		options, ok := status.Properties["options"]
		if !ok || options.Items == nil || options.Items.Schema == nil {
			continue
		}
		assertMaxLengthCoverage(t, "options[]", options.Items.Schema, llmStart)
	}
}

// TestDiagnosisRootCauseLimit checks the generated CRD and LLM schemas allow
// longer complex diagnoses while retaining the same bounded rootCause length.
func TestDiagnosisRootCauseLimit(t *testing.T) {
	const want = int64(8192)
	raw, err := os.ReadFile(filepath.Join(crdBasesDir(t), "agentic.openshift.io_analysisresults.yaml"))
	if err != nil {
		t.Fatalf("read CRD: %v", err)
	}
	var crd apiextensionsv1.CustomResourceDefinition
	if err := yaml.Unmarshal(raw, &crd); err != nil {
		t.Fatalf("unmarshal CRD: %v", err)
	}
	if len(crd.Spec.Versions) == 0 {
		t.Fatal("CRD declares no versions")
	}
	for _, version := range crd.Spec.Versions {
		if version.Schema == nil || version.Schema.OpenAPIV3Schema == nil {
			t.Fatalf("CRD version %s has no schema", version.Name)
		}
		status := version.Schema.OpenAPIV3Schema.Properties["status"]
		top := status.Properties["diagnosis"].Properties["rootCause"]
		options := status.Properties["options"]
		if options.Items == nil || options.Items.Schema == nil {
			t.Fatal("CRD options has no item schema")
		}
		perOption := options.Items.Schema.Properties["diagnosis"].Properties["rootCause"]
		for name, prop := range map[string]apiextensionsv1.JSONSchemaProps{"status.diagnosis": top, "status.options[].diagnosis": perOption} {
			if prop.MaxLength == nil {
				t.Errorf("%s.rootCause has no maxLength, want %d", name, want)
			} else if *prop.MaxLength != want {
				t.Errorf("%s.rootCause maxLength = %d, want %d", name, *prop.MaxLength, want)
			}
		}
	}

	for _, tc := range []struct {
		name   string
		schema json.RawMessage
		path   []string
	}{
		{"default top-level", AnalysisOutputSchema, []string{"properties", "diagnosis", "properties", "rootCause"}},
		{"default per-option", AnalysisOutputSchema, []string{"properties", "options", "items", "properties", "diagnosis", "properties", "rootCause"}},
		{"minimal top-level", MinimalAnalysisOutputSchema, []string{"properties", "diagnosis", "properties", "rootCause"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var schema map[string]any
			if err := json.Unmarshal(tc.schema, &schema); err != nil {
				t.Fatalf("unmarshal LLM schema: %v", err)
			}
			rootCause, ok := digObject(schema, tc.path...)
			if !ok {
				t.Fatal("LLM schema missing rootCause")
			}
			if got, ok := rootCause["maxLength"].(float64); !ok || int64(got) != want {
				t.Errorf("rootCause maxLength = %v, want %d", rootCause["maxLength"], want)
			}
		})
	}
}

// assertMaxLengthCoverage walks a CRD schema node and the corresponding LLM
// schema node in parallel, asserting that every string field with a CRD
// maxLength also has a maxLength in the LLM schema.
func assertMaxLengthCoverage(t *testing.T, path string, crd *apiextensionsv1.JSONSchemaProps, llm map[string]any) {
	t.Helper()
	if crd == nil || llm == nil {
		return
	}

	if crd.Type == "array" {
		if crd.Items == nil || crd.Items.Schema == nil {
			return
		}
		llmItems, ok := asJSONObject(llm["items"])
		if !ok {
			t.Errorf("maxLength drift at %s: CRD has array items but LLM schema is missing items node", path)
			return
		}
		assertMaxLengthCoverage(t, path+"[]", crd.Items.Schema, llmItems)
		return
	}

	crdRequired := map[string]bool{}
	for _, r := range crd.Required {
		crdRequired[r] = true
	}

	llmProps, _ := asJSONObject(llm["properties"])
	for name, prop := range crd.Properties {
		childPath := path + "." + name
		llmProp, ok := asJSONObject(llmProps[name])
		if !ok {
			if prop.Type == "string" && prop.MaxLength != nil && crdRequired[name] {
				t.Errorf("maxLength drift at %s: CRD requires field with maxLength=%d but LLM schema does not model this property", childPath, *prop.MaxLength)
			}
			continue
		}

		if prop.Type == "string" && prop.MaxLength != nil {
			llmMax, hasMax := llmProp["maxLength"]
			if !hasMax {
				t.Errorf("maxLength drift at %s: CRD has maxLength=%d but LLM schema has no maxLength", childPath, *prop.MaxLength)
			} else if num, ok := llmMax.(float64); ok && int64(num) != *prop.MaxLength {
				t.Errorf("maxLength drift at %s: CRD maxLength=%d but LLM schema maxLength=%d", childPath, *prop.MaxLength, int64(num))
			} else if !ok {
				t.Errorf("maxLength drift at %s: LLM schema maxLength is not numeric", childPath)
			}
		}

		assertMaxLengthCoverage(t, childPath, &prop, llmProp)
	}
}

// assertRequiredCoverage walks a CRD schema node and the corresponding LLM
// schema node in parallel, asserting that every field required by the CRD is
// also required by the LLM schema (for fields the LLM schema actually models).
func assertRequiredCoverage(t *testing.T, path string, crd *apiextensionsv1.JSONSchemaProps, llm map[string]any) {
	t.Helper()
	if crd == nil || llm == nil {
		return
	}

	// Arrays: descend into the element schema on both sides.
	if crd.Type == "array" {
		if crd.Items == nil || crd.Items.Schema == nil {
			return
		}
		llmItems, ok := asJSONObject(llm["items"])
		if !ok {
			t.Errorf("schema/CRD drift at %s: LLM schema is missing object-shaped array items", path)
			return
		}
		assertRequiredCoverage(t, path+"[]", crd.Items.Schema, llmItems)
		return
	}

	llmProps, _ := asJSONObject(llm["properties"])
	llmRequired := requiredSet(llm["required"])
	for _, req := range crd.Required {
		// A field the CRD requires must be both modeled and required in the LLM
		// output schema. If it is absent entirely the agent is never asked for
		// it, so the status patch is rejected at runtime — the exact drift this
		// guard exists to catch.
		if _, modeled := llmProps[req]; !modeled {
			t.Errorf("schema/CRD drift at %s: CRD requires %q but the LLM output schema does not model the field", path, req)
			continue
		}
		if !llmRequired[req] {
			t.Errorf("schema/CRD drift at %s: CRD requires %q but the LLM output schema does not mark it required", path, req)
		}
	}

	// Reverse direction: a property the LLM schema models that does not exist
	// in the CRD is silently pruned by the API server on the status patch —
	// silent data loss instead of a rejection. Skip CRD nodes that
	// legitimately accept arbitrary fields.
	if len(crd.Properties) > 0 && !acceptsUnknownFields(crd) {
		for name := range llmProps {
			if llmOnlyControlFields[name] {
				continue
			}
			if _, ok := crd.Properties[name]; !ok {
				t.Errorf("schema/CRD drift at %s: LLM output schema models %q but the CRD has no such property (silently pruned on status patch)", path, name)
			}
		}
	}

	// Recurse into properties present in both schemas.
	for name := range crd.Properties {
		child := crd.Properties[name]
		llmChild, ok := asJSONObject(llmProps[name])
		if !ok {
			continue
		}
		assertRequiredCoverage(t, path+"."+name, &child, llmChild)
	}
}

// llmOnlyControlFields are properties the LLM output schemas model that are
// deliberately not persisted to any CRD: the operator consumes them for
// control flow (the sandbox agent uses them to determine step outcome)
// before building the result CR, so the pruning check does not apply
// to them.
var llmOnlyControlFields = map[string]bool{
	"success": true,
}

// acceptsUnknownFields reports whether a CRD schema node tolerates properties
// beyond those it declares, in which case the reverse (LLM→CRD) pruning check
// does not apply.
func acceptsUnknownFields(crd *apiextensionsv1.JSONSchemaProps) bool {
	if crd.XPreserveUnknownFields != nil && *crd.XPreserveUnknownFields {
		return true
	}
	if crd.AdditionalProperties != nil {
		return crd.AdditionalProperties.Allows || crd.AdditionalProperties.Schema != nil
	}
	return false
}

// digObject walks nested object properties of a decoded JSON schema.
func digObject(m map[string]any, keys ...string) (map[string]any, bool) {
	cur := m
	for _, k := range keys {
		next, ok := asJSONObject(cur[k])
		if !ok {
			return nil, false
		}
		cur = next
	}
	return cur, true
}

func asJSONObject(v any) (map[string]any, bool) {
	m, ok := v.(map[string]any)
	return m, ok
}

func requiredSet(v any) map[string]bool {
	out := map[string]bool{}
	arr, ok := v.([]any)
	if !ok {
		return out
	}
	for _, e := range arr {
		if s, ok := e.(string); ok {
			out[s] = true
		}
	}
	return out
}

// TestSchemasCoverCRDRBACMinItems guards OLS-3465: the CRD requires
// RBACRule apiGroups/resources/verbs to be non-empty (MinItems=1), and the
// LLM analysis output schema must declare the same minItems so structured
// output and shaping validation reject hedged empty-array rules at the
// source instead of losing the entire AnalysisResult at publish.
func TestSchemasCoverCRDRBACMinItems(t *testing.T) {
	crdPath := filepath.Join(crdBasesDir(t), "agentic.openshift.io_analysisresults.yaml")
	raw, err := os.ReadFile(crdPath)
	if err != nil {
		t.Fatalf("read CRD: %v", err)
	}

	var crd apiextensionsv1.CustomResourceDefinition
	if err := yaml.Unmarshal(raw, &crd); err != nil {
		t.Fatalf("unmarshal CRD: %v", err)
	}

	var llm map[string]any
	if err := json.Unmarshal(AnalysisOutputSchema, &llm); err != nil {
		t.Fatalf("unmarshal LLM schema: %v", err)
	}

	for _, v := range crd.Spec.Versions {
		if v.Schema == nil || v.Schema.OpenAPIV3Schema == nil {
			continue
		}
		status, ok := v.Schema.OpenAPIV3Schema.Properties["status"]
		if !ok {
			continue
		}
		options, ok := status.Properties["options"]
		if !ok || options.Items == nil || options.Items.Schema == nil {
			continue
		}
		rbac, ok := options.Items.Schema.Properties["rbac"]
		if !ok {
			continue
		}
		for _, scope := range []string{"namespaceScoped", "clusterScoped"} {
			scopeProps, ok := rbac.Properties[scope]
			if !ok || scopeProps.Items == nil || scopeProps.Items.Schema == nil {
				continue
			}
			crdRule := scopeProps.Items.Schema
			llmRuleProps, ok := digObject(llm, "properties", "options", "items", "properties", "rbac", "properties", scope, "items", "properties")
			if !ok {
				t.Errorf("LLM schema is missing options[].rbac.%s items properties", scope)
				continue
			}
			for _, field := range []string{"apiGroups", "resources", "verbs"} {
				crdField, ok := crdRule.Properties[field]
				if !ok {
					t.Errorf("CRD options[].rbac.%s[] is missing %s", scope, field)
					continue
				}
				if crdField.MinItems == nil || *crdField.MinItems != 1 {
					t.Errorf("CRD options[].rbac.%s[].%s must set MinItems=1", scope, field)
				}
				llmField, ok := llmRuleProps[field].(map[string]any)
				if !ok {
					t.Errorf("LLM schema options[].rbac.%s[] is missing %s", scope, field)
					continue
				}
				if minItems, ok := llmField["minItems"].(float64); !ok || minItems != 1 {
					t.Errorf("LLM schema options[].rbac.%s[].%s must declare minItems: 1 to match the CRD", scope, field)
				}
			}
		}
	}
}
