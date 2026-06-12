package controller

import (
	"context"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	agenticconsole "github.com/openshift/lightspeed-agentic-operator/controller/console"
	"github.com/openshift/lightspeed-agentic-operator/controller/proposal"
	agenticsandbox "github.com/openshift/lightspeed-agentic-operator/controller/sandbox"
)

type Options struct {
	Namespace           string
	AgenticConsoleImage string
	AgenticSandboxImage string
	SandboxMode         string
	ImagePullPolicy     string
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
	)

	if err := (&proposal.ProposalReconciler{
		Client:    mgr.GetClient(),
		Agent:     agentCaller,
		Namespace: opts.Namespace,
	}).SetupWithManager(mgr); err != nil {
		return err
	}
	log.Info("Proposal controller registered", "sandboxMode", opts.SandboxMode)

	if err := mgr.Add(manager.RunnableFunc(func(ctx context.Context) error {
		return agenticconsole.EnsureAgenticConsole(ctx, mgr.GetClient(), agenticconsole.AgenticConsoleConfig{
			Image:     opts.AgenticConsoleImage,
			Namespace: opts.Namespace,
		})
	})); err != nil {
		return err
	}
	log.Info("Agentic console runnable registered")

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

	return nil
}
