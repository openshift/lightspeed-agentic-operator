package agenticrun

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"reflect"
	"text/template"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
)

//go:embed templates/*.tmpl
var templateFS embed.FS

var templates = template.Must(template.ParseFS(templateFS, "templates/*.tmpl"))

func renderTemplate(name string, data any) string {
	var buf bytes.Buffer
	if err := templates.ExecuteTemplate(&buf, name, data); err != nil {
		return fmt.Sprintf("(template %q error: %v)", name, err)
	}
	return buf.String()
}

const (
	ErrGetAnalysisResult         = "get AnalysisResult"
	ErrTrimAnalysisResultOptions = "trim AnalysisResult options"

	rbacCleanupFinalizer = "agentic.openshift.io/execution-rbac-cleanup"

	reasonInProgress        = "InProgress"
	reasonComplete          = "Complete"
	reasonFailed            = "Failed"
	reasonSkipped           = "Skipped"
	reasonPassed            = "Passed"
	reasonWorkflowFailed    = "WorkflowResolutionFailed"
	reasonUserDenied        = "UserDenied"
	defaultSandboxSA        = "lightspeed-agent"
	reasonRevising          = "Revising"
	reasonRevisionComplete  = "RevisionComplete"
	reasonRetryingExecution = agenticv1alpha1.ReasonRetryingExecution
	reasonRetriesExhausted  = agenticv1alpha1.ReasonRetriesExhausted
	reasonSystemSuspended   = "SystemSuspended"
	reasonNoActionRequired  = agenticv1alpha1.ReasonNoActionRequired

	LogKeyName      = "name"
	LogKeyStep      = "step"
	LogKeyPhase     = "phase"
	LogKeyClaim     = "claimName"
	LogKeyTemplate  = "template"
	LogKeySummary   = "summary"
	LogKeyCondition = "condition"
)

func isSuspended(ctx context.Context, c client.Client) (bool, error) {
	var config agenticv1alpha1.AgenticOLSConfig
	if err := c.Get(ctx, client.ObjectKey{Name: "cluster"}, &config); err != nil {
		if client.IgnoreNotFound(err) == nil {
			return false, nil
		}
		return false, err
	}
	return config.Spec.Suspended, nil
}

func readAuditConfig(ctx context.Context, c client.Client) (*agenticv1alpha1.AuditConfig, error) {
	var config agenticv1alpha1.AgenticOLSConfig
	if err := c.Get(ctx, client.ObjectKey{Name: "cluster"}, &config); err != nil {
		if client.IgnoreNotFound(err) == nil {
			return nil, nil
		}
		return nil, err
	}
	return &config.Spec.Audit, nil
}

// failStep marks a step as failed and creates a failure result CR.
// The caller must have set the step condition to ConditionUnknown before
// calling failStep so that conditionTime can extract the start time.
func (r *AgenticRunReconciler) failStep(ctx context.Context, run *agenticv1alpha1.AgenticRun, conditionType string, err error) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	log.Error(err, "step failed", LogKeyCondition, conditionType)
	base := run.DeepCopy()
	completedAt := metav1.Now()
	startTime := conditionTime(run.Status.Conditions, conditionType)

	var crName string
	var createErr error
	switch conditionType {
	case agenticv1alpha1.AgenticRunConditionAnalyzed:
		crName, _, createErr = r.createAnalysisResult(ctx, run, nil, run.Status.Steps.Analysis.Sandbox, startTime, &completedAt, err.Error())
		if createErr == nil {
			run.Status.Steps.Analysis.Results = append(run.Status.Steps.Analysis.Results, agenticv1alpha1.StepResultRef{Name: crName, Outcome: agenticv1alpha1.ActionOutcomeFailed})
		}
	case agenticv1alpha1.AgenticRunConditionExecuted:
		crName, _, createErr = r.createExecutionResult(ctx, run, nil, run.Status.Steps.Execution.Sandbox, startTime, &completedAt, err.Error())
		if createErr == nil {
			run.Status.Steps.Execution.Results = append(run.Status.Steps.Execution.Results, agenticv1alpha1.StepResultRef{Name: crName, Outcome: agenticv1alpha1.ActionOutcomeFailed})
		}
	case agenticv1alpha1.AgenticRunConditionVerified:
		crName, _, createErr = r.createVerificationResult(ctx, run, nil, run.Status.Steps.Verification.Sandbox, startTime, &completedAt, err.Error())
		if createErr == nil {
			run.Status.Steps.Verification.Results = append(run.Status.Steps.Verification.Results, agenticv1alpha1.StepResultRef{Name: crName, Outcome: agenticv1alpha1.ActionOutcomeFailed})
		}
	case agenticv1alpha1.AgenticRunConditionEscalated:
		crName, _, createErr = r.createEscalationResult(ctx, run, nil, run.Status.Steps.Escalation.Sandbox, startTime, &completedAt, err.Error())
		if createErr == nil {
			run.Status.Steps.Escalation.Results = append(run.Status.Steps.Escalation.Results, agenticv1alpha1.StepResultRef{Name: crName, Outcome: agenticv1alpha1.ActionOutcomeFailed})
		}
	}
	if createErr != nil {
		log.Error(createErr, "failed to create failure result CR")
	}

	meta.SetStatusCondition(&run.Status.Conditions, metav1.Condition{
		Type:               conditionType,
		Status:             metav1.ConditionFalse,
		Reason:             reasonFailed,
		Message:            err.Error(),
		ObservedGeneration: run.Generation,
	})
	if statusErr := r.statusPatch(ctx, run, base); statusErr != nil {
		log.Error(statusErr, "failed to patch status after step failure")
	}
	return ctrl.Result{}, nil
}

func (r *AgenticRunReconciler) statusPatch(ctx context.Context, run *agenticv1alpha1.AgenticRun, base *agenticv1alpha1.AgenticRun) error {
	return r.Status().Patch(ctx, run, client.MergeFrom(base))
}

func hasSandboxClaims(run *agenticv1alpha1.AgenticRun) bool {
	return run.Status.Steps.Analysis.Sandbox.ClaimName != "" ||
		run.Status.Steps.Execution.Sandbox.ClaimName != "" ||
		run.Status.Steps.Verification.Sandbox.ClaimName != "" ||
		run.Status.Steps.Escalation.Sandbox.ClaimName != ""
}

func terminalReason(run *agenticv1alpha1.AgenticRun) string {
	for _, c := range run.Status.Conditions {
		if c.Status == metav1.ConditionFalse && c.Reason == reasonFailed {
			return c.Message
		}
		if c.Status == metav1.ConditionTrue && (c.Reason == reasonUserDenied || c.Reason == reasonSystemSuspended || c.Reason == reasonNoActionRequired) {
			return c.Message
		}
	}
	return ""
}

func isTerminal(phase agenticv1alpha1.AgenticRunPhase) bool {
	switch phase {
	case agenticv1alpha1.AgenticRunPhaseCompleted, agenticv1alpha1.AgenticRunPhaseFailed, agenticv1alpha1.AgenticRunPhaseDenied, agenticv1alpha1.AgenticRunPhaseEscalated, agenticv1alpha1.AgenticRunPhaseEmergencyStopped, agenticv1alpha1.AgenticRunPhaseNoActionRequired:
		return true
	}
	return false
}

func setVerificationSkipped(run *agenticv1alpha1.AgenticRun) {
	meta.SetStatusCondition(&run.Status.Conditions, metav1.Condition{
		Type:               agenticv1alpha1.AgenticRunConditionVerified,
		Status:             metav1.ConditionTrue,
		Reason:             reasonSkipped,
		Message:            "Verification step not configured in workflow",
		ObservedGeneration: run.Generation,
	})
}

func (r *AgenticRunReconciler) getLatestAnalysisResult(ctx context.Context, run *agenticv1alpha1.AgenticRun) (*agenticv1alpha1.AnalysisResult, error) {
	analysis := run.Status.Steps.Analysis
	if len(analysis.Results) == 0 {
		return nil, nil
	}
	latestRef := analysis.Results[len(analysis.Results)-1]
	var result agenticv1alpha1.AnalysisResult
	if err := r.Get(ctx, types.NamespacedName{Name: latestRef.Name, Namespace: run.Namespace}, &result); err != nil {
		return nil, fmt.Errorf("%s %s: %w", ErrGetAnalysisResult, latestRef.Name, err)
	}
	return &result, nil
}

func (r *AgenticRunReconciler) selectedOption(ctx context.Context, run *agenticv1alpha1.AgenticRun) (*agenticv1alpha1.RemediationOption, error) {
	result, err := r.getLatestAnalysisResult(ctx, run)
	if err != nil {
		return nil, err
	}
	if result == nil || len(result.Status.Options) == 0 {
		return nil, nil
	}
	return &result.Status.Options[0], nil
}

// trimNonSelectedOptions keeps only the user-approved option on the
// AnalysisResult, discarding the rest, and returns it. The selected
// index comes from the AgenticRunApproval's execution stage.
func (r *AgenticRunReconciler) trimNonSelectedOptions(ctx context.Context, run *agenticv1alpha1.AgenticRun, approval *agenticv1alpha1.AgenticRunApproval, policy *agenticv1alpha1.ApprovalPolicy) (*agenticv1alpha1.RemediationOption, error) {
	result, err := r.getLatestAnalysisResult(ctx, run)
	if err != nil {
		return nil, err
	}
	if result == nil || len(result.Status.Options) == 0 {
		return nil, nil
	}

	if len(result.Status.Options) == 1 {
		return &result.Status.Options[0], nil
	}

	idx := int(*getStageOption(approval, policy))
	if idx < 0 || idx >= len(result.Status.Options) {
		return nil, fmt.Errorf("selected option index %d out of range (have %d options)", idx, len(result.Status.Options))
	}

	selected := result.Status.Options[idx]
	base := result.DeepCopy()
	result.Status.Options = []agenticv1alpha1.RemediationOption{selected}
	if err := r.Status().Patch(ctx, result, client.MergeFrom(base)); err != nil {
		return nil, fmt.Errorf("%s: %w", ErrTrimAnalysisResultOptions, err)
	}
	return &result.Status.Options[0], nil
}

func resetExecutionAndVerification(steps *agenticv1alpha1.StepsStatus) {
	steps.Execution.Sandbox = agenticv1alpha1.SandboxInfo{}
	steps.Verification.Sandbox = agenticv1alpha1.SandboxInfo{}
}

func maxAttempts(approval *agenticv1alpha1.AgenticRunApproval, policy *agenticv1alpha1.ApprovalPolicy) int {
	ceiling := 1
	if policy != nil && policy.Spec.MaxAttempts > 0 {
		ceiling = int(policy.Spec.MaxAttempts)
	}
	if approval != nil {
		for _, s := range approval.Spec.Stages {
			if s.Type == agenticv1alpha1.ApprovalStageExecution && s.Execution != nil && s.Execution.MaxAttempts > 0 {
				v := int(s.Execution.MaxAttempts)
				if v > ceiling {
					return ceiling
				}
				return v
			}
		}
	}
	return ceiling
}

type escalationData struct {
	Name                string
	Namespace           string
	Request             string
	AnalysisResults     []agenticv1alpha1.StepResultRef
	ExecutionResults    []agenticv1alpha1.StepResultRef
	VerificationResults []agenticv1alpha1.StepResultRef
}

func buildEscalationRequest(run *agenticv1alpha1.AgenticRun) string {
	data := escalationData{
		Name:                run.Name,
		Namespace:           run.Namespace,
		Request:             run.Spec.Request,
		AnalysisResults:     run.Status.Steps.Analysis.Results,
		ExecutionResults:    run.Status.Steps.Execution.Results,
		VerificationResults: run.Status.Steps.Verification.Results,
	}
	return renderTemplate("escalation_request.tmpl", data)
}

func needsRevision(run *agenticv1alpha1.AgenticRun) bool {
	if run.Spec.RevisionFeedback == "" {
		return false
	}
	analyzed := meta.FindStatusCondition(run.Status.Conditions, agenticv1alpha1.AgenticRunConditionAnalyzed)
	if analyzed == nil {
		return false
	}
	return run.Generation > analyzed.ObservedGeneration
}

type revisionData struct {
	Generation     int64
	AgenticRunName string
	Namespace      string
	Feedback       string
}

func buildRevisionContext(run *agenticv1alpha1.AgenticRun) string {
	data := revisionData{
		Generation:     run.Generation,
		AgenticRunName: run.Name,
		Namespace:      run.Namespace,
		Feedback:       run.Spec.RevisionFeedback,
	}
	return renderTemplate("revision_context.tmpl", data)
}

func prettyJSON(v interface{}) string {
	if v == nil {
		return "{}"
	}
	rv := reflect.ValueOf(v)
	if rv.Kind() == reflect.Ptr && rv.IsNil() {
		return "{}"
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return "{}"
	}
	return string(b)
}

type analysisQuery struct {
	Request         string
	HasExecution    bool
	HasVerification bool
}

func buildAnalysisQuery(requestText string, run *agenticv1alpha1.AgenticRun) string {
	return renderTemplate("analysis_query.tmpl", analysisQuery{
		Request:         requestText,
		HasExecution:    !run.Spec.Execution.IsZero(),
		HasVerification: !run.Spec.Verification.IsZero(),
	})
}

type executionQuery struct {
	OptionJSON string
}

func buildExecutionQuery(option *agenticv1alpha1.RemediationOption) string {
	return renderTemplate("execution_query.tmpl", executionQuery{OptionJSON: prettyJSON(option)})
}

type verificationQuery struct {
	OptionJSON    string
	ExecutionJSON string
}

func buildVerificationQuery(option *agenticv1alpha1.RemediationOption, exec *ExecutionOutput) string {
	return renderTemplate("verification_query.tmpl", verificationQuery{
		OptionJSON:    prettyJSON(option),
		ExecutionJSON: prettyJSON(executionOutputToAgentResult(exec)),
	})
}
