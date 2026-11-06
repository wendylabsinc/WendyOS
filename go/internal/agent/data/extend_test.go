package data

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func withCapture(extra string) []byte {
	return []byte(strings.Replace(drainDigestPinYAML, "  after_trigger: 5s\n", "  after_trigger: 5s\n"+extra, 1))
}

func TestCaptureExtendValidation(t *testing.T) {
	for _, extra := range []string{
		"  max_duration: 30s\n",                  // a cap without extend
		"  extend: false\n  max_duration: 30s\n", // ditto, spelled out
		"  extend: true\n  max_duration: 4s\n",   // shorter than after_trigger
		"  extend: true\n  max_duration: 25h\n",
		"  extend: true\n  max_duration: soon\n",
	} {
		if _, err := ParseCampaign(withCapture(extra)); err == nil || !strings.Contains(err.Error(), "capture.max_duration") {
			t.Errorf("%q was accepted: %v", extra, err)
		}
	}

	fixed, err := ParseCampaign([]byte(drainDigestPinYAML))
	if err != nil {
		t.Fatal(err)
	}
	if fixed.Capture.Extend || fixed.MaxEpisodeDuration() != 0 {
		t.Fatalf("a plan without extend has a cap: %s", fixed.MaxEpisodeDuration())
	}
	defaulted, err := ParseCampaign(withCapture("  extend: true\n"))
	if err != nil {
		t.Fatal(err)
	}
	if defaulted.MaxEpisodeDuration() != DefaultExtendMaxDuration {
		t.Fatalf("extend without max_duration caps at %s, want the %s default", defaulted.MaxEpisodeDuration(), DefaultExtendMaxDuration)
	}
	// The default never undercuts after_trigger, which a plan may set longer.
	long, err := ParseCampaign([]byte(strings.Replace(string(withCapture("  extend: true\n")), "after_trigger: 5s", "after_trigger: 3m", 1)))
	if err != nil {
		t.Fatal(err)
	}
	if long.MaxEpisodeDuration() != 3*time.Minute {
		t.Fatalf("default cap %s is shorter than after_trigger 3m", long.MaxEpisodeDuration())
	}
	exact, err := ParseCampaign(withCapture("  extend: true\n  max_duration: 5s\n"))
	if err != nil {
		t.Fatalf("a cap equal to after_trigger was rejected: %v", err)
	}
	if exact.MaxEpisodeDuration() != 5*time.Second {
		t.Fatalf("max_duration 5s gives %s", exact.MaxEpisodeDuration())
	}
}

// A plan that does not extend must keep the revision it had before extend
// existed, or every campaign on the fleet would appear edited on upgrade.
func TestCaptureExtendRevision(t *testing.T) {
	revision := func(extra string) string {
		t.Helper()
		campaign, err := ParseCampaign(withCapture(extra))
		if err != nil {
			t.Fatal(err)
		}
		return campaign.Revision
	}
	if got := revision(""); got != drainDigestPinRevision {
		t.Fatalf("a plan without extend hashes to %s, want the pinned %s", got, drainDigestPinRevision)
	}
	if got := revision("  extend: false\n"); got != drainDigestPinRevision {
		t.Fatalf("extend: false is today's behaviour and must hash like an absent key: %s", got)
	}
	extended := revision("  extend: true\n")
	capped := revision("  extend: true\n  max_duration: 30s\n")
	if extended == drainDigestPinRevision || capped == drainDigestPinRevision || extended == capped {
		t.Fatalf("extend and max_duration must change the revision: %s %s %s", drainDigestPinRevision, extended, capped)
	}

	// Neither key is written into a stored plan that does not use it.
	fixed, err := ParseCampaign([]byte(drainDigestPinYAML))
	if err != nil {
		t.Fatal(err)
	}
	stored, err := json.Marshal(fixed)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(stored), "extend") || strings.Contains(string(stored), "max_duration") {
		t.Fatalf("a plan without extend stores extend fields: %s", stored)
	}
}

// Handoff frees the campaign key before the drain and the seal, so the
// follow-up part opens at once, and both parts link to each other.
func TestEpisodeHandoffLinksPartsAndFreesTheKeyFirst(t *testing.T) {
	m, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const key = "fire-watch"
	const drain = 400 * time.Millisecond
	start := func(extension EpisodeExtension) Manifest {
		t.Helper()
		started, err := m.Start(StartOptions{Name: key, Sources: []string{"applications"}, DrainDuration: drain,
			Trigger: EpisodeTrigger{Reason: "event:fire", CampaignName: key}, Extension: extension})
		if err != nil {
			t.Fatal(err)
		}
		return started
	}
	limit := time.Minute.Nanoseconds()
	first := start(EpisodeExtension{Part: 1, MaxDurationNanos: limit})
	if first.Extension.Part != 1 || first.Extension.MaxDurationNanos != limit {
		t.Fatalf("part 1 manifest: %+v", first.Extension)
	}

	m.SetEndReason(key, EndReasonMaxDuration)
	m.SetEndReason(key, EndReasonManualStop) // the first reason names the cause
	handedOff := time.Now()
	handoff, err := m.Handoff(key)
	if err != nil {
		t.Fatal(err)
	}
	if handoff.ID() != first.ID {
		t.Fatalf("handed off %s, want %s", handoff.ID(), first.ID)
	}
	if keys := m.ActiveEpisodeKeys(); len(keys) != 0 {
		t.Fatalf("the key is still held after the handoff: %v", keys)
	}
	second := start(EpisodeExtension{Part: 2, PreviousEpisode: handoff.ID(), MaxDurationNanos: limit})
	if waited := time.Since(handedOff); waited >= drain {
		t.Fatalf("the follow-up waited %s for the previous part's drain", waited)
	}

	sealed, err := handoff.Seal(second.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(handedOff) < drain {
		t.Fatal("Seal did not serve the previous part's drain")
	}
	want := EpisodeExtension{Part: 1, NextEpisode: second.ID, MaxDurationNanos: limit, EndReason: EndReasonMaxDuration}
	if sealed.State != "complete" || sealed.Extension != want {
		t.Fatalf("part 1 sealed as %s with %+v, want complete with %+v", sealed.State, sealed.Extension, want)
	}

	m.SetEndReason(key, EndReasonEventCleared)
	if _, err := m.Stop(key); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]EpisodeExtension{
		first.ID:  want,
		second.ID: {Part: 2, PreviousEpisode: first.ID, MaxDurationNanos: limit, EndReason: EndReasonEventCleared},
	} {
		manifest, _, err := m.Inspect(id, true)
		if err != nil {
			t.Fatal(err)
		}
		if manifest.Extension != want {
			t.Errorf("%s on disk: %+v, want %+v", id, manifest.Extension, want)
		}
	}
}

func TestEpisodeWithoutExtendHasNoExtensionBlock(t *testing.T) {
	m, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	started, err := m.Start(StartOptions{Name: "fixed", Sources: []string{"applications"}, Trigger: EpisodeTrigger{CampaignName: "fixed"}})
	if err != nil {
		t.Fatal(err)
	}
	m.SetEndReason("fixed", EndReasonManualStop)
	if _, err := m.Stop("fixed"); err != nil {
		t.Fatal(err)
	}
	manifest, _, err := m.Inspect(started.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), `"extension"`) {
		t.Fatalf("a fixed-window episode carries an extension block: %s", encoded)
	}
}
