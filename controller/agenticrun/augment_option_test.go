package agenticrun

import (
	"context"
	"fmt"
	"testing"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// ceReadOption returns a remediation option with a patch+wait on clusteextensions
func ceReadOption(title string, clusterRBAC ...agenticv1alpha1.RBACRule) agenticv1alpha1.RemediationOption {
	result := agenticv1alpha1.RemediationOption{
		Title: title,
		RemediationPlan: agenticv1alpha1.RemediationPlan{
			Description: "Update the my-operator ClusterExtension",
			Actions: []agenticv1alpha1.ProposedAction{
				{
					Command:     `oc patch clusterextensions.olm.operatorframework.io my-operator --type=merge -p '{"spec":{"source":{"catalog":{"version":">=1.2.0 <2.0.0"}}}}'`,
					Type:        "mutation",
					Description: "Patch the ClusterExtension version range",
				},
				{
					Command:     "oc wait clusterextensions.olm.operatorframework.io my-operator --for=condition=Installed=True --timeout=300s",
					Type:        "wait",
					Description: "Wait for the update to complete",
				},
			},
			Reversible: agenticv1alpha1.ReversibilityReversible,
		},
	}
	if len(clusterRBAC) > 0 {
		result.RBAC.ClusterScoped = clusterRBAC
	}
	return result
}

func testOption() agenticv1alpha1.RemediationOption {
	return agenticv1alpha1.RemediationOption{
		Title: "Restart deployment",
		RemediationPlan: agenticv1alpha1.RemediationPlan{
			Description: "Restart the web deployment",
			Actions: []agenticv1alpha1.ProposedAction{{
				Command:     "oc rollout restart deployment/web -n default",
				Type:        "mutation",
				Description: "Restart",
			}},
		},
	}
}

// analysisResultWithOptions return an AnalysisResult CR with Completed=True and the given options.
func analysisResultWithOptions(name string, options ...agenticv1alpha1.RemediationOption) *agenticv1alpha1.AnalysisResult {
	return &agenticv1alpha1.AnalysisResult{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec:       agenticv1alpha1.AnalysisResultSpec{AgenticRunName: "fix-crash"},
		Status: agenticv1alpha1.AnalysisResultStatus{
			ActionRequired: agenticv1alpha1.ActionRequiredTrue,
			Options:        options,
			Conditions: []metav1.Condition{{
				Type:   agenticv1alpha1.ResultConditionCompleted,
				Status: metav1.ConditionTrue,
				Reason: agenticv1alpha1.ResultReasonSucceeded,
			}},
		},
	}
}

// analysisRunInProgress returns an AgenticRun with analysis in progress
func analysisRunInProgress() *agenticv1alpha1.AgenticRun {
	run := testAgenticRun()
	meta.SetStatusCondition(&run.Status.Conditions, metav1.Condition{
		Type:               agenticv1alpha1.AgenticRunConditionAnalyzed,
		Status:             metav1.ConditionUnknown,
		Reason:             reasonInProgress,
		Message:            "Analysis agent is running",
		ObservedGeneration: run.Generation,
	})
	return run
}

func newAugmentReconciler(t *testing.T, objs ...client.Object) (*AgenticRunReconciler, client.WithWatch) {
	t.Helper()
	fc := fake.NewClientBuilder().WithScheme(testScheme()).
		WithObjects(objs...).
		WithStatusSubresource(objs...).
		Build()
	return &AgenticRunReconciler{Client: fc, APIReader: fc, Namespace: "default"}, fc
}

func succeededPod() *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "ls-analysis-test", Namespace: "default"},
		Status:     corev1.PodStatus{Phase: corev1.PodSucceeded},
	}
}

func maxRules() []agenticv1alpha1.RBACRule {
	rules := make([]agenticv1alpha1.RBACRule, maxRBACRules)
	for i := range rules {
		rules[i] = agenticv1alpha1.RBACRule{
			APIGroups:     []string{"example.com"},
			Resources:     []string{"foos"},
			Verbs:         []string{"get"},
			Justification: "filler rule",
		}
	}
	return rules
}

func TestReadsClusterExtensions(t *testing.T) {
	tests := []struct {
		name    string
		command string
		want    bool
	}{
		{"bare get", "oc get clusterextensions", true},
		{"fully qualified get", "oc get clusterextensions.olm.operatorframework.io my-ext -o jsonpath='{.status.install.bundle.version}'", true},
		{"wait", "oc wait clusterextensions.olm.operatorframework.io my-ext --for=condition=Installed=True --timeout=300s", true},
		{"kubectl describe singular", "kubectl describe clusterextension my-ext", true},
		{"flags before resource", "oc get -A clusterextensions", true},
		{"read in pipeline", "oc get clusterextensions | wc -l", true},
		{"read after semicolon", "oc get pods; oc get clusterextensions", true},
		{"read in shell wrapper", `bash -c "oc adm upgrade --to-latest && oc get clusterextensions"`, true},
		{"read of another resource", "oc get clusterversions version", false},
		{"patch only", "oc patch clusterextensions.olm.operatorframework.io my-ext --type=merge -p '{}'", false},
		{"pipeline grep", "oc get pods | grep clusterextensions", false},
		{"semicolon grep", "oc get pods; grep clusterextensions", false},
		{"kubectl other resource", "kubectl get pods", false},
		{"adjacent token", "oc get clusterextensions.txt", false},
		{"echo of a read command", `echo "oc get clusterextensions"`, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			option := agenticv1alpha1.RemediationOption{
				Title: "test",
				RemediationPlan: agenticv1alpha1.RemediationPlan{
					Actions: []agenticv1alpha1.ProposedAction{{
						Command: tt.command, Type: "mutation", Description: "test",
					}},
				},
			}
			if got := readsClusterExtensions(&option); got != tt.want {
				t.Errorf("readsClusterExtensions(%q) = %v, want %v", tt.command, got, tt.want)
			}
		})
	}
}

func TestAugmentOptions(t *testing.T) {
	tests := []struct {
		name          string
		existing      *agenticv1alpha1.AnalysisResult
		augmentations []optionAugmentation
		expected      *agenticv1alpha1.AnalysisResult
		wantErr       string
	}{
		{
			name: "augmentat rbac only on matching option missing rbac",
			existing: analysisResultWithOptions("fix-crash-analysis-1", ceReadOption("Update my-operator-missing-rbac"),
				ceReadOption("Update my-operator-with-rbac", clusterExtensionsReadRule), testOption()),
			augmentations: preApprovalAugmentations,
			expected: analysisResultWithOptions("fix-crash-analysis-1", ceReadOption("Update my-operator-missing-rbac", clusterExtensionsReadRule),
				ceReadOption("Update my-operator-with-rbac", clusterExtensionsReadRule), testOption()),
		},
		{
			name:          "augmentation rbac absent on matching option",
			existing:      analysisResultWithOptions("fix-crash-analysis-1", ceReadOption("Update my-operator-missing-rbac", maxRules()...)),
			augmentations: preApprovalAugmentations,
			wantErr:       "augment analysis options fix-crash-analysis-1 (\"clusterextensions-read-rbac\"): option \"Update my-operator-missing-rbac\" at cluster-scoped rule limit",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			r, fc := newAugmentReconciler(t, tt.existing)
			err := r.augmentOptions(ctx, tt.existing, tt.augmentations)

			var actual agenticv1alpha1.AnalysisResult
			if getErr := fc.Get(ctx, client.ObjectKeyFromObject(tt.existing), &actual); getErr != nil {
				t.Fatalf("get AnalysisResult: %v", getErr)
			}

			if tt.wantErr != "" {
				if err == nil || err.Error() != tt.wantErr {
					t.Errorf("expected error: %s, actual: %s", tt.wantErr, err)
				}
				if !equality.Semantic.DeepEqual(actual.Status, tt.existing.Status) {
					t.Fatalf("expected analysisResult to be unchanged: expected status: %+v, got: %+v", tt.existing.Status, actual.Status)
				}
				return
			}

			if err != nil {
				t.Errorf("augmentOptions: %v", err)
			}
			if !equality.Semantic.DeepEqual(actual.Status, tt.expected.Status) {
				t.Fatalf("expected analysisResult status: %+v, got %+v", tt.expected.Status, actual.Status)
			}
		})
	}
}
func TestCompleteStepAugments(t *testing.T) {
	tests := []struct {
		name           string
		existing       *agenticv1alpha1.AnalysisResult
		expectedStatus metav1.ConditionStatus
		wantErr        error
	}{
		{
			name:           "successful augmentation on agenticrun",
			existing:       analysisResultWithOptions("fix-crash-analysis-1", ceReadOption("Update my-operator")),
			expectedStatus: metav1.ConditionTrue,
		},
		{
			name:           "failing augmentation on agenticrun",
			existing:       analysisResultWithOptions("fix-crash-analysis-1", ceReadOption("Update my-operator-missing-rbac", maxRules()...)),
			expectedStatus: metav1.ConditionUnknown,
			wantErr:        fmt.Errorf("augment analysis options fix-crash-analysis-1 (\"clusterextensions-read-rbac\"): option \"Update my-operator-missing-rbac\" at cluster-scoped rule limit"),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			run := analysisRunInProgress()

			fc := fake.NewClientBuilder().WithScheme(testScheme()).
				WithObjects(run, tt.existing).
				WithStatusSubresource(run, tt.existing).
				Build()
			r := &AgenticRunReconciler{
				Client:    fc,
				APIReader: fc,
				Agent:     newTestAgentCaller().withClient(t, fc, "default"),
				Namespace: "default",
			}

			if err := r.completeStep(ctx, run, succeededPod(), "analysis", agenticv1alpha1.AgenticRunConditionAnalyzed, "", ""); err != nil {
				if tt.wantErr != nil && err.Error() != tt.wantErr.Error() {
					t.Errorf("completeStep: expected error: %s, got error: %s", tt.wantErr, err)
				} else if tt.wantErr == nil {
					t.Errorf("completeStep: expected step to succeed, got error: %s", err)
				}
			} else if tt.wantErr != nil {
				t.Errorf("completeStep: expected step to fail with error: %s", tt.wantErr)
			}

			actualRun, err := getAgenticRun(r, "fix-crash")
			if err != nil {
				t.Errorf("get run: %v", err)
			}
			actualConditionAnalyzed := meta.FindStatusCondition(actualRun.Status.Conditions, agenticv1alpha1.AgenticRunConditionAnalyzed)
			if actualConditionAnalyzed == nil {
				t.Errorf("expected condition %s to be present on agenticRun", agenticv1alpha1.AgenticRunConditionAnalyzed)
			} else if actualConditionAnalyzed.Status != tt.expectedStatus {
				t.Errorf("expected condition %s on agenticRun to be: %s, got: %s", agenticv1alpha1.AgenticRunConditionAnalyzed, tt.expectedStatus, actualConditionAnalyzed.Status)
			}
		})
	}
}
