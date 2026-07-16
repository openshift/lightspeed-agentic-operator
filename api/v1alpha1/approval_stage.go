package v1alpha1

// NewApprovalStage builds a valid ApprovalStage for the given type.
// The discriminant arm is always non-nil (including empty {}) so JSON
// serialization satisfies CRD CEL has(self.<arm>) rules. agent is only
// set when non-empty (omitted means no override).
func NewApprovalStage(typ ApprovalStageType, decision ApprovalDecision, agent string, option *int32, maxAttempts int32) ApprovalStage {
	stage := ApprovalStage{Type: typ, Decision: decision}
	switch typ {
	case ApprovalStageAnalysis:
		a := &AnalysisApproval{}
		if agent != "" {
			a.Agent = agent
		}
		stage.Analysis = a
	case ApprovalStageExecution:
		e := &ExecutionApproval{}
		if agent != "" {
			e.Agent = agent
		}
		if option != nil {
			e.Option = option
		}
		if maxAttempts > 0 {
			e.MaxAttempts = maxAttempts
		}
		stage.Execution = e
	case ApprovalStageVerification:
		v := &VerificationApproval{}
		if agent != "" {
			v.Agent = agent
		}
		stage.Verification = v
	case ApprovalStageEscalation:
		e := &EscalationApproval{}
		if agent != "" {
			e.Agent = agent
		}
		stage.Escalation = e
	}
	return stage
}
