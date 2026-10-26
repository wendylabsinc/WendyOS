package services

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	"github.com/wendylabsinc/wendy/go/internal/agent/inference"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

type inferenceVideo interface {
	SubscribeSensor(context.Context, string) (sensorSubscription, error)
}

const campaignAppPrefix = "sh.wendy.campaign."

const inferenceAppPrefix = "sh.wendy.inference."

// Job keys keep a campaign's inline inference and an inference document
// apart even when they share a name.
func campaignJobKey(name string) string  { return "campaign/" + name }
func inferenceJobKey(name string) string { return "inference/" + name }

// wantedInference is one job a reconcile pass wants running.
type wantedInference struct {
	campaign data.Campaign
	document bool
	// ownerRef is a document's owner reference, or empty.
	ownerRef string
	// private marks an inference a leased watch owns; see ownedByLeasedWatch.
	private bool
}

type campaignInferenceManager struct {
	service *DataService
	factory inference.Factory
	sender  CampaignNotificationSender
	wake    chan struct{}
	mu      sync.Mutex
	jobs    map[string]*campaignInferenceJob
	ctx     context.Context
	// reconcileMu runs one pass at a time: a pass that read the plans before
	// a removal must not restart a job that a later pass, run for that
	// removal, has just retired.
	reconcileMu sync.Mutex
}

type campaignInferenceJob struct {
	owner *campaignInferenceManager
	// key is the job's key in jobs; document marks an inference document's job:
	// no campaign capture, no campaign notifications.
	key      string
	document bool
	ownerRef string
	// private keeps the job's events off the shared device-event journal.
	private bool
	// audience is a document job's current record audience, replaced (never
	// mutated) under mu by run and read by the stream goroutines.
	audience       map[string]bool
	campaign       data.Campaign
	cancel         context.CancelFunc
	done           chan struct{}
	queue          chan DetectionNotification
	mu             sync.Mutex
	status         data.InferenceStatus
	generations    map[string]uint64
	nextGeneration atomic.Uint64
}

// StartCampaignInference restores persisted inference plans and supervises their
// workers until shutdown. The returned stop function waits for processes and
// camera subscriptions to exit before the video service is shut down.
func (s *DataService) StartCampaignInference(ctx context.Context, factory inference.Factory, sender CampaignNotificationSender) func() {
	ctx, cancel := context.WithCancel(ctx)
	manager := &campaignInferenceManager{service: s, factory: factory, sender: sender, wake: make(chan struct{}, 1), ctx: ctx, jobs: map[string]*campaignInferenceJob{}}
	s.inference = manager // Configured once, before registering the RPC server and before the expiry loop reads it.
	// Leased documents of a previous run go before the first inference job
	// can open a camera for them.
	watchesDone := s.startWatches(ctx, sender)
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
	return func() { cancel(); <-done; <-watchesDone }
}

func (m *campaignInferenceManager) stopAll() {
	m.mu.Lock()
	jobs := make([]*campaignInferenceJob, 0, len(m.jobs))
	for _, job := range m.jobs {
		jobs = append(jobs, job)
	}
	m.mu.Unlock()
	for _, job := range jobs {
		job.cancel()
	}
	for _, job := range jobs {
		<-job.done
		m.mu.Lock()
		if m.jobs[job.key] == job {
			delete(m.jobs, job.key)
		}
		m.mu.Unlock()
	}
}

func (m *campaignInferenceManager) reconcile(ctx context.Context) {
	m.reconcileMu.Lock()
	defer m.reconcileMu.Unlock()
	campaigns, err := m.service.manager.Campaigns()
	if err != nil {
		m.service.manager.Warnf("reading inference campaigns: %v", err)
		return
	}
	wanted := map[string]wantedInference{}
	for _, campaign := range campaigns {
		if campaign.State == "armed" && (campaign.Inference.IsEnabled() || campaign.Notify != nil && campaign.Notify.On == data.NotifyOnEvent) {
			wanted[campaignJobKey(campaign.Name)] = wantedInference{campaign: campaign}
		}
	}
	documents, err := m.service.manager.Documents(data.KindInference)
	documentsKnown := err == nil
	if err != nil {
		m.service.manager.Warnf("reading inference documents: %v", err)
	}
	for _, stored := range documents {
		document, err := stored.Inference()
		if err != nil {
			m.service.manager.Warnf("inference document %q: %v", stored.Name, err)
			continue
		}
		if document.IsEnabled() {
			wanted[inferenceJobKey(stored.Name)] = wantedInference{campaign: document.Campaign(stored.Revision), document: true, ownerRef: document.Owner, private: m.ownedByLeasedWatch(document.Owner)}
		}
	}
	m.mu.Lock()
	var retired []*campaignInferenceJob
	for key, job := range m.jobs {
		want, ok := wanted[key]
		if ok && want.campaign.Revision == job.campaign.Revision {
			continue
		}
		// An unreadable document directory is not a removal.
		if !ok && job.document && !documentsKnown {
			continue
		}
		job.cancel()
		retired = append(retired, job)
	}
	m.mu.Unlock()
	for _, job := range retired {
		<-job.done
		m.mu.Lock()
		if m.jobs[job.key] == job {
			delete(m.jobs, job.key)
		}
		m.mu.Unlock()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for key, want := range wanted {
		if m.jobs[key] != nil || ctx.Err() != nil {
			continue
		}
		child, cancel := context.WithCancel(ctx)
		job := &campaignInferenceJob{owner: m, key: key, document: want.document, ownerRef: want.ownerRef, private: want.private, campaign: want.campaign, cancel: cancel, done: make(chan struct{}), queue: make(chan DetectionNotification, 16), generations: map[string]uint64{}, status: data.InferenceStatus{State: "loading", Sources: map[string]string{}}}
		m.jobs[key] = job
		go job.supervise(child)
	}
}

// ownedByLeasedWatch reports whether owner names a leased watch, or a watch
// that cannot be read. That watch's inference is private: like the watch's
// journal entries, its events reach only the client that leased it, so they
// stay off the device-event journal every reader shares (spec §6.5). A watch
// cannot add or drop its lease, so the answer holds for the job's life.
func (m *campaignInferenceManager) ownedByLeasedWatch(owner string) bool {
	name, ok := strings.CutPrefix(owner, data.KindWatch+"/")
	if !ok {
		return false
	}
	stored, err := m.service.manager.Document(data.KindWatch, name)
	if err != nil {
		return true
	}
	watch, err := stored.Watch()
	return err != nil || watch.Leased()
}

// reconcileNow runs one pass in the caller's goroutine; it returns once the
// jobs it retired have stopped.
func (m *campaignInferenceManager) reconcileNow() { m.reconcile(m.ctx) }

// wakeNow asks the loop for a pass without waiting for it.
func (m *campaignInferenceManager) wakeNow() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

// Keep retired jobs visible until their process and camera subscriptions exit.
// Persisting enabled:false only requests shutdown; it does not complete it.
func (m *campaignInferenceManager) inferenceActive(key string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	job := m.jobs[key]
	if job == nil || !job.campaign.Inference.IsEnabled() {
		return false
	}
	select {
	case <-job.done:
		return false
	default:
		return true
	}
}

func (m *campaignInferenceManager) snapshot(key, revision string) *data.InferenceStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	job := m.jobs[key]
	if job == nil || job.campaign.Revision != revision {
		return &data.InferenceStatus{State: "stopped"}
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

func (j *campaignInferenceJob) setState(state string, err error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.status.State = state
	j.status.Error = ""
	if err != nil {
		j.status.Error = err.Error()
	}
}
func (j *campaignInferenceJob) sourceState(source, state string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.status.Sources[source] = state
}

func (j *campaignInferenceJob) supervise(ctx context.Context) {
	defer close(j.done)
	ctx, cancel := context.WithCancel(ctx)
	notifyDone := make(chan struct{})
	go func() { defer close(notifyDone); j.notifications(ctx, j.queue) }()
	defer func() { cancel(); <-notifyDone }()
	if !j.campaign.Inference.IsEnabled() {
		j.setState("running", nil)
		<-ctx.Done()
		return
	}
	for ctx.Err() == nil {
		j.setState("loading", nil)
		err := j.run(ctx)
		if ctx.Err() != nil {
			return
		}
		j.setState("error", err)
		what := "campaign"
		if j.document {
			what = "inference document"
		}
		j.owner.service.manager.Warnf("%s %q inference: %v; retrying in 5s", what, j.campaign.Name, err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
	}
}

type inferenceSource struct {
	cancel context.CancelFunc
	done   chan struct{}
}

func (j *campaignInferenceJob) run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if j.owner.factory == nil {
		return errors.New("model runtime is unavailable")
	}
	session, err := j.owner.factory.Start(ctx, *j.campaign.Inference)
	if err != nil {
		return err
	}
	sources := map[string]inferenceSource{}
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
	presence := map[string]*inferencePresence{}
	// The collections whose episodes an inference document's records may
	// reach, refreshed with the cameras on every pass (spec §7.3).
	var audience map[string]bool
	reconcile := func() {
		if j.document {
			audience = j.owner.service.collectionsTriggeredBy(j.campaign.Inference.Event)
			j.mu.Lock()
			j.audience = audience
			j.mu.Unlock()
		}
		ids, _, _, resolveErr := j.owner.service.manager.ResolveCampaignSources(j.campaign)
		selected := map[string]bool{}
		if resolveErr == nil {
			for _, id := range ids {
				if _, ok := cameraDeviceID(id); ok {
					selected[id] = true
				}
			}
		}
		for id, source := range sources {
			if selected[id] {
				continue
			}
			source.cancel()
			<-source.done
			j.mu.Lock()
			delete(j.generations, id)
			delete(j.status.Sources, id)
			j.mu.Unlock()
			delete(sources, id)
			// Keep presence and cooldown across temporary camera loss.
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
			sources[id] = inferenceSource{stop, done}
			if presence[id] == nil {
				presence[id] = &inferencePresence{}
			}
			changed = true
			go func(id string) { defer close(done); j.stream(child, session, id) }(id)
		}
		if resolveErr != nil {
			j.setState("waiting_for_cameras", resolveErr)
		} else if len(sources) == 0 {
			j.setState("waiting_for_cameras", nil)
		} else {
			j.setState("running", nil)
		}
		if changed && !j.document {
			if _, active := j.owner.service.manager.ActiveSession(j.campaign.Name); !active {
				j.owner.service.armCampaign(j.campaign)
			}
		}
	}
	reconcile()
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			reconcile()
		case result, ok := <-session.Results():
			if !ok {
				return errors.New("model process exited")
			}
			if result.Type == "error" {
				return errors.New(result.Error)
			}
			j.mu.Lock()
			generation, active := j.generations[result.SourceID]
			j.mu.Unlock()
			if !active || generation != result.Generation || ctx.Err() != nil {
				continue
			}
			if result.Type == "source_error" {
				j.sourceState(result.SourceID, result.Error)
				continue
			}
			if result.Type != "prediction" {
				return errors.New("unexpected model result type")
			}
			detections, err := validateDetections(j.campaign.Inference, result.Detections)
			if err != nil {
				return err
			}
			j.sourceState(result.SourceID, "detecting")
			if j.document {
				j.documentResult(presence[result.SourceID], result.SourceID, detections, result.DroppedResults, audience)
				continue
			}
			attributes := map[string]any{"campaign": j.campaign.Name, "source_id": result.SourceID, "model_version": j.campaign.Inference.Revision, "detections": detections,
				"input_reference_status": "encoded_stream_decode_does_not_preserve_sample_ids", "runtime_results_dropped": result.DroppedResults}
			record := data.ApplicationRecord{Version: 1, Type: "prediction", Model: j.campaign.Inference.Model, Attributes: attributes}
			if _, err := j.owner.service.manager.RecordCampaignApplication(j.appID(), record); err != nil {
				j.owner.service.manager.Warnf("recording campaign prediction: %v", err)
			}
			state := presence[result.SourceID]
			if state == nil || !state.observe(len(detections) > 0, time.Now(), j.campaign.Inference) {
				continue
			}
			record.Type, record.Name = "event", j.campaign.Inference.Event
			accepted, triggerErr := j.owner.service.triggerInference(ctx, j.campaign, record)
			if !accepted {
				continue
			}
			if triggerErr != nil {
				j.owner.service.manager.Warnf("campaign %q detection could not start an episode: %v", j.campaign.Name, triggerErr)
			}
			if j.campaign.Notify != nil && (j.campaign.Notify.On == data.NotifyOnDetection || j.campaign.Notify.On == data.NotifyOnEvent && j.campaign.Notify.Event == record.Name) {
				request := detectionNotification(j.campaign, result.SourceID, detections)
				j.enqueueNotification(request)
			}
		}
	}
}

func (j *campaignInferenceJob) stream(ctx context.Context, session inference.Session, sourceID string) {
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
					err = errors.New("camera sample exceeds inference limit of 8MiB")
					break
				}
				err = session.Send(inference.Input{SourceID: sourceID, Generation: generation, Encoding: sample.Encoding, Payload: sample.Payload, Initialization: sample.DecoderInit, DroppedBefore: sample.DroppedBefore})
				if err != nil {
					break
				}
				if j.ownerLapsed() {
					continue
				}
				if err := j.owner.service.manager.RecordModelInputFor(data.ModelInput{AppID: j.appID(), Model: j.campaign.Inference.Model, SourceID: sourceID, SampleID: sample.SampleID, BootNanos: sample.BootNanos, UncertaintyNanos: sample.UncertaintyNanos, PayloadBytes: len(sample.Payload), Encoding: sample.Encoding, SelfContained: sample.SelfContained, DroppedBefore: sample.DroppedBefore}, j.modelInputAudience()); err != nil {
					j.owner.service.manager.Warnf("recording campaign model input: %v", err)
				}
			}
			subscription.Close()
			j.mu.Lock()
			delete(j.generations, sourceID)
			j.mu.Unlock()
			// An end marker tears down the decoder and its queued frames before reuse.
			_ = session.Send(inference.Input{SourceID: sourceID, Generation: generation, End: true})
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

func validateDetections(config *data.CampaignInference, values []inference.Detection) ([]inference.Detection, error) {
	if len(values) > 100 {
		return nil, errors.New("model returned too many detections")
	}
	labels := map[string]bool{}
	for _, label := range config.Labels {
		labels[label] = true
	}
	out := []inference.Detection{}
	for _, detection := range values {
		if math.IsNaN(detection.Score) || math.IsInf(detection.Score, 0) || detection.Score < 0 || detection.Score > 1 {
			return nil, errors.New("invalid model detection score")
		}
		for _, coordinate := range detection.Box {
			if math.IsNaN(coordinate) || math.IsInf(coordinate, 0) {
				return nil, errors.New("invalid model detection box")
			}
		}
		if labels[detection.Label] && detection.Score >= config.Threshold {
			out = append(out, detection)
		}
	}
	return out, nil
}

type inferencePresence struct {
	present                            bool
	emptySince, lastObserved, lastSent time.Time
}

func (p *inferencePresence) observe(detected bool, now time.Time, config *data.CampaignInference) bool {
	maxGap := max(5*time.Second, time.Duration(2/config.Rate*float64(time.Second)))
	if now.Sub(p.lastObserved) > maxGap {
		p.emptySince = time.Time{}
	}
	p.lastObserved = now
	if !detected {
		if p.emptySince.IsZero() {
			p.emptySince = now
		}
		if now.Sub(p.emptySince) >= config.ClearDuration() {
			p.present = false
		}
		return false
	}
	p.emptySince = time.Time{}
	if p.present || (!p.lastSent.IsZero() && now.Sub(p.lastSent) < config.CooldownDuration()) {
		return false
	}
	p.present, p.lastSent = true, now
	return true
}

func detectionNotification(campaign data.Campaign, source string, detections []inference.Detection) DetectionNotification {
	return DetectionNotification{ID: uuid.NewString(), Event: campaign.Inference.Event, Campaign: campaign.Name, SourceID: source, Model: campaign.Inference.Model, Revision: campaign.Inference.Revision, Count: len(detections), Detections: notificationDetections(detections), OccurredAt: time.Now().UTC().Format(time.RFC3339Nano)}
}

// inferenceNotification is an inference document's arrival as a watch
// receives it: no campaign, the inference named instead.
func inferenceNotification(campaign data.Campaign, source string, detections []inference.Detection) DetectionNotification {
	notification := detectionNotification(campaign, source, detections)
	notification.Campaign = ""
	notification.Inference = campaign.Name
	return notification
}

// documentResult handles one scored frame of an inference document (spec
// §7.3). An arrival reaches watches first; the records go only to the
// collections that trigger on its event, and storing them never holds up a
// notification: watches are told before anything is written.
func (j *campaignInferenceJob) documentResult(state *inferencePresence, sourceID string, detections []inference.Detection, dropped uint64, audience map[string]bool) {
	if j.ownerLapsed() {
		return
	}
	service := j.owner.service
	appID := j.appID()
	attributes := map[string]any{"inference": j.campaign.Name, "source_id": sourceID, "model_version": j.campaign.Inference.Revision, "detections": detections,
		"input_reference_status": "encoded_stream_decode_does_not_preserve_sample_ids", "runtime_results_dropped": dropped}
	record := data.ApplicationRecord{Version: 1, Type: "prediction", Model: j.campaign.Inference.Model, Attributes: attributes}
	arrival := state != nil && state.observe(len(detections) > 0, time.Now(), j.campaign.Inference)
	// Watches are told first: the record writes below take the manager lock,
	// which can queue behind a seal.
	if arrival && service.watches != nil {
		service.watches.deliver(j.campaign.Name, inferenceNotification(j.campaign, sourceID, detections))
	}
	if _, err := service.manager.RecordApplicationFor(appID, record, audience, false, j.private); err != nil {
		service.manager.Warnf("inference %q prediction: %v", j.campaign.Name, err)
	}
	if !arrival {
		return
	}
	record.Type, record.Name = "event", j.campaign.Inference.Event
	if _, err := service.manager.RecordApplicationFor(appID, record, audience, true, j.private); err != nil {
		service.manager.Warnf("inference %q event: %v", j.campaign.Name, err)
	}
}

// ownerLapsed reports whether the job belongs to a watch whose lease has
// lapsed. The watch may live a few seconds longer, until the expiry pass, but
// its client has lost it, so the job writes nothing meanwhile (spec §6.3).
func (j *campaignInferenceJob) ownerLapsed() bool {
	name, ok := strings.CutPrefix(j.ownerRef, "watch/")
	return j.document && ok && j.owner.service.leases.lapsed(name)
}

// appID is the application id every record of this job carries.
func (j *campaignInferenceJob) appID() string {
	if j.document {
		return inferenceAppPrefix + j.campaign.Name
	}
	return campaignAppPrefix + j.campaign.Name
}

// modelInputAudience is the episodes this job's model inputs may reach: all of
// them for a campaign (nil), the document's collections for a document.
func (j *campaignInferenceJob) modelInputAudience() map[string]bool {
	if !j.document {
		return nil
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.audience == nil {
		return map[string]bool{}
	}
	return j.audience
}

// collectionsTriggeredBy names the armed collections whose capture triggers
// include event: the only episodes an inference document's records reach.
func (s *DataService) collectionsTriggeredBy(event string) map[string]bool {
	audience := map[string]bool{}
	campaigns, err := s.manager.Campaigns()
	if err != nil {
		s.manager.Warnf("reading campaigns for inference records: %v", err)
		return audience
	}
	for _, campaign := range campaigns {
		if campaign.State != "armed" {
			continue
		}
		for _, trigger := range campaign.Capture.Triggers {
			if trigger.Event == event {
				audience[campaign.Name] = true
			}
		}
	}
	return audience
}

// notificationDetections keeps the five highest-scored detections, which keeps
// a journal entry well under its 4096-byte limit.
func notificationDetections(detections []inference.Detection) []data.NotificationDetection {
	sorted := slices.Clone(detections)
	slices.SortStableFunc(sorted, func(a, b inference.Detection) int { return cmp.Compare(b.Score, a.Score) })
	sorted = sorted[:min(len(sorted), 5)]
	out := make([]data.NotificationDetection, 0, len(sorted))
	for _, detection := range sorted {
		out = append(out, data.NotificationDetection{Label: detection.Label, Score: detection.Score})
	}
	return out
}

func (j *campaignInferenceJob) enqueueNotification(request DetectionNotification) {
	if err := j.owner.service.manager.RecordNotification(request); err != nil {
		j.notificationError(fmt.Errorf("persist notification: %w", err))
		return
	}
	select {
	case j.queue <- request:
	default:
		j.notificationError(errors.New("external notification queue full; notification retained in Wendy Data"))
	}
}

func (j *campaignInferenceJob) notificationError(err error) {
	j.mu.Lock()
	j.status.NotificationError = ""
	if err != nil {
		j.status.NotificationError = err.Error()
	}
	j.mu.Unlock()
	if err != nil {
		j.owner.service.manager.Warnf("campaign %q notification: %v", j.campaign.Name, err)
	}
}

func (j *campaignInferenceJob) notifications(ctx context.Context, queue <-chan DetectionNotification) {
	for {
		select {
		case <-ctx.Done():
			return
		case request := <-queue:
			err := deliverNotification(ctx, j.owner.sender, j.campaign.Notify.Webhook, request)
			if ctx.Err() != nil {
				return
			}
			j.notificationError(err)
		}
	}
}

func (s *DataService) campaignMessage(campaign data.Campaign) (*agentpbv2.DataCampaign, error) {
	eventNotifications := campaign.Notify != nil && campaign.Notify.On == data.NotifyOnEvent
	if campaign.Inference != nil || eventNotifications {
		campaign.InferenceStatus = &data.InferenceStatus{State: "disabled"}
		if !campaign.Inference.IsEnabled() && s.inference != nil && s.inference.inferenceActive(campaignJobKey(campaign.Name)) {
			campaign.InferenceStatus.State = "stopping"
		} else if campaign.Inference.IsEnabled() || eventNotifications {
			if s.inference == nil {
				campaign.InferenceStatus = &data.InferenceStatus{State: "error", Error: "agent campaign runtime is unavailable"}
			} else {
				campaign.InferenceStatus = s.inference.snapshot(campaignJobKey(campaign.Name), campaign.Revision)
				if campaign.InferenceStatus.State == "stopped" {
					campaign.InferenceStatus.State = "pending"
				}
			}
		}
	}
	return campaignMessage(campaign)
}

func (s *DataService) triggerInference(ctx context.Context, campaign data.Campaign, record data.ApplicationRecord) (bool, error) {
	s.deploymentMu.Lock()
	defer s.deploymentMu.Unlock()
	current, err := s.manager.Campaign(campaign.Name)
	if err != nil || current.Revision != campaign.Revision || !current.Inference.IsEnabled() || ctx.Err() != nil {
		return false, err
	}
	if _, err := s.manager.RecordCampaignApplication(campaignAppPrefix+campaign.Name, record); err != nil {
		return true, err
	}
	if _, active := s.manager.ActiveSession(campaign.Name); active {
		return true, nil
	}
	_, err = s.triggerCampaign(ctx, campaign, "event:"+campaign.Inference.Event, "event:"+campaign.Inference.Event)
	return true, err
}

// notifyEvent routes application events independently of episode capture triggers.
// Native model events use their owning job directly and bypass the global observer.
func (m *campaignInferenceManager) notifyEvent(campaign data.Campaign, record data.ApplicationRecord) {
	if campaign.Notify == nil || campaign.Notify.On != data.NotifyOnEvent || record.Type != "event" || record.Name != campaign.Notify.Event {
		return
	}
	m.mu.Lock()
	job := m.jobs[campaignJobKey(campaign.Name)]
	m.mu.Unlock()
	if job == nil || job.campaign.Revision != campaign.Revision {
		return
	}
	request := DetectionNotification{ID: uuid.NewString(), Event: record.Name, Campaign: campaign.Name, Model: record.Model, OccurredAt: time.Now().UTC().Format(time.RFC3339Nano)}
	job.enqueueNotification(request)
}
