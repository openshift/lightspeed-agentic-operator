package proposal

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
)

// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;create;delete

// BarePodManager is a SandboxProvider that creates bare Pods using PodSpecBuilder
// instead of relying on the Sandbox API (SandboxClaim/SandboxTemplate).
type BarePodManager struct {
	Client    client.Client
	Builder   *PodSpecBuilder
	Namespace string
}

// NewBarePodManager creates a BarePodManager that manages bare Pods in the given namespace.
func NewBarePodManager(c client.Client, builder *PodSpecBuilder, namespace string) *BarePodManager {
	return &BarePodManager{
		Client:    c,
		Builder:   builder,
		Namespace: namespace,
	}
}

// Claim creates a bare Pod for the given proposal step. Returns the pod
// name. Idempotent: returns the name if the pod already exists.
func (m *BarePodManager) Claim(ctx context.Context, proposalName, step string, agent *agenticv1alpha1.Agent, llm *agenticv1alpha1.LLMProvider, tools *agenticv1alpha1.ToolsSpec) (string, error) {
	log := logf.FromContext(ctx)

	podName := truncateK8sName(fmt.Sprintf("ls-%s-%s", step, proposalName))

	podSpec, err := m.Builder.Build(agent, llm, tools, step)
	if err != nil {
		return "", fmt.Errorf("build pod spec: %w", err)
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: m.Namespace,
			Labels: map[string]string{
				LabelProposal: proposalName,
				LabelStep:     step,
			},
		},
		Spec: *podSpec,
	}

	if err := m.Client.Create(ctx, pod); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return podName, nil
		}
		return "", fmt.Errorf("create pod for %s: %w", step, err)
	}

	log.Info("Created bare pod", "name", podName, "step", step)
	return podName, nil
}

// WaitReady polls the Pod until it is Ready with a non-empty PodIP, then
// returns the IP address. Returns an error on timeout or context cancellation.
func (m *BarePodManager) WaitReady(ctx context.Context, podName string, timeout time.Duration) (string, error) {
	log := logf.FromContext(ctx)

	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	key := types.NamespacedName{Name: podName, Namespace: m.Namespace}

	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-ticker.C:
			if time.Now().After(deadline) {
				return "", fmt.Errorf("timeout waiting for pod %q after %s", podName, timeout)
			}

			var pod corev1.Pod
			if err := m.Client.Get(ctx, key, &pod); err != nil {
				log.V(1).Info("Waiting for pod", "name", podName)
				continue
			}

			for _, cond := range pod.Status.Conditions {
				if cond.Type == corev1.PodReady && cond.Status == corev1.ConditionTrue {
					if pod.Status.PodIP == "" {
						continue
					}
					log.Info("Pod ready", "name", podName, "podIP", pod.Status.PodIP)
					return pod.Status.PodIP, nil
				}
			}
		}
	}
}

// Release deletes the bare Pod. Idempotent: returns nil if the pod is
// already gone.
func (m *BarePodManager) Release(ctx context.Context, podName string) error {
	log := logf.FromContext(ctx)

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: m.Namespace,
		},
	}

	if err := m.Client.Delete(ctx, pod); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("delete pod %q: %w", podName, err)
	}

	log.Info("Released bare pod", "name", podName)
	return nil
}
