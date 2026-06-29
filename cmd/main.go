package main

import (
	"context"
	"flag"
	"os"
	"time"

	// Import auth plugins (Azure, GCP, OIDC, etc.) for local and hosted kubeconfigs.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.24.0"
	uberzap "go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
	agenticcontroller "github.com/openshift/lightspeed-agentic-operator/controller"
	"github.com/openshift/lightspeed-agentic-operator/controller/proposal"
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(agenticv1alpha1.AddToScheme(scheme))
}

// initTracerProvider initializes the OTEL tracer provider.
// When endpoint is empty, returns a no-op tracer provider (no export).
// When endpoint is set, creates OTLP gRPC exporter to that endpoint.
func initTracerProvider(endpoint string, otlpInsecure bool) (*sdktrace.TracerProvider, error) {
	res := resource.NewWithAttributes(
		semconv.SchemaURL,
		semconv.ServiceName("lightspeed-agentic-operator"),
		semconv.ServiceVersion("dev"),
	)

	idGen := &proposal.ProposalIDGenerator{}

	// No endpoint - create tracer provider without exporter (no traces exported)
	if endpoint == "" {
		tp := sdktrace.NewTracerProvider(
			sdktrace.WithResource(res),
			sdktrace.WithIDGenerator(idGen),
		)
		return tp, nil
	}

	// Create OTLP gRPC exporter
	opts := []otlptracegrpc.Option{
		otlptracegrpc.WithEndpoint(endpoint),
	}
	if otlpInsecure {
		opts = append(opts, otlptracegrpc.WithInsecure())
	}

	exporter, err := otlptracegrpc.New(context.Background(), opts...)
	if err != nil {
		return nil, err
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithResource(res),
		sdktrace.WithIDGenerator(idGen),
		sdktrace.WithBatcher(exporter),
	)

	return tp, nil
}

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

	// Read AgenticOLSConfig for audit configuration
	log.Info("Reading AgenticOLSConfig for audit settings")
	agenticCfg := &agenticv1alpha1.AgenticOLSConfig{}
	if err := mgr.GetAPIReader().Get(context.Background(), client.ObjectKey{Name: "cluster"}, agenticCfg); err != nil {
		if !errors.IsNotFound(err) {
			log.Error(err, "unable to fetch AgenticOLSConfig, falling back to audit defaults")
		} else {
			log.Info("AgenticOLSConfig not found, using audit defaults (logging enabled, no OTEL)")
		}
		agenticCfg = nil
	}

	// Initialize OTEL tracer provider
	var auditConfig agenticv1alpha1.AuditConfig
	if agenticCfg != nil {
		auditConfig = agenticCfg.Spec.Audit
	}

	otlpEndpoint := auditConfig.OTELEndpoint()
	otlpInsecure := auditConfig.OTELInsecure()

	tp, err := initTracerProvider(otlpEndpoint, otlpInsecure)
	if err != nil {
		log.Error(err, "unable to initialize OTEL tracer provider")
		os.Exit(1)
	}
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})

	// Shutdown tracer on exit
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := tp.Shutdown(shutdownCtx); err != nil {
			log.Error(err, "failed to shutdown OTEL tracer provider")
		}
	}()

	if otlpEndpoint == "" {
		log.Info("OTEL tracing disabled (no endpoint configured)")
	} else if otlpInsecure {
		log.Info("OTEL tracing enabled (insecure)", "endpoint", otlpEndpoint)
	} else {
		log.Info("OTEL tracing enabled (TLS)", "endpoint", otlpEndpoint)
	}

	// Create AuditLogger
	var auditLogger proposal.AuditLogger
	loggingEnabled := auditConfig.LoggingEnabled()

	if loggingEnabled {
		// Use zap logger for structured JSON
		zapLogger, err := uberzap.NewProduction()
		if err != nil {
			log.Error(err, "unable to create zap logger for audit")
			os.Exit(1)
		}
		defer zapLogger.Sync()
		auditLogger = proposal.NewProductionAuditLogger(zapLogger)
		log.Info("Audit logging enabled")
	} else {
		auditLogger = proposal.NewNoOpAuditLogger()
		log.Info("Audit logging disabled")
	}

	if err := agenticcontroller.Setup(mgr, agenticcontroller.Options{
		Namespace:           namespace,
		AgenticSandboxImage: agenticSandboxImage,
		SandboxMode:         sandboxMode,
		ImagePullPolicy:     imagePullPolicy,
		Audit:               auditLogger,
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
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = tp.Shutdown(shutdownCtx)
		os.Exit(1)
	}
}
