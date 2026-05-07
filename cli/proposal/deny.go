package proposal

import (
	"context"
	"fmt"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type DenyOptions struct {
	configFlags *genericclioptions.ConfigFlags
	name        string
	stage       string

	client    client.Client
	namespace string

	genericclioptions.IOStreams
}

func NewDenyCmd(streams genericclioptions.IOStreams) *cobra.Command {
	o := &DenyOptions{
		configFlags: genericclioptions.NewConfigFlags(true),
		IOStreams:    streams,
	}

	cmd := &cobra.Command{
		Use:   "deny NAME",
		Short: "Deny a proposal step",
		Example: `  # Deny the execution step
  oc agentic proposal deny fix-crash --stage=execution

  # Deny the next pending step
  oc agentic proposal deny fix-crash`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := o.Complete(cmd, args); err != nil {
				return err
			}
			return o.Run(cmd.Context())
		},
	}

	o.configFlags.AddFlags(cmd.Flags())
	cmd.Flags().StringVar(&o.stage, "stage", "", "Step to deny: analysis, execution, or verification (defaults to next pending)")

	return cmd
}

func (o *DenyOptions) Complete(cmd *cobra.Command, args []string) error {
	o.name = args[0]
	var err error
	o.client, err = NewClient(o.configFlags)
	if err != nil {
		return err
	}
	o.namespace, err = ResolveNamespace(o.configFlags)
	return err
}

func (o *DenyOptions) Run(ctx context.Context) error {
	p := &agenticv1alpha1.Proposal{}
	if err := o.client.Get(ctx, types.NamespacedName{Name: o.name, Namespace: o.namespace}, p); err != nil {
		return fmt.Errorf("failed to get proposal %q: %w", o.name, err)
	}

	approval := &agenticv1alpha1.ProposalApproval{}
	if err := o.client.Get(ctx, types.NamespacedName{Name: o.name, Namespace: o.namespace}, approval); err != nil {
		return fmt.Errorf("failed to get ProposalApproval %q: %w", o.name, err)
	}

	stageName := o.stage
	if stageName == "" {
		stageName = o.nextPendingStage(p, approval)
		if stageName == "" {
			return fmt.Errorf("no pending stages to deny")
		}
	}

	stageType := normalizeStageType(stageName)

	for _, s := range approval.Spec.Stages {
		if s.Type == stageType {
			if s.Decision == agenticv1alpha1.ApprovalDecisionDenied {
				return fmt.Errorf("stage %s is already denied", stageName)
			}
			return fmt.Errorf("stage %s is already approved, cannot deny", stageName)
		}
	}

	entry := agenticv1alpha1.ApprovalStage{Type: stageType, Decision: agenticv1alpha1.ApprovalDecisionDenied}
	switch stageType {
	case agenticv1alpha1.ApprovalStageAnalysis:
		entry.Analysis = agenticv1alpha1.AnalysisApproval{}
	case agenticv1alpha1.ApprovalStageExecution:
		entry.Execution = agenticv1alpha1.ExecutionApproval{}
	case agenticv1alpha1.ApprovalStageVerification:
		entry.Verification = agenticv1alpha1.VerificationApproval{}
	}

	patch := client.MergeFrom(approval.DeepCopy())
	approval.Spec.Stages = append(approval.Spec.Stages, entry)
	if err := o.client.Patch(ctx, approval, patch); err != nil {
		return fmt.Errorf("failed to deny stage %s: %w", stageName, err)
	}

	fmt.Fprintf(o.Out, "proposal/%s stage %s denied\n", o.name, stageName)
	return nil
}

func (o *DenyOptions) nextPendingStage(p *agenticv1alpha1.Proposal, approval *agenticv1alpha1.ProposalApproval) string {
	approved := map[agenticv1alpha1.ApprovalStageType]bool{}
	for _, s := range approval.Spec.Stages {
		approved[s.Type] = true
	}

	stages := []struct {
		configured bool
		stageType  agenticv1alpha1.ApprovalStageType
		name       string
	}{
		{!p.Spec.Analysis.IsZero(), agenticv1alpha1.ApprovalStageAnalysis, "analysis"},
		{!p.Spec.Execution.IsZero(), agenticv1alpha1.ApprovalStageExecution, "execution"},
		{!p.Spec.Verification.IsZero(), agenticv1alpha1.ApprovalStageVerification, "verification"},
	}

	for _, s := range stages {
		if s.configured && !approved[s.stageType] {
			return s.name
		}
	}
	return ""
}

