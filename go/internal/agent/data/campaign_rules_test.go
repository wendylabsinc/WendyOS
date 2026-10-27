package data

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func thermalRulesExample(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile("../../../../Examples/WendyDataThermalRules/campaign.yaml")
	if err != nil {
		t.Fatalf("reading thermal rules example: %v", err)
	}
	return raw
}

// The shipped example parses, its rule compiles, and the rule's alarm is what
// starts its recording and its notification.
func TestShippedThermalRulesCampaignParses(t *testing.T) {
	campaign, err := ParseCampaign(thermalRulesExample(t))
	if err != nil {
		t.Fatalf("thermal rules campaign.yaml does not parse: %v", err)
	}
	if len(campaign.Rules) != 1 {
		t.Fatalf("rules %+v", campaign.Rules)
	}
	rule := campaign.Rules[0]
	if rule.Name != "hotspot-alarm" || rule.Input != RuleInputPrefix+"v4l2:/dev/video2" || len(rule.Steps) != 3 || len(rule.Outputs.Events) != 2 {
		t.Fatalf("rule %+v", rule)
	}
	if _, err := rule.Compile(); err != nil {
		t.Fatal(err)
	}
	if _, _, matched := campaign.Match(ApplicationRecord{Version: 1, Type: "event", Name: "thermal_alarm"}); !matched {
		t.Error("thermal_alarm does not start the recording")
	}
	if _, _, matched := campaign.Match(ApplicationRecord{Version: 1, Type: "event", Name: "thermal_warning"}); matched {
		t.Error("thermal_warning starts a recording; the example records alarms only")
	}
	if campaign.Notify == nil || campaign.Notify.On != NotifyOnEvent || campaign.Notify.Event != "thermal_alarm" {
		t.Errorf("notify %+v", campaign.Notify)
	}
	if campaign.BufferDuration().String() != "10s" || campaign.AfterTriggerDuration().String() != "30s" {
		t.Errorf("capture %+v", campaign.Capture)
	}
}

// Deployment refuses a bad rule with a message that names the rule and the
// field, before anything is stored.
func TestCampaignRefusesBadRules(t *testing.T) {
	base := string(thermalRulesExample(t))
	for _, tc := range []struct{ name, from, to, want string }{
		{"app stream input", "input: agent/camera/grid/v4l2:/dev/video2", "input: apps/thermal/grid",
			"rules[0] (hotspot-alarm): input must name one of the agent's camera grids, agent/camera/grid/<source>"},
		{"input without a source", "input: agent/camera/grid/v4l2:/dev/video2", "input: agent/camera/grid/",
			"input must name one of the agent's camera grids"},
		{"overlapping bands", "{name: warm, min: 40, max: 50,", "{name: warm, min: 40, max: 55,",
			"rules[0] (hotspot-alarm): steps[1].bands[1] (hot) overlaps band warm"},
		{"unknown band", "when: area.band == hot,", "when: area.band == red,",
			"rules[0] (hotspot-alarm): outputs.events[1]: thermal_alarm: when \"area.band == red\": no band is named red; the bands are warm, hot"},
		{"upside-down hysteresis", "clear_below: 48", "clear_below: 52",
			"clear_below 52 is above band hot's min, 50"},
		{"typo in a step", "min_cells: 4", "min_cell: 4", "field min_cell not found"},
		{"typo in an event", "cooldown: 2m}", "cool_down: 2m}", "field cool_down not found"},
		{"bad duration", "for: 2s,", "for: 2 seconds,", "for must be a duration from 0s to 24h"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !strings.Contains(base, tc.from) {
				t.Fatalf("example has no %q", tc.from)
			}
			_, err := ParseCampaign([]byte(strings.Replace(base, tc.from, tc.to, 1)))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %v; want it to contain %q", err, tc.want)
			}
		})
	}

	twice := strings.Replace(base, "rules:\n", "rules:\n  - {name: hotspot-alarm, input: agent/camera/grid/v4l2:/dev/video3, steps: [{bands: [{name: hot, min: 50}]}], outputs: {markers: areas}}\n", 1)
	if _, err := ParseCampaign([]byte(twice)); err == nil || !strings.Contains(err.Error(), "rules[1] (hotspot-alarm): another rule is named hotspot-alarm") {
		t.Fatalf("duplicate rule names: %v", err)
	}
}

// A plan without rules digests exactly as before rules existed, so no deployed
// campaign changes revision; a plan with rules digests them, so any change to
// a rule is a new revision.
func TestRulesAndTheRevision(t *testing.T) {
	base := string(thermalRulesExample(t))
	withRules, err := ParseCampaign([]byte(base))
	if err != nil {
		t.Fatal(err)
	}
	i := strings.Index(base, "rules:\n")
	j := strings.Index(base, "capture:\n")
	without, err := ParseCampaign([]byte(base[:i] + base[j:]))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := without.planDigestInput()["rules"]; ok {
		t.Fatal("a plan without rules digests a rules key")
	}
	if without.Revision == withRules.Revision {
		t.Fatal("rules do not change the revision")
	}
	for _, change := range [][2]string{{"color: red", "color: orange"}, {"min_cells: 4", "min_cells: 5"}, {"cooldown: 2m", "cooldown: 3m"}, {"width: 1,", "width: 0.5,"}} {
		changed, err := ParseCampaign([]byte(strings.Replace(base, change[0], change[1], 1)))
		if err != nil {
			t.Fatal(err)
		}
		if changed.Revision == withRules.Revision {
			t.Errorf("%s -> %s kept the revision", change[0], change[1])
		}
	}
	again, err := ParseCampaign([]byte(base))
	if err != nil || again.Revision != withRules.Revision {
		t.Fatalf("the same plan changed revision: %v", err)
	}
}

// Deploying stores the rules and reads them back as written; their status is
// not stored.
func TestDeployedRulesRoundTrip(t *testing.T) {
	m, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	deployed, err := m.DeployCampaign(thermalRulesExample(t))
	if err != nil {
		t.Fatal(err)
	}
	stored, err := m.Campaign(deployed.Name)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored.Rules) != 1 || stored.RulesStatus != nil || stored.Revision != deployed.Revision {
		t.Fatalf("stored %+v", stored)
	}
	got, want := stored.Rules[0].DigestInput(), deployed.Rules[0].DigestInput()
	if !equalJSON(t, got, want) {
		t.Fatalf("rule changed on the way through the store:\n%v\n%v", got, want)
	}
	if _, err := stored.Rules[0].Compile(); err != nil {
		t.Fatal(err)
	}
}

func equalJSON(t *testing.T, a, b any) bool {
	t.Helper()
	x, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	y, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	return string(x) == string(y)
}
