package mcp

import (
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	colmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/encoding/protojson"
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

func startFakeTelemetryServer(t *testing.T, fake agentpb.WendyTelemetryServiceServer) *grpcclient.AgentConnection {
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

type fakeMetricsReplayServer struct {
	agentpb.UnimplementedWendyTelemetryServiceServer
	batches  []*agentpb.StreamMetricsResponse
	requests chan *agentpb.StreamMetricsRequest
	canceled chan struct{}
}

func (s *fakeMetricsReplayServer) StreamMetrics(req *agentpb.StreamMetricsRequest, stream agentpb.WendyTelemetryService_StreamMetricsServer) error {
	s.requests <- req
	batches := s.batches
	if n := int(req.GetLastN()); n > 0 && len(batches) > n {
		batches = batches[len(batches)-n:]
	}
	for _, batch := range batches {
		if err := stream.Send(batch); err != nil {
			return err
		}
	}
	// Match the agent protocol: after replay, remain subscribed for live data.
	<-stream.Context().Done()
	close(s.canceled)
	return stream.Context().Err()
}

func metricReplayBatch(t *testing.T, timestamp uint64) *agentpb.StreamMetricsResponse {
	t.Helper()
	var batch agentpb.StreamMetricsResponse
	err := protojson.Unmarshal([]byte(fmt.Sprintf(`{"isHistory":true,"metrics":{"resourceMetrics":[{"scopeMetrics":[{"metrics":[{"name":"cpu.time","unit":"s","sum":{"aggregationTemporality":"AGGREGATION_TEMPORALITY_CUMULATIVE","isMonotonic":true,"dataPoints":[{"timeUnixNano":"%d","asDouble":2}]}}]}]}]}}`, timestamp)), &batch)
	if err != nil {
		t.Fatal(err)
	}
	return &batch
}

func TestTelemetryMetricsHistoryKeepsLatestTailWithinBudgets(t *testing.T) {
	const first = uint64(1700000000000000001)
	fake := &fakeMetricsReplayServer{requests: make(chan *agentpb.StreamMetricsRequest, 1), canceled: make(chan struct{})}
	for i := uint64(0); i < 5; i++ {
		fake.batches = append(fake.batches, metricReplayBatch(t, first+i))
	}
	s := New(&config.Config{}, nil)
	s.SetConn(startFakeTelemetryServer(t, fake))
	result, err := s.callTool(context.Background(), "telemetry_metrics", map[string]any{"last_n": 5, "max_batches": 2, "max_records": 1, "app_name": "vision", "service_name": "camera", "metric_name_prefix": "cpu."})
	if err != nil || result.IsError {
		t.Fatalf("history: %v %v", result, err)
	}
	request := <-fake.requests
	if request.GetLastN() != 2 || request.GetAppName() != "vision" || request.GetServiceName() != "camera" || request.GetMetricNamePrefix() != "cpu." {
		t.Fatalf("wrong bounded replay request: %v", request)
	}
	rows := listPayload(t, result, "metrics")
	if len(rows) != 1 || rows[0]["timeUnixNano"] != fmt.Sprint(first+4) || rows[0]["isMonotonic"] != true || rows[0]["aggregationTemporality"] != "AGGREGATION_TEMPORALITY_CUMULATIVE" {
		t.Fatalf("row budget lost latest point or metric semantics: %v", rows)
	}
	data := structuredMap(t, result)
	if data["history_batches"] != 2 || data["omitted"] != 1 || data["newest_first"] != true {
		t.Fatalf("incorrect history metadata: %v", data)
	}
	select {
	case <-fake.canceled:
	case <-time.After(time.Second):
		t.Fatal("completed history collection retained a live subscription")
	}
}

func TestTelemetryMetricsSparseHistoryDoesNotWaitForLiveData(t *testing.T) {
	fake := &fakeMetricsReplayServer{requests: make(chan *agentpb.StreamMetricsRequest, 1), canceled: make(chan struct{}), batches: []*agentpb.StreamMetricsResponse{metricReplayBatch(t, 1700000000000000001), metricReplayBatch(t, 1700000001000000001)}}
	s := New(&config.Config{}, nil)
	s.SetConn(startFakeTelemetryServer(t, fake))
	started := time.Now()
	result, err := s.callTool(context.Background(), "telemetry_metrics", map[string]any{"last_n": 20, "max_batches": 20})
	if err != nil || result.IsError || time.Since(started) > time.Second {
		t.Fatalf("sparse history waited for live data: elapsed=%s result=%v error=%v", time.Since(started), result, err)
	}
	if len(listPayload(t, result, "metrics")) != 2 || structuredMap(t, result)["history_batches"] != 2 {
		t.Fatalf("sparse replay lost history: %v", result)
	}
	select {
	case <-fake.canceled:
	case <-time.After(time.Second):
		t.Fatal("idle history collection retained a live subscription")
	}
}

func TestTelemetryMetricsHistoryRespectsCallerCancellation(t *testing.T) {
	fake := &fakeMetricsReplayServer{requests: make(chan *agentpb.StreamMetricsRequest, 1), canceled: make(chan struct{})}
	s := New(&config.Config{}, nil)
	s.SetConn(startFakeTelemetryServer(t, fake))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Cancel from the caller after the subscription opens, rather than racing
	// the client's deadline timer against the server's propagated deadline.
	go func() {
		select {
		case <-fake.requests:
			cancel()
		case <-ctx.Done():
		}
	}()
	started := time.Now()
	result, err := s.handleTelemetryMetrics(ctx, callToolReq("telemetry_metrics", map[string]any{"last_n": 20, "max_batches": 20}))
	if err != nil || result.IsError || len(listPayload(t, result, "metrics")) != 0 || time.Since(started) > time.Second {
		t.Fatalf("empty replay did not honor cancellation: %v %v", result, err)
	}
}

func TestTelemetryMetricsRejectsInvalidHistoryWindow(t *testing.T) {
	s := New(&config.Config{}, nil)
	for _, value := range []any{0, -1, 101, 1.5, true, "20"} {
		result, err := s.handleTelemetryMetrics(context.Background(), callToolReq("telemetry_metrics", map[string]any{"last_n": value}))
		if err != nil || !result.IsError || !strings.Contains(toolResultText(t, result), "last_n") {
			t.Fatalf("invalid last_n accepted: %v, result=%v error=%v", value, result, err)
		}
	}
}

func TestTelemetryLogs_NotConnected(t *testing.T) {
	srv := New(&config.Config{}, nil)
	result, err := srv.callTool(context.Background(), "telemetry_logs", map[string]any{"format": "otlp"})
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

	result, err := srv.callTool(context.Background(), "telemetry_logs", map[string]any{"max_batches": 1, "format": "otlp"})
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

	result, err := srv.callTool(context.Background(), "telemetry_logs", map[string]any{"format": "otlp"})
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

	result, err := srv.callTool(context.Background(), "telemetry_logs", map[string]any{"format": "otlp"})
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

	result, err := srv.callTool(context.Background(), "telemetry_metrics", map[string]any{"format": "otlp"})
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

	result, err := srv.callTool(context.Background(), "telemetry_traces", map[string]any{"format": "otlp"})
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
