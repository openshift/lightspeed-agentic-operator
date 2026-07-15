package agenticrun

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.24.0"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
)

func TestSerializeCR_AgenticRun(t *testing.T) {
	run := &agenticv1alpha1.AgenticRun{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "test-run",
			Namespace:         "test-ns",
			UID:               types.UID("a1b2c3d4-e5f6-7890-1234-567890abcdef"),
			CreationTimestamp: metav1.Now(),
			Annotations:       map[string]string{"extra": "should-not-appear"},
		},
		Spec: agenticv1alpha1.AgenticRunSpec{Request: "test request"},
	}

	serialized, err := serializeCR(run)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

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
	if len(metadata) != 4 {
		t.Errorf("Expected exactly 4 metadata fields, got %d", len(metadata))
	}
	if _, ok := serialized["spec"]; !ok {
		t.Error("spec field missing")
	}
}

func TestSerializeCR_AnalysisResult(t *testing.T) {
	result := &agenticv1alpha1.AnalysisResult{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "test-result",
			Namespace:         "test-ns",
			UID:               types.UID("b2c3d4e5-f6a7-8901-2345-67890abcdef0"),
			CreationTimestamp: metav1.Now(),
		},
		Spec: agenticv1alpha1.AnalysisResultSpec{AgenticRunName: "test-run"},
		Status: agenticv1alpha1.AnalysisResultStatus{
			Conditions: []metav1.Condition{{Type: "Completed", Status: metav1.ConditionTrue}},
		},
	}

	serialized, err := serializeCR(result)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if _, ok := serialized["spec"]; !ok {
		t.Error("spec field missing")
	}
	status, ok := serialized["status"].(map[string]interface{})
	if !ok {
		t.Fatal("status field missing or wrong type")
	}
	if _, ok := status["conditions"]; !ok {
		t.Error("status.conditions missing")
	}
}

func setupRecorder(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { otel.SetTracerProvider(sdktrace.NewTracerProvider()) })
	return sr
}

func testRun() *agenticv1alpha1.AgenticRun {
	return &agenticv1alpha1.AgenticRun{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "test-run",
			Namespace:         "test-ns",
			UID:               types.UID("a1b2c3d4-e5f6-7890-1234-567890abcdef"),
			CreationTimestamp: metav1.Now(),
		},
		Spec: agenticv1alpha1.AgenticRunSpec{Request: "test request"},
	}
}

func TestNoOpAuditLogger_NoPanic(t *testing.T) {
	logger := NewNoOpAuditLogger()
	run := testRun()

	logger.EmitAgenticRunReceived(context.Background(), run)
	logger.EmitAnalysisCompleted(context.Background(), run, nil)
	logger.EmitExecutionCompleted(context.Background(), run, nil)
	logger.EmitVerificationCompleted(context.Background(), run, nil)
	logger.EmitVerificationRetry(context.Background(), run, nil, 1)
	logger.EmitEscalationCompleted(context.Background(), run, nil)
	logger.EmitApprovalSpan(context.Background(), run, nil, "")
	logger.EmitTerminalSpan(context.Background(), run, "Completed", "success")
	logger.InjectTraceContext(context.Background(), run, http.Header{})
	logger.Cleanup(run)

	ctx, span := logger.StartAnalysisSpan(context.Background(), run)
	if ctx == nil {
		t.Error("StartAnalysisSpan should return non-nil ctx")
	}
	span.End()
}

func TestStartPhaseSpan_IndependentTraces(t *testing.T) {
	sr := setupRecorder(t)
	auditLogger := NewProductionAuditLogger().(*ProductionAuditLogger)
	run := testRun()

	_, s1 := auditLogger.StartAnalysisSpan(context.Background(), run)
	s1.End()

	_, s2 := auditLogger.StartExecutionSpan(context.Background(), run)
	s2.End()

	spans := sr.Ended()
	if len(spans) != 2 {
		t.Fatalf("Expected 2 spans, got %d", len(spans))
	}

	if spans[0].SpanContext().TraceID() == spans[1].SpanContext().TraceID() {
		t.Error("Each phase should have its own trace ID, but they match")
	}
}

func TestStartPhaseSpan_SpanLinks(t *testing.T) {
	sr := setupRecorder(t)
	auditLogger := NewProductionAuditLogger().(*ProductionAuditLogger)
	run := testRun()

	_, s1 := auditLogger.StartAnalysisSpan(context.Background(), run)
	s1.End()
	analysisSpanCtx := s1.(sdktrace.ReadOnlySpan).SpanContext()

	_, s2 := auditLogger.StartExecutionSpan(context.Background(), run)
	s2.End()

	spans := sr.Ended()
	if len(spans) != 2 {
		t.Fatalf("Expected 2 spans, got %d", len(spans))
	}

	analysisSpan := spans[0]
	execSpan := spans[1]

	if len(analysisSpan.Links()) != 0 {
		t.Error("First phase (analysis) should have no span links")
	}

	if len(execSpan.Links()) != 1 {
		t.Fatalf("Execution span should have 1 link, got %d", len(execSpan.Links()))
	}
	link := execSpan.Links()[0]
	if link.SpanContext.TraceID() != analysisSpanCtx.TraceID() {
		t.Errorf("Link should point to analysis trace %s, got %s", analysisSpanCtx.TraceID(), link.SpanContext.TraceID())
	}
	if link.SpanContext.SpanID() != analysisSpanCtx.SpanID() {
		t.Errorf("Link should point to analysis span %s, got %s", analysisSpanCtx.SpanID(), link.SpanContext.SpanID())
	}
}

func TestStartPhaseSpan_StandardAttributes(t *testing.T) {
	sr := setupRecorder(t)
	auditLogger := NewProductionAuditLogger().(*ProductionAuditLogger)
	run := testRun()

	_, span := auditLogger.StartAnalysisSpan(context.Background(), run)
	span.End()

	spans := sr.Ended()
	if len(spans) != 1 {
		t.Fatalf("Expected 1 span, got %d", len(spans))
	}

	attrMap := make(map[string]string)
	for _, a := range spans[0].Attributes() {
		attrMap[string(a.Key)] = a.Value.Emit()
	}

	checks := map[string]string{
		"agenticrun.uid":       strings.ReplaceAll(string(run.UID), "-", ""),
		"agenticrun.name":      "test-run",
		"agenticrun.namespace": "test-ns",
	}
	for key, want := range checks {
		if got, ok := attrMap[key]; !ok {
			t.Errorf("missing attribute %q", key)
		} else if got != want {
			t.Errorf("attribute %q = %q, want %q", key, got, want)
		}
	}
}

func TestStartPhaseSpan_KindInternal(t *testing.T) {
	sr := setupRecorder(t)
	auditLogger := NewProductionAuditLogger().(*ProductionAuditLogger)
	run := testRun()

	_, span := auditLogger.StartAnalysisSpan(context.Background(), run)
	span.End()

	spans := sr.Ended()
	if spans[0].SpanKind() != trace.SpanKindInternal {
		t.Errorf("Expected SpanKindInternal, got %v", spans[0].SpanKind())
	}
}

func TestAllPhaseSpanNames(t *testing.T) {
	sr := setupRecorder(t)
	auditLogger := NewProductionAuditLogger().(*ProductionAuditLogger)
	run := testRun()

	tests := []struct {
		name         string
		startFunc    func(context.Context, *agenticv1alpha1.AgenticRun) (context.Context, trace.Span)
		expectedName string
	}{
		{"analysis", auditLogger.StartAnalysisSpan, "agenticrun.analyze"},
		{"execution", auditLogger.StartExecutionSpan, "agenticrun.execute"},
		{"verification", auditLogger.StartVerificationSpan, "agenticrun.verify"},
		{"escalation", auditLogger.StartEscalationSpan, "agenticrun.escalate"},
	}

	for _, tc := range tests {
		_, span := tc.startFunc(context.Background(), run)
		span.End()
	}

	spans := sr.Ended()
	if len(spans) != 4 {
		t.Fatalf("Expected 4 spans, got %d", len(spans))
	}

	expectedNames := []string{"agenticrun.analyze", "agenticrun.execute", "agenticrun.verify", "agenticrun.escalate"}
	for i, span := range spans {
		if span.Name() != expectedNames[i] {
			t.Errorf("Span %d: expected name %s, got %s", i, expectedNames[i], span.Name())
		}
	}
}

func TestEmitApprovalSpan_ShortLived(t *testing.T) {
	sr := setupRecorder(t)
	auditLogger := NewProductionAuditLogger().(*ProductionAuditLogger)
	run := testRun()

	selectedOption := int32(1)
	approval := &agenticv1alpha1.AgenticRunApproval{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-approval",
			Namespace: "test-ns",
			UID:       types.UID("c3d4e5f6-a7b8-9012-3456-7890abcdef01"),
		},
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
			Approver: agenticv1alpha1.ApproverInfo{
				UID:      "user-123",
				Username: "admin",
			},
		},
	}

	auditLogger.EmitApprovalSpan(context.Background(), run, approval, "Restart failing pods")

	spans := sr.Ended()
	if len(spans) != 1 {
		t.Fatalf("Expected 1 span, got %d", len(spans))
	}
	if spans[0].Name() != "agenticrun.human_approval" {
		t.Errorf("Expected span name 'agenticrun.human_approval', got %s", spans[0].Name())
	}

	events := spans[0].Events()
	if len(events) != 1 {
		t.Fatalf("Expected 1 event, got %d", len(events))
	}
	if events[0].Name != "agenticrun.approval.completed" {
		t.Errorf("Expected event name 'agenticrun.approval.completed', got %s", events[0].Name)
	}

	attrMap := make(map[string]string)
	for _, a := range events[0].Attributes {
		attrMap[string(a.Key)] = a.Value.Emit()
	}
	if attrMap["approval.decision"] != string(agenticv1alpha1.ApprovalDecisionApproved) {
		t.Errorf("Expected decision='Approved', got %q", attrMap["approval.decision"])
	}
	if attrMap["selected_option"] != "1" {
		t.Errorf("Expected selected_option=1, got %q", attrMap["selected_option"])
	}
	if attrMap["approver.uid"] != "user-123" {
		t.Errorf("Expected approver.uid='user-123', got %q", attrMap["approver.uid"])
	}
	if attrMap["selected_option.title"] != "Restart failing pods" {
		t.Errorf("Expected selected_option.title='Restart failing pods', got %q", attrMap["selected_option.title"])
	}
}

func TestEmitApprovalSpan_IdempotentOnRetry(t *testing.T) {
	sr := setupRecorder(t)
	auditLogger := NewProductionAuditLogger().(*ProductionAuditLogger)
	run := testRun()

	auditLogger.EmitApprovalSpan(context.Background(), run, nil, "")
	auditLogger.EmitApprovalSpan(context.Background(), run, nil, "")

	spans := sr.Ended()
	if len(spans) != 1 {
		t.Fatalf("Expected exactly 1 approval span (idempotent), got %d", len(spans))
	}
}

func TestEmitTerminalSpan_ShortLived(t *testing.T) {
	sr := setupRecorder(t)
	auditLogger := NewProductionAuditLogger().(*ProductionAuditLogger)
	run := testRun()

	auditLogger.EmitTerminalSpan(context.Background(), run, "Completed", "all checks passed")

	spans := sr.Ended()
	if len(spans) != 1 {
		t.Fatalf("Expected 1 span, got %d", len(spans))
	}
	if spans[0].Name() != "agenticrun.terminal" {
		t.Errorf("Expected span name 'agenticrun.terminal', got %s", spans[0].Name())
	}

	attrMap := make(map[string]string)
	for _, a := range spans[0].Attributes() {
		attrMap[string(a.Key)] = a.Value.Emit()
	}
	if attrMap["phase"] != "Completed" {
		t.Errorf("Expected phase='Completed', got %q", attrMap["phase"])
	}
	if attrMap["reason"] != "all checks passed" {
		t.Errorf("Expected reason='all checks passed', got %q", attrMap["reason"])
	}
}

func TestEmitTerminalSpan_IdempotentOnReReconcile(t *testing.T) {
	sr := setupRecorder(t)
	auditLogger := NewProductionAuditLogger().(*ProductionAuditLogger)
	run := testRun()

	auditLogger.EmitTerminalSpan(context.Background(), run, "Completed", "success")
	auditLogger.EmitTerminalSpan(context.Background(), run, "Completed", "success")

	spans := sr.Ended()
	if len(spans) != 1 {
		t.Fatalf("Expected exactly 1 terminal span (idempotent), got %d", len(spans))
	}
}

func TestEmitAgenticRunReceived_OnPhaseSpan(t *testing.T) {
	sr := setupRecorder(t)
	auditLogger := NewProductionAuditLogger().(*ProductionAuditLogger)
	run := testRun()

	spanCtx, span := auditLogger.StartAnalysisSpan(context.Background(), run)
	auditLogger.EmitAgenticRunReceived(spanCtx, run)
	span.End()

	spans := sr.Ended()
	if len(spans) != 1 {
		t.Fatalf("Expected 1 span, got %d", len(spans))
	}

	events := spans[0].Events()
	if len(events) != 1 {
		t.Fatalf("Expected 1 event, got %d", len(events))
	}
	if events[0].Name != "agenticrun.received" {
		t.Errorf("Expected event 'agenticrun.received', got %s", events[0].Name)
	}

	attrMap := make(map[string]string)
	for _, a := range events[0].Attributes {
		attrMap[string(a.Key)] = a.Value.Emit()
	}
	if attrMap["agenticrun.name"] != "test-run" {
		t.Errorf("Expected name='test-run', got %q", attrMap["agenticrun.name"])
	}
	if _, ok := attrMap["agenticrun.cr"]; !ok {
		t.Error("agenticrun.cr attribute missing on received event")
	}
}

func TestEmitAnalysisCompleted_EventAttributes(t *testing.T) {
	sr := setupRecorder(t)
	auditLogger := NewProductionAuditLogger().(*ProductionAuditLogger)
	run := testRun()

	analysisResult := &agenticv1alpha1.AnalysisResult{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-analysis", Namespace: "test-ns",
			UID: types.UID("b2c3d4e5-f6a7-8901-2345-67890abcdef0"),
		},
		Status: agenticv1alpha1.AnalysisResultStatus{
			Options: []agenticv1alpha1.RemediationOption{
				{Title: "Increase memory", RemediationPlan: agenticv1alpha1.RemediationPlan{Risk: agenticv1alpha1.RiskLevelLow}},
				{Title: "Restart pod", RemediationPlan: agenticv1alpha1.RemediationPlan{Risk: agenticv1alpha1.RiskLevelMedium}},
			},
		},
	}

	spanCtx, span := auditLogger.StartAnalysisSpan(context.Background(), run)
	auditLogger.EmitAnalysisCompleted(spanCtx, run, analysisResult)
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

	var completedEvent *sdktrace.Event
	for i := range analysisSpan.Events() {
		if analysisSpan.Events()[i].Name == "agenticrun.analysis.completed" {
			completedEvent = &analysisSpan.Events()[i]
			break
		}
	}
	if completedEvent == nil {
		t.Fatal("agenticrun.analysis.completed event not found")
	}

	attrMap := make(map[string]string)
	for _, a := range completedEvent.Attributes {
		attrMap[string(a.Key)] = a.Value.Emit()
	}

	checks := map[string]string{
		"agenticrun.name": "test-run",
		"result.name":     "test-analysis",
		"options.count":   "2",
		"option.0.title":  "Increase memory",
		"option.0.risk":   "Low",
		"option.1.title":  "Restart pod",
		"option.1.risk":   "Medium",
	}
	for key, want := range checks {
		if got, ok := attrMap[key]; !ok {
			t.Errorf("missing attribute %q", key)
		} else if got != want {
			t.Errorf("attribute %q = %q, want %q", key, got, want)
		}
	}
}

func TestInjectTraceContext_W3CFormat(t *testing.T) {
	sr := setupRecorder(t)
	auditLogger := NewProductionAuditLogger().(*ProductionAuditLogger)
	run := testRun()

	t.Run("no_active_span_no_header", func(t *testing.T) {
		headers := http.Header{}
		auditLogger.InjectTraceContext(context.Background(), run, headers)
		if headers.Get("traceparent") != "" {
			t.Error("Should not inject header when no active span")
		}
	})

	t.Run("active_span_injects_header", func(t *testing.T) {
		spanCtx, span := auditLogger.StartAnalysisSpan(context.Background(), run)

		headers := http.Header{}
		auditLogger.InjectTraceContext(spanCtx, run, headers)

		traceparent := headers.Get("traceparent")
		if traceparent == "" {
			t.Fatal("traceparent header missing")
		}

		parts := strings.Split(traceparent, "-")
		if len(parts) != 4 {
			t.Fatalf("Expected 4 parts in traceparent, got %d: %s", len(parts), traceparent)
		}

		activeSpanID := span.(sdktrace.ReadOnlySpan).SpanContext().SpanID().String()
		if parts[2] != activeSpanID {
			t.Errorf("Injected span ID = %s, want active span ID %s", parts[2], activeSpanID)
		}
		span.End()
	})

	_ = sr
}

func TestCleanup_RemovesPriorPhase(t *testing.T) {
	sr := setupRecorder(t)
	auditLogger := NewProductionAuditLogger().(*ProductionAuditLogger)
	run := testRun()

	_, s1 := auditLogger.StartAnalysisSpan(context.Background(), run)
	s1.End()

	auditLogger.Cleanup(run)

	_, s2 := auditLogger.StartExecutionSpan(context.Background(), run)
	s2.End()

	spans := sr.Ended()
	execSpan := spans[len(spans)-1]
	if len(execSpan.Links()) != 0 {
		t.Error("After Cleanup, next phase should have no link to prior phase")
	}
}

func TestSpanServiceName(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	res := resource.NewWithAttributes(
		semconv.SchemaURL,
		semconv.ServiceName("lightspeed-agentic-operator"),
		semconv.ServiceVersion("test"),
	)
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr), sdktrace.WithResource(res))
	otel.SetTracerProvider(tp)
	defer otel.SetTracerProvider(sdktrace.NewTracerProvider())

	auditLogger := NewProductionAuditLogger().(*ProductionAuditLogger)
	run := testRun()

	_, span := auditLogger.StartAnalysisSpan(context.Background(), run)
	span.End()

	spans := sr.Ended()
	if len(spans) != 1 {
		t.Fatalf("Expected 1 span, got %d", len(spans))
	}
	var serviceName string
	for _, attr := range spans[0].Resource().Attributes() {
		if string(attr.Key) == "service.name" {
			serviceName = attr.Value.AsString()
		}
	}
	if serviceName != "lightspeed-agentic-operator" {
		t.Errorf("Expected service.name='lightspeed-agentic-operator', got %s", serviceName)
	}
}

func TestSpanInstrumentationScope(t *testing.T) {
	sr := setupRecorder(t)
	auditLogger := NewProductionAuditLogger().(*ProductionAuditLogger)
	run := testRun()

	_, span := auditLogger.StartAnalysisSpan(context.Background(), run)
	span.End()

	spans := sr.Ended()
	scope := spans[0].InstrumentationScope()
	if scope.Name != tracerName {
		t.Errorf("Expected instrumentation scope name=%s, got %s", tracerName, scope.Name)
	}
	if scope.Version != tracerVersion {
		t.Errorf("Expected instrumentation scope version=%s, got %s", tracerVersion, scope.Version)
	}
}

func TestFullLifecycle_PerPhaseTraces(t *testing.T) {
	sr := setupRecorder(t)
	auditLogger := NewProductionAuditLogger().(*ProductionAuditLogger)
	run := testRun()

	analysisResult := &agenticv1alpha1.AnalysisResult{
		ObjectMeta: metav1.ObjectMeta{Name: "r-analysis", Namespace: "test-ns", UID: "uid-1"},
	}
	executionResult := &agenticv1alpha1.ExecutionResult{
		ObjectMeta: metav1.ObjectMeta{Name: "r-execution", Namespace: "test-ns", UID: "uid-2"},
	}
	verificationResult := &agenticv1alpha1.VerificationResult{
		ObjectMeta: metav1.ObjectMeta{Name: "r-verification", Namespace: "test-ns", UID: "uid-3"},
	}

	// 1. Analysis phase
	ctx1, s1 := auditLogger.StartAnalysisSpan(context.Background(), run)
	auditLogger.EmitAgenticRunReceived(ctx1, run)
	auditLogger.EmitAnalysisCompleted(ctx1, run, analysisResult)
	s1.End()

	// 2. Approval
	auditLogger.EmitApprovalSpan(context.Background(), run, nil, "")

	// 3. Execution phase
	ctx2, s2 := auditLogger.StartExecutionSpan(context.Background(), run)
	auditLogger.EmitExecutionCompleted(ctx2, run, executionResult)
	s2.End()

	// 4. Verification phase
	ctx3, s3 := auditLogger.StartVerificationSpan(context.Background(), run)
	auditLogger.EmitVerificationCompleted(ctx3, run, verificationResult)
	s3.End()

	// 5. Terminal
	auditLogger.EmitTerminalSpan(context.Background(), run, "Completed", "success")

	// 6. Cleanup
	auditLogger.Cleanup(run)

	spans := sr.Ended()
	expectedNames := []string{
		"agenticrun.analyze",
		"agenticrun.human_approval",
		"agenticrun.execute",
		"agenticrun.verify",
		"agenticrun.terminal",
	}
	if len(spans) != len(expectedNames) {
		t.Fatalf("Expected %d spans, got %d", len(expectedNames), len(spans))
	}
	for i, s := range spans {
		if s.Name() != expectedNames[i] {
			t.Errorf("Span %d: expected name=%s, got %s", i, expectedNames[i], s.Name())
		}
	}

	traceIDs := make(map[string]bool)
	for _, s := range spans {
		traceIDs[s.SpanContext().TraceID().String()] = true
	}
	if len(traceIDs) != len(spans) {
		t.Errorf("Expected %d unique trace IDs (one per phase), got %d", len(spans), len(traceIDs))
	}

	if len(spans[0].Links()) != 0 {
		t.Error("First span should have no links")
	}
	for i := 1; i < len(spans); i++ {
		if len(spans[i].Links()) != 1 {
			t.Errorf("Span %d (%s) should have 1 link, got %d", i, spans[i].Name(), len(spans[i].Links()))
		}
	}
}

func TestTerminalReason(t *testing.T) {
	tests := []struct {
		name       string
		conditions []metav1.Condition
		want       string
	}{
		{
			name:       "failed_step",
			conditions: []metav1.Condition{{Type: "Analyzed", Status: metav1.ConditionFalse, Reason: "Failed", Message: "LLM timeout"}},
			want:       "LLM timeout",
		},
		{
			name:       "user_denied",
			conditions: []metav1.Condition{{Type: "Denied", Status: metav1.ConditionTrue, Reason: "UserDenied", Message: "Execution denied by user"}},
			want:       "Execution denied by user",
		},
		{
			name:       "system_suspended",
			conditions: []metav1.Condition{{Type: "EmergencyStopped", Status: metav1.ConditionTrue, Reason: "SystemSuspended", Message: "Terminated by system kill switch"}},
			want:       "Terminated by system kill switch",
		},
		{
			name:       "completed_no_reason",
			conditions: []metav1.Condition{{Type: "Verified", Status: metav1.ConditionTrue, Reason: "Passed", Message: "All checks passed"}},
			want:       "",
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
	sr := setupRecorder(t)
	auditLogger := NewProductionAuditLogger().(*ProductionAuditLogger)
	run := testRun()

	_, s1 := auditLogger.StartAnalysisSpan(context.Background(), run)
	s1.End()

	// Auto-approve: skip EmitApprovalSpan

	_, s2 := auditLogger.StartExecutionSpan(context.Background(), run)
	s2.End()

	for _, s := range sr.Ended() {
		if s.Name() == "agenticrun.human_approval" {
			t.Error("human_approval span should not exist when execution is auto-approved")
		}
	}
}
