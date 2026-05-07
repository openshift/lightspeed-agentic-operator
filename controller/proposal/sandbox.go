package proposal

import (
	"context"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

var (
	sandboxClaimGVK = schema.GroupVersionKind{
		Group: "extensions.agents.x-k8s.io", Version: "v1alpha1", Kind: "SandboxClaim",
	}
	sandboxGVK = schema.GroupVersionKind{
		Group: "agents.x-k8s.io", Version: "v1alpha1", Kind: "Sandbox",
	}
)

// SandboxProvider abstracts sandbox lifecycle for testability.
type SandboxProvider interface {
	Claim(ctx context.Context, proposalName, step, templateName string) (claimName string, err error)
	WaitReady(ctx context.Context, claimName string, timeout time.Duration) (endpoint string, err error)
	Release(ctx context.Context, claimName string) error
}

// SandboxManager handles SandboxClaim lifecycle for proposal execution.
type SandboxManager struct {
	Client    client.Client
	Namespace string
}

func NewSandboxManager(c client.Client, namespace string) *SandboxManager {
	return &SandboxManager{Client: c, Namespace: namespace}
}

func (m *SandboxManager) buildClaim(claimName, proposalName, step, templateName string) *unstructured.Unstructured {
	return &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": sandboxClaimGVK.Group + "/" + sandboxClaimGVK.Version,
			"kind":       sandboxClaimGVK.Kind,
			"metadata": map[string]any{
				"name":      claimName,
				"namespace": m.Namespace,
				"labels": map[string]any{
					LabelProposal: proposalName,
					LabelStep:    step,
				},
			},
			"spec": map[string]any{
				"sandboxTemplateRef": map[string]any{
					"name": templateName,
				},
				"lifecycle": map[string]any{
					"shutdownPolicy": "Delete",
				},
			},
		},
	}
}

func (m *SandboxManager) Claim(ctx context.Context, proposalName, step, templateName string) (string, error) {
	log := logf.FromContext(ctx)

	claimName := truncateK8sName(fmt.Sprintf("ls-%s-%s", step, proposalName))

	claim := m.buildClaim(claimName, proposalName, step, templateName)
	if err := m.Client.Create(ctx, claim); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return claimName, nil
		}
		return "", fmt.Errorf("failed to create SandboxClaim for %s: %w", step, err)
	}

	log.Info("Created SandboxClaim", "name", claimName, "step", step, "template", templateName)
	return claimName, nil
}

func (m *SandboxManager) WaitReady(ctx context.Context, claimName string, timeout time.Duration) (string, error) {
	log := logf.FromContext(ctx)

	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	claim := &unstructured.Unstructured{}
	sandbox := &unstructured.Unstructured{}
	claimKey := types.NamespacedName{Name: claimName, Namespace: m.Namespace}

	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-ticker.C:
			if time.Now().After(deadline) {
				return "", fmt.Errorf("timeout waiting for sandbox %q after %s", claimName, timeout)
			}

			claim.SetGroupVersionKind(sandboxClaimGVK)
			if err := m.Client.Get(ctx, claimKey, claim); err != nil {
				log.V(1).Info("Waiting for SandboxClaim", "name", claimName)
				continue
			}

			sandboxName, found, nestedErr := unstructured.NestedString(claim.Object, "status", "sandbox", "name")
			if nestedErr != nil {
				return "", fmt.Errorf("extract sandbox name from claim %q: %w", claimName, nestedErr)
			}
			if !found || sandboxName == "" {
				continue
			}

			sandbox.SetGroupVersionKind(sandboxGVK)
			if err := m.Client.Get(ctx, types.NamespacedName{
				Name: sandboxName, Namespace: m.Namespace,
			}, sandbox); err != nil {
				log.V(1).Info("Waiting for Sandbox", "name", sandboxName, "error", err)
				continue
			}

			conditions, found, nestedErr := unstructured.NestedSlice(sandbox.Object, "status", "conditions")
			if nestedErr != nil {
				return "", fmt.Errorf("extract conditions from sandbox %q: %w", sandboxName, nestedErr)
			}
			if !found {
				continue
			}

			for _, c := range conditions {
				cond, ok := c.(map[string]any)
				if !ok {
					continue
				}
				if cond["type"] == "Ready" && cond["status"] == string(metav1.ConditionTrue) {
					fqdn, fqdnFound, fqdnErr := unstructured.NestedString(sandbox.Object, "status", "serviceFQDN")
					if fqdnErr != nil {
						return "", fmt.Errorf("extract serviceFQDN from sandbox %q: %w", sandboxName, fqdnErr)
					}
					if !fqdnFound || fqdn == "" {
						continue
					}
					log.Info("Sandbox ready", "sandbox", sandboxName, "fqdn", fqdn)
					return fqdn, nil
				}
			}
		}
	}
}

func (m *SandboxManager) Release(ctx context.Context, claimName string) error {
	log := logf.FromContext(ctx)

	claim := &unstructured.Unstructured{}
	claim.SetGroupVersionKind(sandboxClaimGVK)
	claim.SetName(claimName)
	claim.SetNamespace(m.Namespace)

	if err := m.Client.Delete(ctx, claim); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("failed to delete SandboxClaim %q: %w", claimName, err)
	}

	log.Info("Released SandboxClaim", "name", claimName)
	return nil
}
