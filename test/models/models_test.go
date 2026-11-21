package models

import (
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestCommittedMatrix(t *testing.T) {
	file, err := Load("model-versions.yaml")
	if err != nil {
		t.Fatal(err)
	}
	want := []Pair{
		{"claude", "claude-haiku-5-5"}, {"gemini", "gemini-3.1-flash-lite"},
		{"openai", "gpt-6-luna"}, {"azure-openai", "gpt-6-luna"},
		{"bedrock-claude", "global.anthropic.claude-haiku-5-5"}, {"bedrock-openai", "openai.gpt-6-luna"},
		{"claude", "claude-sonnet-5-5"}, {"claude", "claude-opus-5-5"},
		{"gemini", "gemini-3.8-flash"}, {"openai", "gpt-6.1-sol"}, {"openai", "gpt-6.1-terra"},
		{"azure-openai", "gpt-6.1-sol"}, {"azure-openai", "gpt-6.1-terra"},
		{"bedrock-claude", "global.anthropic.claude-sonnet-5-5"}, {"bedrock-claude", "global.anthropic.claude-opus-5-5"},
		{"bedrock-openai", "openai.gpt-6.1-sol"}, {"bedrock-openai", "openai.gpt-6.1-terra"},
	}
	if got := file.SmokeMatrix(); !reflect.DeepEqual(got, want) {
		t.Fatalf("matrix: got %v, want %v", got, want)
	}
	for _, pair := range want[:6] {
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

func TestHistoryValidationAndWarnings(t *testing.T) {
	data, err := os.ReadFile("model-history.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if h, err := ParseHistory(data); err != nil || len(h) != 0 {
		t.Fatalf("initial history: %v, %v", h, err)
	}
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
