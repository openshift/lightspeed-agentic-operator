package run

import (
	"github.com/spf13/cobra"
	"k8s.io/cli-runtime/pkg/genericclioptions"
)

func NewAgenticRunCmd(streams genericclioptions.IOStreams) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "run",
		Aliases: []string{"runs"},
		Short:   "Manage AgenticRun resources",
		Long:    "Create, list, approve, deny, watch, and inspect agentic runs.",
	}

	cmd.AddCommand(NewListCmd(streams))
	cmd.AddCommand(NewGetCmd(streams))
	cmd.AddCommand(NewCreateCmd(streams))
	cmd.AddCommand(NewApproveCmd(streams))
	cmd.AddCommand(NewDenyCmd(streams))
	cmd.AddCommand(NewWatchCmd(streams))
	cmd.AddCommand(NewLogsCmd(streams))
	cmd.AddCommand(NewDeleteCmd(streams))
	cmd.AddCommand(NewCleanupCmd(streams))

	return cmd
}
