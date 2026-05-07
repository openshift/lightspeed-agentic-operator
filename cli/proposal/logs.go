package proposal

import (
	"bufio"
	"context"
	"fmt"
	"strings"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type LogsOptions struct {
	configFlags *genericclioptions.ConfigFlags
	name        string
	step        string
	follow      bool

	k8sClient client.Client
	clientset *kubernetes.Clientset
	namespace string

	genericclioptions.IOStreams
}

func NewLogsCmd(streams genericclioptions.IOStreams) *cobra.Command {
	o := &LogsOptions{
		configFlags: genericclioptions.NewConfigFlags(true),
		IOStreams:    streams,
	}

	cmd := &cobra.Command{
		Use:   "logs NAME",
		Short: "Stream sandbox pod logs for a proposal",
		Example: `  # Stream logs from the latest sandbox step
  oc agentic proposal logs fix-crash

  # Stream execution step logs
  oc agentic proposal logs fix-crash --step=Execution

  # Follow logs
  oc agentic proposal logs fix-crash -f`,
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
	cmd.Flags().StringVar(&o.step, "step", "", "Sandbox step: Analysis, Execution, or Verification")
	cmd.Flags().BoolVarP(&o.follow, "follow", "f", false, "Follow log output")

	return cmd
}

func (o *LogsOptions) Complete(cmd *cobra.Command, args []string) error {
	o.name = args[0]

	cfg, err := o.configFlags.ToRESTConfig()
	if err != nil {
		return fmt.Errorf("failed to get REST config: %w", err)
	}

	o.k8sClient, err = NewClientFromConfig(cfg)
	if err != nil {
		return err
	}

	o.clientset, err = kubernetes.NewForConfig(cfg)
	if err != nil {
		return fmt.Errorf("failed to create clientset: %w", err)
	}

	o.namespace, err = ResolveNamespace(o.configFlags)
	return err
}

func (o *LogsOptions) Validate() error {
	if o.step != "" && !IsValidStep(o.step) {
		return fmt.Errorf("invalid step %q, must be one of: %s", o.step, strings.Join(validSandboxSteps, ", "))
	}
	return nil
}

func (o *LogsOptions) Run(ctx context.Context) error {
	p := &agenticv1alpha1.Proposal{}
	if err := o.k8sClient.Get(ctx, types.NamespacedName{Name: o.name, Namespace: o.namespace}, p); err != nil {
		return fmt.Errorf("failed to get proposal %q: %w", o.name, err)
	}

	sandbox := o.resolveSandbox(p)
	if sandbox == nil || sandbox.ClaimName == "" {
		return fmt.Errorf("no sandbox found for proposal %q (step: %s)", o.name, o.step)
	}

	podName := sandbox.ClaimName
	podNamespace := sandbox.Namespace
	if podNamespace == "" {
		podNamespace = o.namespace
	}

	req := o.clientset.CoreV1().Pods(podNamespace).GetLogs(podName, &corev1.PodLogOptions{
		Follow: o.follow,
	})
	stream, err := req.Stream(ctx)
	if err != nil {
		return fmt.Errorf("failed to stream logs from pod %s/%s: %w", podNamespace, podName, err)
	}
	defer stream.Close()

	scanner := bufio.NewScanner(stream)
	for scanner.Scan() {
		fmt.Fprintln(o.Out, scanner.Text())
	}
	return scanner.Err()
}

func (o *LogsOptions) resolveSandbox(p *agenticv1alpha1.Proposal) *agenticv1alpha1.SandboxInfo {
	if o.step != "" {
		step := NormalizeStep(o.step)
		switch step {
		case agenticv1alpha1.SandboxStepAnalysis:
			return &p.Status.Steps.Analysis.Sandbox
		case agenticv1alpha1.SandboxStepExecution:
			return &p.Status.Steps.Execution.Sandbox
		case agenticv1alpha1.SandboxStepVerification:
			return &p.Status.Steps.Verification.Sandbox
		}
	}

	if p.Status.Steps.Verification.Sandbox.ClaimName != "" {
		return &p.Status.Steps.Verification.Sandbox
	}
	if p.Status.Steps.Execution.Sandbox.ClaimName != "" {
		return &p.Status.Steps.Execution.Sandbox
	}
	if p.Status.Steps.Analysis.Sandbox.ClaimName != "" {
		return &p.Status.Steps.Analysis.Sandbox
	}
	return nil
}
