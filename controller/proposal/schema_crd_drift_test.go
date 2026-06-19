package proposal

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/yaml"
)

// TestAnalysisSchemaCoversCRDRequiredFields guards against the schema/CRD
// contract drift described in lightspeed-agentic-operator#162: the JSON
// schema sent to the LLM for structured output must mark as required every
// field the AnalysisResult CRD requires. When the two disagree, a valid LLM
// response can be rejected by CRD validation at status-patch time, which
// fails the analysis and orphans the sandbox pod.
//
// The generated CRD (which encodes the +required markers) is the source of
// truth. For every object node the two schemas share, the CRD's required set
// must be a subset of the LLM schema's required set. This would have caught
// both the original estimatedImpact incident and the latent verification gap.
func TestAnalysisSchemaCoversCRDRequiredFields(t *testing.T) {
	crdPath := filepath.Join("..", "..", "config", "crd", "bases", "agentic.openshift.io_analysisresults.yaml")
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

	// The LLM analysis schema describes a single option; its per-option shape
	// lives at properties.options.items.
	var llm map[string]any
	if err := json.Unmarshal(AnalysisOutputSchema, &llm); err != nil {
		t.Fatalf("unmarshal AnalysisOutputSchema: %v", err)
	}
	llmOption, ok := digObject(llm, "properties", "options", "items")
	if !ok {
		t.Fatal("AnalysisOutputSchema is missing properties.options.items")
	}

	for _, v := range crd.Spec.Versions {
		if v.Schema == nil || v.Schema.OpenAPIV3Schema == nil {
			continue
		}
		// The matching per-option schema in the CRD is at status.options.items.
		status, ok := v.Schema.OpenAPIV3Schema.Properties["status"]
		if !ok {
			continue
		}
		options, ok := status.Properties["options"]
		if !ok || options.Items == nil || options.Items.Schema == nil {
			t.Fatalf("CRD version %s is missing status.options.items schema", v.Name)
		}
		assertRequiredCoverage(t, "options[]", options.Items.Schema, llmOption)
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
		// Only enforce coverage for fields the LLM schema actually models;
		// the LLM may legitimately omit a field the CRD allows.
		if _, modeled := llmProps[req]; !modeled {
			continue
		}
		if !llmRequired[req] {
			t.Errorf("schema/CRD drift at %s: CRD requires %q but the LLM output schema does not mark it required", path, req)
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
