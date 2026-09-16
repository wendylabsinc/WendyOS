package services

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

// slowStartAdapter models a capture adapter whose Start takes real time, which
// the camera adapter's does: it waits for the producer's first encoded frame,
// with a twenty second ceiling.
type slowStartAdapter struct {
	delay   time.Duration
	sources []data.Source
}

func (a *slowStartAdapter) Discover(context.Context) []data.Source { return a.sources }

func (a *slowStartAdapter) Start(context.Context, data.CaptureSession, []data.Source) (runningDataCapture, error) {
	time.Sleep(a.delay)
	return noopCapture{}, nil
}

type noopCapture struct{}

func (noopCapture) Stop(context.Context) ([]data.CaptureResult, error) { return nil, nil }

// fakeArmingAdapter records the pre-roll arm and disarm calls a campaign makes.
type fakeArmingAdapter struct {
	mu   sync.Mutex
	arms []armCall
}

type armCall struct {
	key     string
	buffer  time.Duration
	sources []data.Source
}

func (a *fakeArmingAdapter) Discover(context.Context) []data.Source { return nil }

func (a *fakeArmingAdapter) Start(context.Context, data.CaptureSession, []data.Source) (runningDataCapture, error) {
	return noopCapture{}, nil
}

func (a *fakeArmingAdapter) Arm(key string, buffer time.Duration, selected []data.Source) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.arms = append(a.arms, armCall{key: key, buffer: buffer, sources: selected})
}

func (a *fakeArmingAdapter) Disarm(string) {}

func (a *fakeArmingAdapter) armCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.arms)
}

func (a *fakeArmingAdapter) lastArm() (armCall, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.arms) == 0 {
		return armCall{}, false
	}
	return a.arms[len(a.arms)-1], true
}

// installArming registers an arming adapter the way SetVideoService would.
func installArming(service *DataService, adapter *fakeArmingAdapter) {
	service.addAdapter(adapter)
	service.adapterMu.Lock()
	service.armingAdapter = adapter
	service.adapterMu.Unlock()
}

func mustDeploy(t *testing.T, service *DataService, yaml string) *agentpbv2.DataCampaign {
	t.Helper()
	campaign, err := service.CampaignDeploy(context.Background(), &agentpbv2.DataCampaignDeployRequest{CampaignYaml: []byte(yaml)})
	if err != nil {
		t.Fatalf("deploy: %v", err)
	}
	return campaign
}

// TestSecondCampaignOriginIsStampedAtItsOwnTrigger pins the episode origin to
// the instant its campaign fired.
//
// Start and stop used to be serialised on one device-wide mutex, and the origin
// is read inside manager.Start, under that mutex. A campaign whose adapters take
// real time to come up (a camera waits for its first frame, up to twenty
// seconds) therefore moved every other campaign's origin forward to whenever it
// finished: the second campaign's own triggering record then fell OUTSIDE the
// pre-roll window measured back from that late origin, so the episode opened
// after the event that caused it.
func TestSecondCampaignOriginIsStampedAtItsOwnTrigger(t *testing.T) {
	manager, err := data.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	service := NewDataService(manager)
	service.addAdapter(&slowStartAdapter{delay: 600 * time.Millisecond})

	const preRoll = 200 * time.Millisecond
	mustDeploy(t, service, `version: 1
name: slow-campaign
sources:
  - telemetry: true
capture:
  buffer: 0s
  drain: 0s
  after_trigger: 5s
  triggers:
    - event: first_event
upload: {when: wifi, destination: example-episodes}
export: {annotation: cvat}
`)
	mustDeploy(t, service, `version: 1
name: prompt-campaign
sources:
  - telemetry: true
capture:
  buffer: 200ms
  drain: 0s
  after_trigger: 5s
  triggers:
    - event: second_event
upload: {when: wifi, destination: example-episodes}
export: {annotation: cvat}
`)

	// The first campaign fires and sits in its 600ms adapter startup.
	go func() {
		_, _ = service.CampaignTrigger(context.Background(), &agentpbv2.DataCampaignTriggerRequest{Name: "slow-campaign", Reason: "first_event"})
	}()
	time.Sleep(100 * time.Millisecond)

	// The second campaign's triggering record arrives 100ms in, while the first
	// campaign's adapters are still coming up.
	_, recordBoot, _, err := data.CaptureReceipt()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = manager.RecordApplication("test.app", data.ApplicationRecord{Version: 1, Type: "event", Name: "second_event"}); err != nil {
		t.Fatal(err)
	}

	var session data.CaptureSession
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if s, ok := manager.ActiveSession("prompt-campaign"); ok {
			session = s
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if session.ID == "" {
		t.Fatal("the second campaign never opened an episode")
	}
	if lag := time.Duration(session.RequestBootNanos - recordBoot); lag > 250*time.Millisecond {
		t.Fatalf("episode origin is %s after its triggering record; it must be stamped when the campaign fires, not when another campaign's adapters finish starting", lag)
	}
	// The point of stamping it there: the record that fired the campaign has to
	// be inside the window the episode reaches back over.
	if window := session.RequestBootNanos - preRoll.Nanoseconds(); window > recordBoot {
		t.Fatalf("the triggering record is %s before the start of the episode's own pre-roll window", time.Duration(window-recordBoot))
	}

	_, _ = service.stopCapture(context.Background(), "prompt-campaign")
	_, _ = service.stopCapture(context.Background(), "slow-campaign")
}

// TestCameraPreRollArmsDespiteAnUnpublishedROS2Topic pins arming to the camera
// selectors alone. Resolving the whole plan meant one selector that did not
// resolve, a ROS 2 topic whose publisher has not started being the ordinary
// case, returned nothing and left the camera ring disarmed for the entire
// campaign, with nothing said anywhere.
func TestCameraPreRollArmsDespiteAnUnpublishedROS2Topic(t *testing.T) {
	manager, err := data.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	service := NewDataService(manager)
	service.addAdapter(&slowStartAdapter{sources: []data.Source{
		{ID: "v4l2:/dev/video0", Kind: "camera", ClockDomain: "FAKE_NATIVE", Healthy: true, Detail: "front test camera"},
	}})
	arming := &fakeArmingAdapter{}
	installArming(service, arming)

	mustDeploy(t, service, `version: 1
name: mixed-sources
sources:
  - camera: front
  - ros2: /scan
capture:
  buffer: 2s
  drain: 0s
  after_trigger: 30ms
  triggers:
    - event: person_detected
upload: {when: wifi, destination: example-episodes}
export: {annotation: cvat}
`)

	deadline := time.Now().Add(2 * time.Second)
	for arming.armCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	call, ok := arming.lastArm()
	if !ok {
		t.Fatal("the camera was never armed; one unpublished ROS 2 topic disabled pre-roll for the whole campaign")
	}
	if call.key != "mixed-sources" || call.buffer != 2*time.Second {
		t.Fatalf("arm call = %+v", call)
	}
	if len(call.sources) != 1 || call.sources[0].ID != "v4l2:/dev/video0" {
		t.Fatalf("armed sources = %+v, want the one camera", call.sources)
	}
}

// TestDeployWarnsAboutAnUnpublishedROS2Topic: deploy is the last moment an
// operator is present to read the reason, so a topic nothing publishes is worth
// saying then. It is a warning and not a refusal, because a plan is routinely
// deployed before the node that publishes its topic is running.
func TestDeployWarnsAboutAnUnpublishedROS2Topic(t *testing.T) {
	manager, err := data.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	service := NewDataService(manager)
	campaign := mustDeploy(t, service, `version: 1
name: lidar-plan
sources:
  - telemetry: true
  - ros2: /scan
capture:
  buffer: 0s
  drain: 0s
  after_trigger: 30ms
  triggers:
    - event: person_detected
upload: {when: wifi, destination: example-episodes}
export: {annotation: cvat}
`)
	if campaign.GetState() != "armed" {
		t.Fatalf("campaign state = %q; an unpublished topic must not refuse the deploy", campaign.GetState())
	}
	joined := strings.Join(campaign.GetWarnings(), " | ")
	if !strings.Contains(joined, "/scan") || !strings.Contains(joined, "publishes") {
		t.Fatalf("warnings %q do not name the unpublished ROS 2 selector", joined)
	}
}

// TestTriggerDegradesWhenAROS2TopicIsAbsent: an absent topic costs the episode
// that source, not the camera, the telemetry and the application records of the
// event that fired it. The manifest carries the absent source so a reader can
// see which one was lost rather than inferring it from the plan.
func TestTriggerDegradesWhenAROS2TopicIsAbsent(t *testing.T) {
	manager, err := data.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	service := NewDataService(manager)
	mustDeploy(t, service, `version: 1
name: lidar-plan
sources:
  - telemetry: true
  - ros2: /scan
capture:
  buffer: 0s
  drain: 0s
  after_trigger: 30ms
  triggers:
    - event: person_detected
upload: {when: wifi, destination: example-episodes}
export: {annotation: cvat}
`)
	episode, err := service.CampaignTrigger(context.Background(), &agentpbv2.DataCampaignTriggerRequest{Name: "lidar-plan", Reason: "manual"})
	if err != nil {
		t.Fatalf("trigger: %v; an absent ROS 2 topic must not abort the whole episode", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for manager.Status() != nil && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	manifest, _, err := manager.Inspect(episode.GetId(), false)
	if err != nil {
		t.Fatal(err)
	}
	var absent *data.SourceStats
	for i := range manifest.Sources {
		if strings.Contains(manifest.Sources[i].Source.ID, "/scan") {
			absent = &manifest.Sources[i]
		}
	}
	if absent == nil {
		t.Fatalf("the manifest does not mention the absent ROS 2 source: %+v", manifest.Sources)
	}
	if absent.Source.Healthy {
		t.Fatal("the absent source is recorded as healthy")
	}
	if absent.DropAccounting != "source_absent_at_trigger" {
		t.Fatalf("drop accounting = %q, want source_absent_at_trigger", absent.DropAccounting)
	}
	if !strings.Contains(absent.Source.Detail, "triggered") {
		t.Fatalf("absent source detail %q does not say why it is absent", absent.Source.Detail)
	}
	// The rest of the episode is intact.
	if manifest.State != "complete" {
		t.Fatalf("episode state = %q, want complete", manifest.State)
	}
}

// TestCampaignIsRearmedBeforeThePostSealDrain: the ring must be back up as soon
// as the adapters release the camera. Re-arming after the seal left the ring
// empty for the whole drain, so a trigger landing in that window opened an
// episode with no camera pre-roll at all. Armed at capture stop, the drain
// doubles as the next episode's pre-roll window.
func TestCampaignIsRearmedBeforeThePostSealDrain(t *testing.T) {
	manager, err := data.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	service := NewDataService(manager)
	service.addAdapter(&slowStartAdapter{sources: []data.Source{
		{ID: "v4l2:/dev/video0", Kind: "camera", ClockDomain: "FAKE_NATIVE", Healthy: true, Detail: "front test camera"},
	}})
	arming := &fakeArmingAdapter{}
	installArming(service, arming)

	mustDeploy(t, service, `version: 1
name: doorway
sources:
  - camera: front
capture:
  buffer: 1s
  drain: 2s
  after_trigger: 50ms
  triggers:
    - event: person_detected
upload: {when: wifi, destination: example-episodes}
export: {annotation: cvat}
`)
	deadline := time.Now().Add(2 * time.Second)
	for arming.armCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if arming.armCount() == 0 {
		t.Fatal("the campaign was never armed at deploy")
	}
	armsAtTrigger := arming.armCount()

	if _, err = service.CampaignTrigger(context.Background(), &agentpbv2.DataCampaignTriggerRequest{Name: "doorway", Reason: "manual"}); err != nil {
		t.Fatal(err)
	}
	// after_trigger is 50ms and the drain is 2s. Well inside the drain the ring
	// must already be armed again.
	deadline = time.Now().Add(time.Second)
	for arming.armCount() == armsAtTrigger && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if arming.armCount() == armsAtTrigger {
		t.Fatal("the campaign was not re-armed within a second of its capture stopping; a trigger inside the post-seal drain would get no camera pre-roll")
	}
	// The proof that this happened during the drain, not after it: the episode
	// is not sealed yet.
	if list, listErr := manager.List(); listErr != nil {
		t.Fatal(listErr)
	} else if len(list) != 0 {
		t.Fatalf("the episode had already finalized (%d sealed) before the re-arm, so the drain window was still blind", len(list))
	}
}

// failingAdapter refuses to start, standing in for a camera that faults after
// an earlier adapter is already recording.
type failingAdapter struct{}

func (failingAdapter) Discover(context.Context) []data.Source { return nil }
func (failingAdapter) Start(context.Context, data.CaptureSession, []data.Source) (runningDataCapture, error) {
	return nil, errors.New("simulated adapter failure")
}

// TestAFailedAdapterStartDrainsOnceSomethingHasCaptured pins the drain to the
// only claim that justifies skipping it.
//
// Skipping the drain when NOTHING started is right: no application can have read
// a sample from an episode whose adapters never ran, and the wait would be
// charged to every later start and stop of the campaign. Once an earlier adapter
// has started, that claim is simply untrue, and a record about this episode can
// still be outstanding.
func TestAFailedAdapterStartDrainsOnceSomethingHasCaptured(t *testing.T) {
	const drain = 400 * time.Millisecond
	plan := `version: 1
name: partial-start
sources:
  - telemetry: true
capture:
  buffer: 0s
  drain: 400ms
  after_trigger: 5s
  triggers:
    - event: person_detected
upload: {when: wifi, destination: example-episodes}
export: {annotation: cvat}
`

	// Nothing started: the failure returns without serving the drain.
	manager, err := data.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	service := NewDataService(manager)
	service.addAdapter(failingAdapter{})
	mustDeploy(t, service, plan)
	start := time.Now()
	if _, err = service.CampaignTrigger(context.Background(), &agentpbv2.DataCampaignTriggerRequest{Name: "partial-start", Reason: "manual"}); err == nil {
		t.Fatal("a failing adapter must fail the trigger")
	}
	if elapsed := time.Since(start); elapsed >= drain {
		t.Fatalf("took %s: an episode whose adapters never started must not serve the drain", elapsed)
	}

	// One adapter captured before the next failed: the drain is owed.
	manager2, err := data.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	service2 := NewDataService(manager2)
	service2.addAdapter(&slowStartAdapter{})
	service2.addAdapter(failingAdapter{})
	mustDeploy(t, service2, plan)
	start = time.Now()
	if _, err = service2.CampaignTrigger(context.Background(), &agentpbv2.DataCampaignTriggerRequest{Name: "partial-start", Reason: "manual"}); err == nil {
		t.Fatal("a failing adapter must fail the trigger")
	}
	if elapsed := time.Since(start); elapsed < drain {
		t.Fatalf("took %s: once an adapter has captured, a record about the episode can be outstanding and the drain must be served", elapsed)
	}
}
