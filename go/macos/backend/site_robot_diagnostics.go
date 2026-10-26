package backend

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

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
