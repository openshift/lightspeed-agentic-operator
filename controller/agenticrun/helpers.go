package agenticrun

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	stderrors "errors"
	"fmt"
	"net"
	"reflect"
	"strings"
	"text/template"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
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

// maxRenderedTemplateSize allows up to 32,768 Unicode characters encoded as UTF-8 (worst case: 4 bytes each).
const maxRenderedTemplateSize = 32768 * 4 // 131 KiB byte bound on rendered output

type limitedWriter struct {
	buf   bytes.Buffer
	limit int64
}

func (w *limitedWriter) Write(p []byte) (int, error) {
	if int64(w.buf.Len())+int64(len(p)) > int64(w.limit) {
		return 0, fmt.Errorf("rendered template exceeds %d bytes", w.limit)
	}
	return w.buf.Write(p)
}

func (w *limitedWriter) String() string {
	return w.buf.String()
}

func cleanupContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
}

func preserveFailedSandbox(run *agenticv1alpha1.AgenticRun) bool {
	return run != nil && strings.EqualFold(strings.TrimSpace(run.Annotations[preserveSandboxAnnotation]), "true")
}

func renderTemplate(tmpl string, data any) (string, error) {
	t, err := template.New("custom").Parse(tmpl)
	if err != nil {
		return "", fmt.Errorf("template parse: %w", err)
	}
	t = t.Funcs(template.FuncMap{}) // Restrict to default safe functions only
	w := &limitedWriter{limit: maxRenderedTemplateSize}
	if err := t.Execute(w, data); err != nil {
		return "", fmt.Errorf("template exec: %w", err)
	}
	return w.String(), nil
}

// readBuiltinTemplate reads a built-in template file from the embedded FS.
func readBuiltinTemplate(name string) (string, error) {
	content, err := templateFS.ReadFile(name)
	if err != nil {
		return "", fmt.Errorf("read built-in template %s: %w", name, err)
	}
	return string(content), nil
}

const (
	preserveSandboxAnnotation = "agentic.openshift.io/preserve-sandbox"
	cleanupTimeout            = 30 * time.Second
	// SuspensionStopWindow is the fixed cleanup window for a stop activation.
	SuspensionStopWindow = 10 * time.Second

	ErrGetAnalysisResult         = "get AnalysisResult"
	ErrTrimAnalysisResultOptions = "trim AnalysisResult options"
	// ErrStopAccepted signals that the requested run must not receive workflow work.
	ErrStopAccepted = "run stop accepted"

	rbacCleanupFinalizer    = "agentic.openshift.io/execution-rbac-cleanup"
	templogCleanupFinalizer = "agentic.openshift.io/templog-cleanup"

	templogCleanupAttemptsAnnotation = "agentic.openshift.io/templog-cleanup-attempts"
	templogMaxCleanupAttempts        = 3
	templogCleanupRequeueAfter       = 5 * time.Second

	reasonInProgress          = "InProgress"
	reasonComplete            = "Complete"
	reasonFailed              = "Failed"
	reasonSkipped             = "Skipped"
	reasonPassed              = "Passed"
	reasonWorkflowFailed      = "WorkflowResolutionFailed"
	reasonUserDenied          = "UserDenied"
	defaultSandboxSA          = "lightspeed-agent"
	reasonRevising            = "Revising"
	reasonRevisionComplete    = "RevisionComplete"
	reasonSystemSuspended     = "SystemSuspended"
	reasonTeardownComplete    = "TeardownComplete"
	reasonTeardownFailed      = "TeardownFailed"
	reasonEmergencyStopFailed = "EmergencyStopFailed"
	reasonNoActionRequired    = agenticv1alpha1.ReasonNoActionRequired

	LogKeyName      = "name"
	LogKeyStep      = "step"
	LogKeyPhase     = "phase"
	LogKeyClaim     = "claimName"
	LogKeyCondition = "condition"

	maxCreateRetries = 3
)

var errStopAccepted = stderrors.New(ErrStopAccepted)

// retryBaseDelay is the base delay for exponential backoff between transient
// retries. Package-level var so tests can override it.
var retryBaseDelay = 5 * time.Second

// isTransient returns true for Kubernetes API errors that are likely to
// succeed on retry (server timeouts, throttling, temporary unavailability).
func isTransient(err error) bool {
	if err == nil {
		return false
	}
	if errors.IsServerTimeout(err) || errors.IsTimeout(err) ||
		errors.IsTooManyRequests(err) || errors.IsServiceUnavailable(err) ||
		errors.IsInternalError(err) || errors.IsConflict(err) {
		return true
	}
	var netErr net.Error
	return stderrors.As(err, &netErr)
}

// retryBackoff returns the delay before the given attempt (1-indexed).
// Uses exponential backoff: 5s, 10s, 20s, …
func retryBackoff(attempt int) time.Duration {
	return retryBaseDelay * (1 << (attempt - 1))
}

// retryOnTransient calls fn up to maxCreateRetries times, retrying only
// when the error is transient. Permanent errors and context cancellation
// are returned immediately.
func retryOnTransient(ctx context.Context, fn func() error) error {
	log := logf.FromContext(ctx)
	var lastErr error
	for attempt := 1; attempt <= maxCreateRetries; attempt++ {
		if err := fn(); err == nil {
			return nil
		} else if !isTransient(err) {
			return err
		} else {
			lastErr = err
		}
		log.Info("transient failure, retrying", "attempt", attempt, "maxRetries", maxCreateRetries, "error", lastErr)
		if attempt < maxCreateRetries {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(retryBackoff(attempt)):
			}
		}
	}
	return lastErr
}

func hasAcceptedStop(run *agenticv1alpha1.AgenticRun) bool {
	stopping := meta.FindStatusCondition(run.Status.Conditions, agenticv1alpha1.AgenticRunConditionStopping)
	if stopping != nil {
		if stopping.Status == metav1.ConditionTrue && stopping.Reason == reasonSystemSuspended {
			return true
		}
		if stopping.Status == metav1.ConditionFalse &&
			(stopping.Reason == reasonTeardownComplete || stopping.Reason == reasonTeardownFailed) {
			return true
		}
	}

	emergency := meta.FindStatusCondition(run.Status.Conditions, agenticv1alpha1.AgenticRunConditionEmergencyStopped)
	if emergency != nil && emergency.Status == metav1.ConditionTrue {
		return true
	}
	for i := range run.Status.Conditions {
		condition := &run.Status.Conditions[i]
		if condition.Status == metav1.ConditionFalse && condition.Reason == reasonEmergencyStopFailed {
			return true
		}
	}
	return false
}

func readRunnableRun(ctx context.Context, reader client.Reader, key types.NamespacedName, uid types.UID) (*agenticv1alpha1.AgenticRun, error) {
	if reader == nil {
		return nil, fmt.Errorf("uncached API reader is required before run workflow work")
	}
	var current agenticv1alpha1.AgenticRun
	if err := reader.Get(ctx, key, &current); err != nil {
		return nil, err
	}
	if current.UID != uid || hasAcceptedStop(&current) {
		return nil, errStopAccepted
	}

	var config agenticv1alpha1.AgenticOLSConfig
	err := reader.Get(ctx, types.NamespacedName{Name: "cluster"}, &config)
	if err != nil && !errors.IsNotFound(err) {
		return nil, err
	}
	if err == nil && config.Spec.Suspended {
		return nil, errStopAccepted
	}
	return &current, nil
}

// failStep marks a step as failed and creates a failure result CR.
// The caller must have set the step condition to ConditionUnknown before
// calling failStep so that conditionTime can extract the start time.
func (r *AgenticRunReconciler) failStep(ctx context.Context, run *agenticv1alpha1.AgenticRun, conditionType string, err error) (ctrl.Result, error) {
	if r.APIReader == nil {
		return ctrl.Result{}, stderrors.New("uncached API reader is required before failing a run step")
	}
	current, guardErr := readRunnableRun(ctx, r.APIReader, client.ObjectKeyFromObject(run), run.UID)
	if guardErr == errStopAccepted {
		return ctrl.Result{Requeue: true}, nil
	}
	if guardErr != nil {
		return ctrl.Result{}, guardErr
	}
	run = current

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

func isStopAcceptedError(err error) bool {
	return stderrors.Is(err, errStopAccepted)
}

func (r *AgenticRunReconciler) statusPatch(ctx context.Context, run *agenticv1alpha1.AgenticRun, base *agenticv1alpha1.AgenticRun) error {
	return r.Status().Patch(ctx, run, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
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
	case agenticv1alpha1.AgenticRunPhaseCompleted, agenticv1alpha1.AgenticRunPhaseFailed, agenticv1alpha1.AgenticRunPhaseDenied, agenticv1alpha1.AgenticRunPhaseEscalated, agenticv1alpha1.AgenticRunPhaseEmergencyStopped:
		return true
	}
	return false
}

func isNoActionRequired(run *agenticv1alpha1.AgenticRun) bool {
	c := meta.FindStatusCondition(run.Status.Conditions, agenticv1alpha1.AgenticRunConditionAnalyzed)
	return c != nil && c.Status == metav1.ConditionTrue && c.Reason == reasonNoActionRequired
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
	if err := r.Get(ctx, types.NamespacedName{Name: latestRef.Name, Namespace: r.Namespace}, &result); err != nil {
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
func (r *AgenticRunReconciler) trimNonSelectedOptions(ctx context.Context, run *agenticv1alpha1.AgenticRun, approval *agenticv1alpha1.AgenticRunApproval) (*agenticv1alpha1.RemediationOption, error) {
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

	idx := int(*getStageOption(approval))
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

type escalationData struct {
	Name                string
	Namespace           string
	ResultNamespace     string
	Request             string
	AnalysisResults     []agenticv1alpha1.StepResultRef
	ExecutionResults    []agenticv1alpha1.StepResultRef
	VerificationResults []agenticv1alpha1.StepResultRef
}

func buildEscalationRequest(run *agenticv1alpha1.AgenticRun, resultNamespace string) (string, error) {
	tmpl, err := readBuiltinTemplate("templates/escalation_request.tmpl")
	if err != nil {
		return "", err
	}
	data := escalationData{
		Name:                run.Name,
		Namespace:           run.Namespace,
		ResultNamespace:     resultNamespace,
		Request:             run.Spec.Request,
		AnalysisResults:     run.Status.Steps.Analysis.Results,
		ExecutionResults:    run.Status.Steps.Execution.Results,
		VerificationResults: run.Status.Steps.Verification.Results,
	}
	return renderTemplate(tmpl, data)
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

func buildRevisionContext(run *agenticv1alpha1.AgenticRun) (string, error) {
	tmpl, err := readBuiltinTemplate("templates/revision_context.tmpl")
	if err != nil {
		return "", err
	}
	data := revisionData{
		Generation:     run.Generation,
		AgenticRunName: run.Name,
		Namespace:      run.Namespace,
		Feedback:       run.Spec.RevisionFeedback,
	}
	return renderTemplate(tmpl, data)
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

func buildAnalysisQuery(requestText string, run *agenticv1alpha1.AgenticRun) (string, error) {
	tmpl, err := readBuiltinTemplate("templates/analysis_query.tmpl")
	if err != nil {
		return "", err
	}
	return renderTemplate(tmpl, analysisQuery{
		Request:         requestText,
		HasExecution:    !run.Spec.Execution.IsZero(),
		HasVerification: !run.Spec.Verification.IsZero(),
	})
}

type executionQuery struct {
	OptionJSON string
}

type verificationQuery struct {
	OptionJSON    string
	ExecutionJSON string
}
