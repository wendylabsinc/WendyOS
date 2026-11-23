package services

import (
	"slices"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	colmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
)

// inferenceMetrics is one throughput report of a campaign's model process as
// OpenTelemetry metrics, in scope wendy.data.inference. It is built from the
// values inference_status carries at that moment, so that campaign inspect and
// the metrics cannot disagree, and it is nil without a report.
//
//	inference.rate.requested     gauge, {frame}/s   throughput.requested_fps
//	inference.rate.achieved      gauge, {frame}/s   stats.fps
//	inference.model.too_slow     gauge, 0 or 1      throughput.model_too_slow
//	inference.frame.duration_ms  gauge, ms          stats.infer_ms_p50 and _p95, as inference.quantile p50 and p95
//	inference.frames.scored      cumulative sum     stats.frames_scored
//
// Every point names its campaign and model. The gauges also name the engine
// that scores, when the process has said which. The count does not: it runs
// on through a change of engine, so a point named after the new engine would
// begin a series at a count the old engine's series already holds.
//
// The requested rate and the verdict are there only when the status has a
// throughput: with no camera being scored, or nothing scored lately, the
// status compares nothing and neither do the metrics. A frame duration is
// there only when the process timed a frame; it reports 0 when it has not,
// which is no duration.
//
// started is when the model process started, which is where its count of
// scored frames begins: both sources of that count, the worker's scoring loop
// and the YOLO detector's accelerator, count from zero in every new process
// and never reset within one. A restarted worker therefore has a later
// started, as a cumulative sum that begins again requires.
func inferenceMetrics(campaign data.Campaign, status data.InferenceStatus, started, now time.Time) *colmetricspb.ExportMetricsServiceRequest {
	stats := status.Stats
	if stats == nil {
		return nil
	}
	counted := []*commonpb.KeyValue{
		stringKV("inference.campaign", boundedLabel(campaign.Name)),
		stringKV("inference.model", boundedLabel(campaign.Inference.Model)),
	}
	attributes := counted
	if accelerator := status.Accelerator; accelerator != nil && accelerator.Active != "" {
		attributes = append(slices.Clip(counted), stringKV("inference.engine", boundedLabel(accelerator.Active)))
	}
	at := uint64(now.UnixNano())
	double := func(value float64, extra ...*commonpb.KeyValue) *metricspb.NumberDataPoint {
		return &metricspb.NumberDataPoint{Attributes: append(slices.Clip(attributes), extra...), TimeUnixNano: at,
			Value: &metricspb.NumberDataPoint_AsDouble{AsDouble: value}}
	}
	gauge := func(name, unit string, points ...*metricspb.NumberDataPoint) *metricspb.Metric {
		return &metricspb.Metric{Name: name, Unit: unit, Data: &metricspb.Metric_Gauge{Gauge: &metricspb.Gauge{DataPoints: points}}}
	}

	var metrics []*metricspb.Metric
	if status.Throughput != nil {
		metrics = append(metrics, gauge("inference.rate.requested", "{frame}/s", double(status.Throughput.RequestedFPS)))
	}
	metrics = append(metrics, gauge("inference.rate.achieved", "{frame}/s", double(stats.FPS)))
	if status.Throughput != nil {
		tooSlow := int64(0)
		if status.Throughput.ModelTooSlow {
			tooSlow = 1
		}
		metrics = append(metrics, gauge("inference.model.too_slow", "1", &metricspb.NumberDataPoint{
			Attributes: attributes, TimeUnixNano: at, Value: &metricspb.NumberDataPoint_AsInt{AsInt: tooSlow}}))
	}
	var durations []*metricspb.NumberDataPoint
	for _, quantile := range []struct {
		name string
		ms   float64
	}{{"p50", stats.InferMsP50}, {"p95", stats.InferMsP95}} {
		if quantile.ms > 0 {
			durations = append(durations, double(quantile.ms, stringKV("inference.quantile", quantile.name)))
		}
	}
	if len(durations) > 0 {
		metrics = append(metrics, gauge("inference.frame.duration_ms", "ms", durations...))
	}
	metrics = append(metrics, &metricspb.Metric{Name: "inference.frames.scored", Unit: "{frame}", Data: &metricspb.Metric_Sum{Sum: &metricspb.Sum{
		IsMonotonic:            true,
		AggregationTemporality: metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE,
		DataPoints: []*metricspb.NumberDataPoint{{
			Attributes: counted, StartTimeUnixNano: uint64(started.UnixNano()), TimeUnixNano: at,
			Value: &metricspb.NumberDataPoint_AsInt{AsInt: int64(stats.FramesScored)}}},
	}}})

	return &colmetricspb.ExportMetricsServiceRequest{ResourceMetrics: []*metricspb.ResourceMetrics{{
		Resource: newAgentResource(),
		ScopeMetrics: []*metricspb.ScopeMetrics{{
			Scope:   &commonpb.InstrumentationScope{Name: "wendy.data.inference"},
			Metrics: metrics,
		}},
	}}}
}

// boundedLabel cuts an attribute value to what the Cloud export keeps of one
// (sanitizeAttributes), so that a metric reads the same on the device and
// after export. A campaign's name is at most 128 bytes, but its model's name
// is checked for its characters only, which are ASCII, and not for its
// length.
func boundedLabel(value string) string {
	if len(value) > maxLabelValLen {
		return value[:maxLabelValLen]
	}
	return value
}

// throughputMetrics is the latest throughput report as metrics, or nil when
// the agent publishes none. Callers hold j.mu, and publish without it.
func (j *campaignInferenceJob) throughputMetrics(now time.Time) *colmetricspb.ExportMetricsServiceRequest {
	if j.owner.service.telemetry == nil {
		return nil
	}
	return inferenceMetrics(j.campaign, j.status, j.scoringSince, now)
}
