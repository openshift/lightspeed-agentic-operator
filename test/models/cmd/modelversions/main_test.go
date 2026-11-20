package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSwapWritesCatalogAndHistory(t *testing.T) {
	dir := t.TempDir()
	for name, text := range map[string]string{
		"model-versions.yaml": "routes: {openai: {family: openai, primary: a}}\n",
		"model-history.yaml":  "# History header\n[]\n",
		"summary.json":        `{"results":[{"route":"openai","model":"a","result":"passed","date":"2026-01-01"}]}`,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var diagnostics bytes.Buffer
	args := []string{"swap", "--slot", "routes.openai.primary", "--old", "a", "--new", "b",
		"--summary", filepath.Join(dir, "summary.json"), "--date", "2026-01-02"}
	if err := run(args, dir, io.Discard, &diagnostics); err != nil || diagnostics.Len() != 0 {
		t.Fatalf("swap: %v, %s", err, &diagnostics)
	}
	var out bytes.Buffer
	if err := run([]string{"primary", "openai"}, dir, &out, io.Discard); err != nil || out.String() != "b\n" {
		t.Fatalf("primary: %v, %q", err, &out)
	}
	records, err := loadHistory(filepath.Join(dir, "model-history.yaml"))
	if err != nil || len(records) != 1 || records[0].LastRun == nil || records[0].LastRun.Result != "passed" {
		t.Fatalf("history: %+v, %v", records, err)
	}
}

func TestQueries(t *testing.T) {
	base := filepath.Join("..", "..")
	for _, tc := range []struct {
		args     []string
		contains string
		wantErr  bool
	}{
		{[]string{"validate"}, "", false},
		{[]string{"primary", "bedrock-openai"}, "openai.gpt-6-luna\n", false},
		{[]string{"primary", "bedrock-deepseek"}, "", true},
		{[]string{"matrix"}, `"model":"openai.gpt-6.1-terra"`, false},
		{[]string{"check-history", "--base", "no-such-ref"}, "WARNING:", false},
	} {
		var out bytes.Buffer
		err := run(tc.args, base, &out, &out)
		if (err != nil) != tc.wantErr || !strings.Contains(out.String(), tc.contains) {
			t.Errorf("%v: got output %q and error %v", tc.args, out.String(), err)
		}
	}
}
