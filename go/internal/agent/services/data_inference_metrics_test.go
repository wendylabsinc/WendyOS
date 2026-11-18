package services

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	"github.com/wendylabsinc/wendy/go/internal/agent/inference"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	colmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	"google.golang.org/protobuf/proto"
)

// recordingPublisher keeps the metric exports it is given, from any goroutine.
type recordingPublisher struct {
	mu      sync.Mutex
	exports []*colmetricspb.ExportMetricsServiceRequest
}

func (p *recordingPublisher) PublishLogs(*collogspb.ExportLogsServiceRequest)     {}
func (p *recordingPublisher) PublishTraces(*coltracepb.ExportTraceServiceRequest) {}
func (p *recordingPublisher) PublishMetrics(req *colmetricspb.ExportMetricsServiceRequest) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.exports = append(p.exports, req)
}

func (p *recordingPublisher) published() []*colmetricspb.ExportMetricsServiceRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.exports)
}

// next waits for the export after the first seen ones.
func (p *recordingPublisher) next(t *testing.T, seen int) *colmetricspb.ExportMetricsServiceRequest {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if exports := p.published(); len(exports) > seen {
			return exports[seen]
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("no metrics were published after the first %d", seen)
	return nil
}

func metricNames(req *colmetricspb.ExportMetricsServiceRequest) []string {
	var names []string
	for _, rm := range req.GetResourceMetrics() {
		for _, sm := range rm.GetScopeMetrics() {
			for _, m := range sm.GetMetrics() {
				names = append(names, m.GetName())
			}
		}
	}
	return names
}

// inferencePoints returns the data points of a gauge or a sum, having checked
// what every point of these metrics has in common: when it was taken and the
// campaign, model and engine it describes.
func inferencePoints(t *testing.T, req *colmetricspb.ExportMetricsServiceRequest, name, unit, engine string, now time.Time) []*metricspb.NumberDataPoint {
	t.Helper()
	metric := findMetric(req, name)
	if metric == nil {
		t.Fatalf("%s is missing from %v", name, metricNames(req))
	}
	if metric.GetUnit() != unit {
		t.Fatalf("%s unit = %q, want %q", name, metric.GetUnit(), unit)
	}
	points := metric.GetGauge().GetDataPoints()
	if sum := metric.GetSum(); sum != nil {
		points = sum.GetDataPoints()
	}
	if len(points) == 0 {
		t.Fatalf("%s has no data points", name)
	}
	for _, point := range points {
		if !now.IsZero() && point.GetTimeUnixNano() != uint64(now.UnixNano()) {
			t.Fatalf("%s taken at %d, want %d", name, point.GetTimeUnixNano(), now.UnixNano())
		}
		want := map[string]string{"inference.campaign": "people-all-cameras", "inference.model": "facebook/detr-resnet-50", "inference.engine": engine}
		for key, value := range want {
			got, ok := attrString(point.GetAttributes(), key)
			if got != value || ok != (value != "") {
				t.Fatalf("%s attribute %s = %q (present %v), want %q", name, key, got, ok, value)
			}
		}
	}
	return points
}

var metricsCampaign = data.Campaign{Name: "people-all-cameras", Inference: &data.CampaignInference{Model: "facebook/detr-resnet-50", Rate: 1}}

func TestInferenceMetricsOfAModelThatMissesItsRate(t *testing.T) {
	started := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	now := started.Add(time.Minute)
	status := data.InferenceStatus{State: "running", Accelerator: cpuOnly, Stats: slowStats,
		Throughput: &data.InferenceThroughput{RequestedFPS: 1, AchievedFPS: 0.11, ModelTooSlow: true, Summary: slowSummary}}
	req := inferenceMetrics(metricsCampaign, status, started, now)

	if len(req.GetResourceMetrics()) != 1 || scopeName(req) != "wendy.data.inference" {
		t.Fatalf("scope %q in %d resources", scopeName(req), len(req.GetResourceMetrics()))
	}
	if service, _ := attrString(req.GetResourceMetrics()[0].GetResource().GetAttributes(), "service.name"); service != "wendy-agent" {
		t.Fatalf("resource service.name = %q", service)
	}
	want := []string{"inference.rate.requested", "inference.rate.achieved", "inference.model.too_slow", "inference.frame.duration_ms", "inference.frames.scored"}
	if names := metricNames(req); !slices.Equal(names, want) {
		t.Fatalf("metrics %v, want %v", names, want)
	}

	for name, value := range map[string]float64{"inference.rate.requested": 1, "inference.rate.achieved": 0.11} {
		points := inferencePoints(t, req, name, "{frame}/s", "cpu", now)
		if findMetric(req, name).GetGauge() == nil || len(points) != 1 || points[0].GetStartTimeUnixNano() != 0 {
			t.Fatalf("%s is not a gauge of one point: %v", name, points)
		}
		if _, double := points[0].GetValue().(*metricspb.NumberDataPoint_AsDouble); !double || points[0].GetAsDouble() != value {
			t.Fatalf("%s = %v, want the double %v", name, points[0].GetValue(), value)
		}
	}

	tooSlow := inferencePoints(t, req, "inference.model.too_slow", "1", "cpu", now)
	if findMetric(req, "inference.model.too_slow").GetGauge() == nil || len(tooSlow) != 1 {
		t.Fatalf("inference.model.too_slow is not a gauge of one point: %v", tooSlow)
	}
	if _, integer := tooSlow[0].GetValue().(*metricspb.NumberDataPoint_AsInt); !integer || tooSlow[0].GetAsInt() != 1 {
		t.Fatalf("inference.model.too_slow = %v, want the integer 1", tooSlow[0].GetValue())
	}

	durations := inferencePoints(t, req, "inference.frame.duration_ms", "ms", "cpu", now)
	if findMetric(req, "inference.frame.duration_ms").GetGauge() == nil || len(durations) != 2 {
		t.Fatalf("inference.frame.duration_ms is not a gauge of two points: %v", durations)
	}
	for i, quantile := range []struct {
		name  string
		value float64
	}{{"p50", 9104}, {"p95", 9377}} {
		if got, _ := attrString(durations[i].GetAttributes(), "inference.quantile"); got != quantile.name || durations[i].GetAsDouble() != quantile.value {
			t.Fatalf("duration point %d = %s %v, want %s %v", i, got, durations[i].GetAsDouble(), quantile.name, quantile.value)
		}
	}

	// The count names no engine: see the engine change below.
	scored := inferencePoints(t, req, "inference.frames.scored", "{frame}", "", now)
	sum := findMetric(req, "inference.frames.scored").GetSum()
	if sum == nil || !sum.GetIsMonotonic() || sum.GetAggregationTemporality() != metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE || len(scored) != 1 {
		t.Fatalf("inference.frames.scored is not a monotonic cumulative sum of one point: %+v", sum)
	}
	if _, integer := scored[0].GetValue().(*metricspb.NumberDataPoint_AsInt); !integer || scored[0].GetAsInt() != 12 {
		t.Fatalf("inference.frames.scored = %v, want the integer 12", scored[0].GetValue())
	}
	if scored[0].GetStartTimeUnixNano() != uint64(started.UnixNano()) {
		t.Fatalf("inference.frames.scored counts from %d, want %d", scored[0].GetStartTimeUnixNano(), started.UnixNano())
	}
}

func TestInferenceMetricsOfAModelThatKeepsUp(t *testing.T) {
	now := time.Now()
	status := data.InferenceStatus{State: "running",
		Accelerator: &data.AcceleratorStatus{Requested: "auto", Active: "tensorrt", Candidates: []string{"tensorrt", "cpu"}},
		Stats:       &data.InferenceStats{FPS: 9.97, InferMsP50: 5.53, InferMsP95: 6.1, FramesScored: 900},
		Throughput:  &data.InferenceThroughput{RequestedFPS: 10, AchievedFPS: 9.97, Summary: "model scores 10 frames/s of the 10 requested (about 5.5 ms per frame on tensorrt)"}}
	req := inferenceMetrics(metricsCampaign, status, now.Add(-time.Hour), now)
	if got := inferencePoints(t, req, "inference.model.too_slow", "1", "tensorrt", now)[0].GetAsInt(); got != 0 {
		t.Fatalf("inference.model.too_slow = %d, want 0", got)
	}
	if got := inferencePoints(t, req, "inference.rate.requested", "{frame}/s", "tensorrt", now)[0].GetAsDouble(); got != 10 {
		t.Fatalf("inference.rate.requested = %v, want 10", got)
	}
	if got := inferencePoints(t, req, "inference.rate.achieved", "{frame}/s", "tensorrt", now)[0].GetAsDouble(); got != 9.97 {
		t.Fatalf("inference.rate.achieved = %v, want 9.97", got)
	}
	// The process counted these 900 frames on the cpu and then on tensorrt.
	// Its count is one series through the change: a point named tensorrt
	// would begin a second series at a count the first already holds.
	onTensorRT := inferencePoints(t, req, "inference.frames.scored", "{frame}", "", now)[0]
	if onTensorRT.GetAsInt() != 900 {
		t.Fatalf("inference.frames.scored = %d, want 900", onTensorRT.GetAsInt())
	}
	status.Accelerator = cpuOnly
	onCPU := inferencePoints(t, inferenceMetrics(metricsCampaign, status, now.Add(-time.Hour), now), "inference.frames.scored", "{frame}", "", now)[0]
	if !proto.Equal(onCPU, onTensorRT) {
		t.Fatalf("the count changes series with the engine:\n%v\n%v", onCPU, onTensorRT)
	}
}

// The status compares nothing while no camera is being scored or nothing was
// scored lately, and then the metrics compare nothing either.
func TestInferenceMetricsClaimNoMoreThanTheStatus(t *testing.T) {
	now := time.Now()
	// The worker reported, and no camera is being scored: no throughput in
	// the status, so no requested rate and no verdict.
	status := data.InferenceStatus{State: "running", Accelerator: cpuOnly, Stats: fastStats}
	req := inferenceMetrics(metricsCampaign, status, now, now)
	if names, want := metricNames(req), []string{"inference.rate.achieved", "inference.frame.duration_ms", "inference.frames.scored"}; !slices.Equal(names, want) {
		t.Fatalf("metrics %v, want %v", names, want)
	}
	// Nothing scored yet: no frame was timed, so there is no duration, and
	// certainly not one of 0 ms.
	status.Stats = &data.InferenceStats{}
	req = inferenceMetrics(metricsCampaign, status, now, now)
	if names, want := metricNames(req), []string{"inference.rate.achieved", "inference.frames.scored"}; !slices.Equal(names, want) {
		t.Fatalf("metrics %v, want %v", names, want)
	}
	if got := inferencePoints(t, req, "inference.rate.achieved", "{frame}/s", "cpu", now)[0].GetAsDouble(); got != 0 {
		t.Fatalf("inference.rate.achieved = %v, want 0", got)
	}
	// No report, nothing to publish.
	status.Stats = nil
	if req := inferenceMetrics(metricsCampaign, status, now, now); req != nil {
		t.Fatalf("published without a report: %v", metricNames(req))
	}
}

func TestInferenceMetricsAttributesAreBoundedAndReachCloud(t *testing.T) {
	now := time.Now()
	status := data.InferenceStatus{State: "running", Stats: slowStats,
		Throughput: &data.InferenceThroughput{RequestedFPS: 1, AchievedFPS: 0.11, ModelTooSlow: true, Summary: slowSummary}}
	// A model name is validated for its characters, not for its length.
	campaign := data.Campaign{Name: "people-all-cameras", Inference: &data.CampaignInference{Model: "owner/" + strings.Repeat("m", 5000), Rate: 1}}
	req := inferenceMetrics(campaign, status, now, now)
	for _, name := range metricNames(req) {
		metric := findMetric(req, name)
		points := metric.GetGauge().GetDataPoints()
		if sum := metric.GetSum(); sum != nil {
			points = sum.GetDataPoints()
		}
		for _, point := range points {
			if model, _ := attrString(point.GetAttributes(), "inference.model"); len(model) != maxLabelValLen || !strings.HasPrefix(model, "owner/mmm") {
				t.Fatalf("%s model attribute has %d bytes, want %d", name, len(model), maxLabelValLen)
			}
			// What scores is unknown until the worker says.
			if engine, present := attrString(point.GetAttributes(), "inference.engine"); present {
				t.Fatalf("%s names the engine %q that nothing reported", name, engine)
			}
		}
	}

	// The Cloud flusher's guards leave these metrics as they are, and the
	// name prefix that telemetry_metrics filters on selects all of them.
	req = inferenceMetrics(metricsCampaign, data.InferenceStatus{Accelerator: cpuOnly, Stats: slowStats, Throughput: status.Throughput}, now, now)
	sanitized := proto.Clone(req).(*colmetricspb.ExportMetricsServiceRequest)
	sanitizeMetrics(sanitized)
	if !proto.Equal(sanitized, req) {
		t.Fatalf("the Cloud export differs:\n%v\nwas\n%v", sanitized, req)
	}
	prefix, service := "inference.", "wendy-agent"
	filter := &agentpb.StreamMetricsRequest{MetricNamePrefix: &prefix, ServiceName: &service}
	filtered := filterMetrics(req, filter)
	if !slices.Equal(metricNames(filtered), metricNames(req)) || len(metricNames(req)) != 5 {
		t.Fatalf("prefix %q selects %v of %v", prefix, metricNames(filtered), metricNames(req))
	}

	// The publisher the agent uses keeps the export whole: its disk buffer,
	// which the Cloud flusher and a tail of telemetry_metrics read, hands
	// back what was published.
	buffer, _ := makeTestBuffer(t, 10<<20, 1<<20)
	buffer.PublishMetrics(req)
	stored := buffer.ReadLastNMatching(SignalMetrics, 5, func(m proto.Message) proto.Message {
		if kept := filterMetrics(m.(*colmetricspb.ExportMetricsServiceRequest), filter); kept != nil {
			return kept
		}
		return nil
	})
	if len(stored) != 1 || !proto.Equal(stored[0], req) {
		t.Fatalf("the disk buffer holds %v", stored)
	}
}

// newInferenceMetricsTestService is newInferenceTestService with a publisher,
// set before inference starts as the agent sets it.
func newInferenceMetricsTestService(t *testing.T) (*DataService, *inferenceTestFactory, *recordingPublisher) {
	t.Helper()
	manager, err := data.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	service := NewDataService(manager)
	service.addAdapter(&inferenceTestAdapter{sources: []data.Source{{ID: "v4l2:/dev/video0", Kind: "camera", Healthy: true}}})
	service.video = inferenceTestVideo{}
	publisher := &recordingPublisher{}
	service.SetTelemetryPublisher(publisher)
	if _, err := manager.DeployCampaign(inferenceTestYAML(t)); err != nil {
		t.Fatal(err)
	}
	factory := &inferenceTestFactory{sessions: make(chan *inferenceTestSession, 8)}
	stop := service.StartCampaignInference(context.Background(), factory, &inferenceTestSender{requests: make(chan DetectionNotification, 8)})
	t.Cleanup(func() {
		stop()
		for _, key := range manager.ActiveEpisodeKeys() {
			_, _ = service.stopCapture(context.Background(), key)
		}
	})
	return service, factory, publisher
}

func TestAgentInferencePublishesWhatInspectShows(t *testing.T) {
	previousHold, previousRetry := inferenceThroughputHold, inferenceRetryDelay
	inferenceThroughputHold, inferenceRetryDelay = 20*time.Millisecond, 5*time.Millisecond
	t.Cleanup(func() { inferenceThroughputHold, inferenceRetryDelay = previousHold, previousRetry })
	service, factory, publisher := newInferenceMetricsTestService(t)
	session := receiveInference(t, factory.sessions)
	input := receiveInference(t, session.inputs)
	seen := 0
	// report sends one throughput report and returns its export with the
	// status inspect shows once the agent has taken it in.
	report := func(session *inferenceTestSession, stats *data.InferenceStats) (*colmetricspb.ExportMetricsServiceRequest, *data.InferenceStatus) {
		t.Helper()
		session.results <- inference.Result{Type: "accelerator", Accelerator: cpuOnly, Stats: stats}
		export := publisher.next(t, seen)
		seen++
		return export, inspectInference(t, service)
	}

	// Before a camera is scored the status compares nothing, and so do the
	// metrics.
	export, status := report(session, slowStats)
	if status.Throughput != nil {
		t.Fatalf("throughput before a camera is scored: %+v", status.Throughput)
	}
	if names, want := metricNames(export), []string{"inference.rate.achieved", "inference.frame.duration_ms", "inference.frames.scored"}; !slices.Equal(names, want) {
		t.Fatalf("metrics %v, want %v", names, want)
	}

	session.results <- inference.Result{Type: "prediction", SourceID: input.SourceID, Generation: input.Generation, InferMs: 9104}
	// Every export says what inspect says, before and after the model is
	// found too slow.
	flagged := false
	for range 200 {
		export, status = report(session, slowStats)
		if status.Throughput == nil {
			t.Fatal("no throughput with a camera being scored")
		}
		requested := inferencePoints(t, export, "inference.rate.requested", "{frame}/s", "cpu", time.Time{})[0].GetAsDouble()
		achieved := inferencePoints(t, export, "inference.rate.achieved", "{frame}/s", "cpu", time.Time{})[0].GetAsDouble()
		tooSlow := inferencePoints(t, export, "inference.model.too_slow", "1", "cpu", time.Time{})[0].GetAsInt() == 1
		if requested != status.Throughput.RequestedFPS || achieved != status.Throughput.AchievedFPS || achieved != status.Stats.FPS || tooSlow != status.Throughput.ModelTooSlow {
			t.Fatalf("metrics say %v of %v requested, too slow %v; inspect says %+v", achieved, requested, tooSlow, status.Throughput)
		}
		if flagged = tooSlow; flagged {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !flagged {
		t.Fatal("the model was never found too slow")
	}
	first := inferencePoints(t, export, "inference.frames.scored", "{frame}", "", time.Time{})[0]
	if first.GetAsInt() != 12 || first.GetStartTimeUnixNano() == 0 || first.GetStartTimeUnixNano() > first.GetTimeUnixNano() {
		t.Fatalf("frames scored %d, counted from %d, at %d", first.GetAsInt(), first.GetStartTimeUnixNano(), first.GetTimeUnixNano())
	}

	// A restarted worker counts from zero again, so its count has a new
	// start: a cumulative sum that fell with the old start would read as a
	// reset the receiver has to guess at.
	close(session.results)
	restarted := receiveInference(t, factory.sessions)
	input = receiveInference(t, restarted.inputs)
	restarted.results <- inference.Result{Type: "prediction", SourceID: input.SourceID, Generation: input.Generation, InferMs: 9104}
	export, _ = report(restarted, &data.InferenceStats{FPS: 0.11, InferMsP50: 9104, InferMsP95: 9377, FramesScored: 2})
	second := inferencePoints(t, export, "inference.frames.scored", "{frame}", "", time.Time{})[0]
	if second.GetAsInt() != 2 || second.GetStartTimeUnixNano() <= first.GetTimeUnixNano() || second.GetStartTimeUnixNano() > second.GetTimeUnixNano() {
		t.Fatalf("after a restart: %d frames counted from %d, at %d; before it: counted from %d, last at %d",
			second.GetAsInt(), second.GetStartTimeUnixNano(), second.GetTimeUnixNano(), first.GetStartTimeUnixNano(), first.GetTimeUnixNano())
	}

	// A campaign that stopped publishes nothing more.
	if _, err := service.CampaignDisarm(context.Background(), &agentpbv2.DataCampaignDisarmRequest{Name: "people-all-cameras"}); err != nil {
		t.Fatal(err)
	}
	receiveInference(t, restarted.closed)
	before := len(publisher.published())
	select {
	case restarted.results <- inference.Result{Type: "accelerator", Accelerator: cpuOnly, Stats: slowStats}:
	default:
	}
	time.Sleep(50 * time.Millisecond)
	if after := len(publisher.published()); after != before || before != seen {
		t.Fatalf("%d exports after the campaign stopped, %d before, %d expected", after, before, seen)
	}
}

// Without a publisher, as in every other test of the job, nothing is built
// and nothing is published.
func TestAgentInferencePublishesNothingWithoutAPublisher(t *testing.T) {
	job := &campaignInferenceJob{owner: &campaignInferenceManager{service: &DataService{}}, campaign: metricsCampaign,
		status: data.InferenceStatus{State: "running", Accelerator: cpuOnly, Stats: slowStats, Sources: map[string]string{}}}
	if req := job.throughputMetrics(time.Now()); req != nil {
		t.Fatalf("built %v with no publisher", metricNames(req))
	}
	publisher := &recordingPublisher{}
	job.owner.service.SetTelemetryPublisher(publisher)
	if req := job.throughputMetrics(time.Now()); req == nil {
		t.Fatal("built nothing with a publisher")
	}
}
