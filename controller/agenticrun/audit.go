package agenticrun

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
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
	// Short-lived phase spans (created and ended immediately).
	EmitApprovalSpan(ctx context.Context, run *agenticv1alpha1.AgenticRun, approval *agenticv1alpha1.AgenticRunApproval, selectedOptionTitle string)
	EmitTerminalSpan(ctx context.Context, run *agenticv1alpha1.AgenticRun, phase, reason string)

	// Span events — emitted on the current span from ctx.
	EmitAgenticRunReceived(ctx context.Context, run *agenticv1alpha1.AgenticRun)

	// BeginStep opens a phase span for a sandbox step and stores it in-memory
	// so CompleteStep can close it when the pod terminates (async model).
	BeginStep(ctx context.Context, run *agenticv1alpha1.AgenticRun, step string) context.Context
	// CompleteStep ends the phase span opened by BeginStep. If result is
	// non-nil, rich audit attributes are emitted as a span event before
	// closing. No-op if no span was stored (e.g. operator restarted).
	CompleteStep(run *agenticv1alpha1.AgenticRun, step string, result client.Object)

	// Cleanup removes in-memory state for a completed run (except terminal guard).
	Cleanup(run *agenticv1alpha1.AgenticRun)

	// CleanupDeleted removes all in-memory state including the terminal guard.
	// Called when the AgenticRun is deleted (NotFound on Get).
	CleanupDeleted(key types.NamespacedName)
}

// LogEmitter is the interface for emitting OTLP log records.
// Implemented by pkg/configuration.Provider.
type LogEmitter interface {
	EmitLog(ctx context.Context, traceID trace.TraceID, runUID, phase, event string, payload interface{})
}

// NoOpLogEmitter is a no-op LogEmitter for use in tests.
type NoOpLogEmitter struct{}

func (NoOpLogEmitter) EmitLog(_ context.Context, _ trace.TraceID, _, _, _ string, _ interface{}) {}

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
	activeSpans     sync.Map // map[string]trace.Span — "{UID}/{step}" → open phase span (async model)
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
func (l *ProductionAuditLogger) emitStructuredLog(ctx context.Context, runUID, phase, event string, payload interface{}) {
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
	l.logEmitter.EmitLog(ctx, tid, runUID, phase, event, payload)
}

// runAttrs returns the standard span attributes for an AgenticRun.
func runAttrs(run *agenticv1alpha1.AgenticRun) []attribute.KeyValue {
	return []attribute.KeyValue{
		attribute.String("agenticrun.uid", string(run.UID)),
		attribute.String("agenticrun.name", run.Name),
		attribute.String("agenticrun.namespace", run.Namespace),
	}
}

// phaseForSpanName returns the canonical AgenticRun phase for an audit span.
func phaseForSpanName(spanName string) string {
	switch spanName {
	case "agenticrun.analyze":
		return "analysis"
	case "agenticrun.human_approval":
		return "approval"
	case "agenticrun.execute":
		return "execution"
	case "agenticrun.verify":
		return "verification"
	case "agenticrun.escalate":
		return "escalation"
	case "agenticrun.terminal":
		return "terminal"
	default:
		return ""
	}
}

// startPhaseSpan creates a new root span for a phase with span link to prior phase.
func (l *ProductionAuditLogger) startPhaseSpan(ctx context.Context, run *agenticv1alpha1.AgenticRun, spanName string, extraAttrs ...attribute.KeyValue) (context.Context, trace.Span) {
	attrs := runAttrs(run)
	if phase := phaseForSpanName(spanName); phase != "" {
		attrs = append(attrs, attribute.String("agenticrun.phase", phase))
	}
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

func (l *ProductionAuditLogger) startAnalysisSpan(ctx context.Context, run *agenticv1alpha1.AgenticRun) (context.Context, trace.Span) {
	return l.startPhaseSpan(ctx, run, "agenticrun.analyze")
}

func (l *ProductionAuditLogger) startExecutionSpan(ctx context.Context, run *agenticv1alpha1.AgenticRun) (context.Context, trace.Span) {
	return l.startPhaseSpan(ctx, run, "agenticrun.execute")
}

func (l *ProductionAuditLogger) startVerificationSpan(ctx context.Context, run *agenticv1alpha1.AgenticRun) (context.Context, trace.Span) {
	return l.startPhaseSpan(ctx, run, "agenticrun.verify")
}

func (l *ProductionAuditLogger) startEscalationSpan(ctx context.Context, run *agenticv1alpha1.AgenticRun) (context.Context, trace.Span) {
	return l.startPhaseSpan(ctx, run, "agenticrun.escalate")
}

func activeSpanKey(uid types.UID, step string) string {
	return string(uid) + "/" + step
}

func (l *ProductionAuditLogger) BeginStep(ctx context.Context, run *agenticv1alpha1.AgenticRun, step string) context.Context {
	var spanCtx context.Context
	var span trace.Span
	switch step {
	case "analysis":
		spanCtx, span = l.startAnalysisSpan(ctx, run)
	case "execution":
		spanCtx, span = l.startExecutionSpan(ctx, run)
	case "verification":
		spanCtx, span = l.startVerificationSpan(ctx, run)
	case "escalation":
		spanCtx, span = l.startEscalationSpan(ctx, run)
	default:
		return ctx
	}
	l.activeSpans.Store(activeSpanKey(run.UID, step), span)
	return spanCtx
}

func (l *ProductionAuditLogger) CompleteStep(run *agenticv1alpha1.AgenticRun, step string, result client.Object) {
	key := activeSpanKey(run.UID, step)
	v, ok := l.activeSpans.LoadAndDelete(key)
	if !ok {
		return
	}
	span, _ := v.(trace.Span)
	if span == nil {
		return
	}
	if result != nil {
		l.emitResultEvent(span, run, step, result)
	}
	span.End()
}

func (l *ProductionAuditLogger) emitResultEvent(span trace.Span, run *agenticv1alpha1.AgenticRun, step string, result client.Object) {
	attrs := []attribute.KeyValue{
		attribute.String("agenticrun.name", run.Name),
		attribute.String("result.name", result.GetName()),
		attribute.String("result.uid", string(result.GetUID())),
	}

	switch r := result.(type) {
	case *agenticv1alpha1.AnalysisResult:
		attrs = append(attrs, attribute.Int("options.count", len(r.Status.Options)))
		for i, opt := range r.Status.Options {
			if i >= 3 {
				break
			}
			attrs = append(attrs, attribute.String(fmt.Sprintf("option.%d.title", i), opt.Title))
		}
	case *agenticv1alpha1.ExecutionResult:
		attrs = append(attrs,
			attribute.Int("actions_taken.count", len(r.Status.ActionsTaken)),
			attribute.String("failure_reason", r.Status.FailureReason),
		)
		for i, action := range r.Status.ActionsTaken {
			if i >= 5 {
				break
			}
			attrs = append(attrs,
				attribute.String(fmt.Sprintf("action.%d.type", i), action.Type),
				attribute.String(fmt.Sprintf("action.%d.description", i), action.Description),
			)
		}
	case *agenticv1alpha1.VerificationResult:
		attrs = append(attrs,
			attribute.String("summary", r.Status.Summary),
			attribute.Int("checks.count", len(r.Status.Checks)),
		)
		for i, check := range r.Status.Checks {
			if i >= 5 {
				break
			}
			attrs = append(attrs,
				attribute.String(fmt.Sprintf("check.%d.name", i), check.Name),
				attribute.String(fmt.Sprintf("check.%d.result", i), string(check.Result)),
			)
		}
	case *agenticv1alpha1.EscalationResult:
		attrs = append(attrs, attribute.String("summary", r.Status.Summary))
	}

	serialized, err := serializeCR(result)
	if err == nil {
		l.emitStructuredLog(trace.ContextWithSpan(context.Background(), span),
			string(run.UID), step, fmt.Sprintf("audit.%s.completed", step),
			map[string]interface{}{"result": serialized})
	}

	attrs = append(attrs, attribute.String("agenticrun.cr", serializeCRJSON(result)))
	span.AddEvent(fmt.Sprintf("agenticrun.%s.completed", step), trace.WithAttributes(attrs...))
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
	l.emitStructuredLog(spanCtx, string(run.UID), "controller", "audit.approval.received", payload)
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
	l.emitStructuredLog(spanCtx, string(run.UID), "controller", "audit.agenticrun.terminal", map[string]interface{}{
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
		l.emitStructuredLog(ctx, string(run.UID), "controller", "audit.agenticrun.received", map[string]interface{}{"run": serialized})
	}

	span := trace.SpanFromContext(ctx)
	if span == nil || !span.IsRecording() {
		return
	}
	span.AddEvent("agenticrun.received", trace.WithAttributes(
		attribute.String("agenticrun.name", run.Name),
		attribute.String("agenticrun.namespace", run.Namespace),
		attribute.String("agenticrun.uid", string(run.UID)),
		attribute.String("agenticrun.request", run.Spec.Request),
		attribute.String("agenticrun.cr", serializeCRJSON(run)),
	))
}

var auditSteps = []string{"analysis", "execution", "verification", "escalation"}

func (l *ProductionAuditLogger) endActiveSpans(uid types.UID) {
	for _, step := range auditSteps {
		if val, ok := l.activeSpans.LoadAndDelete(activeSpanKey(uid, step)); ok {
			if span, ok := val.(trace.Span); ok {
				span.End()
			}
		}
	}
}

func (l *ProductionAuditLogger) Cleanup(run *agenticv1alpha1.AgenticRun) {
	l.priorPhase.Delete(run.UID)
	l.emittedApproval.Delete(run.UID)
	l.endActiveSpans(run.UID)
}

func (l *ProductionAuditLogger) CleanupDeleted(key types.NamespacedName) {
	if uid, ok := l.knownUIDs.LoadAndDelete(key.String()); ok {
		typedUID, ok := uid.(types.UID)
		if !ok {
			return
		}
		l.emittedTerminal.Delete(typedUID)
		l.priorPhase.Delete(typedUID)
		l.emittedApproval.Delete(typedUID)
		l.endActiveSpans(typedUID)
	}
}

// --- NoOp implementations ---

func (l *NoOpAuditLogger) EmitApprovalSpan(_ context.Context, _ *agenticv1alpha1.AgenticRun, _ *agenticv1alpha1.AgenticRunApproval, _ string) {
}
func (l *NoOpAuditLogger) EmitTerminalSpan(_ context.Context, _ *agenticv1alpha1.AgenticRun, _, _ string) {
}
func (l *NoOpAuditLogger) EmitAgenticRunReceived(_ context.Context, _ *agenticv1alpha1.AgenticRun) {
}
func (l *NoOpAuditLogger) BeginStep(ctx context.Context, _ *agenticv1alpha1.AgenticRun, _ string) context.Context {
	return ctx
}
func (l *NoOpAuditLogger) CompleteStep(_ *agenticv1alpha1.AgenticRun, _ string, _ client.Object) {}
func (l *NoOpAuditLogger) Cleanup(_ *agenticv1alpha1.AgenticRun)                                 {}
func (l *NoOpAuditLogger) CleanupDeleted(_ types.NamespacedName)                                 {}
