package disconnected

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRecordRunRetainsDeletedRunIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runs.jsonl")
	t.Setenv("E2E_DISCONNECTED_RUNS_FILE", path)
	if err := RecordRun("sandboxes", "first", "uid-1"); err != nil {
		t.Fatal(err)
	}
	if err := RecordRun("sandboxes", "second", "uid-2"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected two retained identities, got %q", data)
	}
	for i, line := range lines {
		var record map[string]string
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatal(err)
		}
		if record["namespace"] != "sandboxes" || record["uid"] != []string{"uid-1", "uid-2"}[i] || record["name"] == "" {
			t.Fatalf("invalid retained identity: %v", record)
		}
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("journal must be private: %v, %v", info, err)
	}
}

func TestRecordRunRequiresJournal(t *testing.T) {
	t.Setenv("E2E_DISCONNECTED_RUNS_FILE", "")
	if RecordRun("sandboxes", "run", "uid") == nil {
		t.Fatal("missing journal must fail before silently losing ownership")
	}
}

func TestCleanupUsesAuthoritativeScript(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "cleanup.sh")
	marker := filepath.Join(dir, "called")
	if err := os.WriteFile(script, []byte("#!/bin/bash\ntouch \"$CALL_MARKER\"\necho sensitive-key\nexit 17\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("E2E_DISCONNECTED_CLEANUP_SCRIPT", script)
	t.Setenv("CALL_MARKER", marker)
	t.Setenv("VLLM_API_KEY", "sensitive-key")
	err := Cleanup(context.Background())
	if err == nil {
		t.Fatal("cleanup failure must be reported, not followed by direct policy deletion")
	}
	if strings.Contains(err.Error(), "sensitive-key") || !strings.Contains(err.Error(), "[REDACTED]") {
		t.Fatalf("cleanup output was not redacted: %v", err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("authoritative script was not called: %v", err)
	}
}

func TestCleanupRequiresScript(t *testing.T) {
	t.Setenv("E2E_DISCONNECTED_CLEANUP_SCRIPT", "")
	if Cleanup(context.Background()) == nil {
		t.Fatal("missing cleanup script must not silently restore egress")
	}
}
