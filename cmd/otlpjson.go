package main

import (
	"context"
	"os"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/encoding/protojson"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// otlpJSONStdoutExporter writes spans as OTLP JSON (one line per batch) to stdout.
type otlpJSONStdoutExporter struct{}

func (e *otlpJSONStdoutExporter) ExportSpans(_ context.Context, spans []sdktrace.ReadOnlySpan) error {
	if len(spans) == 0 {
		return nil
	}
	// WithSyncer flushes one span at a time, so grouping under spans[0] is safe.
	ss := &tracepb.ScopeSpans{
		Scope: &commonpb.InstrumentationScope{
			Name:    spans[0].InstrumentationScope().Name,
			Version: spans[0].InstrumentationScope().Version,
		},
	}
	for _, s := range spans {
		ss.Spans = append(ss.Spans, convertSpan(s))
	}
	data := &tracepb.TracesData{
		ResourceSpans: []*tracepb.ResourceSpans{{
			Resource:   convertResource(spans[0]),
			ScopeSpans: []*tracepb.ScopeSpans{ss},
		}},
	}
	opts := protojson.MarshalOptions{UseProtoNames: true}
	b, err := opts.Marshal(data)
	if err != nil {
		return err
	}
	if _, err = os.Stdout.Write(b); err != nil {
		return err
	}
	if _, err = os.Stdout.Write([]byte("\n")); err != nil {
		return err
	}
	return nil
}

func (e *otlpJSONStdoutExporter) Shutdown(context.Context) error { return nil }

func convertResource(s sdktrace.ReadOnlySpan) *resourcepb.Resource {
	return &resourcepb.Resource{Attributes: convertAttrs(s.Resource().Attributes())}
}

func convertSpan(s sdktrace.ReadOnlySpan) *tracepb.Span {
	tid := s.SpanContext().TraceID()
	sid := s.SpanContext().SpanID()
	psid := s.Parent().SpanID()

	span := &tracepb.Span{
		TraceId:                tid[:],
		SpanId:                 sid[:],
		Name:                   s.Name(),
		Kind:                   convertKind(s.SpanKind()),
		StartTimeUnixNano:      uint64(s.StartTime().UnixNano()),
		EndTimeUnixNano:        uint64(s.EndTime().UnixNano()),
		Attributes:             convertAttrs(s.Attributes()),
		DroppedAttributesCount: uint32(s.DroppedAttributes()),
		DroppedEventsCount:     uint32(s.DroppedEvents()),
		DroppedLinksCount:      uint32(s.DroppedLinks()),
		Status:                 convertStatus(s.Status()),
	}
	if s.Parent().IsValid() {
		span.ParentSpanId = psid[:]
	}
	for _, ev := range s.Events() {
		span.Events = append(span.Events, convertEvent(ev))
	}
	for _, lk := range s.Links() {
		span.Links = append(span.Links, convertLink(lk))
	}
	return span
}

func convertKind(k trace.SpanKind) tracepb.Span_SpanKind {
	switch k {
	case trace.SpanKindInternal:
		return tracepb.Span_SPAN_KIND_INTERNAL
	case trace.SpanKindServer:
		return tracepb.Span_SPAN_KIND_SERVER
	case trace.SpanKindClient:
		return tracepb.Span_SPAN_KIND_CLIENT
	case trace.SpanKindProducer:
		return tracepb.Span_SPAN_KIND_PRODUCER
	case trace.SpanKindConsumer:
		return tracepb.Span_SPAN_KIND_CONSUMER
	default:
		return tracepb.Span_SPAN_KIND_UNSPECIFIED
	}
}

func convertStatus(st sdktrace.Status) *tracepb.Status {
	ps := &tracepb.Status{Message: st.Description}
	switch st.Code {
	case codes.Ok:
		ps.Code = tracepb.Status_STATUS_CODE_OK
	case codes.Error:
		ps.Code = tracepb.Status_STATUS_CODE_ERROR
	default:
		ps.Code = tracepb.Status_STATUS_CODE_UNSET
	}
	return ps
}

func convertEvent(ev sdktrace.Event) *tracepb.Span_Event {
	return &tracepb.Span_Event{
		TimeUnixNano:           uint64(ev.Time.UnixNano()),
		Name:                   ev.Name,
		Attributes:             convertAttrs(ev.Attributes),
		DroppedAttributesCount: uint32(ev.DroppedAttributeCount),
	}
}

func convertLink(lk sdktrace.Link) *tracepb.Span_Link {
	tid := lk.SpanContext.TraceID()
	sid := lk.SpanContext.SpanID()
	return &tracepb.Span_Link{
		TraceId:                tid[:],
		SpanId:                 sid[:],
		Attributes:             convertAttrs(lk.Attributes),
		DroppedAttributesCount: uint32(lk.DroppedAttributeCount),
	}
}

func convertAttrs(kvs []attribute.KeyValue) []*commonpb.KeyValue {
	if len(kvs) == 0 {
		return nil
	}
	out := make([]*commonpb.KeyValue, len(kvs))
	for i, kv := range kvs {
		out[i] = &commonpb.KeyValue{Key: string(kv.Key), Value: convertValue(kv.Value)}
	}
	return out
}

func convertValue(v attribute.Value) *commonpb.AnyValue {
	switch v.Type() {
	case attribute.STRING:
		return &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: v.AsString()}}
	case attribute.BOOL:
		return &commonpb.AnyValue{Value: &commonpb.AnyValue_BoolValue{BoolValue: v.AsBool()}}
	case attribute.INT64:
		return &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: v.AsInt64()}}
	case attribute.FLOAT64:
		return &commonpb.AnyValue{Value: &commonpb.AnyValue_DoubleValue{DoubleValue: v.AsFloat64()}}
	case attribute.BOOLSLICE:
		vals := make([]*commonpb.AnyValue, len(v.AsBoolSlice()))
		for i, b := range v.AsBoolSlice() {
			vals[i] = &commonpb.AnyValue{Value: &commonpb.AnyValue_BoolValue{BoolValue: b}}
		}
		return &commonpb.AnyValue{Value: &commonpb.AnyValue_ArrayValue{ArrayValue: &commonpb.ArrayValue{Values: vals}}}
	case attribute.INT64SLICE:
		vals := make([]*commonpb.AnyValue, len(v.AsInt64Slice()))
		for i, n := range v.AsInt64Slice() {
			vals[i] = &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: n}}
		}
		return &commonpb.AnyValue{Value: &commonpb.AnyValue_ArrayValue{ArrayValue: &commonpb.ArrayValue{Values: vals}}}
	case attribute.FLOAT64SLICE:
		vals := make([]*commonpb.AnyValue, len(v.AsFloat64Slice()))
		for i, f := range v.AsFloat64Slice() {
			vals[i] = &commonpb.AnyValue{Value: &commonpb.AnyValue_DoubleValue{DoubleValue: f}}
		}
		return &commonpb.AnyValue{Value: &commonpb.AnyValue_ArrayValue{ArrayValue: &commonpb.ArrayValue{Values: vals}}}
	case attribute.STRINGSLICE:
		vals := make([]*commonpb.AnyValue, len(v.AsStringSlice()))
		for i, s := range v.AsStringSlice() {
			vals[i] = &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: s}}
		}
		return &commonpb.AnyValue{Value: &commonpb.AnyValue_ArrayValue{ArrayValue: &commonpb.ArrayValue{Values: vals}}}
	default:
		return &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: v.Emit()}}
	}
}
