package services

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	"github.com/wendylabsinc/wendy/go/internal/agent/data/live"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"github.com/wendylabsinc/wendy/go/proto/gen/datapb"
)

const (
	ruleCampaign  = "thermal-watch"
	ruleMarkersID = "agent/campaign/" + ruleCampaign + "/rules/hotspot-alarm/markers/" + thermalSource
)

// thermalRulesCampaign is the shipped example on the test rig's thermal
// camera, /dev/video4, sealing without a drain so a test can stop it.
func thermalRulesCampaign(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile("../../../../Examples/WendyDataThermalRules/campaign.yaml")
	if err != nil {
		t.Fatal(err)
	}
	text := strings.ReplaceAll(string(raw), "/dev/video2", "/dev/video4")
	text = strings.Replace(text, "  buffer: 10s\n", "  buffer: 10s\n  drain: 0s\n", 1)
	return []byte(text)
}

// alarmOnlyCampaign has a rule that raises "alarm" on every hot frame, a
// second apart at most, records nothing, and notifies on each alarm.
func alarmOnlyCampaign(rules bool) []byte {
	text := `version: 1
name: ` + ruleCampaign + `
sources:
  - camera: /dev/video4
capture:
  buffer: 0s
  drain: 0s
  after_trigger: 10s
  triggers:
    - event: inspection
upload:
  when: manual
export:
  annotation: none
notify:
  on: event
  event: alarm
`
	if rules {
		text += `rules:
  - name: hotspot-alarm
    input: agent/camera/grid/v4l2:/dev/video4
    steps:
      - bands: [{name: hot, min: 50, color: red}]
      - areas: {min_cells: 4}
    outputs:
      markers: areas
      events:
        - {name: alarm, when: area.band == hot}
`
	}
	return []byte(text)
}

// hotBlock is a 22 °C room with a 4x4 sensor-pixel block at celsius.
func hotBlock(celsius float64) func(int, int) float64 {
	return func(col, row int) float64 {
		if col >= 100 && col <= 103 && row >= 80 && row <= 83 {
			return celsius
		}
		return 22
	}
}

// ruleService is a data service with campaign notifications through sender,
// and a camera source the rig's thermal camera answers to.
func ruleService(t *testing.T, manager *data.Manager, sender CampaignNotificationSender) *DataService {
	t.Helper()
	service := NewDataService(manager)
	service.addAdapter(&inferenceTestAdapter{sources: []data.Source{{ID: thermalSource, Kind: "camera", Healthy: true}}})
	service.video = inferenceTestVideo{}
	stop := service.StartCampaignInference(context.Background(), nil, sender)
	t.Cleanup(func() {
		stop()
		for _, key := range manager.ActiveEpisodeKeys() {
			_, _ = service.stopCapture(context.Background(), key)
		}
	})
	return service
}

func deployRules(t *testing.T, service *DataService, yaml []byte) {
	t.Helper()
	if _, err := service.CampaignDeploy(context.Background(), &agentpbv2.DataCampaignDeployRequest{CampaignYaml: yaml}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the campaign's notification job runs", func() bool {
		service.inference.mu.Lock()
		defer service.inference.mu.Unlock()
		return service.inference.jobs[ruleCampaign] != nil
	})
}

func ruleEvents(t *testing.T, manager *data.Manager, name string) []data.DeviceEvent {
	t.Helper()
	events, _, _, err := manager.DeviceEvents(campaignAppPrefix+ruleCampaign, name, "", true)
	if err != nil {
		t.Fatal(err)
	}
	return events
}

func frameTime(nanos int64) string { return time.Unix(0, nanos).UTC().Format(time.RFC3339Nano) }

// A fake TC001 sequence through the shipped example: 2 s of a 63.1 °C block
// draws a red box on the rule's markers stream, raises thermal_alarm into the
// Events inbox, starts the campaign's recording with the event in its
// events.jsonl, and sends its notification with a plain text.
func TestGridRuleDrivesMarkersEventRecordingAndNotification(t *testing.T) {
	manager, err := data.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sender := &inferenceTestSender{requests: make(chan DetectionNotification, 8)}
	service := ruleService(t, manager, sender)
	deployRules(t, service, thermalRulesCampaign(t))
	if !service.rules.wants(thermalGridID) {
		t.Fatal("the rule does not ask for its grid")
	}
	viewer, err := manager.Live().Subscribe(live.Selection{StreamIDs: []string{ruleMarkersID}})
	if err != nil {
		t.Fatal(err)
	}
	defer viewer.Close()

	hot := thermalGrid(stackedThermalFrame(hotBlock(63.1)), "/dev/video4", noCorrection)
	frame := 40 * time.Millisecond
	start := time.Now().Add(-3 * time.Second).UnixNano()
	at := func(i int) int64 { return start + int64(i)*int64(frame) }
	for i := 0; i < 50; i++ { // 1.96 s
		service.rules.observe(thermalGridID, hot, at(i))
	}
	if events := ruleEvents(t, manager, ""); len(events) != 0 {
		t.Fatalf("events before 2 s of red: %+v", events)
	}
	service.rules.observe(thermalGridID, hot, at(50))
	alarmAt := at(50)

	// Markers: the newest frame's red box, on the frame's time.
	r := nextRecord(t, viewer)
	if r.StreamID != ruleMarkersID || r.Schema != "wendy.data.v1.VideoMarkers" || r.TimestampUnixNanos != alarmAt || r.SourceID != thermalSource {
		t.Fatalf("markers record %+v", r)
	}
	markers := unmarshalPayload(t, r, &datapb.VideoMarkers{})
	if markers.GetCamera() != "/dev/video4" || len(markers.GetMarkers()) != 1 {
		t.Fatalf("markers %v", markers)
	}
	box := markers.GetMarkers()[0]
	if box.GetKind() != datapb.Marker_KIND_BOX || box.GetLabel() != "hot" || box.GetColor() != "red" || box.GetUnit() != "°C" ||
		math.Abs(box.GetValue()-63.1) > 0.01 || !within(box.GetX(), 102.0/256, 1e-6) || !within(box.GetY(), 82.0/192, 1e-6) ||
		!within(box.GetWidth(), 4.0/256, 1e-6) || !within(box.GetHeight(), 4.0/192, 1e-6) {
		t.Fatalf("box %v", box)
	}

	// The event, in the durable Events inbox.
	events := ruleEvents(t, manager, "")
	if len(events) != 1 || events[0].Name != "thermal_alarm" {
		t.Fatalf("events %+v", events)
	}
	attrs := events[0].Attributes
	if attrs["campaign"] != ruleCampaign || attrs["rule"] != "hotspot-alarm" || attrs["source_id"] != thermalSource || attrs["camera"] != "/dev/video4" ||
		attrs["band"] != "hot" || attrs["unit"] != "°C" || attrs["cells"] != float64(16) || math.Abs(attrs["max"].(float64)-63.1) > 0.01 || attrs["frame_time"] != frameTime(alarmAt) {
		t.Fatalf("event attributes %v", attrs)
	}
	if box := attrs["box"].(map[string]any); math.Abs(box["x"].(float64)-102.0/256) > 1e-9 || math.Abs(box["height"].(float64)-4.0/192) > 1e-9 {
		t.Fatalf("event box %v", box)
	}

	// The recording, which holds the event that started it.
	eventually(t, "the alarm starts the campaign's recording", func() bool {
		_, ok := manager.ActiveSession(ruleCampaign)
		return ok
	})
	session, _ := manager.ActiveSession(ruleCampaign)
	journal, err := os.ReadFile(filepath.Join(session.Directory, "events.jsonl"))
	if err != nil || !strings.Contains(string(journal), `"name":"thermal_alarm"`) || !strings.Contains(string(journal), `"rule":"hotspot-alarm"`) {
		t.Fatalf("events.jsonl %q %v", journal, err)
	}
	if status := manager.Status(); status == nil || status.Trigger.Reason != "event:thermal_alarm" {
		t.Fatalf("episode %+v", status)
	}

	// The notification.
	request := receiveInference(t, sender.requests)
	if request.Event != "thermal_alarm" || request.Campaign != ruleCampaign || request.SourceID != thermalSource ||
		request.Message != "thermal_alarm on /dev/video4: 63.1 °C area" || request.OccurredAt != frameTime(alarmAt) {
		t.Fatalf("notification %+v", request)
	}
	if _, err := uuid.Parse(request.ID); err != nil {
		t.Fatal(err)
	}
	retained, _, _, err := manager.Notifications("", "", "", true)
	if err != nil || len(retained) != 1 || retained[0].Message != request.Message {
		t.Fatalf("retained %+v %v", retained, err)
	}

	// While it stays red, and while it cools to 49 °C, above clear_below, it
	// is the same alarm. Cooled below 48 °C it clears, and the cooldown keeps
	// the next red for 2 minutes from raising another.
	cooling := thermalGrid(stackedThermalFrame(hotBlock(49)), "/dev/video4", noCorrection)
	cooled := thermalGrid(stackedThermalFrame(hotBlock(30)), "/dev/video4", noCorrection)
	i := 51
	for _, step := range []struct {
		grid   *datapb.Grid
		frames int
	}{{hot, 25}, {cooling, 25}, {hot, 25}, {cooled, 5}, {hot, 100}} {
		for n := 0; n < step.frames; n++ {
			service.rules.observe(thermalGridID, step.grid, at(i))
			i++
		}
	}
	if events := ruleEvents(t, manager, ""); len(events) != 1 {
		t.Fatalf("%d events; want the one alarm", len(events))
	}

	inspection, err := service.CampaignInspect(context.Background(), &agentpbv2.DataCampaignInspectRequest{Name: ruleCampaign})
	if err != nil {
		t.Fatal(err)
	}
	var plan data.Campaign
	if err := json.Unmarshal(inspection.PlanJson, &plan); err != nil {
		t.Fatal(err)
	}
	st := plan.RulesStatus["hotspot-alarm"]
	if st.State != "running" || st.Events != 1 || st.LastEvent != "thermal_alarm" || st.LastEventAt != frameTime(alarmAt) || st.MarkersStream != ruleMarkersID || st.LastFrameAt != frameTime(at(i-1)) {
		t.Fatalf("rule status %+v", st)
	}
}

// The rules run on every frame the decoder hands them, so a viewer that reads
// slowly costs it markers, which are newest-wins, and never an event: ten
// alarms in twenty frames are ten events in the inbox and ten notifications,
// while a viewer that read nothing meanwhile gets one markers record, told it
// skipped the rest.
func TestGridRuleEventsAreNotCoalescedForASlowViewer(t *testing.T) {
	manager, err := data.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sender := &inferenceTestSender{requests: make(chan DetectionNotification, 32)}
	service := ruleService(t, manager, sender)
	deployRules(t, service, alarmOnlyCampaign(true))
	stalled, err := manager.Live().Subscribe(live.Selection{StreamIDs: []string{ruleMarkersID}})
	if err != nil {
		t.Fatal(err)
	}
	defer stalled.Close()

	hot := thermalGrid(stackedThermalFrame(hotBlock(70)), "/dev/video4", noCorrection)
	cold := thermalGrid(stackedThermalFrame(uniform(22)), "/dev/video4", noCorrection)
	start := time.Now().UnixNano()
	var want []string
	for i := 0; i < 20; i++ {
		grid := cold
		if i%2 == 0 {
			grid = hot
			want = append(want, frameTime(start+int64(i)*int64(time.Second)))
		}
		service.rules.observe(thermalGridID, grid, start+int64(i)*int64(time.Second))
	}

	events := ruleEvents(t, manager, "alarm")
	if len(events) != len(want) {
		t.Fatalf("%d events for %d alarms", len(events), len(want))
	}
	for i, event := range events {
		if event.Attributes["frame_time"] != want[i] {
			t.Fatalf("event %d is of frame %v, want %s", i, event.Attributes["frame_time"], want[i])
		}
	}
	for range want {
		if request := receiveInference(t, sender.requests); request.Event != "alarm" {
			t.Fatalf("notification %+v", request)
		}
	}
	if retained, _, _, err := manager.Notifications("", "alarm", "", true); err != nil || len(retained) != len(want) {
		t.Fatalf("%d notifications kept: %v", len(retained), err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	d, err := stalled.Next(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if d.Record.TimestampUnixNanos != start+19*int64(time.Second) || d.Skipped != 19 {
		t.Fatalf("slow viewer got the frame of %d after skipping %d", d.Record.TimestampUnixNanos-start, d.Skipped)
	}
	requireNoRecord(t, stalled)
}

// An active rule is demand: with no viewer at all, the publisher starts the
// thermal camera's decoder, as a parameter-less raw subscriber, and the rule
// gets its frames. Without the rule, the decoder stops.
func TestGridRuleKeepsTheDecoderRunningWithoutAViewer(t *testing.T) {
	var service *DataService
	rig := newGridRigWith(t, func(rig *gridRig, g *cameraGrids) {
		service = NewDataService(rig.manager)
		g.rules = service.rules
	}, "/dev/video0", "/dev/video4")
	never(t, "a producer started with no rule and no viewer", func() bool { return rig.fp.count() > 0 })

	if _, err := rig.manager.DeployCampaign(alarmOnlyCampaign(true)); err != nil {
		t.Fatal(err)
	}
	service.rules.reconcile()
	hub := rig.thermalHub(t, 0, tc001Format, tc001PictureTop)
	if rig.manager.Live().Wanted(thermalGridID, "wendy.data.v1.Grid") || rig.manager.Live().Subscribed() {
		t.Fatal("a viewer asks for the grid; the rule alone should")
	}
	frameAt := time.Now().UnixNano()
	if !hub.publishRaw(stackedThermalFrame(hotBlock(70)), uint64(frameAt), tc001Format) {
		t.Fatal("the hub has no raw subscriber")
	}
	eventually(t, "the rule raises its alarm", func() bool { return len(ruleEvents(t, rig.manager, "alarm")) == 1 })
	if got := ruleEvents(t, rig.manager, "alarm")[0].Attributes["frame_time"]; got != frameTime(frameAt) {
		t.Fatalf("event of frame %v, want the raw frame's timestamp_ns %s", got, frameTime(frameAt))
	}

	// The campaign redeployed without its rule: nothing asks for the grid.
	if _, err := rig.manager.DeployCampaign(alarmOnlyCampaign(false)); err != nil {
		t.Fatal(err)
	}
	service.rules.reconcile()
	eventually(t, "the raw subscription ends", func() bool { return hub.ctx.Err() != nil })
	if rig.fp.count() != 1 {
		t.Fatalf("%d producers started", rig.fp.count())
	}
}

// A redeploy of the same plan keeps a rule's timing, so it cannot cut a
// cooldown short; a changed plan starts it over and takes the old rule's
// markers stream out of the catalog until the new one draws.
func TestGridRuleRedeploy(t *testing.T) {
	manager, err := data.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	service := ruleService(t, manager, &inferenceTestSender{requests: make(chan DetectionNotification, 8)})
	deployRules(t, service, thermalRulesCampaign(t))
	key := gridRuleKey(ruleCampaign, "hotspot-alarm")
	first := service.rules.runs[key]
	service.rules.observe(thermalGridID, thermalGrid(stackedThermalFrame(uniform(22)), "/dev/video4", noCorrection), time.Now().UnixNano())
	if !first.advertised {
		t.Fatal("markers stream not listed after a frame")
	}
	deployRules(t, service, thermalRulesCampaign(t))
	if service.rules.runs[key] != first {
		t.Fatal("the same plan replaced the rule")
	}
	deployRules(t, service, []byte(strings.Replace(string(thermalRulesCampaign(t)), "cooldown: 2m", "cooldown: 5m", 1)))
	if second := service.rules.runs[key]; second == first || !first.retired || first.advertised || second.advertised {
		t.Fatal("a changed plan kept the old rule")
	}
}

// Deployment refuses rules an agent without camera grids cannot run, and a bad
// rule, with the reason.
func TestGridRuleDeploymentRefusals(t *testing.T) {
	manager, err := data.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	service := NewDataService(manager)
	silent := strings.Replace(string(alarmOnlyCampaign(true)), "notify:\n  on: event\n  event: alarm\n", "", 1)
	if strings.Contains(silent, "notify") {
		t.Fatal("the plan still notifies")
	}
	_, err = service.CampaignDeploy(context.Background(), &agentpbv2.DataCampaignDeployRequest{CampaignYaml: []byte(silent)})
	if status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "cannot run campaign rules") {
		t.Fatalf("no camera runtime: %v", err)
	}
	service.video = inferenceTestVideo{}
	bad := strings.Replace(string(alarmOnlyCampaign(true)), "area.band == hot", "area.band == red", 1)
	_, err = service.CampaignDeploy(context.Background(), &agentpbv2.DataCampaignDeployRequest{CampaignYaml: []byte(bad)})
	if status.Code(err) != codes.InvalidArgument || !strings.Contains(err.Error(), "no band is named red; the bands are hot") {
		t.Fatalf("bad rule: %v", err)
	}
	if campaigns, err := manager.Campaigns(); err != nil || len(campaigns) != 0 {
		t.Fatalf("a refused plan was stored: %v %v", campaigns, err)
	}
}

// A rule's notification reaches Cloud with its text as the body.
func TestCampaignCloudNotificationUsesTheRuleText(t *testing.T) {
	cloud := &recordingNotificationSender{}
	sender := &CampaignCloudSender{Cloud: cloud}
	text := "thermal_alarm on /dev/video4: 63.1 °C area"
	if err := sender.Send(context.Background(), "", DetectionNotification{ID: uuid.NewString(), Event: "thermal_alarm", Campaign: ruleCampaign, Message: text}); err != nil {
		t.Fatal(err)
	}
	if err := sender.Send(context.Background(), "", DetectionNotification{ID: uuid.NewString(), Event: "person_detected", Campaign: "people"}); err != nil {
		t.Fatal(err)
	}
	if len(cloud.requests) != 2 || cloud.requests[0].GetBody() != text || cloud.requests[1].GetBody() != "Campaign people emitted person_detected." {
		t.Fatalf("bodies %q", []string{cloud.requests[0].GetBody(), cloud.requests[1].GetBody()})
	}
}

// A grid that cannot be read is skipped and logged once, and the rule goes on
// with the next one.
func TestGridRuleSkipsAGridWithoutValues(t *testing.T) {
	manager, err := data.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var warnings []string
	manager.SetWarnLogger(func(msg string) {
		mu.Lock()
		warnings = append(warnings, msg)
		mu.Unlock()
	})
	service := ruleService(t, manager, &inferenceTestSender{requests: make(chan DetectionNotification, 8)})
	deployRules(t, service, alarmOnlyCampaign(true))
	now := time.Now().UnixNano()
	for i := 0; i < 3; i++ {
		service.rules.observe(thermalGridID, &datapb.Grid{Width: 2, Height: 2}, now+int64(i))
	}
	service.rules.observe(thermalGridID, thermalGrid(stackedThermalFrame(hotBlock(70)), "/dev/video4", noCorrection), now+10)
	mu.Lock()
	defer mu.Unlock()
	skipped := 0
	for _, w := range warnings {
		if strings.Contains(w, "skips frames") {
			skipped++
		}
	}
	if skipped != 1 || len(ruleEvents(t, manager, "alarm")) != 1 {
		t.Fatalf("warnings %q", warnings)
	}
}
