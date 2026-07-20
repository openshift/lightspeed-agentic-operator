package agenticrun

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
	"go.uber.org/zap"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
)

const (
	tracerName    = "github.com/openshift/lightspeed-agentic-operator/controller/agenticrun"
	tracerVersion = "v1alpha1"
)

// AgenticRunIDGenerator is an OTEL IDGenerator. With the per-phase trace model
// it uses the SDK default random IDs (no context override). Kept so the
// telemetry Provider can supply a stable generator implementation.
type AgenticRunIDGenerator struct{}

var _ sdktrace.IDGenerator = (*AgenticRunIDGenerator)(nil)

func (*AgenticRunIDGenerator) NewIDs(_ context.Context) (trace.TraceID, trace.SpanID) {
	var tid trace.TraceID
	var sid trace.SpanID
	_, _ = rand.Read(tid[:])
	_, _ = rand.Read(sid[:])
	return tid, sid
}

func (*AgenticRunIDGenerator) NewSpanID(_ context.Context, _ trace.TraceID) trace.SpanID {
	var sid trace.SpanID
	_, _ = rand.Read(sid[:])
	return sid
}

// AuditLogger emits compliance audit data as OTel spans, span events, and
// structured logs (stdout + optional OTLP via LogEmitter).
// Each phase of an AgenticRun gets its own independent trace (fresh trace ID).
// Phase spans link back to the prior phase's root span via span links.
type AuditLogger interface {
	// Phase spans — each creates a new root trace with auto-generated trace ID.
	StartAnalysisSpan(ctx context.Context, run *agenticv1alpha1.AgenticRun) (context.Context, trace.Span)
	StartExecutionSpan(ctx context.Context, run *agenticv1alpha1.AgenticRun) (context.Context, trace.Span)
	StartVerificationSpan(ctx context.Context, run *agenticv1alpha1.AgenticRun) (context.Context, trace.Span)
	StartEscalationSpan(ctx context.Context, run *agenticv1alpha1.AgenticRun) (context.Context, trace.Span)

	// Short-lived phase spans (created and ended immediately).
	EmitApprovalSpan(ctx context.Context, run *agenticv1alpha1.AgenticRun, approval *agenticv1alpha1.AgenticRunApproval, selectedOptionTitle string)
	EmitTerminalSpan(ctx context.Context, run *agenticv1alpha1.AgenticRun, phase, reason string)

	// Span events — emitted on the current span from ctx.
	EmitAgenticRunReceived(ctx context.Context, run *agenticv1alpha1.AgenticRun)
	EmitAnalysisCompleted(ctx context.Context, run *agenticv1alpha1.AgenticRun, result *agenticv1alpha1.AnalysisResult)
	EmitExecutionCompleted(ctx context.Context, run *agenticv1alpha1.AgenticRun, result *agenticv1alpha1.ExecutionResult)
	EmitVerificationCompleted(ctx context.Context, run *agenticv1alpha1.AgenticRun, result *agenticv1alpha1.VerificationResult)
	EmitVerificationRetry(ctx context.Context, run *agenticv1alpha1.AgenticRun, result *agenticv1alpha1.VerificationResult, retryCount int)
	EmitEscalationCompleted(ctx context.Context, run *agenticv1alpha1.AgenticRun, result *agenticv1alpha1.EscalationResult)

	// InjectTraceContext injects W3C traceparent header for downstream propagation.
	InjectTraceContext(ctx context.Context, run *agenticv1alpha1.AgenticRun, headers http.Header)

	// Cleanup removes in-memory state for a completed run (except terminal guard).
	Cleanup(run *agenticv1alpha1.AgenticRun)

	// CleanupDeleted removes all in-memory state including the terminal guard.
	// Called when the AgenticRun is deleted (NotFound on Get).
	CleanupDeleted(key types.NamespacedName)
}

// LogEmitter is the interface for emitting OTLP log records.
// Implemented by pkg/telemetry.Provider.
type LogEmitter interface {
	EmitLog(ctx context.Context, traceID trace.TraceID, event string, payload interface{})
}

// NoOpLogEmitter is a no-op LogEmitter for use in tests.
type NoOpLogEmitter struct{}

func (NoOpLogEmitter) EmitLog(_ context.Context, _ trace.TraceID, _ string, _ interface{}) {}

// ProductionAuditLogger implements AuditLogger with per-phase OTel traces,
// zap stdout logs, and optional OTLP log emission.
// Known limitation: priorPhase is in-memory only. On operator restart the span
// link chain between phases is broken — the first post-restart phase span has
// no link to the prior phase. The agenticrun.uid correlation attribute (from
// metadata.uid) still connects all phases across the restart boundary.
type ProductionAuditLogger struct {
	logger          *zap.Logger
	tracer          trace.Tracer
	logEmitter      LogEmitter
	priorPhase      sync.Map // map[types.UID]trace.SpanContext
	emittedTerminal sync.Map // map[types.UID]bool — prevents duplicate terminal spans
	emittedApproval sync.Map // map[types.UID]bool — prevents duplicate approval spans on retry
	knownUIDs       sync.Map // map[string]types.UID — "namespace/name" → UID for cleanup after deletion
}

// NoOpAuditLogger implements AuditLogger with no-op behavior (audit disabled).
type NoOpAuditLogger struct{}

// NewProductionAuditLogger creates an audit logger that emits OTel spans,
// JSON stdout logs, and OTLP log records (when logEmitter is non-nil).
func NewProductionAuditLogger(logger *zap.Logger, logEmitter LogEmitter) AuditLogger {
	if logEmitter == nil {
		logEmitter = NoOpLogEmitter{}
	}
	return &ProductionAuditLogger{
		logger:     logger,
		tracer:     otel.Tracer(tracerName, trace.WithInstrumentationVersion(tracerVersion)),
		logEmitter: logEmitter,
	}
}

// NewNoOpAuditLogger creates a no-op audit logger (audit disabled).
func NewNoOpAuditLogger() AuditLogger {
	return &NoOpAuditLogger{}
}

// serializeCR builds an audit-safe representation of a CR.
func serializeCR(obj client.Object) (map[string]interface{}, error) {
	metadata := map[string]interface{}{
		"name":      obj.GetName(),
		"namespace": obj.GetNamespace(),
		"uid":       string(obj.GetUID()),
	}
	if ts := obj.GetCreationTimestamp(); !ts.IsZero() {
		metadata["creationTimestamp"] = ts.Format(time.RFC3339)
	}

	data, err := json.Marshal(obj)
	if err != nil {
		return nil, err
	}
	var full map[string]interface{}
	if err := json.Unmarshal(data, &full); err != nil {
		return nil, err
	}

	result := map[string]interface{}{
		"metadata": metadata,
	}
	if spec, ok := full["spec"]; ok {
		result["spec"] = spec
	}
	if status, ok := full["status"]; ok {
		result["status"] = status
	}
	return result, nil
}

// serializeCRJSON returns a JSON string of the audit-safe CR representation.
func serializeCRJSON(obj client.Object) string {
	safe, err := serializeCR(obj)
	if err != nil {
		return "{}"
	}
	data, err := json.Marshal(safe)
	if err != nil {
		return "{}"
	}
	return string(data)
}

// emitStructuredLog writes a JSON audit event to stdout and emits an OTLP log record.
// Stdout is always emitted. OTLP emission depends on Provider configuration.
func (l *ProductionAuditLogger) emitStructuredLog(ctx context.Context, event string, payload interface{}) {
	sc := trace.SpanContextFromContext(ctx)
	var tid trace.TraceID
	if sc.IsValid() {
		tid = sc.TraceID()
	}

	l.logger.Info(event,
		zap.String("event", event),
		zap.String("trace_id", tid.String()),
		zap.Any("payload", payload),
	)
	l.logEmitter.EmitLog(ctx, tid, event, payload)
}

// runAttrs returns the standard span attributes for an AgenticRun.
func runAttrs(run *agenticv1alpha1.AgenticRun) []attribute.KeyValue {
	return []attribute.KeyValue{
		attribute.String("agenticrun.uid", strings.ReplaceAll(string(run.UID), "-", "")),
		attribute.String("agenticrun.name", run.Name),
		attribute.String("agenticrun.namespace", run.Namespace),
	}
}

// startPhaseSpan creates a new root span for a phase with span link to prior phase.
func (l *ProductionAuditLogger) startPhaseSpan(ctx context.Context, run *agenticv1alpha1.AgenticRun, spanName string, extraAttrs ...attribute.KeyValue) (context.Context, trace.Span) {
	attrs := runAttrs(run)
	attrs = append(attrs, extraAttrs...)

	opts := []trace.SpanStartOption{
		trace.WithNewRoot(),
		trace.WithAttributes(attrs...),
		trace.WithSpanKind(trace.SpanKindInternal),
	}

	if prior, ok := l.priorPhase.Load(run.UID); ok {
		if sc, ok := prior.(trace.SpanContext); ok && sc.IsValid() {
			opts = append(opts, trace.WithLinks(trace.Link{SpanContext: sc}))
		}
	}

	spanCtx, span := l.tracer.Start(ctx, spanName, opts...)
	l.priorPhase.Store(run.UID, span.SpanContext())
	l.knownUIDs.Store(run.Namespace+"/"+run.Name, run.UID)
	return spanCtx, span
}

func (l *ProductionAuditLogger) StartAnalysisSpan(ctx context.Context, run *agenticv1alpha1.AgenticRun) (context.Context, trace.Span) {
	return l.startPhaseSpan(ctx, run, "agenticrun.analyze")
}

func (l *ProductionAuditLogger) StartExecutionSpan(ctx context.Context, run *agenticv1alpha1.AgenticRun) (context.Context, trace.Span) {
	var extra []attribute.KeyValue
	if run.Status.Steps.Execution.RetryCount != nil && *run.Status.Steps.Execution.RetryCount > 0 {
		extra = append(extra, attribute.Int("retry_index", int(*run.Status.Steps.Execution.RetryCount)))
	}
	return l.startPhaseSpan(ctx, run, "agenticrun.execute", extra...)
}

func (l *ProductionAuditLogger) StartVerificationSpan(ctx context.Context, run *agenticv1alpha1.AgenticRun) (context.Context, trace.Span) {
	var extra []attribute.KeyValue
	if run.Status.Steps.Execution.RetryCount != nil && *run.Status.Steps.Execution.RetryCount > 0 {
		extra = append(extra, attribute.Int("retry_index", int(*run.Status.Steps.Execution.RetryCount)))
	}
	return l.startPhaseSpan(ctx, run, "agenticrun.verify", extra...)
}

func (l *ProductionAuditLogger) StartEscalationSpan(ctx context.Context, run *agenticv1alpha1.AgenticRun) (context.Context, trace.Span) {
	return l.startPhaseSpan(ctx, run, "agenticrun.escalate")
}

// EmitApprovalSpan creates a short-lived agenticrun.human_approval trace.
// Idempotent: only emits once per UID — retries do not re-emit.
func (l *ProductionAuditLogger) EmitApprovalSpan(ctx context.Context, run *agenticv1alpha1.AgenticRun, approval *agenticv1alpha1.AgenticRunApproval, selectedOptionTitle string) {
	if _, already := l.emittedApproval.LoadOrStore(run.UID, true); already {
		return
	}
	spanCtx, span := l.startPhaseSpan(ctx, run, "agenticrun.human_approval")

	eventAttrs := []attribute.KeyValue{
		attribute.String("agenticrun.name", run.Name),
	}
	payload := map[string]interface{}{}
	if approval != nil {
		for i := len(approval.Spec.Stages) - 1; i >= 0; i-- {
			if approval.Spec.Stages[i].Decision != "" {
				eventAttrs = append(eventAttrs, attribute.String("approval.decision", string(approval.Spec.Stages[i].Decision)))
				break
			}
		}
		for _, stage := range approval.Spec.Stages {
			if stage.Type == agenticv1alpha1.ApprovalStageExecution && stage.Execution.Option != nil {
				eventAttrs = append(eventAttrs, attribute.Int("selected_option", int(*stage.Execution.Option)))
				payload["selectedOption"] = *stage.Execution.Option
				break
			}
		}
		if approval.Spec.Approver.UID != "" {
			eventAttrs = append(eventAttrs,
				attribute.String("approver.uid", approval.Spec.Approver.UID),
				attribute.String("approver.username", approval.Spec.Approver.Username),
			)
			payload["approver"] = map[string]interface{}{
				"uid":        approval.Spec.Approver.UID,
				"username":   approval.Spec.Approver.Username,
				"approvedAt": approval.Spec.Approver.ApprovedAt,
			}
		}
		eventAttrs = append(eventAttrs, attribute.String("agenticrun.cr", serializeCRJSON(approval)))
		payload["approvalStages"] = approval.Spec.Stages
	}
	if selectedOptionTitle != "" {
		eventAttrs = append(eventAttrs, attribute.String("selected_option.title", selectedOptionTitle))
		payload["selectedOptionTitle"] = selectedOptionTitle
	}
	l.emitStructuredLog(spanCtx, "audit.approval.received", payload)
	span.AddEvent("agenticrun.approval.completed", trace.WithAttributes(eventAttrs...))
	span.End()
}

// EmitTerminalSpan creates a short-lived agenticrun.terminal trace.
// Idempotent: only emits once per UID — subsequent reconciles are no-ops.
func (l *ProductionAuditLogger) EmitTerminalSpan(ctx context.Context, run *agenticv1alpha1.AgenticRun, phase, reason string) {
	if _, already := l.emittedTerminal.LoadOrStore(run.UID, true); already {
		return
	}
	spanCtx, span := l.startPhaseSpan(ctx, run, "agenticrun.terminal",
		attribute.String("phase", phase),
		attribute.String("reason", reason),
	)
	l.emitStructuredLog(spanCtx, "audit.agenticrun.terminal", map[string]interface{}{
		"phase":  phase,
		"reason": reason,
	})
	span.AddEvent("agenticrun.terminal", trace.WithAttributes(
		attribute.String("agenticrun.name", run.Name),
		attribute.String("phase", phase),
		attribute.String("reason", reason),
	))
	span.End()
}

func (l *ProductionAuditLogger) EmitAgenticRunReceived(ctx context.Context, run *agenticv1alpha1.AgenticRun) {
	serialized, err := serializeCR(run)
	if err != nil {
		l.logger.Error("Failed to serialize AgenticRun for audit", zap.Error(err))
	} else {
		l.emitStructuredLog(ctx, "audit.agenticrun.received", map[string]interface{}{"run": serialized})
	}

	span := trace.SpanFromContext(ctx)
	if span == nil || !span.IsRecording() {
		return
	}
	span.AddEvent("agenticrun.received", trace.WithAttributes(
		attribute.String("agenticrun.name", run.Name),
		attribute.String("agenticrun.namespace", run.Namespace),
		attribute.String("agenticrun.uid", strings.ReplaceAll(string(run.UID), "-", "")),
		attribute.String("agenticrun.request", run.Spec.Request),
		attribute.String("agenticrun.cr", serializeCRJSON(run)),
	))
}

func (l *ProductionAuditLogger) EmitAnalysisCompleted(ctx context.Context, run *agenticv1alpha1.AgenticRun, result *agenticv1alpha1.AnalysisResult) {
	serialized, err := serializeCR(result)
	if err != nil {
		l.logger.Error("Failed to serialize AnalysisResult for audit", zap.Error(err))
	} else {
		l.emitStructuredLog(ctx, "audit.analysis.completed", map[string]interface{}{"analysisResult": serialized})
	}

	span := trace.SpanFromContext(ctx)
	if span == nil || !span.IsRecording() {
		return
	}
	attrs := []attribute.KeyValue{
		attribute.String("agenticrun.name", run.Name),
		attribute.String("result.name", result.Name),
		attribute.String("result.uid", string(result.UID)),
		attribute.Int("options.count", len(result.Status.Options)),
	}
	for i, opt := range result.Status.Options {
		if i >= 3 {
			break
		}
		prefix := fmt.Sprintf("option.%d.", i)
		attrs = append(attrs,
			attribute.String(prefix+"title", opt.Title),
			attribute.String(prefix+"risk", string(opt.RemediationPlan.Risk)),
		)
	}
	attrs = append(attrs, attribute.String("agenticrun.cr", serializeCRJSON(result)))
	span.AddEvent("agenticrun.analysis.completed", trace.WithAttributes(attrs...))
}

func (l *ProductionAuditLogger) EmitExecutionCompleted(ctx context.Context, run *agenticv1alpha1.AgenticRun, result *agenticv1alpha1.ExecutionResult) {
	serialized, err := serializeCR(result)
	if err != nil {
		l.logger.Error("Failed to serialize ExecutionResult for audit", zap.Error(err))
	} else {
		l.emitStructuredLog(ctx, "audit.execution.completed", map[string]interface{}{"executionResult": serialized})
	}

	span := trace.SpanFromContext(ctx)
	if span == nil || !span.IsRecording() {
		return
	}
	attrs := []attribute.KeyValue{
		attribute.String("agenticrun.name", run.Name),
		attribute.String("result.name", result.Name),
		attribute.String("result.uid", string(result.UID)),
		attribute.Int("actions_taken.count", len(result.Status.ActionsTaken)),
		attribute.String("failure_reason", result.Status.FailureReason),
	}
	for i, action := range result.Status.ActionsTaken {
		if i >= 5 {
			break
		}
		attrs = append(attrs,
			attribute.String(fmt.Sprintf("action.%d.type", i), action.Type),
			attribute.String(fmt.Sprintf("action.%d.description", i), action.Description),
		)
	}
	attrs = append(attrs, attribute.String("agenticrun.cr", serializeCRJSON(result)))
	span.AddEvent("agenticrun.execution.completed", trace.WithAttributes(attrs...))
}

func (l *ProductionAuditLogger) EmitVerificationCompleted(ctx context.Context, run *agenticv1alpha1.AgenticRun, result *agenticv1alpha1.VerificationResult) {
	serialized, err := serializeCR(result)
	if err != nil {
		l.logger.Error("Failed to serialize VerificationResult for audit", zap.Error(err))
	} else {
		l.emitStructuredLog(ctx, "audit.verification.completed", map[string]interface{}{"verificationResult": serialized})
	}

	span := trace.SpanFromContext(ctx)
	if span == nil || !span.IsRecording() {
		return
	}
	attrs := []attribute.KeyValue{
		attribute.String("agenticrun.name", run.Name),
		attribute.String("result.name", result.Name),
		attribute.String("result.uid", string(result.UID)),
		attribute.String("summary", result.Status.Summary),
		attribute.Int("checks.count", len(result.Status.Checks)),
	}
	for i, check := range result.Status.Checks {
		if i >= 5 {
			break
		}
		attrs = append(attrs,
			attribute.String(fmt.Sprintf("check.%d.name", i), check.Name),
			attribute.String(fmt.Sprintf("check.%d.result", i), string(check.Result)),
		)
	}
	attrs = append(attrs, attribute.String("agenticrun.cr", serializeCRJSON(result)))
	span.AddEvent("agenticrun.verification.completed", trace.WithAttributes(attrs...))
}

func (l *ProductionAuditLogger) EmitVerificationRetry(ctx context.Context, run *agenticv1alpha1.AgenticRun, result *agenticv1alpha1.VerificationResult, retryCount int) {
	serialized, err := serializeCR(result)
	if err != nil {
		l.logger.Error("Failed to serialize VerificationResult for audit retry", zap.Error(err))
	} else {
		l.emitStructuredLog(ctx, "audit.verification.retry", map[string]interface{}{
			"verificationResult": serialized,
			"retryCount":         retryCount,
		})
	}

	span := trace.SpanFromContext(ctx)
	if span == nil || !span.IsRecording() {
		return
	}
	span.AddEvent("agenticrun.verification.retry", trace.WithAttributes(
		attribute.String("agenticrun.name", run.Name),
		attribute.String("result.name", result.Name),
		attribute.String("summary", result.Status.Summary),
		attribute.Int("retry_count", retryCount),
		attribute.Int("checks.count", len(result.Status.Checks)),
		attribute.String("agenticrun.cr", serializeCRJSON(result)),
	))
}

func (l *ProductionAuditLogger) EmitEscalationCompleted(ctx context.Context, run *agenticv1alpha1.AgenticRun, result *agenticv1alpha1.EscalationResult) {
	serialized, err := serializeCR(result)
	if err != nil {
		l.logger.Error("Failed to serialize EscalationResult for audit", zap.Error(err))
	} else {
		l.emitStructuredLog(ctx, "audit.escalation.completed", map[string]interface{}{"escalationResult": serialized})
	}

	span := trace.SpanFromContext(ctx)
	if span == nil || !span.IsRecording() {
		return
	}
	span.AddEvent("agenticrun.escalation.completed", trace.WithAttributes(
		attribute.String("agenticrun.name", run.Name),
		attribute.String("result.name", result.Name),
		attribute.String("result.uid", string(result.UID)),
		attribute.String("summary", result.Status.Summary),
		attribute.String("agenticrun.cr", serializeCRJSON(result)),
	))
}

func (l *ProductionAuditLogger) InjectTraceContext(ctx context.Context, _ *agenticv1alpha1.AgenticRun, headers http.Header) {
	sc := trace.SpanContextFromContext(ctx)
	if !sc.IsValid() {
		return
	}
	propagator := propagation.TraceContext{}
	propagator.Inject(ctx, propagation.HeaderCarrier(headers))
}

func (l *ProductionAuditLogger) Cleanup(run *agenticv1alpha1.AgenticRun) {
	l.priorPhase.Delete(run.UID)
	l.emittedApproval.Delete(run.UID)
}

func (l *ProductionAuditLogger) CleanupDeleted(key types.NamespacedName) {
	if uid, ok := l.knownUIDs.LoadAndDelete(key.String()); ok {
		l.emittedTerminal.Delete(uid)
		l.priorPhase.Delete(uid)
		l.emittedApproval.Delete(uid)
	}
}

// --- NoOp implementations ---

var noopTracer = noop.NewTracerProvider().Tracer("noop")

func (l *NoOpAuditLogger) StartAnalysisSpan(ctx context.Context, _ *agenticv1alpha1.AgenticRun) (context.Context, trace.Span) {
	return noopTracer.Start(ctx, "agenticrun.analyze")
}
func (l *NoOpAuditLogger) StartExecutionSpan(ctx context.Context, _ *agenticv1alpha1.AgenticRun) (context.Context, trace.Span) {
	return noopTracer.Start(ctx, "agenticrun.execute")
}
func (l *NoOpAuditLogger) StartVerificationSpan(ctx context.Context, _ *agenticv1alpha1.AgenticRun) (context.Context, trace.Span) {
	return noopTracer.Start(ctx, "agenticrun.verify")
}
func (l *NoOpAuditLogger) StartEscalationSpan(ctx context.Context, _ *agenticv1alpha1.AgenticRun) (context.Context, trace.Span) {
	return noopTracer.Start(ctx, "agenticrun.escalate")
}
func (l *NoOpAuditLogger) EmitApprovalSpan(_ context.Context, _ *agenticv1alpha1.AgenticRun, _ *agenticv1alpha1.AgenticRunApproval, _ string) {
}
func (l *NoOpAuditLogger) EmitTerminalSpan(_ context.Context, _ *agenticv1alpha1.AgenticRun, _, _ string) {
}
func (l *NoOpAuditLogger) EmitAgenticRunReceived(_ context.Context, _ *agenticv1alpha1.AgenticRun) {
}
func (l *NoOpAuditLogger) EmitAnalysisCompleted(_ context.Context, _ *agenticv1alpha1.AgenticRun, _ *agenticv1alpha1.AnalysisResult) {
}
func (l *NoOpAuditLogger) EmitExecutionCompleted(_ context.Context, _ *agenticv1alpha1.AgenticRun, _ *agenticv1alpha1.ExecutionResult) {
}
func (l *NoOpAuditLogger) EmitVerificationCompleted(_ context.Context, _ *agenticv1alpha1.AgenticRun, _ *agenticv1alpha1.VerificationResult) {
}
func (l *NoOpAuditLogger) EmitVerificationRetry(_ context.Context, _ *agenticv1alpha1.AgenticRun, _ *agenticv1alpha1.VerificationResult, _ int) {
}
func (l *NoOpAuditLogger) EmitEscalationCompleted(_ context.Context, _ *agenticv1alpha1.AgenticRun, _ *agenticv1alpha1.EscalationResult) {
}
func (l *NoOpAuditLogger) InjectTraceContext(_ context.Context, _ *agenticv1alpha1.AgenticRun, _ http.Header) {
}
func (l *NoOpAuditLogger) Cleanup(_ *agenticv1alpha1.AgenticRun) {}
func (l *NoOpAuditLogger) CleanupDeleted(_ types.NamespacedName) {}
