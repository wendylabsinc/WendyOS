package services

// capture.extend: a campaign recording that lasts as long as its event.
//
// Without extend, an episode ends a fixed after_trigger after its trigger,
// whatever is happening by then, and a trigger that arrives while it records
// is dropped. With extend, everything that would have triggered the campaign
// keeps its recording going instead:
//
//   - an application event matching one of its triggers
//     (observeApplicationRecord);
//   - a manual trigger (CampaignTrigger);
//   - an inference detection still in view, or not yet cleared
//     (campaignInferenceJob.run).
//
// Each reports that the trigger condition was active at that instant
// (extendCampaign), and the recording ends after_trigger after the last such
// instant. A source whose condition is raised until it clears, such as a
// grid rule event (#2548), plugs in the same way: it calls extendCampaign on
// every evaluation while the condition is raised, and the recording then ends
// after_trigger after it cleared. That is deliberately not a raise/clear pair
// with no expiry: a source that died while raised would keep a camera
// recording until someone noticed.
//
// capture.max_duration caps one episode. If the recording would still be
// going at the cap, the episode is sealed and a follow-up opens at once,
// linked to it through the manifest's extension block (data.EpisodeExtension):
// the cap splits a long recording into parts and never shortens it. The
// follow-up does not get the campaign's buffer again. It reaches back
// extendHandoverOverlap into the part it continues, through a camera ring
// armed shortly before the cap, so its video opens on a keyframe from before
// the handover and nothing between the two parts is lost.

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/protobuf/proto"
)

const (
	// extendHandoverOverlap is how far a follow-up part reaches back into the
	// part it continues. A video file can only begin on a keyframe, so a
	// follow-up that opened exactly where its predecessor stopped would lose
	// everything up to the camera's next one. It opens instead at the last
	// keyframe at least this long before the handover: the parts overlap by
	// this much plus at most one group of pictures, and application records
	// inside the window are in both.
	extendHandoverOverlap = 2 * time.Second
	// extendHandoverLead is how long before the cap the handover ring is
	// armed, so that by the cap it holds a keyframe from before the overlap
	// window. It covers groups of pictures of up to eight seconds.
	extendHandoverLead = 10 * time.Second
)

// extendedRecording is one campaign's recording under capture.extend, from
// its first part to its last.
type extendedRecording struct {
	name, reason, expression string
	// ready is closed once the first part has opened or failed to.
	ready chan struct{}
	// ctx ends the supervisor; cancel is called when the recording ends.
	ctx    context.Context
	cancel context.CancelFunc

	mu sync.Mutex
	// campaign is the plan the current part records with.
	campaign data.Campaign
	episode  *agentpbv2.DataEpisode
	part     int
	// started is when the current part opened; its cap counts from there.
	started time.Time
	// lastActive is the last instant the trigger condition was active.
	lastActive time.Time
	ended      bool
	// handover reports that a handover ring was armed, or an arming was
	// attempted, for the next part and no part has used it yet.
	handover bool
}

// extendCampaign reports that the trigger condition of the named campaign is
// active now. It keeps the campaign's extended recording going and returns
// true, or returns false when no extended recording of it is running. It
// never blocks on a capture start or stop, so the inference loop can call it
// for every prediction.
func (s *DataService) extendCampaign(name string) bool {
	r := s.extendedFor(name)
	if r == nil {
		return false
	}
	_, ok := r.extend(time.Now(), false)
	return ok
}

// extend records activity at now unless the recording has ended, and returns
// its current part. With wait it first waits for the first part to open.
func (r *extendedRecording) extend(now time.Time, wait bool) (*agentpbv2.DataEpisode, bool) {
	if wait {
		<-r.ready
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ended {
		return nil, false
	}
	if now.After(r.lastActive) {
		r.lastActive = now
	}
	if r.episode == nil {
		return nil, true
	}
	return proto.Clone(r.episode).(*agentpbv2.DataEpisode), true
}

func (s *DataService) extendedFor(name string) *extendedRecording {
	s.captureMu.Lock()
	defer s.captureMu.Unlock()
	return s.extended[name]
}

// forgetExtended drops r from the registry unless a newer recording of the
// same campaign has replaced it.
func (s *DataService) forgetExtended(r *extendedRecording) {
	s.captureMu.Lock()
	defer s.captureMu.Unlock()
	if s.extended[r.name] == r {
		delete(s.extended, r.name)
	}
}

// triggerExtended is triggerCampaign for a campaign with capture.extend.
// While its recording runs, a trigger keeps it going and returns the part
// recording now; otherwise it opens part 1.
func (s *DataService) triggerExtended(ctx context.Context, campaign data.Campaign, reason, expression string) (*agentpbv2.DataEpisode, error) {
	now := time.Now()
	for {
		s.captureMu.Lock()
		if s.extended == nil {
			s.extended = map[string]*extendedRecording{}
		}
		existing := s.extended[campaign.Name]
		if existing == nil || existing.isEnded() {
			r := &extendedRecording{name: campaign.Name, reason: reason, expression: expression, ready: make(chan struct{}), campaign: campaign, lastActive: now}
			r.ctx, r.cancel = context.WithCancel(context.Background())
			s.extended[campaign.Name] = r
			s.captureMu.Unlock()
			return s.startExtended(ctx, r)
		}
		s.captureMu.Unlock()
		if episode, ok := existing.extend(now, true); ok {
			return episode, nil
		}
		// It ended while this trigger waited for it: open the next one.
	}
}

func (r *extendedRecording) isEnded() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.ended
}

// startExtended opens part 1 of r and starts its supervisor.
func (s *DataService) startExtended(ctx context.Context, r *extendedRecording) (*agentpbv2.DataEpisode, error) {
	campaign := r.campaign
	opts, err := s.campaignStartOptions(campaign, r.reason, r.expression)
	var episode *agentpbv2.DataEpisode
	if err == nil {
		opts.Extension = data.EpisodeExtension{Part: 1, MaxDurationNanos: campaign.MaxEpisodeDuration().Nanoseconds()}
		episode, err = s.startCapture(ctx, opts)
	}
	if err != nil {
		r.end()
		close(r.ready)
		s.forgetExtended(r)
		return nil, err
	}
	adopted := s.adoptPart(r, campaign, episode, 1)
	close(r.ready)
	if !adopted {
		// Stopped before it could be supervised; that stop sealed it.
		r.end()
		s.forgetExtended(r)
		return episode, nil
	}
	go s.superviseExtended(r)
	return episode, nil
}

// adoptPart makes episode the recording's current part. It runs under the
// campaign key's lock and checks that the episode is still the one
// recording, so a stop that landed after the start ends the recording rather
// than leaving it to extend an episode that is already sealed.
func (s *DataService) adoptPart(r *extendedRecording, campaign data.Campaign, episode *agentpbv2.DataEpisode, part int) bool {
	lock := s.keyLock(campaign.Name)
	lock.Lock()
	defer lock.Unlock()
	session, ok := s.manager.ActiveSession(campaign.Name)
	now := time.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ended || !ok || session.ID != episode.GetId() {
		r.ended = true
		return false
	}
	r.campaign, r.episode, r.part, r.started, r.handover = campaign, episode, part, now, false
	if now.After(r.lastActive) && part == 1 {
		// Part 1's after_trigger counts from when it began recording, as the
		// fixed window does.
		r.lastActive = now
	}
	return true
}

type extendStep int

const (
	extendWait extendStep = iota
	extendEnd
	extendRollOver
	extendArmHandover
	extendStopped
)

// next decides what the supervisor does at now, and how long it waits
// otherwise. Deciding to end marks the recording ended in the same critical
// section, so activity reported after the decision opens a new recording
// instead of extending one that is stopping.
func (r *extendedRecording) next(now time.Time) (extendStep, time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ended {
		return extendStopped, 0
	}
	end := r.lastActive.Add(r.campaign.AfterTriggerDuration())
	capAt := r.started.Add(r.campaign.MaxEpisodeDuration())
	armAt := capAt.Add(-extendHandoverLead)
	switch {
	case !now.Before(capAt) && capAt.Before(end):
		// Still active at the cap: the recording continues in a new part.
		return extendRollOver, 0
	case !now.Before(end):
		r.ended = true
		return extendEnd, 0
	case !r.handover && !now.Before(armAt):
		r.handover = true
		return extendArmHandover, 0
	}
	wait := min(end.Sub(now), capAt.Sub(now))
	if !r.handover {
		wait = min(wait, armAt.Sub(now))
	}
	return extendWait, wait
}

// superviseExtended runs r from its first part to its end.
func (s *DataService) superviseExtended(r *extendedRecording) {
	defer s.forgetExtended(r)
	timer := time.NewTimer(time.Hour)
	defer timer.Stop()
	for {
		step, wait := r.next(time.Now())
		switch step {
		case extendStopped:
			return
		case extendEnd:
			s.endExtended(r, data.EndReasonEventCleared)
			return
		case extendRollOver:
			if !s.rollOver(r) {
				return
			}
			continue
		case extendArmHandover:
			s.armHandover(r)
			continue
		}
		timer.Reset(wait)
		select {
		case <-r.ctx.Done():
			return
		case <-timer.C:
		}
	}
}

// endExtended stops r's current part, recording reason as why the recording
// ended.
func (s *DataService) endExtended(r *extendedRecording, reason string) {
	campaign, id := r.current()
	lock := s.keyLock(campaign.Name)
	lock.Lock()
	defer lock.Unlock()
	if session, ok := s.manager.ActiveSession(campaign.Name); !ok || session.ID != id {
		r.end()
		s.releaseHandover(r)
		return
	}
	s.manager.SetEndReason(campaign.Name, reason)
	_, _ = s.stopCaptureLocked(context.Background(), campaign.Name)
}

func (r *extendedRecording) current() (data.Campaign, string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.campaign, r.episode.GetId()
}

func (r *extendedRecording) end() {
	r.mu.Lock()
	r.ended = true
	r.mu.Unlock()
	r.cancel()
}

// endExtendedPart ends the extended recording whose current part is the
// episode keyed by key, if there is one: stopping a part stops the recording,
// it is not continued. It also releases a handover ring the recording armed
// and did not use. stopCaptureLocked calls it first, before it re-arms the
// campaign's own pre-roll.
func (s *DataService) endExtendedPart(key string) {
	r := s.extendedFor(key)
	if r == nil {
		return
	}
	session, ok := s.manager.ActiveSession(key)
	if !ok {
		return
	}
	if _, id := r.current(); id != session.ID {
		return
	}
	r.end()
	s.releaseHandover(r)
}

// rollOver seals r's current part at its cap and opens the follow-up. It
// returns false when the recording ended instead.
func (s *DataService) rollOver(r *extendedRecording) bool {
	previous, id := r.current()
	name := previous.Name
	r.mu.Lock()
	part := r.part
	r.mu.Unlock()
	// The follow-up records with the plan deployed now, and its sources are
	// resolved before the current part stops, so the handover is short.
	campaign, err := s.manager.Campaign(name)
	var opts data.StartOptions
	if err == nil && !campaign.Capture.Extend {
		err = errors.New("the deployed plan no longer sets capture.extend")
	}
	if err == nil {
		opts, err = s.campaignStartOptions(campaign, r.reason, r.expression)
	}
	if err != nil {
		s.manager.Warnf("campaign %q: recording %s reached capture.max_duration while its event was still active, but its next part cannot open, so it ends here: %v", name, id, err)
		s.endExtended(r, data.EndReasonMaxDuration)
		r.end()
		return false
	}
	// The overlap replaces the campaign's buffer, and the notify intent stays
	// with part 1: one event commits one notification, not one per part.
	opts.PreRollDuration = extendHandoverOverlap
	opts.Trigger.Notify = nil
	opts.Extension = data.EpisodeExtension{Part: part + 1, PreviousEpisode: id, MaxDurationNanos: campaign.MaxEpisodeDuration().Nanoseconds()}

	lock := s.keyLock(name)
	lock.Lock()
	if session, ok := s.manager.ActiveSession(name); !ok || session.ID != id {
		lock.Unlock()
		r.end()
		return false
	}
	// Unlike stopCaptureLocked, this does not re-arm the campaign's pre-roll:
	// the follow-up takes the handover ring armed before the cap.
	results, captureErrs := s.stopAdaptersLocked(context.Background(), name)
	if len(results) > 0 {
		if applyErr := s.manager.ApplyCaptureResults(name, results); applyErr != nil {
			s.manager.Warnf("recording capture results into episode key %q failed; its manifest will omit per-source capture counters: %v", name, applyErr)
		}
	}
	s.manager.SetEndReason(name, data.EndReasonMaxDuration)
	handoff, err := s.manager.Handoff(name)
	lock.Unlock()
	if err != nil {
		s.manager.Warnf("campaign %q: handing recording %s over to its next part failed: %v", name, id, err)
		r.end()
		s.releaseHandover(r)
		go s.rearmByName(name)
		return false
	}
	interruption := ""
	if len(captureErrs) > 0 {
		interruption = "capture_adapter_failed"
	}
	episode, startErr := s.startCapture(context.Background(), opts)
	next := ""
	if startErr == nil {
		next = episode.GetId()
	}
	go func() {
		if _, sealErr := handoff.Seal(next, interruption); sealErr != nil {
			s.manager.Warnf("campaign %q: sealing part %d (%s) failed: %v", name, part, handoff.ID(), sealErr)
		}
	}()
	if startErr != nil {
		s.manager.Warnf("campaign %q: recording %s reached capture.max_duration while its event was still active, but its next part failed to start, so it ends here: %v", name, id, startErr)
		r.end()
		s.releaseHandover(r)
		go s.rearmByName(name)
		return false
	}
	return s.adoptPart(r, campaign, episode, part+1)
}

// armHandover arms the campaign's cameras with a ring of
// extendHandoverOverlap for the follow-up part to open from. It replaces the
// campaign's own pre-roll ring if one is armed; the ring is put back when the
// recording ends (stopCaptureLocked re-arms the campaign).
func (s *DataService) armHandover(r *extendedRecording) {
	arming := s.arming()
	if arming == nil {
		return
	}
	campaign, _ := r.current()
	cameras := s.cameraSourcesFor(campaign)
	if len(cameras) == 0 {
		return
	}
	arming.Disarm(campaign.Name)
	arming.Arm(campaign.Name, extendHandoverOverlap, cameras)
}

// releaseHandover disarms a handover ring r armed and no part used, so a
// campaign without a buffer of its own does not keep its cameras subscribed.
func (s *DataService) releaseHandover(r *extendedRecording) {
	r.mu.Lock()
	armed := r.handover
	r.handover = false
	r.mu.Unlock()
	if !armed {
		return
	}
	if arming := s.arming(); arming != nil {
		arming.Disarm(r.name)
	}
}
