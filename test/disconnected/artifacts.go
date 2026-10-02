package disconnected

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
)

// ArchiveRun retains workflow evidence before the per-scenario finalizer cleanup.
func ArchiveRun(t *testing.T, namespace, name, uid string) {
	t.Helper()
	if os.Getenv("ARTIFACT_DIR") == "" {
		return
	}
	dir := filepath.Join(os.Getenv("ARTIFACT_DIR"), "disconnected", "runs", name)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Log(err)
		return
	}
	for _, resource := range []string{"agenticrun", "analysisresults", "executionresults", "verificationresults"} {
		args := []string{"get", resource, "-n", namespace, "-o", "yaml"}
		if resource == "agenticrun" {
			args = append(args, name)
		} else {
			args = append(args, "-l", RunLabel+"="+uid)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		data, err := exec.CommandContext(ctx, "oc", args...).CombinedOutput()
		cancel()
		if err != nil {
			t.Logf("archive %s: %v", resource, err)
		}
		if err := os.WriteFile(filepath.Join(dir, resource+".yaml"), []byte(redact(string(data))), 0600); err != nil {
			t.Log(err)
		}
	}
}

// ArchivePod records status/termination messages from watch events before GC.
func ArchivePod(t *testing.T, pod *corev1.Pod) {
	if os.Getenv("ARTIFACT_DIR") == "" {
		return
	}
	dir := filepath.Join(os.Getenv("ARTIFACT_DIR"), "disconnected", "sandbox-pods")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Log(err)
		return
	}
	if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		output, _ := exec.CommandContext(ctx, "oc", "logs", "-n", pod.Namespace, pod.Name, "--all-containers", "--tail=500").CombinedOutput()
		cancel()
		_ = os.WriteFile(filepath.Join(dir, pod.Name+".log"), []byte(redact(string(output))), 0600)
	}
	data, err := json.MarshalIndent(pod, "", "  ")
	if err != nil {
		t.Log(err)
		return
	}
	if err := os.WriteFile(filepath.Join(dir, pod.Name+".json"), []byte(redact(string(data))), 0600); err != nil {
		t.Log(err)
	}
}
