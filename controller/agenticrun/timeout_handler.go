package agenticrun

import (
	"context"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
)

const sandboxTimeoutCheckInterval = 1 * time.Minute

// isSandboxClaimMode returns true if the current configuration selects
// sandbox-claim mode. When Sandbox CRDs are not installed, the config
// cache forces bare-pod mode so this naturally returns false.
func (r *AgenticRunReconciler) isSandboxClaimMode() bool {
	cfg := r.Config.Get()
	return cfg != nil && cfg.Sandbox.Mode == sandboxModeSandboxClaim
}

// runTimeoutLoop dispatches to the mode-appropriate timeout handler.
// Stopped when ctx is cancelled (manager shutdown).
func (r *AgenticRunReconciler) runTimeoutLoop(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(sandboxTimeoutCheckInterval):
			r.handleTimeEvent(ctx)
		}
	}
}

// handleTimeEvent collects sandbox pods based on the current mode and
// checks each for start/overall timeouts, retrying completion for
// terminal pods whose step condition patch failed earlier.
type podEntry struct {
	pod     *corev1.Pod
	step    string
	runName string
}

func (r *AgenticRunReconciler) handleTimeEvent(ctx context.Context) {
	log := logf.FromContext(ctx).WithName("sandbox-timeout")

	var entries []podEntry
	if r.isSandboxClaimMode() {
		entries = r.listSandboxPods(ctx, log)
	} else {
		entries = r.listBarePods(ctx, log)
	}

	now := time.Now()
	for _, e := range entries {
		condType := stepConditionType(e.step)

		var run agenticv1alpha1.AgenticRun
		if err := r.Get(ctx, client.ObjectKey{Name: e.runName, Namespace: r.Namespace}, &run); err != nil {
			continue
		}

		if !isStepInProgress(&run, condType) {
			continue
		}

		phase := e.pod.Status.Phase
		if phase == corev1.PodSucceeded || phase == corev1.PodFailed {
			log.Info("retrying completion for terminal pod", LogKeyName, e.pod.Name, LogKeyStep, e.step)
			_ = r.completeStep(ctx, &run, e.pod, e.step, condType, "")
			continue
		}

		timeout := resolveOverallTimeout(ctx, r.Client, &run, e.step)
		var message string
		created := e.pod.CreationTimestamp.Time
		if startTimedOut(phase, created, now, podStartTimeout) {
			message = fmt.Sprintf("sandbox pod did not start within %s", podStartTimeout)
		} else if overallTimedOut(created, now, timeout) {
			message = fmt.Sprintf("sandbox exceeded timeout %s", timeout)
		} else {
			continue
		}

		_ = r.completeStep(ctx, &run, e.pod, e.step, condType, message)
	}
}

// ---------------------------------------------------------------------------
// Pod listing by mode
// ---------------------------------------------------------------------------

func (r *AgenticRunReconciler) listBarePods(ctx context.Context, log logr.Logger) []podEntry {
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(r.Namespace), client.HasLabels{LabelRun, LabelStep}); err != nil {
		log.Error(err, "failed to list sandbox pods")
		return nil
	}
	var entries []podEntry
	for i := range pods.Items {
		pod := &pods.Items[i]
		step, runName := resolveBarePodMetadata(pod)
		if step == "" || runName == "" {
			continue
		}
		entries = append(entries, podEntry{pod, step, runName})
	}
	return entries
}

func (r *AgenticRunReconciler) listSandboxPods(ctx context.Context, log logr.Logger) []podEntry {
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(r.Namespace), client.HasLabels{"agents.x-k8s.io/sandbox-name-hash"}); err != nil {
		log.Error(err, "failed to list sandbox pods")
		return nil
	}
	var entries []podEntry
	for i := range pods.Items {
		pod := &pods.Items[i]
		step, runName, _ := resolveSandboxPodMetadata(ctx, r.Client, pod)
		if step == "" || runName == "" {
			continue
		}
		entries = append(entries, podEntry{pod, step, runName})
	}
	return entries
}

// resolveOverallTimeout computes the effective overall timeout (agent budget +
// startup padding) for a step of a given run. It reads the per-run override
// and the Agent CR to apply the layered precedence logic.
func resolveOverallTimeout(ctx context.Context, c client.Reader, run *agenticv1alpha1.AgenticRun, step string) time.Duration {
	runStep := runStepForName(run, step)
	var agent *agenticv1alpha1.Agent
	agentName := stepAgentName(runStep)
	a := &agenticv1alpha1.Agent{}
	if err := c.Get(ctx, client.ObjectKey{Name: agentName}, a); err == nil {
		agent = a
	}
	resolved := resolvedStep{Agent: agent, TimeoutMinutes: runStep.TimeoutMinutes}
	return effectiveStepTimeout(step, resolved) + defaultSandboxTimeout
}

// runStepForName returns the AgenticRunStep for a given step name.
func runStepForName(run *agenticv1alpha1.AgenticRun, step string) agenticv1alpha1.AgenticRunStep {
	switch step {
	case "analysis", "escalation":
		return run.Spec.Analysis
	case "execution":
		return run.Spec.Execution
	case "verification":
		return run.Spec.Verification
	default:
		return run.Spec.Analysis
	}
}

// ---------------------------------------------------------------------------
// Shared timeout helpers
// ---------------------------------------------------------------------------

// startTimedOut returns true if the resource has not reached Running within the deadline.
// For bare-pod mode, phase is checked to skip already-terminal pods.
// For sandbox-claim mode, pass an empty string as phase (caller checks Ready separately).
func startTimedOut(phase corev1.PodPhase, created, now time.Time, timeout time.Duration) bool {
	if phase == corev1.PodRunning || phase == corev1.PodSucceeded || phase == corev1.PodFailed {
		return false
	}
	return now.Sub(created) > timeout
}

func overallTimedOut(created, now time.Time, timeout time.Duration) bool {
	return now.Sub(created) > timeout
}
