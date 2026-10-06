package mcp

import (
	"cmp"
	"context"
	"encoding/json"
	"io"
	"slices"
	"strconv"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

var telemetryProtoJSON = protojson.MarshalOptions{
	EmitUnpopulated: false,
}

func (s *mcpServer) registerTelemetryTools(srv *server.MCPServer) {
	logsOpts := []mcpgo.ToolOption{
		mcpgo.WithDescription("Collect compact log events from the connected device. format=otlp returns raw batches. Omitted counts cover collected records only."),
		mcpgo.WithString("app_name",
			mcpgo.Description("Filter by app/container name (optional)"),
		),
		mcpgo.WithString("service_name",
			mcpgo.Description("Filter by service name (optional)"),
		),
		mcpgo.WithNumber("min_severity",
			mcpgo.Description("Minimum severity (TRACE=1, DEBUG=5, INFO=9, WARN=13, ERROR=17, FATAL=21)"),
		),
	}
	logsOpts = append(logsOpts, telemetryOptions()...)
	logsOpts = append(logsOpts, readOnly()...)
	logsOpts = append(logsOpts, localOnly()...)
	srv.AddTool(mcpgo.NewTool("telemetry_logs", logsOpts...), s.handleTelemetryLogs)

	metricsOpts := []mcpgo.ToolOption{
		mcpgo.WithDescription("Collect compact metric points; format=otlp returns raw batches. Use device_info for battery readings."),
		mcpgo.WithString("app_name",
			mcpgo.Description("Filter by app/container name (optional)"),
		),
		mcpgo.WithString("service_name",
			mcpgo.Description("Filter by service name (optional)"),
		),
		mcpgo.WithString("metric_name_prefix",
			mcpgo.Description("Filter by metric name prefix (optional)"),
		),
		mcpgo.WithInteger("last_n", mcpgo.Min(1), mcpgo.Max(100), mcpgo.Description("Read up to this many recent metric batches, bounded by max_batches. Returns after replay or a short idle gap; compact points retain newest timestamps first when limited.")),
	}
	metricsOpts = append(metricsOpts, telemetryOptions()...)
	metricsOpts = append(metricsOpts, readOnly()...)
	metricsOpts = append(metricsOpts, localOnly()...)
	srv.AddTool(mcpgo.NewTool("telemetry_metrics", metricsOpts...), s.handleTelemetryMetrics)

	tracesOpts := []mcpgo.ToolOption{
		mcpgo.WithDescription("Collect compact trace spans; format=otlp returns raw batches."),
		mcpgo.WithString("app_name",
			mcpgo.Description("Filter by app/container name (optional)"),
		),
		mcpgo.WithString("service_name",
			mcpgo.Description("Filter by service name (optional)"),
		),
		mcpgo.WithString("span_name_prefix",
			mcpgo.Description("Filter by span name prefix (optional)"),
		),
	}
	tracesOpts = append(tracesOpts, telemetryOptions()...)
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
	opts, err := parseTelemetryOptions(req)
	if err != nil {
		return errResult(errCodeInvalidArgument, err.Error()), nil
	}
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
	maxBatches := opts.maxBatches

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
	return telemetryResult("logs", result, opts), nil
}

func (s *mcpServer) handleTelemetryMetrics(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	opts, err := parseTelemetryOptions(req)
	if err != nil {
		return errResult(errCodeInvalidArgument, err.Error()), nil
	}
	lastN := 0
	if _, provided := req.GetArguments()["last_n"]; provided {
		lastN, err = ros2Int(req, "last_n", 0, 1, 100)
		if err != nil {
			return errResult(errCodeInvalidArgument, err.Error()), nil
		}
	}
	conn := s.GetConn()
	if conn == nil {
		return errNotConnected(), nil
	}

	metricsReq := &agentpb.StreamMetricsRequest{}
	if lastN > 0 {
		// The agent replays oldest-to-newest within this tail. Request only
		// what we can collect so a smaller batch budget still gets the newest.
		lastN = min(lastN, opts.maxBatches)
		n := int32(lastN)
		metricsReq.LastN = &n
		opts.maxBatches = lastN
	}
	if v := stringParam(req, "app_name"); v != "" {
		metricsReq.AppName = &v
	}
	if v := stringParam(req, "service_name"); v != "" {
		metricsReq.ServiceName = &v
	}
	if v := stringParam(req, "metric_name_prefix"); v != "" {
		metricsReq.MetricNamePrefix = &v
	}
	maxBatches := opts.maxBatches

	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var idle *time.Timer
	if lastN > 0 {
		// Older agents have no finite-history flag or replay-complete marker.
		// Read the immediate replay burst, without waiting for a live sample.
		idle = time.AfterFunc(3*time.Second, cancel)
		defer idle.Stop()
	}

	stream, err := conn.TelemetryService.StreamMetrics(ctx, metricsReq)
	if err != nil {
		return errResult(codeFromGRPC(err), grpcErrString(err)), nil
	}
	historyBatches := 0
	result, err := collectProtoStream(ctx, func() (*agentpb.StreamMetricsResponse, error) {
		for {
			response, err := stream.Recv()
			if err != nil {
				return response, err
			}
			if response.GetMetrics() == nil {
				continue
			}
			if response.GetIsHistory() {
				historyBatches++
			}
			if idle != nil {
				idle.Reset(250 * time.Millisecond)
			}
			return response, nil
		}
	}, maxBatches)
	cancel()
	if err != nil {
		return errResult(codeFromGRPC(err), grpcErrString(err)), nil
	}
	if lastN > 0 {
		return telemetryMetricsHistoryResult(result, opts, lastN, historyBatches), nil
	}
	return telemetryResult("metrics", result, opts), nil
}

func telemetryMetricsHistoryResult(batches []json.RawMessage, opts telemetryOptionsValue, lastN, historyBatches int) *mcpgo.CallToolResult {
	metadata := map[string]any{"batches_collected": len(batches), "collection_limited": len(batches) >= opts.maxBatches, "last_n": lastN, "history_batches": historyBatches, "newest_first": true}
	if opts.format == "otlp" {
		slices.Reverse(batches)
		return okRowsBounded("batches", batches, metadata, opts.maxBytes, len(batches))
	}
	rows, err := compactTelemetryRows("metrics", batches)
	if err != nil {
		return errResultf(errCodeInternal, "decoding telemetry: %v", err)
	}
	timestamp := func(row map[string]any) uint64 {
		value, _ := row["timeUnixNano"].(string)
		nanos, _ := strconv.ParseUint(value, 10, 64)
		return nanos
	}
	slices.SortStableFunc(rows, func(a, b map[string]any) int { return cmp.Compare(timestamp(b), timestamp(a)) })
	return okRowsBounded("metrics", rows, metadata, opts.maxBytes, opts.maxRecords)
}

func (s *mcpServer) handleTelemetryTraces(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	opts, err := parseTelemetryOptions(req)
	if err != nil {
		return errResult(errCodeInvalidArgument, err.Error()), nil
	}
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
	maxBatches := opts.maxBatches

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
	return telemetryResult("traces", result, opts), nil
}
