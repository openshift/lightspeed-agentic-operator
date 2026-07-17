package agenticrun

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.24.0"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
)

func TestTraceIDFromAgenticRun(t *testing.T) {
	// Given: AgenticRun with UID "a1b2c3d4-e5f6-7890-1234-567890abcdef"
	run := &agenticv1alpha1.AgenticRun{
		ObjectMeta: metav1.ObjectMeta{
			UID: types.UID("a1b2c3d4-e5f6-7890-1234-567890abcdef"),
		},
	}

	// When: Convert to trace ID
	traceID := traceIDFromAgenticRun(run)

	// Then: Hyphens stripped, valid trace.TraceID (32 hex chars)
	expected := "a1b2c3d4e5f678901234567890abcdef"
	actual := traceID.String()
	if actual != expected {
		t.Errorf("Expected trace ID %s, got %s", expected, actual)
	}

	// Verify it's a valid trace ID (non-zero)
	if !traceID.IsValid() {
		t.Error("Trace ID should be valid")
	}
}

func TestTraceIDFromAgenticRun_InvalidUID(t *testing.T) {
	// Given: AgenticRun with short UID (invalid for trace ID)
	run := &agenticv1alpha1.AgenticRun{
		ObjectMeta: metav1.ObjectMeta{
			UID: types.UID("short"),
		},
	}

	// When: Convert to trace ID
	traceID := traceIDFromAgenticRun(run)

	// Then: Should return zero trace ID for invalid input
	if traceID.IsValid() {
		t.Error("Expected zero/invalid trace ID for malformed UID")
	}
}

func TestSerializeCR_AgenticRun(t *testing.T) {
	// Given: AgenticRun CR with metadata and spec
	now := metav1.Now()
	run := &agenticv1alpha1.AgenticRun{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "test-run",
			Namespace:         "test-ns",
			UID:               types.UID("a1b2c3d4-e5f6-7890-1234-567890abcdef"),
			CreationTimestamp: now,
			Annotations:       map[string]string{"extra": "should-not-appear"},
		},
		Spec: agenticv1alpha1.AgenticRunSpec{
			Request: "test request",
		},
	}

	// When: Serialize CR
	serialized, err := serializeCR(run)

	// Then: No error
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	// Assert metadata contains ONLY required fields
	metadata, ok := serialized["metadata"].(map[string]interface{})
	if !ok {
		t.Fatal("metadata field missing or wrong type")
	}
	if metadata["name"] != "test-run" {
		t.Errorf("Expected name='test-run', got %v", metadata["name"])
	}
	if metadata["namespace"] != "test-ns" {
		t.Errorf("Expected namespace='test-ns', got %v", metadata["namespace"])
	}
	if metadata["uid"] != "a1b2c3d4-e5f6-7890-1234-567890abcdef" {
		t.Errorf("Expected uid, got %v", metadata["uid"])
	}
	if _, ok := metadata["creationTimestamp"]; !ok {
		t.Error("creationTimestamp missing")
	}
	if len(metadata) != 4 {
		t.Errorf("Expected exactly 4 metadata fields, got %d: %v", len(metadata), metadata)
	}

	// Assert spec present
	if _, ok := serialized["spec"]; !ok {
		t.Error("spec field missing")
	}

	// Status may or may not be present (JSON marshal includes zero values)
	// Just verify no panic
}

func TestSerializeCR_AnalysisResult(t *testing.T) {
	// Given: AnalysisResult CR with status
	now := metav1.Now()
	result := &agenticv1alpha1.AnalysisResult{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "test-result",
			Namespace:         "test-ns",
			UID:               types.UID("b2c3d4e5-f6a7-8901-2345-67890abcdef0"),
			CreationTimestamp: now,
		},
		Spec: agenticv1alpha1.AnalysisResultSpec{
			AgenticRunName: "test-run",
		},
		Status: agenticv1alpha1.AnalysisResultStatus{
			Conditions: []metav1.Condition{
				{
					Type:   "Completed",
					Status: metav1.ConditionTrue,
				},
			},
		},
	}

	// When: Serialize CR
	serialized, err := serializeCR(result)

	// Then: No error
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	// Assert spec present
	if _, ok := serialized["spec"]; !ok {
		t.Error("spec field missing")
	}

	// Assert status present (Result CRs include status)
	status, ok := serialized["status"].(map[string]interface{})
	if !ok {
		t.Fatal("status field missing or wrong type for Result CR")
	}
	if _, ok := status["conditions"]; !ok {
		t.Error("status.conditions missing")
	}
}

func TestNoOpAuditLogger_NoPanic(t *testing.T) {
	// Given: NoOpAuditLogger
	logger := NewNoOpAuditLogger()
	run := &agenticv1alpha1.AgenticRun{}

	// When: Call all methods
	// Then: No panics
	logger.EmitAgenticRunReceived(context.Background(), run)
	logger.EmitAnalysisCompleted(context.Background(), run, nil)
	logger.EmitApprovalReceived(context.Background(), run, nil)
	logger.EmitExecutionCompleted(context.Background(), run, nil)
	logger.EmitVerificationCompleted(context.Background(), run, nil)
	logger.EmitVerificationRetry(context.Background(), run, nil, 1)
	logger.EmitEscalationCompleted(context.Background(), run, nil)
	logger.EmitAgenticRunTerminal(context.Background(), run, "Completed", "success")
	logger.InjectTraceContext(context.Background(), run, http.Header{})
	ctx, span := logger.StartAnalysisSpan(context.Background(), run)
	if ctx == nil || span != nil {
		t.Error("StartAnalysisSpan should return ctx unchanged, span nil")
	}
}

func TestEmitAgenticRunReceived_Structure(t *testing.T) {
	// Given: ProductionAuditLogger with in-memory buffer
	var buf bytes.Buffer
	encoder := zapcore.NewJSONEncoder(zapcore.EncoderConfig{
		TimeKey:        "timestamp",
		LevelKey:       "level",
		MessageKey:     "msg",
		EncodeTime:     zapcore.ISO8601TimeEncoder,
		EncodeLevel:    zapcore.LowercaseLevelEncoder,
		EncodeDuration: zapcore.SecondsDurationEncoder,
	})
	core := zapcore.NewCore(encoder, zapcore.AddSync(&buf), zapcore.InfoLevel)
	logger := zap.New(core)
	auditLogger := NewProductionAuditLogger(logger)

	// When: Emit run received event
	run := &agenticv1alpha1.AgenticRun{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "test-run",
			Namespace:         "test-ns",
			UID:               types.UID("a1b2c3d4-e5f6-7890-1234-567890abcdef"),
			CreationTimestamp: metav1.Now(),
		},
		Spec: agenticv1alpha1.AgenticRunSpec{
			Request: "test request",
		},
	}
	auditLogger.EmitAgenticRunReceived(context.Background(), run)

	// Then: JSON log has required fields per spec §20
	var logEntry map[string]interface{}
	if err := json.Unmarshal(buf.Bytes(), &logEntry); err != nil {
		t.Fatalf("Failed to parse JSON log: %v", err)
	}

	// Assert required fields present
	if _, ok := logEntry["timestamp"]; !ok {
		t.Error("timestamp missing")
	}
	if logEntry["level"] != "info" {
		t.Errorf("Expected level='info', got %v", logEntry["level"])
	}
	if logEntry["event"] != "audit.agenticrun.received" {
		t.Errorf("Expected event='audit.agenticrun.received', got %v", logEntry["event"])
	}
	if logEntry["trace_id"] != "a1b2c3d4e5f678901234567890abcdef" {
		t.Errorf("Expected trace_id (no hyphens), got %v", logEntry["trace_id"])
	}
	if _, ok := logEntry["payload"]; !ok {
		t.Error("payload missing")
	}

	// Assert payload contains run
	payload := logEntry["payload"].(map[string]interface{})
	if _, ok := payload["run"]; !ok {
		t.Error("payload.run missing")
	}
}

func TestStartAnalysisSpan_CreatesSpan(t *testing.T) {
	// Given: ProductionAuditLogger with in-memory span recorder and AgenticRunIDGenerator
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr), sdktrace.WithIDGenerator(&AgenticRunIDGenerator{}))
	otel.SetTracerProvider(tp)
	defer otel.SetTracerProvider(sdktrace.NewTracerProvider()) // Reset

	logger := zap.NewNop()
	auditLogger := NewProductionAuditLogger(logger).(*ProductionAuditLogger)

	run := &agenticv1alpha1.AgenticRun{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-run",
			Namespace: "test-ns",
			UID:       types.UID("a1b2c3d4-e5f6-7890-1234-567890abcdef"),
		},
	}

	// When: Ensure lifecycle span, then start analysis span as child
	auditLogger.EnsureLifecycleSpan(context.Background(), run)
	ctx, span := auditLogger.StartAnalysisSpan(context.Background(), run)
	if span != nil {
		span.End()
	}

	// Then: Two spans created — lifecycle (root) and analyze (child)
	spans := sr.Ended()
	if len(spans) != 2 {
		t.Fatalf("Expected 2 spans, got %d", len(spans))
	}

	lifecycleSpan := spans[0]
	if lifecycleSpan.Name() != "agenticrun.lifecycle" {
		t.Errorf("Expected span name 'agenticrun.lifecycle', got %s", lifecycleSpan.Name())
	}

	analyzeSpan := spans[1]
	if analyzeSpan.Name() != "agenticrun.analyze" {
		t.Errorf("Expected span name 'agenticrun.analyze', got %s", analyzeSpan.Name())
	}

	expectedTraceID := "a1b2c3d4e5f678901234567890abcdef"
	if analyzeSpan.SpanContext().TraceID().String() != expectedTraceID {
		t.Errorf("Expected trace ID %s, got %s", expectedTraceID, analyzeSpan.SpanContext().TraceID().String())
	}

	// Verify analyze is a child of lifecycle
	if analyzeSpan.Parent().SpanID() != lifecycleSpan.SpanContext().SpanID() {
		t.Errorf("Analyze span should be child of lifecycle span")
	}

	// Verify context has span
	if trace.SpanFromContext(ctx) == nil {
		t.Error("Context should contain span")
	}
}

func TestEnsureLifecycleSpan_ShortLived(t *testing.T) {
	// Given: ProductionAuditLogger with span recorder and AgenticRunIDGenerator
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr), sdktrace.WithIDGenerator(&AgenticRunIDGenerator{}))
	otel.SetTracerProvider(tp)
	defer otel.SetTracerProvider(sdktrace.NewTracerProvider())

	logger := zap.NewNop()
	auditLogger := NewProductionAuditLogger(logger).(*ProductionAuditLogger)

	run := &agenticv1alpha1.AgenticRun{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-run",
			Namespace: "test-ns",
			UID:       types.UID("a1b2c3d4-e5f6-7890-1234-567890abcdef"),
		},
	}

	// When: EnsureLifecycleSpan is called
	ctx1 := auditLogger.EnsureLifecycleSpan(context.Background(), run)

	// Then: Lifecycle span is immediately ended (exported to recorder)
	spans := sr.Ended()
	if len(spans) != 1 {
		t.Fatalf("Expected 1 ended span after EnsureLifecycleSpan, got %d", len(spans))
	}
	if spans[0].Name() != "agenticrun.lifecycle" {
		t.Errorf("Expected 'agenticrun.lifecycle', got %s", spans[0].Name())
	}

	// When: Called again (idempotent)
	ctx2 := auditLogger.EnsureLifecycleSpan(context.Background(), run)

	// Then: No new span created, same context returned
	if len(sr.Ended()) != 1 {
		t.Errorf("Expected still 1 span (idempotent), got %d", len(sr.Ended()))
	}
	if trace.SpanFromContext(ctx1).SpanContext().SpanID() != trace.SpanFromContext(ctx2).SpanContext().SpanID() {
		t.Error("Idempotent call should return same context")
	}

	// When: EndLifecycleSpan cleans up
	auditLogger.EndLifecycleSpan(run)

	// Then: A new call creates a fresh span
	auditLogger.EnsureLifecycleSpan(context.Background(), run)
	if len(sr.Ended()) != 2 {
		t.Errorf("Expected 2 spans after re-create, got %d", len(sr.Ended()))
	}
}

func TestRecoverLifecycleContext_NoSpanExported(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr), sdktrace.WithIDGenerator(&AgenticRunIDGenerator{}))
	otel.SetTracerProvider(tp)
	defer otel.SetTracerProvider(sdktrace.NewTracerProvider())

	logger := zap.NewNop()
	auditLogger := NewProductionAuditLogger(logger).(*ProductionAuditLogger)

	run := &agenticv1alpha1.AgenticRun{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-run",
			Namespace: "test-ns",
			UID:       types.UID("a1b2c3d4-e5f6-7890-1234-567890abcdef"),
		},
	}

	ctx := auditLogger.RecoverLifecycleContext(context.Background(), run)

	if len(sr.Ended()) != 0 {
		t.Fatalf("RecoverLifecycleContext must not export spans, got %d", len(sr.Ended()))
	}

	ctx2 := auditLogger.RecoverLifecycleContext(context.Background(), run)
	if len(sr.Ended()) != 0 {
		t.Fatal("Idempotent RecoverLifecycleContext must not export spans")
	}
	_ = ctx
	_ = ctx2
}

func TestRecoverLifecycleContext_ChildSpansNestedUnderLifecycle(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr), sdktrace.WithIDGenerator(&AgenticRunIDGenerator{}))
	otel.SetTracerProvider(tp)
	defer otel.SetTracerProvider(sdktrace.NewTracerProvider())

	logger := zap.NewNop()
	auditLogger := NewProductionAuditLogger(logger).(*ProductionAuditLogger)

	run := &agenticv1alpha1.AgenticRun{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-run",
			Namespace: "test-ns",
			UID:       types.UID("a1b2c3d4-e5f6-7890-1234-567890abcdef"),
		},
	}

	auditLogger.RecoverLifecycleContext(context.Background(), run)
	_, span := auditLogger.StartAnalysisSpan(context.Background(), run)
	span.End()

	spans := sr.Ended()
	if len(spans) != 1 {
		t.Fatalf("Expected 1 span (analysis only), got %d", len(spans))
	}

	expectedTraceID := traceIDFromAgenticRun(run)
	expectedParentSpanID := lifecycleSpanID(expectedTraceID)

	if spans[0].SpanContext().TraceID() != expectedTraceID {
		t.Errorf("Child span trace ID = %s, want %s", spans[0].SpanContext().TraceID(), expectedTraceID)
	}
	if spans[0].Parent().SpanID() != expectedParentSpanID {
		t.Errorf("Child span parent ID = %s, want %s (lifecycle deterministic span ID)", spans[0].Parent().SpanID(), expectedParentSpanID)
	}
}

func TestRecoverLifecycleContext_MatchesEnsureLifecycleSpanParentID(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr), sdktrace.WithIDGenerator(&AgenticRunIDGenerator{}))
	otel.SetTracerProvider(tp)
	defer otel.SetTracerProvider(sdktrace.NewTracerProvider())

	logger := zap.NewNop()

	run := &agenticv1alpha1.AgenticRun{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-run",
			Namespace: "test-ns",
			UID:       types.UID("a1b2c3d4-e5f6-7890-1234-567890abcdef"),
		},
	}

	// Simulate normal flow: EnsureLifecycleSpan + child
	normalLogger := NewProductionAuditLogger(logger).(*ProductionAuditLogger)
	normalLogger.EnsureLifecycleSpan(context.Background(), run)
	_, normalChild := normalLogger.StartExecutionSpan(context.Background(), run)
	normalChild.End()

	// Simulate restart flow: RecoverLifecycleContext + child
	restartLogger := NewProductionAuditLogger(logger).(*ProductionAuditLogger)
	restartLogger.RecoverLifecycleContext(context.Background(), run)
	_, restartChild := restartLogger.StartExecutionSpan(context.Background(), run)
	restartChild.End()

	spans := sr.Ended()
	// lifecycle + normal child + restart child = 3
	if len(spans) != 3 {
		t.Fatalf("Expected 3 spans, got %d", len(spans))
	}

	lifecycleSpanID := spans[0].SpanContext().SpanID()
	normalParentID := spans[1].Parent().SpanID()
	restartParentID := spans[2].Parent().SpanID()

	if normalParentID != lifecycleSpanID {
		t.Errorf("Normal child parent = %s, want lifecycle %s", normalParentID, lifecycleSpanID)
	}
	if restartParentID != lifecycleSpanID {
		t.Errorf("Restart child parent = %s, want lifecycle %s (must match for Jaeger nesting)", restartParentID, lifecycleSpanID)
	}
}

func TestInjectTraceContext_W3CFormat(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr), sdktrace.WithIDGenerator(&AgenticRunIDGenerator{}))
	otel.SetTracerProvider(tp)
	defer otel.SetTracerProvider(sdktrace.NewTracerProvider())

	logger := zap.NewNop()
	auditLogger := NewProductionAuditLogger(logger).(*ProductionAuditLogger)

	run := &agenticv1alpha1.AgenticRun{
		ObjectMeta: metav1.ObjectMeta{
			UID: types.UID("a1b2c3d4-e5f6-7890-1234-567890abcdef"),
		},
	}

	t.Run("fallback_no_active_span", func(t *testing.T) {
		headers := http.Header{}
		auditLogger.InjectTraceContext(context.Background(), run, headers)

		traceparent := headers.Get("traceparent")
		if traceparent == "" {
			t.Fatal("traceparent header missing")
		}

		parts := strings.Split(traceparent, "-")
		if len(parts) != 4 {
			t.Fatalf("Expected 4 parts in traceparent, got %d: %s", len(parts), traceparent)
		}
		expectedTraceID := "a1b2c3d4e5f678901234567890abcdef"
		if parts[1] != expectedTraceID {
			t.Errorf("Expected trace ID %s, got %s", expectedTraceID, parts[1])
		}
		traceID := traceIDFromAgenticRun(run)
		expectedSpanID := lifecycleSpanID(traceID).String()
		if parts[2] != expectedSpanID {
			t.Errorf("Fallback span ID = %s, want lifecycle span ID %s", parts[2], expectedSpanID)
		}
	})

	t.Run("uses_active_phase_span", func(t *testing.T) {
		auditLogger.EnsureLifecycleSpan(context.Background(), run)
		spanCtx, span := auditLogger.StartAnalysisSpan(context.Background(), run)

		headers := http.Header{}
		auditLogger.InjectTraceContext(spanCtx, run, headers)

		traceparent := headers.Get("traceparent")
		parts := strings.Split(traceparent, "-")
		if len(parts) != 4 {
			t.Fatalf("Expected 4 parts in traceparent, got %d: %s", len(parts), traceparent)
		}

		activeSpanID := span.SpanContext().SpanID().String()
		if parts[2] != activeSpanID {
			t.Errorf("Injected span ID = %s, want active analysis span ID %s", parts[2], activeSpanID)
		}

		span.End()
		auditLogger.EndLifecycleSpan(run)
	})
}

func TestEmitApprovalReceived_Structure(t *testing.T) {
	// Given: ProductionAuditLogger
	var buf bytes.Buffer
	encoder := zapcore.NewJSONEncoder(zapcore.EncoderConfig{
		TimeKey:        "timestamp",
		LevelKey:       "level",
		MessageKey:     "msg",
		EncodeTime:     zapcore.ISO8601TimeEncoder,
		EncodeLevel:    zapcore.LowercaseLevelEncoder,
		EncodeDuration: zapcore.SecondsDurationEncoder,
	})
	core := zapcore.NewCore(encoder, zapcore.AddSync(&buf), zapcore.InfoLevel)
	logger := zap.New(core)
	auditLogger := NewProductionAuditLogger(logger)

	run := &agenticv1alpha1.AgenticRun{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-run",
			Namespace: "test-ns",
			UID:       types.UID("a1b2c3d4-e5f6-7890-1234-567890abcdef"),
		},
	}

	selectedOption := int32(1)
	approval := &agenticv1alpha1.AgenticRunApproval{
		Spec: agenticv1alpha1.AgenticRunApprovalSpec{
			Stages: []agenticv1alpha1.ApprovalStage{
				{
					Type:     agenticv1alpha1.ApprovalStageExecution,
					Decision: agenticv1alpha1.ApprovalDecisionApproved,
					Execution: &agenticv1alpha1.ExecutionApproval{
						Option: &selectedOption,
					},
				},
			},
		},
	}

	// When: Emit approval received
	auditLogger.EmitApprovalReceived(context.Background(), run, approval)

	// Then: JSON log has approval payload
	var logEntry map[string]interface{}
	if err := json.Unmarshal(buf.Bytes(), &logEntry); err != nil {
		t.Fatalf("Failed to parse JSON log: %v", err)
	}

	if logEntry["event"] != "audit.approval.received" {
		t.Errorf("Expected event='audit.approval.received', got %v", logEntry["event"])
	}

	payload := logEntry["payload"].(map[string]interface{})
	if _, ok := payload["approvalStages"]; !ok {
		t.Error("payload.approvalStages missing")
	}
	if payload["selectedOption"] != float64(1) {
		t.Errorf("Expected selectedOption=1, got %v", payload["selectedOption"])
	}
}

func TestAllSpanTypes(t *testing.T) {
	// Given: ProductionAuditLogger with lifecycle span and AgenticRunIDGenerator
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr), sdktrace.WithIDGenerator(&AgenticRunIDGenerator{}))
	otel.SetTracerProvider(tp)
	defer otel.SetTracerProvider(sdktrace.NewTracerProvider())

	logger := zap.NewNop()
	auditLogger := NewProductionAuditLogger(logger).(*ProductionAuditLogger)

	run := &agenticv1alpha1.AgenticRun{
		ObjectMeta: metav1.ObjectMeta{
			UID: types.UID("a1b2c3d4-e5f6-7890-1234-567890abcdef"),
		},
	}

	// Create lifecycle root span first
	auditLogger.EnsureLifecycleSpan(context.Background(), run)

	// When: Start all span types
	testCases := []struct {
		name         string
		startFunc    func(context.Context, *agenticv1alpha1.AgenticRun) (context.Context, trace.Span)
		expectedName string
	}{
		{"analysis", auditLogger.StartAnalysisSpan, "agenticrun.analyze"},
		{"execution", auditLogger.StartExecutionSpan, "agenticrun.execute"},
		{"verification", auditLogger.StartVerificationSpan, "agenticrun.verify"},
		{"escalation", auditLogger.StartEscalationSpan, "agenticrun.escalate"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			_, span := tc.startFunc(context.Background(), run)
			if span != nil {
				span.End()
			}
		})
	}

	// Then: All spans created with correct names (lifecycle + 4 phase spans)
	spans := sr.Ended()
	if len(spans) != 5 {
		t.Fatalf("Expected 5 spans (lifecycle + 4 phases), got %d", len(spans))
	}

	expectedNames := []string{"agenticrun.lifecycle", "agenticrun.analyze", "agenticrun.execute", "agenticrun.verify", "agenticrun.escalate"}
	for i, span := range spans {
		if span.Name() != expectedNames[i] {
			t.Errorf("Span %d: expected name %s, got %s", i, expectedNames[i], span.Name())
		}
	}

	// Verify all phase spans are children of lifecycle
	lifecycleSpanID := spans[0].SpanContext().SpanID()
	for _, childSpan := range spans[1:] {
		if childSpan.Parent().SpanID() != lifecycleSpanID {
			t.Errorf("Span %s should be child of lifecycle", childSpan.Name())
		}
	}
}

func TestAuditEventNames_AllEightMatchSpec(t *testing.T) {
	// Given: ProductionAuditLogger with in-memory buffer
	var buf bytes.Buffer
	encoder := zapcore.NewJSONEncoder(zapcore.EncoderConfig{
		TimeKey:        "timestamp",
		LevelKey:       "level",
		MessageKey:     "msg",
		EncodeTime:     zapcore.ISO8601TimeEncoder,
		EncodeLevel:    zapcore.LowercaseLevelEncoder,
		EncodeDuration: zapcore.SecondsDurationEncoder,
	})
	core := zapcore.NewCore(encoder, zapcore.AddSync(&buf), zapcore.InfoLevel)
	logger := zap.New(core)
	auditLogger := NewProductionAuditLogger(logger)

	now := metav1.Now()
	run := &agenticv1alpha1.AgenticRun{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "test-run",
			Namespace:         "test-ns",
			UID:               types.UID("a1b2c3d4-e5f6-7890-1234-567890abcdef"),
			CreationTimestamp: now,
		},
		Spec: agenticv1alpha1.AgenticRunSpec{
			Request: "test request",
		},
	}

	analysisResult := &agenticv1alpha1.AnalysisResult{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "test-analysis",
			Namespace:         "test-ns",
			UID:               types.UID("b2c3d4e5-f6a7-8901-2345-67890abcdef0"),
			CreationTimestamp: now,
		},
		Spec: agenticv1alpha1.AnalysisResultSpec{
			AgenticRunName: "test-run",
		},
	}

	selectedOption := int32(1)
	approval := &agenticv1alpha1.AgenticRunApproval{
		Spec: agenticv1alpha1.AgenticRunApprovalSpec{
			Stages: []agenticv1alpha1.ApprovalStage{
				{
					Type:     agenticv1alpha1.ApprovalStageExecution,
					Decision: agenticv1alpha1.ApprovalDecisionApproved,
					Execution: &agenticv1alpha1.ExecutionApproval{
						Option: &selectedOption,
					},
				},
			},
		},
	}

	executionResult := &agenticv1alpha1.ExecutionResult{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "test-execution",
			Namespace:         "test-ns",
			UID:               types.UID("c3d4e5f6-a7b8-9012-3456-7890abcdef01"),
			CreationTimestamp: now,
		},
		Spec: agenticv1alpha1.ExecutionResultSpec{
			AgenticRunName: "test-run",
		},
	}

	verificationResult := &agenticv1alpha1.VerificationResult{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "test-verification",
			Namespace:         "test-ns",
			UID:               types.UID("d4e5f6a7-b890-1234-5678-90abcdef0123"),
			CreationTimestamp: now,
		},
		Spec: agenticv1alpha1.VerificationResultSpec{
			AgenticRunName: "test-run",
		},
	}

	escalationResult := &agenticv1alpha1.EscalationResult{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "test-escalation",
			Namespace:         "test-ns",
			UID:               types.UID("e5f6a7b8-9012-3456-7890-abcdef012345"),
			CreationTimestamp: now,
		},
		Spec: agenticv1alpha1.EscalationResultSpec{
			AgenticRunName: "test-run",
		},
	}

	// When: Emit all 8 event types
	auditLogger.EnsureLifecycleSpan(context.Background(), run)
	auditLogger.EmitAgenticRunReceived(context.Background(), run)
	auditLogger.EmitAnalysisCompleted(context.Background(), run, analysisResult)
	auditLogger.EmitApprovalReceived(context.Background(), run, approval)
	auditLogger.EmitExecutionCompleted(context.Background(), run, executionResult)
	auditLogger.EmitVerificationCompleted(context.Background(), run, verificationResult)
	auditLogger.EmitVerificationRetry(context.Background(), run, verificationResult, 1)
	auditLogger.EmitEscalationCompleted(context.Background(), run, escalationResult)
	auditLogger.EmitAgenticRunTerminal(context.Background(), run, "Completed", "success")

	// Then: Parse each JSON line, verify event names match spec
	expectedEvents := []string{
		"audit.agenticrun.received",
		"audit.analysis.completed",
		"audit.approval.received",
		"audit.execution.completed",
		"audit.verification.completed",
		"audit.verification.retry",
		"audit.escalation.completed",
		"audit.agenticrun.terminal",
	}

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 8 {
		t.Fatalf("Expected 8 log lines, got %d", len(lines))
	}

	for i, line := range lines {
		var entry map[string]interface{}
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("Failed to parse line %d: %v", i, err)
		}

		eventName, ok := entry["event"].(string)
		if !ok {
			t.Fatalf("Line %d: event field missing or not a string", i)
		}

		if eventName != expectedEvents[i] {
			t.Errorf("Line %d: expected event=%s, got %s", i, expectedEvents[i], eventName)
		}
	}
}

func TestSpanServiceName_MatchesJaegerDisplayName(t *testing.T) {
	// Given: TracerProvider with service name resource
	sr := tracetest.NewSpanRecorder()
	res := resource.NewWithAttributes(
		semconv.SchemaURL,
		semconv.ServiceName("lightspeed-agentic-operator"),
		semconv.ServiceVersion("test"),
	)

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSpanProcessor(sr),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(tp)
	defer otel.SetTracerProvider(sdktrace.NewTracerProvider()) // Reset

	// When: Create ProductionAuditLogger and start a span
	logger := zap.NewNop()
	auditLogger := NewProductionAuditLogger(logger).(*ProductionAuditLogger)

	run := &agenticv1alpha1.AgenticRun{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-run",
			Namespace: "test-ns",
			UID:       types.UID("a1b2c3d4-e5f6-7890-1234-567890abcdef"),
		},
	}

	_, span := auditLogger.StartAnalysisSpan(context.Background(), run)
	if span != nil {
		span.End()
	}

	// Then: Span's resource contains service.name = "lightspeed-agentic-operator"
	spans := sr.Ended()
	if len(spans) != 1 {
		t.Fatalf("Expected 1 span, got %d", len(spans))
	}

	endedSpan := spans[0]
	resourceAttrs := endedSpan.Resource().Attributes()

	var serviceName string
	for _, attr := range resourceAttrs {
		if string(attr.Key) == "service.name" {
			serviceName = attr.Value.AsString()
			break
		}
	}

	if serviceName != "lightspeed-agentic-operator" {
		t.Errorf("Expected service.name='lightspeed-agentic-operator', got %s", serviceName)
	}
}

func TestSpanInstrumentationLibrary(t *testing.T) {
	// Given: TracerProvider with service name resource
	sr := tracetest.NewSpanRecorder()
	res := resource.NewWithAttributes(
		semconv.SchemaURL,
		semconv.ServiceName("lightspeed-agentic-operator"),
		semconv.ServiceVersion("test"),
	)

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSpanProcessor(sr),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(tp)
	defer otel.SetTracerProvider(sdktrace.NewTracerProvider()) // Reset

	// When: Create ProductionAuditLogger and start a span
	logger := zap.NewNop()
	auditLogger := NewProductionAuditLogger(logger).(*ProductionAuditLogger)

	run := &agenticv1alpha1.AgenticRun{
		ObjectMeta: metav1.ObjectMeta{
			UID: types.UID("a1b2c3d4-e5f6-7890-1234-567890abcdef"),
		},
	}

	_, span := auditLogger.StartAnalysisSpan(context.Background(), run)
	if span != nil {
		span.End()
	}

	// Then: Instrumentation library name and version match
	spans := sr.Ended()
	if len(spans) != 1 {
		t.Fatalf("Expected 1 span, got %d", len(spans))
	}

	endedSpan := spans[0]
	instrLib := endedSpan.InstrumentationLibrary()

	expectedName := "github.com/openshift/lightspeed-agentic-operator/controller/agenticrun"
	if instrLib.Name != expectedName {
		t.Errorf("Expected instrumentation library name=%s, got %s", expectedName, instrLib.Name)
	}

	expectedVersion := "v1alpha1"
	if instrLib.Version != expectedVersion {
		t.Errorf("Expected instrumentation library version=%s, got %s", expectedVersion, instrLib.Version)
	}
}

func TestFullLifecycleAuditTrail(t *testing.T) {
	// Given: ProductionAuditLogger with in-memory buffer and span recorder
	var buf bytes.Buffer
	encoder := zapcore.NewJSONEncoder(zapcore.EncoderConfig{
		TimeKey:        "timestamp",
		LevelKey:       "level",
		MessageKey:     "msg",
		EncodeTime:     zapcore.ISO8601TimeEncoder,
		EncodeLevel:    zapcore.LowercaseLevelEncoder,
		EncodeDuration: zapcore.SecondsDurationEncoder,
	})
	core := zapcore.NewCore(encoder, zapcore.AddSync(&buf), zapcore.InfoLevel)
	logger := zap.New(core)

	sr := tracetest.NewSpanRecorder()
	res := resource.NewWithAttributes(
		semconv.SchemaURL,
		semconv.ServiceName("lightspeed-agentic-operator"),
		semconv.ServiceVersion("test"),
	)

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSpanProcessor(sr),
		sdktrace.WithResource(res),
		sdktrace.WithIDGenerator(&AgenticRunIDGenerator{}),
	)
	otel.SetTracerProvider(tp)
	defer otel.SetTracerProvider(sdktrace.NewTracerProvider())

	auditLogger := NewProductionAuditLogger(logger).(*ProductionAuditLogger)

	now := metav1.Now()
	run := &agenticv1alpha1.AgenticRun{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "lifecycle-run",
			Namespace:         "test-ns",
			UID:               types.UID("a1b2c3d4-e5f6-7890-1234-567890abcdef"),
			CreationTimestamp: now,
		},
		Spec: agenticv1alpha1.AgenticRunSpec{
			Request: "lifecycle test",
		},
	}

	analysisResult := &agenticv1alpha1.AnalysisResult{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "lifecycle-analysis",
			Namespace:         "test-ns",
			UID:               types.UID("b2c3d4e5-f6a7-8901-2345-67890abcdef0"),
			CreationTimestamp: now,
		},
		Spec: agenticv1alpha1.AnalysisResultSpec{
			AgenticRunName: "lifecycle-run",
		},
	}

	selectedOption := int32(0)
	approval := &agenticv1alpha1.AgenticRunApproval{
		Spec: agenticv1alpha1.AgenticRunApprovalSpec{
			Stages: []agenticv1alpha1.ApprovalStage{
				{
					Type:     agenticv1alpha1.ApprovalStageExecution,
					Decision: agenticv1alpha1.ApprovalDecisionApproved,
					Execution: &agenticv1alpha1.ExecutionApproval{
						Option: &selectedOption,
					},
				},
			},
		},
	}

	executionResult := &agenticv1alpha1.ExecutionResult{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "lifecycle-execution",
			Namespace:         "test-ns",
			UID:               types.UID("c3d4e5f6-a7b8-9012-3456-7890abcdef01"),
			CreationTimestamp: now,
		},
		Spec: agenticv1alpha1.ExecutionResultSpec{
			AgenticRunName: "lifecycle-run",
		},
	}

	verificationResult := &agenticv1alpha1.VerificationResult{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "lifecycle-verification",
			Namespace:         "test-ns",
			UID:               types.UID("d4e5f6a7-b890-1234-5678-90abcdef0123"),
			CreationTimestamp: now,
		},
		Spec: agenticv1alpha1.VerificationResultSpec{
			AgenticRunName: "lifecycle-run",
		},
	}

	// When: Simulate full lifecycle
	// 1. EnsureLifecycleSpan (creates and immediately ends the root span)
	auditLogger.EnsureLifecycleSpan(context.Background(), run)

	// 2. EmitAgenticRunReceived
	auditLogger.EmitAgenticRunReceived(context.Background(), run)

	// 3. StartAnalysisSpan → EmitAnalysisCompleted → span.End()
	ctx, span := auditLogger.StartAnalysisSpan(context.Background(), run)
	auditLogger.EmitAnalysisCompleted(ctx, run, analysisResult)
	if span != nil {
		span.End()
	}

	// 4. EmitApprovalReceived
	auditLogger.EmitApprovalReceived(context.Background(), run, approval)

	// 5. StartExecutionSpan → EmitExecutionCompleted → span.End()
	ctx, span = auditLogger.StartExecutionSpan(context.Background(), run)
	auditLogger.EmitExecutionCompleted(ctx, run, executionResult)
	if span != nil {
		span.End()
	}

	// 6. StartVerificationSpan → EmitVerificationCompleted → span.End()
	ctx, span = auditLogger.StartVerificationSpan(context.Background(), run)
	auditLogger.EmitVerificationCompleted(ctx, run, verificationResult)
	if span != nil {
		span.End()
	}

	// 7. EmitAgenticRunTerminal
	auditLogger.EmitAgenticRunTerminal(context.Background(), run, "Completed", "success")

	// 8. EndLifecycleSpan (cleanup map entry)
	auditLogger.EndLifecycleSpan(run)

	// Then: Verify log lines
	expectedEvents := []string{
		"audit.agenticrun.received",
		"audit.analysis.completed",
		"audit.approval.received",
		"audit.execution.completed",
		"audit.verification.completed",
		"audit.agenticrun.terminal",
	}

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 6 {
		t.Fatalf("Expected 6 log lines, got %d", len(lines))
	}

	expectedTraceID := "a1b2c3d4e5f678901234567890abcdef"
	for i, line := range lines {
		var entry map[string]interface{}
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("Failed to parse line %d: %v", i, err)
		}

		eventName := entry["event"].(string)
		if eventName != expectedEvents[i] {
			t.Errorf("Line %d: expected event=%s, got %s", i, expectedEvents[i], eventName)
		}

		traceID := entry["trace_id"].(string)
		if traceID != expectedTraceID {
			t.Errorf("Line %d: expected trace_id=%s, got %s", i, expectedTraceID, traceID)
		}
	}

	// Then: Verify spans (lifecycle + analyze + execute + verify + terminal)
	spans := sr.Ended()
	if len(spans) != 5 {
		t.Fatalf("Expected 5 spans, got %d", len(spans))
	}

	expectedSpanNames := []string{"agenticrun.lifecycle", "agenticrun.analyze", "agenticrun.execute", "agenticrun.verify", "agenticrun.terminal"}
	for i, endedSpan := range spans {
		if endedSpan.Name() != expectedSpanNames[i] {
			t.Errorf("Span %d: expected name=%s, got %s", i, expectedSpanNames[i], endedSpan.Name())
		}

		spanTraceID := endedSpan.SpanContext().TraceID().String()
		if spanTraceID != expectedTraceID {
			t.Errorf("Span %d: expected trace_id=%s, got %s", i, expectedTraceID, spanTraceID)
		}
	}

	// Verify child spans are parented under lifecycle
	lifecycleSpanID := spans[0].SpanContext().SpanID()
	for _, childSpan := range spans[1:] {
		if childSpan.Parent().SpanID() != lifecycleSpanID {
			t.Errorf("Span %s: expected parent span ID %s, got %s",
				childSpan.Name(), lifecycleSpanID, childSpan.Parent().SpanID())
		}
	}
}

func TestAuditConfigScenarios(t *testing.T) {
	t.Run("NoOpAuditLogger_NoOutput", func(t *testing.T) {
		// Given: NoOpAuditLogger with a buffer to verify no output
		var buf bytes.Buffer
		encoder := zapcore.NewJSONEncoder(zapcore.EncoderConfig{
			TimeKey:    "timestamp",
			LevelKey:   "level",
			MessageKey: "msg",
		})
		core := zapcore.NewCore(encoder, zapcore.AddSync(&buf), zapcore.InfoLevel)
		logger := zap.New(core)

		// Use NoOpAuditLogger (not the production logger)
		auditLogger := NewNoOpAuditLogger()

		now := metav1.Now()
		run := &agenticv1alpha1.AgenticRun{
			ObjectMeta: metav1.ObjectMeta{
				Name:              "noop-test",
				Namespace:         "test-ns",
				UID:               types.UID("a1b2c3d4-e5f6-7890-1234-567890abcdef"),
				CreationTimestamp: now,
			},
		}

		result := &agenticv1alpha1.AnalysisResult{
			ObjectMeta: metav1.ObjectMeta{
				Name:              "noop-result",
				Namespace:         "test-ns",
				UID:               types.UID("b2c3d4e5-f6a7-8901-2345-67890abcdef0"),
				CreationTimestamp: now,
			},
		}

		// When: Emit all events
		auditLogger.EmitAgenticRunReceived(context.Background(), run)
		auditLogger.EmitAnalysisCompleted(context.Background(), run, result)
		auditLogger.EmitAgenticRunTerminal(context.Background(), run, "Completed", "success")

		// Then: Buffer should be empty (no output produced)
		if buf.Len() != 0 {
			t.Errorf("NoOpAuditLogger should produce no output, but got %d bytes: %s", buf.Len(), buf.String())
		}

		// Verify logger still works (to ensure we're testing NoOp, not broken logger)
		logger.Info("control-test")
		if buf.Len() == 0 {
			t.Error("Control test failed: logger itself is not working")
		}
	})

	t.Run("ProductionAuditLogger_ProducesOutput", func(t *testing.T) {
		// Given: ProductionAuditLogger with buffer
		var buf bytes.Buffer
		encoder := zapcore.NewJSONEncoder(zapcore.EncoderConfig{
			TimeKey:        "timestamp",
			LevelKey:       "level",
			MessageKey:     "msg",
			EncodeTime:     zapcore.ISO8601TimeEncoder,
			EncodeLevel:    zapcore.LowercaseLevelEncoder,
			EncodeDuration: zapcore.SecondsDurationEncoder,
		})
		core := zapcore.NewCore(encoder, zapcore.AddSync(&buf), zapcore.InfoLevel)
		logger := zap.New(core)
		auditLogger := NewProductionAuditLogger(logger)

		now := metav1.Now()
		run := &agenticv1alpha1.AgenticRun{
			ObjectMeta: metav1.ObjectMeta{
				Name:              "prod-test",
				Namespace:         "test-ns",
				UID:               types.UID("a1b2c3d4-e5f6-7890-1234-567890abcdef"),
				CreationTimestamp: now,
			},
		}

		// When: Emit one event
		auditLogger.EmitAgenticRunReceived(context.Background(), run)

		// Then: Output IS produced
		if buf.Len() == 0 {
			t.Error("ProductionAuditLogger should produce output, but buffer is empty")
		}

		var entry map[string]interface{}
		if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
			t.Fatalf("Failed to parse JSON output: %v", err)
		}

		if entry["event"] != "audit.agenticrun.received" {
			t.Errorf("Expected event='audit.agenticrun.received', got %v", entry["event"])
		}
	})
}

func TestAnalysisSpan_ContainsResultStatusFields(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr), sdktrace.WithIDGenerator(&AgenticRunIDGenerator{}))
	otel.SetTracerProvider(tp)
	defer otel.SetTracerProvider(sdktrace.NewTracerProvider())

	logger := zap.NewNop()
	auditLogger := NewProductionAuditLogger(logger).(*ProductionAuditLogger)

	run := &agenticv1alpha1.AgenticRun{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-run",
			Namespace: "test-ns",
			UID:       types.UID("a1b2c3d4-e5f6-7890-1234-567890abcdef"),
		},
	}

	analysisResult := &agenticv1alpha1.AnalysisResult{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-run-analysis-1",
			Namespace: "test-ns",
			UID:       types.UID("b2c3d4e5-f6a7-8901-2345-67890abcdef0"),
		},
		Status: agenticv1alpha1.AnalysisResultStatus{
			Options: []agenticv1alpha1.RemediationOption{
				{
					Title: "Increase memory limit to 512Mi",
					RemediationPlan: agenticv1alpha1.RemediationPlan{
						Risk: agenticv1alpha1.RiskLevelLow,
					},
				},
				{
					Title: "Restart with exponential backoff",
					RemediationPlan: agenticv1alpha1.RemediationPlan{
						Risk: agenticv1alpha1.RiskLevelMedium,
					},
				},
			},
		},
	}

	auditLogger.EnsureLifecycleSpan(context.Background(), run)
	ctx, span := auditLogger.StartAnalysisSpan(context.Background(), run)
	auditLogger.EmitAnalysisCompleted(ctx, run, analysisResult)
	span.End()

	spans := sr.Ended()
	var analysisSpan sdktrace.ReadOnlySpan
	for _, s := range spans {
		if s.Name() == "agenticrun.analyze" {
			analysisSpan = s
			break
		}
	}
	if analysisSpan == nil {
		t.Fatal("agenticrun.analyze span not found")
	}

	events := analysisSpan.Events()
	var completedEvent *sdktrace.Event
	for i := range events {
		if events[i].Name == "audit.analysis.completed" {
			completedEvent = &events[i]
			break
		}
	}
	if completedEvent == nil {
		t.Fatal("audit.analysis.completed event not found on analysis span")
	}

	attrMap := make(map[string]string)
	for _, a := range completedEvent.Attributes {
		attrMap[string(a.Key)] = a.Value.Emit()
	}

	checks := map[string]string{
		"agenticrun.name": "test-run",
		"result.name":     "test-run-analysis-1",
		"result.uid":      "b2c3d4e5-f6a7-8901-2345-67890abcdef0",
		"options.count":   "2",
		"option.0.title":  "Increase memory limit to 512Mi",
		"option.0.risk":   "Low",
		"option.1.title":  "Restart with exponential backoff",
		"option.1.risk":   "Medium",
	}
	for key, want := range checks {
		got, ok := attrMap[key]
		if !ok {
			t.Errorf("missing attribute %q on analysis completed event", key)
		} else if got != want {
			t.Errorf("attribute %q = %q, want %q", key, got, want)
		}
	}
}

func TestAnalysisSpan_ReconcilerFlowPopulatesStatusFields(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr), sdktrace.WithIDGenerator(&AgenticRunIDGenerator{}))
	otel.SetTracerProvider(tp)
	defer otel.SetTracerProvider(sdktrace.NewTracerProvider())

	auditLogger := NewProductionAuditLogger(zap.NewNop()).(*ProductionAuditLogger)

	run := testAgenticRun()
	scheme := testScheme()
	objs := []client.Object{run, testDefaultAgent(), testLLM("smart"), testAutoApprovePolicy()}
	fc := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(run, &agenticv1alpha1.AnalysisResult{}).Build()

	r := &AgenticRunReconciler{
		Client:    fc,
		Agent:     newTestAgentCaller(),
		Namespace: "default",
		Audit:     auditLogger,
	}

	if _, err := reconcileOnce(r, "fix-crash"); err != nil {
		t.Fatalf("analysis reconcile: %v", err)
	}

	var analysisSpan sdktrace.ReadOnlySpan
	for _, s := range sr.Ended() {
		if s.Name() == "agenticrun.analyze" {
			analysisSpan = s
			break
		}
	}
	if analysisSpan == nil {
		t.Fatal("agenticrun.analyze span not found after reconcile")
	}

	var completedEvent *sdktrace.Event
	for i := range analysisSpan.Events() {
		if analysisSpan.Events()[i].Name == "audit.analysis.completed" {
			completedEvent = &analysisSpan.Events()[i]
			break
		}
	}
	if completedEvent == nil {
		t.Fatal("audit.analysis.completed event not found on analysis span")
	}

	attrMap := make(map[string]string)
	for _, a := range completedEvent.Attributes {
		attrMap[string(a.Key)] = a.Value.Emit()
	}

	if got := attrMap["options.count"]; got != "1" {
		t.Errorf("options.count = %q, want %q (stub agent returns 1 option)", got, "1")
	}
	if got := attrMap["option.0.title"]; got != "Stub remediation" {
		t.Errorf("option.0.title = %q, want %q", got, "Stub remediation")
	}
	if got := attrMap["option.0.risk"]; got != "Low" {
		t.Errorf("option.0.risk = %q, want %q", got, "Low")
	}
}

func TestTerminalReason(t *testing.T) {
	tests := []struct {
		name       string
		conditions []metav1.Condition
		want       string
	}{
		{
			name: "failed_step",
			conditions: []metav1.Condition{
				{Type: "Analyzed", Status: metav1.ConditionFalse, Reason: "Failed", Message: "LLM timeout"},
			},
			want: "LLM timeout",
		},
		{
			name: "user_denied",
			conditions: []metav1.Condition{
				{Type: "Denied", Status: metav1.ConditionTrue, Reason: "UserDenied", Message: "Execution denied by user"},
			},
			want: "Execution denied by user",
		},
		{
			name: "system_suspended",
			conditions: []metav1.Condition{
				{Type: "EmergencyStopped", Status: metav1.ConditionTrue, Reason: "SystemSuspended", Message: "Terminated by system kill switch"},
			},
			want: "Terminated by system kill switch",
		},
		{
			name: "completed_no_reason",
			conditions: []metav1.Condition{
				{Type: "Verified", Status: metav1.ConditionTrue, Reason: "Passed", Message: "All checks passed"},
			},
			want: "",
		},
		{
			name:       "no_conditions",
			conditions: nil,
			want:       "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			run := &agenticv1alpha1.AgenticRun{}
			run.Status.Conditions = tt.conditions
			got := terminalReason(run)
			if got != tt.want {
				t.Errorf("terminalReason() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestEndLifecycleSpan_ReturnsBool(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr), sdktrace.WithIDGenerator(&AgenticRunIDGenerator{}))
	otel.SetTracerProvider(tp)
	defer otel.SetTracerProvider(sdktrace.NewTracerProvider())

	auditLogger := NewProductionAuditLogger(zap.NewNop()).(*ProductionAuditLogger)
	run := &agenticv1alpha1.AgenticRun{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test", Namespace: "test-ns",
			UID: types.UID("a1b2c3d4-e5f6-7890-1234-567890abcdef"),
		},
	}

	auditLogger.EnsureLifecycleSpan(context.Background(), run)

	if !auditLogger.EndLifecycleSpan(run) {
		t.Error("first EndLifecycleSpan should return true")
	}
	if auditLogger.EndLifecycleSpan(run) {
		t.Error("second EndLifecycleSpan should return false (already cleaned up)")
	}
}

func TestIsTerminal_IncludesFailed(t *testing.T) {
	terminals := []agenticv1alpha1.AgenticRunPhase{
		agenticv1alpha1.AgenticRunPhaseCompleted,
		agenticv1alpha1.AgenticRunPhaseFailed,
		agenticv1alpha1.AgenticRunPhaseDenied,
		agenticv1alpha1.AgenticRunPhaseEscalated,
		agenticv1alpha1.AgenticRunPhaseEmergencyStopped,
	}
	for _, phase := range terminals {
		if !isTerminal(phase) {
			t.Errorf("isTerminal(%s) should be true", phase)
		}
	}

	nonTerminals := []agenticv1alpha1.AgenticRunPhase{
		agenticv1alpha1.AgenticRunPhasePending,
		agenticv1alpha1.AgenticRunPhaseAnalyzing,
		agenticv1alpha1.AgenticRunPhaseProposed,
		agenticv1alpha1.AgenticRunPhaseExecuting,
		agenticv1alpha1.AgenticRunPhaseVerifying,
		agenticv1alpha1.AgenticRunPhaseEscalating,
	}
	for _, phase := range nonTerminals {
		if isTerminal(phase) {
			t.Errorf("isTerminal(%s) should be false", phase)
		}
	}
}

func TestNoApprovalSpan_AutoApproveExecution(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr), sdktrace.WithIDGenerator(&AgenticRunIDGenerator{}))
	otel.SetTracerProvider(tp)
	defer otel.SetTracerProvider(sdktrace.NewTracerProvider())

	auditLogger := NewProductionAuditLogger(zap.NewNop()).(*ProductionAuditLogger)
	run := &agenticv1alpha1.AgenticRun{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test", Namespace: "test-ns",
			UID: types.UID("a1b2c3d4-e5f6-7890-1234-567890abcdef"),
		},
	}

	auditLogger.EnsureLifecycleSpan(context.Background(), run)

	_, span := auditLogger.StartAnalysisSpan(context.Background(), run)
	if span != nil {
		span.End()
	}

	// Simulate auto-approve: do NOT call StartApprovalWait

	_, span = auditLogger.StartExecutionSpan(context.Background(), run)
	if span != nil {
		span.End()
	}

	auditLogger.EndLifecycleSpan(run)

	spans := sr.Ended()
	for _, s := range spans {
		if s.Name() == "agenticrun.human_approval" {
			t.Error("human_approval span should not exist when execution is auto-approved")
		}
	}
}

func TestApprovalSpan_ManualApproveExecution(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr), sdktrace.WithIDGenerator(&AgenticRunIDGenerator{}))
	otel.SetTracerProvider(tp)
	defer otel.SetTracerProvider(sdktrace.NewTracerProvider())

	auditLogger := NewProductionAuditLogger(zap.NewNop()).(*ProductionAuditLogger)
	run := &agenticv1alpha1.AgenticRun{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test", Namespace: "test-ns",
			UID: types.UID("a1b2c3d4-e5f6-7890-1234-567890abcdef"),
		},
	}

	auditLogger.EnsureLifecycleSpan(context.Background(), run)

	_, span := auditLogger.StartAnalysisSpan(context.Background(), run)
	if span != nil {
		span.End()
	}

	// Simulate manual approval: start and end approval wait
	auditLogger.StartApprovalWait(context.Background(), run)
	auditLogger.EndApprovalWait(run, nil)

	_, span = auditLogger.StartExecutionSpan(context.Background(), run)
	if span != nil {
		span.End()
	}

	auditLogger.EndLifecycleSpan(run)

	found := false
	for _, s := range sr.Ended() {
		if s.Name() == "agenticrun.human_approval" {
			found = true
			break
		}
	}
	if !found {
		t.Error("human_approval span should exist when execution requires manual approval")
	}
}

func TestApprovalWait_BackdatesStartOnRestart(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr), sdktrace.WithIDGenerator(&AgenticRunIDGenerator{}))
	otel.SetTracerProvider(tp)
	defer otel.SetTracerProvider(sdktrace.NewTracerProvider())

	auditLogger := NewProductionAuditLogger(zap.NewNop()).(*ProductionAuditLogger)
	analysisTime := time.Date(2025, 6, 1, 10, 0, 0, 0, time.UTC)
	run := &agenticv1alpha1.AgenticRun{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test", Namespace: "test-ns",
			UID: types.UID("a1b2c3d4-e5f6-7890-1234-567890abcdef"),
		},
		Status: agenticv1alpha1.AgenticRunStatus{
			Conditions: []metav1.Condition{
				{
					Type:               agenticv1alpha1.AgenticRunConditionAnalyzed,
					Status:             metav1.ConditionTrue,
					Reason:             "Analyzed",
					LastTransitionTime: metav1.NewTime(analysisTime),
				},
			},
		},
	}

	auditLogger.RecoverLifecycleContext(context.Background(), run)
	auditLogger.StartApprovalWait(context.Background(), run)
	auditLogger.EndApprovalWait(run, nil)

	for _, s := range sr.Ended() {
		if s.Name() == "agenticrun.human_approval" {
			if !s.StartTime().Equal(analysisTime) {
				t.Errorf("expected approval span start = %v, got %v", analysisTime, s.StartTime())
			}
			return
		}
	}
	t.Error("human_approval span not found")
}

func TestRepeatedTerminalReconcile_NoDuplicateLog(t *testing.T) {
	var buf bytes.Buffer
	encoder := zapcore.NewJSONEncoder(zapcore.EncoderConfig{
		TimeKey:    "timestamp",
		LevelKey:   "level",
		MessageKey: "msg",
		EncodeTime: zapcore.ISO8601TimeEncoder,
	})
	core := zapcore.NewCore(encoder, zapcore.AddSync(&buf), zapcore.InfoLevel)

	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr), sdktrace.WithIDGenerator(&AgenticRunIDGenerator{}))
	otel.SetTracerProvider(tp)
	defer otel.SetTracerProvider(sdktrace.NewTracerProvider())

	auditLogger := NewProductionAuditLogger(zap.New(core)).(*ProductionAuditLogger)
	run := &agenticv1alpha1.AgenticRun{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test", Namespace: "test-ns",
			UID: types.UID("a1b2c3d4-e5f6-7890-1234-567890abcdef"),
		},
	}

	auditLogger.EnsureLifecycleSpan(context.Background(), run)

	// Simulate two terminal reconciles (as happens with watch-triggered re-reconcile).
	// EmitAgenticRunTerminal must be called before EndLifecycleSpan (which deletes the map entry).
	// EmitAgenticRunTerminal gates internally: skips if no lifecycle entry exists.
	for i := 0; i < 3; i++ {
		auditLogger.EndApprovalWait(run, nil)
		auditLogger.EmitAgenticRunTerminal(context.Background(), run, "Failed", "")
		auditLogger.EndLifecycleSpan(run)
	}

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	terminalCount := 0
	for _, line := range lines {
		var entry map[string]interface{}
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			continue
		}
		if entry["event"] == "audit.agenticrun.terminal" {
			terminalCount++
		}
	}
	if terminalCount != 1 {
		t.Errorf("expected exactly 1 terminal log, got %d", terminalCount)
	}

	lifecycleCount := 0
	for _, s := range sr.Ended() {
		if s.Name() == "agenticrun.lifecycle" {
			lifecycleCount++
		}
	}
	if lifecycleCount != 1 {
		t.Errorf("expected exactly 1 lifecycle span, got %d", lifecycleCount)
	}
}
