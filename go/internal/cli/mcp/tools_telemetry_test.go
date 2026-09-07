package mcp

import (
	"context"
	"net"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	colmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

type fakeTelemetryServer struct {
	agentpb.UnimplementedWendyTelemetryServiceServer
	logBatches    []*agentpb.StreamLogsResponse
	metricBatches []*agentpb.StreamMetricsResponse
	traceBatches  []*agentpb.StreamTracesResponse
}

func (s *fakeTelemetryServer) StreamLogs(_ *agentpb.StreamLogsRequest, stream agentpb.WendyTelemetryService_StreamLogsServer) error {
	for _, b := range s.logBatches {
		if err := stream.Send(b); err != nil {
			return err
		}
	}
	return nil
}

func (s *fakeTelemetryServer) StreamMetrics(_ *agentpb.StreamMetricsRequest, stream agentpb.WendyTelemetryService_StreamMetricsServer) error {
	for _, b := range s.metricBatches {
		if err := stream.Send(b); err != nil {
			return err
		}
	}
	return nil
}

func (s *fakeTelemetryServer) StreamTraces(_ *agentpb.StreamTracesRequest, stream agentpb.WendyTelemetryService_StreamTracesServer) error {
	for _, b := range s.traceBatches {
		if err := stream.Send(b); err != nil {
			return err
		}
	}
	return nil
}

func startFakeTelemetryServer(t *testing.T, fake *fakeTelemetryServer) *grpcclient.AgentConnection {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	g := grpc.NewServer()
	agentpb.RegisterWendyTelemetryServiceServer(g, fake)
	go func() { _ = g.Serve(ln) }()
	t.Cleanup(func() { g.Stop() })

	conn, err := grpc.NewClient(ln.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return &grpcclient.AgentConnection{
		Conn:             conn,
		TelemetryService: agentpb.NewWendyTelemetryServiceClient(conn),
	}
}

func TestTelemetryLogs_NotConnected(t *testing.T) {
	srv := New(&config.Config{}, nil)
	result, err := srv.callTool(context.Background(), "telemetry_logs", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected IsError=true when not connected")
	}
}

func TestTelemetryLogs_ReturnsJSON(t *testing.T) {
	fake := &fakeTelemetryServer{
		logBatches: []*agentpb.StreamLogsResponse{
			{}, {}, {Logs: &collogspb.ExportLogsServiceRequest{}}, {},
		},
	}
	conn := startFakeTelemetryServer(t, fake)
	srv := New(&config.Config{}, nil)
	srv.SetConn(conn)

	result, err := srv.callTool(context.Background(), "telemetry_logs", map[string]any{"max_batches": 1})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected error result: %v", result.Content)
	}
	if batches := listPayload(t, result, "batches"); len(batches) != 1 {
		t.Errorf("expected 1 batch, got %d", len(batches))
	}
}

func TestTelemetryLogs_HasStructuredContent(t *testing.T) {
	fake := &fakeTelemetryServer{
		logBatches: []*agentpb.StreamLogsResponse{
			{Logs: &collogspb.ExportLogsServiceRequest{}},
		},
	}
	conn := startFakeTelemetryServer(t, fake)
	srv := New(&config.Config{}, nil)
	srv.SetConn(conn)

	result, err := srv.callTool(context.Background(), "telemetry_logs", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected error result: %v", result.Content)
	}
	if _, ok := structuredMap(t, result)["batches"]; !ok {
		t.Error("telemetry_logs envelope is missing the batches key")
	}
}

func TestTelemetryLogs_EmptyReturnsEmptyList(t *testing.T) {
	fake := &fakeTelemetryServer{}
	conn := startFakeTelemetryServer(t, fake)
	srv := New(&config.Config{}, nil)
	srv.SetConn(conn)

	result, err := srv.callTool(context.Background(), "telemetry_logs", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected error result: %v", result.Content)
	}
	// An empty stream must still be an object envelope with the key present,
	// not a bare [] — conformant clients reject a non-object structuredContent.
	if batches := listPayload(t, result, "batches"); len(batches) != 0 {
		t.Errorf("expected no batches, got %v", batches)
	}
}

func TestTelemetryMetrics_ReturnsJSON(t *testing.T) {
	fake := &fakeTelemetryServer{
		metricBatches: []*agentpb.StreamMetricsResponse{
			{Metrics: &colmetricspb.ExportMetricsServiceRequest{}},
		},
	}
	conn := startFakeTelemetryServer(t, fake)
	srv := New(&config.Config{}, nil)
	srv.SetConn(conn)

	result, err := srv.callTool(context.Background(), "telemetry_metrics", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected error result: %v", result.Content)
	}
	if batches := listPayload(t, result, "batches"); len(batches) != 1 {
		t.Errorf("expected 1 batch, got %d", len(batches))
	}
}

func TestTelemetryTraces_ReturnsJSON(t *testing.T) {
	fake := &fakeTelemetryServer{
		traceBatches: []*agentpb.StreamTracesResponse{
			{Traces: &coltracepb.ExportTraceServiceRequest{}},
		},
	}
	conn := startFakeTelemetryServer(t, fake)
	srv := New(&config.Config{}, nil)
	srv.SetConn(conn)

	result, err := srv.callTool(context.Background(), "telemetry_traces", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected error result: %v", result.Content)
	}
	if batches := listPayload(t, result, "batches"); len(batches) != 1 {
		t.Errorf("expected 1 batch, got %d", len(batches))
	}
}
