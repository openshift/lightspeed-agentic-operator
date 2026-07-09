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

const (
	ErrBuildPodSpec = "build pod spec"
	ErrCreatePod    = "create pod for"
	ErrDeletePod    = "delete pod"

	defaultDeletionTimeout = 2 * time.Minute
)

// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;create;delete

// BarePodManager is a SandboxProvider that creates bare Pods using PodSpecBuilder
// instead of relying on the Sandbox API (SandboxClaim/SandboxTemplate).
type BarePodManager struct {
	Client          client.Client
	Builder         *PodSpecBuilder
	Namespace       string
	DeletionTimeout time.Duration

	agent          *agenticv1alpha1.Agent
	llm            *agenticv1alpha1.LLMProvider
	tools          *agenticv1alpha1.ToolsSpec
	serviceAccount string
}

// NewBarePodManager creates a BarePodManager that manages bare Pods in the given namespace.
func NewBarePodManager(c client.Client, builder *PodSpecBuilder, namespace string) *BarePodManager {
	return &BarePodManager{
		Client:    c,
		Builder:   builder,
		Namespace: namespace,
	}
}

// SetStep stores the resolved step configuration so that the next Claim
// call can build the correct PodSpec.
func (m *BarePodManager) SetStep(agent *agenticv1alpha1.Agent, llm *agenticv1alpha1.LLMProvider, tools *agenticv1alpha1.ToolsSpec, serviceAccount string) {
	m.agent = agent
	m.llm = llm
	m.tools = tools
	m.serviceAccount = serviceAccount
}

// Claim creates a bare Pod for the given proposal step. The templateName
// parameter is ignored (bare pods use PodSpecBuilder instead of templates).
// Returns the pod name. Idempotent: returns the name if the pod already exists.
func (m *BarePodManager) Claim(ctx context.Context, proposalName, step, _ string) (string, error) {
	log := logf.FromContext(ctx)

	podName := truncateK8sName(fmt.Sprintf("ls-%s-%s", step, proposalName))

	podSpec, err := m.Builder.Build(m.agent, m.llm, m.tools, step, m.serviceAccount)
	if err != nil {
		return "", fmt.Errorf("%s: %w", ErrBuildPodSpec, err)
	}

	if err := appendAuditEnvVars(ctx, m.Client, &podSpec.Containers[0]); err != nil {
		return "", fmt.Errorf("append audit env vars: %w", err)
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: m.Namespace,
			Labels: map[string]string{
				LabelProposal: truncateK8sName(proposalName),
				LabelStep:     step,
			},
		},
		Spec: *podSpec,
	}

	if err := m.Client.Create(ctx, pod); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return "", fmt.Errorf("%s %s: %w", ErrCreatePod, step, err)
		}

		var existing corev1.Pod
		key := types.NamespacedName{Name: podName, Namespace: m.Namespace}
		if getErr := m.Client.Get(ctx, key, &existing); getErr != nil {
			return "", fmt.Errorf("get existing pod %q: %w", podName, getErr)
		}
		if existing.DeletionTimestamp.IsZero() {
			return podName, nil
		}

		log.Info("Waiting for terminating pod to disappear", LogKeyName, podName)
		if err := m.waitForDeletion(ctx, key); err != nil {
			return "", fmt.Errorf("wait for terminating pod %q: %w", podName, err)
		}
		if err := m.Client.Create(ctx, pod); err != nil {
			if apierrors.IsAlreadyExists(err) {
				return podName, nil
			}
			return "", fmt.Errorf("%s %s: %w", ErrCreatePod, step, err)
		}
	}

	log.Info("Created bare pod", LogKeyName, podName, LogKeyStep, step)
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
				log.V(1).Info("Waiting for pod", LogKeyName, podName)
				continue
			}

			for _, cond := range pod.Status.Conditions {
				if cond.Type == corev1.PodReady && cond.Status == corev1.ConditionTrue {
					if pod.Status.PodIP == "" {
						continue
					}
					log.Info("Pod ready", LogKeyName, podName, "podIP", pod.Status.PodIP)
					return pod.Status.PodIP, nil
				}
			}
		}
	}
}

// waitForDeletion polls until the named pod no longer exists. It is used
// by Claim to wait for a terminating pod to disappear before creating a
// replacement with the same name. The timeout is controlled by DeletionTimeout
// (defaults to defaultDeletionTimeout).
func (m *BarePodManager) waitForDeletion(ctx context.Context, key types.NamespacedName) error {
	timeout := m.DeletionTimeout
	if timeout == 0 {
		timeout = defaultDeletionTimeout
	}
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		var pod corev1.Pod
		err := m.Client.Get(ctx, key, &pod)
		switch {
		case apierrors.IsNotFound(err):
			return nil
		case err != nil:
			return fmt.Errorf("get pod %q: %w", key.Name, err)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timeout waiting for pod %q to be deleted after %s", key.Name, timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
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
		return fmt.Errorf("%s %q: %w", ErrDeletePod, podName, err)
	}

	log.Info("Released bare pod", LogKeyName, podName)
	return nil
}
