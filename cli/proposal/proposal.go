package proposal

import (
	"github.com/spf13/cobra"
	"k8s.io/cli-runtime/pkg/genericclioptions"
)

func NewProposalCmd(streams genericclioptions.IOStreams) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "proposal",
		Aliases: []string{"proposals", "prop"},
		Short:   "Manage Proposal resources",
		Long:    "Create, list, approve, deny, watch, and inspect agentic proposals.",
	}

	cmd.AddCommand(NewListCmd(streams))
	cmd.AddCommand(NewGetCmd(streams))
	cmd.AddCommand(NewCreateCmd(streams))
	cmd.AddCommand(NewApproveCmd(streams))
	cmd.AddCommand(NewDenyCmd(streams))
	cmd.AddCommand(NewWatchCmd(streams))
	cmd.AddCommand(NewLogsCmd(streams))
	cmd.AddCommand(NewDeleteCmd(streams))

	return cmd
}
