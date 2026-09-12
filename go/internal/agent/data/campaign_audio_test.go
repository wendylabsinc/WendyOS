package data

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
)

const audioThresholdCampaign = `version: 1
name: cabin-audio
sources:
  - audio: cabin-mic
    capture:
      mode: threshold
      trigger: "level_db > -20"
      fragment: 30s
capture:
  buffer: 0s
  after_trigger: 20s
  triggers:
    - event: loud_event
upload:
  when: wifi
export:
  annotation: cvat
`

// TestAudioThresholdCampaignParsesAndDeploysWithoutModeWarning proves the audio
// source kind and threshold capture mode are first-class: the plan parses and
// deployment does not warn that the mode is unimplemented for that source.
func TestAudioThresholdCampaignParsesAndDeploysWithoutModeWarning(t *testing.T) {
	campaign, err := ParseCampaign([]byte(audioThresholdCampaign))
	if err != nil {
		t.Fatalf("audio threshold campaign did not parse: %v", err)
	}
	if len(campaign.Sources) != 1 || campaign.Sources[0].Audio != "cabin-mic" {
		t.Fatalf("expected one audio source, got %+v", campaign.Sources)
	}
	if campaign.Sources[0].kind() != "audio" {
		t.Fatalf("source kind = %q, want audio", campaign.Sources[0].kind())
	}
	if campaign.Sources[0].Capture.EffectiveMode() != "threshold" {
		t.Fatalf("capture mode = %q, want threshold", campaign.Sources[0].Capture.EffectiveMode())
	}

	manager, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// Deploy now resolves audio selectors, so the campaign needs a healthy
	// source to name; see TestDeployRefusesUnhealthyAudioSource.
	manager.SetSourceProvider(func(context.Context) []Source {
		return []Source{{ID: "audio:1", Kind: "audio", ClockDomain: "ALSA_CAPTURE/AGENT_RECEIPT", Healthy: true, Detail: "cabin-mic USB Audio plughw:0,0"}}
	})
	deployed, err := manager.DeployCampaign([]byte(audioThresholdCampaign))
	if err != nil {
		t.Fatalf("deploy: %v", err)
	}
	for _, w := range deployed.Warnings {
		if strings.Contains(w, "not implemented yet for their source kind") {
			t.Fatalf("audio threshold should be implemented, but deploy warned: %q", w)
		}
	}
}

// TestShippedAudioCampaignParses guards the real
// Examples/WendyDataCampaign/campaign.yaml, which now enables an audio
// threshold source, against the schema this agent parses.
func TestShippedAudioCampaignParses(t *testing.T) {
	raw, err := os.ReadFile("../../../../Examples/WendyDataCampaign/campaign.yaml")
	if err != nil {
		t.Fatalf("reading campaign example: %v", err)
	}
	campaign, err := ParseCampaign(raw)
	if err != nil {
		t.Fatalf("WendyDataCampaign/campaign.yaml does not parse: %v", err)
	}
	var audio *CampaignSource
	for i := range campaign.Sources {
		if campaign.Sources[i].Audio != "" {
			audio = &campaign.Sources[i]
			break
		}
	}
	if audio == nil {
		t.Fatal("expected an enabled audio source in the shipped campaign")
	}
	if audio.Capture == nil || audio.Capture.EffectiveMode() != "threshold" {
		t.Fatalf("audio source is not in threshold mode: %+v", audio.Capture)
	}
	if _, _, _, err := ParseFieldThreshold(audio.Capture.Trigger); err != nil {
		t.Fatalf("audio trigger %q does not parse: %v", audio.Capture.Trigger, err)
	}
}

// admaifSource is one of the twenty Tegra audio-hub DMA endpoints a Jetson Orin
// Nano advertises, with the detail the audio adapter now attaches. The
// description is verbatim from wendyos-hubert.local.
func admaifSource() Source {
	return Source{
		ID:          "audio:16777729",
		Kind:        "audio",
		ClockDomain: "ALSA_CAPTURE/AGENT_RECEIPT",
		Healthy:     false,
		Detail: "APE [NVIDIA Jetson Orin Nano APE], device 0: fe.admaif@290f000.ADMAIF1 (*) [] plughw:2,0; " +
			"audio hub DMA endpoint, not a physical input: captures digital silence unless an external I2S codec is wired and the audio hub is routed to it",
	}
}

const admaifCampaign = `version: 1
name: cabin-audio
sources:
  - audio: ADMAIF1
capture:
  buffer: 0s
  after_trigger: 20s
  triggers:
    - event: loud_event
upload:
  when: wifi
export:
  annotation: cvat
`

// TestDeployRefusesUnhealthyAudioSource proves a campaign pointed at an
// endpoint that streams digital silence is refused while an operator is still
// there to read why, rather than deploying cleanly and sealing empty episodes.
func TestDeployRefusesUnhealthyAudioSource(t *testing.T) {
	manager, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	manager.SetSourceProvider(func(context.Context) []Source {
		return []Source{admaifSource()}
	})

	_, err = manager.DeployCampaign([]byte(admaifCampaign))
	if err == nil {
		t.Fatal("deploy accepted a campaign whose only audio source records silence")
	}
	// The refusal has to name the source and the reason, or the operator has
	// nothing to act on.
	for _, want := range []string{"ADMAIF1", "audio:16777729", "audio hub DMA endpoint"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("deploy error %q does not mention %q", err, want)
		}
	}
	if _, err := manager.Campaign("cabin-audio"); err == nil {
		t.Error("refused campaign was still persisted")
	}
}

// TestResolveRefusesUnhealthyAudioSource covers the trigger-time half: even if
// a campaign predates the deploy check, resolution must not select an unhealthy
// audio source.
func TestResolveRefusesUnhealthyAudioSource(t *testing.T) {
	manager, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	manager.SetSourceProvider(func(context.Context) []Source {
		return []Source{admaifSource()}
	})
	campaign, err := ParseCampaign([]byte(admaifCampaign))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := manager.ResolveCampaignSources(campaign); err == nil {
		t.Fatal("resolution selected an audio source that records silence")
	}
}

// TestDeployResolvesDefaultAudioPastTheHubEndpoints is the payoff on a real
// Jetson: with the twenty audio-hub endpoints reporting unhealthy, exactly one
// healthy audio source is left, so "default" is no longer ambiguous and lands
// on the actual microphone.
func TestDeployResolvesDefaultAudioPastTheHubEndpoints(t *testing.T) {
	manager, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c920 := Source{ID: "audio:16777217", Kind: "audio", ClockDomain: "ALSA_CAPTURE/AGENT_RECEIPT", Healthy: true,
		Detail: "C920 [HD Pro Webcam C920], device 0: USB Audio [USB Audio] plughw:0,0"}
	manager.SetSourceProvider(func(context.Context) []Source {
		sources := []Source{c920}
		for i := 0; i < 20; i++ {
			admaif := admaifSource()
			admaif.ID = fmt.Sprintf("audio:%d", 16777729+i)
			sources = append(sources, admaif)
		}
		return sources
	})

	defaultCampaign := strings.Replace(admaifCampaign, "audio: ADMAIF1", "audio: default", 1)
	if _, err := manager.DeployCampaign([]byte(defaultCampaign)); err != nil {
		t.Fatalf("deploy of a default audio selector failed: %v", err)
	}
	campaign, err := ParseCampaign([]byte(defaultCampaign))
	if err != nil {
		t.Fatal(err)
	}
	ids, _, _, err := manager.ResolveCampaignSources(campaign)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	found := false
	for _, id := range ids {
		if id == c920.ID {
			found = true
		}
		if strings.HasPrefix(id, "audio:") && id != c920.ID {
			t.Errorf("resolution selected an audio hub endpoint %s", id)
		}
	}
	if !found {
		t.Errorf("default audio did not resolve to the real microphone; got %v", ids)
	}
}
