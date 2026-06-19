package system

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
)

type StatusOptions struct {
	configFlags *genericclioptions.ConfigFlags
	client      client.Client
	genericclioptions.IOStreams
	now func() time.Time
}

func NewStatusCmd(streams genericclioptions.IOStreams) *cobra.Command {
	o := &StatusOptions{
		configFlags: genericclioptions.NewConfigFlags(true),
		IOStreams:   streams,
		now:         time.Now,
	}

	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show system status",
		Example: `  # Check if agentic system is suspended
  oc agentic status`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := o.Complete(); err != nil {
				return err
			}
			return o.Run(cmd.Context())
		},
	}

	o.configFlags.AddFlags(cmd.Flags())
	return cmd
}

func (o *StatusOptions) Complete() error {
	var err error
	o.client, err = newClient(o.configFlags)
	if err != nil {
		return err
	}
	if o.now == nil {
		o.now = time.Now
	}
	return nil
}

func (o *StatusOptions) Run(ctx context.Context) error {
	cfg, err := getConfig(ctx, o.client)
	if apierrors.IsNotFound(err) {
		fmt.Fprintln(o.Out, "Agentic System: Active")
		return nil
	}
	if isNoMatchError(err) {
		return fmt.Errorf("agentic system is not installed (AgenticOLSConfig CRD not found)")
	}
	if err != nil {
		return fmt.Errorf("failed to get AgenticOLSConfig: %w", err)
	}

	suspendedCond := meta.FindStatusCondition(cfg.Status.Conditions, agenticv1alpha1.AgenticOLSConfigConditionSuspended)
	if cfg.Spec.Suspended && suspendedCond != nil {
		relative, absolute := formatConditionTimes(suspendedCond.LastTransitionTime, o.now())
		fmt.Fprintf(o.Out, "Agentic System: SUSPENDED (since %s ago, %s, %s)\n",
			relative, absolute, suspendedCond.Message)
		return nil
	}
	if !cfg.Spec.Suspended && suspendedCond != nil && suspendedCond.Reason == "AdminDeactivated" {
		relative, absolute := formatConditionTimes(suspendedCond.LastTransitionTime, o.now())
		fmt.Fprintf(o.Out, "Agentic System: Active (resumed %s ago, %s)\n", relative, absolute)
		return nil
	}

	if cfg.Spec.Suspended {
		fmt.Fprintln(o.Out, "Agentic System: SUSPENDED")
	} else {
		fmt.Fprintln(o.Out, "Agentic System: Active")
	}
	return nil
}

func formatConditionTimes(lastTransition metav1.Time, now time.Time) (relative, absolute string) {
	if lastTransition.IsZero() {
		return "unknown", "unknown"
	}
	elapsed := now.Sub(lastTransition.Time)
	if elapsed < 0 {
		elapsed = 0
	}
	return formatDuration(elapsed), lastTransition.UTC().Format(time.RFC3339)
}

func formatDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}
