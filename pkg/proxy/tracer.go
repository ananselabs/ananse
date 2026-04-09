package proxy

import (
	"context"
	"os"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	propagation "go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.uber.org/zap"
)

// healthFilterExporter wraps a SpanExporter and drops successful health check spans.
// Enabled via FILTER_HEALTH_CHECKS=true. Only spans where the response was an error
// (5xx / transport failure) are forwarded to Tempo.
type healthFilterExporter struct {
	wrapped sdktrace.SpanExporter
}

func (f *healthFilterExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	kept := make([]sdktrace.ReadOnlySpan, 0, len(spans))
	for _, s := range spans {
		if isHealthSpan(s) && s.Status().Code != codes.Error {
			continue
		}
		kept = append(kept, s)
	}
	if len(kept) == 0 {
		return nil
	}
	return f.wrapped.ExportSpans(ctx, kept)
}

func (f *healthFilterExporter) Shutdown(ctx context.Context) error {
	return f.wrapped.Shutdown(ctx)
}

func isHealthSpan(s sdktrace.ReadOnlySpan) bool {
	for _, attr := range s.Attributes() {
		if attr.Key == attribute.Key("http.url") {
			return strings.Contains(attr.Value.AsString(), "/health")
		}
	}
	return false
}

var (
	serviceName  = getServiceName()
	collectorURL = os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
)

func getServiceName() string {
	// SERVICE_NAME is set by injector from pod's app label
	if name := os.Getenv("SERVICE_NAME"); name != "" {
		return name
	}
	// Fallback to OTEL standard env var
	if name := os.Getenv("OTEL_SERVICE_NAME"); name != "" {
		return name
	}
	return ""
}

func InitTracer() func(context.Context) error {
	// Check if tracing is disabled
	if os.Getenv("ANANSE_TRACING_ENABLED") == "false" {
		Logger.Info("Tracing disabled via ANANSE_TRACING_ENABLED=false")
		return func(context.Context) error { return nil }
	}

	if serviceName == "" {
		serviceName = "ananse-proxy"
	}
	if collectorURL == "" {
		collectorURL = "localhost:4317"
	}

	rawExporter, err := otlptracegrpc.New(
		context.Background(),
		otlptracegrpc.WithEndpoint(collectorURL),
		otlptracegrpc.WithInsecure(),
	)
	if err != nil {
		Logger.Fatal("failed to create exporter", zap.Error(err))
	}

	var exporter sdktrace.SpanExporter = rawExporter
	if os.Getenv("FILTER_HEALTH_CHECKS") == "true" {
		exporter = &healthFilterExporter{wrapped: rawExporter}
		Logger.Info("health check trace filtering enabled — only errored health spans will be exported")
	}

	resources, err := resource.New(
		context.Background(),
		resource.WithAttributes(
			attribute.String("service.name", serviceName),
			attribute.String("telemetry.sdk.language", "go"),
		),
	)
	if err != nil {
		Logger.Error("Could not set resources", zap.Error(err))
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.ParentBased(
			sdktrace.TraceIDRatioBased(0.1), // 10% of NEW traces
		)),
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(resources),
	)
	otel.SetTracerProvider(tp)

	otel.SetTextMapPropagator(
		propagation.NewCompositeTextMapPropagator(
			propagation.TraceContext{},
			propagation.Baggage{},
		),
	)

	// Add error handler to surface hidden errors
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
		Logger.Error("OTel export error", zap.Error(err))
		Logger.Info("collector-url", zap.String("collector url", collectorURL))
	}))

	Logger.Info("OpenTelemetry initialized",
		zap.String("service", serviceName),
		zap.String("collector", collectorURL),
	)

	return exporter.Shutdown
}
