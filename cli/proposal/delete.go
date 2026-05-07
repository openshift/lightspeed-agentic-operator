package proposal

import (
	"context"
	"fmt"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type DeleteOptions struct {
	configFlags *genericclioptions.ConfigFlags
	name        string

	client    client.Client
	namespace string

	genericclioptions.IOStreams
}

func NewDeleteCmd(streams genericclioptions.IOStreams) *cobra.Command {
	o := &DeleteOptions{
		configFlags: genericclioptions.NewConfigFlags(true),
		IOStreams:    streams,
	}

	cmd := &cobra.Command{
		Use:   "delete NAME",
		Short: "Delete a Proposal",
		Example: `  # Delete a proposal
  oc agentic proposal delete fix-crash`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := o.Complete(cmd, args); err != nil {
				return err
			}
			return o.Run(cmd.Context())
		},
	}

	o.configFlags.AddFlags(cmd.Flags())

	return cmd
}

func (o *DeleteOptions) Complete(cmd *cobra.Command, args []string) error {
	o.name = args[0]
	var err error
	o.client, err = NewClient(o.configFlags)
	if err != nil {
		return err
	}
	o.namespace, err = ResolveNamespace(o.configFlags)
	return err
}

func (o *DeleteOptions) Run(ctx context.Context) error {
	p := &agenticv1alpha1.Proposal{
		ObjectMeta: metav1.ObjectMeta{
			Name:      o.name,
			Namespace: o.namespace,
		},
	}
	if err := o.client.Delete(ctx, p); err != nil {
		return fmt.Errorf("failed to delete proposal %q: %w", o.name, err)
	}

	fmt.Fprintf(o.Out, "proposal/%s deleted\n", o.name)
	return nil
}
