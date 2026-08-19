package run

import (
	"bufio"
	"context"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
	"github.com/spf13/cobra"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// validTerminalStates lists the terminal phases accepted by --state.
var validTerminalStates = []string{
	"completed", "failed", "denied", "escalated", "emergencystopped", "noactionrequired",
}

type CleanupOptions struct {
	configFlags   *genericclioptions.ConfigFlags
	allNamespaces bool
	states        string
	olderThan     string
	dryRun        bool
	yes           bool

	// parsed values
	stateFilter    map[string]bool
	olderThanDur   time.Duration
	hasOlderThan   bool
	hasStateFilter bool

	client    client.Client
	namespace string

	genericclioptions.IOStreams
}

func NewCleanupCmd(streams genericclioptions.IOStreams) *cobra.Command {
	o := &CleanupOptions{
		configFlags: genericclioptions.NewConfigFlags(true),
		IOStreams:   streams,
	}

	cmd := &cobra.Command{
		Use:   "cleanup",
		Short: "Delete terminal AgenticRun resources in batch",
		Long: `Delete terminal AgenticRun resources matching the specified filters.

Terminal states: completed, failed, denied, escalated, emergencystopped, noactionrequired.
Kubernetes garbage collection cascades deletion to owned resources via owner references.

This is a destructive, irreversible operation. Unless --yes is passed, the
matching runs are listed and a confirmation prompt is shown before anything
is deleted. Use --dry-run to preview matches without any prompt.`,
		Example: `  # Delete all terminal runs in current namespace
  oc agentic run cleanup

  # Dry-run to see what would be deleted
  oc agentic run cleanup --dry-run

  # Delete only completed and failed runs older than 7 days
  oc agentic run cleanup --state=completed,failed --older-than=7d

  # Delete all terminal runs across all namespaces
  oc agentic run cleanup -A

  # Delete denied runs older than 24 hours
  oc agentic run cleanup --state=denied --older-than=24h

  # Delete without prompting for confirmation
  oc agentic run cleanup --yes`,
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
	cmd.Flags().BoolVarP(&o.allNamespaces, "all-namespaces", "A", false, "Delete terminal runs across all namespaces")
	cmd.Flags().StringVar(&o.states, "state", "", "Comma-separated terminal states to include (completed,failed,denied,escalated,emergencystopped,noactionrequired)")
	cmd.Flags().StringVar(&o.olderThan, "older-than", "", "Only runs terminal longer than this duration (e.g. 7d, 24h, 30m)")
	cmd.Flags().BoolVar(&o.dryRun, "dry-run", false, "List matching runs without deleting")
	cmd.Flags().BoolVarP(&o.yes, "yes", "y", false, "Skip confirmation prompt")

	return cmd
}

func (o *CleanupOptions) Complete(_ *cobra.Command, _ []string) error {
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

func (o *CleanupOptions) Validate() error {
	if o.states != "" {
		o.stateFilter = make(map[string]bool)
		o.hasStateFilter = true
		for _, s := range strings.Split(o.states, ",") {
			s = strings.TrimSpace(strings.ToLower(s))
			if s == "" {
				continue
			}
			valid := false
			for _, v := range validTerminalStates {
				if s == v {
					valid = true
					break
				}
			}
			if !valid {
				return fmt.Errorf("invalid state %q, must be one of: %s", s, strings.Join(validTerminalStates, ", "))
			}
			o.stateFilter[s] = true
		}
	}

	if o.olderThan != "" {
		dur, err := parseDuration(o.olderThan)
		if err != nil {
			return fmt.Errorf("invalid --older-than value %q: %w", o.olderThan, err)
		}
		o.olderThanDur = dur
		o.hasOlderThan = true
	}

	return nil
}

func (o *CleanupOptions) Run(ctx context.Context) error {
	list := &agenticv1alpha1.AgenticRunList{}
	var opts []client.ListOption
	if !o.allNamespaces {
		opts = append(opts, client.InNamespace(o.namespace))
	}

	if err := o.client.List(ctx, list, opts...); err != nil {
		return fmt.Errorf("failed to list agentic runs: %w", err)
	}

	// Filter to terminal runs matching criteria.
	var matched []agenticv1alpha1.AgenticRun
	for i := range list.Items {
		run := &list.Items[i]
		phase := agenticv1alpha1.DerivePhase(run.Status.Conditions)

		if !IsTerminalPhase(phase) {
			continue
		}

		if o.hasStateFilter && !o.stateFilter[strings.ToLower(string(phase))] {
			continue
		}

		if o.hasOlderThan {
			if run.Status.TerminalTime == nil {
				fmt.Fprintf(o.ErrOut, "Warning: run/%s has no terminalTime, skipping (--older-than requires terminalTime)\n", run.Name)
				continue
			}
			if time.Since(run.Status.TerminalTime.Time) < o.olderThanDur {
				continue
			}
		}

		matched = append(matched, *run)
	}

	if len(matched) == 0 {
		fmt.Fprintln(o.Out, "No matching terminal runs found.")
		return nil
	}

	SortAgenticRunsByAge(matched)

	if o.dryRun {
		o.printRunsTable(matched)
		fmt.Fprintf(o.Out, "\n%d run(s) would be deleted (dry-run).\n", len(matched))
		return nil
	}

	if !o.yes {
		o.printRunsTable(matched)
		fmt.Fprintf(o.Out, "\nThis will permanently delete %d run(s) listed above. Continue? [y/N] ", len(matched))
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

	deleted := 0
	failed := 0
	for i := range matched {
		run := &matched[i]
		if err := client.IgnoreNotFound(o.client.Delete(ctx, run)); err != nil {
			if o.allNamespaces {
				fmt.Fprintf(o.ErrOut, "Warning: failed to delete run/%s -n %s: %v\n", run.Name, run.Namespace, err)
			} else {
				fmt.Fprintf(o.ErrOut, "Warning: failed to delete run/%s: %v\n", run.Name, err)
			}
			failed++
			continue
		}
		if o.allNamespaces {
			fmt.Fprintf(o.Out, "run/%s deleted -n %s\n", run.Name, run.Namespace)
		} else {
			fmt.Fprintf(o.Out, "run/%s deleted\n", run.Name)
		}
		deleted++
	}

	fmt.Fprintf(o.Out, "Deleted %d run(s).\n", deleted)
	if failed > 0 {
		return fmt.Errorf("failed to delete %d of %d run(s)", failed, len(matched))
	}
	return nil
}

func (o *CleanupOptions) printRunsTable(items []agenticv1alpha1.AgenticRun) {
	var headers []string
	if o.allNamespaces {
		headers = []string{"NAMESPACE", "NAME", "PHASE", "TERMINAL-AGE"}
	} else {
		headers = []string{"NAME", "PHASE", "TERMINAL-AGE"}
	}
	rows := make([][]string, 0, len(items))
	for _, p := range items {
		terminalAge := "<none>"
		if p.Status.TerminalTime != nil {
			terminalAge = HumanDuration(p.Status.TerminalTime.Time)
		}
		row := []string{}
		if o.allNamespaces {
			row = append(row, p.Namespace)
		}
		row = append(row, p.Name, ColoredPhase(agenticv1alpha1.DerivePhase(p.Status.Conditions)), terminalAge)
		rows = append(rows, row)
	}
	PrintTable(o.Out, headers, rows)
}

// daysPattern matches durations like "7d", "30d".
var daysPattern = regexp.MustCompile(`^(\d+)d$`)

// maxDurationDays is the largest day count that converts to a time.Duration
// (int64 nanoseconds) without overflowing.
const maxDurationDays = int(math.MaxInt64 / int64(24*time.Hour))

// parseDuration parses a duration string supporting Go durations and Nd (days).
func parseDuration(s string) (time.Duration, error) {
	if m := daysPattern.FindStringSubmatch(s); m != nil {
		days, err := strconv.Atoi(m[1])
		if err != nil {
			return 0, err
		}
		if days > maxDurationDays {
			return 0, fmt.Errorf("value too large: max supported is %dd", maxDurationDays)
		}
		return time.Duration(days) * 24 * time.Hour, nil
	}
	return time.ParseDuration(s)
}
