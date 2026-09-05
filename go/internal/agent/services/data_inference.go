package services

import (
	"context"
	"errors"
	"fmt"
	"math"
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

type campaignInferenceManager struct {
	service *DataService
	factory inference.Factory
	sender  CampaignNotificationSender
	wake    chan struct{}
	mu      sync.Mutex
	jobs    map[string]*campaignInferenceJob
}

type campaignInferenceJob struct {
	owner          *campaignInferenceManager
	campaign       data.Campaign
	cancel         context.CancelFunc
	done           chan struct{}
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
	manager := &campaignInferenceManager{service: s, factory: factory, sender: sender, wake: make(chan struct{}, 1), jobs: map[string]*campaignInferenceJob{}}
	s.inference = manager // Configured once, before registering the RPC server.
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

func (m *campaignInferenceManager) stopAll() {
	m.mu.Lock()
	jobs := m.jobs
	m.jobs = map[string]*campaignInferenceJob{}
	m.mu.Unlock()
	for _, job := range jobs {
		job.cancel()
	}
	for _, job := range jobs {
		<-job.done
	}
}

func (m *campaignInferenceManager) reconcile(ctx context.Context) {
	campaigns, err := m.service.manager.Campaigns()
	if err != nil {
		m.service.manager.Warnf("reading inference campaigns: %v", err)
		return
	}
	wanted := map[string]data.Campaign{}
	for _, campaign := range campaigns {
		if campaign.State == "armed" && campaign.Inference.IsEnabled() {
			wanted[campaign.Name] = campaign
		}
	}
	m.mu.Lock()
	var retired []*campaignInferenceJob
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
		job := &campaignInferenceJob{owner: m, campaign: campaign, cancel: cancel, done: make(chan struct{}), generations: map[string]uint64{}, status: data.InferenceStatus{State: "loading", Sources: map[string]string{}}}
		m.jobs[name] = job
		go job.supervise(child)
	}
}

func (m *campaignInferenceManager) snapshot(name, revision string) *data.InferenceStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	job := m.jobs[name]
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
	for ctx.Err() == nil {
		j.setState("loading", nil)
		err := j.run(ctx)
		if ctx.Err() != nil {
			return
		}
		j.setState("error", err)
		j.owner.service.manager.Warnf("campaign %q inference: %v; retrying in 5s", j.campaign.Name, err)
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
	notifications := make(chan DetectionNotification, 16)
	notifyDone := make(chan struct{})
	go func() { defer close(notifyDone); j.notifications(ctx, notifications) }()
	defer func() { cancel(); <-notifyDone }()
	presence := map[string]*inferencePresence{}
	reconcile := func() {
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
			delete(presence, id)
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
			presence[id] = &inferencePresence{}
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
		if changed {
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
			attributes := map[string]any{"campaign": j.campaign.Name, "source_id": result.SourceID, "model_version": j.campaign.Inference.Revision, "detections": detections,
				"input_reference_status": "encoded_stream_decode_does_not_preserve_sample_ids", "runtime_results_dropped": result.DroppedResults}
			record := data.ApplicationRecord{Version: 1, Type: "prediction", Model: j.campaign.Inference.Model, Attributes: attributes}
			if _, err := j.owner.service.manager.RecordCampaignApplication(campaignAppPrefix+j.campaign.Name, record); err != nil {
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
			if j.campaign.Notify != nil && j.campaign.Notify.On == data.NotifyOnDetection {
				request := detectionNotification(j.campaign, result.SourceID, len(detections))
				select {
				case notifications <- request:
				default:
					j.notificationError(errors.New("notification queue full; detection notification dropped"))
				}
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
				err = session.Send(inference.Input{SourceID: sourceID, Generation: generation, Encoding: sample.Encoding, Payload: sample.Payload, DroppedBefore: sample.DroppedBefore})
				if err != nil {
					break
				}
				if err := j.owner.service.manager.RecordModelInput(data.ModelInput{AppID: campaignAppPrefix + j.campaign.Name, Model: j.campaign.Inference.Model, SourceID: sourceID, SampleID: sample.SampleID, BootNanos: sample.BootNanos, UncertaintyNanos: sample.UncertaintyNanos, PayloadBytes: len(sample.Payload), Encoding: sample.Encoding, SelfContained: sample.SelfContained, DroppedBefore: sample.DroppedBefore}); err != nil {
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

func detectionNotification(campaign data.Campaign, source string, count int) DetectionNotification {
	return DetectionNotification{ID: uuid.NewString(), Event: campaign.Inference.Event, Campaign: campaign.Name, SourceID: source, Model: campaign.Inference.Model, Revision: campaign.Inference.Revision, Count: count}
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
			var err error
			for attempt := 0; attempt < 3 && ctx.Err() == nil; attempt++ {
				if j.owner.sender == nil {
					err = errors.New("webhook notification delivery is unavailable")
					break
				}
				forward, cancel := context.WithTimeout(ctx, 10*time.Second)
				err = j.owner.sender.Send(forward, j.campaign.Notify.Webhook, request)
				cancel()
				if err == nil {
					break
				}

				if attempt < 2 {
					select {
					case <-ctx.Done():
						return
					case <-time.After(time.Duration(1<<attempt) * time.Second):
					}
				}
			}
			j.notificationError(err)
		}
	}
}

func (s *DataService) campaignMessage(campaign data.Campaign) (*agentpbv2.DataCampaign, error) {
	if campaign.Inference != nil {
		campaign.InferenceStatus = &data.InferenceStatus{State: "disabled"}
		if campaign.Inference.IsEnabled() {
			if s.inference == nil {
				campaign.InferenceStatus = &data.InferenceStatus{State: "error", Error: "agent inference runtime is unavailable"}
			} else {
				campaign.InferenceStatus = s.inference.snapshot(campaign.Name, campaign.Revision)
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
