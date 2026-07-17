package run

import (
	"context"
	"fmt"
	"strings"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
	"github.com/spf13/cobra"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type ApproveOptions struct {
	configFlags *genericclioptions.ConfigFlags
	name        string
	stage       string
	option      int32
	agent       string
	all         bool
	wait        bool

	client    client.Client
	namespace string

	genericclioptions.IOStreams
}

func NewApproveCmd(streams genericclioptions.IOStreams) *cobra.Command {
	o := &ApproveOptions{
		configFlags: genericclioptions.NewConfigFlags(true),
		IOStreams:   streams,
	}

	cmd := &cobra.Command{
		Use:   "approve NAME",
		Short: "Approve a run step",
		Example: `  # Approve the analysis step
  oc agentic run approve fix-crash --stage=analysis

  # Approve execution with option 1 and agent override
  oc agentic run approve fix-crash --stage=execution --option=1 --agent=fast

  # Approve verification
  oc agentic run approve fix-crash --stage=verification

  # Approve all pending steps
  oc agentic run approve fix-crash --all`,
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
	cmd.Flags().StringVar(&o.stage, "stage", "", "Step to approve: analysis, execution, or verification (required unless --all)")
	cmd.Flags().Int32Var(&o.option, "option", 0, "0-based index of the remediation option (execution only)")
	cmd.Flags().StringVar(&o.agent, "agent", "", "Override agent for this step")
	cmd.Flags().BoolVar(&o.all, "all", false, "Approve all remaining unapproved steps")
	cmd.Flags().BoolVar(&o.wait, "wait", false, "Wait for run to reach a terminal phase after approving")

	return cmd
}

func (o *ApproveOptions) Complete(cmd *cobra.Command, args []string) error {
	o.name = args[0]
	var err error
	o.client, err = NewClient(o.configFlags)
	if err != nil {
		return err
	}
	o.namespace, err = ResolveNamespace(o.configFlags)
	return err
}

func (o *ApproveOptions) Validate() error {
	if !o.all && o.stage == "" {
		return fmt.Errorf("--stage is required (analysis, execution, or verification) unless --all is set")
	}
	if o.stage != "" {
		switch strings.ToLower(o.stage) {
		case "analysis", "execution", "verification":
		default:
			return fmt.Errorf("--stage must be analysis, execution, or verification")
		}
	}
	if o.option < 0 {
		return fmt.Errorf("--option must be >= 0")
	}
	return nil
}

func (o *ApproveOptions) Run(ctx context.Context) error {
	p := &agenticv1alpha1.AgenticRun{}
	if err := o.client.Get(ctx, types.NamespacedName{Name: o.name, Namespace: o.namespace}, p); err != nil {
		return fmt.Errorf("failed to get run %q: %w", o.name, err)
	}

	approval, err := o.getOrCreateApproval(ctx, p)
	if err != nil {
		return err
	}

	var stages []string
	if o.all {
		stages = o.pendingStages(p, approval)
		if len(stages) == 0 {
			return fmt.Errorf("no pending stages to approve")
		}
	} else {
		stages = []string{o.stage}
	}

	// Validate all stages before patching.
	var entries []agenticv1alpha1.ApprovalStage
	for _, stageName := range stages {
		stageType := normalizeStageType(stageName)

		for _, s := range approval.Spec.Stages {
			if s.Type == stageType {
				if s.Decision == agenticv1alpha1.ApprovalDecisionDenied {
					return fmt.Errorf("stage %s is already denied", stageName)
				}
				return fmt.Errorf("stage %s is already approved", stageName)
			}
		}

		var option *int32
		if stageType == agenticv1alpha1.ApprovalStageExecution {
			option = &o.option
		}
		entries = append(entries, agenticv1alpha1.NewApprovalStage(stageType, "", o.agent, option, 0))
	}

	patch := client.MergeFrom(approval.DeepCopy())
	approval.Spec.Stages = append(approval.Spec.Stages, entries...)
	if err := o.client.Patch(ctx, approval, patch); err != nil {
		return fmt.Errorf("failed to approve stages: %w", err)
	}

	for _, stageName := range stages {
		fmt.Fprintf(o.Out, "run/%s stage %s approved\n", o.name, stageName)
	}

	if o.wait {
		return doWatch(ctx, o.configFlags, o.namespace, o.name, o.Out)
	}
	return nil
}

func (o *ApproveOptions) getOrCreateApproval(ctx context.Context, p *agenticv1alpha1.AgenticRun) (*agenticv1alpha1.AgenticRunApproval, error) {
	approval := &agenticv1alpha1.AgenticRunApproval{}
	err := o.client.Get(ctx, types.NamespacedName{Name: o.name, Namespace: o.namespace}, approval)
	if err == nil {
		return approval, nil
	}
	if !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("failed to get AgenticRunApproval: %w", err)
	}

	approval = &agenticv1alpha1.AgenticRunApproval{
		ObjectMeta: metav1.ObjectMeta{
			Name:      o.name,
			Namespace: o.namespace,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "agentic.openshift.io/v1alpha1",
				Kind:       "AgenticRun",
				Name:       p.Name,
				UID:        p.UID,
			}},
		},
	}
	if err := o.client.Create(ctx, approval); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return o.getOrCreateApproval(ctx, p)
		}
		return nil, fmt.Errorf("failed to create AgenticRunApproval: %w", err)
	}
	return approval, nil
}

func (o *ApproveOptions) pendingStages(p *agenticv1alpha1.AgenticRun, approval *agenticv1alpha1.AgenticRunApproval) []string {
	approved := map[agenticv1alpha1.ApprovalStageType]bool{}
	for _, s := range approval.Spec.Stages {
		approved[s.Type] = true
	}

	var pending []string
	if !p.Spec.Analysis.IsZero() && !approved[agenticv1alpha1.ApprovalStageAnalysis] {
		pending = append(pending, "analysis")
	}
	if !p.Spec.Execution.IsZero() && !approved[agenticv1alpha1.ApprovalStageExecution] {
		pending = append(pending, "execution")
	}
	if !p.Spec.Verification.IsZero() && !approved[agenticv1alpha1.ApprovalStageVerification] {
		pending = append(pending, "verification")
	}
	return pending
}

func normalizeStageType(s string) agenticv1alpha1.ApprovalStageType {
	switch strings.ToLower(s) {
	case "analysis":
		return agenticv1alpha1.ApprovalStageAnalysis
	case "execution":
		return agenticv1alpha1.ApprovalStageExecution
	case "verification":
		return agenticv1alpha1.ApprovalStageVerification
	default:
		return agenticv1alpha1.ApprovalStageType(s)
	}
}
