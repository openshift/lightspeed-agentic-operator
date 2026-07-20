package controller

import (
	"context"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/openshift/lightspeed-agentic-operator/controller/agenticolsconfig"
	"github.com/openshift/lightspeed-agentic-operator/controller/agenticrun"
	agenticsandbox "github.com/openshift/lightspeed-agentic-operator/controller/sandbox"
)

type Options struct {
	Namespace           string
	AgenticSandboxImage string
	SandboxMode         string
	ImagePullPolicy     string
	Audit               agenticrun.AuditLogger
	TempLog             agenticrun.TempLogCleaner
}

// RBAC for ConfigMap-driven telemetry (pkg/configwatch + credentials-secret).
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch

func Setup(mgr ctrl.Manager, opts Options) error {
	log := ctrl.Log.WithName("agentic-setup")

	var sandboxProvider agenticrun.SandboxProvider
	switch opts.SandboxMode {
	case "sandbox-claim":
		sandboxProvider = agenticrun.NewSandboxManager(mgr.GetClient(), opts.Namespace, "lightspeed-agent")
	default:
		builder := &agenticrun.PodSpecBuilder{
			Image:           opts.AgenticSandboxImage,
			ImagePullPolicy: opts.ImagePullPolicy,
		}
		sandboxProvider = agenticrun.NewBarePodManager(mgr.GetClient(), builder, opts.Namespace)
	}

	agentCaller := agenticrun.NewSandboxAgentCaller(
		sandboxProvider,
		mgr.GetClient(),
		agenticrun.NewAgentHTTPClient,
		opts.Namespace,
		opts.Audit,
	)

	if err := (&agenticrun.AgenticRunReconciler{
		Client:    mgr.GetClient(),
		Agent:     agentCaller,
		Namespace: opts.Namespace,
		Audit:     opts.Audit,
		TempLog:   opts.TempLog,
	}).SetupWithManager(mgr); err != nil {
		return err
	}
	log.Info("AgenticRun controller registered", "sandboxMode", opts.SandboxMode)

	if err := (&agenticolsconfig.Reconciler{
		Client:        mgr.GetClient(),
		EventRecorder: mgr.GetEventRecorderFor("agenticolsconfig-controller"),
	}).SetupWithManager(mgr); err != nil {
		return err
	}
	log.Info("AgenticOLSConfig controller registered")

	if err := mgr.Add(manager.RunnableFunc(func(ctx context.Context) error {
		return agenticsandbox.EnsureBootstrapResources(ctx, mgr.GetClient(), agenticsandbox.BootstrapConfig{
			Image:       opts.AgenticSandboxImage,
			Namespace:   opts.Namespace,
			SandboxMode: opts.SandboxMode,
		})
	})); err != nil {
		return err
	}
	log.Info("Sandbox bootstrap runnable registered", "sandboxMode", opts.SandboxMode)

	mgr.GetWebhookServer().Register("/mutate-agenticrunapproval", &admission.Webhook{
		Handler: &agenticrun.AgenticRunApprovalMutator{},
	})
	log.Info("AgenticRunApproval mutating webhook registered")

	return nil
}
