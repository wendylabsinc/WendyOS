package data

import (
	"fmt"
	"math"
	"net"
	"net/url"
	"strings"
	"time"
)

// CampaignThermal evaluates the maximum measured temperature, never image brightness.
// Use separate campaigns for separate sensors so each keeps its own alarm latch.
type CampaignThermal struct {
	Camera            string  `json:"camera,omitempty" yaml:"camera,omitempty"`
	MLX90642URL       string  `json:"mlx90642_url,omitempty" yaml:"mlx90642_url,omitempty"`
	AboveCelsius      float64 `json:"above_celsius" yaml:"above_celsius"`
	ClearBelowCelsius float64 `json:"clear_below_celsius" yaml:"clear_below_celsius"`
	For               string  `json:"for" yaml:"for"`
	Event             string  `json:"event" yaml:"event"`
}

func (t *CampaignThermal) validate(c Campaign) error {
	if t == nil {
		return nil
	}
	if c.Notify != nil {
		return fmt.Errorf("thermal campaigns record locally; configure ThermalNotify separately instead of notify")
	}
	if c.Inference != nil {
		return fmt.Errorf("thermal and inference require separate campaigns")
	}
	if (t.Camera == "") == (t.MLX90642URL == "") {
		return fmt.Errorf("thermal requires exactly one camera or mlx90642_url")
	}
	if t.Camera != "" {
		matched := false
		for _, s := range c.Sources {
			matched = matched || s.Camera == t.Camera
		}
		if !matched || t.Camera == "*" {
			return fmt.Errorf("thermal.camera must match one explicit sources.camera selector")
		}
	}
	if t.MLX90642URL != "" {
		u, err := url.Parse(t.MLX90642URL)
		if err != nil || u.Scheme != "http" || u.User != nil || u.Fragment != "" || !net.ParseIP(u.Hostname()).IsLoopback() {
			return fmt.Errorf("thermal.mlx90642_url must use http and a literal loopback IP, without credentials or fragment")
		}
	}
	if math.IsNaN(t.AboveCelsius) || math.IsInf(t.AboveCelsius, 0) || math.IsNaN(t.ClearBelowCelsius) || math.IsInf(t.ClearBelowCelsius, 0) || t.ClearBelowCelsius < -273.15 || t.AboveCelsius > 1000 || t.ClearBelowCelsius >= t.AboveCelsius {
		return fmt.Errorf("thermal thresholds must be finite, within -273.15..1000 Celsius, and clear_below_celsius < above_celsius")
	}
	d, err := time.ParseDuration(t.For)
	if err != nil || d < time.Second || d > 5*time.Minute {
		return fmt.Errorf("thermal.for must be between 1s and 5m")
	}
	if strings.TrimSpace(t.Event) == "" {
		return fmt.Errorf("thermal.event is required")
	}
	for _, trigger := range c.Capture.Triggers {
		if trigger.Event == t.Event {
			return nil
		}
	}
	return fmt.Errorf("thermal.event must match a capture trigger")
}

// ThermalLatch requires fresh consecutive samples and rearms only after cooling.
// Missing data resets the dwell, never the alarm latch.
type ThermalLatch struct {
	Since, Last time.Time
	Raised      bool
}

func (s *ThermalLatch) Observe(c *CampaignThermal, now time.Time, value float64, valid bool) bool {
	if !valid || math.IsNaN(value) || math.IsInf(value, 0) || value < -273.15 || value > 1000 {
		s.Since = time.Time{}
		return false
	}
	if !s.Last.IsZero() && (now.Sub(s.Last) > 3*time.Second || !now.After(s.Last)) {
		s.Since = time.Time{}
	}
	s.Last = now
	if value < c.ClearBelowCelsius {
		s.Raised = false
	}
	if value <= c.AboveCelsius || s.Raised {
		s.Since = time.Time{}
		return false
	}
	if s.Since.IsZero() {
		s.Since = now
	}
	dwell, _ := time.ParseDuration(c.For)
	if now.Sub(s.Since) < dwell {
		return false
	}
	s.Raised = true
	return true
}
