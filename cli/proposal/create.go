package proposal

import (
	"context"
	"fmt"
	"strings"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type CreateOptions struct {
	configFlags      *genericclioptions.ConfigFlags
	agent            string
	request          string
	targetNamespaces []string
	output string

	client    client.Client
	namespace string

	genericclioptions.IOStreams
}

func NewCreateCmd(streams genericclioptions.IOStreams) *cobra.Command {
	o := &CreateOptions{
		configFlags: genericclioptions.NewConfigFlags(true),
		IOStreams:    streams,
	}

	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create a new Proposal",
		Example: `  # Create a proposal with the default agent
  oc agentic proposal create --request="Fix crashloop in production"

  # Create a proposal with a specific agent
  oc agentic proposal create --agent=smart --request="Upgrade to 4.22"`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := o.Complete(cmd, args); err != nil {
				return err
			}
			if err := o.Validate(); err != nil {
				return err
			}
			return o.Run(cmd.Context())
		},
	}

	o.configFlags.AddFlags(cmd.Flags())
	cmd.Flags().StringVar(&o.agent, "agent", "default", "Agent CR name for the analysis step")
	cmd.Flags().StringVar(&o.request, "request", "", "Description of what to do (required)")
	cmd.Flags().StringSliceVar(&o.targetNamespaces, "target-namespaces", nil, "Target namespace(s), comma-separated")
	cmd.Flags().StringVarP(&o.output, "output", "o", "", "Output format: json or yaml")

	_ = cmd.MarkFlagRequired("request")

	return cmd
}

func (o *CreateOptions) Complete(cmd *cobra.Command, args []string) error {
	var err error
	o.client, err = NewClient(o.configFlags)
	if err != nil {
		return err
	}
	o.namespace, err = ResolveNamespace(o.configFlags)
	return err
}

func (o *CreateOptions) Validate() error {
	if strings.TrimSpace(o.request) == "" {
		return fmt.Errorf("--request must not be empty")
	}
	return ValidateOutputFormat(o.output, false)
}

func (o *CreateOptions) Run(ctx context.Context) error {
	proposal := &agenticv1alpha1.Proposal{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "ag-",
			Namespace:    o.namespace,
		},
		Spec: agenticv1alpha1.ProposalSpec{
			Request:          o.request,
			TargetNamespaces: o.targetNamespaces,
			Analysis: agenticv1alpha1.ProposalStep{
				Agent: o.agent,
			},
		},
	}

	if err := o.client.Create(ctx, proposal); err != nil {
		return fmt.Errorf("failed to create proposal: %w", err)
	}

	if o.output == OutputJSON || o.output == OutputYAML {
		return MarshalOutput(o.Out, proposal, o.output)
	}

	fmt.Fprintf(o.Out, "proposal/%s created\n", proposal.Name)
	return nil
}
