package main

import (
	"context"
	"errors"
	"flag"
	"os"
	"syscall"
	"time"

	// Import auth plugins (Azure, GCP, OIDC, etc.) for local and hosted kubeconfigs.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	uberzap "go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
	agenticcontroller "github.com/openshift/lightspeed-agentic-operator/controller"
	"github.com/openshift/lightspeed-agentic-operator/controller/agenticrun"
	"github.com/openshift/lightspeed-agentic-operator/pkg/configwatch"
	"github.com/openshift/lightspeed-agentic-operator/pkg/telemetry"
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(agenticv1alpha1.AddToScheme(scheme))
}

const telemetryStartupTimeout = 5 * time.Minute

func main() {
	var (
		metricsAddr         string
		healthAddr          string
		namespace           string
		agenticSandboxImage string
		sandboxMode         string
		imagePullPolicy     string
	)

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metric endpoint binds to.")
	flag.StringVar(&healthAddr, "health-probe-bind-address", ":8081", "The address the health probe endpoint binds to.")
	flag.StringVar(&namespace, "namespace", "", "The namespace where the operator runs (required).")
	flag.StringVar(&agenticSandboxImage, "agentic-sandbox-image", "", "The image of the agentic sandbox container.")
	flag.StringVar(&sandboxMode, "sandbox-mode", "bare-pod", "Sandbox mode: bare-pod (default) or sandbox-claim.")
	flag.StringVar(&imagePullPolicy, "image-pull-policy", "", "Image pull policy for sandbox pods (Always, IfNotPresent, Never). Empty uses Kubernetes default.")
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseDevMode(true)))
	log := ctrl.Log.WithName("setup")

	switch corev1.PullPolicy(imagePullPolicy) {
	case "", corev1.PullAlways, corev1.PullIfNotPresent, corev1.PullNever:
	default:
		log.Error(nil, "invalid --image-pull-policy", "value", imagePullPolicy, "allowed", "Always|IfNotPresent|Never")
		os.Exit(1)
	}

	if namespace == "" {
		ns := os.Getenv("POD_NAMESPACE")
		if ns == "" {
			log.Error(nil, "--namespace flag or POD_NAMESPACE env var is required")
			os.Exit(1)
		}
		namespace = ns
	}

	cfg, err := config.GetConfig()
	if err != nil {
		log.Error(err, "unable to get Kubernetes config")
		os.Exit(1)
	}

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: healthAddr,
		WebhookServer: webhook.NewServer(webhook.Options{
			Port:    9443,
			CertDir: "/tmp/k8s-webhook-server/serving-certs",
		}),
	})
	if err != nil {
		log.Error(err, "unable to create manager")
		os.Exit(1)
	}

	// Initialize telemetry provider from Collector ConfigMap
	telemetryProvider := telemetry.NewProvider(&agenticrun.AgenticRunIDGenerator{})
	telemetryProvider.SetSecretSource(mgr.GetAPIReader(), namespace)
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := telemetryProvider.Shutdown(shutdownCtx); err != nil {
			log.Error(err, "failed to shutdown telemetry provider")
		}
	}()

	// Block until Collector ConfigMap is available (fatal after timeout)
	if err := configwatch.WaitFor(
		context.Background(), mgr.GetAPIReader(), namespace,
		telemetry.ConfigMapName, telemetryStartupTimeout,
		telemetryProvider.OnConfigMapChange,
	); err != nil {
		log.Error(err, "telemetry configuration failed")
		os.Exit(1)
	}

	// Register ConfigMap watcher for runtime reconfiguration
	cmWatcher := configwatch.New(mgr.GetClient(), namespace,
		configwatch.Registration{Name: telemetry.ConfigMapName, Handler: telemetryProvider.OnConfigMapChange},
	)
	if err := cmWatcher.SetupWithManager(mgr); err != nil {
		log.Error(err, "unable to set up ConfigMap watcher")
		os.Exit(1)
	}

	// Create AuditLogger (always enabled — stdout audit is unconditional)
	zapLogger, err := uberzap.NewProduction()
	if err != nil {
		log.Error(err, "unable to create zap logger for audit")
		os.Exit(1)
	}
	defer func() {
		if syncErr := zapLogger.Sync(); syncErr != nil && !errors.Is(syncErr, syscall.EINVAL) {
			log.Error(syncErr, "failed to sync zap logger")
		}
	}()
	auditLogger := agenticrun.NewProductionAuditLogger(zapLogger, telemetryProvider)

	if err := agenticcontroller.Setup(mgr, agenticcontroller.Options{
		Namespace:           namespace,
		AgenticSandboxImage: agenticSandboxImage,
		SandboxMode:         sandboxMode,
		ImagePullPolicy:     imagePullPolicy,
		Audit:               auditLogger,
		TempLog:             telemetryProvider,
	}); err != nil {
		log.Error(err, "unable to set up agentic controllers")
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		log.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		log.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	log.Info("starting manager", "namespace", namespace)
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		log.Error(err, "problem running manager")
		// os.Exit skips deferred Shutdown; flush buffered telemetry explicitly.
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if shutErr := telemetryProvider.Shutdown(shutdownCtx); shutErr != nil {
			log.Error(shutErr, "failed to shutdown telemetry provider")
		}
		cancel()
		os.Exit(1)
	}
}
