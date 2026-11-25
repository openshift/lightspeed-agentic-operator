package models

import (
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestCommittedCatalog(t *testing.T) {
	catalog, err := Load("model-versions.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var bedrockOpenAI []string
	for _, pair := range catalog.SmokeMatrix() {
		if pair.Route == "bedrock-openai" {
			bedrockOpenAI = append(bedrockOpenAI, pair.Model)
		}
	}
	if len(bedrockOpenAI) != 3 {
		t.Fatalf("want three Bedrock OpenAI smoke models, got %v", bedrockOpenAI)
	}
	for _, id := range bedrockOpenAI {
		if !strings.HasPrefix(id, "openai.") {
			t.Errorf("Bedrock OpenAI smoke model %q must use the openai. prefix", id)
		}
	}
}

func TestRouteOrderAndPrimaries(t *testing.T) {
	file, err := Parse([]byte(`routes:
  openai: {family: openai, primary: native, extras: [new]}
  azure-openai: {family: openai, primary: azure, extras: [azure-new]}
`))
	if err != nil {
		t.Fatal(err)
	}
	want := []Pair{
		{"openai", "native"}, {"azure-openai", "azure"},
		{"openai", "new"}, {"azure-openai", "azure-new"},
	}
	if got := file.SmokeMatrix(); !reflect.DeepEqual(got, want) {
		t.Fatalf("matrix: got %v, want %v", got, want)
	}
	for _, pair := range want[:2] {
		got, err := file.Primary(pair.Route)
		if err != nil || got != pair.Model {
			t.Errorf("primary %s: got %q, %v", pair.Route, got, err)
		}
	}
}

func TestInvalidRoutes(t *testing.T) {
	_, err := Parse([]byte("routes:\n  unknown: {family: openai, smoke: false, extras: [\"\"]}\n"))
	if err == nil {
		t.Fatal("expected errors")
	}
	for _, want := range []string{"unknown route", "primary", "smoke", "empty"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%v does not report %q", err, want)
		}
	}
}

func TestMalformedCatalog(t *testing.T) {
	for _, tc := range []struct{ name, yaml, wantError string }{
		{"invalid YAML", "routes: [", "yaml:"},
		{"wrong route shape", "routes: [openai]", "cannot unmarshal"},
		{"empty routes", "routes: {}", "routes must not be empty"},
		{"unknown field", "routes: {openai: {family: openai, primary: model, typo: true}}", "field typo not found"},
		{"wrong family", "routes: {openai: {family: google, primary: model}}", `routes.openai.family must be "openai"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Parse([]byte(tc.yaml)); err == nil || !strings.Contains(err.Error(), tc.wantError) {
				t.Fatalf("expected error containing %q, got %v", tc.wantError, err)
			}
		})
	}
}

func TestMatrixDeduplicatesAndSkipsDisabled(t *testing.T) {
	f, err := Parse([]byte("routes:\n  openai: {family: openai, primary: a, extras: [a, b, b]}\n  claude: {family: anthropic, primary: c, smoke: false}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := f.SmokeMatrix(); !reflect.DeepEqual(got, []Pair{{"openai", "a"}, {"openai", "b"}}) {
		t.Fatal(got)
	}
}

func TestSwap(t *testing.T) {
	for _, tc := range []struct {
		slot, old, new string
		found          bool
	}{
		{"routes.openai.primary", "a", "a2", true}, {"routes.openai.extras", "b", "b2", false},
	} {
		before := []byte("# versions header\nroutes:\n  openai: {family: openai, primary: a, extras: [b]}\n")
		hist := []byte("# history header\n- slot: routes.openai.extras\n  model: retired\n  swappedAt: 2025-01-01\n  expiresAt: 2025-07-01\n  lastRun: null\n")
		summary := Summary{Results: []Run{{Route: "openai", Model: "a", Result: "passed", Date: "2026-01-01"}}}
		next, records, warnings, err := Swap(before, hist, tc.slot, tc.old, tc.new, "2026-01-02", summary)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(next), "# versions header") || !strings.Contains(string(records), "# history header") {
			t.Fatalf("comments lost: versions:\n%s\nhistory:\n%s", next, records)
		}
		if strings.Contains(string(records), "retired") || !strings.Contains(string(next), tc.new) {
			t.Fatalf("unexpected versions/history: %s\n%s", next, records)
		}
		parsed, err := ParseHistory(records)
		if err != nil || len(parsed) != 1 {
			t.Fatalf("history: %v, %v", parsed, err)
		}
		if (parsed[0].LastRun != nil) != tc.found || (len(warnings) == 0) != tc.found {
			t.Fatalf("history/warnings: %v, %v", parsed, warnings)
		}
		if _, _, _, err := Swap(before, hist, tc.slot, "wrong", tc.new, "2026-01-02", summary); err == nil {
			t.Fatal("unknown old model accepted")
		}
	}
}

func TestSwapRejectsSharedYAMLAlias(t *testing.T) {
	versions := []byte(`routes:
  openai: {family: openai, primary: &shared original}
  azure-openai: {family: openai, primary: *shared}
`)
	next, records, _, err := Swap(versions, []byte("[]"), "routes.openai.primary", "original", "replacement", "2026-01-02", Summary{})
	if err == nil || !strings.Contains(err.Error(), "YAML alias") || next != nil || records != nil {
		t.Fatalf("expected an alias error and no updates, got %v, %q, %q", err, next, records)
	}
}

func TestCommittedHistory(t *testing.T) {
	data, err := os.ReadFile("model-history.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseHistory(data); err != nil {
		t.Fatal(err)
	}
}

func TestEmptyHistory(t *testing.T) {
	if h, err := ParseHistory([]byte("[]")); err != nil || len(h) != 0 {
		t.Fatalf("empty history: %v, %v", h, err)
	}
}

func TestMalformedHistory(t *testing.T) {
	for _, tc := range []struct{ name, yaml, wantError string }{
		{"invalid YAML", "- [", "yaml:"},
		{"wrong shape", "history: []", "history must be a YAML list"},
		{"unknown field", "- typo: true", "field typo not found"},
		{"invalid record", "- slot: routes.openai.primary", "history record 1: history model is empty"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseHistory([]byte(tc.yaml)); err == nil || !strings.Contains(err.Error(), tc.wantError) {
				t.Fatalf("expected error containing %q, got %v", tc.wantError, err)
			}
		})
	}
}

func TestHistoryValidationAndWarnings(t *testing.T) {
	base, _ := Parse([]byte("routes: {openai: {family: openai, primary: a, extras: [b]}}"))
	head, _ := Parse([]byte("routes: {openai: {family: openai, primary: c}}"))
	history := []Record{{Slot: "routes.openai.primary", Model: "a", SwappedAt: "2026-01-31", ExpiresAt: "2026-07-31"}}
	if err := history[0].Validate(); err != nil {
		t.Fatal(err)
	}
	warnings := CheckHistory(base, head, history, "2026-08-01")
	if len(warnings) != 2 || !strings.Contains(strings.Join(warnings, " "), "expired") {
		t.Fatal(warnings)
	}
	for _, invalid := range []Record{
		{Slot: "bad", Model: "a", SwappedAt: "2026-01-31", ExpiresAt: "2026-07-31"},
		{Slot: "routes.openai.primary", Model: "a", SwappedAt: "bad", ExpiresAt: "2026-07-31"},
		{Slot: "routes.openai.primary", Model: "a", SwappedAt: "2026-01-31", ExpiresAt: "2026-08-01"},
		{Slot: "routes.openai.primary", Model: "a", SwappedAt: "2026-01-31", ExpiresAt: "2026-07-31", LastRun: &LastRun{Date: "2026-01-30", Route: "openai", Result: "bad"}},
	} {
		if err := invalid.Validate(); err == nil {
			t.Errorf("accepted %+v", invalid)
		}
	}
}
