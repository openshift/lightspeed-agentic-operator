package agenticrun

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
)

const (
	ErrAugmentOptions = "augment analysis options"

	// maxRBACRules mirrors the CRD MaxItems=50 on RBACResult lists
	// (agenticrun_analysis_types.go).
	maxRBACRules = 50

	// augmentPatchAttempts is the number of retries for patching
	// AnalysisResult status.
	augmentPatchAttempts = 5
)

// optionPredicate is a prerequisite for an applying an optionTransform.
type optionPredicate func(*agenticv1alpha1.RemediationOption) bool

// optionTransform mutates an option and reports whether anything changed.
type optionTransform func(*agenticv1alpha1.RemediationOption) (bool, error)

// optionAugmentation groups an optionTransform with its prerequisites.
type optionAugmentation struct {
	name       string
	predicates []optionPredicate
	transform  optionTransform
}

var (
	// resourceReadCmd matches oc/kubectl commands that read resources
	// (get, wait, describe). Quote characters count as token boundaries so
	// wrapped commands (bash -c "oc get …") match.
	resourceReadCmd = regexp.MustCompile(`(?:^|[\s"'])(?:oc|kubectl)\s+(?:get|wait|describe)(?:\s|$)`)

	// clusterExtensionsResource matches a ClusterExtension reference in any
	// valid form: singular, plural, or fully qualified.
	clusterExtensionsResource = regexp.MustCompile(`(?:^|[\s"'])clusterextensions?(?:\.olm\.operatorframework\.io)?(?:[\s"']|$)`)
)

// clusterExtensionsReadRule is appended to every option whose remediation
// plan reads OLM ClusterExtensions: read-only access covering the
// read commands (oc wait / oc get) that follow a clusterextension patch.
var clusterExtensionsReadRule = agenticv1alpha1.RBACRule{
	APIGroups:     []string{"olm.operatorframework.io"},
	Resources:     []string{"clusterextensions"},
	Verbs:         []string{"get", "list", "watch"},
	Justification: "Option includes commands reading OLM ClusterExtensions; grants the read access its actions require",
}

// readsClusterExtensions matches options whose remediation plan includes an
// oc/kubectl command reading OLM ClusterExtensions.
func readsClusterExtensions(option *agenticv1alpha1.RemediationOption) bool {
	for _, action := range option.RemediationPlan.Actions {
		// check piped, chained, or separate commands separately
		splitCommand := func(command string) []string {
			return strings.FieldsFunc(command, func(r rune) bool {
				return r == '|' || r == '&' || r == ';' || r == '\n'
			})
		}
		for _, cmd := range splitCommand(action.Command) {
			if resourceReadCmd.MatchString(cmd) && clusterExtensionsResource.MatchString(cmd) {
				return true
			}
		}
	}
	return false
}

var preApprovalAugmentations = []optionAugmentation{
	{
		name:       "clusterextensions-read-rbac",
		predicates: []optionPredicate{readsClusterExtensions},
		transform:  appendClusterRBACRule(clusterExtensionsReadRule),
	},
}

// augmentOptions applies the given optionAugmentations to the options on the AnalysisResult.
func (r *AgenticRunReconciler) augmentOptions(ctx context.Context, ar *agenticv1alpha1.AnalysisResult, augmentations []optionAugmentation) error {
	// Re-read through the uncached reader: the sandbox created this CR
	// moments ago and the cached client may not have observed it yet.
	for range augmentPatchAttempts {
		fresh := &agenticv1alpha1.AnalysisResult{}
		if err := r.APIReader.Get(ctx, client.ObjectKeyFromObject(ar), fresh); err != nil {
			return fmt.Errorf("%s %s: %w", ErrAugmentOptions, ar.Name, err)
		}

		// Capture the pre-mutation base before applying augmentations —
		// MergeFrom diffs obj against base to build the patch.
		base := fresh.DeepCopy()

		changed := false
		for i := range fresh.Status.Options {
			option := &fresh.Status.Options[i]
			for _, augmentation := range augmentations {
				var failedPredicate bool
				// check for all predicates
				for _, predicate := range augmentation.predicates {
					if !predicate(option) {
						failedPredicate = true
						break
					}
				}
				if failedPredicate {
					break
				}

				// apply augmentation
				optChanged, err := augmentation.transform(option)
				if err != nil {
					return fmt.Errorf("%s %s (%q): %w", ErrAugmentOptions, fresh.Name, augmentation.name, err)
				}
				changed = changed || optChanged
			}
		}

		if !changed {
			ar.Status.Options = fresh.Status.Options
			return nil
		}

		if err := r.Status().Patch(ctx, fresh, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
			if apierrors.IsConflict(err) {
				continue // re-read and re-apply; transforms are idempotent
			}
			return fmt.Errorf("%s %s: %w", ErrAugmentOptions, ar.Name, err)
		}
		ar.Status.Options = fresh.Status.Options
		return nil
	}
	return fmt.Errorf("%s %s: status conflict retry limit reached", ErrAugmentOptions, ar.Name)
}

// ruleExists reports whether rules already contains an identical rule.
func ruleExists(rules []agenticv1alpha1.RBACRule, rule agenticv1alpha1.RBACRule) bool {
	for _, existing := range rules {
		if equality.Semantic.DeepEqual(existing, rule) {
			return true
		}
	}
	return false
}

// appendClusterRBACRule appends rule to the option's cluster-scoped RBAC.
// Idempotent — an identical existing rule is a no-op.
func appendClusterRBACRule(rule agenticv1alpha1.RBACRule) optionTransform {
	return func(option *agenticv1alpha1.RemediationOption) (bool, error) {
		if ruleExists(option.RBAC.ClusterScoped, rule) {
			return false, nil
		}
		if len(option.RBAC.ClusterScoped) >= maxRBACRules {
			return false, fmt.Errorf("option %q at cluster-scoped rule limit", option.Title)
		}
		option.RBAC.ClusterScoped = append(option.RBAC.ClusterScoped, rule)
		return true, nil
	}
}
