package agenticrun

import (
	"context"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
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
	if err := r.sweepExpiredRuns(ctx); err != nil {
		logf.FromContext(ctx).Error(err, "failed to sweep expired AgenticRuns")
	}
	for i := 1; ; i++ {
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(sandboxTimeoutCheckInterval):
			r.handlePodTimeEvent(ctx)
			if i%60 == 0 {
				if err := r.sweepExpiredRuns(ctx); err != nil {
					logf.FromContext(ctx).Error(err, "failed to sweep expired AgenticRuns")
				}
			}
		}
	}
}

func (r *AgenticRunReconciler) sweepExpiredRuns(ctx context.Context) error {
	var runs agenticv1alpha1.AgenticRunList
	if err := r.List(ctx, &runs, client.MatchingLabels{terminalTTLLabel: "true"}); err != nil {
		return fmt.Errorf("list terminal AgenticRuns: %w", err)
	}
	now := time.Now()
	for i := range runs.Items {
		run := &runs.Items[i]
		if run.Status.DeleteAfter == nil || now.Before(run.Status.DeleteAfter.Time) {
			continue
		}
		phase := agenticv1alpha1.DerivePhase(run.Status.Conditions)
		if !isTerminal(phase) || (phase == agenticv1alpha1.AgenticRunPhaseFailed && preserveFailedSandbox(run)) {
			continue
		}
		if err := r.Delete(ctx, run); client.IgnoreNotFound(err) != nil {
			logf.FromContext(ctx).Error(fmt.Errorf("%s: %w", ErrDeleteExpiredRun, err), "failed to delete expired AgenticRun", LogKeyName, run.Name)
		}
	}
	return nil
}

// handlePodTimeEvent collects sandbox pods based on the current mode and
// checks each for start/overall timeouts, retrying completion for
// terminal pods whose step condition patch failed earlier.
type podEntry struct {
	pod     *corev1.Pod
	step    string
	runName string
	// created is the sandbox resource creation time. In claim mode this is
	// the claim timestamp, not the backing pod timestamp.
	created time.Time
}

func (r *AgenticRunReconciler) handlePodTimeEvent(ctx context.Context) {
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

		// A SandboxClaim can exist before its backing Pod is created. Enforce
		// the startup clock from the claim timestamp instead of waiting for a
		// pod entry to appear.
		if e.pod == nil {
			if now.Sub(e.created) > podStartTimeout {
				message := fmt.Sprintf("sandbox did not start within %s", podStartTimeout)
				if err := r.patchStepCondition(ctx, &run, condType, metav1.ConditionFalse, ReasonSandboxStartupTimeout, message); err == nil {
					r.releaseSandbox(ctx, &run, e.step)
				}
			}
			continue
		}

		phase := e.pod.Status.Phase
		if phase == corev1.PodSucceeded || phase == corev1.PodFailed {
			log.Info("retrying completion for terminal pod", LogKeyName, e.pod.Name, LogKeyStep, e.step)
			_ = r.completeStep(ctx, &run, e.pod, e.step, condType, "", "")
			continue
		}

		var message, reason string
		created := e.created
		if created.IsZero() {
			created = e.pod.CreationTimestamp.Time
		}
		if startTimedOut(phase, created, now, podStartTimeout) {
			message = fmt.Sprintf("sandbox pod did not start within %s", podStartTimeout)
			reason = ReasonSandboxStartupTimeout
		} else {
			// Resolve the agent for this step to get the configured timeout
			agent, err := r.resolveStepAgent(ctx, &run, e.step)
			if err != nil {
				log.Error(err, "failed to resolve step agent", LogKeyStep, e.step)
				continue
			}

			agentTimeoutSecs := resolveTimeout(agent, e.step)
			agentTimeout := time.Duration(agentTimeoutSecs) * time.Second

			startedAt := mainContainerStartedAt(e.pod)
			// Keep a creation-based upper bound across container restarts. The
			// startedAt clock additionally enforces the current container run.
			creationDeadline := sandboxStartupTimeout + agentTimeout + sandboxRunningGrace
			if overallTimedOut(created, now, creationDeadline) ||
				(!startedAt.IsZero() && overallTimedOut(startedAt, now, agentTimeout+sandboxRunningGrace)) {
				message = fmt.Sprintf("sandbox exceeded timeout %s", agentTimeout+sandboxRunningGrace)
				reason = ReasonSandboxTimeout
			} else {
				continue
			}
		}

		_ = r.completeStep(ctx, &run, e.pod, e.step, condType, message, reason)
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
		entries = append(entries, podEntry{pod: pod, step: step, runName: runName, created: pod.CreationTimestamp.Time})
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
	seen := make(map[string]bool)
	for i := range pods.Items {
		pod := &pods.Items[i]
		step, runName, err := resolveSandboxPodMetadata(ctx, r.Client, pod)
		if err != nil {
			// Do not let the claim-only fallback classify this pod-backed step
			// as still provisioning when its owner chain cannot be resolved.
			log.Error(err, "failed to resolve sandbox pod metadata", LogKeyName, pod.Name)
			return entries
		}
		if step == "" || runName == "" {
			continue
		}
		created := pod.CreationTimestamp.Time
		if claimCreated, err := sandboxClaimCreationTime(ctx, r.Client, pod); err == nil && !claimCreated.IsZero() {
			created = claimCreated
		}
		entries = append(entries, podEntry{pod: pod, step: step, runName: runName, created: created})
		seen[runName+"\x00"+step] = true
	}

	// Include claims with no backing pod so a claim stuck in provisioning still
	// reaches the five-minute startup deadline.
	claims := &unstructured.UnstructuredList{}
	claims.SetGroupVersionKind(smClaimGVK)
	if err := r.List(ctx, claims, client.InNamespace(r.Namespace)); err != nil {
		log.Error(err, "failed to list sandbox claims")
		return entries
	}
	for i := range claims.Items {
		claim := &claims.Items[i]
		labels := claim.GetLabels()
		annotations := claim.GetAnnotations()
		step, runName := labels[LabelStep], annotations[AnnotationRunName]
		if step == "" || runName == "" || seen[runName+"\x00"+step] {
			continue
		}
		entries = append(entries, podEntry{step: step, runName: runName, created: claim.GetCreationTimestamp().Time})
	}
	return entries
}

// ---------------------------------------------------------------------------
// Shared timeout helpers
// ---------------------------------------------------------------------------

// resolveStepAgent gets the Agent CR for the given step. It handles both
// the configured agent for the step and any approval overrides.
func (r *AgenticRunReconciler) resolveStepAgent(ctx context.Context, run *agenticv1alpha1.AgenticRun, step string) (*agenticv1alpha1.Agent, error) {
	var approval agenticv1alpha1.AgenticRunApproval
	if err := r.Get(ctx, client.ObjectKey{Name: run.Name, Namespace: run.Namespace}, &approval); err != nil && !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("get AgenticRunApproval: %w", err)
	}
	var agentName string
	switch step {
	case "analysis":
		agentName = effectiveStepAgentName(&approval, agenticv1alpha1.SandboxStepAnalysis, run.Spec.Analysis)
	case "execution":
		agentName = effectiveStepAgentName(&approval, agenticv1alpha1.SandboxStepExecution, run.Spec.Execution)
	case "verification":
		agentName = effectiveStepAgentName(&approval, agenticv1alpha1.SandboxStepVerification, run.Spec.Verification)
	case "escalation":
		// Escalation falls back to the resolved analysis Agent, unless it
		// has an explicit escalation override.
		agentName = effectiveStepAgentName(&approval, agenticv1alpha1.SandboxStepAnalysis, run.Spec.Analysis)
		if override := getStageOverrideAgent(&approval, agenticv1alpha1.SandboxStepEscalation); override != "" {
			agentName = override
		}
	}

	agent := &agenticv1alpha1.Agent{}
	if err := r.Get(ctx, client.ObjectKey{Name: agentName}, agent); err != nil {
		return nil, err
	}

	return agent, nil
}

// startTimedOut returns true if the resource has not reached Running within the deadline.
// For bare-pod mode, phase is checked to skip already-terminal pods.
// For sandbox-claim mode, pass an empty string as phase (caller checks Ready separately).
func startTimedOut(phase corev1.PodPhase, created, now time.Time, timeout time.Duration) bool {
	if phase == corev1.PodRunning || phase == corev1.PodSucceeded || phase == corev1.PodFailed {
		return false
	}
	return now.Sub(created) > timeout
}

func overallTimedOut(startedAt, now time.Time, timeout time.Duration) bool {
	return now.Sub(startedAt) > timeout
}

func mainContainerStartedAt(pod *corev1.Pod) time.Time {
	if pod == nil {
		return time.Time{}
	}
	for _, status := range pod.Status.ContainerStatuses {
		if status.Name == "agent" && status.State.Running != nil {
			return status.State.Running.StartedAt.Time
		}
	}
	for _, status := range pod.Status.ContainerStatuses {
		if status.State.Running != nil {
			return status.State.Running.StartedAt.Time
		}
	}
	return time.Time{}
}

func sandboxClaimCreationTime(ctx context.Context, c client.Client, pod *corev1.Pod) (time.Time, error) {
	for _, ref := range pod.OwnerReferences {
		if ref.Kind != "Sandbox" {
			continue
		}
		sb := &unstructured.Unstructured{}
		sb.SetGroupVersionKind(smSandboxGVK)
		if err := c.Get(ctx, client.ObjectKey{Name: ref.Name, Namespace: pod.Namespace}, sb); err != nil {
			return time.Time{}, err
		}
		for _, sbRef := range sb.GetOwnerReferences() {
			if sbRef.Kind != "SandboxClaim" {
				continue
			}
			claim := &unstructured.Unstructured{}
			claim.SetGroupVersionKind(smClaimGVK)
			if err := c.Get(ctx, client.ObjectKey{Name: sbRef.Name, Namespace: pod.Namespace}, claim); err != nil {
				return time.Time{}, err
			}
			return claim.GetCreationTimestamp().Time, nil
		}
	}
	return time.Time{}, nil
}
