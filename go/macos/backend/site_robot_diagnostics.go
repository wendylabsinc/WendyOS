package backend

import (
	"encoding/json"
	"github.com/wendylabsinc/wendy/go/macos/backend/sitenav"
	"math"
	"os"
	"path/filepath"
	"time"
)

// Opt-in, local-only replay of one bounded localization attempt. This contains
// room geometry, so it is disabled unless explicitly enabled for diagnosis.
func (r *SiteRobot) recordLocalizationEvidence(request SiteFusionRequest, input sitenav.RegistrationInput, report SiteFusionReport, now time.Time) {
	if os.Getenv("WENDY_LOCALIZATION_DIAGNOSTICS") != "1" || now.Sub(r.lastLocalizationLog) < 5*time.Second {
		return
	}
	r.lastLocalizationLog = now
	data, err := json.Marshal(struct {
		At      time.Time
		Request SiteFusionRequest
		Input   sitenav.RegistrationInput
		Report  SiteFusionReport
	}{now, request, input, report})
	if err != nil {
		return
	}
	dir, err := os.UserCacheDir()
	if err != nil {
		return
	}
	dir = filepath.Join(dir, "Wendy")
	if os.MkdirAll(dir, 0700) != nil {
		return
	}
	path := filepath.Join(dir, "go2-localization-evidence.json")
	if os.WriteFile(path+".tmp", data, 0600) == nil {
		_ = os.Rename(path+".tmp", path)
	}
}

// Keep the latest rejection locally for device support. No raw sensor data,
// robot identifier or authentication data is recorded. Limit disk writes to
// one per second even when a broken stream rejects every packet.
func (r *SiteRobot) recordObservationFault(topic string, err error, received, now time.Time) {
	if now.Sub(r.lastObservationLog) < time.Second {
		return
	}
	r.lastObservationLog = now
	dir, e := os.UserCacheDir()
	if e != nil {
		return
	}
	data, e := json.Marshal(struct {
		At            time.Time `json:"at"`
		Topic         string    `json:"topic"`
		Error         string    `json:"error"`
		ClockOffsetMS float64   `json:"clockOffsetMS"`
		GatewayAgeMS  float64   `json:"gatewayAgeMS"`
		PoseAgeMS     float64   `json:"poseAgeMS"`
		CaptureAgeMS  float64   `json:"captureAgeMS"`
	}{now, topic, err.Error(), r.observed.ClockOffset.Seconds() * 1000, now.Sub(received).Seconds() * 1000, now.Sub(r.observed.Pose.Received).Seconds() * 1000, now.Sub(r.observed.Pose.Captured).Seconds() * 1000})
	if e != nil {
		return
	}
	dir = filepath.Join(dir, "Wendy")
	if os.MkdirAll(dir, 0700) != nil {
		return
	}
	path := filepath.Join(dir, "go2-observation-fault.json")
	if os.WriteFile(path+".tmp", data, 0600) == nil {
		_ = os.Rename(path+".tmp", path)
	}
}

// Keep one bounded local scan around the body to explain false turn blockers.
// This does not initiate movement or change the observed collision grid.
func (r *SiteRobot) recordNavigationEvidence(now time.Time) {
	if now.Sub(r.lastNavigationLog) < time.Second {
		return
	}
	r.lastNavigationLog = now
	p := r.observed.Pose
	type cellEvidence struct {
		X, Y           float64
		Floor, Blocked bool
	}
	var cells []cellEvidence
	for y := -10; y <= 10; y++ {
		for x := -10; x <= 10; x++ {
			c := sitenav.Cell{X: int(math.Floor(p.Position.X/sitenav.CellSize)) + x, Y: int(math.Floor(p.Position.Y/sitenav.CellSize)) + y}
			if r.observed.Grid.Floor[c] || r.observed.Grid.Obstacles[c] {
				cells = append(cells, cellEvidence{(float64(c.X) + .5) * sitenav.CellSize, (float64(c.Y) + .5) * sitenav.CellSize, r.observed.Grid.Floor[c], r.observed.Grid.Obstacles[c]})
			}
		}
	}
	data, err := json.Marshal(struct {
		At                   time.Time
		Pose                 sitenav.Pose
		FloorHeight          float64
		AllowUnobservedFloor bool
		Cells                []cellEvidence
		Samples              []sitenav.Vertex
	}{now, p, r.observed.FloorHeight, r.allowUnobservedFloor, cells, r.observed.LocalNavigationSamples})
	if err != nil {
		return
	}
	dir, err := os.UserCacheDir()
	if err != nil {
		return
	}
	dir = filepath.Join(dir, "Wendy")
	if os.MkdirAll(dir, 0700) != nil {
		return
	}
	path := filepath.Join(dir, "go2-navigation-evidence.json")
	if os.WriteFile(path+".tmp", data, 0600) == nil {
		_ = os.Rename(path+".tmp", path)
	}
}
