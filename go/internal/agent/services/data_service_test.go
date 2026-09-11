package services

import (
	"context"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

type fakeDataAdapter struct {
	started bool
	stopped bool
}

func (f *fakeDataAdapter) Discover(context.Context) []data.Source {
	return []data.Source{{ID: "fake:camera", Kind: "camera", ClockDomain: "FAKE_NATIVE", Healthy: true, Detail: "front test camera"}}
}

func TestCampaignDeployTriggerAndTimedFinalization(t *testing.T) {
	manager, err := data.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	service := NewDataService(manager)
	adapter := &fakeDataAdapter{}
	service.addAdapter(adapter)
	yaml := []byte(`version: 1
name: test-flight
fleet: test-lab
sources:
  - camera: front
capture:
  buffer: 10ms
  drain: 0s
  after_trigger: 30ms
  triggers:
    - event: emergency_stop
upload:
  when: wifi
  destination: example-episodes
export:
  annotation: cvat
`)
	campaign, err := service.CampaignDeploy(context.Background(), &agentpbv2.DataCampaignDeployRequest{CampaignYaml: yaml})
	if err != nil {
		t.Fatal(err)
	}
	if campaign.GetState() != "armed" || campaign.GetRevision() == "" {
		t.Fatalf("bad campaign response: %+v", campaign)
	}
	episode, err := service.CampaignTrigger(context.Background(), &agentpbv2.DataCampaignTriggerRequest{Name: "test-flight", Reason: "hardware_test"})
	if err != nil {
		t.Fatal(err)
	}
	if !adapter.started {
		t.Fatal("campaign did not start camera adapter")
	}
	deadline := time.Now().Add(2 * time.Second)
	for manager.Status() != nil && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if manager.Status() != nil || !adapter.stopped {
		t.Fatal("campaign did not stop after after_trigger")
	}
	manifest, failures, err := manager.Inspect(episode.GetId(), true)
	if err != nil {
		t.Fatal(err)
	}
	if len(failures) != 0 {
		t.Fatalf("verification failures: %v", failures)
	}
	if manifest.Trigger.CampaignName != "test-flight" || manifest.Trigger.Reason != "hardware_test" || manifest.Upload.Destination != "example-episodes" || manifest.Labeling.Destination != "cvat" {
		t.Fatalf("campaign metadata missing from episode: %+v", manifest)
	}
	if manifest.CollectorVersion == "" || manifest.Device.ID == "" {
		t.Fatalf("episode identity/version missing: %+v", manifest)
	}
	for _, source := range manifest.Sources {
		if source.Source.ID == "fake:camera" && source.RequestedOffset != -(10*time.Millisecond).Nanoseconds() {
			t.Fatalf("camera requested offset = %d", source.RequestedOffset)
		}
	}
}

func TestCampaignApplicationEventAutomaticallyTriggers(t *testing.T) {
	manager, err := data.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	service := NewDataService(manager)
	yaml := []byte(`version: 1
name: event-flight
sources:
  - telemetry: true
capture:
  buffer: 1s
  drain: 0s
  after_trigger: 30ms
  triggers:
    - event: emergency_stop
upload: {when: wifi, destination: example-episodes}
export: {annotation: cvat}
`)
	if _, err = service.CampaignDeploy(context.Background(), &agentpbv2.DataCampaignDeployRequest{CampaignYaml: yaml}); err != nil {
		t.Fatal(err)
	}
	if _, err = manager.RecordApplication("test.app", data.ApplicationRecord{Version: 1, Type: "event", Name: "emergency_stop"}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		list, listErr := manager.List()
		if listErr != nil {
			t.Fatal(listErr)
		}
		if len(list) == 1 {
			manifest, _, inspectErr := manager.Inspect(list[0].ID, false)
			if inspectErr != nil {
				t.Fatal(inspectErr)
			}
			if manifest.Trigger.Reason != "event:emergency_stop" {
				t.Fatalf("wrong trigger: %+v", manifest.Trigger)
			}
			for _, source := range manifest.Sources {
				if source.Source.ID == "applications" && (source.Count != 1 || source.ActualOffset >= 0 || source.RequestedOffset != -time.Second.Nanoseconds()) {
					t.Fatalf("application pre-roll accounting is not exact: %+v", source)
				}
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("application event did not produce a finalized episode")
}

// deployEventCampaign arms a campaign that an application event triggers. Like
// deployTestCampaign it opts out of the post-seal drain: these tests measure
// the campaign lifecycle, and the default drain would add two seconds to every
// episode without exercising anything they assert.
func deployEventCampaign(t *testing.T, service *DataService, name, event string) {
	t.Helper()
	yaml := []byte(`version: 1
name: ` + name + `
sources:
  - telemetry: true
capture:
  buffer: 1s
  drain: 0s
  after_trigger: 150ms
  triggers:
    - event: ` + event + `
upload: {when: wifi, destination: example-episodes}
export: {annotation: cvat}
`)
	if _, err := service.CampaignDeploy(context.Background(), &agentpbv2.DataCampaignDeployRequest{CampaignYaml: yaml}); err != nil {
		t.Fatal(err)
	}
}

func waitForFinalizedEpisodes(t *testing.T, manager *data.Manager, want int) []data.EpisodeInfo {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		list, err := manager.List()
		if err != nil {
			t.Fatal(err)
		}
		if len(list) >= want && len(manager.ActiveEpisodeKeys()) == 0 {
			return list
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d finalized episodes", want)
	return nil
}

func TestTwoCampaignsCaptureConcurrently(t *testing.T) {
	manager, err := data.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	service := NewDataService(manager)
	deployEventCampaign(t, service, "campaign-a", "event_a")
	deployEventCampaign(t, service, "campaign-b", "event_b")
	if _, err = manager.RecordApplication("test.app", data.ApplicationRecord{Version: 1, Type: "event", Name: "event_a"}); err != nil {
		t.Fatal(err)
	}
	if _, err = manager.RecordApplication("test.app", data.ApplicationRecord{Version: 1, Type: "event", Name: "event_b"}); err != nil {
		t.Fatal(err)
	}
	// Both campaigns must be recording at the same time before either
	// finalizes; after_trigger leaves a wide window for this.
	deadline := time.Now().Add(2 * time.Second)
	concurrent := false
	for !concurrent && time.Now().Before(deadline) {
		keys := manager.ActiveEpisodeKeys()
		concurrent = len(keys) == 2
		time.Sleep(2 * time.Millisecond)
	}
	if !concurrent {
		t.Fatal("campaigns did not capture concurrently")
	}
	episodes := waitForFinalizedEpisodes(t, manager, 2)
	campaigns := map[string]bool{}
	for _, episode := range episodes {
		manifest, _, err := manager.Inspect(episode.ID, false)
		if err != nil {
			t.Fatal(err)
		}
		campaigns[manifest.Trigger.CampaignName] = true
	}
	if len(episodes) != 2 || !campaigns["campaign-a"] || !campaigns["campaign-b"] {
		t.Fatalf("episodes=%d campaigns=%v", len(episodes), campaigns)
	}
}

func TestSameCampaignTriggerDroppedWhileActive(t *testing.T) {
	manager, err := data.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	service := NewDataService(manager)
	deployEventCampaign(t, service, "single-flight", "event_x")
	if _, err = manager.RecordApplication("test.app", data.ApplicationRecord{Version: 1, Type: "event", Name: "event_x"}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	active := false
	for !active && time.Now().Before(deadline) {
		_, active = manager.ActiveSession("single-flight")
		time.Sleep(2 * time.Millisecond)
	}
	if !active {
		t.Fatal("first trigger did not start an episode")
	}
	// A second trigger for the SAME campaign while its episode is active must
	// be dropped rather than queueing or interrupting the recording.
	if _, err = manager.RecordApplication("test.app", data.ApplicationRecord{Version: 1, Type: "event", Name: "event_x"}); err != nil {
		t.Fatal(err)
	}
	episodes := waitForFinalizedEpisodes(t, manager, 1)
	if len(episodes) != 1 {
		t.Fatalf("episodes = %d, want exactly 1", len(episodes))
	}
}

func (f *fakeDataAdapter) Start(_ context.Context, _ data.CaptureSession, selected []data.Source) (runningDataCapture, error) {
	for _, source := range selected {
		if source.ID == "fake:camera" {
			f.started = true
			return f, nil
		}
	}
	return nil, nil
}
func (f *fakeDataAdapter) Stop(context.Context) ([]data.CaptureResult, error) {
	f.stopped = true
	drops, mapping := uint64(3), int64(42)
	return []data.CaptureResult{{SourceID: "fake:camera", Count: 9, Drops: &drops, DropAccounting: "exact", MappingError: &mapping}}, nil
}

func TestDataServiceRunsAdaptersAndSealsResults(t *testing.T) {
	manager, err := data.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	service := NewDataService(manager)
	adapter := &fakeDataAdapter{}
	service.addAdapter(adapter)
	sources, err := service.Sources(context.Background(), &agentpbv2.DataSourcesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(sources.GetSources()) != 3 {
		t.Fatalf("sources = %d, want 3", len(sources.GetSources()))
	}
	started, err := service.Start(context.Background(), &agentpbv2.DataStartRequest{Sources: []string{"fake:camera"}})
	if err != nil {
		t.Fatal(err)
	}
	if !adapter.started {
		t.Fatal("adapter was not started")
	}
	stopped, err := service.Stop(context.Background(), &agentpbv2.DataStopRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if !adapter.stopped || stopped.GetId() != started.GetId() {
		t.Fatalf("bad stop: %+v", stopped)
	}
	manifest, failures, err := manager.Inspect(stopped.GetId(), true)
	if err != nil {
		t.Fatal(err)
	}
	if len(failures) != 0 {
		t.Fatalf("verification failures: %v", failures)
	}
	if len(manifest.Sources) != 1 || manifest.Sources[0].Count != 9 || manifest.Sources[0].Drops == nil || *manifest.Sources[0].Drops != 3 {
		t.Fatalf("adapter results not sealed: %+v", manifest.Sources)
	}
}

// TestAdHocEpisodesCarryTheDefaultSealDrain pins the policy for the Start RPC.
// An ad-hoc episode has no campaign plan to declare a drain of its own, so the
// service supplies the default; without it a `wendy data record` session would
// seal the moment capture stopped and file an application's late verdict into
// whatever recorded next.
func TestAdHocEpisodesCarryTheDefaultSealDrain(t *testing.T) {
	manager, err := data.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	service := NewDataService(manager)

	// The default, measured rather than restated. Comparing service.adHocDrain
	// against data.DefaultSealDrain only reads the constructor back to itself:
	// it cannot tell whether the value reaches the episode, which is the whole
	// claim. Stopping an untouched ad-hoc episode can.
	if _, err = service.Start(context.Background(), &agentpbv2.DataStartRequest{Sources: []string{"applications"}}); err != nil {
		t.Fatal(err)
	}
	begin := time.Now()
	if _, err = service.Stop(context.Background(), &agentpbv2.DataStopRequest{}); err != nil {
		t.Fatal(err)
	}
	// Slack below the default absorbs timer granularity without admitting a
	// drain that was skipped or halved.
	if elapsed := time.Since(begin); elapsed < data.DefaultSealDrain-200*time.Millisecond {
		t.Fatalf("an ad-hoc Stop returned after %s; a `wendy data record` episode must serve the %s default drain",
			elapsed, data.DefaultSealDrain)
	}

	// And the duration is a value the service supplies, not a constant baked
	// into the episode: a different one produces a different wait.
	service.adHocDrain = 200 * time.Millisecond
	if _, err = service.Start(context.Background(), &agentpbv2.DataStartRequest{Sources: []string{"applications"}}); err != nil {
		t.Fatal(err)
	}
	begin = time.Now()
	if _, err = service.Stop(context.Background(), &agentpbv2.DataStopRequest{}); err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(begin)
	if elapsed < 150*time.Millisecond {
		t.Fatalf("ad-hoc Stop returned after %s; the configured drain did not reach the episode", elapsed)
	}
	if elapsed >= data.DefaultSealDrain {
		t.Fatalf("ad-hoc Stop took %s with a 200ms drain configured; the episode ignored it and used the default", elapsed)
	}
}
