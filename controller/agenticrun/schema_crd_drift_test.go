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
// the latent verification (checks source/value) and execution (verification
// conditionOutcome/summary) gaps.
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

			for _, v := range crd.Spec.Versions {
				if v.Schema == nil || v.Schema.OpenAPIV3Schema == nil {
					continue
				}
				status, ok := v.Schema.OpenAPIV3Schema.Properties["status"]
				if !ok {
					continue
				}
				crdStart, ok := tc.crdNode(&status)
				if !ok {
					t.Fatalf("CRD version %s is missing the expected status schema node", v.Name)
				}
				assertRequiredCoverage(t, tc.path, crdStart, llmStart)
			}
		})
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
// control flow (see executionResponse/verificationResponse in
// sandbox_agent.go) before building the status patch, so the pruning check
// does not apply to them.
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
