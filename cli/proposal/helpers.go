package proposal

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/duration"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	sigsyaml "sigs.k8s.io/yaml"
)

const (
	ColorReset   = "\033[0m"
	ColorRed     = "\033[31m"
	ColorGreen   = "\033[32m"
	ColorYellow  = "\033[33m"
	ColorBlue    = "\033[34m"
	ColorMagenta = "\033[35m"
	ColorCyan    = "\033[36m"
)

const (
	OutputJSON = "json"
	OutputYAML = "yaml"
	OutputWide = "wide"
)

// Keep in sync with ProposalPhase constants in api/v1alpha1/proposal_types.go.
var validProposalPhases = []string{
	string(agenticv1alpha1.ProposalPhasePending),
	string(agenticv1alpha1.ProposalPhaseAnalyzing),
	string(agenticv1alpha1.ProposalPhaseExecuting),
	string(agenticv1alpha1.ProposalPhaseVerifying),
	string(agenticv1alpha1.ProposalPhaseCompleted),
	string(agenticv1alpha1.ProposalPhaseFailed),
	string(agenticv1alpha1.ProposalPhaseDenied),
	string(agenticv1alpha1.ProposalPhaseEscalated),
}

var validSandboxSteps = []string{
	string(agenticv1alpha1.SandboxStepAnalysis),
	string(agenticv1alpha1.SandboxStepExecution),
	string(agenticv1alpha1.SandboxStepVerification),
}

var scheme = func() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	_ = agenticv1alpha1.AddToScheme(s)
	return s
}()

func NewClient(f *genericclioptions.ConfigFlags) (client.Client, error) {
	cfg, err := f.ToRESTConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to get REST config: %w", err)
	}
	return NewClientFromConfig(cfg)
}

func NewClientFromConfig(cfg *rest.Config) (client.Client, error) {
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		return nil, fmt.Errorf("failed to create client: %w", err)
	}
	return c, nil
}

func ResolveNamespace(f *genericclioptions.ConfigFlags) (string, error) {
	if f.Namespace != nil && *f.Namespace != "" {
		return *f.Namespace, nil
	}
	rawConfig, err := f.ToRawKubeConfigLoader().RawConfig()
	if err != nil {
		return "", fmt.Errorf("failed to load kubeconfig: %w", err)
	}
	if ctx, ok := rawConfig.Contexts[rawConfig.CurrentContext]; ok && ctx.Namespace != "" {
		return ctx.Namespace, nil
	}
	return "default", nil
}

func IsTerminalPhase(phase agenticv1alpha1.ProposalPhase) bool {
	switch phase {
	case agenticv1alpha1.ProposalPhaseCompleted,
		agenticv1alpha1.ProposalPhaseFailed,
		agenticv1alpha1.ProposalPhaseEscalated,
		agenticv1alpha1.ProposalPhaseDenied:
		return true
	}
	return false
}

func PhaseColor(phase agenticv1alpha1.ProposalPhase) string {
	switch phase {
	case agenticv1alpha1.ProposalPhaseCompleted:
		return ColorGreen
	case agenticv1alpha1.ProposalPhaseFailed,
		agenticv1alpha1.ProposalPhaseDenied:
		return ColorRed
	case agenticv1alpha1.ProposalPhaseAnalyzing,
		agenticv1alpha1.ProposalPhaseExecuting,
		agenticv1alpha1.ProposalPhaseVerifying:
		return ColorYellow
	case agenticv1alpha1.ProposalPhaseEscalated:
		return ColorMagenta
	default:
		return ColorReset
	}
}

func ColoredPhase(phase agenticv1alpha1.ProposalPhase) string {
	return PhaseColor(phase) + string(phase) + ColorReset
}

func HumanDuration(t time.Time) string {
	return duration.HumanDuration(time.Since(t))
}

func PrintTable(w io.Writer, headers []string, rows [][]string) {
	tw := tabwriter.NewWriter(w, 0, 4, 3, ' ', 0)
	fmt.Fprintln(tw, strings.Join(headers, "\t"))
	for _, row := range rows {
		fmt.Fprintln(tw, strings.Join(row, "\t"))
	}
	tw.Flush()
}

func MarshalOutput(w io.Writer, obj interface{}, format string) error {
	switch format {
	case OutputJSON:
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(obj)
	case OutputYAML:
		data, err := sigsyaml.Marshal(obj)
		if err != nil {
			return err
		}
		_, err = w.Write(data)
		return err
	default:
		return fmt.Errorf("unknown output format: %s", format)
	}
}

func SortProposalsByAge(items []agenticv1alpha1.Proposal) {
	sort.Slice(items, func(i, j int) bool {
		return items[j].CreationTimestamp.Before(&items[i].CreationTimestamp)
	})
}

func IsValidPhase(phase string) bool {
	for _, p := range validProposalPhases {
		if p == phase {
			return true
		}
	}
	return false
}

func IsValidStep(step string) bool {
	lower := strings.ToLower(step)
	for _, s := range validSandboxSteps {
		if strings.ToLower(s) == lower {
			return true
		}
	}
	return false
}

func NormalizeStep(step string) agenticv1alpha1.SandboxStep {
	lower := strings.ToLower(step)
	for _, s := range validSandboxSteps {
		if strings.ToLower(s) == lower {
			return agenticv1alpha1.SandboxStep(s)
		}
	}
	return agenticv1alpha1.SandboxStep(step)
}

func ValidateOutputFormat(format string, allowWide bool) error {
	if format == "" {
		return nil
	}
	valid := []string{OutputJSON, OutputYAML}
	if allowWide {
		valid = append(valid, OutputWide)
	}
	for _, v := range valid {
		if format == v {
			return nil
		}
	}
	return fmt.Errorf("invalid output format %q, must be one of: %s", format, strings.Join(valid, ", "))
}

func stepStatusFromConditions(conditions []metav1.Condition, condType string) string {
	for _, c := range conditions {
		if c.Type == condType {
			switch c.Status {
			case metav1.ConditionTrue:
				return ColorGreen + "True" + ColorReset + " (" + c.Reason + ")"
			case metav1.ConditionFalse:
				return ColorRed + "False" + ColorReset + " (" + c.Reason + ")"
			default:
				return ColorYellow + "Unknown" + ColorReset + " (" + c.Reason + ")"
			}
		}
	}
	return "-"
}

func valueOrDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func int32PtrStr(p *int32) string {
	if p == nil {
		return "-"
	}
	return fmt.Sprintf("%d", *p)
}
