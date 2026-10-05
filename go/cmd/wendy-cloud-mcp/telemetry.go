package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// Export configuration is operator-controlled through standard OTEL environment
// variables. No inbound request may select an exporter, endpoint, or sampling
// policy. The provider also supplies valid IDs when exporting is disabled.
func configureTelemetry(ctx context.Context, endpoint string) (func(), error) {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	opts := []sdktrace.TracerProviderOption{
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithResource(resource.NewSchemaless(attribute.String("service.name", "wendy-cloud-mcp"))),
	}
	exporterOptions := []otlptracehttp.Option{}
	if endpoint != "" {
		u, err := url.Parse(endpoint)
		if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "https" && !(u.Scheme == "http" && (u.Hostname() == "localhost" || net.ParseIP(u.Hostname()).IsLoopback()))) {
			return nil, fmt.Errorf("trace endpoint requires HTTPS or loopback HTTP")
		}
		exporterOptions = append(exporterOptions, otlptracehttp.WithEndpointURL(endpoint))
	}
	if endpoint != "" || os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") != "" || os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT") != "" {
		exporter, err := otlptracehttp.New(ctx, exporterOptions...)
		if err != nil {
			return nil, fmt.Errorf("could not configure trace exporter")
		}
		opts = append(opts, sdktrace.WithBatcher(exporter, sdktrace.WithMaxQueueSize(2048), sdktrace.WithExportTimeout(5*time.Second)))
	} else {
		slog.Info("OTLP export disabled; correlation IDs remain available in structured logs")
	}
	// Exporter errors may include endpoint details; never log arbitrary error text.
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(error) { slog.Error("trace export failed") }))
	provider := sdktrace.NewTracerProvider(opts...)
	otel.SetTracerProvider(provider)
	return func() {
		flush, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := provider.Shutdown(flush); err != nil {
			slog.Error("trace shutdown incomplete")
		}
	}, nil
}
