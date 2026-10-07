package disconnected

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
)

// RecordRun retains sandbox ownership after per-scenario cleanup deletes the run.
func RecordRun(namespace, name, uid string) error {
	path := os.Getenv("E2E_DISCONNECTED_RUNS_FILE")
	if path == "" {
		return fmt.Errorf("E2E_DISCONNECTED_RUNS_FILE is required")
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	if err := json.NewEncoder(file).Encode(map[string]string{"namespace": namespace, "name": name, "uid": uid}); err != nil {
		return err
	}
	return file.Sync()
}

// Cleanup uses the same stop/wait/verify ordering as hard-timeout recovery.
// Never delete policies directly if the authoritative cleanup fails.
func Cleanup(ctx context.Context) error {
	path := os.Getenv("E2E_DISCONNECTED_CLEANUP_SCRIPT")
	if path == "" {
		return fmt.Errorf("E2E_DISCONNECTED_CLEANUP_SCRIPT is required")
	}
	output, err := exec.CommandContext(ctx, "bash", path).CombinedOutput()
	if err != nil {
		return fmt.Errorf("disconnected cleanup: %w: %s", err, redact(string(output)))
	}
	return nil
}
