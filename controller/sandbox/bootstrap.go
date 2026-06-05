package sandbox

// +kubebuilder:rbac:groups="",resources=serviceaccounts,verbs=create

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
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

type BaseSandboxConfig struct {
	Image     string
	Namespace string
}

func EnsureBaseSandboxTemplate(ctx context.Context, c client.Client, cfg BaseSandboxConfig) error {
	log := logf.FromContext(ctx).WithName("sandbox-bootstrap")

	if cfg.Image == "" {
		log.Info("No agentic sandbox image configured — skipping base SandboxTemplate creation")
		return nil
	}

	log.Info("Ensuring base SandboxTemplate", "image", cfg.Image, "namespace", cfg.Namespace)

	if err := ensureServiceAccount(ctx, c, cfg); err != nil {
		return fmt.Errorf("ensure ServiceAccount: %w", err)
	}
	log.V(1).Info("ServiceAccount ready")

	if err := ensureSandboxTemplate(ctx, c, cfg); err != nil {
		return fmt.Errorf("ensure SandboxTemplate: %w", err)
	}
	log.V(1).Info("SandboxTemplate ready")

	log.Info("Base SandboxTemplate bootstrap complete")
	return nil
}

func labels() map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":       templateName,
		"app.kubernetes.io/component":  "sandbox",
		"app.kubernetes.io/managed-by": "lightspeed-operator",
	}
}

func ensureServiceAccount(ctx context.Context, c client.Client, cfg BaseSandboxConfig) error {
	sa := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      templateName,
			Namespace: cfg.Namespace,
			Labels:    labels(),
		},
		AutomountServiceAccountToken: ptr.To(false),
	}
	if err := c.Create(ctx, sa); err != nil && !errors.IsAlreadyExists(err) {
		return err
	}
	return nil
}

func ensureSandboxTemplate(ctx context.Context, c client.Client, cfg BaseSandboxConfig) error {
	tmpl := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": sandboxTemplateGVK.Group + "/" + sandboxTemplateGVK.Version,
			"kind":       sandboxTemplateGVK.Kind,
			"metadata": map[string]any{
				"name":      templateName,
				"namespace": cfg.Namespace,
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
						"containers": []any{
							map[string]any{
								"name":  "agent",
								"image": cfg.Image,
								"ports": []any{
									map[string]any{
										"name":          "http",
										"containerPort": int64(8080),
										"protocol":      "TCP",
									},
								},
								"securityContext": map[string]any{
									"allowPrivilegeEscalation": false,
									"capabilities": map[string]any{
										"drop": []any{"ALL"},
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
