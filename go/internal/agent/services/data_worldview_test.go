package services

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	"github.com/wendylabsinc/wendy/go/internal/agent/worldview"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// worldViewTestYAML is the documented can campaign (data.md) with pre-roll and
// drain removed so episodes stop promptly in tests.
const worldViewTestYAML = `version: 1
name: coke-can-watch
sources:
  - camera: front
objects:
  coke_can:
    event: coke_can_seen
    rate: 2
    clear_after: 5s
    cooldown: 30s
    fusion:
      threshold: 0.75
      required: [shape, colour]
    attributes:
      shape:
        expect: {primitive: cylinder}
        weight: 1
      size:
        expect: {w_m: [0.06, 0.07], h_m: [0.11, 0.13]}
        weight: 0.5
      colour:
        expect:
          palette:
            - {lab: [45, 65, 45], share: 0.6}
          tolerance: 25
        weight: 1.5
depth:
  source: front-depth
  scale_m: 0.001
  intrinsics: {fx: 615.2, fy: 615.9, cx: 320.5, cy: 240.25}
capture:
  buffer: 0s
  drain: 0s
  after_trigger: 10s
  triggers:
    - object.coke_can.confidence: "> 0.8"
upload:
  when: wifi
export:
  annotation: cvat
`

const worldViewTestCamera = "v4l2:/dev/video0"

// worldViewTestVideo serves one encoded frame per camera subscription and
// refuses any identifier that is not a camera. It serves no depth frames, so a
// depth source that resolves to a camera is reported as not z16.
type worldViewTestVideo struct{}

func (worldViewTestVideo) SubscribeSensor(_ context.Context, id string) (sensorSubscription, error) {
	if _, ok := cameraDeviceID(id); !ok {
		return nil, errors.New("no sensor named " + id)
	}
	return &inferenceTestSubscription{}, nil
}

type worldViewTestSession struct {
	config  worldview.Config
	mu      sync.Mutex
	results chan worldview.Result
	inputs  chan worldview.Input
	closed  chan struct{}
	stopped bool
}

func (s *worldViewTestSession) Send(input worldview.Input) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return errors.New("closed")
	}
	if !input.End {
		select {
		case s.inputs <- input:
		default:
		}
	}
	return nil
}
func (s *worldViewTestSession) Results() <-chan worldview.Result { return s.results }
func (s *worldViewTestSession) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.stopped {
		s.stopped = true
		close(s.closed)
	}
	return nil
}

type worldViewTestFactory struct{ sessions chan *worldViewTestSession }

func (f *worldViewTestFactory) Start(_ context.Context, config worldview.Config) (worldview.Session, error) {
	s := &worldViewTestSession{config: config, results: make(chan worldview.Result, 64), inputs: make(chan worldview.Input, 16), closed: make(chan struct{})}
	f.sessions <- s
	return s, nil
}

type worldViewHarness struct {
	service *DataService
	factory *worldViewTestFactory
	sender  *inferenceTestSender
	records chan data.ApplicationRecord
	name    string
}

func newWorldViewHarness(t *testing.T, yaml string, factory worldview.Factory) *worldViewHarness {
	t.Helper()
	return newWorldViewHarnessWith(t, yaml, factory, []data.Source{{ID: worldViewTestCamera, Kind: "camera", Healthy: true}}, worldViewTestVideo{})
}

// newWorldViewHarnessWith is newWorldViewHarness with the device's sources and
// video service chosen by the test.
func newWorldViewHarnessWith(t *testing.T, yaml string, factory worldview.Factory, sources []data.Source, video inferenceVideo) *worldViewHarness {
	t.Helper()
	manager, err := data.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	service := NewDataService(manager)
	service.addAdapter(&inferenceTestAdapter{sources: sources})
	service.video = video
	campaign, err := manager.DeployCampaign([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	h := &worldViewHarness{service: service, sender: &inferenceTestSender{requests: make(chan DetectionNotification, 8)}, records: make(chan data.ApplicationRecord, 256), name: campaign.Name}
	if factory == nil {
		h.factory = &worldViewTestFactory{sessions: make(chan *worldViewTestSession, 8)}
		factory = h.factory
	}
	stop := service.startCampaignWorldView(context.Background(), &campaignWorldViewManager{factory: factory, sender: h.sender,
		record: func(appID string, record data.ApplicationRecord) (string, error) {
			h.records <- record
			return manager.RecordCampaignApplication(appID, record)
		}})
	t.Cleanup(func() {
		stop()
		for _, key := range manager.ActiveEpisodeKeys() {
			_, _ = service.stopCapture(context.Background(), key)
		}
	})
	return h
}

// start waits for the worker session and the camera's first input.
func (h *worldViewHarness) start(t *testing.T) (*worldViewTestSession, worldview.Input) {
	t.Helper()
	session := receiveInference(t, h.factory.sessions)
	return session, receiveInference(t, session.inputs)
}

func (h *worldViewHarness) job(t *testing.T) *campaignWorldViewJob {
	t.Helper()
	h.service.worldView.mu.Lock()
	defer h.service.worldView.mu.Unlock()
	job := h.service.worldView.jobs[h.name]
	if job == nil {
		t.Fatal("no world view job")
	}
	return job
}

// waitHandled waits until the job has processed n worker results.
func (h *worldViewHarness) waitHandled(t *testing.T, n uint64) {
	t.Helper()
	job := h.job(t)
	deadline := time.Now().Add(8 * time.Second)
	for job.handled.Load() < n {
		if time.Now().After(deadline) {
			t.Fatalf("job handled %d of %d results", job.handled.Load(), n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (h *worldViewHarness) expectNoRecord(t *testing.T, wait time.Duration) {
	t.Helper()
	select {
	case record := <-h.records:
		t.Fatalf("unexpected record: %+v", record)
	case <-time.After(wait):
	}
}

func (h *worldViewHarness) inspect(t *testing.T) data.Campaign {
	t.Helper()
	inspection, err := h.service.CampaignInspect(context.Background(), &agentpbv2.DataCampaignInspectRequest{Name: h.name})
	if err != nil {
		t.Fatal(err)
	}
	var plan data.Campaign
	if err := json.Unmarshal(inspection.PlanJson, &plan); err != nil {
		t.Fatal(err)
	}
	return plan
}

func (h *worldViewHarness) waitStatus(t *testing.T, done func(*data.ObjectsStatus) bool) *data.ObjectsStatus {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for {
		status := h.inspect(t).ObjectsStatus
		if status != nil && done(status) {
			return status
		}
		if time.Now().After(deadline) {
			t.Fatalf("objects_status never reached the expected state: %+v", status)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

const worldViewBootBase = int64(1_000_000_000_000)

func worldViewResult(input worldview.Input, offset time.Duration, proposals ...worldview.ProposalWire) worldview.Result {
	return worldview.Result{Type: "proposals", SourceID: input.SourceID, Generation: input.Generation,
		SampleID: uint64(offset / (100 * time.Millisecond)), BootNanos: worldViewBootBase + int64(offset),
		FrameW: 640, FrameH: 480, AchievedFPS: 2, Proposals: proposals}
}

// canProposal is a red cylinder silhouette. lab moves its dominant colour.
func canProposal(lab [3]float64) worldview.ProposalWire {
	return worldview.ProposalWire{Box: [4]float64{100, 100, 60, 110}, AreaPx: 6600,
		Palette:    []worldview.PaletteWire{{Lab: lab, Share: 0.7}},
		Silhouette: worldview.SilhouetteWire{Primitive: "rect", Aspect: 1.8}}
}

var canRed = [3]float64{45, 65, 45}

// canOffRed is 10 delta E from the expected red, so colour scores 0.6.
var canOffRed = [3]float64{45, 65, 55}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-6 }

func TestWorldViewCanAppearsOnceAndTriggersEpisode(t *testing.T) {
	h := newWorldViewHarness(t, worldViewTestYAML, nil)
	session, input := h.start(t)
	if session.config.Rate == nil || *session.config.Rate != 2 || session.config.EveryFrames != nil || session.config.Depth != nil || session.config.Proposer != worldview.ProposerContours {
		t.Fatalf("unexpected worker config: %+v", session.config)
	}
	if input.Kind != worldview.KindRGB || input.SourceID != worldViewTestCamera || input.SampleID != 1 || input.Encoding != "h264" {
		t.Fatalf("unexpected worker input: %+v", input)
	}
	// The depth subscription fails; the job still runs, without depth.
	status := h.waitStatus(t, func(s *data.ObjectsStatus) bool { return s.State == "running" })
	if !strings.HasPrefix(status.Sources["front-depth"], "depth unavailable: ") {
		t.Fatalf("depth failure not reported: %+v", status)
	}

	session.results <- worldViewResult(input, 0, canProposal(canRed))
	prediction := receiveInference(t, h.records)
	if prediction.Type != "prediction" || prediction.Model != "worldview" || prediction.Attributes["kind"] != "appeared" || prediction.Attributes["object"] != "coke_can" {
		t.Fatalf("unexpected prediction: %+v", prediction)
	}
	if !near(prediction.Attributes["confidence"].(float64), 1) || len(prediction.Inputs) != 1 || prediction.Inputs[0].SourceID != worldViewTestCamera {
		t.Fatalf("unexpected prediction values: %+v", prediction)
	}
	if prediction.Attributes["model_version"] != h.job(t).campaign.Revision || prediction.Attributes["requested_rate"] != 2.0 || prediction.Attributes["depth_paired"] != false {
		t.Fatalf("unexpected provenance: %+v", prediction.Attributes)
	}
	if _, ok := prediction.Attributes["position"]; ok {
		t.Fatal("position reported without depth")
	}
	event := receiveInference(t, h.records)
	if event.Type != "event" || event.Name != "coke_can_seen" || event.Attributes["track_id"] != prediction.Attributes["track_id"] {
		t.Fatalf("unexpected event: %+v", event)
	}
	deadline := time.Now().Add(8 * time.Second)
	for h.service.manager.Status() == nil && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	manifest := h.service.manager.Status()
	if manifest == nil || manifest.Trigger.Reason != "object_confidence:coke_can:1" || manifest.Trigger.Expression != "object.coke_can.confidence > 0.8" {
		t.Fatalf("object confidence trigger did not start an episode: %+v", manifest)
	}

	// The same can seen again produces no further records.
	for i := 1; i <= 20; i++ {
		session.results <- worldViewResult(input, time.Duration(i)*500*time.Millisecond, canProposal(canRed))
	}
	h.waitHandled(t, 21)
	h.expectNoRecord(t, 50*time.Millisecond)
}

func TestWorldViewBelowThresholdProducesNothing(t *testing.T) {
	h := newWorldViewHarness(t, worldViewTestYAML, nil)
	session, input := h.start(t)
	// Red covering 0.33 of the region against an expected 0.6 scores colour
	// 0.55, above its veto floor, and fuses to (1 + 1.5 * 0.55) / 2.5 = 0.73,
	// under the 0.75 threshold.
	sparse := canProposal(canRed)
	sparse.Palette[0].Share = 0.33
	session.results <- worldViewResult(input, 0, sparse)
	// White is far from the expected red: the required colour vetoes it.
	session.results <- worldViewResult(input, 500*time.Millisecond, canProposal([3]float64{95, 0, 0}))
	h.waitHandled(t, 2)
	h.expectNoRecord(t, 50*time.Millisecond)
	if len(h.service.manager.ActiveEpisodeKeys()) != 0 {
		t.Fatal("a rejected proposal started an episode")
	}
}

func TestWorldViewSizeUnavailableWithoutMetric(t *testing.T) {
	h := newWorldViewHarness(t, worldViewTestYAML, nil)
	session, input := h.start(t)
	session.results <- worldViewResult(input, 0, canProposal(canOffRed))
	prediction := receiveInference(t, h.records)
	unavailable, _ := prediction.Attributes["unavailable"].([]string)
	scores, _ := prediction.Attributes["scores"].(map[string]float64)
	if len(unavailable) != 1 || unavailable[0] != "size" {
		t.Fatalf("size not unavailable: %+v", prediction.Attributes)
	}
	if _, scored := scores["size"]; scored || !near(scores["shape"], 1) || !near(scores["colour"], 0.6) {
		t.Fatalf("unexpected scores: %+v", scores)
	}
	// (1 * 1 + 1.5 * 0.6) / (1 + 1.5): size neither raises nor lowers it.
	if confidence := prediction.Attributes["confidence"].(float64); !near(confidence, 0.76) {
		t.Fatalf("confidence %v, want 0.76", confidence)
	}
	receiveInference(t, h.records) // the appeared event
	time.Sleep(100 * time.Millisecond)
	if len(h.service.manager.ActiveEpisodeKeys()) != 0 {
		t.Fatal("confidence 0.76 fired a > 0.8 trigger")
	}
}

func TestWorldViewAppPredictionRaisesConfidenceThroughClass(t *testing.T) {
	yaml := strings.Replace(worldViewTestYAML, "        weight: 1.5\n", "        weight: 1.5\n      class:\n        expect: {source: app, label: can}\n        weight: 1\n", 1)
	h := newWorldViewHarness(t, yaml, nil)
	session, input := h.start(t)
	session.results <- worldViewResult(input, 0, canProposal(canOffRed))
	appeared := receiveInference(t, h.records)
	receiveInference(t, h.records)
	if appeared.Attributes["kind"] != "appeared" || !near(appeared.Attributes["confidence"].(float64), 0.76) {
		t.Fatalf("unexpected first sighting: %+v", appeared.Attributes)
	}
	unavailable := appeared.Attributes["unavailable"].([]string)
	if len(unavailable) != 2 || unavailable[0] != "class" || unavailable[1] != "size" {
		t.Fatalf("class should be unavailable without app evidence: %v", unavailable)
	}
	// An application detects a can over the same region 400 ms before the
	// next frame, inside the 500 ms association window.
	h.service.observeApplicationRecord("detector-app", data.ApplicationRecord{Version: 1, Type: "prediction", Model: "detector",
		ClientBootNanos: worldViewBootBase + int64(100*time.Millisecond),
		Inputs:          []data.SampleRef{{SourceID: worldViewTestCamera, SampleID: 1}},
		Attributes: map[string]any{"detections": []any{
			map[string]any{"class_name": "can", "confidence": 0.95, "box": []any{102.0, 101.0, 60.0, 110.0}},
		}}})
	session.results <- worldViewResult(input, 500*time.Millisecond, canProposal(canOffRed))
	peak := receiveInference(t, h.records)
	scores := peak.Attributes["scores"].(map[string]float64)
	// (1 + 1.5 * 0.6 + 1 * 0.95) / 3.5
	if peak.Attributes["kind"] != "peak" || !near(scores["class"], 0.95) || !near(peak.Attributes["confidence"].(float64), 2.85/3.5) {
		t.Fatalf("class evidence did not raise confidence: %+v", peak.Attributes)
	}
}

func TestWorldViewLostOnceAfterClearAfter(t *testing.T) {
	h := newWorldViewHarness(t, strings.Replace(worldViewTestYAML, "clear_after: 5s", "clear_after: 1s", 1), nil)
	session, input := h.start(t)
	session.results <- worldViewResult(input, 0, canProposal(canRed))
	appeared := receiveInference(t, h.records)
	receiveInference(t, h.records)
	lost := receiveInference(t, h.records)
	if lost.Attributes["kind"] != "lost" || lost.Attributes["track_id"] != appeared.Attributes["track_id"] {
		t.Fatalf("expected lost for the track: %+v", lost.Attributes)
	}
	if !near(lost.Attributes["confidence"].(float64), 1) || lost.Attributes["bbox"].([]float64)[2] != 60 {
		t.Fatalf("lost lost the last match: %+v", lost.Attributes)
	}
	h.expectNoRecord(t, 1500*time.Millisecond)
}

func TestWorldViewRateGatingSkipsResults(t *testing.T) {
	h := newWorldViewHarness(t, worldViewTestYAML, nil)
	session, input := h.start(t)
	// Scored: the first result for the camera.
	session.results <- worldViewResult(input, 0)
	// Skipped: 100 ms is well inside the 500 ms interval of rate 2.
	session.results <- worldViewResult(input, 100*time.Millisecond, canProposal(canRed))
	h.waitHandled(t, 2)
	h.expectNoRecord(t, 50*time.Millisecond)
	session.results <- worldViewResult(input, 500*time.Millisecond, canProposal(canRed))
	prediction := receiveInference(t, h.records)
	if prediction.Attributes["kind"] != "appeared" || prediction.Attributes["boot_nanos"] != worldViewBootBase+int64(500*time.Millisecond) {
		t.Fatalf("expected the 500 ms frame to be scored: %+v", prediction.Attributes)
	}
	if _, ok := prediction.Attributes["object_achieved_fps"].(float64); !ok {
		t.Fatalf("object_achieved_fps missing: %+v", prediction.Attributes)
	}
}

func TestWorldViewNotifyOncePerAppeared(t *testing.T) {
	yaml := strings.Replace(worldViewTestYAML, "export:\n", "notify:\n  on: event\n  event: coke_can_seen\n  webhook: https://notifications.example/cans\nexport:\n", 1)
	h := newWorldViewHarness(t, yaml, nil)
	session, input := h.start(t)
	for i := 0; i < 5; i++ {
		session.results <- worldViewResult(input, time.Duration(i)*500*time.Millisecond, canProposal(canRed))
	}
	request := receiveInference(t, h.sender.requests)
	if request.Event != "coke_can_seen" || request.Campaign != h.name || request.SourceID != worldViewTestCamera || request.Model != "worldview" {
		t.Fatalf("unexpected notification: %+v", request)
	}
	h.waitHandled(t, 5)
	select {
	case extra := <-h.sender.requests:
		t.Fatalf("a standing can notified again: %+v", extra)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestWorldViewUnsupportedHostReportsError(t *testing.T) {
	previous := worldViewSupported
	worldViewSupported = func() bool { return false }
	t.Cleanup(func() { worldViewSupported = previous })
	h := newWorldViewHarness(t, worldViewTestYAML, &worldview.ManagedFactory{Root: t.TempDir()})
	status := h.waitStatus(t, func(s *data.ObjectsStatus) bool { return s.State == "error" })
	if !strings.Contains(status.Error, "unsupported on this platform") {
		t.Fatalf("unclear unsupported error: %+v", status)
	}
}

func TestWorldViewDeploymentRequiresRuntime(t *testing.T) {
	manager, err := data.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	service := NewDataService(manager)
	service.video = worldViewTestVideo{}
	_, err = service.CampaignDeploy(context.Background(), &agentpbv2.DataCampaignDeployRequest{CampaignYaml: []byte(worldViewTestYAML)})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("world view campaign deployed without a runtime: %v", err)
	}
	disabled := strings.Replace(worldViewTestYAML, "    rate: 2\n", "    rate: 2\n    enabled: false\n", 1)
	campaign, err := service.CampaignDeploy(context.Background(), &agentpbv2.DataCampaignDeployRequest{CampaignYaml: []byte(disabled)})
	if err != nil {
		t.Fatal(err)
	}
	var plan data.Campaign
	if err := json.Unmarshal(campaign.PlanJson, &plan); err != nil {
		t.Fatal(err)
	}
	if plan.ObjectsStatus == nil || plan.ObjectsStatus.State != "disabled" {
		t.Fatalf("disabled objects status: %+v", plan.ObjectsStatus)
	}
}

func TestWorldViewConfigCadence(t *testing.T) {
	object := func(rate float64, every int) worldViewObject {
		return worldViewObject{desc: &data.ObjectDescriptor{Rate: rate, EveryFrames: every}}
	}
	rates := worldViewConfig([]worldViewObject{object(2, 0), object(5, 0)})
	if rates.Rate == nil || *rates.Rate != 5 || rates.EveryFrames != nil {
		t.Fatalf("rate objects: %+v", rates)
	}
	frames := worldViewConfig([]worldViewObject{object(0, 15), object(0, 5)})
	if frames.EveryFrames == nil || *frames.EveryFrames != 5 || frames.Rate != nil {
		t.Fatalf("every_frames objects: %+v", frames)
	}
	mixed := worldViewConfig([]worldViewObject{object(2, 0), object(0, 5)})
	if mixed.Rate == nil || *mixed.Rate != 6 || mixed.EveryFrames != nil {
		t.Fatalf("mixed objects: %+v", mixed)
	}
	if err := mixed.Validate(); err != nil {
		t.Fatal(err)
	}
	// every_frames 15 with worker results every 5 frames scores every third.
	gate, desc := &objectGate{}, &data.ObjectDescriptor{EveryFrames: 15}
	var admitted []uint64
	for sample := uint64(0); sample <= 45; sample += 5 {
		if gate.admit(desc, 0, sample, 0, 5) {
			admitted = append(admitted, sample)
		}
	}
	if len(admitted) != 4 || admitted[1] != 15 || admitted[3] != 45 {
		t.Fatalf("every_frames gating admitted %v", admitted)
	}
}

func TestWorldViewClassEvidenceParsingAndInferenceFilter(t *testing.T) {
	manager := &campaignWorldViewManager{evidence: map[string][]worldview.ClassEvidence{}}
	record := func(model string, detections ...any) data.ApplicationRecord {
		return data.ApplicationRecord{Version: 1, Type: "prediction", Model: model, ClientBootNanos: worldViewBootBase,
			Inputs: []data.SampleRef{{SourceID: worldViewTestCamera, SampleID: 3}}, Attributes: map[string]any{"detections": detections}}
	}
	manager.observePrediction("app", record("detector",
		map[string]any{"label": "can", "score": 0.9, "box": []any{1.0, 2.0, 3.0, 4.0}},
		map[string]any{"class_name": "cup", "confidence": 0.8, "box": []any{1.0, 2.0, 3.0}},
		map[string]any{"class_name": "cup", "box": []any{1.0, 2.0, 3.0, 4.0}},
	))
	manager.observePrediction("app", record("facebook/detr-resnet-50", map[string]any{"label": "can", "score": 0.99, "box": []any{1.0, 2.0, 3.0, 4.0}}))
	manager.observePrediction("app", record("worldview", map[string]any{"label": "can", "score": 0.99, "box": []any{1.0, 2.0, 3.0, 4.0}}))
	all := manager.evidenceFor(worldViewTestCamera)
	if len(all) != 2 || all[0].Label != "can" || all[0].Confidence != 0.9 || all[0].Box != [4]float64{1, 2, 3, 4} || all[0].BootNanos != worldViewBootBase {
		t.Fatalf("unexpected evidence: %+v", all)
	}
	job := &campaignWorldViewJob{owner: manager, campaign: data.Campaign{Inference: &data.CampaignInference{Model: "facebook/detr-resnet-50"}}}
	if kept := job.classEvidence(worldViewTestCamera); len(kept) != 1 || kept[0].Model != "detector" {
		t.Fatalf("the campaign's inference model counted as class evidence: %+v", kept)
	}
	// Entries older than two seconds than the newest are dropped.
	later := record("detector", map[string]any{"label": "can", "score": 0.5, "box": []any{1.0, 2.0, 3.0, 4.0}})
	later.ClientBootNanos += int64(3 * time.Second)
	manager.observePrediction("app", later)
	if kept := manager.evidenceFor(worldViewTestCamera); len(kept) != 1 || kept[0].Confidence != 0.5 {
		t.Fatalf("stale evidence kept: %+v", kept)
	}
}

func TestWorldViewCompositionWithoutMetricScoresPrimitivesOnly(t *testing.T) {
	desc := &data.ObjectDescriptor{Fusion: data.ObjectFusion{Threshold: 0.5}, Attributes: map[string]*data.ObjectAttribute{
		"shape": {Expect: map[string]any{"primitive": "cylinder"}, Weight: 1},
	}, Composition: []data.ObjectPrimitive{
		{Primitive: "cylinder", WidthM: [2]float64{0.06, 0.09}, HeightM: [2]float64{0.12, 0.2}},
		{Primitive: "cone", WidthM: [2]float64{0.02, 0.09}, HeightM: [2]float64{0.03, 0.06}},
	}}
	attributes, spec := objectScoring(desc)
	if len(attributes) != 1 || spec.Weights["shape"] != 1 {
		t.Fatalf("unexpected scoring: %+v %+v", attributes, spec)
	}
	proposal := worldview.Proposal{Silhouette: worldview.Silhouette{Primitive: "rect"}}
	evidence := attributes[0].score(proposal, nil)
	// The body is 0.16 of 0.205 metres of expected height.
	if !evidence.Available || !near(evidence.Score, 0.16/0.205) {
		t.Fatalf("composition without metric: %+v", evidence)
	}
	proposal.Metric = &worldview.Metric{WidthM: 0.5, HeightM: 0.5}
	if evidence := attributes[0].score(proposal, nil); !evidence.Available || evidence.Score != 0 {
		t.Fatalf("an oversized part matched: %+v", evidence)
	}
}

// worldViewDepthVideo serves one encoded frame per colour camera subscription
// and one z16 frame per subscription to a node in depth, as the video service
// serves a Z16 depth node, and records every identifier subscribed.
type worldViewDepthVideo struct {
	depth      map[string]bool
	mu         sync.Mutex
	subscribed []string
}

func (v *worldViewDepthVideo) SubscribeSensor(_ context.Context, id string) (sensorSubscription, error) {
	if _, ok := cameraDeviceID(id); !ok {
		return nil, errors.New("no sensor named " + id)
	}
	v.mu.Lock()
	v.subscribed = append(v.subscribed, id)
	v.mu.Unlock()
	if v.depth[id] {
		return &worldViewDepthSubscription{}, nil
	}
	return &inferenceTestSubscription{}, nil
}

func (v *worldViewDepthVideo) subscriptions(id string) int {
	v.mu.Lock()
	defer v.mu.Unlock()
	n := 0
	for _, subscribed := range v.subscribed {
		if subscribed == id {
			n++
		}
	}
	return n
}

type worldViewDepthSubscription struct{ sent bool }

func (s *worldViewDepthSubscription) Next(ctx context.Context) (SensorSample, error) {
	if !s.sent {
		s.sent = true
		return SensorSample{SampleID: 1, Encoding: "z16", Width: 4, Height: 2, Payload: make([]byte, 16)}, nil
	}
	<-ctx.Done()
	return SensorSample{}, ctx.Err()
}
func (*worldViewDepthSubscription) Close() {}

const (
	worldViewTestDepthNode  = "v4l2:/dev/video2"
	worldViewTestDepthNode2 = "v4l2:/dev/video4"
)

// worldViewDepthYAML is the test campaign with its camera and depth selectors
// replaced.
func worldViewDepthYAML(t *testing.T, camera, depth string) string {
	t.Helper()
	yaml := strings.Replace(worldViewTestYAML, "  - camera: front\n", "  - camera: "+camera+"\n", 1)
	yaml = strings.Replace(yaml, "  source: front-depth\n", "  source: "+depth+"\n", 1)
	if !strings.Contains(yaml, "camera: "+camera+"\n") || !strings.Contains(yaml, "source: "+depth+"\n") {
		t.Fatal("test campaign no longer has the camera and depth selectors this test replaces")
	}
	return yaml
}

// A depth.source selector resolves as a camera: selector does, here through a
// fragment of the depth node's name, and the node it names is subscribed for
// depth and never streamed as a colour camera, even under camera: "*".
func TestWorldViewDepthSelectorResolvesToDepthNode(t *testing.T) {
	video := &worldViewDepthVideo{depth: map[string]bool{worldViewTestDepthNode: true}}
	sources := []data.Source{
		{ID: worldViewTestCamera, Kind: "camera", Healthy: true, Detail: "HD Pro Webcam C920 USB"},
		{ID: worldViewTestDepthNode, Kind: "camera", Healthy: true, Detail: "Intel RealSense D435 Depth USB"},
	}
	h := newWorldViewHarnessWith(t, worldViewDepthYAML(t, `"*"`, "realsense"), nil, sources, video)
	session := receiveInference(t, h.factory.sessions)
	if session.config.Depth == nil || len(session.config.Pairs) != 1 || session.config.Pairs[worldViewTestCamera] != "realsense" {
		t.Fatalf("depth not paired with the one colour camera: depth %+v, pairs %v", session.config.Depth, session.config.Pairs)
	}
	status := h.waitStatus(t, func(s *data.ObjectsStatus) bool {
		return s.State == "running" && s.Sources["realsense"] == "depth streaming" && s.Sources[worldViewTestCamera] != ""
	})
	if _, streamed := status.Sources[worldViewTestDepthNode]; streamed {
		t.Fatalf("the depth node is also searched as a colour camera: %+v", status)
	}
	var depthInput, colourInput bool
	for !depthInput || !colourInput {
		input := receiveInference(t, session.inputs)
		switch {
		case input.Kind == worldview.KindDepth && input.SourceID == "realsense" && input.Encoding == "z16" && input.Width == 4 && input.Height == 2:
			depthInput = true
		case input.Kind == worldview.KindRGB && input.SourceID == worldViewTestCamera:
			colourInput = true
		default:
			t.Fatalf("unexpected worker input: %+v", input)
		}
	}
	if n := video.subscriptions(worldViewTestDepthNode); n != 1 {
		t.Fatalf("depth node subscribed %d times, want once for depth", n)
	}
}

// A depth.source selector that names no camera, or more than one, is reported
// in objects_status and nothing is subscribed for depth.
func TestWorldViewDepthSelectorUnresolvedIsReported(t *testing.T) {
	sources := []data.Source{
		{ID: worldViewTestCamera, Kind: "camera", Healthy: true, Detail: "HD Pro Webcam C920 USB"},
		{ID: worldViewTestDepthNode, Kind: "camera", Healthy: true, Detail: "Intel RealSense D435 Depth USB"},
		{ID: worldViewTestDepthNode2, Kind: "camera", Healthy: true, Detail: "Intel RealSense D435 Infrared USB"},
	}
	for _, tc := range []struct{ selector, reason string }{
		{"realsense", `depth unavailable: camera selector "realsense" is ambiguous: ` + worldViewTestDepthNode + ", " + worldViewTestDepthNode2},
		{"zed", `depth unavailable: no healthy camera matches "zed"`},
	} {
		t.Run(tc.selector, func(t *testing.T) {
			video := &worldViewDepthVideo{depth: map[string]bool{worldViewTestDepthNode: true, worldViewTestDepthNode2: true}}
			h := newWorldViewHarnessWith(t, worldViewDepthYAML(t, worldViewTestCamera, tc.selector), nil, sources, video)
			session := receiveInference(t, h.factory.sessions)
			if session.config.Depth != nil || len(session.config.Pairs) != 0 {
				t.Fatalf("depth paired from an unresolved selector: depth %+v, pairs %v", session.config.Depth, session.config.Pairs)
			}
			status := h.waitStatus(t, func(s *data.ObjectsStatus) bool { return s.State == "running" && s.Sources[tc.selector] != "" })
			if got := status.Sources[tc.selector]; got != tc.reason {
				t.Fatalf("depth status = %q, want %q", got, tc.reason)
			}
			if video.subscriptions(worldViewTestDepthNode)+video.subscriptions(worldViewTestDepthNode2) != 0 {
				t.Fatal("a depth node was subscribed from an unresolved selector")
			}
		})
	}
}
