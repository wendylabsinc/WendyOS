package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	"github.com/wendylabsinc/wendy/go/internal/agent/worldview"
)

// worldViewModel is the model name on every record the world view emits. It
// must equal the name data.Campaign.Match reads object confidence triggers
// from.
const worldViewModel = "worldview"

const (
	worldViewMaxProposals    = 50
	worldViewMinAreaFraction = 0.002
	// classEvidenceMaxAge and classEvidencePerSource bound the cache of
	// application detections kept per camera for the class attribute.
	classEvidenceMaxAge    = 2 * time.Second
	classEvidencePerSource = 256
	// depthProbeTimeout is how long a job waits for the first depth frame
	// before it runs without depth.
	depthProbeTimeout = 10 * time.Second
	// depthForwardInterval throttles depth frames to the worker. The worker
	// keeps only the latest depth frame per source and pairs it with an RGB
	// frame at most 100 milliseconds away, so 20 frames per second is enough.
	depthForwardInterval = 50 * time.Millisecond
	worldViewExpireEvery = time.Second
	worldViewBackoffMin  = 5 * time.Second
	worldViewBackoffMax  = time.Minute
)

// worldViewSupported is replaced in tests that exercise an unsupported host.
var worldViewSupported = worldview.Supported

type campaignWorldViewManager struct {
	service *DataService
	factory worldview.Factory
	sender  CampaignNotificationSender
	// record writes one campaign record; nil uses
	// data.Manager.RecordCampaignApplication. Tests observe records through it.
	record func(appID string, record data.ApplicationRecord) (string, error)
	wake   chan struct{}
	mu     sync.Mutex
	jobs   map[string]*campaignWorldViewJob

	evidenceMu sync.Mutex
	evidence   map[string][]worldview.ClassEvidence
}

type campaignWorldViewJob struct {
	owner       *campaignWorldViewManager
	campaign    data.Campaign
	cancel      context.CancelFunc
	done        chan struct{}
	queue       chan DetectionNotification
	mu          sync.Mutex
	status      data.ObjectsStatus
	generations map[string]uint64
	// depthID is the source identifier depth.source last resolved to, or
	// empty. It is excluded from the colour cameras the search streams.
	depthID        string
	nextGeneration atomic.Uint64
	// handled counts worker results the job has processed, for tests.
	handled atomic.Uint64

	// The fields below are touched only by the goroutine running the results
	// loop, and survive worker restarts within one revision so presence and
	// cooldown are not reset by a crashed worker.
	objects  []worldViewObject
	sources  map[string]*worldViewSourceState
	trackers map[string]*worldview.Tracker
}

// worldViewObject is one enabled object with its scoring prepared once.
type worldViewObject struct {
	name       string
	desc       *data.ObjectDescriptor
	attributes []scoredAttribute
	spec       worldview.FusionSpec
}

// worldViewSourceState is what the job remembers about one camera between
// worker results.
type worldViewSourceState struct {
	seen        bool
	lastTime    int64
	lastSample  uint64
	achievedFPS float64
	depthPaired bool
	gates       map[string]*objectGate
	meters      map[string]*worldview.RateMeter
}

type objectGate struct {
	scored bool
	time   int64
	sample uint64
}

// StartCampaignWorldView restores persisted world view plans and supervises one
// perception worker per armed campaign with at least one enabled object, until
// shutdown. The returned stop function waits for workers and camera
// subscriptions to exit before the video service is shut down.
func (s *DataService) StartCampaignWorldView(ctx context.Context, factory worldview.Factory, sender CampaignNotificationSender) func() {
	return s.startCampaignWorldView(ctx, &campaignWorldViewManager{factory: factory, sender: sender})
}

func (s *DataService) startCampaignWorldView(ctx context.Context, manager *campaignWorldViewManager) func() {
	ctx, cancel := context.WithCancel(ctx)
	manager.service = s
	manager.wake = make(chan struct{}, 1)
	manager.jobs = map[string]*campaignWorldViewJob{}
	manager.evidence = map[string][]worldview.ClassEvidence{}
	if manager.record == nil {
		manager.record = s.manager.RecordCampaignApplication
	}
	s.worldView = manager // Configured once, before registering the RPC server.
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer manager.stopAll()
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			manager.reconcile(ctx)
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			case <-manager.wake:
			}
		}
	}()
	return func() { cancel(); <-done }
}

// hasEnabledObject reports whether the campaign asks the world view to search
// for anything.
func hasEnabledObject(campaign data.Campaign) bool {
	for _, object := range campaign.Objects {
		if object.IsEnabled() {
			return true
		}
	}
	return false
}

func hasCameraSource(campaign data.Campaign) bool {
	for _, source := range campaign.Sources {
		if source.Camera != "" {
			return true
		}
	}
	return false
}

func (m *campaignWorldViewManager) stopAll() {
	m.mu.Lock()
	jobs := m.jobs
	m.jobs = map[string]*campaignWorldViewJob{}
	m.mu.Unlock()
	for _, job := range jobs {
		job.cancel()
	}
	for _, job := range jobs {
		<-job.done
	}
}

func (m *campaignWorldViewManager) reconcile(ctx context.Context) {
	campaigns, err := m.service.manager.Campaigns()
	if err != nil {
		m.service.manager.Warnf("reading world view campaigns: %v", err)
		return
	}
	wanted := map[string]data.Campaign{}
	for _, campaign := range campaigns {
		if campaign.State == "armed" && hasEnabledObject(campaign) && hasCameraSource(campaign) {
			wanted[campaign.Name] = campaign
		}
	}
	m.mu.Lock()
	var retired []*campaignWorldViewJob
	for name, job := range m.jobs {
		campaign, ok := wanted[name]
		if ok && campaign.Revision == job.campaign.Revision {
			continue
		}
		job.cancel()
		retired = append(retired, job)
		delete(m.jobs, name)
	}
	m.mu.Unlock()
	for _, job := range retired {
		<-job.done
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for name, campaign := range wanted {
		if m.jobs[name] != nil || ctx.Err() != nil {
			continue
		}
		child, cancel := context.WithCancel(ctx)
		job := newWorldViewJob(m, campaign, cancel)
		m.jobs[name] = job
		go job.supervise(child)
	}
}

func newWorldViewJob(owner *campaignWorldViewManager, campaign data.Campaign, cancel context.CancelFunc) *campaignWorldViewJob {
	job := &campaignWorldViewJob{
		owner: owner, campaign: campaign, cancel: cancel, done: make(chan struct{}),
		queue: make(chan DetectionNotification, 16), generations: map[string]uint64{},
		status:  data.ObjectsStatus{State: "loading", Sources: map[string]string{}},
		sources: map[string]*worldViewSourceState{}, trackers: map[string]*worldview.Tracker{},
	}
	names := make([]string, 0, len(campaign.Objects))
	for name, desc := range campaign.Objects {
		if desc.IsEnabled() {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	for _, name := range names {
		desc := campaign.Objects[name]
		attributes, spec := objectScoring(desc)
		job.objects = append(job.objects, worldViewObject{name: name, desc: desc, attributes: attributes, spec: spec})
		job.trackers[name] = worldview.NewTracker(desc.ClearDuration(), desc.CooldownDuration())
	}
	return job
}

func (m *campaignWorldViewManager) snapshot(name, revision string) *data.ObjectsStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	job := m.jobs[name]
	if job == nil || job.campaign.Revision != revision {
		return &data.ObjectsStatus{State: "stopped"}
	}
	job.mu.Lock()
	defer job.mu.Unlock()
	snapshot := job.status
	snapshot.Sources = map[string]string{}
	for source, state := range job.status.Sources {
		snapshot.Sources[source] = state
	}
	return &snapshot
}

// objectsStatus is the live world view state campaign inspect reports as
// objects_status, the way inference_status reports the inference job.
func (s *DataService) objectsStatus(campaign data.Campaign) *data.ObjectsStatus {
	if campaign.Objects == nil {
		return nil
	}
	if !hasEnabledObject(campaign) {
		return &data.ObjectsStatus{State: "disabled"}
	}
	if s.worldView == nil {
		return &data.ObjectsStatus{State: "error", Error: "agent world view runtime is unavailable"}
	}
	status := s.worldView.snapshot(campaign.Name, campaign.Revision)
	if status.State == "stopped" {
		status.State = "pending"
	}
	return status
}

func (j *campaignWorldViewJob) setState(state string, err error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.status.State = state
	j.status.Error = ""
	if err != nil {
		j.status.Error = err.Error()
	}
}

func (j *campaignWorldViewJob) sourceState(source, state string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.status.Sources[source] = state
}

func (j *campaignWorldViewJob) notificationError(err error) {
	j.mu.Lock()
	j.status.NotificationError = ""
	if err != nil {
		j.status.NotificationError = err.Error()
	}
	j.mu.Unlock()
	if err != nil {
		j.owner.service.manager.Warnf("campaign %q world view notification: %v", j.campaign.Name, err)
	}
}

func (j *campaignWorldViewJob) supervise(ctx context.Context) {
	defer close(j.done)
	ctx, cancel := context.WithCancel(ctx)
	notifyDone := make(chan struct{})
	go func() {
		defer close(notifyDone)
		deliverCampaignNotifications(ctx, j.owner.sender, j.campaign, j.queue, j.notificationError)
	}()
	defer func() { cancel(); <-notifyDone }()
	backoff := worldViewBackoffMin
	for ctx.Err() == nil {
		j.setState("loading", nil)
		started := time.Now()
		err := j.run(ctx)
		if ctx.Err() != nil {
			return
		}
		if time.Since(started) > worldViewBackoffMax {
			backoff = worldViewBackoffMin
		}
		j.setState("error", err)
		j.owner.service.manager.Warnf("campaign %q world view: %v; retrying in %s", j.campaign.Name, err, backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(2*backoff, worldViewBackoffMax)
	}
}

// worldViewConfig derives the one worker configuration a campaign's enabled
// objects share. The worker samples at the fastest cadence any object asks
// for; each object is then gated to its own cadence in the agent.
func worldViewConfig(objects []worldViewObject) worldview.Config {
	config := worldview.Config{Proposer: worldview.ProposerContours, MaxProposals: worldViewMaxProposals, MinAreaFraction: worldViewMinAreaFraction}
	maxRate, minEvery := 0.0, 0
	for _, object := range objects {
		if object.desc.Rate > 0 {
			maxRate = math.Max(maxRate, object.desc.Rate)
		}
		if object.desc.EveryFrames > 0 && (minEvery == 0 || object.desc.EveryFrames < minEvery) {
			minEvery = object.desc.EveryFrames
		}
	}
	switch {
	case maxRate > 0 && minEvery > 0:
		// Mixed forms: a camera at 30 frames per second sampled every N frames
		// needs 30 / N results per second.
		rate := math.Min(30, math.Max(maxRate, 30/float64(minEvery)))
		config.Rate = &rate
	case maxRate > 0:
		config.Rate = &maxRate
	default:
		config.EveryFrames = &minEvery
	}
	return config
}

type worldViewSource struct {
	cancel context.CancelFunc
	done   chan struct{}
}

func (j *campaignWorldViewJob) run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if j.owner.factory == nil {
		return errors.New("world view runtime is unavailable")
	}
	if _, managed := j.owner.factory.(*worldview.ManagedFactory); managed && !worldViewSupported() {
		return errors.New("world view is unsupported on this platform: the agent has no managed runtime for this operating system and architecture")
	}
	if j.owner.service.video == nil {
		return errors.New("camera service is unavailable")
	}
	config := worldViewConfig(j.objects)
	depthSubscription, depthFirst := j.openDepthForConfig(ctx, &config)
	session, err := j.owner.factory.Start(ctx, config)
	if err != nil {
		if depthSubscription != nil {
			depthSubscription.Close()
		}
		return err
	}
	sources := map[string]worldViewSource{}
	defer func() {
		cancel()
		// Close the process first: a blocked write to its stdin must be released
		// before waiting for a camera's subscription goroutine.
		_ = session.Close()
		for _, source := range sources {
			source.cancel()
			<-source.done
		}
	}()
	if depthSubscription != nil {
		child, stop := context.WithCancel(ctx)
		done := make(chan struct{})
		sources[j.campaign.Depth.Source] = worldViewSource{stop, done}
		go func() { defer close(done); j.streamDepth(child, session, depthSubscription, depthFirst) }()
	}
	reconcile := func() {
		ids, _, _, resolveErr := j.owner.service.manager.ResolveCampaignSources(j.campaign)
		selected := map[string]bool{}
		if resolveErr == nil {
			for _, id := range j.colourCameras(ids) {
				selected[id] = true
			}
		}
		cameras := 0
		for id, source := range sources {
			if j.campaign.Depth != nil && id == j.campaign.Depth.Source {
				continue
			}
			if selected[id] {
				cameras++
				continue
			}
			source.cancel()
			<-source.done
			j.mu.Lock()
			delete(j.generations, id)
			delete(j.status.Sources, id)
			j.mu.Unlock()
			delete(sources, id)
			// Keep trackers and gates across temporary camera loss.
		}
		for _, source := range j.owner.service.manager.Sources(ctx) {
			if source.Kind == "camera" && !source.Healthy {
				j.sourceState(source.ID, "unavailable: "+source.Detail)
			}
		}
		changed := false
		for id := range selected {
			if _, ok := sources[id]; ok {
				continue
			}
			child, stop := context.WithCancel(ctx)
			done := make(chan struct{})
			sources[id] = worldViewSource{stop, done}
			cameras++
			changed = true
			go func(id string) { defer close(done); j.stream(child, session, id) }(id)
		}
		if resolveErr != nil {
			j.setState("waiting_for_cameras", resolveErr)
		} else if cameras == 0 {
			j.setState("waiting_for_cameras", nil)
		} else {
			j.setState("running", nil)
		}
		// The inference job re-arms pre-roll when its cameras change. Re-arming
		// discards the standby ring, so only one job does it: this one, when the
		// campaign runs no inference.
		if changed && !j.campaign.Inference.IsEnabled() {
			if _, active := j.owner.service.manager.ActiveSession(j.campaign.Name); !active {
				j.owner.service.armCampaign(j.campaign)
			}
		}
	}
	reconcile()
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	expire := time.NewTicker(worldViewExpireEvery)
	defer expire.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			reconcile()
		case now := <-expire.C:
			j.expire(ctx, now)
		case result, ok := <-session.Results():
			if !ok {
				return errors.New("world view process exited")
			}
			if result.Type == "error" {
				return errors.New(result.Error)
			}
			j.mu.Lock()
			generation, active := j.generations[result.SourceID]
			j.mu.Unlock()
			if !active || generation != result.Generation || ctx.Err() != nil {
				j.handled.Add(1)
				continue
			}
			switch result.Type {
			case "source_error":
				state := result.Error
				if j.campaign.Depth != nil && result.SourceID == j.campaign.Depth.Source {
					state = "depth unavailable: " + result.Error
				}
				j.sourceState(result.SourceID, state)
			case "proposals":
				j.sourceState(result.SourceID, "scoring")
				j.handleProposals(ctx, result, time.Now())
			default:
				return fmt.Errorf("unexpected world view result type %q", result.Type)
			}
			j.handled.Add(1)
		}
	}
}

// resolveDepth resolves depth.source through the camera selector a campaign's
// `camera:` sources use, so an exact identifier, a /dev/videoN path or a
// unique name fragment all name the depth node. A successful resolution is
// recorded; a failed one keeps the last node, so a depth camera that drops off
// the bus is not taken for a colour camera in the meantime.
func (j *campaignWorldViewJob) resolveDepth() (string, error) {
	id, err := j.owner.service.manager.ResolveCameraSelector(j.campaign.Depth.Source)
	if err != nil {
		return "", err
	}
	j.mu.Lock()
	j.depthID = id
	j.mu.Unlock()
	return id, nil
}

// colourCameras keeps the camera identifiers among resolved source ids, less
// the resolved depth node, which serves depth frames rather than a picture.
func (j *campaignWorldViewJob) colourCameras(ids []string) []string {
	j.mu.Lock()
	depthID := j.depthID
	j.mu.Unlock()
	var cameras []string
	for _, id := range ids {
		if _, ok := cameraDeviceID(id); ok && id != depthID {
			cameras = append(cameras, id)
		}
	}
	return cameras
}

// openDepthForConfig tries the campaign's depth source and, when it delivers
// z16 raw frames and exactly one colour camera is resolved to pair it with,
// fills the worker configuration's depth and pairs. Any failure is recorded in
// the status and the job runs without depth, which leaves the size attribute
// unavailable rather than zero.
func (j *campaignWorldViewJob) openDepthForConfig(ctx context.Context, config *worldview.Config) (sensorSubscription, SensorSample) {
	depth := j.campaign.Depth
	if depth == nil {
		return nil, SensorSample{}
	}
	if _, err := j.resolveDepth(); err != nil {
		j.sourceState(depth.Source, "depth unavailable: "+err.Error())
		return nil, SensorSample{}
	}
	ids, _, _, err := j.owner.service.manager.ResolveCampaignSources(j.campaign)
	var cameras []string
	if err == nil {
		cameras = j.colourCameras(ids)
	}
	// One intrinsics block describes one camera, so depth pairs only with a
	// campaign that resolves to a single colour camera.
	if len(cameras) != 1 {
		j.sourceState(depth.Source, fmt.Sprintf("depth unavailable: depth pairs with exactly one colour camera, and %d resolved", len(cameras)))
		return nil, SensorSample{}
	}
	subscription, first, err := j.openDepth(ctx)
	if err != nil {
		j.sourceState(depth.Source, "depth unavailable: "+err.Error())
		return nil, SensorSample{}
	}
	intrinsics := worldview.Intrinsics{Fx: depth.Intrinsics.Fx, Fy: depth.Intrinsics.Fy, Cx: depth.Intrinsics.Cx, Cy: depth.Intrinsics.Cy}
	config.Depth = &worldview.DepthConfig{ScaleM: depth.ScaleM, Intrinsics: intrinsics}
	config.Pairs = map[string]string{cameras[0]: depth.Source}
	j.sourceState(depth.Source, "depth streaming")
	return subscription, first
}

// openDepth resolves the depth source, subscribes to it and waits for its
// first frame, which must be a z16 raw frame with known dimensions. It
// resolves again on every call, so a reconnect follows the selector to the
// node it names now.
func (j *campaignWorldViewJob) openDepth(ctx context.Context) (sensorSubscription, SensorSample, error) {
	id, err := j.resolveDepth()
	if err != nil {
		return nil, SensorSample{}, err
	}
	subscription, err := j.owner.service.video.SubscribeSensor(ctx, id)
	if err != nil {
		return nil, SensorSample{}, err
	}
	timeout, cancel := context.WithTimeout(ctx, depthProbeTimeout)
	sample, err := subscription.Next(timeout)
	cancel()
	if err != nil {
		subscription.Close()
		return nil, SensorSample{}, fmt.Errorf("no depth frame: %w", err)
	}
	if sample.Encoding != "z16" || sample.Width <= 0 || sample.Height <= 0 {
		subscription.Close()
		return nil, SensorSample{}, fmt.Errorf("samples are %q, not z16 raw frames with known dimensions", sample.Encoding)
	}
	return subscription, sample, nil
}

func (j *campaignWorldViewJob) streamDepth(ctx context.Context, session worldview.Session, subscription sensorSubscription, sample SensorSample) {
	source := j.campaign.Depth.Source
	for ctx.Err() == nil {
		generation := j.nextGeneration.Add(1)
		j.mu.Lock()
		j.generations[source] = generation
		j.mu.Unlock()
		j.sourceState(source, "depth streaming")
		var err error
		var last time.Time
		for ctx.Err() == nil {
			if sample.Encoding != "z16" || sample.Width <= 0 || sample.Height <= 0 {
				err = fmt.Errorf("samples are %q, not z16 raw frames with known dimensions", sample.Encoding)
				break
			}
			if now := time.Now(); now.Sub(last) >= depthForwardInterval {
				last = now
				if len(sample.Payload) > 8<<20 {
					err = errors.New("depth sample exceeds world view limit of 8MiB")
					break
				}
				if err = session.Send(worldview.Input{Kind: worldview.KindDepth, SourceID: source, Generation: generation, SampleID: sample.SampleID, BootNanos: sample.BootNanos,
					Encoding: "z16", Width: sample.Width, Height: sample.Height, Payload: sample.Payload}); err != nil {
					break
				}
			}
			timeout, cancel := context.WithTimeout(ctx, 30*time.Second)
			sample, err = subscription.Next(timeout)
			cancel()
			if err != nil {
				break
			}
		}
		subscription.Close()
		j.mu.Lock()
		delete(j.generations, source)
		j.mu.Unlock()
		_ = session.Send(worldview.Input{Kind: worldview.KindDepth, SourceID: source, Generation: generation, End: true})
		if ctx.Err() != nil {
			return
		}
		// Depth is optional: losing it leaves size unavailable, never fails the job.
		j.sourceState(source, fmt.Sprintf("depth unavailable: %v; reconnecting", err))
		for ctx.Err() == nil {
			select {
			case <-ctx.Done():
				return
			case <-time.After(2 * time.Second):
			}
			subscription, sample, err = j.openDepth(ctx)
			if err == nil {
				break
			}
			j.sourceState(source, fmt.Sprintf("depth unavailable: %v; reconnecting", err))
		}
	}
}

func (j *campaignWorldViewJob) stream(ctx context.Context, session worldview.Session, sourceID string) {
	appID := campaignAppPrefix + j.campaign.Name
	for ctx.Err() == nil {
		generation := j.nextGeneration.Add(1)
		j.mu.Lock()
		j.generations[sourceID] = generation
		j.mu.Unlock()
		j.sourceState(sourceID, "connecting")
		subscription, err := j.owner.service.video.SubscribeSensor(ctx, sourceID)
		if err == nil {
			j.sourceState(sourceID, "streaming")
			for ctx.Err() == nil {
				timeout, cancel := context.WithTimeout(ctx, 30*time.Second)
				sample, nextErr := subscription.Next(timeout)
				cancel()
				if nextErr != nil {
					err = nextErr
					break
				}
				if len(sample.Payload) > 8<<20 {
					err = errors.New("camera sample exceeds world view limit of 8MiB")
					break
				}
				err = session.Send(worldview.Input{Kind: worldview.KindRGB, SourceID: sourceID, Generation: generation, SampleID: sample.SampleID, BootNanos: sample.BootNanos,
					Encoding: sample.Encoding, Payload: sample.Payload, Initialization: sample.DecoderInit, DroppedBefore: sample.DroppedBefore})
				if err != nil {
					break
				}
				if err := j.owner.service.manager.RecordModelInput(data.ModelInput{AppID: appID, Model: worldViewModel, SourceID: sourceID, SampleID: sample.SampleID, BootNanos: sample.BootNanos, UncertaintyNanos: sample.UncertaintyNanos, PayloadBytes: len(sample.Payload), Encoding: sample.Encoding, SelfContained: sample.SelfContained, DroppedBefore: sample.DroppedBefore}); err != nil {
					j.owner.service.manager.Warnf("recording campaign world view input: %v", err)
				}
			}
			subscription.Close()
			j.mu.Lock()
			delete(j.generations, sourceID)
			j.mu.Unlock()
			// An end marker tears down the decoder and its queued frames before reuse.
			_ = session.Send(worldview.Input{Kind: worldview.KindRGB, SourceID: sourceID, Generation: generation, End: true})
		}
		if ctx.Err() != nil {
			return
		}
		j.sourceState(sourceID, fmt.Sprintf("reconnecting: %v", err))
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
	}
}

// handleProposals scores one worker result for every object whose cadence
// asks for it, tracks the matches and emits the lifecycle records.
func (j *campaignWorldViewJob) handleProposals(ctx context.Context, result worldview.Result, now time.Time) {
	state := j.sources[result.SourceID]
	if state == nil {
		state = &worldViewSourceState{gates: map[string]*objectGate{}, meters: map[string]*worldview.RateMeter{}}
		j.sources[result.SourceID] = state
	}
	at := result.BootNanos
	if at <= 0 {
		at = now.UnixNano()
	}
	// The step since the previous result for this camera sets the gating
	// tolerance, so an object whose cadence equals the worker's is not halved
	// by jitter.
	var stepTime int64
	var stepSamples uint64
	if state.seen && at >= state.lastTime && result.SampleID >= state.lastSample {
		stepTime, stepSamples = at-state.lastTime, result.SampleID-state.lastSample
	}
	state.seen, state.lastTime, state.lastSample = true, at, result.SampleID
	state.achievedFPS, state.depthPaired = result.AchievedFPS, result.DepthPaired

	proposals := make([]worldview.Proposal, 0, len(result.Proposals))
	for _, wire := range result.Proposals {
		proposals = append(proposals, proposalFromWire(result, wire))
	}
	evidence := j.classEvidence(result.SourceID)
	for _, object := range j.objects {
		gate := state.gates[object.name]
		if gate == nil {
			gate = &objectGate{}
			state.gates[object.name] = gate
		}
		if !gate.admit(object.desc, at, result.SampleID, stepTime, stepSamples) {
			continue
		}
		meter := state.meters[object.name]
		if meter == nil {
			meter = &worldview.RateMeter{}
			state.meters[object.name] = meter
		}
		meter.Tick(now)
		var matches []worldview.Match
		for _, proposal := range proposals {
			fused := fuseProposal(object, proposal, evidence)
			if fused.Matched {
				matches = append(matches, worldview.Match{Box: proposal.Box, Fused: fused, Proposal: proposal})
			}
		}
		for _, event := range j.trackers[object.name].Observe(result.SourceID, object.name, now, matches) {
			j.emit(ctx, object, event, now)
		}
	}
}

func (j *campaignWorldViewJob) expire(ctx context.Context, now time.Time) {
	for _, object := range j.objects {
		for _, event := range j.trackers[object.name].Expire(now) {
			j.emit(ctx, object, event, now)
		}
	}
}

// admit reports whether an object scores this result, and records it when it
// does. A rate object scores when at least one of its intervals has passed
// since it last scored, and an every_frames object when at least that many
// frames have; half a worker step is forgiven so jitter cannot halve the
// cadence. A clock or sample counter that goes backwards resets the gate.
func (g *objectGate) admit(desc *data.ObjectDescriptor, at int64, sample uint64, stepTime int64, stepSamples uint64) bool {
	admit := !g.scored
	switch {
	case admit:
	case desc.Rate > 0:
		interval := float64(time.Second) / desc.Rate
		admit = at < g.time || float64(at-g.time) >= interval-float64(stepTime)/2
	case desc.EveryFrames > 0:
		admit = sample < g.sample || float64(sample-g.sample) >= float64(desc.EveryFrames)-float64(stepSamples)/2
	default:
		admit = true
	}
	if admit {
		g.scored, g.time, g.sample = true, at, sample
	}
	return admit
}

func proposalFromWire(result worldview.Result, wire worldview.ProposalWire) worldview.Proposal {
	proposal := worldview.Proposal{
		SourceID: result.SourceID, SampleID: result.SampleID, BootNanos: result.BootNanos,
		Box: wire.Box, FrameW: result.FrameW, FrameH: result.FrameH,
		Silhouette: worldview.Silhouette{Primitive: wire.Silhouette.Primitive, Aspect: wire.Silhouette.Aspect},
	}
	for _, entry := range wire.Palette {
		proposal.Palette = append(proposal.Palette, worldview.PaletteEntry{Lab: entry.Lab, Share: entry.Share})
	}
	if wire.Metric != nil {
		proposal.Metric = &worldview.Metric{WidthM: wire.Metric.WidthM, HeightM: wire.Metric.HeightM, DistanceM: wire.Metric.DistanceM, BearingDeg: wire.Metric.BearingDeg}
	}
	return proposal
}

// classEvidence returns the cached application detections for one camera the
// class attribute may use. Records from the campaign's own inference model are
// excluded: that model's Hugging Face boxes are corners, [x0, y0, x1, y1], not
// the [x, y, w, h] application convention this version accepts.
func (j *campaignWorldViewJob) classEvidence(sourceID string) []worldview.ClassEvidence {
	all := j.owner.evidenceFor(sourceID)
	if j.campaign.Inference == nil {
		return all
	}
	kept := all[:0]
	for _, evidence := range all {
		if evidence.Model != j.campaign.Inference.Model {
			kept = append(kept, evidence)
		}
	}
	return kept
}

func fuseProposal(object worldViewObject, proposal worldview.Proposal, evidence []worldview.ClassEvidence) worldview.Fused {
	scored := make([]worldview.Evidence, 0, len(object.attributes))
	for _, attribute := range object.attributes {
		scored = append(scored, attribute.score(proposal, evidence))
	}
	return worldview.Fuse(scored, object.spec)
}

// emit records one lifecycle event as a prediction record, and for appeared
// also the object's event record, then evaluates the campaign's triggers and
// notification against them.
func (j *campaignWorldViewJob) emit(ctx context.Context, object worldViewObject, event worldview.Lifecycle, now time.Time) {
	match := event.Match
	if event.Kind == worldview.KindLost {
		match = event.Last
	}
	if match == nil {
		return
	}
	proposal := match.Proposal
	state := j.sources[event.SourceID]
	fused := match.Fused
	attributes := map[string]any{
		"campaign":      j.campaign.Name,
		"object":        object.name,
		"track_id":      event.TrackID,
		"kind":          event.Kind,
		"confidence":    fused.Confidence,
		"scores":        fused.Scores,
		"unavailable":   nonNilStrings(fused.Unavailable),
		"unweighted":    nonNilStrings(fused.Unweighted),
		"bbox":          []float64{proposal.Box[0], proposal.Box[1], proposal.Box[2], proposal.Box[3]},
		"frame":         map[string]any{"w": proposal.FrameW, "h": proposal.FrameH},
		"source_id":     proposal.SourceID,
		"sample_id":     proposal.SampleID,
		"boot_nanos":    proposal.BootNanos,
		"model_version": j.campaign.Revision,
	}
	if fused.Vetoed != "" {
		attributes["vetoed"] = fused.Vetoed
	}
	if metric := proposal.Metric; metric != nil {
		attributes["position"] = map[string]any{"distance_m": metric.DistanceM, "bearing_deg": metric.BearingDeg}
		attributes["size_m"] = map[string]any{"w": metric.WidthM, "h": metric.HeightM}
	}
	if object.desc.Rate > 0 {
		attributes["requested_rate"] = object.desc.Rate
	} else {
		attributes["requested_every_frames"] = object.desc.EveryFrames
	}
	if state != nil {
		attributes["achieved_fps"] = state.achievedFPS
		attributes["depth_paired"] = state.depthPaired
		if meter := state.meters[object.name]; meter != nil {
			attributes["object_achieved_fps"] = meter.Achieved(now)
		}
	}
	// SampleID and BootNanos are the worker's: those of the latest input sample
	// its decoder had read when the frame was produced, which can run slightly
	// ahead of the frame actually scored. They are carried as given.
	record := data.ApplicationRecord{Version: 1, Type: "prediction", Model: worldViewModel,
		Inputs: []data.SampleRef{{SourceID: proposal.SourceID, SampleID: proposal.SampleID}}, Attributes: attributes}
	// A lost record reports the object leaving; it never starts an episode.
	j.recordAndTrigger(ctx, record, event.Kind != worldview.KindLost)
	if event.Kind != worldview.KindAppeared {
		return
	}
	eventRecord := data.ApplicationRecord{Version: 1, Type: "event", Name: object.desc.Event, Model: worldViewModel,
		Inputs: record.Inputs, Attributes: map[string]any{"object": object.name, "track_id": event.TrackID, "confidence": fused.Confidence, "source_id": proposal.SourceID}}
	j.recordAndTrigger(ctx, eventRecord, true)
	if notify := j.campaign.Notify; notify != nil && notify.On == data.NotifyOnEvent && notify.Event == object.desc.Event {
		request := DetectionNotification{ID: uuid.NewString(), Event: object.desc.Event, Campaign: j.campaign.Name, SourceID: proposal.SourceID, Model: worldViewModel, Revision: j.campaign.Revision, Count: 1}
		select {
		case j.queue <- request:
		default:
			j.notificationError(errors.New("notification queue full; world view notification dropped"))
		}
	}
}

func nonNilStrings(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}

// recordAndTrigger records a world view record and, when trigger is set,
// evaluates the campaign's triggers against it synchronously: campaign
// records never reach the global application observer. Like triggerInference
// it re-reads the campaign first, so a retired revision records and triggers
// nothing.
func (j *campaignWorldViewJob) recordAndTrigger(ctx context.Context, record data.ApplicationRecord, trigger bool) {
	s := j.owner.service
	s.deploymentMu.Lock()
	defer s.deploymentMu.Unlock()
	current, err := s.manager.Campaign(j.campaign.Name)
	if err != nil || current.Revision != j.campaign.Revision || ctx.Err() != nil {
		return
	}
	if _, err := j.owner.record(campaignAppPrefix+j.campaign.Name, record); err != nil {
		s.manager.Warnf("recording campaign world view record: %v", err)
		return
	}
	if !trigger {
		return
	}
	reason, expression, matched := j.campaign.Match(record)
	if !matched {
		return
	}
	if _, active := s.manager.ActiveSession(j.campaign.Name); active {
		return
	}
	if _, err := s.triggerCampaign(ctx, j.campaign, reason, expression); err != nil {
		s.manager.Warnf("campaign %q matched %s but starting its episode failed: %v", j.campaign.Name, reason, err)
	}
}

// observePrediction caches an application's detections as class evidence.
// Only the application convention is accepted in this version: each entry of
// attributes.detections carries a numeric four element box [x, y, w, h] in
// frame pixels, a label in class_name or label, and a confidence in
// confidence or score. Hugging Face corner boxes [x0, y0, x1, y1] are not
// accepted. The camera is attributes.source_id or the record's first input,
// and the time is the record's client boot time.
func (m *campaignWorldViewManager) observePrediction(_ string, record data.ApplicationRecord) {
	if record.Type != "prediction" || record.Model == worldViewModel || record.ClientBootNanos <= 0 {
		return
	}
	source, _ := record.Attributes["source_id"].(string)
	if source == "" && len(record.Inputs) > 0 {
		source = record.Inputs[0].SourceID
	}
	if source == "" {
		return
	}
	var entries []map[string]any
	switch detections := record.Attributes["detections"].(type) {
	case []any:
		for _, raw := range detections {
			if entry, ok := raw.(map[string]any); ok {
				entries = append(entries, entry)
			}
		}
	case []map[string]any:
		entries = detections
	}
	var parsed []worldview.ClassEvidence
	for _, entry := range entries {
		box, ok := numberBox(entry["box"])
		if !ok {
			continue
		}
		label, _ := entry["class_name"].(string)
		if label == "" {
			label, _ = entry["label"].(string)
		}
		confidence, ok := jsonNumber(entry["confidence"])
		if !ok {
			confidence, ok = jsonNumber(entry["score"])
		}
		if label == "" || !ok {
			continue
		}
		parsed = append(parsed, worldview.ClassEvidence{SourceID: source, Label: label, Model: record.Model, Confidence: confidence, Box: box, BootNanos: record.ClientBootNanos})
	}
	if len(parsed) == 0 {
		return
	}
	m.evidenceMu.Lock()
	defer m.evidenceMu.Unlock()
	cached := append(m.evidence[source], parsed...)
	newest := int64(math.MinInt64)
	for _, evidence := range cached {
		newest = max(newest, evidence.BootNanos)
	}
	kept := cached[:0]
	for _, evidence := range cached {
		if newest-evidence.BootNanos <= classEvidenceMaxAge.Nanoseconds() {
			kept = append(kept, evidence)
		}
	}
	if len(kept) > classEvidencePerSource {
		kept = append([]worldview.ClassEvidence(nil), kept[len(kept)-classEvidencePerSource:]...)
	}
	m.evidence[source] = kept
}

func (m *campaignWorldViewManager) evidenceFor(sourceID string) []worldview.ClassEvidence {
	m.evidenceMu.Lock()
	defer m.evidenceMu.Unlock()
	return append([]worldview.ClassEvidence(nil), m.evidence[sourceID]...)
}

func numberBox(value any) ([4]float64, bool) {
	var box [4]float64
	list, ok := value.([]any)
	if !ok {
		if floats, isFloats := value.([]float64); isFloats && len(floats) == 4 {
			copy(box[:], floats)
			return box, true
		}
		return box, false
	}
	if len(list) != 4 {
		return box, false
	}
	for i, item := range list {
		if box[i], ok = jsonNumber(item); !ok {
			return box, false
		}
	}
	return box, true
}

// jsonNumber reads a finite number decoded from JSON or YAML.
func jsonNumber(value any) (float64, bool) {
	var number float64
	switch typed := value.(type) {
	case float64:
		number = typed
	case float32:
		number = float64(typed)
	case int:
		number = float64(typed)
	case int64:
		number = float64(typed)
	case uint64:
		number = float64(typed)
	case json.Number:
		parsed, err := typed.Float64()
		if err != nil {
			return 0, false
		}
		number = parsed
	default:
		return 0, false
	}
	return number, !math.IsNaN(number) && !math.IsInf(number, 0)
}

// scoredAttribute scores one declared attribute of an object.
type scoredAttribute struct {
	kind  string
	score func(worldview.Proposal, []worldview.ClassEvidence) worldview.Evidence
}

// objectScoring adapts a campaign object descriptor to the world view scorers
// and fusion spec. The descriptor was validated at deploy, so malformed expect
// values cannot reach here; a value that still fails to read leaves that
// constraint open rather than failing the job.
func objectScoring(desc *data.ObjectDescriptor) (attributes []scoredAttribute, spec worldview.FusionSpec) {
	spec = worldview.FusionSpec{Threshold: desc.Fusion.Threshold, Required: append([]string(nil), desc.Fusion.Required...), Weights: map[string]float64{}, Min: map[string]float64{}}
	kinds := make([]string, 0, len(desc.Attributes))
	for kind, attribute := range desc.Attributes {
		kinds = append(kinds, kind)
		spec.Weights[kind] = attribute.Weight
	}
	for _, kind := range desc.Fusion.Required {
		spec.Min[kind] = desc.Attributes[kind].VetoFloor()
	}
	composed := len(desc.Composition) > 0
	if composed {
		if _, declared := desc.Attributes[worldview.AttributeShape]; !declared {
			// The composition still scores as shape; without a declared shape
			// weight it is reported in unweighted and does not move confidence.
			kinds = append(kinds, worldview.AttributeShape)
		}
	}
	sort.Strings(kinds)
	for _, kind := range kinds {
		expect := map[string]any{}
		if attribute := desc.Attributes[kind]; attribute != nil {
			expect = attribute.Expect
		}
		switch kind {
		case worldview.AttributeShape:
			if composed {
				parts := make([]worldview.Part, 0, len(desc.Composition))
				for _, part := range desc.Composition {
					parts = append(parts, worldview.Part{Primitive: part.Primitive, WidthM: part.WidthM, HeightM: part.HeightM})
				}
				attributes = append(attributes, scoredAttribute{kind, func(p worldview.Proposal, _ []worldview.ClassEvidence) worldview.Evidence {
					return scoreComposition(p, parts)
				}})
				continue
			}
			shape := worldview.ShapeExpect{}
			shape.Primitive, _ = expect["primitive"].(string)
			if aspect, ok := numberRange(expect["aspect"]); ok {
				shape.Aspect = &aspect
			}
			attributes = append(attributes, scoredAttribute{kind, func(p worldview.Proposal, _ []worldview.ClassEvidence) worldview.Evidence {
				return worldview.ScoreShape(p, shape)
			}})
		case worldview.AttributeSize:
			size := worldview.SizeExpect{}
			size.WidthM, _ = numberRange(expect["w_m"])
			size.HeightM, _ = numberRange(expect["h_m"])
			attributes = append(attributes, scoredAttribute{kind, func(p worldview.Proposal, _ []worldview.ClassEvidence) worldview.Evidence {
				return worldview.ScoreSize(p, size)
			}})
		case worldview.AttributeColour:
			colour := worldview.ColourExpect{Tolerance: desc.Attributes[kind].ColourTolerance()}
			entries, _ := expect["palette"].([]any)
			for _, raw := range entries {
				entry, ok := raw.(map[string]any)
				if !ok {
					continue
				}
				lab, okLab := numberTriple(entry["lab"])
				share, okShare := jsonNumber(entry["share"])
				if okLab && okShare {
					colour.Palette = append(colour.Palette, worldview.PaletteEntry{Lab: lab, Share: share})
				}
			}
			attributes = append(attributes, scoredAttribute{kind, func(p worldview.Proposal, _ []worldview.ClassEvidence) worldview.Evidence {
				return worldview.ScoreColour(p, colour)
			}})
		case worldview.AttributeClass:
			class := worldview.ClassExpect{}
			class.Label, _ = expect["label"].(string)
			class.Model, _ = expect["model"].(string)
			attributes = append(attributes, scoredAttribute{kind, func(p worldview.Proposal, evidence []worldview.ClassEvidence) worldview.Evidence {
				return worldview.ScoreClass(p, class, evidence, worldview.DefaultAssociationWindow)
			}})
		}
	}
	return attributes, spec
}

// scoreComposition is the shape evidence of an object built from parts.
// Version 1 treats one proposal as one observed part: the worker segments an
// object as a single region, so only the first part its silhouette can be
// matches. Without a paired depth measurement the parts' metric ranges are
// left open, so the primitives alone decide which part matches and a missing
// measurement never scores as a zero size; the matched part still counts only
// its share of the expected height, as MatchComposition weighs it.
func scoreComposition(p worldview.Proposal, parts []worldview.Part) worldview.Evidence {
	evidence := worldview.Evidence{Attribute: worldview.AttributeShape, Detail: map[string]any{"primitive": p.Silhouette.Primitive, "composition": true}}
	if p.Silhouette.Primitive == "" {
		evidence.Detail["reason"] = "no silhouette primitive"
		return evidence
	}
	observed := worldview.ObservedPart{Primitive: p.Silhouette.Primitive}
	var score float64
	var visible []int
	if p.Metric != nil {
		observed.WidthM, observed.HeightM = p.Metric.WidthM, p.Metric.HeightM
		score, visible = worldview.MatchComposition([]worldview.ObservedPart{observed}, parts)
	} else {
		open := make([]worldview.Part, len(parts))
		for i, part := range parts {
			open[i] = worldview.Part{Primitive: part.Primitive}
		}
		_, visible = worldview.MatchComposition([]worldview.ObservedPart{observed}, open)
		shares := heightShares(parts)
		for _, index := range visible {
			score += shares[index]
		}
		evidence.Detail["reason"] = "no depth paired; primitives only"
	}
	evidence.Available, evidence.Score = true, score
	evidence.Detail["visible_parts"] = visible
	return evidence
}

// heightShares is each part's share of the total expected height by range
// midpoint, equal shares when no height is given; the weighting
// worldview.MatchComposition applies.
func heightShares(parts []worldview.Part) []float64 {
	shares := make([]float64, len(parts))
	total := 0.0
	for i, part := range parts {
		shares[i] = math.Max(0, (part.HeightM[0]+part.HeightM[1])/2)
		total += shares[i]
	}
	for i := range shares {
		if total > 0 {
			shares[i] /= total
		} else {
			shares[i] = 1 / float64(len(parts))
		}
	}
	return shares
}

func numberRange(value any) ([2]float64, bool) {
	var out [2]float64
	list, ok := value.([]any)
	if !ok || len(list) != 2 {
		return out, false
	}
	for i, item := range list {
		if out[i], ok = jsonNumber(item); !ok {
			return [2]float64{}, false
		}
	}
	return out, true
}

func numberTriple(value any) ([3]float64, bool) {
	var out [3]float64
	list, ok := value.([]any)
	if !ok || len(list) != 3 {
		return out, false
	}
	for i, item := range list {
		if out[i], ok = jsonNumber(item); !ok {
			return [3]float64{}, false
		}
	}
	return out, true
}
