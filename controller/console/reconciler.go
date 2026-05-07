package console

import (
	"context"
	"fmt"
	"slices"

	consolev1 "github.com/openshift/api/console/v1"
	openshiftv1 "github.com/openshift/api/operator/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	pluginName     = "lightspeed-agentic-console-plugin"
	pluginPort     = 9443
	certSecretName = pluginName + "-cert"
	consoleCRName  = "cluster"

	servingCertAnnotation = "service.beta.openshift.io/serving-cert-secret-name"

	nginxConfig = `error_log /dev/stdout info;
events {}
http {
  include            /etc/nginx/mime.types;
  default_type       application/octet-stream;
  keepalive_timeout  65;
  server {
    listen              9443 ssl;
    ssl_certificate     /var/cert/tls.crt;
    ssl_certificate_key /var/cert/tls.key;
    root                /usr/share/nginx/html;
    access_log          /dev/stdout;
  }
}
`
)

type AgenticConsoleConfig struct {
	Image     string
	Namespace string
}

func EnsureAgenticConsole(ctx context.Context, c client.Client, cfg AgenticConsoleConfig) error {
	log := logf.FromContext(ctx).WithName("agentic-console")

	if cfg.Image == "" {
		log.Info("No agentic console image configured — skipping console plugin deployment")
		return nil
	}

	log.Info("Ensuring agentic console plugin", "image", cfg.Image, "namespace", cfg.Namespace)

	for _, fn := range []struct {
		name string
		fn   func(context.Context, client.Client, AgenticConsoleConfig) error
	}{
		{"ConfigMap", ensureConfigMap},
		{"ServiceAccount", ensureServiceAccount},
		{"Service", ensureService},
		{"Deployment", ensureDeployment},
		{"ConsolePlugin", ensureConsolePlugin},
		{"ConsoleActivation", ensureConsoleActivation},
	} {
		if err := fn.fn(ctx, c, cfg); err != nil {
			return fmt.Errorf("ensure %s: %w", fn.name, err)
		}
		log.V(1).Info("Resource ready", "resource", fn.name)
	}

	log.Info("Agentic console plugin deployed")
	return nil
}

func labels() map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":       pluginName,
		"app.kubernetes.io/component":  "console",
		"app.kubernetes.io/managed-by": "lightspeed-operator",
	}
}

func ensureConfigMap(ctx context.Context, c client.Client, cfg AgenticConsoleConfig) error {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: pluginName, Namespace: cfg.Namespace, Labels: labels()},
	}
	_, err := controllerutil.CreateOrUpdate(ctx, c, cm, func() error {
		cm.Data = map[string]string{"nginx.conf": nginxConfig}
		return nil
	})
	return err
}

func ensureServiceAccount(ctx context.Context, c client.Client, cfg AgenticConsoleConfig) error {
	sa := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{Name: pluginName, Namespace: cfg.Namespace, Labels: labels()},
	}
	if err := c.Create(ctx, sa); err != nil && !errors.IsAlreadyExists(err) {
		return err
	}
	return nil
}

func ensureService(ctx context.Context, c client.Client, cfg AgenticConsoleConfig) error {
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: pluginName, Namespace: cfg.Namespace, Labels: labels()},
	}
	_, err := controllerutil.CreateOrUpdate(ctx, c, svc, func() error {
		svc.Annotations = map[string]string{servingCertAnnotation: certSecretName}
		svc.Spec.Selector = map[string]string{"app.kubernetes.io/name": pluginName}
		svc.Spec.Ports = []corev1.ServicePort{{
			Name:       "https",
			Port:       pluginPort,
			TargetPort: intstr.FromInt32(pluginPort),
			Protocol:   corev1.ProtocolTCP,
		}}
		return nil
	})
	return err
}

func ensureDeployment(ctx context.Context, c client.Client, cfg AgenticConsoleConfig) error {
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: pluginName, Namespace: cfg.Namespace, Labels: labels()},
	}
	_, err := controllerutil.CreateOrUpdate(ctx, c, dep, func() error {
		dep.Spec.Replicas = ptr.To(int32(1))
		dep.Spec.Selector = &metav1.LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/name": pluginName}}
		dep.Spec.Template = corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Labels: labels()},
			Spec: corev1.PodSpec{
				ServiceAccountName: pluginName,
				SecurityContext: &corev1.PodSecurityContext{
					RunAsNonRoot:   ptr.To(true),
					SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
				},
				Containers: []corev1.Container{{
					Name:            "console",
					Image:           cfg.Image,
					ImagePullPolicy: corev1.PullAlways,
					Ports: []corev1.ContainerPort{{
						ContainerPort: pluginPort,
						Protocol:      corev1.ProtocolTCP,
					}},
					SecurityContext: &corev1.SecurityContext{
						AllowPrivilegeEscalation: ptr.To(false),
						Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
					},
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("10m"),
							corev1.ResourceMemory: resource.MustParse("50Mi"),
						},
						Limits: corev1.ResourceList{
							corev1.ResourceMemory: resource.MustParse("100Mi"),
						},
					},
					VolumeMounts: []corev1.VolumeMount{
						{Name: "cert", MountPath: "/var/cert", ReadOnly: true},
						{Name: "nginx-conf", MountPath: "/etc/nginx/nginx.conf", SubPath: "nginx.conf", ReadOnly: true},
						{Name: "nginx-tmp", MountPath: "/tmp/nginx"},
					},
				}},
				Volumes: []corev1.Volume{
					{Name: "cert", VolumeSource: corev1.VolumeSource{
						Secret: &corev1.SecretVolumeSource{SecretName: certSecretName},
					}},
					{Name: "nginx-conf", VolumeSource: corev1.VolumeSource{
						ConfigMap: &corev1.ConfigMapVolumeSource{
							LocalObjectReference: corev1.LocalObjectReference{Name: pluginName},
						},
					}},
					{Name: "nginx-tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
				},
			},
		}
		return nil
	})
	return err
}

func ensureConsolePlugin(ctx context.Context, c client.Client, cfg AgenticConsoleConfig) error {
	plugin := &consolev1.ConsolePlugin{
		ObjectMeta: metav1.ObjectMeta{Name: pluginName, Labels: labels()},
	}
	_, err := controllerutil.CreateOrUpdate(ctx, c, plugin, func() error {
		plugin.Spec = consolev1.ConsolePluginSpec{
			DisplayName: "OpenShift Lightspeed Agentic Console Plugin",
			Backend: consolev1.ConsolePluginBackend{
				Type: consolev1.Service,
				Service: &consolev1.ConsolePluginService{
					Name:      pluginName,
					Namespace: cfg.Namespace,
					Port:      pluginPort,
					BasePath:  "/",
				},
			},
			I18n: consolev1.ConsolePluginI18n{LoadType: consolev1.Preload},
		}
		return nil
	})
	return err
}

func ensureConsoleActivation(ctx context.Context, c client.Client, _ AgenticConsoleConfig) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		console := &openshiftv1.Console{}
		if err := c.Get(ctx, types.NamespacedName{Name: consoleCRName}, console); err != nil {
			if errors.IsNotFound(err) {
				return fmt.Errorf("Console CR %q not found — OpenShift Console operator may not be installed", consoleCRName)
			}
			return fmt.Errorf("get Console CR: %w", err)
		}
		if slices.Contains(console.Spec.Plugins, pluginName) {
			return nil
		}
		console.Spec.Plugins = append(console.Spec.Plugins, pluginName)
		return c.Update(ctx, console)
	})
}
