package services

import (
	"context"
	"strconv"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"

	"go.uber.org/zap"

	colmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
)

// meshDialDurationBoundsMs are the explicit histogram bucket upper bounds (ms)
// for mesh.dial.duration_ms, chosen to resolve both fast LAN-direct dials
// (single digit ms) and slower cloud-relay round trips (hundreds of ms) with
// a coarse tail bucket for outliers.
var meshDialDurationBoundsMs = []float64{5, 10, 25, 50, 100, 250, 500, 1000, 2500}

// meshConnKey identifies one mesh.connections timeseries. peer is the mesh
// peer label: a device UUID for app-session flows, a legacy asset id for the
// LAN-relay/build-delivery flows.
type meshConnKey struct {
	peer   string
	mode   string
	result string
}

// meshBytesKey identifies one mesh.bytes timeseries.
type meshBytesKey struct {
	peer string
	dir  string
}

// meshDialKey identifies one mesh.dial.duration_ms timeseries.
type meshDialKey struct {
	peer string
	mode string
}

// meshHistogram is the cumulative bucket/sum/count state for one
// mesh.dial.duration_ms timeseries. Like the Sum counters below, it is never
// reset between publishes: OTel cumulative temporality expects monotonically
// increasing counts measured from a fixed start time.
type meshHistogram struct {
	bucketCounts []uint64 // len(meshDialDurationBoundsMs)+1
	sum          float64
	count        uint64
}

// MeshMetrics accumulates mesh data-plane connection metrics in memory and
// flushes them as OTel metrics on a 15s tick, mirroring the
// CollectAgentMetrics/publishProcessMetrics pattern used for process and
// container metrics. RecordDial and RecordBytes are cheap (mutex + map
// update) so callers on the dial/relay hot path can call them inline.
type MeshMetrics struct {
	pub    TelemetryPublisher
	logger *zap.Logger

	startTime time.Time

	mu    sync.Mutex
	conns map[meshConnKey]uint64
	bytes map[meshBytesKey]int64
	dials map[meshDialKey]*meshHistogram
}

// NewMeshMetrics builds a MeshMetrics that publishes through pub.
func NewMeshMetrics(pub TelemetryPublisher, logger *zap.Logger) *MeshMetrics {
	return &MeshMetrics{
		pub:       pub,
		logger:    logger,
		startTime: time.Now(),
		conns:     make(map[meshConnKey]uint64),
		bytes:     make(map[meshBytesKey]int64),
		dials:     make(map[meshDialKey]*meshHistogram),
	}
}

// RecordDial records the outcome and wall-clock duration of one app-session
// dial attempt toward a device principal. Safe for concurrent use; safe to
// call on a nil *MeshMetrics (a no-op), so callers that construct a dialer
// without metrics wired up don't need extra nil checks.
func (m *MeshMetrics) RecordDial(device meshid.Device, mode, result string, durMs float64) {
	m.recordDial(device.UUID().String(), mode, result, durMs)
}

// RecordAssetDial records one dial attempt of the legacy asset-keyed paths
// (LAN-direct MeshDial, cloud relay, build delivery), which address peers by
// cloud asset id rather than device principal.
func (m *MeshMetrics) RecordAssetDial(assetID int32, mode, result string, durMs float64) {
	m.recordDial(strconv.Itoa(int(assetID)), mode, result, durMs)
}

func (m *MeshMetrics) recordDial(peer, mode, result string, durMs float64) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.conns[meshConnKey{peer: peer, mode: mode, result: result}]++

	hk := meshDialKey{peer: peer, mode: mode}
	h, ok := m.dials[hk]
	if !ok {
		h = &meshHistogram{bucketCounts: make([]uint64, len(meshDialDurationBoundsMs)+1)}
		m.dials[hk] = h
	}
	h.sum += durMs
	h.count++
	idx := len(meshDialDurationBoundsMs)
	for i, bound := range meshDialDurationBoundsMs {
		if durMs <= bound {
			idx = i
			break
		}
	}
	h.bucketCounts[idx]++
}

// RecordBytes accumulates n bytes transferred in direction dir ("tx"/"rx")
// with the device principal peer. Safe for concurrent use; safe to call on a
// nil *MeshMetrics.
func (m *MeshMetrics) RecordBytes(device meshid.Device, dir string, n int64) {
	if m == nil || n <= 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.bytes[meshBytesKey{peer: device.UUID().String(), dir: dir}] += n
}

// Collect flushes accumulated mesh metrics as one OTel export every 15s until
// ctx is cancelled. Run it as a goroutine from main.
func (m *MeshMetrics) Collect(ctx context.Context) {
	resource := newAgentResource()
	ticker := time.NewTicker(metricsCollectionInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case t := <-ticker.C:
			m.publish(resource, t)
		}
	}
}

// publish snapshots the accumulated counters and builds+publishes one
// ExportMetricsServiceRequest. Snapshotting under the lock (rather than
// holding it across PublishMetrics) keeps RecordDial/RecordBytes from ever
// blocking on network/disk I/O in the publisher.
func (m *MeshMetrics) publish(resource *resourcepb.Resource, t time.Time) {
	m.mu.Lock()
	connsSnap := make(map[meshConnKey]uint64, len(m.conns))
	for k, v := range m.conns {
		connsSnap[k] = v
	}
	bytesSnap := make(map[meshBytesKey]int64, len(m.bytes))
	for k, v := range m.bytes {
		bytesSnap[k] = v
	}
	dialsSnap := make(map[meshDialKey]meshHistogram, len(m.dials))
	for k, h := range m.dials {
		counts := make([]uint64, len(h.bucketCounts))
		copy(counts, h.bucketCounts)
		dialsSnap[k] = meshHistogram{bucketCounts: counts, sum: h.sum, count: h.count}
	}
	m.mu.Unlock()

	if len(connsSnap) == 0 && len(bytesSnap) == 0 && len(dialsSnap) == 0 {
		return
	}

	nowNano := uint64(t.UnixNano())
	startNano := uint64(m.startTime.UnixNano())
	metrics := make([]*metricspb.Metric, 0, 3)

	if len(connsSnap) > 0 {
		dps := make([]*metricspb.NumberDataPoint, 0, len(connsSnap))
		for k, v := range connsSnap {
			dps = append(dps, &metricspb.NumberDataPoint{
				Attributes: []*commonpb.KeyValue{
					stringKV("mesh.peer", k.peer),
					stringKV("mesh.mode", k.mode),
					stringKV("mesh.result", k.result),
				},
				StartTimeUnixNano: startNano,
				TimeUnixNano:      nowNano,
				Value:             &metricspb.NumberDataPoint_AsInt{AsInt: int64(v)},
			})
		}
		metrics = append(metrics, &metricspb.Metric{
			Name: "mesh.connections",
			Unit: "1",
			Data: &metricspb.Metric_Sum{
				Sum: &metricspb.Sum{
					IsMonotonic:            true,
					AggregationTemporality: metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE,
					DataPoints:             dps,
				},
			},
		})
	}

	if len(bytesSnap) > 0 {
		dps := make([]*metricspb.NumberDataPoint, 0, len(bytesSnap))
		for k, v := range bytesSnap {
			dps = append(dps, &metricspb.NumberDataPoint{
				Attributes: []*commonpb.KeyValue{
					stringKV("mesh.peer", k.peer),
					stringKV("mesh.dir", k.dir),
				},
				StartTimeUnixNano: startNano,
				TimeUnixNano:      nowNano,
				Value:             &metricspb.NumberDataPoint_AsInt{AsInt: v},
			})
		}
		metrics = append(metrics, &metricspb.Metric{
			Name: "mesh.bytes",
			Unit: "By",
			Data: &metricspb.Metric_Sum{
				Sum: &metricspb.Sum{
					IsMonotonic:            true,
					AggregationTemporality: metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE,
					DataPoints:             dps,
				},
			},
		})
	}

	if len(dialsSnap) > 0 {
		dps := make([]*metricspb.HistogramDataPoint, 0, len(dialsSnap))
		for k, h := range dialsSnap {
			sum := h.sum
			dps = append(dps, &metricspb.HistogramDataPoint{
				Attributes: []*commonpb.KeyValue{
					stringKV("mesh.peer", k.peer),
					stringKV("mesh.mode", k.mode),
				},
				StartTimeUnixNano: startNano,
				TimeUnixNano:      nowNano,
				Count:             h.count,
				Sum:               &sum,
				BucketCounts:      h.bucketCounts,
				ExplicitBounds:    meshDialDurationBoundsMs,
			})
		}
		metrics = append(metrics, &metricspb.Metric{
			Name: "mesh.dial.duration_ms",
			Unit: "ms",
			Data: &metricspb.Metric_Histogram{
				Histogram: &metricspb.Histogram{
					AggregationTemporality: metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE,
					DataPoints:             dps,
				},
			},
		})
	}

	m.pub.PublishMetrics(&colmetricspb.ExportMetricsServiceRequest{
		ResourceMetrics: []*metricspb.ResourceMetrics{
			{
				Resource: resource,
				ScopeMetrics: []*metricspb.ScopeMetrics{
					{
						Scope:   &commonpb.InstrumentationScope{Name: "wendy.mesh"},
						Metrics: metrics,
					},
				},
			},
		},
	})
}
