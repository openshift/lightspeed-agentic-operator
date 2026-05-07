package proposal

import (
	"context"
	"fmt"
	"strings"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
	"github.com/spf13/cobra"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type ListOptions struct {
	configFlags   *genericclioptions.ConfigFlags
	allNamespaces bool
	phase         string
	output        string

	client    client.Client
	namespace string

	genericclioptions.IOStreams
}

func NewListCmd(streams genericclioptions.IOStreams) *cobra.Command {
	o := &ListOptions{
		configFlags: genericclioptions.NewConfigFlags(true),
		IOStreams:    streams,
	}

	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List Proposal resources",
		Example: `  # List proposals in current namespace
  oc agentic proposal list

  # List all proposals across namespaces
  oc agentic proposal list -A

  # Filter by phase
  oc agentic proposal list --phase=Proposed

  # Wide output with additional columns
  oc agentic proposal list -o wide`,
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
	cmd.Flags().BoolVarP(&o.allNamespaces, "all-namespaces", "A", false, "List proposals across all namespaces")
	cmd.Flags().StringVar(&o.phase, "phase", "", "Filter by phase (Pending, Analyzing, Proposed, Approved, Denied, Executing, Verifying, Completed, Failed, Escalated)")
	cmd.Flags().StringVarP(&o.output, "output", "o", "", "Output format: json, yaml, or wide")

	return cmd
}

func (o *ListOptions) Complete(cmd *cobra.Command, args []string) error {
	var err error
	o.client, err = NewClient(o.configFlags)
	if err != nil {
		return err
	}
	if !o.allNamespaces {
		o.namespace, err = ResolveNamespace(o.configFlags)
		if err != nil {
			return err
		}
	}
	return nil
}

func (o *ListOptions) Validate() error {
	if o.phase != "" && !IsValidPhase(o.phase) {
		return fmt.Errorf("invalid phase %q, must be one of: %s", o.phase, strings.Join(validProposalPhases, ", "))
	}
	return ValidateOutputFormat(o.output, true)
}

func (o *ListOptions) Run(ctx context.Context) error {
	list := &agenticv1alpha1.ProposalList{}
	var opts []client.ListOption
	if !o.allNamespaces {
		opts = append(opts, client.InNamespace(o.namespace))
	}

	if err := o.client.List(ctx, list, opts...); err != nil {
		return fmt.Errorf("failed to list proposals: %w", err)
	}

	filtered := make([]agenticv1alpha1.Proposal, 0, len(list.Items))
	for _, p := range list.Items {
		if o.phase != "" && string(agenticv1alpha1.DerivePhase(p.Status.Conditions)) != o.phase {
			continue
		}
		filtered = append(filtered, p)
	}
	list.Items = filtered

	if o.output == OutputJSON || o.output == OutputYAML {
		return MarshalOutput(o.Out, list, o.output)
	}

	if len(list.Items) == 0 {
		if o.allNamespaces {
			fmt.Fprintln(o.Out, "No proposals found.")
		} else {
			fmt.Fprintf(o.Out, "No proposals found in namespace %q.\n", o.namespace)
		}
		return nil
	}

	SortProposalsByAge(list.Items)

	if o.output == OutputWide {
		o.printWideTable(list.Items)
	} else {
		o.printTable(list.Items)
	}
	return nil
}

func (o *ListOptions) printTable(items []agenticv1alpha1.Proposal) {
	var headers []string
	if o.allNamespaces {
		headers = []string{"NAMESPACE", "NAME", "PHASE", "AGE"}
	} else {
		headers = []string{"NAME", "PHASE", "AGE"}
	}
	rows := make([][]string, 0, len(items))
	for _, p := range items {
		row := []string{}
		if o.allNamespaces {
			row = append(row, p.Namespace)
		}
		row = append(row, p.Name, ColoredPhase(agenticv1alpha1.DerivePhase(p.Status.Conditions)), HumanDuration(p.CreationTimestamp.Time))
		rows = append(rows, row)
	}
	PrintTable(o.Out, headers, rows)
}

func (o *ListOptions) printWideTable(items []agenticv1alpha1.Proposal) {
	var headers []string
	if o.allNamespaces {
		headers = []string{"NAMESPACE", "NAME", "PHASE", "TARGET-NS", "AGE"}
	} else {
		headers = []string{"NAME", "PHASE", "TARGET-NS", "AGE"}
	}
	rows := make([][]string, 0, len(items))
	for _, p := range items {
		targetNS := "-"
		if len(p.Spec.TargetNamespaces) > 0 {
			targetNS = strings.Join(p.Spec.TargetNamespaces, ",")
		}
		row := []string{}
		if o.allNamespaces {
			row = append(row, p.Namespace)
		}
		row = append(row, p.Name, ColoredPhase(agenticv1alpha1.DerivePhase(p.Status.Conditions)),
			targetNS, HumanDuration(p.CreationTimestamp.Time))
		rows = append(rows, row)
	}
	PrintTable(o.Out, headers, rows)
}
