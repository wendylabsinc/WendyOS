package mcp

import (
	"context"
	"encoding/json"
	"io"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

var telemetryProtoJSON = protojson.MarshalOptions{
	Multiline:       true,
	Indent:          "  ",
	EmitUnpopulated: false,
}

func (s *mcpServer) registerTelemetryTools(srv *server.MCPServer) {
	logsOpts := []mcpgo.ToolOption{
		mcpgo.WithDescription("Stream a bounded snapshot of OTLP logs from the connected device"),
		mcpgo.WithString("app_name",
			mcpgo.Description("Filter by app/container name (optional)"),
		),
		mcpgo.WithString("service_name",
			mcpgo.Description("Filter by service name (optional)"),
		),
		mcpgo.WithNumber("min_severity",
			mcpgo.Description("Minimum severity (TRACE=1, DEBUG=5, INFO=9, WARN=13, ERROR=17, FATAL=21)"),
		),
		mcpgo.WithNumber("max_batches",
			mcpgo.Description("Maximum OTLP batches to collect (default 10)"),
		),
		mcpgo.WithNumber("max_bytes",
			mcpgo.Description("Maximum output size in bytes before the result is truncated (default 100000)"),
		),
	}
	logsOpts = append(logsOpts, readOnly()...)
	logsOpts = append(logsOpts, localOnly()...)
	srv.AddTool(mcpgo.NewTool("telemetry_logs", logsOpts...), s.handleTelemetryLogs)

	metricsOpts := []mcpgo.ToolOption{
		mcpgo.WithDescription("Stream a bounded snapshot of OTLP metrics from the connected device"),
		mcpgo.WithString("app_name",
			mcpgo.Description("Filter by app/container name (optional)"),
		),
		mcpgo.WithString("service_name",
			mcpgo.Description("Filter by service name (optional)"),
		),
		mcpgo.WithString("metric_name_prefix",
			mcpgo.Description("Filter by metric name prefix (optional)"),
		),
		mcpgo.WithNumber("max_batches",
			mcpgo.Description("Maximum OTLP batches to collect (default 10)"),
		),
		mcpgo.WithNumber("max_bytes",
			mcpgo.Description("Maximum output size in bytes before the result is truncated (default 100000)"),
		),
	}
	metricsOpts = append(metricsOpts, readOnly()...)
	metricsOpts = append(metricsOpts, localOnly()...)
	srv.AddTool(mcpgo.NewTool("telemetry_metrics", metricsOpts...), s.handleTelemetryMetrics)

	tracesOpts := []mcpgo.ToolOption{
		mcpgo.WithDescription("Stream a bounded snapshot of OTLP traces from the connected device"),
		mcpgo.WithString("app_name",
			mcpgo.Description("Filter by app/container name (optional)"),
		),
		mcpgo.WithString("service_name",
			mcpgo.Description("Filter by service name (optional)"),
		),
		mcpgo.WithString("span_name_prefix",
			mcpgo.Description("Filter by span name prefix (optional)"),
		),
		mcpgo.WithNumber("max_batches",
			mcpgo.Description("Maximum OTLP batches to collect (default 10)"),
		),
		mcpgo.WithNumber("max_bytes",
			mcpgo.Description("Maximum output size in bytes before the result is truncated (default 100000)"),
		),
	}
	tracesOpts = append(tracesOpts, readOnly()...)
	tracesOpts = append(tracesOpts, localOnly()...)
	srv.AddTool(mcpgo.NewTool("telemetry_traces", tracesOpts...), s.handleTelemetryTraces)
}

// collectProtoStream receives up to maxBatches messages from stream, marshals
// each to JSON with protojson, and returns them as a slice of raw JSON
// messages suitable for structured results. Returns an error on non-EOF
// stream failures.
func collectProtoStream[T proto.Message](
	ctx context.Context,
	recv func() (T, error),
	maxBatches int,
) ([]json.RawMessage, error) {
	parts := []json.RawMessage{}
	for len(parts) < maxBatches {
		msg, err := recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			return nil, err
		}
		b, err := telemetryProtoJSON.Marshal(msg)
		if err != nil {
			continue
		}
		parts = append(parts, json.RawMessage(b))
	}
	return parts, nil
}

func (s *mcpServer) handleTelemetryLogs(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	conn := s.GetConn()
	if conn == nil {
		return errNotConnected(), nil
	}

	logsReq := &agentpb.StreamLogsRequest{}
	if v := stringParam(req, "app_name"); v != "" {
		logsReq.AppName = &v
	}
	if v := stringParam(req, "service_name"); v != "" {
		logsReq.ServiceName = &v
	}
	if v := intParam(req, "min_severity", 0); v > 0 {
		v32 := int32(v)
		logsReq.MinSeverity = &v32
	}
	maxBatches := intParam(req, "max_batches", 10)

	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	stream, err := conn.TelemetryService.StreamLogs(ctx, logsReq)
	if err != nil {
		return errResult(codeFromGRPC(err), grpcErrString(err)), nil
	}
	result, err := collectProtoStream(ctx, func() (*agentpb.StreamLogsResponse, error) {
		for {
			response, err := stream.Recv()
			if err != nil || response.GetLogs() != nil {
				return response, err
			}
		}
	}, maxBatches)
	if err != nil {
		return errResult(codeFromGRPC(err), grpcErrString(err)), nil
	}
	return okListBounded("batches", result, intParam(req, "max_bytes", 100000)), nil
}

func (s *mcpServer) handleTelemetryMetrics(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	conn := s.GetConn()
	if conn == nil {
		return errNotConnected(), nil
	}

	metricsReq := &agentpb.StreamMetricsRequest{}
	if v := stringParam(req, "app_name"); v != "" {
		metricsReq.AppName = &v
	}
	if v := stringParam(req, "service_name"); v != "" {
		metricsReq.ServiceName = &v
	}
	if v := stringParam(req, "metric_name_prefix"); v != "" {
		metricsReq.MetricNamePrefix = &v
	}
	maxBatches := intParam(req, "max_batches", 10)

	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	stream, err := conn.TelemetryService.StreamMetrics(ctx, metricsReq)
	if err != nil {
		return errResult(codeFromGRPC(err), grpcErrString(err)), nil
	}
	result, err := collectProtoStream(ctx, func() (*agentpb.StreamMetricsResponse, error) {
		return stream.Recv()
	}, maxBatches)
	if err != nil {
		return errResult(codeFromGRPC(err), grpcErrString(err)), nil
	}
	return okListBounded("batches", result, intParam(req, "max_bytes", 100000)), nil
}

func (s *mcpServer) handleTelemetryTraces(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	conn := s.GetConn()
	if conn == nil {
		return errNotConnected(), nil
	}

	tracesReq := &agentpb.StreamTracesRequest{}
	if v := stringParam(req, "app_name"); v != "" {
		tracesReq.AppName = &v
	}
	if v := stringParam(req, "service_name"); v != "" {
		tracesReq.ServiceName = &v
	}
	if v := stringParam(req, "span_name_prefix"); v != "" {
		tracesReq.SpanNamePrefix = &v
	}
	maxBatches := intParam(req, "max_batches", 10)

	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	stream, err := conn.TelemetryService.StreamTraces(ctx, tracesReq)
	if err != nil {
		return errResult(codeFromGRPC(err), grpcErrString(err)), nil
	}
	result, err := collectProtoStream(ctx, func() (*agentpb.StreamTracesResponse, error) {
		return stream.Recv()
	}, maxBatches)
	if err != nil {
		return errResult(codeFromGRPC(err), grpcErrString(err)), nil
	}
	return okListBounded("batches", result, intParam(req, "max_bytes", 100000)), nil
}
