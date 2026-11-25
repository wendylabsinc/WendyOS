package patrol

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestLocalizationDiagnosticsSurviveMissionPublication(t *testing.T) {
	raw := []byte(`{"ready":false,"localization_health":{"schema":"wendy.nav.localization-health.v1","ready":false,"reasons":["covariance_high","pose_stale"],"sensor_age_s":0.01,"pose_age_s":null,"costmap_age_s":0.2,"variance":[0.03,0.01,0.04],"variance_limit":0.0225,"passive":{"attempts":3,"pending":true,"last_scan_stamp":18.4,"last_attempt_age_s":0.3},"motion_resumed":false}}`)
	var n Navigation
	if err := json.Unmarshal(raw, &n); err != nil {
		t.Fatal(err)
	}
	published, err := json.Marshal(map[string]any{"navigation": n})
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Navigation Navigation `json:"navigation"`
	}
	if err := json.Unmarshal(published, &result); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(n.LocalizationHealth, result.Navigation.LocalizationHealth) || result.Navigation.LocalizationHealth.PoseAge != nil {
		t.Fatal("publication lost localization evidence or converted unknown age to fresh")
	}
}

func TestRecoveredLocalizationDiagnosticsNeverResumeHeldMission(t *testing.T) {
	for _, returning := range []bool{false, true} {
		e, f, _, held := progressRig(t, returning)
		n := nav(f)
		n.Ready = false
		if err := e.Tick(1100, n); err != nil {
			t.Fatal(err)
		}
		if e.State.State != held {
			t.Fatal(e.State)
		}
		n.Ready = true
		n.LocalizationHealth = &LocalizationHealth{Schema: "wendy.nav.localization-health.v1", Ready: true}
		goal, canceled := f.goal, f.canceled
		for _, now := range []int64{1200, 4000, 9000} {
			if err := e.Tick(now, n); err != nil {
				t.Fatal(err)
			}
			if e.State.State != held || f.goal != goal || f.canceled != canceled || f.captures != 0 {
				t.Fatal("observation recovery changed mission authority", e.State)
			}
		}
	}
}
