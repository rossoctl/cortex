package otelbridge

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

func saveAndRestoreOtelGlobals(t *testing.T) {
	t.Helper()
	origProp := otel.GetTextMapPropagator()
	origTP := otel.GetTracerProvider()
	t.Cleanup(func() {
		otel.SetTextMapPropagator(origProp)
		otel.SetTracerProvider(origTP)
	})
}

func TestInit_NoEndpoint(t *testing.T) {
	saveAndRestoreOtelGlobals(t)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")

	shutdown, err := Init(context.Background())
	if err != nil {
		t.Fatalf("Init() with no endpoint failed: %v", err)
	}
	if shutdown == nil {
		t.Fatal("Init() returned nil shutdown func")
	}

	if err := shutdown(context.Background()); err != nil {
		t.Errorf("shutdown() failed: %v", err)
	}

	prop := otel.GetTextMapPropagator()
	if _, ok := prop.(propagation.TraceContext); ok {
		t.Error("TraceContext propagator set when OTEL_EXPORTER_OTLP_ENDPOINT unset")
	}
}

func TestInit_WithEndpoint(t *testing.T) {
	saveAndRestoreOtelGlobals(t)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://localhost:4318")
	t.Setenv("OTEL_SERVICE_NAME", "test-service")

	shutdown, err := Init(context.Background())
	if err != nil {
		t.Fatalf("Init() with endpoint failed: %v", err)
	}
	defer shutdown(context.Background())

	prop := otel.GetTextMapPropagator()
	if _, ok := prop.(propagation.TraceContext); !ok {
		t.Errorf("propagator type = %T, want propagation.TraceContext", prop)
	}

	tp := otel.GetTracerProvider()
	if tp == nil {
		t.Fatal("TracerProvider not set")
	}

	tracer := tp.Tracer("test")
	if tracer == nil {
		t.Fatal("Tracer() returned nil")
	}
}

const testTraceparent = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
const testTraceID = "4bf92f3577b34da6a3ce929d0e0e4736"

func extractTraceID(t *testing.T, traceparent string) string {
	t.Helper()
	parts := strings.SplitN(traceparent, "-", 4)
	if len(parts) < 3 {
		t.Fatalf("malformed traceparent %q: expected at least 3 dash-separated fields", traceparent)
	}
	return parts[1]
}

func TestExtractTraceContext(t *testing.T) {
	saveAndRestoreOtelGlobals(t)
	otel.SetTextMapPropagator(propagation.TraceContext{})

	headers := http.Header{}
	headers.Set("traceparent", testTraceparent)

	extractedCtx := ExtractTraceContext(context.Background(), headers)

	spanCtx := trace.SpanContextFromContext(extractedCtx)
	if !spanCtx.IsValid() {
		t.Error("ExtractTraceContext() did not extract valid span context")
	}

	if spanCtx.TraceID().String() != testTraceID {
		t.Errorf("trace ID = %s, want %s", spanCtx.TraceID(), testTraceID)
	}
}

func TestInjectTraceContext(t *testing.T) {
	saveAndRestoreOtelGlobals(t)
	otel.SetTextMapPropagator(propagation.TraceContext{})

	inHeaders := http.Header{}
	inHeaders.Set("traceparent", testTraceparent)
	ctx := ExtractTraceContext(context.Background(), inHeaders)

	outHeaders := http.Header{}
	InjectTraceContext(ctx, outHeaders)

	traceparent := outHeaders.Get("traceparent")
	if traceparent == "" {
		t.Fatal("InjectTraceContext() did not inject traceparent header")
	}

	inTraceID := extractTraceID(t, inHeaders.Get("traceparent"))
	outTraceID := extractTraceID(t, traceparent)
	if inTraceID != outTraceID {
		t.Errorf("trace ID changed: in = %s, out = %s", inTraceID, outTraceID)
	}
}

func TestExtractInjectRoundTrip(t *testing.T) {
	saveAndRestoreOtelGlobals(t)
	otel.SetTextMapPropagator(propagation.TraceContext{})

	inHeaders := http.Header{}
	inHeaders.Set("traceparent", testTraceparent)

	ctx := ExtractTraceContext(context.Background(), inHeaders)

	outHeaders := http.Header{}
	InjectTraceContext(ctx, outHeaders)

	inTraceID := extractTraceID(t, inHeaders.Get("traceparent"))
	outTraceID := extractTraceID(t, outHeaders.Get("traceparent"))
	if inTraceID != outTraceID {
		t.Errorf("traceparent trace ID changed: in = %s, out = %s", inTraceID, outTraceID)
	}
}
