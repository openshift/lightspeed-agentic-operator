package proposal

import (
	"context"
	"crypto/rand"
	"encoding/hex"
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
	"go.uber.org/zap"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
)

type contextKeyProposalTraceID struct{}
type contextKeyProposalSpanID struct{}

// ContextWithProposalTraceID stores a desired trace ID in the context.
// The ProposalIDGenerator extracts it when creating root spans.
func ContextWithProposalTraceID(ctx context.Context, traceID trace.TraceID) context.Context {
	return context.WithValue(ctx, contextKeyProposalTraceID{}, traceID)
}

// ContextWithProposalSpanID stores a desired span ID in the context.
// Used by EnsureLifecycleSpan to create a deterministic lifecycle span ID
// that can be reconstructed after operator restart.
func ContextWithProposalSpanID(ctx context.Context, spanID trace.SpanID) context.Context {
	return context.WithValue(ctx, contextKeyProposalSpanID{}, spanID)
}

// lifecycleSpanID derives a deterministic span ID from the trace ID.
// XORs the two halves of the 16-byte trace ID to produce an 8-byte span ID.
// This allows RecoverLifecycleContext to reconstruct the exact parent span
// context after operator restart, preserving parent-child relationships.
func lifecycleSpanID(traceID trace.TraceID) trace.SpanID {
	var sid trace.SpanID
	for i := 0; i < 8; i++ {
		sid[i] = traceID[i] ^ traceID[i+8]
	}
	return sid
}

// ProposalIDGenerator is an OTEL IDGenerator that uses the proposal-derived
// trace ID (and optionally span ID) from the context when available. This
// allows lifecycle spans to be true root spans with deterministic IDs.
type ProposalIDGenerator struct{}

var _ sdktrace.IDGenerator = (*ProposalIDGenerator)(nil)

func (*ProposalIDGenerator) NewIDs(ctx context.Context) (trace.TraceID, trace.SpanID) {
	if tid, ok := ctx.Value(contextKeyProposalTraceID{}).(trace.TraceID); ok {
		if sid, ok := ctx.Value(contextKeyProposalSpanID{}).(trace.SpanID); ok {
			return tid, sid
		}
		var sid trace.SpanID
		_, _ = rand.Read(sid[:])
		return tid, sid
	}
	var tid trace.TraceID
	var sid trace.SpanID
	_, _ = rand.Read(tid[:])
	_, _ = rand.Read(sid[:])
	return tid, sid
}

func (*ProposalIDGenerator) NewSpanID(_ context.Context, _ trace.TraceID) trace.SpanID {
	var sid trace.SpanID
	_, _ = rand.Read(sid[:])
	return sid
}

const (
	tracerName    = "github.com/openshift/lightspeed-agentic-operator/controller/proposal"
	tracerVersion = "v1alpha1"
)

// AuditLogger emits compliance audit events as structured JSON logs and OTEL spans.
type AuditLogger interface {
	EmitProposalReceived(ctx context.Context, proposal *agenticv1alpha1.Proposal)
	EmitAnalysisCompleted(ctx context.Context, proposal *agenticv1alpha1.Proposal, result *agenticv1alpha1.AnalysisResult)
	EmitApprovalReceived(ctx context.Context, proposal *agenticv1alpha1.Proposal, approval *agenticv1alpha1.ProposalApproval)
	EmitExecutionCompleted(ctx context.Context, proposal *agenticv1alpha1.Proposal, result *agenticv1alpha1.ExecutionResult)
	EmitVerificationCompleted(ctx context.Context, proposal *agenticv1alpha1.Proposal, result *agenticv1alpha1.VerificationResult)
	EmitVerificationRetry(ctx context.Context, proposal *agenticv1alpha1.Proposal, result *agenticv1alpha1.VerificationResult, retryCount int)
	EmitEscalationCompleted(ctx context.Context, proposal *agenticv1alpha1.Proposal, result *agenticv1alpha1.EscalationResult)
	EmitProposalTerminal(ctx context.Context, proposal *agenticv1alpha1.Proposal, phase, reason string)

	InjectTraceContext(ctx context.Context, proposal *agenticv1alpha1.Proposal, headers http.Header)

	// Lifecycle root span (§4) — persists across reconciliations.
	EnsureLifecycleSpan(ctx context.Context, proposal *agenticv1alpha1.Proposal) context.Context
	// RecoverLifecycleContext reconstructs trace context after operator restart
	// without exporting a duplicate lifecycle span.
	RecoverLifecycleContext(ctx context.Context, proposal *agenticv1alpha1.Proposal) context.Context
	EndLifecycleSpan(proposal *agenticv1alpha1.Proposal) bool

	// Human approval wait span (§7) — measures human decision time.
	StartApprovalWait(ctx context.Context, proposal *agenticv1alpha1.Proposal)
	EndApprovalWait(proposal *agenticv1alpha1.Proposal, approval *agenticv1alpha1.ProposalApproval)

	// Phase child spans (§6) — children of the lifecycle root.
	StartAnalysisSpan(ctx context.Context, proposal *agenticv1alpha1.Proposal) (context.Context, trace.Span)
	StartExecutionSpan(ctx context.Context, proposal *agenticv1alpha1.Proposal) (context.Context, trace.Span)
	StartVerificationSpan(ctx context.Context, proposal *agenticv1alpha1.Proposal) (context.Context, trace.Span)
	StartEscalationSpan(ctx context.Context, proposal *agenticv1alpha1.Proposal) (context.Context, trace.Span)
}

type spanEntry struct {
	ctx  context.Context
	span trace.Span
}

// ProductionAuditLogger implements AuditLogger with zap + OTEL.
type ProductionAuditLogger struct {
	logger         *zap.Logger
	tracer         trace.Tracer
	lifecycleSpans sync.Map // map[types.UID]*spanEntry
	approvalSpans  sync.Map // map[types.UID]*spanEntry
}

// NoOpAuditLogger implements AuditLogger with no-op behavior (audit disabled).
type NoOpAuditLogger struct{}

// NewProductionAuditLogger creates an audit logger that emits JSON logs and OTEL spans.
func NewProductionAuditLogger(logger *zap.Logger) AuditLogger {
	return &ProductionAuditLogger{
		logger: logger,
		tracer: otel.Tracer(tracerName, trace.WithInstrumentationVersion(tracerVersion)),
	}
}

// NewNoOpAuditLogger creates a no-op audit logger (audit disabled).
func NewNoOpAuditLogger() AuditLogger {
	return &NoOpAuditLogger{}
}

// traceIDFromProposal converts Proposal UID to OTEL trace ID.
// Strips hyphens from UID to produce 32 hex chars.
func traceIDFromProposal(proposal *agenticv1alpha1.Proposal) trace.TraceID {
	uidStr := string(proposal.UID)
	// Remove hyphens: "a1b2c3d4-e5f6-..." → "a1b2c3d4e5f6..."
	hexStr := strings.ReplaceAll(uidStr, "-", "")

	// Decode hex string to bytes (16 bytes = 32 hex chars)
	var traceID trace.TraceID
	decoded, err := hex.DecodeString(hexStr)
	if err != nil || len(decoded) != 16 {
		// Invalid UID format - return zero trace ID (caller handles)
		return traceID
	}
	copy(traceID[:], decoded)
	return traceID
}

// serializeCR builds an audit-safe representation of a CR:
// - metadata: {name, namespace, creationTimestamp, uid} (only these 4 fields)
// - spec: full spec
// - status: full status (for Result CRs)
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

func truncateAttr(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max]) + "…"
}

// emitStructuredLog writes a JSON audit event to stdout.
// Format per spec §20: {timestamp, level, event, trace_id, payload}
func (l *ProductionAuditLogger) emitStructuredLog(event, traceID string, payload interface{}) {
	l.logger.Info(event,
		zap.String("event", event),
		zap.String("trace_id", traceID),
		zap.Any("payload", payload),
	)
}

// addSpanEvent adds an event to the current span with attributes.
func (l *ProductionAuditLogger) addSpanEvent(ctx context.Context, event string, attrs ...attribute.KeyValue) {
	span := trace.SpanFromContext(ctx)
	if span != nil && span.IsRecording() {
		span.AddEvent(event, trace.WithAttributes(attrs...))
	}
}

// proposalRootContext creates a context that carries the proposal's trace ID
// for the ProposalIDGenerator. When tracer.Start is called with this context
// and no parent span, the IDGenerator picks up the trace ID and the resulting
// span is a true root — no phantom parent in Jaeger.
func proposalRootContext(traceID trace.TraceID) context.Context {
	return ContextWithProposalTraceID(context.Background(), traceID)
}

// EmitProposalReceived logs when a new Proposal is received.
func (l *ProductionAuditLogger) EmitProposalReceived(ctx context.Context, proposal *agenticv1alpha1.Proposal) {
	traceID := traceIDFromProposal(proposal)
	serialized, err := serializeCR(proposal)
	if err != nil {
		l.logger.Error("Failed to serialize Proposal for audit", zap.Error(err))
		return
	}

	payload := map[string]interface{}{
		"proposal": serialized,
	}

	l.emitStructuredLog("audit.proposal.received", traceID.String(), payload)
	l.addSpanEvent(ctx, "audit.proposal.received",
		attribute.String("proposal.name", proposal.Name),
		attribute.String("proposal.namespace", proposal.Namespace),
		attribute.String("proposal.uid", string(proposal.UID)),
		attribute.String("proposal.request", truncateAttr(proposal.Spec.Request, 500)),
	)
}

// EmitAnalysisCompleted logs when analysis completes.
func (l *ProductionAuditLogger) EmitAnalysisCompleted(ctx context.Context, proposal *agenticv1alpha1.Proposal, result *agenticv1alpha1.AnalysisResult) {
	traceID := traceIDFromProposal(proposal)
	serialized, err := serializeCR(result)
	if err != nil {
		l.logger.Error("Failed to serialize AnalysisResult for audit", zap.Error(err))
		return
	}

	payload := map[string]interface{}{
		"analysisResult": serialized,
	}

	l.emitStructuredLog("audit.analysis.completed", traceID.String(), payload)
	analysisAttrs := []attribute.KeyValue{
		attribute.String("proposal.name", proposal.Name),
		attribute.String("result.name", result.Name),
		attribute.String("result.uid", string(result.UID)),
		attribute.Int("options.count", len(result.Status.Options)),
	}
	for i, opt := range result.Status.Options {
		if i >= 3 {
			break
		}
		prefix := fmt.Sprintf("option.%d.", i)
		analysisAttrs = append(analysisAttrs,
			attribute.String(prefix+"title", truncateAttr(opt.Title, 200)),
			attribute.String(prefix+"risk", string(opt.Proposal.Risk)),
		)
	}
	l.addSpanEvent(ctx, "audit.analysis.completed", analysisAttrs...)
}

// EmitApprovalReceived logs when an approval decision is made.
func (l *ProductionAuditLogger) EmitApprovalReceived(ctx context.Context, proposal *agenticv1alpha1.Proposal, approval *agenticv1alpha1.ProposalApproval) {
	traceID := traceIDFromProposal(proposal)

	// Extract execution approval if present
	var selectedOption *int32
	for _, stage := range approval.Spec.Stages {
		if stage.Type == agenticv1alpha1.ApprovalStageExecution && stage.Execution.Option != nil {
			selectedOption = stage.Execution.Option
			break
		}
	}

	payload := map[string]interface{}{
		"approvalStages": approval.Spec.Stages,
	}
	if selectedOption != nil {
		payload["selectedOption"] = *selectedOption
	}
	if approval.Spec.Approver.UID != "" {
		payload["approver"] = map[string]interface{}{
			"uid":        approval.Spec.Approver.UID,
			"username":   approval.Spec.Approver.Username,
			"approvedAt": approval.Spec.Approver.ApprovedAt,
		}
	}

	l.emitStructuredLog("audit.approval.received", traceID.String(), payload)
	attrs := []attribute.KeyValue{
		attribute.String("proposal.name", proposal.Name),
	}
	if selectedOption != nil {
		attrs = append(attrs, attribute.Int("selected_option", int(*selectedOption)))
	}
	if approval.Spec.Approver.UID != "" {
		attrs = append(attrs,
			attribute.String("approver.uid", approval.Spec.Approver.UID),
			attribute.String("approver.username", approval.Spec.Approver.Username),
		)
	}
	l.addSpanEvent(ctx, "audit.approval.received", attrs...)
}

// EmitExecutionCompleted logs when execution completes.
func (l *ProductionAuditLogger) EmitExecutionCompleted(ctx context.Context, proposal *agenticv1alpha1.Proposal, result *agenticv1alpha1.ExecutionResult) {
	traceID := traceIDFromProposal(proposal)
	serialized, err := serializeCR(result)
	if err != nil {
		l.logger.Error("Failed to serialize ExecutionResult for audit", zap.Error(err))
		return
	}

	payload := map[string]interface{}{
		"executionResult": serialized,
	}

	l.emitStructuredLog("audit.execution.completed", traceID.String(), payload)
	execAttrs := []attribute.KeyValue{
		attribute.String("proposal.name", proposal.Name),
		attribute.String("result.name", result.Name),
		attribute.String("result.uid", string(result.UID)),
		attribute.Int("actions_taken.count", len(result.Status.ActionsTaken)),
		attribute.String("failure_reason", result.Status.FailureReason),
	}
	for i, action := range result.Status.ActionsTaken {
		if i >= 5 {
			break
		}
		execAttrs = append(execAttrs,
			attribute.String(fmt.Sprintf("action.%d.type", i), action.Type),
			attribute.String(fmt.Sprintf("action.%d.description", i), truncateAttr(action.Description, 200)),
		)
	}
	l.addSpanEvent(ctx, "audit.execution.completed", execAttrs...)
}

// EmitVerificationCompleted logs when verification completes.
func (l *ProductionAuditLogger) EmitVerificationCompleted(ctx context.Context, proposal *agenticv1alpha1.Proposal, result *agenticv1alpha1.VerificationResult) {
	traceID := traceIDFromProposal(proposal)
	serialized, err := serializeCR(result)
	if err != nil {
		l.logger.Error("Failed to serialize VerificationResult for audit", zap.Error(err))
		return
	}

	payload := map[string]interface{}{
		"verificationResult": serialized,
	}

	l.emitStructuredLog("audit.verification.completed", traceID.String(), payload)
	verifyAttrs := []attribute.KeyValue{
		attribute.String("proposal.name", proposal.Name),
		attribute.String("result.name", result.Name),
		attribute.String("result.uid", string(result.UID)),
		attribute.String("summary", truncateAttr(result.Status.Summary, 500)),
		attribute.Int("checks.count", len(result.Status.Checks)),
	}
	for i, check := range result.Status.Checks {
		if i >= 5 {
			break
		}
		verifyAttrs = append(verifyAttrs,
			attribute.String(fmt.Sprintf("check.%d.name", i), check.Name),
			attribute.String(fmt.Sprintf("check.%d.result", i), string(check.Result)),
		)
	}
	l.addSpanEvent(ctx, "audit.verification.completed", verifyAttrs...)
}

// EmitVerificationRetry logs when verification triggers a retry.
func (l *ProductionAuditLogger) EmitVerificationRetry(ctx context.Context, proposal *agenticv1alpha1.Proposal, result *agenticv1alpha1.VerificationResult, retryCount int) {
	traceID := traceIDFromProposal(proposal)
	serialized, err := serializeCR(result)
	if err != nil {
		l.logger.Error("Failed to serialize VerificationResult for audit retry", zap.Error(err))
		return
	}

	payload := map[string]interface{}{
		"verificationResult": serialized,
		"retryCount":         retryCount,
	}

	l.emitStructuredLog("audit.verification.retry", traceID.String(), payload)
	l.addSpanEvent(ctx, "audit.verification.retry",
		attribute.String("proposal.name", proposal.Name),
		attribute.String("result.name", result.Name),
		attribute.String("summary", truncateAttr(result.Status.Summary, 500)),
		attribute.Int("retry_count", retryCount),
		attribute.Int("checks.count", len(result.Status.Checks)),
	)
}

// EmitEscalationCompleted logs when escalation completes.
func (l *ProductionAuditLogger) EmitEscalationCompleted(ctx context.Context, proposal *agenticv1alpha1.Proposal, result *agenticv1alpha1.EscalationResult) {
	traceID := traceIDFromProposal(proposal)
	serialized, err := serializeCR(result)
	if err != nil {
		l.logger.Error("Failed to serialize EscalationResult for audit", zap.Error(err))
		return
	}

	payload := map[string]interface{}{
		"escalationResult": serialized,
	}

	l.emitStructuredLog("audit.escalation.completed", traceID.String(), payload)
	l.addSpanEvent(ctx, "audit.escalation.completed",
		attribute.String("proposal.name", proposal.Name),
		attribute.String("result.name", result.Name),
		attribute.String("result.uid", string(result.UID)),
		attribute.String("summary", truncateAttr(result.Status.Summary, 500)),
	)
}

// EmitProposalTerminal logs when a proposal reaches a terminal state.
// Creates a short-lived child span so the terminal event is visible in Jaeger.
// Must be called BEFORE EndLifecycleSpan (which deletes the map entry).
// Skips emit if no lifecycle context exists (proposal was already terminal before restart).
func (l *ProductionAuditLogger) EmitProposalTerminal(ctx context.Context, proposal *agenticv1alpha1.Proposal, phase, reason string) {
	if _, ok := l.lifecycleSpans.Load(proposal.UID); !ok {
		return
	}

	traceID := traceIDFromProposal(proposal)
	payload := map[string]interface{}{
		"phase":  phase,
		"reason": reason,
	}
	l.emitStructuredLog("audit.proposal.terminal", traceID.String(), payload)

	parentCtx := l.lifecycleContext(proposal)
	_, span := l.tracer.Start(parentCtx, "proposal.terminal",
		trace.WithAttributes(
			attribute.String("proposal.name", proposal.Name),
			attribute.String("proposal.namespace", proposal.Namespace),
			attribute.String("phase", phase),
			attribute.String("reason", reason),
		),
	)
	span.End()
}

// InjectTraceContext injects W3C traceparent header for downstream propagation.
func (l *ProductionAuditLogger) InjectTraceContext(ctx context.Context, proposal *agenticv1alpha1.Proposal, headers http.Header) {
	// Prefer the active span from ctx (set by StartAnalysisSpan, StartExecutionSpan, etc.)
	// so the sandbox call is linked to the correct parent phase span.
	sc := trace.SpanContextFromContext(ctx)
	if !sc.IsValid() {
		traceID := traceIDFromProposal(proposal)
		sc = trace.NewSpanContext(trace.SpanContextConfig{
			TraceID:    traceID,
			SpanID:     lifecycleSpanID(traceID),
			TraceFlags: trace.FlagsSampled,
		})
		ctx = trace.ContextWithSpanContext(context.Background(), sc)
	}

	propagator := propagation.TraceContext{}
	propagator.Inject(ctx, propagation.HeaderCarrier(headers))
}

// lifecycleContext returns the lifecycle span's context for nesting child spans.
// Falls back to a proposal root context if no lifecycle span is active (e.g. operator
// restart before EnsureLifecycleSpan is called).
func (l *ProductionAuditLogger) lifecycleContext(proposal *agenticv1alpha1.Proposal) context.Context {
	if entry, ok := l.lifecycleSpans.Load(proposal.UID); ok {
		if se, ok := entry.(*spanEntry); ok {
			return se.ctx
		}
	}
	return proposalRootContext(traceIDFromProposal(proposal))
}

// EnsureLifecycleSpan creates a short-lived root proposal.lifecycle span using the
// continuation pattern. The span is ended immediately so the batch exporter can
// flush it to collectors without waiting for the proposal to reach a terminal state.
// The span's context is stored so that subsequent child spans (analyze, execute, etc.)
// are parented under it — OTEL child-parent relationships are ID-based, so children
// can outlive their parent span.
//
// Idempotent — safe to call on every reconciliation. On operator restart, reconstructs
// the span from the Proposal UID (§5).
func (l *ProductionAuditLogger) EnsureLifecycleSpan(ctx context.Context, proposal *agenticv1alpha1.Proposal) context.Context {
	if entry, ok := l.lifecycleSpans.Load(proposal.UID); ok {
		if se, ok := entry.(*spanEntry); ok {
			return se.ctx
		}
	}
	traceID := traceIDFromProposal(proposal)
	rootCtx := proposalRootContext(traceID)
	rootCtx = ContextWithProposalSpanID(rootCtx, lifecycleSpanID(traceID))
	spanCtx, span := l.tracer.Start(rootCtx, "proposal.lifecycle",
		trace.WithAttributes(
			attribute.String("proposal.name", proposal.Name),
			attribute.String("proposal.namespace", proposal.Namespace),
			attribute.String("proposal.uid", string(proposal.UID)),
		),
	)
	span.End()
	entry := &spanEntry{ctx: spanCtx, span: span}
	if existing, loaded := l.lifecycleSpans.LoadOrStore(proposal.UID, entry); loaded {
		if se, ok := existing.(*spanEntry); ok {
			return se.ctx
		}
	}
	return spanCtx
}

// RecoverLifecycleContext reconstructs the lifecycle trace context after an operator
// restart. It stores a proposal-root context in the lifecycle map so child spans
// (analyze, execute, etc.) share the same trace ID, but does NOT create or export
// a span — the original lifecycle span was already exported before the restart.
func (l *ProductionAuditLogger) RecoverLifecycleContext(_ context.Context, proposal *agenticv1alpha1.Proposal) context.Context {
	if entry, ok := l.lifecycleSpans.Load(proposal.UID); ok {
		if se, ok := entry.(*spanEntry); ok {
			return se.ctx
		}
	}
	traceID := traceIDFromProposal(proposal)
	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    traceID,
		SpanID:     lifecycleSpanID(traceID),
		TraceFlags: trace.FlagsSampled,
		Remote:     true,
	})
	ctx := trace.ContextWithRemoteSpanContext(context.Background(), sc)
	entry := &spanEntry{ctx: ctx}
	if existing, loaded := l.lifecycleSpans.LoadOrStore(proposal.UID, entry); loaded {
		if se, ok := existing.(*spanEntry); ok {
			return se.ctx
		}
	}
	return ctx
}

func (l *ProductionAuditLogger) EndLifecycleSpan(proposal *agenticv1alpha1.Proposal) bool {
	_, existed := l.lifecycleSpans.LoadAndDelete(proposal.UID)
	return existed
}

// StartApprovalWait creates a proposal.human_approval child span under the lifecycle root.
// Duration = human decision time (§7).
func (l *ProductionAuditLogger) StartApprovalWait(ctx context.Context, proposal *agenticv1alpha1.Proposal) {
	if _, ok := l.approvalSpans.Load(proposal.UID); ok {
		return
	}
	parentCtx := l.lifecycleContext(proposal)
	opts := []trace.SpanStartOption{
		trace.WithAttributes(
			attribute.String("proposal.name", proposal.Name),
			attribute.String("proposal.namespace", proposal.Namespace),
		),
	}
	// After operator restart, backdate span start to when analysis completed
	// so the span covers the full human decision time (§7).
	analyzedCond := meta.FindStatusCondition(proposal.Status.Conditions, agenticv1alpha1.ProposalConditionAnalyzed)
	if analyzedCond != nil && analyzedCond.Status == metav1.ConditionTrue && !analyzedCond.LastTransitionTime.IsZero() {
		opts = append(opts, trace.WithTimestamp(analyzedCond.LastTransitionTime.Time))
	}
	spanCtx, span := l.tracer.Start(parentCtx, "proposal.human_approval", opts...)
	l.approvalSpans.LoadOrStore(proposal.UID, &spanEntry{ctx: spanCtx, span: span})
}

// EndApprovalWait ends the human_approval span, recording approver identity.
func (l *ProductionAuditLogger) EndApprovalWait(proposal *agenticv1alpha1.Proposal, approval *agenticv1alpha1.ProposalApproval) {
	if entry, ok := l.approvalSpans.LoadAndDelete(proposal.UID); ok {
		if se, ok := entry.(*spanEntry); ok {
			if approval != nil {
				if approval.Spec.Approver.UID != "" {
					se.span.SetAttributes(
						attribute.String("approver.uid", approval.Spec.Approver.UID),
						attribute.String("approver.username", approval.Spec.Approver.Username),
						attribute.String("approver.approvedAt", approval.Spec.Approver.ApprovedAt),
					)
				}
				for i := len(approval.Spec.Stages) - 1; i >= 0; i-- {
					if approval.Spec.Stages[i].Decision != "" {
						se.span.SetAttributes(attribute.String("approval.decision", string(approval.Spec.Stages[i].Decision)))
						break
					}
				}
			}
			se.span.End()
		}
	}
}

// StartAnalysisSpan creates a proposal.analyze child span under the lifecycle root.
func (l *ProductionAuditLogger) StartAnalysisSpan(ctx context.Context, proposal *agenticv1alpha1.Proposal) (context.Context, trace.Span) {
	parentCtx := l.lifecycleContext(proposal)
	return l.tracer.Start(parentCtx, "proposal.analyze",
		trace.WithAttributes(
			attribute.String("proposal.name", proposal.Name),
			attribute.String("proposal.namespace", proposal.Namespace),
		),
	)
}

// StartExecutionSpan creates a proposal.execute child span under the lifecycle root.
// Includes retry_index attribute when retrying (§8).
func (l *ProductionAuditLogger) StartExecutionSpan(ctx context.Context, proposal *agenticv1alpha1.Proposal) (context.Context, trace.Span) {
	parentCtx := l.lifecycleContext(proposal)
	attrs := []attribute.KeyValue{
		attribute.String("proposal.name", proposal.Name),
		attribute.String("proposal.namespace", proposal.Namespace),
	}
	if proposal.Status.Steps.Execution.RetryCount != nil && *proposal.Status.Steps.Execution.RetryCount > 0 {
		attrs = append(attrs, attribute.Int("retry_index", int(*proposal.Status.Steps.Execution.RetryCount)))
	}
	return l.tracer.Start(parentCtx, "proposal.execute", trace.WithAttributes(attrs...))
}

// StartVerificationSpan creates a proposal.verify child span under the lifecycle root.
// Includes retry_index attribute when retrying (§8).
func (l *ProductionAuditLogger) StartVerificationSpan(ctx context.Context, proposal *agenticv1alpha1.Proposal) (context.Context, trace.Span) {
	parentCtx := l.lifecycleContext(proposal)
	attrs := []attribute.KeyValue{
		attribute.String("proposal.name", proposal.Name),
		attribute.String("proposal.namespace", proposal.Namespace),
	}
	if proposal.Status.Steps.Execution.RetryCount != nil && *proposal.Status.Steps.Execution.RetryCount > 0 {
		attrs = append(attrs, attribute.Int("retry_index", int(*proposal.Status.Steps.Execution.RetryCount)))
	}
	return l.tracer.Start(parentCtx, "proposal.verify", trace.WithAttributes(attrs...))
}

// StartEscalationSpan creates a proposal.escalate child span under the lifecycle root.
func (l *ProductionAuditLogger) StartEscalationSpan(ctx context.Context, proposal *agenticv1alpha1.Proposal) (context.Context, trace.Span) {
	parentCtx := l.lifecycleContext(proposal)
	return l.tracer.Start(parentCtx, "proposal.escalate",
		trace.WithAttributes(
			attribute.String("proposal.name", proposal.Name),
			attribute.String("proposal.namespace", proposal.Namespace),
		),
	)
}

// NoOp implementations (all methods do nothing)
func (l *NoOpAuditLogger) EmitProposalReceived(ctx context.Context, proposal *agenticv1alpha1.Proposal) {
}
func (l *NoOpAuditLogger) EmitAnalysisCompleted(ctx context.Context, proposal *agenticv1alpha1.Proposal, result *agenticv1alpha1.AnalysisResult) {
}
func (l *NoOpAuditLogger) EmitApprovalReceived(ctx context.Context, proposal *agenticv1alpha1.Proposal, approval *agenticv1alpha1.ProposalApproval) {
}
func (l *NoOpAuditLogger) EmitExecutionCompleted(ctx context.Context, proposal *agenticv1alpha1.Proposal, result *agenticv1alpha1.ExecutionResult) {
}
func (l *NoOpAuditLogger) EmitVerificationCompleted(ctx context.Context, proposal *agenticv1alpha1.Proposal, result *agenticv1alpha1.VerificationResult) {
}
func (l *NoOpAuditLogger) EmitVerificationRetry(ctx context.Context, proposal *agenticv1alpha1.Proposal, result *agenticv1alpha1.VerificationResult, retryCount int) {
}
func (l *NoOpAuditLogger) EmitEscalationCompleted(ctx context.Context, proposal *agenticv1alpha1.Proposal, result *agenticv1alpha1.EscalationResult) {
}
func (l *NoOpAuditLogger) EmitProposalTerminal(ctx context.Context, proposal *agenticv1alpha1.Proposal, phase, reason string) {
}
func (l *NoOpAuditLogger) InjectTraceContext(ctx context.Context, proposal *agenticv1alpha1.Proposal, headers http.Header) {
}
func (l *NoOpAuditLogger) EnsureLifecycleSpan(ctx context.Context, proposal *agenticv1alpha1.Proposal) context.Context {
	return ctx
}
func (l *NoOpAuditLogger) RecoverLifecycleContext(ctx context.Context, proposal *agenticv1alpha1.Proposal) context.Context {
	return ctx
}
func (l *NoOpAuditLogger) EndLifecycleSpan(proposal *agenticv1alpha1.Proposal) bool { return false }
func (l *NoOpAuditLogger) StartApprovalWait(ctx context.Context, proposal *agenticv1alpha1.Proposal) {
}
func (l *NoOpAuditLogger) EndApprovalWait(proposal *agenticv1alpha1.Proposal, approval *agenticv1alpha1.ProposalApproval) {
}
func (l *NoOpAuditLogger) StartAnalysisSpan(ctx context.Context, proposal *agenticv1alpha1.Proposal) (context.Context, trace.Span) {
	return ctx, nil
}
func (l *NoOpAuditLogger) StartExecutionSpan(ctx context.Context, proposal *agenticv1alpha1.Proposal) (context.Context, trace.Span) {
	return ctx, nil
}
func (l *NoOpAuditLogger) StartVerificationSpan(ctx context.Context, proposal *agenticv1alpha1.Proposal) (context.Context, trace.Span) {
	return ctx, nil
}
func (l *NoOpAuditLogger) StartEscalationSpan(ctx context.Context, proposal *agenticv1alpha1.Proposal) (context.Context, trace.Span) {
	return ctx, nil
}
