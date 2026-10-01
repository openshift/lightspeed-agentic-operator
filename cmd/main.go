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
	apiextensionsclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
	"github.com/openshift/lightspeed-agentic-operator/controller/agenticolsconfig"
	"github.com/openshift/lightspeed-agentic-operator/controller/agenticrun"
	"github.com/openshift/lightspeed-agentic-operator/pkg/configuration"
	"github.com/openshift/lightspeed-agentic-operator/pkg/configwatch"
	"github.com/openshift/lightspeed-agentic-operator/pkg/ocpversion"
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(agenticv1alpha1.AddToScheme(scheme))
}

func main() {
	var (
		metricsAddr string
		healthAddr  string
		namespace   string
	)

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metric endpoint binds to.")
	flag.StringVar(&healthAddr, "health-probe-bind-address", ":8081", "The address the health probe endpoint binds to.")
	flag.StringVar(&namespace, "namespace", "", "The namespace where the operator runs (required).")
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseDevMode(true)))
	log := ctrl.Log.WithName("setup")

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

	versionGate := &ocpversion.Gate{Reader: mgr.GetAPIReader()}

	// Initialize configuration cache and OTEL provider
	telemetryProvider := configuration.NewProvider(&agenticrun.AgenticRunIDGenerator{})
	telemetryProvider.SetSecretSource(mgr.GetAPIReader(), namespace)
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := telemetryProvider.Shutdown(shutdownCtx); err != nil {
			log.Error(err, "failed to shutdown telemetry provider")
		}
	}()

	// --- Check for Sandbox CRDs (determines sandbox-claim mode availability) ---
	sandboxCRDs := sandboxCRDInstalled(cfg)
	if sandboxCRDs {
		log.Info("Sandbox CRDs detected, sandbox-claim mode available")
	} else {
		log.Info("Sandbox CRDs not found, defaulting to bare-pod mode")
	}

	cfgCache := &configuration.Cache{ForceBareMode: !sandboxCRDs}
	cfgCache.SetOTELProvider(telemetryProvider)

	// Eagerly read ConfigMap if it already exists (operator restart).
	// Non-fatal if missing — the watcher will pick it up when it appears.
	if versionGate.Enabled(context.Background()) {
		if err := configwatch.TryLoad(
			context.Background(), mgr.GetAPIReader(), namespace,
			configuration.ConfigMapName, cfgCache.OnConfigMapChange,
		); err != nil {
			log.Info("ConfigMap not available at startup, telemetry disabled until it appears", "name", configuration.ConfigMapName, "reason", err.Error())
		}
	}

	// Watch for ConfigMap changes at runtime
	cmWatcher := configwatch.New(mgr.GetClient(), namespace,
		configwatch.Registration{Name: configuration.ConfigMapName, Handler: func(ctx context.Context, cm *corev1.ConfigMap) error {
			if versionGate.Enabled(ctx) {
				return cfgCache.OnConfigMapChange(ctx, cm)
			}
			return nil
		}},
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

	// --- Create sandbox manager and agent caller ---
	sandboxMgr := agenticrun.NewSandboxManager(mgr.GetClient(), cfgCache, namespace, auditLogger)
	agentCaller := &agenticrun.SandboxAgentCaller{
		Sandbox:   sandboxMgr,
		K8sClient: mgr.GetClient(),
		Namespace: namespace,
		Audit:     auditLogger,
	}

	// --- Register controllers ---
	runReconciler := &agenticrun.AgenticRunReconciler{
		Client:    mgr.GetClient(),
		Agent:     agentCaller,
		Config:    cfgCache,
		Namespace: namespace,
		Audit:     auditLogger,
		TempLog:   telemetryProvider,
		Version:   versionGate,
	}
	if err := runReconciler.SetupWithManager(mgr); err != nil {
		log.Error(err, "unable to set up AgenticRun controller")
		os.Exit(1)
	}

	if err := (&agenticolsconfig.Reconciler{
		Client:        mgr.GetClient(),
		EventRecorder: mgr.GetEventRecorderFor("agenticolsconfig-controller"),
		Version:       versionGate,
	}).SetupWithManager(mgr); err != nil {
		log.Error(err, "unable to set up AgenticOLSConfig controller")
		os.Exit(1)
	}

	// The ClusterVersion controller runs on the initial watch/list and every
	// transition. It must not create an operand before version detection succeeds.
	if err := ctrl.NewControllerManagedBy(mgr).For(ocpversion.Object()).Named("agentic-activation").Complete(reconcile.Func(func(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
		if req.NamespacedName != (types.NamespacedName{Name: "version"}) {
			return reconcile.Result{}, nil
		}
		enabled, err := versionGate.Check(ctx)
		if err != nil {
			return reconcile.Result{}, err // retry the completed-version event after a transient read failure
		}
		if !enabled {
			return reconcile.Result{}, nil
		}
		// The controller may have started on 4.x before Sandbox CRDs existed.
		// Recheck on activation and keep polling if they arrive after completion.
		sandboxClaimsAvailable := sandboxCRDInstalled(cfg)
		if sandboxClaimsAvailable {
			cfgCache.EnableSandboxClaims()
		}
		sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "lightspeed-agent", Namespace: namespace}}
		if err := mgr.GetClient().Create(ctx, sa); err != nil && !apierrors.IsAlreadyExists(err) {
			return reconcile.Result{}, err
		}
		// Load pre-existing handoff even when it did not change during activation.
		if err := configwatch.TryLoad(ctx, mgr.GetAPIReader(), namespace, configuration.ConfigMapName, cfgCache.OnConfigMapChange); err != nil {
			if !apierrors.IsNotFound(err) {
				return reconcile.Result{}, err // an existing handoff must not be lost on a transient read error
			}
			log.V(1).Info("handoff not available yet", "reason", err.Error())
			return reconcile.Result{RequeueAfter: time.Minute}, nil
		}
		if !sandboxClaimsAvailable && !sandboxCRDs {
			return reconcile.Result{RequeueAfter: time.Minute}, nil
		}
		return reconcile.Result{}, nil
	})); err != nil {
		log.Error(err, "unable to watch ClusterVersion")
		os.Exit(1)
	}

	mgr.GetWebhookServer().Register("/mutate-agenticrunapproval", &admission.Webhook{
		Handler: ocpversion.Admission{Gate: versionGate, Next: &agenticrun.AgenticRunApprovalMutator{}},
	})
	mgr.GetWebhookServer().Register("/validate-agent", &admission.Webhook{
		Handler: ocpversion.Admission{Gate: versionGate, Next: &agenticrun.AgentValidator{}},
	})

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

// sandboxCRDInstalled queries the apiextensions API directly to check
// whether the Sandbox CRD exists, bypassing any cached REST mapper.
func sandboxCRDInstalled(cfg *rest.Config) bool {
	apiext, err := apiextensionsclient.NewForConfig(cfg)
	if err != nil {
		return false
	}
	_, err = apiext.ApiextensionsV1().CustomResourceDefinitions().Get(
		context.Background(), "sandboxes.agents.x-k8s.io", metav1.GetOptions{})
	return err == nil
}
