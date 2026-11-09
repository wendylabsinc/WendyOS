package data

import (
	"math"
	"strings"
	"testing"
	"time"
)

func TestThermalCampaignValidation(t *testing.T) {
	raw := strings.Replace(exampleCampaignYAML, "sources:", "thermal:\n  camera: front\n  above_celsius: 80\n  clear_below_celsius: 75\n  for: 2s\n  event: emergency_stop\nsources:", 1)
	c, err := ParseCampaign([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	changed, err := ParseCampaign([]byte(strings.Replace(raw, "above_celsius: 80", "above_celsius: 81", 1)))
	if err != nil || changed.Revision == c.Revision {
		t.Fatal("thermal threshold omitted from revision")
	}
	if c.Thermal == nil {
		t.Fatal("thermal configuration lost")
	}
	for _, pair := range [][2]string{{"clear_below_celsius: 75", "clear_below_celsius: 80"}, {"for: 2s", "for: 0s"}, {"camera: front\n  above", "camera: missing\n  above"}, {"above_celsius: 80", "above_celsius: .nan"}, {"thermal:\n", "thermal:\n  unexpected: true\n"}, {"camera: front\n  above", "mlx90642_url: http://example.com/frame\n  above"}} {
		if _, err := ParseCampaign([]byte(strings.Replace(raw, pair[0], pair[1], 1))); err == nil {
			t.Fatalf("accepted invalid %v", pair)
		}
	}
	local := strings.Replace(raw, "camera: front\n  above", "mlx90642_url: http://127.0.0.1:8080/api/frame\n  above", 1)
	if _, err := ParseCampaign([]byte(local)); err != nil {
		t.Fatal(err)
	}
}

func TestThermalLatchDwellCoolingAndMissingSamples(t *testing.T) {
	c := &CampaignThermal{AboveCelsius: 80, ClearBelowCelsius: 75, For: "2s"}
	var s ThermalLatch
	start := time.Unix(100, 0)
	observe := func(sec int, v float64, valid, want bool) {
		t.Helper()
		if got := s.Observe(c, start.Add(time.Duration(sec)*time.Second), v, valid); got != want {
			t.Fatalf("second %d: fired=%v want=%v", sec, got, want)
		}
	}
	observe(0, 81, true, false)
	observe(1, 81, true, false)
	observe(2, 81, true, true)
	observe(3, 100, true, false)
	observe(10, 100, true, false) // missing data cannot rearm
	observe(11, 75, true, false)
	observe(12, 81, true, false) // exactly reset threshold does not clear
	observe(13, 74, true, false)
	observe(14, 81, true, false)
	observe(15, math.NaN(), true, false)
	observe(16, 81, true, false)
	observe(17, 81, true, false)
	observe(18, 81, true, true)
	observe(19, 74, true, false)
	observe(20, 81, true, false)
	observe(25, 81, true, false) // stale gap breaks dwell
	observe(26, 81, true, false)
	observe(27, 81, true, true)
}
