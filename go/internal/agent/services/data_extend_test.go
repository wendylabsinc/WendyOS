package services

import (
	"context"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	"github.com/wendylabsinc/wendy/go/internal/agent/inference"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// The timings below keep a wide margin between how often the condition is
// reported active (every 100ms) and after_trigger (500ms), so a slow CI runner
// cannot end a recording that a test keeps alive.
const (
	extendTestAfterTrigger = 500 * time.Millisecond
	extendTestEvery        = 100 * time.Millisecond
)

func newExtendTestService(t *testing.T, name, capture string) (*DataService, *data.Manager) {
	t.Helper()
	manager, err := data.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	service := NewDataService(manager)
	yaml := []byte(`version: 1
name: ` + name + `
sources:
  - telemetry: true
capture:
  buffer: 1s
  drain: 0s
  after_trigger: 500ms
` + capture + `  triggers:
    - event: fire
upload: {when: wifi, destination: example-episodes}
export: {annotation: cvat}
`)
	if _, err := service.CampaignDeploy(context.Background(), &agentpbv2.DataCampaignDeployRequest{CampaignYaml: yaml}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, key := range manager.ActiveEpisodeKeys() {
			_, _ = service.stopCapture(context.Background(), key)
		}
	})
	return service, manager
}

func fireEvent(t *testing.T, manager *data.Manager) {
	t.Helper()
	if _, err := manager.RecordApplication("test.app", data.ApplicationRecord{Version: 1, Type: "event", Name: "fire"}); err != nil {
		t.Fatal(err)
	}
}

// fireFor reports the event every extendTestEvery for d and returns when it
// last reported it.
func fireFor(t *testing.T, manager *data.Manager, d time.Duration) time.Time {
	t.Helper()
	last := time.Now()
	for deadline := last.Add(d); time.Now().Before(deadline); time.Sleep(extendTestEvery) {
		fireEvent(t, manager)
		last = time.Now()
	}
	return last
}

func waitRecording(t *testing.T, manager *data.Manager, name string, want bool) time.Time {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, active := manager.ActiveSession(name); active == want {
			return time.Now()
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("campaign %s recording=%v never happened", name, want)
	return time.Time{}
}

// extendedParts waits for every part of the campaign's one recording to
// seal, and returns them in part order. A handed-off part seals after its
// follow-up opened, so the list is complete only once the parts number 1..n
// and the last one continues nowhere.
func extendedParts(t *testing.T, manager *data.Manager) []data.Manifest {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var parts []data.Manifest
		list, err := manager.List()
		if err != nil {
			t.Fatal(err)
		}
		for _, episode := range list {
			manifest, failures, err := manager.Inspect(episode.ID, true)
			if err != nil || len(failures) > 0 {
				t.Fatalf("inspect %s: %v %v", episode.ID, err, failures)
			}
			parts = append(parts, manifest)
		}
		sort.Slice(parts, func(i, j int) bool { return parts[i].Extension.Part < parts[j].Extension.Part })
		complete := len(parts) > 0 && len(manager.ActiveEpisodeKeys()) == 0 && parts[len(parts)-1].Extension.NextEpisode == ""
		for i, part := range parts {
			complete = complete && part.Extension.Part == i+1
		}
		if complete {
			return parts
		}
		if time.Now().After(deadline) {
			t.Fatalf("the recording's parts never all sealed: %+v", parts)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestExtendedRecordingFollowsRepeatedEvents(t *testing.T) {
	_, manager := newExtendTestService(t, "fire-watch", "  extend: true\n  max_duration: 10s\n")
	fireEvent(t, manager)
	waitRecording(t, manager, "fire-watch", true)
	// Re-fired events keep it going three after_trigger windows past the
	// trigger, where the fixed window would have ended it after one.
	last := fireFor(t, manager, 3*extendTestAfterTrigger)
	if _, active := manager.ActiveSession("fire-watch"); !active {
		t.Fatal("the recording ended while the event kept firing")
	}
	// It ends after_trigger after the event was last active, not earlier.
	ended := waitRecording(t, manager, "fire-watch", false)
	if quiet := ended.Sub(last); quiet < extendTestAfterTrigger || quiet > extendTestAfterTrigger+2*time.Second {
		t.Fatalf("the recording ended %s after the last event, want after_trigger (%s)", quiet, extendTestAfterTrigger)
	}
	parts := extendedParts(t, manager)
	if len(parts) != 1 {
		t.Fatalf("one event recorded as %d episodes", len(parts))
	}
	got := parts[0]
	want := data.EpisodeExtension{Part: 1, MaxDurationNanos: (10 * time.Second).Nanoseconds(), EndReason: data.EndReasonEventCleared}
	if got.Extension != want {
		t.Fatalf("manifest extension %+v, want %+v", got.Extension, want)
	}
	if recorded := time.Duration(got.CaptureStoppedEpisodeNS); recorded < 3*extendTestAfterTrigger {
		t.Fatalf("recorded %s, want the whole event plus after_trigger", recorded)
	}
}

// A detection that is no longer in view but has not cleared (clear_after)
// keeps the recording going; it ends after_trigger after the clear.
func TestExtendedRecordingFollowsAnUnclearedDetection(t *testing.T) {
	service, factory, _, _ := newInferenceTestService(t, false)
	receiveInference(t, factory.sessions)
	plan := strings.NewReplacer(
		"after_trigger: 10s", "after_trigger: 500ms\n  extend: true\n  max_duration: 30s",
		"clear_after: 5s", "clear_after: 1s",
	).Replace(string(inferenceTestYAML(t)))
	if _, err := service.CampaignDeploy(context.Background(), &agentpbv2.DataCampaignDeployRequest{CampaignYaml: []byte(plan)}); err != nil {
		t.Fatal(err)
	}
	session := receiveInference(t, factory.sessions)
	input := receiveInference(t, session.inputs)
	predict := func(person bool) {
		result := inference.Result{Type: "prediction", SourceID: input.SourceID, Generation: input.Generation}
		if person {
			result.Detections = []inference.Detection{{Label: "person", Score: .99, Box: [4]float64{1, 2, 3, 4}}}
		}
		session.results <- result
	}
	const name = "people-all-cameras"
	predict(true)
	waitRecording(t, service.manager, name, true)
	for range 10 {
		time.Sleep(extendTestEvery)
		predict(true)
	}
	if _, active := service.manager.ActiveSession(name); !active {
		t.Fatal("the recording ended while the person was in view")
	}
	// Gone from view. clear_after is 1s, so for that long the detection has
	// not cleared and the recording must go on past after_trigger.
	gone := time.Now()
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			case <-time.After(extendTestEvery):
				predict(false)
			}
		}
	}()
	defer func() { close(stop); <-done }()
	time.Sleep(2 * extendTestAfterTrigger)
	if _, active := service.manager.ActiveSession(name); !active {
		t.Fatal("the recording ended before the detection cleared")
	}
	ended := waitRecording(t, service.manager, name, false)
	if after := ended.Sub(gone); after < time.Second+extendTestAfterTrigger-extendTestEvery {
		t.Fatalf("the recording ended %s after the person left, before clear_after plus after_trigger", after)
	}
	parts := extendedParts(t, service.manager)
	if len(parts) != 1 || parts[0].Extension.EndReason != data.EndReasonEventCleared {
		t.Fatalf("parts: %+v", parts)
	}
}

func TestExtendedRecordingCapStartsLinkedFollowUp(t *testing.T) {
	_, manager := newExtendTestService(t, "fire-watch", "  extend: true\n  max_duration: 1s\n")
	fireEvent(t, manager)
	waitRecording(t, manager, "fire-watch", true)
	fireFor(t, manager, 2200*time.Millisecond)
	waitRecording(t, manager, "fire-watch", false)
	parts := extendedParts(t, manager)
	if len(parts) < 3 {
		t.Fatalf("a 2.7s event under a 1s cap recorded %d parts", len(parts))
	}
	for i, part := range parts {
		e := part.Extension
		if e.Part != i+1 || e.MaxDurationNanos != time.Second.Nanoseconds() {
			t.Fatalf("part %d: %+v", i+1, e)
		}
		if part.Trigger.Reason != "event:fire" || part.Trigger.CampaignName != "fire-watch" {
			t.Fatalf("part %d trigger: %+v", i+1, part.Trigger)
		}
		last := i == len(parts)-1
		if last {
			if e.EndReason != data.EndReasonEventCleared || e.NextEpisode != "" {
				t.Fatalf("last part: %+v", e)
			}
		} else if e.EndReason != data.EndReasonMaxDuration || e.NextEpisode != parts[i+1].ID {
			t.Fatalf("part %d does not continue in part %d: %+v", i+1, i+2, e)
		}
		if i == 0 {
			if e.PreviousEpisode != "" {
				t.Fatalf("part 1 continues %s", e.PreviousEpisode)
			}
			continue
		}
		if e.PreviousEpisode != parts[i-1].ID {
			t.Fatalf("part %d continues %s, want %s", i+1, e.PreviousEpisode, parts[i-1].ID)
		}
		// No time is lost: the follow-up reaches back extendHandoverOverlap,
		// which covers the handover from the moment the previous part stopped.
		previous := parts[i-1]
		stopped := previous.StartedUnixNanos + previous.CaptureStoppedEpisodeNS
		if gap := time.Duration(part.StartedUnixNanos - stopped); gap >= extendHandoverOverlap {
			t.Fatalf("part %d opened %s after part %d stopped capturing, more than its %s overlap", i+1, gap, i, extendHandoverOverlap)
		}
		for _, source := range part.Sources {
			if source.Source.ID == "applications" && source.RequestedOffset != -extendHandoverOverlap.Nanoseconds() {
				t.Fatalf("part %d requested %s of pre-roll, want the %s overlap instead of the buffer", i+1, time.Duration(-source.RequestedOffset), extendHandoverOverlap)
			}
		}
	}
}

func TestExtendedRecordingWithoutFollowUpWhenTheEventCleared(t *testing.T) {
	_, manager := newExtendTestService(t, "fire-watch", "  extend: true\n  max_duration: 1500ms\n")
	fireEvent(t, manager)
	waitRecording(t, manager, "fire-watch", true)
	fireFor(t, manager, 3*extendTestEvery)
	waitRecording(t, manager, "fire-watch", false)
	// Past the cap, nothing has continued the recording.
	time.Sleep(1500 * time.Millisecond)
	parts := extendedParts(t, manager)
	if len(parts) != 1 {
		t.Fatalf("a cleared event recorded %d episodes", len(parts))
	}
	if e := parts[0].Extension; e.EndReason != data.EndReasonEventCleared || e.NextEpisode != "" {
		t.Fatalf("a cleared event: %+v", e)
	}
}

func TestExtendedRecordingManualTriggerAndStop(t *testing.T) {
	service, manager := newExtendTestService(t, "fire-watch", "  extend: true\n  max_duration: 10s\n")
	trigger := func() *agentpbv2.DataEpisode {
		t.Helper()
		episode, err := service.CampaignTrigger(context.Background(), &agentpbv2.DataCampaignTriggerRequest{Name: "fire-watch"})
		if err != nil {
			t.Fatal(err)
		}
		return episode
	}
	first := trigger()
	// A manual trigger during the recording extends it instead of failing.
	if again := trigger(); again.GetId() != first.GetId() {
		t.Fatalf("a trigger during the recording opened %s beside %s", again.GetId(), first.GetId())
	}
	fireFor(t, manager, 3*extendTestEvery)
	stopped, err := service.Stop(context.Background(), &agentpbv2.DataStopRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if stopped.GetId() != first.GetId() {
		t.Fatalf("stopped %s, want %s", stopped.GetId(), first.GetId())
	}
	if keys := manager.ActiveEpisodeKeys(); len(keys) != 0 {
		t.Fatalf("still recording after the stop: %v", keys)
	}
	// The stop ends the recording; it is not extended or continued after it.
	time.Sleep(2 * extendTestAfterTrigger)
	parts := extendedParts(t, manager)
	if len(parts) != 1 {
		t.Fatalf("a stopped recording became %d episodes", len(parts))
	}
	if e := parts[0].Extension; e.EndReason != data.EndReasonManualStop || e.NextEpisode != "" || e.Part != 1 {
		t.Fatalf("a manually stopped recording: %+v", e)
	}
	if service.extendedFor("fire-watch") != nil {
		t.Fatal("the stopped recording is still registered")
	}
}

// Without capture.extend nothing changes: the episode ends after_trigger
// after its trigger however often the event fires, a manual trigger during it
// fails, and its manifest has no extension block.
func TestFixedWindowIsUnchangedWithoutExtend(t *testing.T) {
	service, manager := newExtendTestService(t, "fixed", "")
	first, err := service.CampaignTrigger(context.Background(), &agentpbv2.DataCampaignTriggerRequest{Name: "fixed"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.CampaignTrigger(context.Background(), &agentpbv2.DataCampaignTriggerRequest{Name: "fixed"}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("a manual trigger during a fixed-window recording: %v, want FailedPrecondition", err)
	}
	fireFor(t, manager, 4*extendTestAfterTrigger)
	waitForFinalizedEpisodes(t, manager, 1)
	manifest, _, err := manager.Inspect(first.GetId(), false)
	if err != nil {
		t.Fatal(err)
	}
	if recorded := time.Duration(manifest.CaptureStoppedEpisodeNS); recorded >= 3*extendTestAfterTrigger {
		t.Fatalf("a fixed-window episode recorded %s while the event re-fired; it must end after_trigger after the trigger", recorded)
	}
	if manifest.Extension != (data.EpisodeExtension{}) {
		t.Fatalf("a fixed-window episode has an extension block: %+v", manifest.Extension)
	}
	if service.extendedFor("fixed") != nil {
		t.Fatal("a fixed-window campaign registered an extended recording")
	}
}

// handoverTestCamera is a camera adapter with pre-roll arming. It records the
// buffer of the ring each episode opened from.
type handoverTestCamera struct {
	mu     sync.Mutex
	armed  map[string]time.Duration
	opened []time.Duration
}

func (c *handoverTestCamera) Discover(context.Context) []data.Source {
	return []data.Source{{ID: "v4l2:/dev/video0", Kind: "camera", Healthy: true, Detail: "front test camera"}}
}

func (c *handoverTestCamera) Start(_ context.Context, session data.CaptureSession, _ []data.Source) (runningDataCapture, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.opened = append(c.opened, c.armed[session.CampaignKey])
	delete(c.armed, session.CampaignKey)
	return nil, nil
}

func (c *handoverTestCamera) Arm(key string, buffer time.Duration, _ []data.Source) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.armed[key] = buffer
}

func (c *handoverTestCamera) Disarm(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.armed, key)
}

func (c *handoverTestCamera) state(key string) (time.Duration, []time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.armed[key], append([]time.Duration(nil), c.opened...)
}

// A follow-up opens from the short handover ring, not from a fresh camera
// subscription or the campaign's whole buffer again, and the campaign's own
// pre-roll is back once the recording ends; a campaign without a buffer keeps
// no ring at all.
func TestExtendedRecordingHandsTheCameraOverThroughAShortRing(t *testing.T) {
	for _, buffer := range []time.Duration{time.Second, 0} {
		t.Run(buffer.String(), func(t *testing.T) {
			manager, err := data.NewManager(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			service := NewDataService(manager)
			camera := &handoverTestCamera{armed: map[string]time.Duration{}}
			service.addAdapter(camera)
			service.armingAdapter = camera
			yaml := []byte(`version: 1
name: fire-watch
sources:
  - camera: front
capture:
  buffer: ` + buffer.String() + `
  drain: 0s
  after_trigger: 500ms
  extend: true
  max_duration: 1s
  triggers:
    - event: fire
upload: {when: wifi}
export: {annotation: cvat}
`)
			if _, err := service.CampaignDeploy(context.Background(), &agentpbv2.DataCampaignDeployRequest{CampaignYaml: yaml}); err != nil {
				t.Fatal(err)
			}
			for deadline := time.Now().Add(5 * time.Second); buffer > 0; time.Sleep(5 * time.Millisecond) {
				if armed, _ := camera.state("fire-watch"); armed == buffer || time.Now().After(deadline) {
					break
				}
			}
			fireEvent(t, manager)
			waitRecording(t, manager, "fire-watch", true)
			fireFor(t, manager, 1200*time.Millisecond)
			waitRecording(t, manager, "fire-watch", false)
			parts := extendedParts(t, manager)
			if len(parts) != 2 {
				t.Fatalf("recorded %d parts, want 2", len(parts))
			}
			want := []time.Duration{buffer, extendHandoverOverlap}
			var armed time.Duration
			var opened []time.Duration
			for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
				if armed, opened = camera.state("fire-watch"); armed == buffer {
					break
				}
			}
			if len(opened) != 2 || opened[0] != want[0] || opened[1] != want[1] {
				t.Fatalf("parts opened from rings of %v, want %v", opened, want)
			}
			if armed != buffer {
				t.Fatalf("after the recording the campaign has a %s ring armed, want its %s buffer", armed, buffer)
			}
		})
	}
}
