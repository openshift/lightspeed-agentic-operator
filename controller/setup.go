package controller

import (
	"context"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	agenticconsole "github.com/openshift/lightspeed-agentic-operator/controller/console"
	"github.com/openshift/lightspeed-agentic-operator/controller/proposal"
)

type Options struct {
	Namespace           string
	AgenticConsoleImage string
	AgenticSandboxImage string
}

func Setup(mgr ctrl.Manager, opts Options) error {
	log := ctrl.Log.WithName("agentic-setup")

	sandboxMgr := proposal.NewSandboxManager(mgr.GetClient(), opts.Namespace)
	agentCaller := proposal.NewSandboxAgentCaller(
		sandboxMgr,
		mgr.GetClient(),
		proposal.NewAgentHTTPClient,
		opts.Namespace,
	)

	if err := (&proposal.ProposalReconciler{
		Client: mgr.GetClient(),
		Log:    ctrl.Log.WithName("controllers").WithName("Proposal"),
		Agent:  agentCaller,
	}).SetupWithManager(mgr); err != nil {
		return err
	}
	log.Info("Proposal controller registered")

	if err := mgr.Add(manager.RunnableFunc(func(ctx context.Context) error {
		return agenticconsole.EnsureAgenticConsole(ctx, mgr.GetClient(), agenticconsole.AgenticConsoleConfig{
			Image:     opts.AgenticConsoleImage,
			Namespace: opts.Namespace,
		})
	})); err != nil {
		return err
	}
	log.Info("Agentic console runnable registered")

	return nil
}
