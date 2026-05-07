package proposal

import (
	"context"
	"fmt"
	"io"
	"time"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/dynamic"
)

var proposalGVR = agenticv1alpha1.GroupVersion.WithResource("proposals")

type WatchOptions struct {
	configFlags *genericclioptions.ConfigFlags
	name        string

	namespace string

	genericclioptions.IOStreams
}

func NewWatchCmd(streams genericclioptions.IOStreams) *cobra.Command {
	o := &WatchOptions{
		configFlags: genericclioptions.NewConfigFlags(true),
		IOStreams:    streams,
	}

	cmd := &cobra.Command{
		Use:   "watch NAME",
		Short: "Watch a proposal's phase transitions",
		Example: `  # Watch a proposal until it reaches a terminal phase
  oc agentic proposal watch fix-crash`,
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

func (o *WatchOptions) Complete(cmd *cobra.Command, args []string) error {
	o.name = args[0]
	var err error
	o.namespace, err = ResolveNamespace(o.configFlags)
	return err
}

func (o *WatchOptions) Run(ctx context.Context) error {
	return doWatch(ctx, o.configFlags, o.namespace, o.name, o.Out)
}

func doWatch(ctx context.Context, configFlags *genericclioptions.ConfigFlags, namespace, name string, w io.Writer) error {
	cfg, err := configFlags.ToRESTConfig()
	if err != nil {
		return fmt.Errorf("failed to get REST config: %w", err)
	}

	dynClient, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return fmt.Errorf("failed to create dynamic client: %w", err)
	}

	watcher, err := dynClient.Resource(proposalGVR).Namespace(namespace).Watch(ctx, metav1.ListOptions{
		FieldSelector: fmt.Sprintf("metadata.name=%s", name),
	})
	if err != nil {
		return fmt.Errorf("failed to watch proposal %q: %w", name, err)
	}
	defer watcher.Stop()

	start := time.Now()
	lastPhase := ""

	fmt.Fprintln(w, "PHASE\tAGE")

	for event := range watcher.ResultChan() {
		if event.Type == watch.Error {
			return fmt.Errorf("watch error: %v", event.Object)
		}
		if event.Type != watch.Modified && event.Type != watch.Added {
			continue
		}

		obj, ok := event.Object.(*unstructured.Unstructured)
		if !ok {
			continue
		}

		conditions := extractConditions(obj)
		phase := string(agenticv1alpha1.DerivePhase(conditions))
		if phase == lastPhase {
			continue
		}
		lastPhase = phase

		elapsed := time.Since(start).Round(time.Second)
		p := agenticv1alpha1.ProposalPhase(phase)
		fmt.Fprintf(w, "%s\t%s\n", ColoredPhase(p), elapsed)

		if IsTerminalPhase(p) {
			return nil
		}
	}

	return nil
}

func extractConditions(obj *unstructured.Unstructured) []metav1.Condition {
	raw, _, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
	conditions := make([]metav1.Condition, 0, len(raw))
	for _, item := range raw {
		m, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		c := metav1.Condition{}
		if v, ok := m["type"].(string); ok {
			c.Type = v
		}
		if v, ok := m["status"].(string); ok {
			c.Status = metav1.ConditionStatus(v)
		}
		if v, ok := m["reason"].(string); ok {
			c.Reason = v
		}
		conditions = append(conditions, c)
	}
	return conditions
}
