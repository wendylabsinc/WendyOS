package data

import (
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// leasedCampaignYAML is the shape a chat watch deploys (spec §5.1).
const leasedCampaignYAML = `version: 1
name: chat-3fa91c0e-1
lease: 60s
sources:
  - camera: /dev/video0
inference:
  model: PekingU/rtdetr_r18vd
  revision: ac77a11ff0170a41b771c03264987f8ce2b0d753
  labels: [person]
  threshold: 0.5
  rate: 2
  event: chat-3fa91c0e-1.detected
  clear_after: 5s
  cooldown: 30s
notify:
  on: detection
`

func TestLeasedCampaignParses(t *testing.T) {
	campaign, err := ParseCampaign([]byte(leasedCampaignYAML))
	if err != nil {
		t.Fatal(err)
	}
	if !campaign.Leased() || campaign.LeaseDuration() != 60*time.Second {
		t.Fatalf("lease not parsed: %q", campaign.Lease)
	}
	for _, lease := range []string{"15s", "10m"} {
		raw := strings.Replace(leasedCampaignYAML, "lease: 60s", "lease: "+lease, 1)
		if _, err := ParseCampaign([]byte(raw)); err != nil {
			t.Fatalf("lease %s is in range but was rejected: %v", lease, err)
		}
	}
}

func TestLeasedCampaignValidation(t *testing.T) {
	cases := []struct{ name, old, new, want string }{
		{"lease below minimum", "lease: 60s", "lease: 14s", "lease must be a duration from 15s through 10m0s"},
		{"lease above maximum", "lease: 60s", "lease: 10m1s", "lease must be a duration from 15s through 10m0s"},
		{"lease without unit", "lease: 60s", `lease: "60"`, "lease must be a duration from 15s through 10m0s"},
		{"audio source", "  - camera: /dev/video0\n", "  - camera: /dev/video0\n  - audio: default\n", "sources[1]: a leased campaign selects cameras only"},
		{"source capture", "  - camera: /dev/video0\n", "  - camera: /dev/video0\n    capture: {mode: continuous}\n", "sources[0]: a leased campaign records nothing, so it takes no capture or calibration_revision"},
		{"capture block", "notify:\n", "capture: {buffer: 1s, after_trigger: 1s, triggers: [{event: go}]}\nnotify:\n", "a leased campaign records nothing, so it takes no capture"},
		{"upload block", "notify:\n", "upload: {when: wifi}\nnotify:\n", "a leased campaign records nothing, so it takes no upload"},
		{"retention block", "notify:\n", "retention: {local_quota: 1GiB}\nnotify:\n", "a leased campaign records nothing, so it takes no retention"},
		{"export block", "notify:\n", "export: {annotation: cvat}\nnotify:\n", "a leased campaign records nothing, so it takes no export"},
		{"models", "notify:\n", "models: {detector: v1}\nnotify:\n", "a leased campaign records nothing, so it takes no models"},
		{"privacy", "notify:\n", "privacy: [{name: blur}]\nnotify:\n", "a leased campaign records nothing, so it takes no privacy"},
		{"inference disabled", "  cooldown: 30s\n", "  cooldown: 30s\n  enabled: false\n", "a leased campaign needs an enabled inference block"},
		{"notify episode_committed", "  on: detection", "  on: episode_committed", "a leased campaign needs notify.on: detection"},
		{"notify event", "  on: detection", "  on: event\n  event: go", "a leased campaign needs notify.on: detection"},
		{"webhook", "  on: detection\n", "  on: detection\n  webhook: https://example.com/hook\n", "remove notify.webhook"},
		{"unknown notify key", "  on: detection\n", "  on: detection\n  channel: x\n", "unknown immediate notification fields: channel"},
		{"inference still validated", "  rate: 2\n", "  rate: 0\n", "inference.rate must be in (0, 30]"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := strings.Replace(leasedCampaignYAML, tc.old, tc.new, 1)
			if raw == leasedCampaignYAML {
				t.Fatalf("replacement %q not found in the fixture", tc.old)
			}
			_, err := ParseCampaign([]byte(raw))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want an error containing %q", err, tc.want)
			}
		})
	}
}

// A client may build the YAML by marshalling a Campaign, which writes the
// empty capture, upload and export blocks out. Those count as absent.
func TestLeasedCampaignRoundTripsThroughYAML(t *testing.T) {
	campaign, err := ParseCampaign([]byte(leasedCampaignYAML))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := yaml.Marshal(campaign)
	if err != nil {
		t.Fatal(err)
	}
	again, err := ParseCampaign(raw)
	if err != nil {
		t.Fatalf("a marshalled leased campaign no longer parses: %v\n%s", err, raw)
	}
	if again.Revision != campaign.Revision {
		t.Fatal("round trip changed the revision")
	}
}

func TestLeaseChangesRevisionOnlyWhenSet(t *testing.T) {
	short, err := ParseCampaign([]byte(leasedCampaignYAML))
	if err != nil {
		t.Fatal(err)
	}
	long, err := ParseCampaign([]byte(strings.Replace(leasedCampaignYAML, "lease: 60s", "lease: 120s", 1)))
	if err != nil {
		t.Fatal(err)
	}
	if short.Revision == long.Revision {
		t.Fatal("changing the lease kept the revision")
	}
	people, err := ParseCampaign(peopleCampaign(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := people.planDigestInput()["lease"]; ok {
		t.Fatal("a campaign without a lease hashes a lease key, which would change every deployed revision")
	}
}
