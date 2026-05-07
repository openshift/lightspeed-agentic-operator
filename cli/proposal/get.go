package proposal

import (
	"context"
	"fmt"
	"strings"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type GetOptions struct {
	configFlags *genericclioptions.ConfigFlags
	name        string
	output      string

	client    client.Client
	namespace string

	genericclioptions.IOStreams
}

func NewGetCmd(streams genericclioptions.IOStreams) *cobra.Command {
	o := &GetOptions{
		configFlags: genericclioptions.NewConfigFlags(true),
		IOStreams:    streams,
	}

	cmd := &cobra.Command{
		Use:   "get NAME",
		Short: "Display details of a Proposal",
		Example: `  # Show proposal details
  oc agentic proposal get fix-crash

  # Output as JSON
  oc agentic proposal get fix-crash -o json`,
		Args: cobra.ExactArgs(1),
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
	cmd.Flags().StringVarP(&o.output, "output", "o", "", "Output format: json or yaml")

	return cmd
}

func (o *GetOptions) Complete(cmd *cobra.Command, args []string) error {
	o.name = args[0]
	var err error
	o.client, err = NewClient(o.configFlags)
	if err != nil {
		return err
	}
	o.namespace, err = ResolveNamespace(o.configFlags)
	return err
}

func (o *GetOptions) Validate() error {
	return ValidateOutputFormat(o.output, false)
}

func (o *GetOptions) Run(ctx context.Context) error {
	p := &agenticv1alpha1.Proposal{}
	if err := o.client.Get(ctx, types.NamespacedName{Name: o.name, Namespace: o.namespace}, p); err != nil {
		return fmt.Errorf("failed to get proposal %q: %w", o.name, err)
	}

	if o.output == OutputJSON || o.output == OutputYAML {
		return MarshalOutput(o.Out, p, o.output)
	}

	o.printDetail(p)
	return nil
}

func (o *GetOptions) printDetail(p *agenticv1alpha1.Proposal) {
	w := o.Out

	fmt.Fprintf(w, "Name:              %s\n", p.Name)
	fmt.Fprintf(w, "Namespace:         %s\n", p.Namespace)
	fmt.Fprintf(w, "Phase:             %s\n", ColoredPhase(agenticv1alpha1.DerivePhase(p.Status.Conditions)))
	fmt.Fprintf(w, "Age:               %s\n", HumanDuration(p.CreationTimestamp.Time))
	fmt.Fprintf(w, "Request:           %s\n", p.Spec.Request)

	if len(p.Spec.TargetNamespaces) > 0 {
		fmt.Fprintf(w, "\nTarget Namespaces: %s\n", strings.Join(p.Spec.TargetNamespaces, ", "))
	}

	// Analysis step
	fmt.Fprintln(w)
	fmt.Fprintf(w, "Analysis:          %s\n",
		stepStatusFromConditions(p.Status.Steps.Analysis.Conditions, agenticv1alpha1.ProposalConditionAnalyzed))
	for _, ref := range p.Status.Steps.Analysis.Results {
		fmt.Fprintf(w, "  Result:          %s (outcome=%s)\n", ref.Name, ref.Outcome)
	}

	// Execution step
	fmt.Fprintln(w)
	fmt.Fprintf(w, "Execution:         %s\n",
		stepStatusFromConditions(p.Status.Steps.Execution.Conditions, agenticv1alpha1.ProposalConditionExecuted))
	for _, ref := range p.Status.Steps.Execution.Results {
		fmt.Fprintf(w, "  Result:          %s (outcome=%s)\n", ref.Name, ref.Outcome)
	}

	// Verification step
	fmt.Fprintln(w)
	fmt.Fprintf(w, "Verification:      %s\n",
		stepStatusFromConditions(p.Status.Steps.Verification.Conditions, agenticv1alpha1.ProposalConditionVerified))
	for _, ref := range p.Status.Steps.Verification.Results {
		fmt.Fprintf(w, "  Result:          %s (outcome=%s)\n", ref.Name, ref.Outcome)
	}

	// Escalation step
	if len(p.Status.Steps.Escalation.Results) > 0 || len(p.Status.Steps.Escalation.Conditions) > 0 {
		fmt.Fprintln(w)
		fmt.Fprintf(w, "Escalation:        %s\n",
			stepStatusFromConditions(p.Status.Steps.Escalation.Conditions, agenticv1alpha1.ProposalConditionEscalated))
		for _, ref := range p.Status.Steps.Escalation.Results {
			fmt.Fprintf(w, "  Result:          %s (outcome=%s)\n", ref.Name, ref.Outcome)
		}
	}

	// Conditions
	if len(p.Status.Conditions) > 0 {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "Conditions:")
		headers := []string{"  TYPE", "STATUS", "REASON", "AGE"}
		rows := make([][]string, 0, len(p.Status.Conditions))
		for _, c := range p.Status.Conditions {
			rows = append(rows, []string{
				"  " + c.Type,
				string(c.Status),
				c.Reason,
				HumanDuration(c.LastTransitionTime.Time),
			})
		}
		PrintTable(w, headers, rows)
	}
}
