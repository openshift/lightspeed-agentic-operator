package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

func TestRejectedSwapLeavesFilesUnchanged(t *testing.T) {
	for _, tc := range []struct {
		name, summary, replacement, wantError string
	}{
		{"malformed summary", `{"results":[`, "new", "parse smoke summary"},
		{"invalid summary result", `{"results":[{"route":"openai","model":"old","result":"unknown","date":"2026-01-01"}]}`, "new", "invalid lastRun"},
		{"unchanged model", `{"results":[]}`, "old", "different from old model"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			files := map[string]string{
				"model-versions.yaml": "# Keep catalog comment\nroutes: {openai: {family: openai, primary: old}}\n",
				"model-history.yaml":  "# Keep history comment\n[]\n",
			}
			for name, original := range files {
				writeTestFile(t, filepath.Join(dir, name), original)
			}
			summary := filepath.Join(dir, "summary.json")
			writeTestFile(t, summary, tc.summary)
			args := []string{"swap", "--slot", "routes.openai.primary", "--old", "old", "--new", tc.replacement,
				"--summary", summary, "--date", "2026-01-02"}
			if err := run(args, dir, io.Discard, io.Discard); err == nil || !strings.Contains(err.Error(), tc.wantError) {
				t.Fatalf("expected error containing %q, got %v", tc.wantError, err)
			}
			for name, original := range files {
				got, err := os.ReadFile(filepath.Join(dir, name))
				if err != nil || string(got) != original {
					t.Errorf("rejected swap changed %s: got %q, read error: %v", name, got, err)
				}
			}
		})
	}
}

func TestCheckHistoryAgainstGitBase(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	git := func(args ...string) {
		t.Helper()
		if output, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, output)
		}
	}
	git("init", "--quiet")
	dir := filepath.Dir(catalogPath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, catalogPath, "routes: {openai: {family: openai, primary: old, extras: [retained]}}\n")
	git("add", "--", catalogPath)
	git("-c", "user.name=Test", "-c", "user.email=test@example.invalid", "-c", "core.hooksPath=/dev/null",
		"commit", "--quiet", "--no-gpg-sign", "-m", "Base catalog")
	writeTestFile(t, catalogPath, "routes: {openai: {family: openai, primary: new, extras: [retained]}}\n")
	writeTestFile(t, filepath.Join(dir, "model-history.yaml"), "[]\n")

	var diagnostics bytes.Buffer
	if err := run([]string{"check-history", "--base", "HEAD"}, dir, io.Discard, &diagnostics); err != nil {
		t.Fatalf("check-history warnings must not fail the command: %v", err)
	}
	const want = "WARNING: removed old from routes.openai.primary without a history record\n"
	if got := diagnostics.String(); got != want {
		t.Fatalf("diagnostics: got %q, want %q", got, want)
	}
}

func writeTestFile(t *testing.T, path, text string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestReadBaseCatalogDeadline(t *testing.T) {
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte("#!/bin/sh\nexec sleep 30\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := readBaseCatalog(ctx, "main"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected deadline error, got %v", err)
	}
}

func TestQueries(t *testing.T) {
	base := t.TempDir()
	for name, text := range map[string]string{
		"model-versions.yaml": "routes: {bedrock-claude: {family: anthropic, primary: primary-model, extras: [extra-model]}}\n",
		"model-history.yaml":  "[]\n",
	} {
		if err := os.WriteFile(filepath.Join(base, name), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		args     []string
		contains string
		wantErr  bool
	}{
		{[]string{"validate"}, "", false},
		{[]string{"primary", "bedrock-claude"}, "primary-model\n", false},
		{[]string{"primary", "bedrock-openai"}, "", true},
		{[]string{"primary", "bedrock-deepseek"}, "", true},
		{[]string{"matrix"}, `"model":"extra-model"`, false},
		{[]string{"check-history", "--base", "no-such-ref"}, "WARNING:", false},
	} {
		var out bytes.Buffer
		err := run(tc.args, base, &out, &out)
		if (err != nil) != tc.wantErr || !strings.Contains(out.String(), tc.contains) {
			t.Errorf("%v: got output %q and error %v", tc.args, out.String(), err)
		}
	}
}
