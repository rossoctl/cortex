package otelbridge

import (
	"context"
	"net/http"
	"os"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

func TestInit_NoEndpoint(t *testing.T) {
	os.Unsetenv("OTEL_EXPORTER_OTLP_ENDPOINT")

	shutdown, err := Init(context.Background())
	if err != nil {
		t.Fatalf("Init() with no endpoint failed: %v", err)
	}
	if shutdown == nil {
		t.Fatal("Init() returned nil shutdown func")
	}

	// Should be no-op
	if err := shutdown(context.Background()); err != nil {
		t.Errorf("shutdown() failed: %v", err)
	}

	// Propagator should not be set (defaults to noop)
	prop := otel.GetTextMapPropagator()
	if _, ok := prop.(propagation.TraceContext); ok {
		t.Error("TraceContext propagator set when OTEL_EXPORTER_OTLP_ENDPOINT unset")
	}
}

func TestInit_WithEndpoint(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://localhost:4318")
	t.Setenv("OTEL_SERVICE_NAME", "test-service")

	shutdown, err := Init(context.Background())
	if err != nil {
		t.Fatalf("Init() with endpoint failed: %v", err)
	}
	defer shutdown(context.Background())

	// Propagator should be TraceContext
	prop := otel.GetTextMapPropagator()
	if _, ok := prop.(propagation.TraceContext); !ok {
		t.Errorf("propagator type = %T, want propagation.TraceContext", prop)
	}

	// TracerProvider should be set
	tp := otel.GetTracerProvider()
	if tp == nil {
		t.Fatal("TracerProvider not set")
	}

	// Should be able to get a tracer
	tracer := tp.Tracer("test")
	if tracer == nil {
		t.Fatal("Tracer() returned nil")
	}
}

func TestExtractTraceContext(t *testing.T) {
	otel.SetTextMapPropagator(propagation.TraceContext{})

	// Use headers with a pre-existing traceparent (simulating inbound request)
	headers := http.Header{}
	headers.Set("traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")

	// Extract should populate the context with trace info
	extractedCtx := ExtractTraceContext(context.Background(), headers)

	spanCtx := trace.SpanContextFromContext(extractedCtx)
	if !spanCtx.IsValid() {
		t.Error("ExtractTraceContext() did not extract valid span context")
	}

	// Should have the trace ID from the header
	expectedTraceID := "4bf92f3577b34da6a3ce929d0e0e4736"
	if spanCtx.TraceID().String() != expectedTraceID {
		t.Errorf("trace ID = %s, want %s", spanCtx.TraceID(), expectedTraceID)
	}
}

func TestInjectTraceContext(t *testing.T) {
	otel.SetTextMapPropagator(propagation.TraceContext{})

	// Start with a context that has trace info (extracted from headers)
	inHeaders := http.Header{}
	inHeaders.Set("traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
	ctx := ExtractTraceContext(context.Background(), inHeaders)

	outHeaders := http.Header{}
	InjectTraceContext(ctx, outHeaders)

	// Should have traceparent header
	traceparent := outHeaders.Get("traceparent")
	if traceparent == "" {
		t.Error("InjectTraceContext() did not inject traceparent header")
	}

	// Should preserve the trace ID
	if inHeaders.Get("traceparent")[:35] != outHeaders.Get("traceparent")[:35] {
		t.Errorf("trace ID changed:\nin  = %s\nout = %s",
			inHeaders.Get("traceparent"), outHeaders.Get("traceparent"))
	}
}

func TestExtractInjectRoundTrip(t *testing.T) {
	otel.SetTextMapPropagator(propagation.TraceContext{})

	// Start with headers containing traceparent
	inHeaders := http.Header{}
	inHeaders.Set("traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")

	// Extract
	ctx := ExtractTraceContext(context.Background(), inHeaders)

	// Inject into new headers
	outHeaders := http.Header{}
	InjectTraceContext(ctx, outHeaders)

	// Should preserve the trace ID
	if inHeaders.Get("traceparent")[:35] != outHeaders.Get("traceparent")[:35] {
		t.Errorf("traceparent trace ID changed:\nin  = %s\nout = %s",
			inHeaders.Get("traceparent"), outHeaders.Get("traceparent"))
	}
}
