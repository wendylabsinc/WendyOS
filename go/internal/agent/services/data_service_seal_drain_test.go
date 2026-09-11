package services

import (
	"context"
	"errors"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

// TestTriggerDuringThePostSealDrainStartsTheNextEpisode pins the campaign
// blind window closed.
//
// The trigger path skips any campaign named in ActiveEpisodeKeys, and the
// post-seal drain used to keep the sealing episode in that list for its whole
// window. A matching record arriving in that window was therefore dropped with
// a bare continue: not queued, not delayed, lost. Every campaign pays a drain
// (a campaign plan always selects the applications source) and the default is
// two seconds, so a second person walking into frame within two seconds of the
// previous episode produced no recording at all, while the detection itself was
// filed into the episode whose cameras had already stopped. An event with no
// video is precisely the failure this platform exists to prevent.
//
// This test deliberately declares NO capture.drain, so it runs against the two
// second default rather than against the drain: 0s that every other campaign
// test in this package opts into. That opt-out is how the defect survived
// review in the first place: the default's interaction with the trigger path
// was untested by construction. The cost is that this test takes about four
// seconds; the default is the configuration that ships, so it is worth it.
func TestTriggerDuringThePostSealDrainStartsTheNextEpisode(t *testing.T) {
	manager, err := data.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	service := NewDataService(manager)
	yaml := []byte(`version: 1
name: doorway
sources:
  - telemetry: true
capture:
  buffer: 1s
  after_trigger: 30ms
  triggers:
    - event: person_detected
upload: {when: wifi, destination: example-episodes}
export: {annotation: cvat}
`)
	if _, err = service.CampaignDeploy(context.Background(), &agentpbv2.DataCampaignDeployRequest{CampaignYaml: yaml}); err != nil {
		t.Fatal(err)
	}
	campaigns, err := manager.Campaigns()
	if err != nil {
		t.Fatal(err)
	}
	// The premise, checked rather than assumed: this campaign really is on the
	// default drain, so the window under test is the shipped one.
	if len(campaigns) != 1 || campaigns[0].DrainDuration() != data.DefaultSealDrain {
		t.Fatalf("campaign drain = %v, want the %s default", campaigns, data.DefaultSealDrain)
	}

	record := func(name string) {
		t.Helper()
		if _, recordErr := manager.RecordApplication("test.app", data.ApplicationRecord{
			Version: 1, Type: "event", Name: name,
		}); recordErr != nil {
			t.Fatal(recordErr)
		}
	}

	// First person through the door.
	record("person_detected")
	// Second person, 300ms later. after_trigger is 30ms, so by now the first
	// episode has stopped capturing and is somewhere inside its two second
	// drain. The wait is a plain sleep on purpose: gating on anything the
	// manager reports about the sealing episode would make the test's own
	// timing depend on the behaviour it is checking.
	time.Sleep(300 * time.Millisecond)
	record("person_detected")

	// Both events must produce a recording. The second episode cannot begin
	// until the first has finished draining, because the data service holds
	// captureMu across the stop, so allow for two full drains plus slack.
	episodes := waitForFinalizedEpisodesBy(t, manager, 2, 4*data.DefaultSealDrain+5*time.Second)
	if len(episodes) != 2 {
		t.Fatalf("episodes = %d, want 2: a detection inside the post-seal drain produced no recording", len(episodes))
	}
	for _, episode := range episodes {
		manifest, _, inspectErr := manager.Inspect(episode.ID, false)
		if inspectErr != nil {
			t.Fatal(inspectErr)
		}
		if manifest.Trigger.CampaignName != "doorway" || manifest.Trigger.Reason != "event:person_detected" {
			t.Fatalf("episode %s has trigger %+v", episode.ID, manifest.Trigger)
		}
	}
}

// TestDrainingEpisodeReleasesItsCampaignKey is the fast, direct statement of
// the same rule at the manager boundary: an episode inside its drain still
// accepts records, and no longer holds its campaign key, so the campaign can
// start its next episode. Both halves matter — releasing the key by sealing
// early would close the window the drain exists for.
func TestDrainingEpisodeReleasesItsCampaignKey(t *testing.T) {
	manager, err := data.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const key = "doorway"
	first, err := manager.Start(data.StartOptions{
		Sources:       []string{"applications"},
		DrainDuration: 2 * time.Second,
		Trigger:       data.EpisodeTrigger{CampaignName: key, Reason: "event:person_detected"},
	})
	if err != nil {
		t.Fatal(err)
	}
	stopped := make(chan error, 1)
	go func() {
		_, stopErr := manager.Stop(key)
		stopped <- stopErr
	}()

	// Wait for the campaign key to come free. On its own that says nothing:
	// the key also comes free when the episode finally seals. What separates
	// the two is what happens NEXT, so the moment the key is free the episode
	// is asked to prove it is still open. Fixed, the key is released as the
	// drain begins and the record lands in the draining episode; unfixed, the
	// key stays held for the whole drain and by the time it is free the
	// episode is sealed and the record can only be buffered.
	deadline := time.Now().Add(30 * time.Second)
	for len(manager.ActiveEpisodeKeys()) != 0 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if len(manager.ActiveEpisodeKeys()) != 0 {
		t.Fatal("the episode never released its campaign key")
	}
	state, err := manager.RecordApplication("com.example.scorer", data.ApplicationRecord{
		Version: 1, Type: "prediction", Name: "late-verdict", Model: "detector",
	})
	if err != nil {
		t.Fatal(err)
	}
	if state != "recorded" {
		t.Fatalf("with the campaign key free, a record's state is %q, want recorded: the key was only released "+
			"once the episode had sealed, so a trigger matching during the drain had nothing to start and nothing to join",
			state)
	}

	// The key really is free: the campaign's next episode starts while the
	// previous one is still draining.
	next, err := manager.Start(data.StartOptions{
		Sources:       []string{"applications"},
		DrainDuration: 0,
		Trigger:       data.EpisodeTrigger{CampaignName: key, Reason: "event:person_detected"},
	})
	if err != nil {
		t.Fatalf("starting the campaign's next episode during the previous drain: %v", err)
	}
	if next.ID == first.ID {
		t.Fatal("the second episode reused the first episode's identity")
	}
	if err = <-stopped; err != nil {
		t.Fatalf("the first episode failed to seal after its key was released: %v", err)
	}
	if _, err = manager.Stop(key); err != nil {
		t.Fatal(err)
	}
	list, err := manager.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("finalized %d episode(s), want 2", len(list))
	}
	// The late verdict belongs to the episode whose samples it scored, not to
	// the one that started after it.
	firstManifest, _, err := manager.Inspect(first.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, source := range firstManifest.Sources {
		if source.Source.ID == "applications" && source.Count == 0 {
			t.Fatal("the draining episode recorded no application records; the drain stopped doing its job")
		}
	}
}

// failingDataAdapter refuses to start, the way a camera adapter does when the
// device is unplugged mid-flap.
type failingDataAdapter struct{}

func (failingDataAdapter) Discover(context.Context) []data.Source { return nil }

func (failingDataAdapter) Start(context.Context, data.CaptureSession, []data.Source) (runningDataCapture, error) {
	return nil, errors.New("camera busy")
}

// TestFailedAdapterStartDoesNotPayTheSealDrain pins the latency of the failing
// start path.
//
// startCapture holds captureMu, the lock that serialises every start and stop
// on the device, from its first line to its last. Finalizing the episode
// through the draining Interrupt therefore charged the whole drain to every
// other caller: a Start whose adapter errored took the full drain to return
// FailedPrecondition, so a flapping camera with capture.drain: 30s stalled the
// data service for thirty seconds per attempt. The drain buys time for an
// application to file a record about samples it read from the episode, and an
// episode whose adapters never started delivered no samples to anybody, so
// there was never anything to wait for.
func TestFailedAdapterStartDoesNotPayTheSealDrain(t *testing.T) {
	manager, err := data.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	service := NewDataService(manager)
	service.addAdapter(failingDataAdapter{})
	// Long enough that paying it could not be mistaken for scheduling noise,
	// and long enough that the test fails fast rather than merely slowly.
	service.adHocDrain = 10 * time.Second

	begin := time.Now()
	_, err = service.Start(context.Background(), &agentpbv2.DataStartRequest{Sources: []string{"applications"}})
	elapsed := time.Since(begin)
	if code := status.Code(err); code != codes.FailedPrecondition {
		t.Fatalf("Start returned %v (code %s), want FailedPrecondition", err, code)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("a failed adapter start took %s to return, holding captureMu throughout; it must not pay the %s drain",
			elapsed, service.adHocDrain)
	}

	// Skipping the drain must not skip the seal: the episode is finalized and
	// its monotonic data retained, exactly as an interrupted episode always is.
	list, err := manager.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("finalized %d episode(s), want the interrupted one", len(list))
	}
	manifest, _, err := manager.Inspect(list[0].ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.State != "interrupted" || manifest.Interruption != "capture_adapter_start_failed" {
		t.Fatalf("episode state=%q interruption=%q, want interrupted/capture_adapter_start_failed",
			manifest.State, manifest.Interruption)
	}
	// And the lock really is free again for the next caller.
	if len(manager.ActiveEpisodeKeys()) != 0 {
		t.Fatalf("episode keys still held after the failed start: %v", manager.ActiveEpisodeKeys())
	}
}

// waitForFinalizedEpisodesBy is waitForFinalizedEpisodes with the deadline
// spelled out, for tests that run against the default drain rather than
// opting out of it.
func waitForFinalizedEpisodesBy(t *testing.T, manager *data.Manager, want int, within time.Duration) []data.EpisodeInfo {
	t.Helper()
	deadline := time.Now().Add(within)
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
	t.Fatalf("timed out after %s waiting for %d finalized episodes", within, want)
	return nil
}
