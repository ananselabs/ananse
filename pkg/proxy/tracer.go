package proxy

import (
	"context"
	"os"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	propagation "go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.uber.org/zap"
)

var (
	serviceName  = os.Getenv("OTEL_SERVICE_NAME")
	collectorURL = os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
)

func InitTracer() func(context.Context) error {
	if serviceName == "" {
		serviceName = "ananse-proxy"
	}
	if collectorURL == "" {
		collectorURL = "localhost:4317"
	}

	exporter, err := otlptracegrpc.New(
		context.Background(),
		otlptracegrpc.WithEndpoint(collectorURL),
		otlptracegrpc.WithInsecure(),
	)
	if err != nil {
		Logger.Fatal("failed to create exporter", zap.Error(err))
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
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
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
	}))

	Logger.Info("OpenTelemetry initialized",
		zap.String("service", serviceName),
		zap.String("collector", collectorURL),
	)

	return exporter.Shutdown
}
