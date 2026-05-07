package cli

import (
	"github.com/openshift/lightspeed-agentic-operator/cli/proposal"
	"github.com/spf13/cobra"
	"k8s.io/cli-runtime/pkg/genericclioptions"
)

func NewRootCmd(streams genericclioptions.IOStreams) *cobra.Command {
	cmd := &cobra.Command{
		Use:          "oc-agentic",
		Short:        "CLI for OpenShift Agentic proposals",
		Long:         "Manage Proposal resources — create, list, approve, watch, and inspect AI-driven agentic proposals.",
		SilenceUsage: true,
	}

	cmd.AddCommand(proposal.NewProposalCmd(streams))
	cmd.AddCommand(NewVersionCmd(streams))

	return cmd
}
