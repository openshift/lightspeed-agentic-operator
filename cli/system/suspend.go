package system

import (
	"bufio"
	"context"
	"fmt"
	"strings"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
	"github.com/spf13/cobra"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type SuspendOptions struct {
	configFlags *genericclioptions.ConfigFlags
	yes         bool
	client      client.Client
	genericclioptions.IOStreams
}

func NewSuspendCmd(streams genericclioptions.IOStreams) *cobra.Command {
	o := &SuspendOptions{
		configFlags: genericclioptions.NewConfigFlags(true),
		IOStreams:   streams,
	}

	cmd := &cobra.Command{
		Use:   "suspend",
		Short: "Suspend all agentic operations",
		Example: `  # Suspend (with confirmation prompt)
  oc agentic suspend

  # Suspend without prompting
  oc agentic suspend --yes`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := o.Complete(); err != nil {
				return err
			}
			return o.Run(cmd.Context())
		},
	}

	o.configFlags.AddFlags(cmd.Flags())
	cmd.Flags().BoolVarP(&o.yes, "yes", "y", false, "Skip confirmation prompt")

	return cmd
}

func (o *SuspendOptions) Complete() error {
	var err error
	o.client, err = newClient(o.configFlags)
	return err
}

func (o *SuspendOptions) Run(ctx context.Context) error {
	if !o.yes {
		fmt.Fprint(o.Out, "All agentic operations will be halted and in-flight proposals will be terminated. Continue? [y/N] ")
		reader := bufio.NewReader(o.In)
		answer, err := reader.ReadString('\n')
		if err != nil {
			return fmt.Errorf("failed to read input: %w", err)
		}
		if !strings.EqualFold(strings.TrimSpace(answer), "y") {
			fmt.Fprintln(o.Out, "Aborted.")
			return nil
		}
	}

	cfg, err := getConfig(ctx, o.client)
	if isNoMatchError(err) {
		return fmt.Errorf("agentic system is not installed (AgenticOLSConfig CRD not found)")
	}
	if apierrors.IsNotFound(err) {
		cfg = &agenticv1alpha1.AgenticOLSConfig{
			ObjectMeta: metav1.ObjectMeta{Name: configName},
			Spec:       agenticv1alpha1.AgenticOLSConfigSpec{Suspended: true},
		}
		if err := o.client.Create(ctx, cfg); err != nil {
			if !apierrors.IsAlreadyExists(err) {
				return fmt.Errorf("failed to create AgenticOLSConfig: %w", err)
			}
			cfg, err = getConfig(ctx, o.client)
			if err != nil {
				return fmt.Errorf("failed to get AgenticOLSConfig after conflict: %w", err)
			}
		} else {
			fmt.Fprintln(o.Out, "Agentic system suspended.")
			return nil
		}
	}
	if err != nil {
		return fmt.Errorf("failed to get AgenticOLSConfig: %w", err)
	}

	if cfg.Spec.Suspended {
		fmt.Fprintln(o.Out, "Agentic system is already suspended.")
		return nil
	}

	patch := client.MergeFrom(cfg.DeepCopy())
	cfg.Spec.Suspended = true
	if err := o.client.Patch(ctx, cfg, patch); err != nil {
		return fmt.Errorf("failed to patch AgenticOLSConfig: %w", err)
	}

	fmt.Fprintln(o.Out, "Agentic system suspended.")
	return nil
}
