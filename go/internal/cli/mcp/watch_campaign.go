package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

	mu      sync.Mutex
	watches map[string]*campaignWatch // by campaign name
}

func newCampaignWatchBackend(detector watchDetector) *campaignWatchBackend {
	return &campaignWatchBackend{detector: detector, renewEvery: watchRenewEvery, statusEvery: watchStatusEvery, pollEvery: watchPollEvery, watches: map[string]*campaignWatch{}}
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
	if _, err := client.CampaignDeploy(ctx, &agentpbv2.DataCampaignDeployRequest{CampaignYaml: plan}); err != nil {
		return nil, watchDeviceError(err)
	}
	watchCtx, cancel := context.WithCancel(context.Background())
	w := &campaignWatch{backend: b, client: client, name: spec.Name, ctx: watchCtx, cancel: cancel, updates: make(chan watchUpdate, 32), done: make(chan struct{})}
	b.mu.Lock()
	b.watches[spec.Name] = w
	b.mu.Unlock()
	go w.run()
	return w, nil
}

// campaignYAML builds the leased campaign of design §5.1 and validates it
// locally, so an invalid plan never reaches the device.
func (b *campaignWatchBackend) campaignYAML(spec watchSpec) ([]byte, error) {
	campaign := data.Campaign{
		Version: data.CampaignVersion, Name: spec.Name, Lease: watchLease,
		Sources: []data.CampaignSource{{Camera: spec.CameraID}},
		Inference: &data.CampaignInference{
			Model: b.detector.Model, Revision: b.detector.Revision, Labels: spec.Classes,
			Threshold: spec.MinConfidence, Rate: watchInferenceRate, Event: spec.Name + ".detected",
			ClearAfter: "5s", Cooldown: "30s",
		},
		Notify: &data.CampaignNotify{On: data.NotifyOnDetection},
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
	ctx, cancel := context.WithTimeout(w.ctx, watchCallTimeout)
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
func (w *campaignWatch) refreshStatus() bool {
	ctx, cancel := context.WithTimeout(w.ctx, watchCallTimeout)
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
		return watchPreparing, "loading the detector; the first watch on a device installs it first, which takes a few minutes"
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
