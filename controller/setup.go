package controller

import (
	"context"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/openshift/lightspeed-agentic-operator/controller/agenticolsconfig"
	"github.com/openshift/lightspeed-agentic-operator/controller/proposal"
	agenticsandbox "github.com/openshift/lightspeed-agentic-operator/controller/sandbox"
)

type Options struct {
	Namespace           string
	AgenticSandboxImage string
	SandboxMode         string
	ImagePullPolicy     string
	Audit               proposal.AuditLogger
}

func Setup(mgr ctrl.Manager, opts Options) error {
	log := ctrl.Log.WithName("agentic-setup")

	var sandboxProvider proposal.SandboxProvider
	switch opts.SandboxMode {
	case "sandbox-claim":
		sandboxProvider = proposal.NewSandboxManager(mgr.GetClient(), opts.Namespace, "lightspeed-agent")
	default:
		builder := &proposal.PodSpecBuilder{
			Image:           opts.AgenticSandboxImage,
			ImagePullPolicy: opts.ImagePullPolicy,
		}
		sandboxProvider = proposal.NewBarePodManager(mgr.GetClient(), builder, opts.Namespace)
	}

	agentCaller := proposal.NewSandboxAgentCaller(
		sandboxProvider,
		mgr.GetClient(),
		proposal.NewAgentHTTPClient,
		opts.Namespace,
		opts.Audit,
	)

	if err := (&proposal.ProposalReconciler{
		Client:    mgr.GetClient(),
		Agent:     agentCaller,
		Namespace: opts.Namespace,
		Audit:     opts.Audit,
	}).SetupWithManager(mgr); err != nil {
		return err
	}
	log.Info("Proposal controller registered", "sandboxMode", opts.SandboxMode)

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

	mgr.GetWebhookServer().Register("/mutate-proposalapproval", &admission.Webhook{
		Handler: &proposal.ProposalApprovalMutator{},
	})
	log.Info("ProposalApproval mutating webhook registered")

	return nil
}
