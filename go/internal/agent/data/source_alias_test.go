package data

import (
	"context"
	"strings"
	"sync"
	"testing"
)

const stableC920 = "audio:alsa_input.usb-046d_HD_Pro_Webcam_C920_C2DD0F9F-02.analog-stereo"

// mesh1Sources is what Mesh 1's agent lists with stable microphone IDs, each
// microphone still carrying the number it was listed under on this boot.
func mesh1Sources() []Source {
	return []Source{
		{ID: stableC920, Kind: "audio", ClockDomain: "PIPEWIRE_CAPTURE/AGENT_RECEIPT", Healthy: true,
			Detail: "C920 PRO HD Webcam Analog Stereo alsa_input.usb-046d_HD_Pro_Webcam_C920_C2DD0F9F-02.analog-stereo", Aliases: []string{"audio:54"}},
		{ID: "audio:alsa_input.platform-sound.analog-stereo", Kind: "audio", ClockDomain: "PIPEWIRE_CAPTURE/AGENT_RECEIPT", Healthy: true,
			Detail: "Built-in Audio Analog Stereo alsa_input.platform-sound.analog-stereo", Aliases: []string{"audio:56"}},
	}
}

func TestIsLegacyAudioSourceID(t *testing.T) {
	for id, want := range map[string]bool{
		"audio:54":                               true,
		"audio:16777217":                         true,
		"audio:":                                 false,
		"audio:54a":                              false,
		"audio:alsa:C920:0":                      false,
		"audio:sensor-link:esp32-cam:microphone": false,
		stableC920:                               false,
		"ipcamera:200":                           false,
		"54":                                     false,
	} {
		if got := IsLegacyAudioSourceID(id); got != want {
			t.Errorf("IsLegacyAudioSourceID(%q) = %v, want %v", id, got, want)
		}
	}
}

func TestResolveSourceAliases(t *testing.T) {
	all := mesh1Sources()
	got, notes := resolveSourceAliases(all, []string{"audio:54", "telemetry", stableC920, "audio:99"})
	want := []string{stableC920, "telemetry", stableC920, "audio:99"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("resolved %q, want %q", got, want)
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "audio:54") || !strings.Contains(notes[0], stableC920) || !strings.Contains(notes[0], "deprecated") {
		t.Fatalf("notes = %q, want one deprecation naming both IDs", notes)
	}

	// An identifier that is some source's own ID is never rewritten, even if
	// another source lists it as an alias.
	clash := append(mesh1Sources(), Source{ID: "audio:56", Kind: "audio", Healthy: true})
	if got, _ := resolveSourceAliases(clash, []string{"audio:56"}); got[0] != "audio:56" {
		t.Fatalf("an exact ID was rewritten through an alias: %q", got)
	}
}

// TestRecordedByDefaultIsSelectSourcesDefault ties the exported rule the CLI
// marks sources with to what an episode without --source actually selects.
func TestRecordedByDefaultIsSelectSourcesDefault(t *testing.T) {
	all := append(mesh1Sources(),
		Source{ID: "ipcamera:200", Kind: "camera", Healthy: false, UnusableReason: "no login stored"},
		Source{ID: "telemetry", Kind: "telemetry", Healthy: true},
	)
	selected, err := selectSources(all, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	chosen := map[string]bool{}
	for _, s := range selected {
		chosen[s.ID] = true
	}
	for _, s := range all {
		if chosen[s.ID] != RecordedByDefault(s) {
			t.Errorf("%s: selected by default = %v, RecordedByDefault = %v", s.ID, chosen[s.ID], RecordedByDefault(s))
		}
	}
}

func TestUnknownLegacyMicrophoneIDSaysWhatChanged(t *testing.T) {
	_, err := selectSources(mesh1Sources(), []string{"audio:99"}, nil)
	if err == nil {
		t.Fatal("an unknown source was selected")
	}
	for _, want := range []string{"unknown or unhealthy source(s): audio:99", "old-style microphone ID", "wendy data sources --kind audio"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
	if _, err := selectSources(mesh1Sources(), []string{"v4l2:/dev/video9"}, nil); err == nil || strings.Contains(err.Error(), "microphone") {
		t.Errorf("a non-audio miss got the microphone hint: %v", err)
	}
}

// TestRecordWithLegacyMicrophoneID is `wendy data record --source audio:54`
// against an agent with stable IDs: the episode records the stable ID, the
// calibration and exclusion typed the old way follow it, and the agent log says
// the old form is deprecated.
func TestRecordWithLegacyMicrophoneID(t *testing.T) {
	m, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var warnings []string
	m.SetWarnLogger(func(s string) { mu.Lock(); warnings = append(warnings, s); mu.Unlock() })
	m.SetSourceProvider(func(context.Context) []Source { return mesh1Sources() })

	started, err := m.Start(StartOptions{
		Sources:      []string{"applications", "audio:54"},
		Calibrations: map[string][]byte{"audio:54": []byte("gain: 1\n")},
	})
	if err != nil {
		t.Fatalf("start with the old microphone ID: %v", err)
	}
	var ids []string
	for _, s := range started.Sources {
		ids = append(ids, s.Source.ID)
	}
	if strings.Join(ids, " ") != "applications "+stableC920 {
		t.Fatalf("episode sources = %q, want applications and the C920's stable ID", ids)
	}
	if len(started.Calibrations) != 1 || started.Calibrations[0].Source != stableC920 {
		t.Fatalf("calibrations = %+v, want the calibration filed under the stable ID", started.Calibrations)
	}
	if _, err := m.Stop(AdHocEpisodeKey); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	logged := strings.Join(warnings, "\n")
	mu.Unlock()
	if strings.Count(logged, "audio:54 is deprecated and resolved to "+stableC920) != 2 {
		t.Errorf("warnings = %q, want one deprecation for --source and one for --calibration", logged)
	}

	// --exclude-source takes the old form too.
	started, err = m.Start(StartOptions{ExcludeSources: []string{"audio:54"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range started.Sources {
		if s.Source.ID == stableC920 {
			t.Fatalf("--exclude-source audio:54 did not exclude the C920: %+v", started.Sources)
		}
	}
	if _, err := m.Stop(AdHocEpisodeKey); err != nil {
		t.Fatal(err)
	}
}

const legacyAudioCampaign = `version: 1
name: legacy-mic
sources:
  - audio: "audio:54"
capture:
  buffer: 0s
  after_trigger: 5s
  triggers:
    - event: loud_event
upload:
  when: wifi
export:
  annotation: cvat
`

// TestCampaignWithLegacyMicrophoneID deploys a plan written against the old
// IDs: it still deploys and resolves to the stable ID, with a deploy warning
// that names the replacement.
func TestCampaignWithLegacyMicrophoneID(t *testing.T) {
	m, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m.SetSourceProvider(func(context.Context) []Source { return mesh1Sources() })
	deployed, err := m.DeployCampaign([]byte(legacyAudioCampaign))
	if err != nil {
		t.Fatalf("deploy: %v", err)
	}
	found := false
	for _, w := range deployed.Warnings {
		if strings.Contains(w, `"audio:54" is a deprecated microphone ID`) && strings.Contains(w, "write audio: "+stableC920) {
			found = true
		}
	}
	if !found {
		t.Fatalf("warnings = %q, want one naming the stable replacement", deployed.Warnings)
	}
	ids, _, _, err := m.ResolveCampaignSources(deployed)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(ids, " ") != "applications "+stableC920 {
		t.Fatalf("resolved %q, want applications and the C920's stable ID", ids)
	}

	// The same plan on a device where that number names nothing is refused,
	// and the refusal says the ID scheme moved.
	m.SetSourceProvider(func(context.Context) []Source {
		sources := mesh1Sources()
		sources[0].Aliases = []string{"audio:61"}
		return sources
	})
	if _, err := m.DeployCampaign([]byte(legacyAudioCampaign)); err == nil || !strings.Contains(err.Error(), "old-style microphone ID") {
		t.Fatalf("deploy err = %v, want a refusal that explains the old ID", err)
	}
}

func TestStableMicrophoneIDSelectsWithoutWarning(t *testing.T) {
	m, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m.SetSourceProvider(func(context.Context) []Source { return mesh1Sources() })
	plan := strings.Replace(legacyAudioCampaign, `"audio:54"`, stableC920, 1)
	deployed, err := m.DeployCampaign([]byte(plan))
	if err != nil {
		t.Fatalf("deploy: %v", err)
	}
	for _, w := range deployed.Warnings {
		if strings.Contains(w, "deprecated") {
			t.Fatalf("a stable ID drew a deprecation warning: %q", w)
		}
	}
	// A name fragment still resolves, as it always did.
	if id, err := resolveKindSelector(mesh1Sources(), "audio", "C920"); err != nil || id != stableC920 {
		t.Fatalf("resolveKindSelector(C920) = %q, %v", id, err)
	}
}
