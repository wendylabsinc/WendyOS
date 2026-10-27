package services

import (
	"testing"
	"time"

	"go.uber.org/zap"

	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	colmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
)

// fakeTelemetryPublisher captures PublishMetrics/PublishLogs/PublishTraces
// calls for assertions, without needing the full TelemetryBuffer/Broadcaster
// (disk I/O, batching, etc). Shared by mesh_metrics_test.go and
// mesh_dialer_test.go.
type fakeTelemetryPublisher struct {
	metricReqs []*colmetricspb.ExportMetricsServiceRequest
	logReqs    []*collogspb.ExportLogsServiceRequest
	traceReqs  []*coltracepb.ExportTraceServiceRequest
}

func (f *fakeTelemetryPublisher) PublishLogs(req *collogspb.ExportLogsServiceRequest) {
	f.logReqs = append(f.logReqs, req)
}
func (f *fakeTelemetryPublisher) PublishMetrics(req *colmetricspb.ExportMetricsServiceRequest) {
	f.metricReqs = append(f.metricReqs, req)
}
func (f *fakeTelemetryPublisher) PublishTraces(req *coltracepb.ExportTraceServiceRequest) {
	f.traceReqs = append(f.traceReqs, req)
}

var _ TelemetryPublisher = (*fakeTelemetryPublisher)(nil)

func findMetric(req *colmetricspb.ExportMetricsServiceRequest, name string) *metricspb.Metric {
	for _, rm := range req.GetResourceMetrics() {
		for _, sm := range rm.GetScopeMetrics() {
			for _, m := range sm.GetMetrics() {
				if m.GetName() == name {
					return m
				}
			}
		}
	}
	return nil
}

func scopeName(req *colmetricspb.ExportMetricsServiceRequest) string {
	for _, rm := range req.GetResourceMetrics() {
		for _, sm := range rm.GetScopeMetrics() {
			return sm.GetScope().GetName()
		}
	}
	return ""
}

func attrString(attrs []*commonpb.KeyValue, key string) (string, bool) {
	for _, a := range attrs {
		if a.GetKey() == key {
			return a.GetValue().GetStringValue(), true
		}
	}
	return "", false
}

func TestMeshMetricsPublishesConnectionsBytesAndDialDuration(t *testing.T) {
	pub := &fakeTelemetryPublisher{}
	m := NewMeshMetrics(pub, zap.NewNop())
	device := appTestMeshDevice(t)

	m.RecordDial(device, "lan-direct", "ok", 3)
	m.RecordDial(device, "lan-direct", "ok", 4)
	m.RecordDial(device, "relay", "error", 120)
	m.RecordBytes(device, "tx", 100)
	m.RecordBytes(device, "tx", 50)
	m.RecordBytes(device, "rx", 30)

	m.publish(newAgentResource(), time.Now())

	if len(pub.metricReqs) != 1 {
		t.Fatalf("PublishMetrics called %d times, want 1", len(pub.metricReqs))
	}
	req := pub.metricReqs[0]
	if got := scopeName(req); got != "wendy.mesh" {
		t.Fatalf("scope = %q, want wendy.mesh", got)
	}

	conns := findMetric(req, "mesh.connections")
	if conns == nil {
		t.Fatal("mesh.connections metric missing")
	}
	sum := conns.GetSum()
	if sum == nil || !sum.GetIsMonotonic() || sum.GetAggregationTemporality() != metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE {
		t.Fatalf("mesh.connections not a monotonic cumulative sum: %+v", sum)
	}
	var okCount, errCount int64
	for _, dp := range sum.GetDataPoints() {
		peer, _ := attrString(dp.GetAttributes(), "mesh.peer")
		mode, _ := attrString(dp.GetAttributes(), "mesh.mode")
		result, _ := attrString(dp.GetAttributes(), "mesh.result")
		if peer != device.UUID().String() {
			t.Fatalf("mesh.peer = %q, want %s", peer, device.UUID())
		}
		switch {
		case mode == "lan-direct" && result == "ok":
			okCount = dp.GetAsInt()
		case mode == "relay" && result == "error":
			errCount = dp.GetAsInt()
		}
	}
	if okCount != 2 {
		t.Fatalf("lan-direct/ok count = %d, want 2", okCount)
	}
	if errCount != 1 {
		t.Fatalf("relay/error count = %d, want 1", errCount)
	}

	bytesMetric := findMetric(req, "mesh.bytes")
	if bytesMetric == nil {
		t.Fatal("mesh.bytes metric missing")
	}
	bsum := bytesMetric.GetSum()
	if bsum == nil || !bsum.GetIsMonotonic() {
		t.Fatal("mesh.bytes not a monotonic sum")
	}
	var tx, rx int64
	for _, dp := range bsum.GetDataPoints() {
		peer, _ := attrString(dp.GetAttributes(), "mesh.peer")
		if peer != device.UUID().String() {
			t.Fatalf("mesh.peer = %q, want %s", peer, device.UUID())
		}
		dir, _ := attrString(dp.GetAttributes(), "mesh.dir")
		switch dir {
		case "tx":
			tx = dp.GetAsInt()
		case "rx":
			rx = dp.GetAsInt()
		}
	}
	if tx != 150 {
		t.Fatalf("tx bytes = %d, want 150", tx)
	}
	if rx != 30 {
		t.Fatalf("rx bytes = %d, want 30", rx)
	}

	dial := findMetric(req, "mesh.dial.duration_ms")
	if dial == nil {
		t.Fatal("mesh.dial.duration_ms metric missing")
	}
	hist := dial.GetHistogram()
	if hist == nil || hist.GetAggregationTemporality() != metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE {
		t.Fatal("mesh.dial.duration_ms is not a cumulative histogram")
	}
	var lanHDP, relayHDP *metricspb.HistogramDataPoint
	for _, dp := range hist.GetDataPoints() {
		mode, _ := attrString(dp.GetAttributes(), "mesh.mode")
		switch mode {
		case "lan-direct":
			lanHDP = dp
		case "relay":
			relayHDP = dp
		}
	}
	if lanHDP == nil || lanHDP.GetCount() != 2 || lanHDP.GetSum() != 7 {
		t.Fatalf("lan-direct histogram = %+v, want count=2 sum=7", lanHDP)
	}
	if relayHDP == nil || relayHDP.GetCount() != 1 || relayHDP.GetSum() != 120 {
		t.Fatalf("relay histogram = %+v, want count=1 sum=120", relayHDP)
	}
}

func TestMeshMetricsPublishSkipsWhenNothingRecorded(t *testing.T) {
	pub := &fakeTelemetryPublisher{}
	m := NewMeshMetrics(pub, zap.NewNop())
	m.publish(newAgentResource(), time.Now())
	if len(pub.metricReqs) != 0 {
		t.Fatalf("PublishMetrics called %d times, want 0 when nothing was recorded", len(pub.metricReqs))
	}
}

// TestMeshMetricsHistogramBucketBoundaries pins down the "<=" inclusive
// upper-bound semantics for the explicit bucket boundaries (matching the
// OTLP HistogramDataPoint doc comment) and the overflow bucket for values
// past the last bound.
func TestMeshMetricsHistogramBucketBoundaries(t *testing.T) {
	pub := &fakeTelemetryPublisher{}
	m := NewMeshMetrics(pub, zap.NewNop())
	device := appTestMeshDevice(t)
	m.RecordDial(device, "relay", "ok", meshDialDurationBoundsMs[0]) // exactly the first bound
	m.RecordDial(device, "relay", "ok", 999999)                      // far past the last bound
	m.publish(newAgentResource(), time.Now())

	dial := findMetric(pub.metricReqs[0], "mesh.dial.duration_ms")
	dps := dial.GetHistogram().GetDataPoints()
	if len(dps) != 1 {
		t.Fatalf("got %d histogram data points, want 1", len(dps))
	}
	counts := dps[0].GetBucketCounts()
	if len(counts) != len(meshDialDurationBoundsMs)+1 {
		t.Fatalf("bucket_counts len = %d, want %d", len(counts), len(meshDialDurationBoundsMs)+1)
	}
	if counts[0] != 1 {
		t.Fatalf("bucket[0] = %d, want 1 (value == bound is inclusive)", counts[0])
	}
	last := len(counts) - 1
	if counts[last] != 1 {
		t.Fatalf("overflow bucket = %d, want 1", counts[last])
	}
}

// TestMeshMetricsNilReceiverIsNoOp proves RecordDial/RecordBytes are safe to
// call on a nil *MeshMetrics, which lets MeshDialer/Proxy be constructed
// without wiring metrics (as the existing dialer/proxy tests do) without a
// forest of nil-checks at every call site.
func TestMeshMetricsNilReceiverIsNoOp(t *testing.T) {
	var m *MeshMetrics
	m.RecordDial(appTestMeshDevice(t), "lan-direct", "ok", 5)
	m.RecordBytes(appTestMeshDevice(t), "tx", 10)
}
