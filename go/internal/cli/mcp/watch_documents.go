package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
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
	// A 60 s lease renewed every 20 s, and once as a watch starts, survives
	// two missed renewals.
	watchLease         = "60s"
	watchRenewEvery    = 20 * time.Second
	watchStatusEvery   = 5 * time.Second
	watchPollEvery     = time.Second
	watchInferenceRate = 2
	watchCallTimeout   = 10 * time.Second
	// The agent applies documents under the lock a recording campaign holds
	// while it starts a capture, which can take over 20 s.
	watchDeployTimeout = 30 * time.Second
	watchGoneReason    = "the device restarted or the watch expired"
	// The gateway reader's bounds on one journal response (spec §11.1).
	maxWatchJournalBytes   = 4 << 20
	maxWatchJournalEntries = 512
	watchGapReason         = "some detections may have been missed"
)

// errWatchAgentTooOld is returned for agents without data documents. They
// answer DocumentApply with UNIMPLEMENTED, so a refused watch leaves nothing
// on the device.
var errWatchAgentTooOld = errors.New("This device's agent is too old for watches; update it with `wendy device update`.")

// errWatchDeviceBusy is an apply the device did not take within
// watchDeployTimeout. The agent may still apply it once it is free; the lease
// then removes the watch, which nothing renews.
var errWatchDeviceBusy = errors.New("The device did not accept the watch within 30 s; it is busy, for example starting a recording. Try again in a minute. If the device accepts this watch late, it removes itself within a minute.")

// documentWatchBackend runs each watch as a leased watch document and an
// inference document the watch owns (data documents spec §11.1). Removing the
// watch, or its lease lapsing, removes both.
type documentWatchBackend struct {
	renewEvery    time.Duration
	statusEvery   time.Duration
	pollEvery     time.Duration
	callTimeout   time.Duration // each device call's deadline
	deployTimeout time.Duration // each apply's deadline

	mu      sync.Mutex
	watches map[string]*documentWatch // by watch name
	pending map[string]bool           // watches being started; the poller names them too
	poller  *documentPoller           // while any watch runs or starts

	// startMu serializes Start, so two concurrent starts never both create a
	// poller.
	startMu sync.Mutex
}

func newDocumentWatchBackend() *documentWatchBackend {
	return &documentWatchBackend{renewEvery: watchRenewEvery, statusEvery: watchStatusEvery, pollEvery: watchPollEvery, callTimeout: watchCallTimeout,
		deployTimeout: watchDeployTimeout, watches: map[string]*documentWatch{}, pending: map[string]bool{}}
}

func (b *documentWatchBackend) Start(ctx context.Context, conn *grpcclient.AgentConnection, spec watchSpec) (watchHandle, error) {
	if conn == nil || conn.DataService == nil {
		return nil, errWatchAgentTooOld
	}
	watchDoc, inferenceDoc, err := watchDocuments(spec)
	if err != nil {
		return nil, err
	}
	client := conn.DataService
	b.startMu.Lock()
	defer b.startMu.Unlock()
	b.mu.Lock()
	b.pending[spec.Name] = true
	b.mu.Unlock()
	fail := func(err error) (watchHandle, error) {
		b.mu.Lock()
		delete(b.pending, spec.Name)
		b.mu.Unlock()
		b.detachIfIdle()
		return nil, err
	}
	if err := b.attach(ctx, client); err != nil {
		return fail(err)
	}
	if err := b.apply(ctx, client, watchDoc); err != nil {
		return fail(err)
	}
	if err := b.apply(ctx, client, inferenceDoc); err != nil {
		// Removing the watch removes an inference the device applies late. If
		// the device cannot be reached, the watch's lease removes both.
		removeCtx, cancel := context.WithTimeout(context.Background(), b.callTimeout)
		_, _ = client.DocumentRemove(removeCtx, &agentpbv2.DataDocumentRemoveRequest{Kind: data.KindWatch, Name: spec.Name})
		cancel()
		return fail(err)
	}
	watchCtx, cancel := context.WithCancel(context.Background())
	w := &documentWatch{backend: b, client: client, name: spec.Name, ctx: watchCtx, cancel: cancel, updates: make(chan watchUpdate, 32), done: make(chan struct{})}
	b.mu.Lock()
	b.watches[spec.Name] = w
	delete(b.pending, spec.Name)
	b.mu.Unlock()
	go w.run()
	return w, nil
}

// apply sends one document. A call that ended without the device's answer
// may still land, which watchUnconfirmedError says.
func (b *documentWatchBackend) apply(ctx context.Context, client agentpbv2.DataServiceClient, document []byte) error {
	applyCtx, cancel := context.WithTimeout(ctx, b.deployTimeout)
	_, err := client.DocumentApply(applyCtx, &agentpbv2.DataDocumentApplyRequest{DocumentYaml: document})
	cancel()
	if err == nil {
		return nil
	}
	switch status.Code(err) {
	case codes.DeadlineExceeded, codes.Canceled, codes.Unavailable:
		if status.Code(err) == codes.DeadlineExceeded && ctx.Err() == nil {
			err = errWatchDeviceBusy
		}
		return watchUnconfirmedError{err}
	}
	return watchDeviceError(err)
}

// watchDocuments builds the watch and the inference it owns (spec §11.1) and
// validates both locally, so an invalid document never reaches the device.
func watchDocuments(spec watchSpec) (watch, inference []byte, err error) {
	watch, err = yaml.Marshal(watchDocumentWire{Kind: data.KindWatch, Version: data.WatchVersion, Name: spec.Name, Lease: watchLease, On: watchTargetWire{Inference: spec.Name}})
	if err != nil {
		return nil, nil, err
	}
	inference, err = yaml.Marshal(inferenceDocumentWire{
		Kind: data.KindInference, Version: data.InferenceDocumentVersion, Name: spec.Name, Owner: data.KindWatch + "/" + spec.Name,
		Sources: []watchSourceWire{{Camera: spec.CameraID}}, Model: spec.Detector.Model, Revision: spec.Detector.Revision,
		Labels: spec.Classes, Threshold: spec.MinConfidence, Rate: watchInferenceRate, ClearAfter: "5s", Cooldown: "30s",
	})
	if err != nil {
		return nil, nil, err
	}
	for _, document := range [][]byte{watch, inference} {
		if _, err := data.ParseDocument(document); err != nil {
			return nil, nil, fmt.Errorf("building the watch documents: %w", err)
		}
	}
	return watch, inference, nil
}

// The wire structs carry exactly the keys spec §11.1 gives a chat watch's
// documents, with the agent parser's YAML names. They are not data.Watch and
// data.InferenceDocument, so a field later added there is never sent to agents
// that predate it.
type watchDocumentWire struct {
	Kind    string          `yaml:"kind"`
	Version int             `yaml:"version"`
	Name    string          `yaml:"name"`
	Lease   string          `yaml:"lease"`
	On      watchTargetWire `yaml:"on"`
}

type watchTargetWire struct {
	Inference string `yaml:"inference"`
}

type inferenceDocumentWire struct {
	Kind       string            `yaml:"kind"`
	Version    int               `yaml:"version"`
	Name       string            `yaml:"name"`
	Owner      string            `yaml:"owner"`
	Sources    []watchSourceWire `yaml:"sources"`
	Model      string            `yaml:"model"`
	Revision   string            `yaml:"revision"`
	Labels     []string          `yaml:"labels"`
	Threshold  float64           `yaml:"threshold"`
	Rate       float64           `yaml:"rate"`
	ClearAfter string            `yaml:"clear_after"`
	Cooldown   string            `yaml:"cooldown"`
}

type watchSourceWire struct {
	Camera string `yaml:"camera"`
}

// watchDeviceError maps a device refusal. An agent without data documents
// answers UNIMPLEMENTED.
func watchDeviceError(err error) error {
	if status.Code(err) == codes.Unimplemented {
		return errWatchAgentTooOld
	}
	return err
}

func (b *documentWatchBackend) forget(w *documentWatch) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.watches[w.name] == w {
		delete(b.watches, w.name)
	}
}

// names lists the watches the poller asks for: running and starting ones.
func (b *documentWatchBackend) names() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	names := slices.Collect(maps.Keys(b.watches))
	for name := range b.pending {
		names = append(names, name)
	}
	slices.Sort(names)
	return slices.Compact(names)
}

// documentPoller reads this server's watches' entries from one device's
// notification journal (spec §6.5, §11.1).
type documentPoller struct {
	client agentpbv2.DataServiceClient
	cursor string
	cancel context.CancelFunc
	done   chan struct{}
}

// attach starts the poller before a watch is applied. Its first read, with an
// empty cursor, returns the journal's tail, so a detection right after the
// apply is not missed. A poller for another client belongs to a connection the
// manager has already ended; it is replaced.
func (b *documentWatchBackend) attach(ctx context.Context, client agentpbv2.DataServiceClient) error {
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
	p := &documentPoller{client: client, cursor: response.GetCursor(), cancel: stop, done: make(chan struct{})}
	b.mu.Lock()
	b.poller = p
	b.mu.Unlock()
	go b.poll(pollCtx, p)
	return nil
}

// detachIfIdle stops the poller once no watch runs or starts.
func (b *documentWatchBackend) detachIfIdle() {
	b.mu.Lock()
	p := b.poller
	if len(b.watches) > 0 || len(b.pending) > 0 || p == nil {
		b.mu.Unlock()
		return
	}
	b.poller = nil
	b.mu.Unlock()
	p.cancel()
	<-p.done
}

func (b *documentWatchBackend) poll(ctx context.Context, p *documentPoller) {
	defer close(p.done)
	ticker := time.NewTicker(b.pollEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		names := b.names()
		if len(names) == 0 {
			continue // never read without names: that would be every unleased entry
		}
		callCtx, cancel := context.WithTimeout(ctx, b.callTimeout)
		response, err := p.client.Events(callCtx, &agentpbv2.DataEventsRequest{Cursor: p.cursor, Replay: true, NotificationsOnly: true, Watches: names})
		cancel()
		if err != nil || !response.GetNotifications() {
			continue // transient; the next read resumes from the same cursor
		}
		raw := response.GetEventsJson()
		var entries []data.CampaignNotification
		gap := response.GetGap()
		if len(raw) > maxWatchJournalBytes || json.Unmarshal(raw, &entries) != nil {
			entries, gap = nil, true
		}
		if len(entries) > maxWatchJournalEntries {
			entries, gap = entries[len(entries)-maxWatchJournalEntries:], true
		}
		p.cursor = response.GetCursor()
		b.route(entries, gap)
	}
}

// route turns journal entries into events for the watches they belong to.
func (b *documentWatchBackend) route(entries []data.CampaignNotification, gap bool) {
	b.mu.Lock()
	watches := maps.Clone(b.watches)
	b.mu.Unlock()
	if gap {
		for _, w := range watches {
			w.send(watchUpdate{Gap: watchGapReason})
		}
	}
	for _, entry := range entries {
		w := watches[entry.Watch]
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

// documentWatch renews one watch's lease and follows its state.
type documentWatch struct {
	backend *documentWatchBackend
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

func (w *documentWatch) Updates() <-chan watchUpdate { return w.updates }

// send delivers u unless the watch has stopped. The manager drains updates
// until they close, so a send blocks only briefly.
func (w *documentWatch) send(u watchUpdate) bool {
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

func (w *documentWatch) closeUpdates() {
	w.sendMu.Lock()
	defer w.sendMu.Unlock()
	if !w.closed {
		w.closed = true
		close(w.updates)
	}
}

func (w *documentWatch) run() {
	defer close(w.done)
	defer w.closeUpdates()
	defer w.backend.detachIfIdle()
	defer w.backend.forget(w)
	renew := time.NewTicker(w.backend.renewEvery)
	defer renew.Stop()
	inspect := time.NewTicker(w.backend.statusEvery)
	defer inspect.Stop()
	// The lease started when the agent applied the watch, possibly long
	// before now; renew at once rather than a full interval later.
	if !w.renew() || !w.refreshStatus() {
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
func (w *documentWatch) renew() bool {
	ctx, cancel := context.WithTimeout(w.ctx, w.backend.callTimeout)
	defer cancel()
	_, err := w.client.DocumentRenew(ctx, &agentpbv2.DataDocumentRenewRequest{Kind: data.KindWatch, Name: w.name})
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

// refreshStatus reports a change in the watch's state.
//
// After a failure the agent loads the detector again every 5 s, so it reports
// loading between errors. That retry belongs to ERROR: an ERROR watch stays
// ERROR, with its last reason, until the detector runs or fails differently.
func (w *documentWatch) refreshStatus() bool {
	ctx, cancel := context.WithTimeout(w.ctx, w.backend.callTimeout)
	defer cancel()
	response, err := w.client.Documents(ctx, &agentpbv2.DataDocumentsRequest{Kind: data.KindWatch, Name: w.name})
	if status.Code(err) == codes.NotFound {
		w.end(watchGoneReason)
		return false
	}
	if err != nil || len(response.GetDocuments()) != 1 {
		return w.ctx.Err() == nil
	}
	state, reason := watchStateFromDocument(response.GetDocuments()[0])
	if w.lastState == watchError && state == watchPreparing {
		return true
	}
	if state != w.lastState || reason != w.lastReason {
		w.lastState, w.lastReason = state, reason
		return w.send(watchUpdate{Status: &watchStatusUpdate{State: state, Reason: reason}})
	}
	return true
}

func (w *documentWatch) end(reason string) {
	w.send(watchUpdate{Status: &watchStatusUpdate{State: watchEnded, Reason: reason}})
}

// watchStateFromDocument maps the device's watch state and its inference's
// status (spec §6.6) to a watch state.
func watchStateFromDocument(document *agentpbv2.DataDocument) (watchState, string) {
	switch document.GetState() {
	case "active":
		return watchReady, ""
	case "error":
		return watchError, document.GetStateDetail()
	}
	var inference struct {
		State string `json:"inference_state"`
		Error string `json:"inference_error"`
	}
	_ = json.Unmarshal(document.GetStatusJson(), &inference)
	if inference.State == "" || inference.State == "disabled" {
		// No inference state explains the wait (its document may have been
		// removed), so the device's own detail does.
		return watchPreparing, document.GetStateDetail()
	}
	return watchStateFromInference(data.InferenceStatus{State: inference.State, Error: inference.Error})
}

// watchStateFromInference maps the agent's inference state.
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

// Stop ends renewals, then removes the watch, which removes its inference.
// NOT_FOUND means it is already gone, which is the goal.
func (w *documentWatch) Stop(ctx context.Context) error {
	w.cancel()
	<-w.done
	_, err := w.client.DocumentRemove(ctx, &agentpbv2.DataDocumentRemoveRequest{Kind: data.KindWatch, Name: w.name})
	switch status.Code(err) {
	case codes.OK, codes.NotFound:
		return nil
	case codes.Unimplemented:
		return errWatchAgentTooOld
	case codes.DeadlineExceeded, codes.Unavailable, codes.Canceled:
		// The call ended without the device's confirmation. The agent waits up
		// to 10 s for the inference to stop, longer than the client's bound,
		// and still removes the watch; the lease removes it if not.
		return watchUnconfirmedError{err}
	}
	return err
}
