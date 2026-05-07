package cli

import (
	"fmt"

	"github.com/spf13/cobra"
	"k8s.io/cli-runtime/pkg/genericclioptions"
)

var Version = "dev"

func NewVersionCmd(streams genericclioptions.IOStreams) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the plugin version",
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Fprintf(streams.Out, "oc-agentic %s\n", Version)
			return nil
		},
	}
}
