package sitenav

import (
	"encoding/json"
	"math"
	"testing"
	"time"
)

func TestObservedPoseRetainsRollPitch(t *testing.T) {
	now := time.Now()
	for _, axis := range []string{"roll", "pitch"} {
		t.Run(axis, func(t *testing.T) {
			q := []float64{math.Cos(.05), 0, 0, 0}
			if axis == "roll" {
				q[1] = math.Sin(.05)
			} else {
				q[2] = math.Sin(.05)
			}
			raw, _ := json.Marshal(map[string]any{"topic": PoseTopic, "data": map[string]any{"position": []float64{0, 0, .3}, "imu_state": map[string]any{"quaternion": q}, "stamp": map[string]any{"sec": now.Unix(), "nanosec": now.Nanosecond()}}})
			var observed Observations
			if err := observed.Receive(PoseTopic, raw, false, now, now); err != nil {
				t.Fatal(err)
			}
			got := observed.Pose.Roll
			if axis == "pitch" {
				got = observed.Pose.Pitch
			}
			if math.Abs(got-.1) > 1e-9 {
				t.Fatalf("lost %s orientation: %+v", axis, observed.Pose)
			}
		})
	}
}
