package services

// Campaign grid rules: "40 to 50 °C is a yellow area, above 50 °C a red one,
// alert on red", written in a campaign's rules block and run by the agent, with
// no application.
//
// A rule reads one of the agent's camera grids, agent/camera/grid/<source>
// (camera_grid.go), and the generic steps in gridrules turn each frame into
// areas of named bands. Its outputs are:
//
//	agent/campaign/<campaign>/rules/<rule>/markers/<source>
//	    wendy.data.v1.VideoMarkers, a box per area in its band's colour,
//	    labelled with the band and the area's highest value
//	events
//	    application events of the campaign, sh.wendy.campaign.<campaign>,
//	    that start its recording and send its notification
//
// Alarms are never dropped. The rules do not watch the live stream, where a
// slow viewer gets only each stream's newest record: the camera's decoder
// hands every grid it makes to the rules, synchronously, before it reads the
// next frame. An event goes into Wendy Data's event path, the one campaign
// triggers and the durable Events inbox use (RecordCampaignApplication): the
// journal, the pre-roll, and events.jsonl of an episode that is recording.
// Only the markers are live only and newest-wins, as display data.
//
// A rule is demand. While a deployed campaign has a rule on a camera's grid,
// that camera's decoder runs whether or not a viewer is connected, and with
// it the camera.

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	"github.com/wendylabsinc/wendy/go/internal/agent/data/gridrules"
	"github.com/wendylabsinc/wendy/go/proto/gen/datapb"
	recordingpb "github.com/wendylabsinc/wendy/go/proto/gen/recordingpb"
)

const (
	// gridRuleMarkersValidFor is how long a viewer shows a rule's boxes
	// without a newer frame, as the thermal markers.
	gridRuleMarkersValidFor = 2 * time.Second
	// gridRuleIdleAfter is how long after its last frame a rule reports
	// waiting_for_frames.
	gridRuleIdleAfter = 10 * time.Second
)

// gridRuleMarkersStream is a rule's markers stream on one camera, below
// "agent/", beside the campaign's inference markers. The campaign is part of
// it because rule names are unique only within a campaign.
func gridRuleMarkersStream(campaign, rule, sourceID string) string {
	return "campaign/" + campaign + "/rules/" + rule + "/markers/" + sourceID
}

// gridRuleRuntime runs the rules of every deployed campaign. The camera grid
// publisher asks it which grids are wanted and hands it their frames.
type gridRuleRuntime struct {
	service *DataService
	mu      sync.Mutex
	// runs by campaign and rule name, and by the grid stream id they read.
	// A byInput slice is replaced, never changed, so observe may iterate it
	// without the lock.
	runs    map[string]*gridRuleRun
	byInput map[string][]*gridRuleRun
	// failed holds why a deployed rule does not run, by the same key.
	failed map[string]string
	// changed is closed when the set of rules changes.
	changed chan struct{}
	// now is the wall clock, for status only; tests set it.
	now func() time.Time
}

// gridRuleRun is one rule of one deployed campaign revision.
type gridRuleRun struct {
	campaign data.Campaign
	program  *gridrules.Program
	sourceID string
	// markers is the rule's markers stream below "agent/", "" without one.
	markers string

	// mu serialises frames and guards what follows. Only the camera's
	// decoder goroutine evaluates, so it is not contended.
	mu         sync.Mutex
	eval       *gridrules.Evaluator
	retired    bool
	advertised bool
	// failure is why the last frame could not be evaluated, and
	// markersFailure why its markers could not be published, each logged
	// once.
	failure        string
	markersFailure string
	lastFrame      int64
	events         uint64
	lastEvent      string
	lastAt         int64
}

func newGridRuleRuntime(service *DataService) *gridRuleRuntime {
	return &gridRuleRuntime{
		service: service,
		runs:    map[string]*gridRuleRun{},
		byInput: map[string][]*gridRuleRun{},
		failed:  map[string]string{},
		changed: make(chan struct{}),
		now:     time.Now,
	}
}

func gridRuleKey(campaign, rule string) string { return campaign + "/" + rule }

// reconcile installs the rules of every deployed campaign, keeping the timing
// of a rule whose campaign revision did not change, and retires the others.
func (r *gridRuleRuntime) reconcile() {
	campaigns, err := r.service.manager.Campaigns()
	if err != nil {
		r.service.manager.Warnf("reading campaigns for their grid rules failed; the rules deployed before keep running: %v", err)
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	next := map[string]*gridRuleRun{}
	failed := map[string]string{}
	for _, campaign := range campaigns {
		if campaign.State != "armed" {
			continue
		}
		for _, rule := range campaign.Rules {
			key := gridRuleKey(campaign.Name, rule.Name)
			if run := r.runs[key]; run != nil && run.campaign.Revision == campaign.Revision {
				next[key] = run
				continue
			}
			program, err := rule.Compile()
			source, ok := strings.CutPrefix(rule.Input, data.RuleInputPrefix)
			if err == nil && (!ok || source == "") {
				err = fmt.Errorf("input %s is not one of the agent's camera grids", rule.Input)
			}
			if err != nil {
				// Deployment validated it, so an agent that reads the plan
				// differently, such as an older one after a rollback, says why
				// it does not run the rule instead of running a different one.
				failed[key] = err.Error()
				if r.failed[key] != failed[key] {
					r.service.manager.Warnf("campaign %q rule %q does not run on this agent: %v", campaign.Name, rule.Name, err)
				}
				continue
			}
			run := &gridRuleRun{campaign: campaign, program: program, sourceID: source, eval: gridrules.NewEvaluator(program)}
			if program.Markers() {
				run.markers = gridRuleMarkersStream(campaign.Name, rule.Name, source)
			}
			next[key] = run
		}
	}
	for key, run := range r.runs {
		if next[key] != run {
			run.retire(r.service.manager)
		}
	}
	byInput := map[string][]*gridRuleRun{}
	for _, run := range next {
		byInput[run.program.Input()] = append(byInput[run.program.Input()], run)
	}
	r.runs, r.byInput, r.failed = next, byInput, failed
	close(r.changed)
	r.changed = make(chan struct{})
}

func (run *gridRuleRun) retire(manager *data.Manager) {
	run.mu.Lock()
	defer run.mu.Unlock()
	run.retired = true
	if run.advertised {
		manager.UnadvertiseAgentStream(run.markers)
		run.advertised = false
	}
}

// changes returns a channel that is closed when the rules change.
func (r *gridRuleRuntime) changes() <-chan struct{} {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.changed
}

// wants reports whether a rule reads the grid stream streamID, the full id,
// "agent/camera/grid/<source>". It is the rules' demand for the grid.
func (r *gridRuleRuntime) wants(streamID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.byInput[streamID]) > 0
}

// observe runs every rule that reads streamID on one of its frames, taken at
// unixNanos. The camera's decoder calls it for every grid it makes, before it
// reads the next frame, so no rule misses a frame the decoder saw. grid is
// only read, and only during the call.
func (r *gridRuleRuntime) observe(streamID string, grid *datapb.Grid, unixNanos int64) {
	r.mu.Lock()
	runs := r.byInput[streamID]
	r.mu.Unlock()
	for _, run := range runs {
		r.observeRun(run, grid, unixNanos)
	}
}

func (r *gridRuleRuntime) observeRun(run *gridRuleRun, grid *datapb.Grid, unixNanos int64) {
	manager := r.service.manager
	run.mu.Lock()
	defer run.mu.Unlock()
	if run.retired {
		return
	}
	frame, err := run.eval.Evaluate(grid, unixNanos)
	if err != nil {
		if msg := err.Error(); msg != run.failure {
			run.failure = msg
			manager.Warnf("campaign %q rule %q skips frames of %s: %v", run.campaign.Name, run.program.Name(), run.sourceID, err)
		}
		return
	}
	run.failure, run.lastFrame = "", unixNanos
	if run.markers != "" {
		if !run.advertised {
			manager.AdvertiseAgentStream(run.markers, markersDescriptor)
			run.advertised = true
		}
		r.publishMarkers(run, frame, grid, unixNanos)
	}
	for _, fired := range frame.Fired {
		run.events++
		run.lastEvent, run.lastAt = fired.Event, fired.FrameNanos
		r.service.emitGridRuleEvent(run.campaign, run.program.Name(), run.sourceID, grid, fired)
	}
}

// publishMarkers puts a frame's areas on the rule's markers stream, stamped
// with the frame's time and naming the camera as their source. They are live
// only (PublishLive): display data at frame rate, newest-wins for a slow
// viewer, and published without the recording lock, which an episode seal can
// hold, so the decoder never waits on it. Episodes get the rule's events.
func (r *gridRuleRuntime) publishMarkers(run *gridRuleRun, frame gridrules.Frame, grid *datapb.Grid, unixNanos int64) {
	payload, err := proto.Marshal(gridrules.Markers(frame, grid, gridRuleMarkersValidFor))
	if err == nil {
		err = r.service.manager.PublishLive(run.markers, markersDescriptor, &recordingpb.Record{
			Payload: payload,
			Timing:  &recordingpb.SampleTiming{Clock: "CLOCK_REALTIME", Count: 1, TimestampsNanos: []int64{unixNanos}},
			Inputs:  []*recordingpb.SampleRef{{SourceId: run.sourceID}},
		})
	}
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	if msg != run.markersFailure {
		run.markersFailure = msg
		if err != nil {
			r.service.manager.Warnf("campaign %q rule %q markers: %v", run.campaign.Name, run.program.Name(), err)
		}
	}
}

// detach marks the rules on a camera that went away: their markers stream
// is gone until the camera's grid is decoded again.
func (r *gridRuleRuntime) detach(sourceID string) {
	r.mu.Lock()
	runs := make([]*gridRuleRun, 0, len(r.runs))
	for _, run := range r.runs {
		if run.sourceID == sourceID {
			runs = append(runs, run)
		}
	}
	r.mu.Unlock()
	for _, run := range runs {
		run.mu.Lock()
		if run.advertised {
			r.service.manager.UnadvertiseAgentStream(run.markers)
			run.advertised = false
		}
		run.mu.Unlock()
	}
}

// status reports the rules of one campaign revision for campaign inspect.
func (r *gridRuleRuntime) status(campaign data.Campaign) data.RuleStatuses {
	if len(campaign.Rules) == 0 {
		return nil
	}
	out := data.RuleStatuses{}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, rule := range campaign.Rules {
		key := gridRuleKey(campaign.Name, rule.Name)
		if msg, ok := r.failed[key]; ok {
			out[rule.Name] = data.RuleStatus{State: "error", Error: msg}
			continue
		}
		run := r.runs[key]
		if run == nil || run.campaign.Revision != campaign.Revision {
			out[rule.Name] = data.RuleStatus{State: "pending"}
			continue
		}
		run.mu.Lock()
		st := data.RuleStatus{State: "waiting_for_frames", Error: run.failure, Events: run.events, LastEvent: run.lastEvent}
		if run.markers != "" {
			st.MarkersStream = "agent/" + run.markers
		}
		if run.lastFrame != 0 {
			st.LastFrameAt = time.Unix(0, run.lastFrame).UTC().Format(time.RFC3339Nano)
			if r.now().Sub(time.Unix(0, run.lastFrame)) < gridRuleIdleAfter {
				st.State = "running"
			}
		}
		if run.lastAt != 0 {
			st.LastEventAt = time.Unix(0, run.lastAt).UTC().Format(time.RFC3339Nano)
		}
		run.mu.Unlock()
		out[rule.Name] = st
	}
	return out
}

// emitGridRuleEvent puts a rule's event on the campaign's event path, then
// starts the campaign's recording when a trigger names the event, and sends
// its notification when notify names it. A rule's events reach only its own
// campaign, as a campaign's inference events do.
//
// The record is written before this returns, durably: the Events inbox, the
// pre-roll a recording started by it opens with, and events.jsonl of every
// episode already recording applications. Starting the recording can wait for
// a camera, so it runs on its own goroutine.
func (s *DataService) emitGridRuleEvent(campaign data.Campaign, rule, sourceID string, grid *datapb.Grid, fired gridrules.Firing) {
	area := fired.Area
	x, y, width, height := area.Box()
	frameTime := time.Unix(0, fired.FrameNanos).UTC()
	attributes := map[string]any{
		"campaign":  campaign.Name,
		"rule":      rule,
		"source_id": sourceID,
		"camera":    grid.GetCamera(),
		"band":      area.Band,
		"max":       area.Max,
		"mean":      area.Mean,
		"unit":      grid.GetUnit(),
		"cells":     area.Cells,
		// The VideoMarkers box convention: centred on (x, y), normalised.
		"box": map[string]any{"x": x, "y": y, "width": width, "height": height},
		// A string, exact to the nanosecond: JSON readers that hold numbers
		// as doubles would round nanoseconds since 1970.
		"frame_time": frameTime.Format(time.RFC3339Nano),
	}
	if c := grid.GetCorrection(); c != nil {
		attributes["correction"] = map[string]any{"gain": c.GetGain(), "offset": c.GetOffset()}
	}
	record := data.ApplicationRecord{Version: 1, Type: "event", Name: fired.Event, Value: area.Max, Attributes: attributes,
		Inputs: []data.SampleRef{{SourceID: sourceID}}}
	if _, err := s.manager.RecordCampaignApplication(campaignAppPrefix+campaign.Name, record); err != nil {
		s.manager.Warnf("campaign %q rule %q: recording event %s failed: %v", campaign.Name, rule, fired.Event, err)
	}
	if reason, expression, matched := campaign.Match(record); matched {
		go s.triggerGridRule(campaign, reason, expression)
	}
	if n := campaign.Notify; n != nil && n.On == data.NotifyOnEvent && n.Event == fired.Event {
		camera := grid.GetCamera()
		if camera == "" {
			camera = sourceID
		}
		s.notifyGridRuleEvent(campaign, DetectionNotification{
			ID:         uuid.NewString(),
			Event:      fired.Event,
			Campaign:   campaign.Name,
			SourceID:   sourceID,
			Count:      1,
			OccurredAt: frameTime.Format(time.RFC3339Nano),
			Message:    gridRuleMessage(fired.Event, camera, area.Max, grid.GetUnit()),
		})
	}
}

// gridRuleMessage is a rule notification's text, for example
// "thermal_alarm on /dev/video2: 63.1 °C area".
func gridRuleMessage(event, camera string, value float64, unit string) string {
	return fmt.Sprintf("%s on %s: %.1f %s area", event, camera, value, unit)
}

// triggerGridRule starts the campaign's recording for a rule event, unless the
// campaign was redeployed meanwhile or is already recording: a campaign has at
// most one capture, and a trigger during it is dropped, as for any trigger.
// The event itself is already recorded.
func (s *DataService) triggerGridRule(campaign data.Campaign, reason, expression string) {
	s.deploymentMu.Lock()
	defer s.deploymentMu.Unlock()
	current, err := s.manager.Campaign(campaign.Name)
	if err != nil || current.Revision != campaign.Revision || current.State != "armed" {
		return
	}
	if _, active := s.manager.ActiveSession(campaign.Name); active {
		return
	}
	if _, err := s.triggerCampaign(context.Background(), current, reason, expression); err != nil {
		s.manager.Warnf("campaign %q matched %s from a grid rule but starting its episode failed: %v", campaign.Name, reason, err)
	}
}

// notifyGridRuleEvent sends a rule event's notification through the
// campaign's notification job, the one notify.on: event runs. The
// notification is kept in Wendy Data's notification journal first either way.
func (s *DataService) notifyGridRuleEvent(campaign data.Campaign, request DetectionNotification) {
	if s.inference != nil && s.inference.enqueueFor(campaign, request) {
		return
	}
	if err := s.manager.RecordNotification(request); err != nil {
		s.manager.Warnf("campaign %q notification for %s could not be kept: %v", campaign.Name, request.Event, err)
		return
	}
	s.manager.Warnf("campaign %q notification for %s is kept in Wendy Data but not sent: the campaign's notification runtime is not running yet", campaign.Name, request.Event)
}
