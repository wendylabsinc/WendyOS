package mcp

import (
	"context"
	"io"
	"strings"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/wendylabsinc/wendy/proto/gen/agentpb"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

var telemetryProtoJSON = protojson.MarshalOptions{
	Multiline:       true,
	Indent:          "  ",
	EmitUnpopulated: false,
}

func (s *mcpServer) registerTelemetryTools(srv *server.MCPServer) {
	srv.AddTool(mcpgo.NewTool("telemetry_logs",
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
	), s.handleTelemetryLogs)

	srv.AddTool(mcpgo.NewTool("telemetry_metrics",
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
	), s.handleTelemetryMetrics)

	srv.AddTool(mcpgo.NewTool("telemetry_traces",
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
	), s.handleTelemetryTraces)
}

// collectProtoStream receives up to maxBatches messages from stream, marshals
// each to JSON with protojson, and returns them as a JSON array string.
func collectProtoStream[T proto.Message](
	ctx context.Context,
	recv func() (T, error),
	maxBatches int,
) string {
	var parts []string
	for len(parts) < maxBatches {
		msg, err := recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			break
		}
		b, err := telemetryProtoJSON.Marshal(msg)
		if err != nil {
			continue
		}
		parts = append(parts, string(b))
	}
	if len(parts) == 0 {
		return "[]"
	}
	return "[" + strings.Join(parts, ",\n") + "]"
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
		return mcpgo.NewToolResultError(grpcErrString(err)), nil
	}
	result := collectProtoStream(ctx, func() (*agentpb.StreamLogsResponse, error) {
		return stream.Recv()
	}, maxBatches)
	return mcpgo.NewToolResultText(result), nil
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
		return mcpgo.NewToolResultError(grpcErrString(err)), nil
	}
	result := collectProtoStream(ctx, func() (*agentpb.StreamMetricsResponse, error) {
		return stream.Recv()
	}, maxBatches)
	return mcpgo.NewToolResultText(result), nil
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
		return mcpgo.NewToolResultError(grpcErrString(err)), nil
	}
	result := collectProtoStream(ctx, func() (*agentpb.StreamTracesResponse, error) {
		return stream.Recv()
	}, maxBatches)
	return mcpgo.NewToolResultText(result), nil
}
