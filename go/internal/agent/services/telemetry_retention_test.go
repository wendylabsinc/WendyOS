package services

import (
	"fmt"
	"runtime"
	"testing"

	colmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	"google.golang.org/protobuf/proto"
)

const retentionService = "sensor-histograms"

// Each resource describes one process instance reporting a histogram per sensor.
// Restarting the process changes instance.id while keeping service.name stable.
func retentionMetrics(instance, series int) *colmetricspb.ExportMetricsServiceRequest {
	points := make([]*metricspb.HistogramDataPoint, series)
	for i := range points {
		bounds := make([]float64, 64)
		counts := make([]uint64, len(bounds)+1)
		for j := range bounds {
			bounds[j] = float64(j + 1)
			counts[j] = 1
		}
		points[i] = &metricspb.HistogramDataPoint{
			Attributes: []*commonpb.KeyValue{stringKV("sensor.id", fmt.Sprint(i))},
			Count:      uint64(len(bounds)), ExplicitBounds: bounds, BucketCounts: counts,
			TimeUnixNano: uint64(instance + 1),
		}
	}
	return &colmetricspb.ExportMetricsServiceRequest{ResourceMetrics: []*metricspb.ResourceMetrics{{
		Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{
			stringKV("service.name", retentionService), stringKV("instance.id", fmt.Sprint(instance)),
		}},
		ScopeMetrics: []*metricspb.ScopeMetrics{{Metrics: []*metricspb.Metric{{
			Name: "sensor.sample.duration", Unit: "ms",
			Data: &metricspb.Metric_Histogram{Histogram: &metricspb.Histogram{
				AggregationTemporality: metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE,
				DataPoints:             points,
			}},
		}}}},
	}}}
}

func assertRetentionInstances(t testing.TB, req *colmetricspb.ExportMetricsServiceRequest, first int) {
	t.Helper()
	if len(req.GetResourceMetrics()) != maxCachedResourcesPerService {
		t.Fatalf("cached resource count = %d; want %d", len(req.GetResourceMetrics()), maxCachedResourcesPerService)
	}
	for i, rm := range req.ResourceMetrics {
		if got, want := rm.Resource.Attributes[1].Value.GetStringValue(), fmt.Sprint(first+i); got != want {
			t.Fatalf("resource %d instance = %q; want %q", i, got, want)
		}
	}
}

func TestBroadcasterMetricsEvictionReleasesReferences(t *testing.T) {
	b := NewTelemetryBroadcaster()
	for i := range maxCachedResourcesPerService {
		b.PublishMetrics(retentionMetrics(i, 2))
	}
	cached := b.latestMetrics[retentionService]
	// Keep the allocation visible to inspect ownership after eviction. This is
	// separate from the heap benchmark, which retains only the broadcaster.
	backing := make([]*metricspb.ResourceMetrics, maxCachedResourcesPerService+1)
	copy(backing, cached.ResourceMetrics)
	cached.ResourceMetrics = backing[:maxCachedResourcesPerService]
	id, updates := b.SubscribeMetrics()
	defer b.UnsubscribeMetrics(id)
	snapshot := <-updates
	before := proto.Clone(snapshot)
	incoming := retentionMetrics(maxCachedResourcesPerService, 2)
	incomingBefore := proto.Clone(incoming)
	b.PublishMetrics(incoming)
	if backing[0] != nil {
		t.Error("evicted resource still retained by the cache backing array")
	}
	assertRetentionInstances(t, b.latestMetrics[retentionService], 1)
	assertRetentionInstances(t, snapshot, 0)
	if !proto.Equal(snapshot, before) || !proto.Equal(incoming, incomingBefore) {
		t.Error("eviction changed a subscriber snapshot or the publisher's request")
	}
	if got := <-updates; got != incoming {
		t.Error("live subscriber did not receive the original request")
	}

	// A partial update must still retain the original metric for that resource.
	partial := retentionMetrics(maxCachedResourcesPerService, 0)
	partial.ResourceMetrics[0].ScopeMetrics[0].Metrics = []*metricspb.Metric{{Name: "sensor.other"}}
	b.PublishMetrics(partial)
	last := b.latestMetrics[retentionService].ResourceMetrics[maxCachedResourcesPerService-1]
	if len(last.ScopeMetrics[0].Metrics) != 2 {
		t.Fatal("partial update lost a cached metric")
	}
}

// BenchmarkTelemetryMetricsRetainedHeap measures reachable heap after a fixed
// restart history, not cumulative allocations or process RSS. Run in a fresh
// process with -benchtime=1x for comparisons. There are no live subscribers or
// retained input requests; the broadcaster alone keeps the latest 100 resources.
func BenchmarkTelemetryMetricsRetainedHeap(b *testing.B) {
	for _, series := range []int{16, 256} {
		b.Run(fmt.Sprintf("series=%d", series), func(b *testing.B) {
			const instances = 128
			serialized := proto.Size(retentionMetrics(0, series))
			runtime.GC()
			var before runtime.MemStats
			runtime.ReadMemStats(&before)
			var broadcaster *TelemetryBroadcaster
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				broadcaster = publishRetentionHistory(instances, series)
			}
			b.StopTimer()
			runtime.GC()
			var after runtime.MemStats
			runtime.ReadMemStats(&after)
			assertRetentionInstances(b, broadcaster.latestMetrics[retentionService], instances-maxCachedResourcesPerService)
			b.ReportMetric(float64(int64(after.HeapAlloc)-int64(before.HeapAlloc)), "retained-B")
			b.ReportMetric(float64(serialized), "resource-wire-B")
			runtime.KeepAlive(broadcaster)
		})
	}
}

func publishRetentionHistory(instances, series int) *TelemetryBroadcaster {
	b := NewTelemetryBroadcaster()
	for i := range instances {
		b.PublishMetrics(retentionMetrics(i, series))
	}
	return b
}
