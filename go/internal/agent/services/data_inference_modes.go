package services

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	"github.com/wendylabsinc/wendy/go/internal/agent/inference"
)

// inferenceMode is what differs between a campaign's inline inference and an
// inference document. The rest of campaignInferenceJob (supervise, run and
// stream) is shared and does not branch on the mode.
type inferenceMode interface {
	// noun names the job in log lines.
	noun() string
	// appID is the application id every record of the job carries.
	appID(name string) string
	// refresh runs on every pass over the cameras, before it.
	refresh(j *campaignInferenceJob)
	// camerasAdded runs after a pass started streaming a camera.
	camerasAdded(j *campaignInferenceJob)
	// result handles one scored frame and its validated detections. presence
	// is the frame's camera state, or nil.
	result(ctx context.Context, j *campaignInferenceJob, presence *inferencePresence, result inference.Result, detections []inference.Detection)
	// paused reports whether the job must write nothing for now.
	paused(j *campaignInferenceJob) bool
	// modelInputAudience is the episodes the job's model inputs may reach; nil
	// means all of them.
	modelInputAudience() map[string]bool
}

// campaignInference is a campaign's inline inference: its detections are the
// campaign's records, start its episodes and send its notifications.
type campaignInference struct{}

func (campaignInference) noun() string                        { return "campaign" }
func (campaignInference) appID(name string) string            { return data.CampaignAppID(name) }
func (campaignInference) refresh(*campaignInferenceJob)       {}
func (campaignInference) paused(*campaignInferenceJob) bool   { return false }
func (campaignInference) modelInputAudience() map[string]bool { return nil }

// camerasAdded arms the campaign's pre-roll on the new cameras, unless an
// episode is already recording.
func (campaignInference) camerasAdded(j *campaignInferenceJob) {
	if _, active := j.owner.service.manager.ActiveSession(j.campaign.Name); !active {
		j.owner.service.armCampaign(j.campaign)
	}
}

func (campaignInference) result(ctx context.Context, j *campaignInferenceJob, state *inferencePresence, result inference.Result, detections []inference.Detection) {
	attributes := j.predictionAttributes("campaign", result, detections)
	record := data.ApplicationRecord{Version: 1, Type: "prediction", Model: j.campaign.Inference.Model, Attributes: attributes}
	if _, err := j.owner.service.manager.RecordCampaignApplication(j.appID(), record); err != nil {
		j.owner.service.manager.Warnf("recording campaign prediction: %v", err)
	}
	if state == nil || !state.observe(len(detections) > 0, time.Now(), j.campaign.Inference) {
		return
	}
	record.Type, record.Name = "event", j.campaign.Inference.Event
	accepted, triggerErr := j.owner.service.triggerInference(ctx, j.campaign, record)
	if !accepted {
		return
	}
	if triggerErr != nil {
		j.owner.service.manager.Warnf("campaign %q detection could not start an episode: %v", j.campaign.Name, triggerErr)
	}
	if j.campaign.Notify != nil && (j.campaign.Notify.On == data.NotifyOnDetection || j.campaign.Notify.On == data.NotifyOnEvent && j.campaign.Notify.Event == record.Name) {
		j.enqueueNotification(detectionNotification(j.campaign, result.SourceID, detections))
	}
}

// documentInference is an inference document's job (spec §7.3): no campaign
// capture and no campaign notifications. Arrivals go to watches, and records
// only to the collections that trigger on the document's event.
type documentInference struct {
	// ownerRef is the document's owner reference, or empty.
	ownerRef string
	// private keeps the job's events off the shared device-event journal; see
	// ownedByLeasedWatch.
	private bool

	mu sync.Mutex
	// audience is the collections whose episodes the job's records may reach.
	// refresh replaces it (never mutates it); the stream goroutines read it.
	audience map[string]bool
}

func (*documentInference) noun() string                       { return "inference document" }
func (*documentInference) appID(name string) string           { return inferenceAppPrefix + name }
func (*documentInference) camerasAdded(*campaignInferenceJob) {}

func (d *documentInference) refresh(j *campaignInferenceJob) {
	audience := j.owner.service.collectionsTriggeredBy(j.campaign.Inference.Event)
	d.mu.Lock()
	d.audience = audience
	d.mu.Unlock()
}

func (d *documentInference) currentAudience() map[string]bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.audience
}

// paused reports whether the job belongs to a watch whose lease has lapsed.
// The watch may live a few seconds longer, until the expiry pass, but its
// client has lost it, so the job writes nothing meanwhile (spec §6.3).
func (d *documentInference) paused(j *campaignInferenceJob) bool {
	name, ok := strings.CutPrefix(d.ownerRef, "watch/")
	return ok && j.owner.service.leases.lapsed(name)
}

// modelInputAudience is the document's collections, none before the first
// pass.
func (d *documentInference) modelInputAudience() map[string]bool {
	if audience := d.currentAudience(); audience != nil {
		return audience
	}
	return map[string]bool{}
}

// result handles one scored frame. An arrival reaches watches first; the
// records go only to the collections that trigger on its event, and storing
// them never holds up a notification: watches are told before anything is
// written.
func (d *documentInference) result(_ context.Context, j *campaignInferenceJob, state *inferencePresence, result inference.Result, detections []inference.Detection) {
	if d.paused(j) {
		return
	}
	service := j.owner.service
	appID := j.appID()
	audience := d.currentAudience()
	attributes := j.predictionAttributes("inference", result, detections)
	record := data.ApplicationRecord{Version: 1, Type: "prediction", Model: j.campaign.Inference.Model, Attributes: attributes}
	arrival := state != nil && state.observe(len(detections) > 0, time.Now(), j.campaign.Inference)
	// Watches are told first: the record writes below take the manager lock,
	// which can queue behind a seal.
	if arrival && service.watches != nil {
		service.watches.deliver(j.campaign.Name, inferenceNotification(j.campaign, result.SourceID, detections))
	}
	if _, err := service.manager.RecordApplicationFor(appID, record, audience, false, d.private); err != nil {
		service.manager.Warnf("inference %q prediction: %v", j.campaign.Name, err)
	}
	if !arrival {
		return
	}
	record.Type, record.Name = "event", j.campaign.Inference.Event
	if _, err := service.manager.RecordApplicationFor(appID, record, audience, true, d.private); err != nil {
		service.manager.Warnf("inference %q event: %v", j.campaign.Name, err)
	}
}
