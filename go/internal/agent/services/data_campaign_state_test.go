package services

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// holdingArmingAdapter models what the camera adapter's pre-roll costs: from
// Arm until Disarm it holds the camera for that campaign key.
type holdingArmingAdapter struct {
	mu   sync.Mutex
	held map[string]bool
	arms int
}

func (a *holdingArmingAdapter) Discover(context.Context) []data.Source { return nil }

func (a *holdingArmingAdapter) Start(context.Context, data.CaptureSession, []data.Source) (runningDataCapture, error) {
	return noopCapture{}, nil
}

func (a *holdingArmingAdapter) Arm(key string, _ time.Duration, _ []data.Source) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.held == nil {
		a.held = map[string]bool{}
	}
	a.held[key] = true
	a.arms++
}

func (a *holdingArmingAdapter) Disarm(key string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.held, key)
}

func (a *holdingArmingAdapter) holding(key string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.held[key]
}

func (a *holdingArmingAdapter) armCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.arms
}

func waitForCampaignState(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A camera campaign with agent inference and a pre-roll buffer: the three
// things on a device that keep running for an armed campaign.
const liveCampaignYAML = `version: 1
name: c920-yolo-live
sources:
  - camera: "*"
inference:
  model: facebook/detr-resnet-50
  revision: 1d5f47bd3bdd2c4bbfa585418ffe6da5028b4c0b
  labels: [person]
  threshold: 0.9
  rate: 1
  event: person_detected
  clear_after: 5s
  cooldown: 30s
capture:
  buffer: 2s
  drain: 0s
  after_trigger: 50ms
  triggers:
    - event: person_detected
upload:
  when: manual
export:
  annotation: cvat
`

// A telemetry-only campaign, for the episode store paths. after_trigger is
// long so a test decides when the recording ends.
const recordingCampaignYAML = `version: 1
name: dock-door
fleet: warehouse-west
sources:
  - telemetry: true
capture:
  buffer: 0s
  drain: 0s
  after_trigger: 1h
  triggers:
    - event: door_open
upload:
  when: manual
export:
  annotation: cvat
`

// newCampaignStateService builds the agent's data service the way main does:
// capture adapters, then pre-roll reconcile, then the inference supervisor.
// Building a second one over the same root is an agent restart.
func newCampaignStateService(t *testing.T, root string) (*DataService, *inferenceTestFactory, *holdingArmingAdapter) {
	t.Helper()
	manager, err := data.NewManager(filepath.Join(root, "episodes"))
	if err != nil {
		t.Fatal(err)
	}
	service := NewDataService(manager)
	service.addAdapter(&inferenceTestAdapter{sources: []data.Source{{ID: "v4l2:/dev/video0", Kind: "camera", ClockDomain: "FAKE_NATIVE", Healthy: true}}})
	arming := &holdingArmingAdapter{}
	service.addAdapter(arming)
	service.adapterMu.Lock()
	service.armingAdapter = arming
	service.adapterMu.Unlock()
	service.video = inferenceTestVideo{}
	service.ReconcileArming(context.Background())
	factory := &inferenceTestFactory{sessions: make(chan *inferenceTestSession, 8)}
	stop := service.StartCampaignInference(context.Background(), factory, &inferenceTestSender{requests: make(chan DetectionNotification, 8)})
	t.Cleanup(func() {
		stop()
		for _, key := range manager.ActiveEpisodeKeys() {
			_, _ = service.stopCapture(context.Background(), key)
		}
	})
	return service, factory, arming
}

func inferenceState(t *testing.T, campaign *agentpbv2.DataCampaign) string {
	t.Helper()
	var plan data.Campaign
	if err := json.Unmarshal(campaign.GetPlanJson(), &plan); err != nil {
		t.Fatal(err)
	}
	if plan.InferenceStatus == nil {
		return ""
	}
	return plan.InferenceStatus.State
}

func expectNoNewSession(t *testing.T, service *DataService, factory *inferenceTestFactory) {
	t.Helper()
	service.inference.reconcileNow()
	select {
	case <-factory.sessions:
		t.Fatal("inference started for a disarmed campaign")
	case <-time.After(50 * time.Millisecond):
	}
}

func personDetected(t *testing.T, service *DataService) {
	t.Helper()
	if _, err := service.manager.RecordApplication("sh.wendy.test", data.ApplicationRecord{Version: 1, Type: "event", Name: "person_detected"}); err != nil {
		t.Fatal(err)
	}
}

func TestDisarmStopsTriggersInferenceAndPreRollAndArmRestoresThem(t *testing.T) {
	service, factory, arming := newCampaignStateService(t, t.TempDir())
	ctx := context.Background()
	mustDeploy(t, service, liveCampaignYAML)
	session := receiveInference(t, factory.sessions)
	waitForCampaignState(t, "the camera pre-roll to be armed", func() bool { return arming.holding("c920-yolo-live") })
	armed, err := service.manager.Campaign("c920-yolo-live")
	if err != nil {
		t.Fatal(err)
	}

	response, err := service.CampaignDisarm(ctx, &agentpbv2.DataCampaignDisarmRequest{Name: "c920-yolo-live"})
	if err != nil {
		t.Fatal(err)
	}
	if response.GetCampaign().GetState() != data.CampaignStateDisarmed || !response.GetChanged() || response.GetRecording() != nil {
		t.Fatalf("disarm response = %+v", response)
	}
	select {
	case <-session.closed:
	default:
		t.Fatal("CampaignDisarm returned while the campaign's model process was still running")
	}
	if arming.holding("c920-yolo-live") {
		t.Fatal("the camera pre-roll is still held for a disarmed campaign")
	}
	if got := inferenceState(t, response.GetCampaign()); got != data.CampaignStateDisarmed {
		t.Fatalf("inference_status.state = %q, want disarmed", got)
	}
	expectNoNewSession(t, service, factory)

	// No trigger of any kind opens an episode: a matching event, a manual
	// trigger, or a trigger that resolved the plan before the disarm landed.
	personDetected(t, service)
	if _, err := service.CampaignTrigger(ctx, &agentpbv2.DataCampaignTriggerRequest{Name: "c920-yolo-live"}); status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "wendy data campaign arm c920-yolo-live") {
		t.Fatalf("manual trigger of a disarmed campaign: %v", err)
	}
	if _, err := service.triggerCampaign(ctx, armed, "event:person_detected", "event:person_detected"); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("a trigger holding the armed plan from before the disarm: %v", err)
	}
	if accepted, err := service.triggerInference(ctx, armed, data.ApplicationRecord{Version: 1, Type: "event", Name: "person_detected"}); accepted || err != nil {
		t.Fatalf("a late detection from the retired worker: accepted=%v err=%v", accepted, err)
	}
	time.Sleep(50 * time.Millisecond)
	if keys := service.manager.ActiveEpisodeKeys(); len(keys) != 0 {
		t.Fatalf("a disarmed campaign started recording: %v", keys)
	}

	again, err := service.CampaignDisarm(ctx, &agentpbv2.DataCampaignDisarmRequest{Name: "c920-yolo-live"})
	if err != nil || again.GetChanged() {
		t.Fatalf("disarming twice: changed=%v err=%v", again.GetChanged(), err)
	}

	// Neither a worker holding the plan from before the disarm nor a redeploy
	// of the plan switches the camera back on.
	service.armCampaign(armed)
	redeployed := mustDeploy(t, service, strings.Replace(liveCampaignYAML, "threshold: 0.9", "threshold: 0.8", 1))
	if redeployed.GetState() != data.CampaignStateDisarmed {
		t.Fatalf("redeploying a disarmed campaign armed it: %q", redeployed.GetState())
	}
	time.Sleep(50 * time.Millisecond)
	if arming.holding("c920-yolo-live") {
		t.Fatal("a stale arm or a redeploy re-armed a disarmed campaign's pre-roll")
	}
	expectNoNewSession(t, service, factory)

	arm, err := service.CampaignArm(ctx, &agentpbv2.DataCampaignArmRequest{Name: "c920-yolo-live"})
	if err != nil {
		t.Fatal(err)
	}
	if arm.GetCampaign().GetState() != data.CampaignStateArmed || !arm.GetChanged() || arm.GetCampaign().GetRevision() != redeployed.GetRevision() {
		t.Fatalf("arm response = %+v", arm)
	}
	receiveInference(t, factory.sessions)
	waitForCampaignState(t, "the camera pre-roll to be armed again", func() bool { return arming.holding("c920-yolo-live") })
	personDetected(t, service)
	waitForCampaignState(t, "an armed campaign to record its trigger", func() bool {
		episodes, _ := service.manager.List()
		return len(service.manager.ActiveEpisodeKeys()) > 0 || len(episodes) > 0
	})
}

func TestDisarmedCampaignStaysDisarmedAcrossAnAgentRestart(t *testing.T) {
	root := t.TempDir()
	first, _, _ := newCampaignStateService(t, root)
	mustDeploy(t, first, liveCampaignYAML)
	if _, err := first.CampaignDisarm(context.Background(), &agentpbv2.DataCampaignDisarmRequest{Name: "c920-yolo-live"}); err != nil {
		t.Fatal(err)
	}

	restarted, factory, arming := newCampaignStateService(t, root)
	if arming.armCount() != 0 {
		t.Fatal("boot re-armed the camera pre-roll of a disarmed campaign")
	}
	expectNoNewSession(t, restarted, factory)
	personDetected(t, restarted)
	time.Sleep(50 * time.Millisecond)
	if keys := restarted.manager.ActiveEpisodeKeys(); len(keys) != 0 {
		t.Fatalf("a disarmed campaign recorded after a restart: %v", keys)
	}
	inspected, err := restarted.CampaignInspect(context.Background(), &agentpbv2.DataCampaignInspectRequest{Name: "c920-yolo-live"})
	if err != nil {
		t.Fatal(err)
	}
	if inspected.GetState() != data.CampaignStateDisarmed || inferenceState(t, inspected) != data.CampaignStateDisarmed {
		t.Fatalf("after restart: state %q, inference %q", inspected.GetState(), inferenceState(t, inspected))
	}

	if _, err := restarted.CampaignArm(context.Background(), &agentpbv2.DataCampaignArmRequest{Name: "c920-yolo-live"}); err != nil {
		t.Fatal(err)
	}
	receiveInference(t, factory.sessions)
	waitForCampaignState(t, "the camera pre-roll to be armed", func() bool { return arming.holding("c920-yolo-live") })
}

// TestDisarmLetsTheRecordingInProgressFinish: disarming is not a stop button
// for the episode already running, and the re-arm that follows every episode
// must not switch the campaign's pre-roll back on.
func TestDisarmLetsTheRecordingInProgressFinish(t *testing.T) {
	service, _, arming := newCampaignStateService(t, t.TempDir())
	ctx := context.Background()
	mustDeploy(t, service, strings.Replace(liveCampaignYAML, "after_trigger: 50ms", "after_trigger: 300ms", 1))
	waitForCampaignState(t, "the camera pre-roll to be armed", func() bool { return arming.holding("c920-yolo-live") })
	episode, err := service.CampaignTrigger(ctx, &agentpbv2.DataCampaignTriggerRequest{Name: "c920-yolo-live"})
	if err != nil {
		t.Fatal(err)
	}

	response, err := service.CampaignDisarm(ctx, &agentpbv2.DataCampaignDisarmRequest{Name: "c920-yolo-live"})
	if err != nil {
		t.Fatal(err)
	}
	if response.GetRecording().GetId() != episode.GetId() || len(response.GetWarnings()) == 0 || !strings.Contains(response.GetWarnings()[0], episode.GetId()) {
		t.Fatalf("disarm did not report the recording in progress: %+v", response)
	}
	armsBefore := arming.armCount()
	waitForCampaignState(t, "the recording to finish on its own", func() bool {
		episodes, _ := service.manager.List()
		return len(episodes) == 1 && episodes[0].ID == episode.GetId()
	})
	time.Sleep(50 * time.Millisecond)
	if arming.holding("c920-yolo-live") || arming.armCount() != armsBefore {
		t.Fatal("the end of the recording re-armed a disarmed campaign's pre-roll")
	}
}

func TestRemoveDeletesThePlanAndKeepsTheEpisodes(t *testing.T) {
	service, _, _ := newCampaignStateService(t, t.TempDir())
	ctx := context.Background()
	mustDeploy(t, service, strings.Replace(recordingCampaignYAML, "after_trigger: 1h", "after_trigger: 30ms", 1))
	episode, err := service.CampaignTrigger(ctx, &agentpbv2.DataCampaignTriggerRequest{Name: "dock-door"})
	if err != nil {
		t.Fatal(err)
	}
	waitForCampaignState(t, "the episode to seal", func() bool {
		episodes, _ := service.manager.List()
		return len(episodes) == 1
	})

	response, err := service.CampaignRemove(ctx, &agentpbv2.DataCampaignRemoveRequest{Name: "dock-door"})
	if err != nil {
		t.Fatal(err)
	}
	if response.GetName() != "dock-door" || response.GetSealed() != nil || response.GetRevision() == "" {
		t.Fatalf("remove response = %+v", response)
	}
	if fleet := response.GetWarnings(); len(fleet) != 1 || !strings.Contains(fleet[0], "warehouse-west") || !strings.Contains(fleet[0], "disarm") {
		t.Fatalf("removing a fleet campaign did not warn that the fleet can bring it back: %q", fleet)
	}
	listed, err := service.Campaigns(ctx, &agentpbv2.DataCampaignsRequest{})
	if err != nil || len(listed.GetCampaigns()) != 0 {
		t.Fatalf("campaigns after remove = %v, %v", listed.GetCampaigns(), err)
	}
	if _, err := service.CampaignInspect(ctx, &agentpbv2.DataCampaignInspectRequest{Name: "dock-door"}); status.Code(err) != codes.NotFound {
		t.Fatalf("inspect after remove: %v", err)
	}
	episodes, err := service.Episodes(ctx, &agentpbv2.DataEpisodesRequest{})
	if err != nil || len(episodes.GetEpisodes()) != 1 || episodes.GetEpisodes()[0].GetId() != episode.GetId() {
		t.Fatalf("episodes after remove = %v, %v; removing a campaign must keep what it recorded", episodes.GetEpisodes(), err)
	}
	if _, err := service.Inspect(ctx, &agentpbv2.DataInspectRequest{Episode: episode.GetId(), Verify: true}); err != nil {
		t.Fatalf("the kept episode no longer inspects: %v", err)
	}
	if _, err := service.CampaignTrigger(ctx, &agentpbv2.DataCampaignTriggerRequest{Name: "dock-door"}); status.Code(err) != codes.NotFound {
		t.Fatalf("trigger after remove: %v", err)
	}

	// The plan said upload.when: manual. Removing the campaign must not turn
	// that into "upload everything it recorded".
	manifest, _, err := service.manager.Inspect(episode.GetId(), false)
	if err != nil {
		t.Fatal(err)
	}
	worker := &DataTransferWorker{logger: zap.NewNop(), manager: service.manager}
	if upload, reason := worker.resolveShouldUpload(manifest); upload || !strings.Contains(reason, "manual") {
		t.Fatalf("a removed manual campaign's episode would upload: %v (%s)", upload, reason)
	}
}

func TestRemoveRefusesARecordingCampaignUnlessForcedAndThenSealsItFirst(t *testing.T) {
	service, _, _ := newCampaignStateService(t, t.TempDir())
	ctx := context.Background()
	mustDeploy(t, service, recordingCampaignYAML)
	episode, err := service.CampaignTrigger(ctx, &agentpbv2.DataCampaignTriggerRequest{Name: "dock-door"})
	if err != nil {
		t.Fatal(err)
	}

	_, err = service.CampaignRemove(ctx, &agentpbv2.DataCampaignRemoveRequest{Name: "dock-door"})
	if status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), episode.GetId()) || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("remove while recording: %v", err)
	}
	still, err := service.manager.Campaign("dock-door")
	if err != nil || still.State != data.CampaignStateArmed {
		t.Fatalf("a refused remove changed the campaign: %+v, %v", still, err)
	}
	if keys := service.manager.ActiveEpisodeKeys(); len(keys) != 1 {
		t.Fatalf("a refused remove stopped the recording: %v", keys)
	}

	response, err := service.CampaignRemove(ctx, &agentpbv2.DataCampaignRemoveRequest{Name: "dock-door", Force: true})
	if err != nil {
		t.Fatal(err)
	}
	if response.GetSealed().GetId() != episode.GetId() || response.GetSealed().GetState() != "complete" {
		t.Fatalf("forced remove did not seal the recording first: %+v", response.GetSealed())
	}
	if keys := service.manager.ActiveEpisodeKeys(); len(keys) != 0 {
		t.Fatalf("still recording after a forced remove: %v", keys)
	}
	episodes, err := service.manager.List()
	if err != nil || len(episodes) != 1 || episodes[0].ID != episode.GetId() || episodes[0].State != "complete" {
		t.Fatalf("episodes after a forced remove = %+v, %v", episodes, err)
	}
	if _, err := service.manager.Campaign("dock-door"); err == nil {
		t.Fatal("the plan survived a forced remove")
	}
}

func TestCampaignStateCallsNameAMissingCampaign(t *testing.T) {
	service, _, _ := newCampaignStateService(t, t.TempDir())
	ctx := context.Background()
	_, armErr := service.CampaignArm(ctx, &agentpbv2.DataCampaignArmRequest{Name: "missing"})
	_, disarmErr := service.CampaignDisarm(ctx, &agentpbv2.DataCampaignDisarmRequest{Name: "missing"})
	_, removeErr := service.CampaignRemove(ctx, &agentpbv2.DataCampaignRemoveRequest{Name: "missing"})
	for _, err := range []error{armErr, disarmErr, removeErr} {
		if status.Code(err) != codes.NotFound || !strings.Contains(err.Error(), "campaign missing is not deployed") {
			t.Fatalf("missing campaign: %v", err)
		}
	}
	if _, err := service.CampaignDisarm(ctx, &agentpbv2.DataCampaignDisarmRequest{Name: "../x"}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("a path as a campaign name: %v", err)
	}

	mustDeploy(t, service, recordingCampaignYAML)
	response, err := service.CampaignDisarm(ctx, &agentpbv2.DataCampaignDisarmRequest{Name: "dock-door"})
	if err != nil {
		t.Fatal(err)
	}
	if warnings := response.GetWarnings(); len(warnings) != 1 || !strings.Contains(warnings[0], "warehouse-west") || !strings.Contains(warnings[0], "stays disarmed") {
		t.Fatalf("disarming a fleet campaign: warnings %q", warnings)
	}
}
