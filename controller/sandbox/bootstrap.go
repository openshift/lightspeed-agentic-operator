package sandbox

// +kubebuilder:rbac:groups="",resources=serviceaccounts,verbs=create
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterrolebindings,verbs=create

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

const templateName = "lightspeed-agent"

var sandboxTemplateGVK = schema.GroupVersionKind{
	Group: "extensions.agents.x-k8s.io", Version: "v1alpha1", Kind: "SandboxTemplate",
}

const (
	ErrEnsureSA                 = "ensure ServiceAccount"
	ErrEnsureClusterRoleBinding = "ensure ClusterRoleBinding"
	ErrEnsureSandboxTemplate    = "ensure SandboxTemplate"
)

type BootstrapConfig struct {
	Image       string
	Namespace   string
	SandboxMode string
}

func EnsureBootstrapResources(ctx context.Context, c client.Client, cfg BootstrapConfig) error {
	log := logf.FromContext(ctx).WithName("sandbox-bootstrap")

	if cfg.Image == "" {
		log.Info("No agentic sandbox image configured — skipping bootstrap")
		return nil
	}

	log.Info("Ensuring bootstrap resources", "image", cfg.Image, "namespace", cfg.Namespace, "mode", cfg.SandboxMode)

	if err := ensureServiceAccount(ctx, c, cfg.Namespace); err != nil {
		return fmt.Errorf("%s: %w", ErrEnsureSA, err)
	}
	log.V(1).Info("ServiceAccount ready")

	if err := ensureClusterRoleBinding(ctx, c, "lightspeed-agent-cluster-reader", "cluster-reader", templateName, cfg.Namespace); err != nil {
		return fmt.Errorf("%s %s: %w", ErrEnsureClusterRoleBinding, "cluster-reader", err)
	}
	if err := ensureClusterRoleBinding(ctx, c, "lightspeed-agent-monitoring-view", "cluster-monitoring-view", templateName, cfg.Namespace); err != nil {
		return fmt.Errorf("%s %s: %w", ErrEnsureClusterRoleBinding, "cluster-monitoring-view", err)
	}
	log.V(1).Info("Agent read ClusterRoleBindings ready")

	if cfg.SandboxMode == "sandbox-claim" {
		if err := ensureSandboxTemplate(ctx, c, cfg.Image, cfg.Namespace); err != nil {
			return fmt.Errorf("%s: %w", ErrEnsureSandboxTemplate, err)
		}
		log.V(1).Info("SandboxTemplate ready")
	}

	log.Info("Bootstrap complete", "mode", cfg.SandboxMode)
	return nil
}

func labels() map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":       templateName,
		"app.kubernetes.io/component":  "sandbox",
		"app.kubernetes.io/managed-by": "lightspeed-operator",
	}
}

func ensureServiceAccount(ctx context.Context, c client.Client, namespace string) error {
	sa := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      templateName,
			Namespace: namespace,
			Labels:    labels(),
		},
		AutomountServiceAccountToken: ptr.To(false),
	}
	if err := c.Create(ctx, sa); err != nil && !errors.IsAlreadyExists(err) {
		return err
	}
	return nil
}

func ensureClusterRoleBinding(ctx context.Context, c client.Client, name, clusterRoleName, saName, namespace string) error {
	crb := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: labels(),
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName,
			Kind:     "ClusterRole",
			Name:     clusterRoleName,
		},
		Subjects: []rbacv1.Subject{{
			Kind:      rbacv1.ServiceAccountKind,
			Name:      saName,
			Namespace: namespace,
		}},
	}
	if err := c.Create(ctx, crb); err != nil && !errors.IsAlreadyExists(err) {
		return err
	}
	return nil
}

func ensureSandboxTemplate(ctx context.Context, c client.Client, image, namespace string) error {
	tmpl := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": sandboxTemplateGVK.Group + "/" + sandboxTemplateGVK.Version,
			"kind":       sandboxTemplateGVK.Kind,
			"metadata": map[string]any{
				"name":      templateName,
				"namespace": namespace,
				"labels":    labelsAny(),
			},
			"spec": map[string]any{
				"networkPolicyManagement": "Unmanaged",
				"podTemplate": map[string]any{
					"metadata": map[string]any{
						"labels": map[string]any{
							"app.kubernetes.io/name": templateName,
						},
					},
					"spec": map[string]any{
						"serviceAccountName":           templateName,
						"automountServiceAccountToken": false,
						"securityContext": map[string]any{
							"runAsNonRoot": true,
							"seccompProfile": map[string]any{
								"type": "RuntimeDefault",
							},
						},
						"containers": []any{
							map[string]any{
								"name":  "agent",
								"image": image,
								"ports": []any{
									map[string]any{
										"name":          "http",
										"containerPort": int64(8080),
										"protocol":      "TCP",
									},
								},
								"securityContext": map[string]any{
									"allowPrivilegeEscalation": false,
									"runAsNonRoot":             true,
									"capabilities": map[string]any{
										"drop": []any{"ALL"},
									},
									"seccompProfile": map[string]any{
										"type": "RuntimeDefault",
									},
								},
							},
						},
						"volumes": []any{
							map[string]any{
								"name": "skills",
								"image": map[string]any{
									"reference": "placeholder:latest",
								},
							},
						},
					},
				},
			},
		},
	}
	if err := c.Create(ctx, tmpl); err != nil && !errors.IsAlreadyExists(err) {
		return err
	}
	return nil
}

func labelsAny() map[string]any {
	return map[string]any{
		"app.kubernetes.io/name":       templateName,
		"app.kubernetes.io/component":  "sandbox",
		"app.kubernetes.io/managed-by": "lightspeed-operator",
	}
}
