package services

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	"github.com/wendylabsinc/wendy/go/internal/agent/worldview"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

// worldViewE2EYAML is the can campaign the end-to-end test deploys. Its one
// camera is the fake camera the replay video serves the fixture on.
const worldViewE2EYAML = `version: 1
name: e2e-can
sources:
  - camera: ` + worldViewTestCamera + `
objects:
  coke_can:
    event: coke_can_seen
    rate: 4
    clear_after: 2s
    cooldown: 10s
    fusion: { threshold: 0.75, required: [shape] }
    attributes:
      shape:  { expect: { primitive: box }, weight: 1 }
      colour: { expect: { palette: [{ lab: [53, 80, 67], share: 0.6 }], tolerance: 25 }, weight: 1 }
      size:   { expect: { w_m: [0.10, 0.20], h_m: [0.05, 0.10] }, weight: 1 }
capture:
  buffer: 2s
  after_trigger: 5s
  triggers:
    - object.coke_can.confidence: "> 0.8"
upload: { when: manual }
export: { annotation: cvat }
`

const (
	// worldViewE2EFrameInterval paces the replay: 150 access units over 12 s.
	worldViewE2EFrameInterval = 80 * time.Millisecond
	worldViewE2EWait          = 90 * time.Second
	// worldViewE2EHold is how long frames must keep arriving after the first
	// appeared for the test to show the track holds without a second appeared.
	worldViewE2EHold = 3 * time.Second
)

// worldViewE2EClock stamps replayed samples. Off Linux the data package's boot
// clock counts from its own package initialization, which this approximates;
// on Linux it is CLOCK_BOOTTIME, and the difference only shifts the episode
// offsets of the model input ledger, not the world view's gating.
var worldViewE2EClock = time.Now()

// splitAnnexB splits an H.264 Annex B stream into access units the way a
// camera producer delivers them: parameter sets and supplemental enhancement
// information (SEI) are carried with the next picture, and each coded slice
// (network abstraction layer (NAL) unit types 1 and 5) ends one unit. It
// assumes one slice per picture, which the fixture has.
func splitAnnexB(stream []byte) [][]byte {
	var starts []int
	for i := 0; i+3 <= len(stream); i++ {
		if stream[i] == 0 && stream[i+1] == 0 && stream[i+2] == 1 {
			start := i
			if i > 0 && stream[i-1] == 0 {
				start = i - 1
			}
			starts = append(starts, start)
			i += 2
		}
	}
	var units [][]byte
	pending := 0
	for n, start := range starts {
		end := len(stream)
		if n+1 < len(starts) {
			end = starts[n+1]
		}
		header := start + 3
		if stream[start+2] == 0 {
			header++
		}
		if header >= end {
			continue
		}
		if kind := stream[header] & 0x1f; kind == 1 || kind == 5 {
			units = append(units, stream[pending:end])
			pending = end
		}
	}
	if pending < len(stream) && len(units) > 0 {
		units[len(units)-1] = append(append([]byte(nil), units[len(units)-1]...), stream[pending:]...)
	}
	return units
}

// worldViewReplayVideo serves the fixture's access units, paced like a live
// camera, on the first subscription to the fake camera, then keeps that
// subscription open and idle. Later subscriptions only idle, so a reconnect
// cannot replay the fixture a second time; they are counted.
type worldViewReplayVideo struct {
	units [][]byte
	mu    sync.Mutex
	subs  int
	// last is when the final access unit was handed out; zero until then.
	last time.Time
}

func (v *worldViewReplayVideo) SubscribeSensor(_ context.Context, id string) (sensorSubscription, error) {
	if id != worldViewTestCamera {
		return nil, errors.New("no sensor named " + id)
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	v.subs++
	if v.subs > 1 {
		return &worldViewReplaySubscription{video: v}, nil
	}
	return &worldViewReplaySubscription{video: v, units: v.units, started: time.Now()}, nil
}

func (v *worldViewReplayVideo) state() (subscriptions int, last time.Time) {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.subs, v.last
}

type worldViewReplaySubscription struct {
	video   *worldViewReplayVideo
	units   [][]byte
	next    int
	started time.Time
}

func (s *worldViewReplaySubscription) Next(ctx context.Context) (SensorSample, error) {
	if s.next >= len(s.units) {
		<-ctx.Done()
		return SensorSample{}, ctx.Err()
	}
	timer := time.NewTimer(time.Until(s.started.Add(time.Duration(s.next) * worldViewE2EFrameInterval)))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return SensorSample{}, ctx.Err()
	case <-timer.C:
	}
	unit := s.units[s.next]
	s.next++
	if s.next == len(s.units) {
		s.video.mu.Lock()
		s.video.last = time.Now()
		s.video.mu.Unlock()
	}
	return SensorSample{SourceID: worldViewTestCamera, SampleID: uint64(s.next), BootNanos: time.Since(worldViewE2EClock).Nanoseconds(),
		Payload: unit, Encoding: "h264", SelfContained: true}, nil
}

func (*worldViewReplaySubscription) Close() {}

type worldViewE2ERecord struct {
	at     time.Time
	record data.ApplicationRecord
}

type worldViewE2ERecords struct {
	mu      sync.Mutex
	records []worldViewE2ERecord
}

func (r *worldViewE2ERecords) add(record data.ApplicationRecord) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.records = append(r.records, worldViewE2ERecord{time.Now(), record})
}

func (r *worldViewE2ERecords) snapshot() []worldViewE2ERecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]worldViewE2ERecord(nil), r.records...)
}

func isAppeared(record data.ApplicationRecord) bool {
	return record.Type == "prediction" && record.Model == worldViewModel && record.Attributes["object"] == "coke_can" && record.Attributes["kind"] == worldview.KindAppeared
}

// Opt-in integration: the real data manager, DataService, world view job,
// managed runtime and worker, on an H.264 fixture of a red rectangle served
// through a fake camera. The fakes are the camera's video service, its source
// adapter (discovery, and a recorder that writes nothing) and the notification
// sender, which this campaign never uses.
func TestWorldViewEndToEndFixture(t *testing.T) {
	fixture := os.Getenv("WENDY_WORLDVIEW_E2E_H264")
	cache := os.Getenv("WENDY_WORLDVIEW_E2E_CACHE")
	if fixture == "" || cache == "" {
		t.Skip("set WENDY_WORLDVIEW_E2E_H264 to the red box H.264 Annex B fixture and WENDY_WORLDVIEW_E2E_CACHE to a world view runtime root to run it; an empty root directory downloads the runtime")
	}
	if !worldview.Supported() {
		t.Skip("unsupported runtime platform")
	}
	stream, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatal(err)
	}
	units := splitAnnexB(stream)
	if len(units) == 0 || !bytes.Equal(bytes.Join(units, nil), stream) {
		t.Fatalf("fixture did not split into access units that rebuild it: %d units", len(units))
	}
	t.Logf("fixture: %d bytes, %d access units, replayed at %s per unit", len(stream), len(units), worldViewE2EFrameInterval)

	manager, err := data.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	service := NewDataService(manager)
	service.addAdapter(&inferenceTestAdapter{sources: []data.Source{{ID: worldViewTestCamera, Kind: "camera", Healthy: true}}})
	video := &worldViewReplayVideo{units: units}
	service.video = video
	// Records are asserted through the manager's record hook, which sees every
	// record including those written after the episode ends, and cross-checked
	// against the triggered episode's sealed events.jsonl.
	records := &worldViewE2ERecords{}
	stop := service.startCampaignWorldView(context.Background(), &campaignWorldViewManager{
		factory: &worldview.ManagedFactory{Root: cache},
		sender:  &inferenceTestSender{requests: make(chan DetectionNotification, 8)},
		record: func(appID string, record data.ApplicationRecord) (string, error) {
			records.add(record)
			return manager.RecordCampaignApplication(appID, record)
		}})
	t.Cleanup(func() {
		stop()
		for _, key := range manager.ActiveEpisodeKeys() {
			_, _ = service.stopCapture(context.Background(), key)
		}
	})
	deployed, err := service.CampaignDeploy(context.Background(), &agentpbv2.DataCampaignDeployRequest{CampaignYaml: []byte(worldViewE2EYAML)})
	if err != nil {
		t.Fatal(err)
	}
	if deployed.GetName() != "e2e-can" {
		t.Fatalf("deployed campaign %q", deployed.GetName())
	}
	began := time.Now()

	// 1. objects_status through CampaignInspect, as campaign inspect reads it.
	var status *data.ObjectsStatus
	for {
		inspection, err := service.CampaignInspect(context.Background(), &agentpbv2.DataCampaignInspectRequest{Name: "e2e-can"})
		if err != nil {
			t.Fatal(err)
		}
		var plan data.Campaign
		if err := json.Unmarshal(inspection.PlanJson, &plan); err != nil {
			t.Fatal(err)
		}
		status = plan.ObjectsStatus
		if status != nil && status.State == "running" {
			break
		}
		if time.Since(began) > worldViewE2EWait {
			t.Fatalf("objects_status never reached running: %+v", status)
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Logf("objects_status running after %s: %+v", time.Since(began).Round(time.Millisecond), *status)

	// The job counts every worker result it processes.
	service.worldView.mu.Lock()
	job := service.worldView.jobs["e2e-can"]
	service.worldView.mu.Unlock()
	if job == nil {
		t.Fatal("no world view job for e2e-can")
	}

	// 2. to 4. Wait for the first appeared and the episode it starts.
	var appearedAt time.Time
	for appearedAt.IsZero() {
		for _, entry := range records.snapshot() {
			if isAppeared(entry.record) {
				appearedAt = entry.at
				break
			}
		}
		if appearedAt.IsZero() {
			if time.Since(began) > worldViewE2EWait {
				_, last := video.state()
				t.Fatalf("no appeared record after %s: %d worker results handled, %d records %+v, fixture finished %v, objects_status %+v",
					worldViewE2EWait, job.handled.Load(), len(records.snapshot()), records.snapshot(), !last.IsZero(), service.objectsStatus(mustCampaign(t, manager)))
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	handledAtAppeared := job.handled.Load()
	t.Logf("first appeared %s after deploy, %d worker results handled by then", appearedAt.Sub(began).Round(time.Millisecond), handledAtAppeared)
	var manifest *data.Manifest
	for manifest == nil {
		if m := manager.Status(); m != nil && m.Trigger.CampaignName == "e2e-can" {
			manifest = m
			break
		}
		if time.Since(appearedAt) > worldViewE2EWait {
			t.Fatal("the appeared record started no episode")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// 5. Keep watching until the fixture has run out, frames have kept arriving
	// for at least the hold after the appeared, and the track has had time to
	// clear.
	lastHandled, lastHandledAt := handledAtAppeared, appearedAt
	for {
		if n := job.handled.Load(); n != lastHandled {
			lastHandled, lastHandledAt = n, time.Now()
		}
		_, last := video.state()
		if !last.IsZero() && time.Since(last) > 3*time.Second {
			break
		}
		if time.Since(appearedAt) > worldViewE2EWait {
			t.Fatal("the fixture replay never finished")
		}
		time.Sleep(20 * time.Millisecond)
	}
	furtherResults := lastHandled - handledAtAppeared
	furtherSpan := lastHandledAt.Sub(appearedAt)
	t.Logf("after the first appeared: %d further worker results over %s", furtherResults, furtherSpan.Round(time.Millisecond))
	if furtherSpan < worldViewE2EHold || furtherResults < 2 {
		t.Fatalf("frames stopped %s after the appeared (%d results); the hold needs %s", furtherSpan, furtherResults, worldViewE2EHold)
	}

	all := records.snapshot()
	var appeared []data.ApplicationRecord
	var events []data.ApplicationRecord
	var kinds []string
	for _, entry := range all {
		record := entry.record
		switch {
		case isAppeared(record):
			appeared = append(appeared, record)
		case record.Type == "event" && record.Name == "coke_can_seen":
			events = append(events, record)
		}
		if record.Type == "prediction" {
			kinds = append(kinds, record.Attributes["kind"].(string))
		} else {
			kinds = append(kinds, record.Type+":"+record.Name)
		}
	}
	t.Logf("records in order: %s", strings.Join(kinds, ", "))
	if len(appeared) != 1 {
		t.Fatalf("got %d appeared predictions for coke_can, want exactly 1", len(appeared))
	}
	prediction := appeared[0]
	printed, _ := json.Marshal(prediction.Attributes)
	t.Logf("appeared prediction attributes: %s", printed)
	attributes := prediction.Attributes
	confidence, _ := attributes["confidence"].(float64)
	if confidence < 0.8 {
		t.Errorf("confidence %v, want at least 0.8", confidence)
	}
	scores, _ := attributes["scores"].(map[string]float64)
	if _, ok := scores["shape"]; !ok {
		t.Errorf("scores lack shape: %v", scores)
	}
	if _, ok := scores["colour"]; !ok {
		t.Errorf("scores lack colour: %v", scores)
	}
	if unavailable, _ := attributes["unavailable"].([]string); len(unavailable) != 1 || unavailable[0] != "size" {
		t.Errorf("unavailable %v, want [size] without depth", attributes["unavailable"])
	}
	bbox, _ := attributes["bbox"].([]float64)
	if len(bbox) != 4 || bbox[0] < 0 || bbox[1] < 0 || bbox[2] <= 0 || bbox[3] <= 0 || bbox[0]+bbox[2] > 320 || bbox[1]+bbox[3] > 240 {
		t.Errorf("bbox %v is not a box inside 320x240", attributes["bbox"])
	}
	if frame, _ := attributes["frame"].(map[string]any); frame["w"] != 320 || frame["h"] != 240 {
		t.Errorf("frame %v, want 320x240", attributes["frame"])
	}
	if attributes["requested_rate"] != 4.0 {
		t.Errorf("requested_rate %v, want 4", attributes["requested_rate"])
	}
	if _, ok := attributes["achieved_fps"].(float64); !ok {
		t.Errorf("achieved_fps missing: %v", attributes["achieved_fps"])
	}
	if len(events) != 1 {
		t.Errorf("got %d coke_can_seen events, want exactly 1", len(events))
	}
	if manifest.Name != "e2e-can" || !strings.HasPrefix(manifest.Trigger.Reason, "object_confidence:coke_can:") {
		t.Errorf("episode %q trigger %+v, want campaign e2e-can with an object_confidence:coke_can: reason", manifest.Name, manifest.Trigger)
	}
	t.Logf("episode %s: trigger reason %q, expression %q", manifest.ID, manifest.Trigger.Reason, manifest.Trigger.Expression)

	// The episode records applications, so the appeared prediction reaches its
	// events.jsonl from the pre-roll ring and the event record live.
	var sealed data.Manifest
	for {
		if sealed, _, err = manager.Inspect(manifest.ID, false); err == nil {
			break
		}
		if time.Since(appearedAt) > worldViewE2EWait {
			t.Fatalf("episode %s never sealed: %v", manifest.ID, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	file, _, err := manager.OpenFile(manifest.ID, "events.jsonl", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	var episodeAppeared, episodeEvents, episodeLines int
	decoder := json.NewDecoder(file)
	for decoder.More() {
		var line struct {
			data.ApplicationRecord
			PrerollFlushed bool `json:"preroll_flushed"`
		}
		if err := decoder.Decode(&line); err != nil {
			t.Fatalf("events.jsonl: %v", err)
		}
		episodeLines++
		switch {
		case isAppeared(line.ApplicationRecord):
			episodeAppeared++
			if line.Attributes["confidence"] != confidence || !line.PrerollFlushed {
				t.Errorf("events.jsonl appeared differs from the recorded one: confidence %v, preroll_flushed %v", line.Attributes["confidence"], line.PrerollFlushed)
			}
		case line.Type == "event" && line.Name == "coke_can_seen":
			episodeEvents++
		}
	}
	t.Logf("sealed episode %s state %s: events.jsonl has %d lines, %d appeared, %d coke_can_seen", sealed.ID, sealed.State, episodeLines, episodeAppeared, episodeEvents)
	if episodeAppeared != 1 || episodeEvents != 1 {
		t.Errorf("events.jsonl has %d appeared predictions and %d coke_can_seen events, want 1 and 1", episodeAppeared, episodeEvents)
	}
	if subscriptions, _ := video.state(); subscriptions != 1 {
		t.Errorf("camera subscribed %d times, want once: the job reconnected", subscriptions)
	}
}

func mustCampaign(t *testing.T, manager *data.Manager) data.Campaign {
	t.Helper()
	campaign, err := manager.Campaign("e2e-can")
	if err != nil {
		t.Fatal(err)
	}
	return campaign
}
