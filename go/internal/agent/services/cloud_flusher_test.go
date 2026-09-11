package services

import (
	"context"
	"errors"
	"sync"
	"testing"

	"go.uber.org/zap"
	"google.golang.org/grpc"

	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	colmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
)

// fakeLogsClient implements collogspb.LogsServiceClient.
type fakeLogsClient struct {
	mu      sync.Mutex
	calls   []*collogspb.ExportLogsServiceRequest
	errOnce error
}

func (f *fakeLogsClient) Export(ctx context.Context, in *collogspb.ExportLogsServiceRequest, opts ...grpc.CallOption) (*collogspb.ExportLogsServiceResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.errOnce != nil {
		err := f.errOnce
		f.errOnce = nil
		return nil, err
	}
	f.calls = append(f.calls, in)
	return &collogspb.ExportLogsServiceResponse{}, nil
}

func (f *fakeLogsClient) count() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.calls) }

// fakeMetricsClient implements colmetricspb.MetricsServiceClient.
type fakeMetricsClient struct {
	mu    sync.Mutex
	calls []*colmetricspb.ExportMetricsServiceRequest
}

func (f *fakeMetricsClient) Export(ctx context.Context, in *colmetricspb.ExportMetricsServiceRequest, opts ...grpc.CallOption) (*colmetricspb.ExportMetricsServiceResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, in)
	return &colmetricspb.ExportMetricsServiceResponse{}, nil
}

func (f *fakeMetricsClient) count() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.calls) }

// fakeTraceClient implements coltracepb.TraceServiceClient.
type fakeTraceClient struct {
	mu    sync.Mutex
	calls []*coltracepb.ExportTraceServiceRequest
}

func (f *fakeTraceClient) Export(ctx context.Context, in *coltracepb.ExportTraceServiceRequest, opts ...grpc.CallOption) (*coltracepb.ExportTraceServiceResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, in)
	return &coltracepb.ExportTraceServiceResponse{}, nil
}

func (f *fakeTraceClient) count() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.calls) }

// logReqWithSensitive builds a log request whose record carries a deny-listed
// attribute, used to assert export-time scrubbing.
func logReqWithSensitive(body string) *collogspb.ExportLogsServiceRequest {
	return &collogspb.ExportLogsServiceRequest{
		ResourceLogs: []*logspb.ResourceLogs{{
			ScopeLogs: []*logspb.ScopeLogs{{
				LogRecords: []*logspb.LogRecord{{
					Body: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: body}},
					Attributes: []*commonpb.KeyValue{
						{Key: "password", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "hunter2"}}},
						{Key: "ok", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "1"}}},
					},
				}},
			}},
		}},
	}
}

func TestCloudFlusher_ExportsAllSignals(t *testing.T) {
	buf, _ := makeTestBuffer(t, 10*1024*1024, 1*1024*1024)
	for i := 0; i < 3; i++ {
		buf.PublishLogs(makeLogReq("log"))
	}
	buf.PublishMetrics(makeMetricReq("m"))
	buf.PublishMetrics(makeMetricReq("m2"))
	buf.PublishTraces(makeTraceReq("s"))

	logs, metrics, traces := &fakeLogsClient{}, &fakeMetricsClient{}, &fakeTraceClient{}
	flusher := NewCloudFlusher(zap.NewNop(), buf, 1, 2)

	if err := flusher.runOnce(context.Background(), logs, metrics, traces); err != nil {
		t.Fatalf("runOnce: %v", err)
	}
	if logs.count() != 3 {
		t.Errorf("want 3 log exports, got %d", logs.count())
	}
	if metrics.count() != 2 {
		t.Errorf("want 2 metric exports, got %d", metrics.count())
	}
	if traces.count() != 1 {
		t.Errorf("want 1 trace export, got %d", traces.count())
	}
}

func TestCloudFlusher_AdvancesCursorPerSignal(t *testing.T) {
	buf, _ := makeTestBuffer(t, 10*1024*1024, 1*1024*1024)
	buf.PublishLogs(makeLogReq("log"))

	logs, metrics, traces := &fakeLogsClient{}, &fakeMetricsClient{}, &fakeTraceClient{}
	flusher := NewCloudFlusher(zap.NewNop(), buf, 1, 2)

	if err := flusher.runOnce(context.Background(), logs, metrics, traces); err != nil {
		t.Fatalf("first runOnce: %v", err)
	}
	if logs.count() != 1 {
		t.Fatalf("want 1 export, got %d", logs.count())
	}
	// Second pass: cursor advanced, nothing new to export.
	if err := flusher.runOnce(context.Background(), logs, metrics, traces); err != nil {
		t.Fatalf("second runOnce: %v", err)
	}
	if logs.count() != 1 {
		t.Errorf("cursor did not advance: want 1 total export, got %d", logs.count())
	}
}

func TestCloudFlusher_RetryOnError(t *testing.T) {
	buf, _ := makeTestBuffer(t, 10*1024*1024, 1*1024*1024)
	buf.PublishLogs(makeLogReq("retry-me"))

	logs := &fakeLogsClient{errOnce: errors.New("transient error")}
	metrics, traces := &fakeMetricsClient{}, &fakeTraceClient{}
	flusher := NewCloudFlusher(zap.NewNop(), buf, 1, 2)

	if err := flusher.runOnce(context.Background(), logs, metrics, traces); err == nil {
		t.Fatal("expected error on first pass, got nil")
	}
	if logs.count() != 0 {
		t.Errorf("want 0 successful exports after error, got %d", logs.count())
	}
	// Second pass: error cleared, cursor was not advanced, entry re-sent.
	if err := flusher.runOnce(context.Background(), logs, metrics, traces); err != nil {
		t.Fatalf("second runOnce: %v", err)
	}
	if logs.count() != 1 {
		t.Errorf("want 1 export after retry, got %d", logs.count())
	}
	got := logs.calls[0].GetResourceLogs()[0].GetScopeLogs()[0].GetLogRecords()[0].GetBody().GetStringValue()
	if got != "retry-me" {
		t.Errorf("re-sent frame body: want %q, got %q", "retry-me", got)
	}
}

func TestCloudFlusher_EmptyBuffer(t *testing.T) {
	buf, _ := makeTestBuffer(t, 10*1024*1024, 1*1024*1024)
	logs, metrics, traces := &fakeLogsClient{}, &fakeMetricsClient{}, &fakeTraceClient{}
	flusher := NewCloudFlusher(zap.NewNop(), buf, 1, 2)

	if err := flusher.runOnce(context.Background(), logs, metrics, traces); err != nil {
		t.Fatalf("expected nil on empty buffer, got %v", err)
	}
	if logs.count()+metrics.count()+traces.count() != 0 {
		t.Errorf("expected no exports on empty buffer")
	}
}

func TestCloudFlusher_ScrubsBeforeExport(t *testing.T) {
	buf, _ := makeTestBuffer(t, 10*1024*1024, 1*1024*1024)
	buf.PublishLogs(logReqWithSensitive("body"))

	logs, metrics, traces := &fakeLogsClient{}, &fakeMetricsClient{}, &fakeTraceClient{}
	flusher := NewCloudFlusher(zap.NewNop(), buf, 1, 2)

	if err := flusher.runOnce(context.Background(), logs, metrics, traces); err != nil {
		t.Fatalf("runOnce: %v", err)
	}
	if logs.count() != 1 {
		t.Fatalf("want 1 export, got %d", logs.count())
	}
	attrs := logs.calls[0].GetResourceLogs()[0].GetScopeLogs()[0].GetLogRecords()[0].GetAttributes()
	if len(attrs) != 1 || attrs[0].GetKey() != "ok" {
		t.Errorf("sensitive attr not scrubbed before export: %+v", attrs)
	}
}

func TestTelemetryCloudEndpointPKI(t *testing.T) {
	endpoint, err := telemetryCloudEndpoint("api.dev.wendy.sh:443", "spiffe://wendy.sh/tenant/t/device/d", "", "")
	if err != nil || endpoint != "devices.dev.wendy.sh:443" {
		t.Fatalf("PKI telemetry endpoint=%q,%v", endpoint, err)
	}
	endpoint, err = telemetryCloudEndpoint("api.dev.wendy.sh:443", "principal", "https://collector.example:4317", "")
	if err != nil || endpoint != "collector.example:4317" {
		t.Fatalf("collector override=%q,%v", endpoint, err)
	}
	endpoint, err = telemetryCloudEndpoint("legacy.example:8501", "", "", "")
	if err != nil || endpoint != "legacy.example:8501" {
		t.Fatalf("legacy endpoint=%q,%v", endpoint, err)
	}
}
