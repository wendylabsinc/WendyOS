package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"strings"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"gopkg.in/yaml.v3"
)

const (
	// A 60 s lease renewed every 20 s survives two missed renewals (design §6.3).
	watchLease         = "60s"
	watchRenewEvery    = 20 * time.Second
	watchStatusEvery   = 5 * time.Second
	watchPollEvery     = time.Second
	watchInferenceRate = 2
	watchCallTimeout   = 10 * time.Second
	watchGoneReason    = "the device restarted or the watch expired"
)

// errWatchAgentTooOld is returned for agents without leased campaigns. Their
// campaign parser rejects the unknown lease field and they lack the renew and
// remove RPCs, so a refused watch leaves nothing on the device (design §5.7).
var errWatchAgentTooOld = errors.New("This device's agent is too old for watches; update it with `wendy device update`.")

// campaignWatchBackend runs each watch as a leased, notify-only Wendy Data
// campaign (design §6.3).
type campaignWatchBackend struct {
	detector    watchDetector
	renewEvery  time.Duration
	statusEvery time.Duration
	pollEvery   time.Duration
	callTimeout time.Duration // each device call's deadline

	mu      sync.Mutex
	watches map[string]*campaignWatch // by campaign name
	poller  *campaignPoller           // while any watch runs

	// startMu serializes Start, so two concurrent starts never both create a
	// poller. starting counts Starts in flight (guarded by mu); detachIfIdle
	// leaves the poller running while one is, since that Start has not yet
	// registered its watch.
	startMu  sync.Mutex
	starting int
}

func newCampaignWatchBackend(detector watchDetector) *campaignWatchBackend {
	return &campaignWatchBackend{detector: detector, renewEvery: watchRenewEvery, statusEvery: watchStatusEvery, pollEvery: watchPollEvery, callTimeout: watchCallTimeout, watches: map[string]*campaignWatch{}}
}

func (b *campaignWatchBackend) Start(ctx context.Context, conn *grpcclient.AgentConnection, spec watchSpec) (watchHandle, error) {
	if conn == nil || conn.DataService == nil {
		return nil, errWatchAgentTooOld
	}
	plan, err := b.campaignYAML(spec)
	if err != nil {
		return nil, err
	}
	client := conn.DataService
	b.startMu.Lock()
	defer b.startMu.Unlock()
	b.mu.Lock()
	b.starting++
	b.mu.Unlock()
	finishStart := func() {
		b.mu.Lock()
		b.starting--
		b.mu.Unlock()
	}
	if err := b.attach(ctx, client); err != nil {
		finishStart()
		b.detachIfIdle()
		return nil, err
	}
	// startMu is held: a deploy without a deadline could block every start.
	deployCtx, cancelDeploy := context.WithTimeout(ctx, b.callTimeout)
	_, err = client.CampaignDeploy(deployCtx, &agentpbv2.DataCampaignDeployRequest{CampaignYaml: plan})
	cancelDeploy()
	if err != nil {
		finishStart()
		b.detachIfIdle()
		return nil, watchDeviceError(err)
	}
	watchCtx, cancel := context.WithCancel(context.Background())
	w := &campaignWatch{backend: b, client: client, name: spec.Name, ctx: watchCtx, cancel: cancel, updates: make(chan watchUpdate, 32), done: make(chan struct{})}
	b.mu.Lock()
	b.watches[spec.Name] = w
	b.starting--
	b.mu.Unlock()
	go w.run()
	return w, nil
}

// campaignYAML builds the leased campaign of design §5.1 and validates it
// locally, so an invalid plan never reaches the device.
func (b *campaignWatchBackend) campaignYAML(spec watchSpec) ([]byte, error) {
	campaign := watchCampaignWire{
		Version: data.CampaignVersion, Name: spec.Name, Lease: watchLease,
		Sources: []watchSourceWire{{Camera: spec.CameraID}},
		Inference: watchInferenceWire{
			Model: b.detector.Model, Revision: b.detector.Revision, Labels: spec.Classes,
			Threshold: spec.MinConfidence, Rate: watchInferenceRate, Event: spec.Name + ".detected",
			ClearAfter: "5s", Cooldown: "30s",
		},
		Notify: watchNotifyWire{On: data.NotifyOnDetection},
	}
	plan, err := yaml.Marshal(campaign)
	if err != nil {
		return nil, err
	}
	if _, err := data.ParseCampaign(plan); err != nil {
		return nil, fmt.Errorf("building the watch campaign: %w", err)
	}
	return plan, nil
}

// The wire structs carry exactly the keys design §5.1 gives a watch's
// campaign, with the agent parser's YAML names. They are not data.Campaign so
// that a field later added to its capture, upload or export sections is never
// sent to agents that predate it.
type watchCampaignWire struct {
	Version   int                `yaml:"version"`
	Name      string             `yaml:"name"`
	Lease     string             `yaml:"lease"`
	Sources   []watchSourceWire  `yaml:"sources"`
	Inference watchInferenceWire `yaml:"inference"`
	Notify    watchNotifyWire    `yaml:"notify"`
}

type watchSourceWire struct {
	Camera string `yaml:"camera"`
}

type watchInferenceWire struct {
	Model      string   `yaml:"model"`
	Revision   string   `yaml:"revision"`
	Labels     []string `yaml:"labels"`
	Threshold  float64  `yaml:"threshold"`
	Rate       float64  `yaml:"rate"`
	Event      string   `yaml:"event"`
	ClearAfter string   `yaml:"clear_after"`
	Cooldown   string   `yaml:"cooldown"`
}

type watchNotifyWire struct {
	On string `yaml:"on"`
}

// watchDeviceError maps a device refusal. An agent that predates leased
// campaigns parses plans with unknown fields disallowed, so it names lease.
func watchDeviceError(err error) error {
	st, ok := status.FromError(err)
	if ok && (st.Code() == codes.Unimplemented || st.Code() == codes.InvalidArgument && strings.Contains(st.Message(), "field lease not found")) {
		return errWatchAgentTooOld
	}
	return err
}

func (b *campaignWatchBackend) forget(w *campaignWatch) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.watches[w.name] == w {
		delete(b.watches, w.name)
	}
}

// campaignPoller reads one device's notification journal for every campaign
// watch and routes entries by campaign (design §6.3).
type campaignPoller struct {
	client agentpbv2.DataServiceClient
	cursor string
	cancel context.CancelFunc
	done   chan struct{}
}

// attach starts the poller before a watch's campaign is deployed. Its first
// read, with an empty cursor, returns the journal's tail, so a detection that
// fires right after the deploy is not missed. A poller for another client
// belongs to a connection the manager has already ended; it is replaced.
func (b *campaignWatchBackend) attach(ctx context.Context, client agentpbv2.DataServiceClient) error {
	b.mu.Lock()
	if b.poller != nil && b.poller.client == client {
		b.mu.Unlock()
		return nil
	}
	old := b.poller
	b.poller = nil
	b.mu.Unlock()
	if old != nil {
		old.cancel()
		<-old.done
	}
	callCtx, cancel := context.WithTimeout(ctx, b.callTimeout)
	defer cancel()
	response, err := client.Events(callCtx, &agentpbv2.DataEventsRequest{NotificationsOnly: true})
	if err != nil {
		return watchDeviceError(err)
	}
	// An older agent ignores notifications_only and answers from its ordinary
	// event journal; it does not mark the response.
	if !response.GetNotifications() {
		return errWatchAgentTooOld
	}
	pollCtx, stop := context.WithCancel(context.Background())
	p := &campaignPoller{client: client, cursor: response.GetCursor(), cancel: stop, done: make(chan struct{})}
	b.mu.Lock()
	b.poller = p
	b.mu.Unlock()
	go b.poll(pollCtx, p)
	return nil
}

// detachIfIdle stops the poller once no watch is left.
func (b *campaignWatchBackend) detachIfIdle() {
	b.mu.Lock()
	p := b.poller
	if len(b.watches) > 0 || b.starting > 0 || p == nil {
		b.mu.Unlock()
		return
	}
	b.poller = nil
	b.mu.Unlock()
	p.cancel()
	<-p.done
}

func (b *campaignWatchBackend) poll(ctx context.Context, p *campaignPoller) {
	defer close(p.done)
	ticker := time.NewTicker(b.pollEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		callCtx, cancel := context.WithTimeout(ctx, b.callTimeout)
		response, err := p.client.Events(callCtx, &agentpbv2.DataEventsRequest{Cursor: p.cursor, Replay: true, NotificationsOnly: true})
		cancel()
		if err != nil || !response.GetNotifications() {
			continue // transient; the next read resumes from the same cursor
		}
		var entries []data.CampaignNotification
		if json.Unmarshal(response.GetEventsJson(), &entries) != nil {
			continue
		}
		p.cursor = response.GetCursor()
		b.route(entries, response.GetGap())
	}
}

// route turns journal entries into events for the watches they belong to.
// Entries for other campaigns are ignored.
func (b *campaignWatchBackend) route(entries []data.CampaignNotification, gap bool) {
	b.mu.Lock()
	watches := maps.Clone(b.watches)
	b.mu.Unlock()
	if gap {
		for _, w := range watches {
			w.send(watchUpdate{Gap: "some detections may have been missed"})
		}
	}
	for _, entry := range entries {
		w := watches[entry.Campaign]
		if w == nil {
			continue
		}
		occurred, err := time.Parse(time.RFC3339Nano, entry.OccurredAt)
		if err != nil {
			occurred = time.Now()
		}
		classes := make([]watchClass, 0, len(entry.Detections))
		for _, detection := range entry.Detections {
			classes = append(classes, watchClass{Label: detection.Label, Score: detection.Score})
		}
		w.send(watchUpdate{Event: &watchEventUpdate{Kind: "entered", Classes: classes, OccurredAt: occurred}})
	}
}

// campaignWatch renews one campaign's lease and follows its inference state.
type campaignWatch struct {
	backend *campaignWatchBackend
	client  agentpbv2.DataServiceClient
	name    string
	ctx     context.Context
	cancel  context.CancelFunc
	done    chan struct{}

	sendMu  sync.Mutex
	closed  bool
	updates chan watchUpdate

	lastState  watchState
	lastReason string
}

func (w *campaignWatch) Updates() <-chan watchUpdate { return w.updates }

// send delivers u unless the watch has stopped. The manager drains updates
// until they close, so a send blocks only briefly.
func (w *campaignWatch) send(u watchUpdate) bool {
	w.sendMu.Lock()
	defer w.sendMu.Unlock()
	if w.closed {
		return false
	}
	select {
	case w.updates <- u:
		return true
	case <-w.ctx.Done():
		return false
	}
}

func (w *campaignWatch) closeUpdates() {
	w.sendMu.Lock()
	defer w.sendMu.Unlock()
	if !w.closed {
		w.closed = true
		close(w.updates)
	}
}

func (w *campaignWatch) run() {
	defer close(w.done)
	defer w.closeUpdates()
	defer w.backend.detachIfIdle()
	defer w.backend.forget(w)
	renew := time.NewTicker(w.backend.renewEvery)
	defer renew.Stop()
	inspect := time.NewTicker(w.backend.statusEvery)
	defer inspect.Stop()
	if !w.refreshStatus() {
		return
	}
	for {
		select {
		case <-w.ctx.Done():
			return
		case <-renew.C:
			if !w.renew() {
				return
			}
		case <-inspect.C:
			if !w.refreshStatus() {
				return
			}
		}
	}
}

// renew pushes the lease forward. A transient failure is retried on the next
// tick. NOT_FOUND means the device restarted or the lease lapsed.
func (w *campaignWatch) renew() bool {
	ctx, cancel := context.WithTimeout(w.ctx, w.backend.callTimeout)
	defer cancel()
	_, err := w.client.CampaignRenew(ctx, &agentpbv2.DataCampaignRenewRequest{Name: w.name})
	switch status.Code(err) {
	case codes.OK:
		return true
	case codes.NotFound:
		w.end(watchGoneReason)
		return false
	case codes.Unimplemented:
		w.end(errWatchAgentTooOld.Error())
		return false
	}
	return w.ctx.Err() == nil
}

// refreshStatus reports a change in the campaign's inference state.
//
// After a failure the agent loads the detector again every 5 s, so it reports
// pending or loading between errors. That retry belongs to ERROR (design
// §6.3): an ERROR watch stays ERROR, with its last reason, until the detector
// runs or fails for a different reason.
func (w *campaignWatch) refreshStatus() bool {
	ctx, cancel := context.WithTimeout(w.ctx, w.backend.callTimeout)
	defer cancel()
	campaign, err := w.client.CampaignInspect(ctx, &agentpbv2.DataCampaignInspectRequest{Name: w.name})
	if status.Code(err) == codes.NotFound {
		w.end(watchGoneReason)
		return false
	}
	if err != nil {
		return w.ctx.Err() == nil
	}
	var plan data.Campaign
	if json.Unmarshal(campaign.GetPlanJson(), &plan) != nil || plan.InferenceStatus == nil {
		return true
	}
	state, reason := watchStateFromInference(*plan.InferenceStatus)
	if w.lastState == watchError && state == watchPreparing {
		return true
	}
	if state != w.lastState || reason != w.lastReason {
		w.lastState, w.lastReason = state, reason
		return w.send(watchUpdate{Status: &watchStatusUpdate{State: state, Reason: reason}})
	}
	return true
}

func (w *campaignWatch) end(reason string) {
	w.send(watchUpdate{Status: &watchStatusUpdate{State: watchEnded, Reason: reason}})
}

// watchStateFromInference maps the agent's inference state (design §6.3).
func watchStateFromInference(s data.InferenceStatus) (watchState, string) {
	switch s.State {
	case "running":
		return watchReady, ""
	case "error":
		return watchError, s.Error
	case "waiting_for_cameras":
		if s.Error != "" {
			return watchError, "the camera is unavailable: " + s.Error
		}
		return watchError, "the camera is unavailable"
	case "loading":
		return watchPreparing, "loading the detector; a first watch on a device also installs it, which takes a few minutes"
	}
	return watchPreparing, ""
}

// Stop ends renewals, then removes the campaign. NOT_FOUND means it is
// already gone, which is the goal.
func (w *campaignWatch) Stop(ctx context.Context) error {
	w.cancel()
	<-w.done
	_, err := w.client.CampaignRemove(ctx, &agentpbv2.DataCampaignRemoveRequest{Name: w.name})
	switch status.Code(err) {
	case codes.OK, codes.NotFound:
		return nil
	case codes.Unimplemented:
		return errWatchAgentTooOld
	}
	return err
}
