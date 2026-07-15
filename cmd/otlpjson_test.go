package main

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

func TestConvertValueScalars(t *testing.T) {
	tests := []struct {
		name string
		val  attribute.Value
		want any
	}{
		{"string", attribute.StringValue("hello"), "hello"},
		{"bool", attribute.BoolValue(true), true},
		{"int64", attribute.Int64Value(42), int64(42)},
		{"float64", attribute.Float64Value(3.14), 3.14},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			av := convertValue(tt.val)
			switch v := tt.want.(type) {
			case string:
				got := av.GetStringValue()
				if got != v {
					t.Errorf("got %q, want %q", got, v)
				}
			case bool:
				got := av.GetBoolValue()
				if got != v {
					t.Errorf("got %v, want %v", got, v)
				}
			case int64:
				got := av.GetIntValue()
				if got != v {
					t.Errorf("got %d, want %d", got, v)
				}
			case float64:
				got := av.GetDoubleValue()
				if got != v {
					t.Errorf("got %f, want %f", got, v)
				}
			}
		})
	}
}

func TestConvertValueSlices(t *testing.T) {
	t.Run("string_slice", func(t *testing.T) {
		av := convertValue(attribute.StringSliceValue([]string{"a", "b", "c"}))
		arr := av.GetArrayValue()
		if arr == nil {
			t.Fatal("expected ArrayValue, got nil")
		}
		if len(arr.Values) != 3 {
			t.Fatalf("expected 3 elements, got %d", len(arr.Values))
		}
		for i, want := range []string{"a", "b", "c"} {
			if got := arr.Values[i].GetStringValue(); got != want {
				t.Errorf("[%d] got %q, want %q", i, got, want)
			}
		}
	})

	t.Run("int64_slice", func(t *testing.T) {
		av := convertValue(attribute.Int64SliceValue([]int64{1, 2, 3}))
		arr := av.GetArrayValue()
		if arr == nil {
			t.Fatal("expected ArrayValue, got nil")
		}
		if len(arr.Values) != 3 {
			t.Fatalf("expected 3 elements, got %d", len(arr.Values))
		}
		for i, want := range []int64{1, 2, 3} {
			if got := arr.Values[i].GetIntValue(); got != want {
				t.Errorf("[%d] got %d, want %d", i, got, want)
			}
		}
	})

	t.Run("float64_slice", func(t *testing.T) {
		av := convertValue(attribute.Float64SliceValue([]float64{1.1, 2.2}))
		arr := av.GetArrayValue()
		if arr == nil {
			t.Fatal("expected ArrayValue, got nil")
		}
		if len(arr.Values) != 2 {
			t.Fatalf("expected 2 elements, got %d", len(arr.Values))
		}
	})

	t.Run("bool_slice", func(t *testing.T) {
		av := convertValue(attribute.BoolSliceValue([]bool{true, false}))
		arr := av.GetArrayValue()
		if arr == nil {
			t.Fatal("expected ArrayValue, got nil")
		}
		if len(arr.Values) != 2 {
			t.Fatalf("expected 2 elements, got %d", len(arr.Values))
		}
		if got := arr.Values[0].GetBoolValue(); got != true {
			t.Errorf("[0] got %v, want true", got)
		}
		if got := arr.Values[1].GetBoolValue(); got != false {
			t.Errorf("[1] got %v, want false", got)
		}
	})

	t.Run("empty_slice", func(t *testing.T) {
		av := convertValue(attribute.StringSliceValue([]string{}))
		arr := av.GetArrayValue()
		if arr == nil {
			t.Fatal("expected ArrayValue, got nil")
		}
		if len(arr.Values) != 0 {
			t.Errorf("expected 0 elements, got %d", len(arr.Values))
		}
	})
}

func TestConvertKind(t *testing.T) {
	tests := []struct {
		in   trace.SpanKind
		want tracepb.Span_SpanKind
	}{
		{trace.SpanKindInternal, tracepb.Span_SPAN_KIND_INTERNAL},
		{trace.SpanKindServer, tracepb.Span_SPAN_KIND_SERVER},
		{trace.SpanKindClient, tracepb.Span_SPAN_KIND_CLIENT},
		{trace.SpanKindProducer, tracepb.Span_SPAN_KIND_PRODUCER},
		{trace.SpanKindConsumer, tracepb.Span_SPAN_KIND_CONSUMER},
		{trace.SpanKind(-1), tracepb.Span_SPAN_KIND_UNSPECIFIED},
	}
	for _, tt := range tests {
		got := convertKind(tt.in)
		if got != tt.want {
			t.Errorf("convertKind(%v) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

func TestConvertStatus(t *testing.T) {
	tests := []struct {
		name string
		st   sdktrace.Status
		code tracepb.Status_StatusCode
	}{
		{"ok", sdktrace.Status{Code: codes.Ok}, tracepb.Status_STATUS_CODE_OK},
		{"error", sdktrace.Status{Code: codes.Error, Description: "fail"}, tracepb.Status_STATUS_CODE_ERROR},
		{"unset", sdktrace.Status{Code: codes.Unset}, tracepb.Status_STATUS_CODE_UNSET},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ps := convertStatus(tt.st)
			if ps.Code != tt.code {
				t.Errorf("got %v, want %v", ps.Code, tt.code)
			}
		})
	}

	t.Run("description_preserved", func(t *testing.T) {
		ps := convertStatus(sdktrace.Status{Code: codes.Error, Description: "boom"})
		if ps.Message != "boom" {
			t.Errorf("got %q, want %q", ps.Message, "boom")
		}
	})
}

func TestConvertAttrsNil(t *testing.T) {
	got := convertAttrs(nil)
	if got != nil {
		t.Errorf("expected nil for empty attrs, got %v", got)
	}
}

func TestConvertEvent(t *testing.T) {
	now := time.Now()
	ev := sdktrace.Event{
		Name: "test.event",
		Time: now,
		Attributes: []attribute.KeyValue{
			attribute.String("key", "val"),
		},
	}
	pe := convertEvent(ev)
	if pe.Name != "test.event" {
		t.Errorf("name = %q, want %q", pe.Name, "test.event")
	}
	if pe.TimeUnixNano != uint64(now.UnixNano()) {
		t.Errorf("time mismatch")
	}
	if len(pe.Attributes) != 1 {
		t.Fatalf("expected 1 attr, got %d", len(pe.Attributes))
	}
	if pe.Attributes[0].Key != "key" {
		t.Errorf("attr key = %q, want %q", pe.Attributes[0].Key, "key")
	}
}

func TestConvertLink(t *testing.T) {
	tid := trace.TraceID{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	sid := trace.SpanID{1, 2, 3, 4, 5, 6, 7, 8}
	sc := trace.NewSpanContext(trace.SpanContextConfig{TraceID: tid, SpanID: sid, TraceFlags: trace.FlagsSampled})
	lk := sdktrace.Link{
		SpanContext: sc,
		Attributes:  []attribute.KeyValue{attribute.String("lk", "val")},
	}
	pl := convertLink(lk)
	if len(pl.TraceId) != 16 {
		t.Errorf("trace ID length = %d", len(pl.TraceId))
	}
	if len(pl.SpanId) != 8 {
		t.Errorf("span ID length = %d", len(pl.SpanId))
	}
	if len(pl.Attributes) != 1 {
		t.Fatalf("expected 1 attr, got %d", len(pl.Attributes))
	}
}

func TestConvertSpanRoundTrip(t *testing.T) {
	stub := tracetest.SpanStub{
		Name:      "test.span",
		SpanKind:  trace.SpanKindClient,
		StartTime: time.Now().Add(-time.Second),
		EndTime:   time.Now(),
		Attributes: []attribute.KeyValue{
			attribute.String("gen_ai.operation.name", "chat"),
			attribute.Int64("gen_ai.usage.input_tokens", 100),
			attribute.StringSlice("target.namespaces", []string{"ns1", "ns2"}),
		},
		Events: []sdktrace.Event{
			{Name: "gen_ai.choice", Time: time.Now(), Attributes: []attribute.KeyValue{attribute.String("gen_ai.completion", "hello")}},
		},
		Status: sdktrace.Status{Code: codes.Ok},
	}
	snap := stub.Snapshot()

	ps := convertSpan(snap)

	if ps.Name != "test.span" {
		t.Errorf("name = %q", ps.Name)
	}
	if ps.Kind != tracepb.Span_SPAN_KIND_CLIENT {
		t.Errorf("kind = %v", ps.Kind)
	}
	if len(ps.Attributes) != 3 {
		t.Fatalf("expected 3 attrs, got %d", len(ps.Attributes))
	}
	if len(ps.Events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(ps.Events))
	}

	// Verify the string slice attribute preserved as array
	var sliceAttr *commonpb.KeyValue
	for _, a := range ps.Attributes {
		if a.Key == "target.namespaces" {
			sliceAttr = a
			break
		}
	}
	if sliceAttr == nil {
		t.Fatal("target.namespaces attr not found")
	}
	arr := sliceAttr.Value.GetArrayValue()
	if arr == nil {
		t.Fatal("expected ArrayValue for string slice")
	}
	if len(arr.Values) != 2 {
		t.Fatalf("expected 2 array elements, got %d", len(arr.Values))
	}
}

func TestExportSpansProducesValidJSON(t *testing.T) {
	stub := tracetest.SpanStub{
		Name:      "test.span",
		SpanKind:  trace.SpanKindInternal,
		StartTime: time.Now().Add(-time.Second),
		EndTime:   time.Now(),
		Attributes: []attribute.KeyValue{
			attribute.String("key", "val"),
		},
		Status: sdktrace.Status{Code: codes.Ok},
	}
	snap := stub.Snapshot()

	exp := &otlpJSONStdoutExporter{}

	// Capture stdout
	r, w, _ := os.Pipe()
	oldStdout := os.Stdout
	os.Stdout = w

	err := exp.ExportSpans(t.Context(), []sdktrace.ReadOnlySpan{snap})
	if err != nil {
		t.Fatalf("ExportSpans: %v", err)
	}

	w.Close()
	buf := make([]byte, 64*1024)
	n, _ := r.Read(buf)
	os.Stdout = oldStdout

	output := string(buf[:n])
	if !json.Valid([]byte(output)) {
		t.Errorf("output is not valid JSON: %s", output)
	}
}

func TestExportSpansEmpty(t *testing.T) {
	exp := &otlpJSONStdoutExporter{}
	err := exp.ExportSpans(t.Context(), nil)
	if err != nil {
		t.Fatalf("ExportSpans with nil: %v", err)
	}
	err = exp.ExportSpans(t.Context(), []sdktrace.ReadOnlySpan{})
	if err != nil {
		t.Fatalf("ExportSpans with empty: %v", err)
	}
}

func TestShutdown(t *testing.T) {
	exp := &otlpJSONStdoutExporter{}
	if err := exp.Shutdown(t.Context()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
}
