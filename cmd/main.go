package main

import (
	"flag"
	"os"

	consolev1 "github.com/openshift/api/console/v1"
	openshiftv1 "github.com/openshift/api/operator/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
	agenticcontroller "github.com/openshift/lightspeed-agentic-operator/controller"
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(agenticv1alpha1.AddToScheme(scheme))
	utilruntime.Must(consolev1.AddToScheme(scheme))
	utilruntime.Must(openshiftv1.AddToScheme(scheme))
}

func main() {
	var (
		metricsAddr         string
		healthAddr          string
		namespace           string
		agenticConsoleImage string
		agenticSandboxImage string
	)

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metric endpoint binds to.")
	flag.StringVar(&healthAddr, "health-probe-bind-address", ":8081", "The address the health probe endpoint binds to.")
	flag.StringVar(&namespace, "namespace", "", "The namespace where the operator runs (required).")
	flag.StringVar(&agenticConsoleImage, "agentic-console-image", "", "The image of the agentic console plugin container.")
	flag.StringVar(&agenticSandboxImage, "agentic-sandbox-image", "", "The image of the agentic sandbox container.")
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

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: healthAddr,
	})
	if err != nil {
		log.Error(err, "unable to create manager")
		os.Exit(1)
	}

	if err := agenticcontroller.Setup(mgr, agenticcontroller.Options{
		Namespace:           namespace,
		AgenticConsoleImage: agenticConsoleImage,
		AgenticSandboxImage: agenticSandboxImage,
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
		os.Exit(1)
	}
}
