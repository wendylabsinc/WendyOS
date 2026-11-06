package data

// Extended episodes.
//
// A campaign with capture.extend keeps its episode recording for as long as
// its trigger condition stays active, and ends it after_trigger after the
// condition was last active. capture.max_duration caps one episode: an event
// still active at the cap is split into parts, each its own sealed episode,
// linked through the manifest's extension block. The agent's data service
// decides when an episode ends; the manager records why, and lets a part's
// follow-up open before the part has sealed (Handoff), so the follow-up
// begins where the part stopped capturing rather than after its drain and its
// seal.

import (
	"errors"
	"fmt"
	"time"
)

// DefaultExtendMaxDuration caps one extended episode when the plan sets no
// capture.max_duration (or after_trigger, if that is longer).
//
// A minute keeps one part small enough for a metered link. Upload resumes per
// file, so an interrupted transfer sends the camera file it was on again from
// its start, and at the few Mbit/s of a USB camera's H.264, plus the
// playable.mp4 the seal derives from it, a minute is tens of megabytes where an
// hour-long event in one episode would be gigabytes. The cap also bounds how
// late a recording arrives: nothing of an event is sealed, uploaded or
// notified on (episode_committed) until its first part ends, so an uncapped
// recording of a long event would deliver nothing until the event was over.
const DefaultExtendMaxDuration = time.Minute

// End reasons recorded in EpisodeExtension.EndReason.
const (
	// EndReasonEventCleared: the trigger condition stopped being active and
	// after_trigger passed without it becoming active again.
	EndReasonEventCleared = "event_cleared"
	// EndReasonMaxDuration: the episode reached capture.max_duration. When
	// the condition was still active, NextEpisode names the follow-up.
	EndReasonMaxDuration = "max_duration"
	// EndReasonManualStop: an operator stopped the recording.
	EndReasonManualStop = "manual_stop"
)

// EpisodeExtension is the manifest's account of one part of an extended
// recording. The parts of one recording form a chain: each follow-up names
// the episode it continues, and each episode that was continued names its
// follow-up, so a reader can walk the whole event in either direction.
type EpisodeExtension struct {
	// Part is the episode's 1-based position in its recording.
	Part int `json:"part"`
	// PreviousEpisode is the episode this one continues, absent on part 1.
	PreviousEpisode string `json:"previous_episode,omitempty"`
	// NextEpisode is the follow-up that continues this episode, absent when
	// the recording ended here.
	NextEpisode string `json:"next_episode,omitempty"`
	// MaxDurationNanos is the cap this episode recorded under.
	MaxDurationNanos int64 `json:"max_duration_nanos"`
	// EndReason is one of the EndReason constants, absent while recording
	// and on an episode recovered after the agent stopped mid-recording.
	EndReason string `json:"end_reason,omitempty"`
}

// validateExtend checks capture.extend and capture.max_duration. after is the
// already validated after_trigger.
func (c CampaignCapture) validateExtend(after time.Duration) error {
	if c.MaxDuration == "" {
		return nil
	}
	if !c.Extend {
		return errors.New("capture.max_duration requires capture.extend: true")
	}
	limit, err := time.ParseDuration(c.MaxDuration)
	if err != nil || limit < after || limit > 24*time.Hour {
		// Shorter than after_trigger, every part would reach the cap before
		// one quiet after_trigger window could end it, and a single event
		// would be split into parts holding nothing but its tail.
		return fmt.Errorf("capture.max_duration must be a duration from after_trigger (%s) through 24h", after)
	}
	return nil
}

// SetEndReason records why the extended episode keyed by key ends. The first
// reason recorded wins, because it names the cause; a stop that follows it is
// its consequence. It does nothing for an episode that is not extended.
func (m *Manager) SetEndReason(key, reason string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a := m.active[key]
	if a == nil || a.manifest.Extension.Part == 0 || a.manifest.Extension.EndReason != "" {
		return
	}
	a.manifest.Extension.EndReason = reason
}

// EpisodeHandoff is an episode whose capture has stopped and whose campaign
// key is already free for its follow-up. Seal finishes it.
type EpisodeHandoff struct {
	m          *Manager
	a          *activeEpisode
	drainUntil time.Time
}

// Handoff ends capture for the episode keyed by key and frees the key at
// once, without waiting for the post-seal drain or the seal, so the campaign's
// follow-up episode can open while this one finishes. The caller stops the
// episode's capture adapters and applies their results first, exactly as for
// Stop, and then calls Seal.
//
// The drain is still served, by Seal, from the moment capture stopped: a
// record an application writes about this episode's last samples is filed
// here, and, like every record inside the window, into the follow-up as well.
func (m *Manager) Handoff(key string) (*EpisodeHandoff, error) {
	a, err := m.beginSeal(key, false)
	if err != nil {
		return nil, err
	}
	stopped := time.Now()
	if err := m.releaseKey(a); err != nil {
		return nil, err
	}
	h := &EpisodeHandoff{m: m, a: a}
	if a.wantsDrain() {
		h.drainUntil = stopped.Add(a.drain)
	}
	return h, nil
}

// ID is the handed-off episode's ID.
func (h *EpisodeHandoff) ID() string { return h.a.manifest.ID }

// Seal serves what is left of the episode's drain and seals it. next is the
// ID of the follow-up that continues it, "" when none opened. A non-empty
// interruption seals it interrupted with that reason, as Interrupt does.
func (h *EpisodeHandoff) Seal(next, interruption string) (Manifest, error) {
	time.Sleep(time.Until(h.drainUntil))
	h.m.mu.Lock()
	if h.m.stillOpenLocked(h.a) {
		h.a.manifest.Extension.NextEpisode = next
	}
	h.m.mu.Unlock()
	if interruption != "" {
		return h.m.finalize(h.a, "interrupted", interruption)
	}
	return h.m.finalize(h.a, "complete", "")
}
