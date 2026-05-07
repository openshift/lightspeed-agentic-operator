package proposal

import (
	"bytes"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
)

func testScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	_ = agenticv1alpha1.AddToScheme(s)
	return s
}

func testProposal(name, namespace string) *agenticv1alpha1.Proposal {
	return &agenticv1alpha1.Proposal{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         namespace,
			CreationTimestamp: metav1.Now(),
		},
		Spec: agenticv1alpha1.ProposalSpec{
			Request:          "Pod crashing in production",
			TargetNamespaces: []string{"production"},
			Analysis:         agenticv1alpha1.ProposalStep{Agent: "default"},
		},
	}
}

func testProposalWithStatus(name, namespace string, phase agenticv1alpha1.ProposalPhase) *agenticv1alpha1.Proposal {
	p := testProposal(name, namespace)
	p.Status = agenticv1alpha1.ProposalStatus{}
	setPhaseConditions(&p.Status, phase)
	return p
}

func setPhaseConditions(s *agenticv1alpha1.ProposalStatus, phase agenticv1alpha1.ProposalPhase) {
	switch phase {
	case agenticv1alpha1.ProposalPhaseDenied:
		meta.SetStatusCondition(&s.Conditions, metav1.Condition{
			Type: agenticv1alpha1.ProposalConditionDenied, Status: metav1.ConditionTrue, Reason: "UserDenied",
		})
	case agenticv1alpha1.ProposalPhaseAnalyzing:
		meta.SetStatusCondition(&s.Conditions, metav1.Condition{
			Type: agenticv1alpha1.ProposalConditionAnalyzed, Status: metav1.ConditionUnknown, Reason: "InProgress",
		})
	case agenticv1alpha1.ProposalPhaseExecuting:
		meta.SetStatusCondition(&s.Conditions, metav1.Condition{
			Type: agenticv1alpha1.ProposalConditionAnalyzed, Status: metav1.ConditionTrue, Reason: "AnalysisComplete",
		})
	case agenticv1alpha1.ProposalPhaseVerifying:
		meta.SetStatusCondition(&s.Conditions, metav1.Condition{
			Type: agenticv1alpha1.ProposalConditionAnalyzed, Status: metav1.ConditionTrue, Reason: "AnalysisComplete",
		})
		meta.SetStatusCondition(&s.Conditions, metav1.Condition{
			Type: agenticv1alpha1.ProposalConditionExecuted, Status: metav1.ConditionTrue, Reason: "ExecutionComplete",
		})
	case agenticv1alpha1.ProposalPhaseCompleted:
		meta.SetStatusCondition(&s.Conditions, metav1.Condition{
			Type: agenticv1alpha1.ProposalConditionAnalyzed, Status: metav1.ConditionTrue, Reason: "AnalysisComplete",
		})
		meta.SetStatusCondition(&s.Conditions, metav1.Condition{
			Type: agenticv1alpha1.ProposalConditionExecuted, Status: metav1.ConditionTrue, Reason: "ExecutionComplete",
		})
		meta.SetStatusCondition(&s.Conditions, metav1.Condition{
			Type: agenticv1alpha1.ProposalConditionVerified, Status: metav1.ConditionTrue, Reason: "VerificationPassed",
		})
	case agenticv1alpha1.ProposalPhaseFailed:
		meta.SetStatusCondition(&s.Conditions, metav1.Condition{
			Type: agenticv1alpha1.ProposalConditionAnalyzed, Status: metav1.ConditionFalse, Reason: "AnalysisFailed",
		})
	case agenticv1alpha1.ProposalPhaseEscalated:
		meta.SetStatusCondition(&s.Conditions, metav1.Condition{
			Type: agenticv1alpha1.ProposalConditionEscalated, Status: metav1.ConditionTrue, Reason: "MaxRetriesExhausted",
		})
	case agenticv1alpha1.ProposalPhasePending:
		// No conditions — fresh proposal
	}
}

func int32Ptr(v int32) *int32 {
	return &v
}

func fakeStreams() (genericclioptions.IOStreams, *bytes.Buffer, *bytes.Buffer) {
	out := &bytes.Buffer{}
	errOut := &bytes.Buffer{}
	streams := genericclioptions.IOStreams{
		In:     &bytes.Buffer{},
		Out:    out,
		ErrOut: errOut,
	}
	return streams, out, errOut
}
