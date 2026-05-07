package proposal

import (
	"testing"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
)

func TestWatch_ProposalGVR(t *testing.T) {
	if proposalGVR.Group != "agentic.openshift.io" {
		t.Errorf("expected group agentic.openshift.io, got %s", proposalGVR.Group)
	}
	if proposalGVR.Version != "v1alpha1" {
		t.Errorf("expected version v1alpha1, got %s", proposalGVR.Version)
	}
	if proposalGVR.Resource != "proposals" {
		t.Errorf("expected resource proposals, got %s", proposalGVR.Resource)
	}
}

func TestWatch_TerminalPhaseExits(t *testing.T) {
	terminal := []agenticv1alpha1.ProposalPhase{
		agenticv1alpha1.ProposalPhaseCompleted,
		agenticv1alpha1.ProposalPhaseFailed,
		agenticv1alpha1.ProposalPhaseDenied,
		agenticv1alpha1.ProposalPhaseEscalated,
	}
	for _, p := range terminal {
		if !IsTerminalPhase(p) {
			t.Errorf("expected %s to be terminal", p)
		}
	}

	nonTerminal := []agenticv1alpha1.ProposalPhase{
		agenticv1alpha1.ProposalPhasePending,
		agenticv1alpha1.ProposalPhaseAnalyzing,
		agenticv1alpha1.ProposalPhaseExecuting,
		agenticv1alpha1.ProposalPhaseVerifying,
	}
	for _, p := range nonTerminal {
		if IsTerminalPhase(p) {
			t.Errorf("expected %s to be non-terminal", p)
		}
	}
}
