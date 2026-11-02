package backend

import (
	"github.com/wendylabsinc/wendy/go/macos/backend/sitenav"
	"math"
	"testing"
	"time"
)

func TestCameraEvidenceRequiresFreshPairedOdometry(t *testing.T) {
	now := time.Now()
	at := now.Add(-time.Second)
	original := sitenav.VisualHint{Position: sitenav.Point{X: 2, Y: 3}, Yaw: math.Pi / 2, At: float64(at.UnixNano()) / 1e9, Inliers: 20, Residual: 2}
	for _, fault := range []string{"valid", "old", "future", "nonfinite", "unpaired", "few-inliers", "bad-residual"} {
		t.Run(fault, func(t *testing.T) {
			hint := original
			history := []sitenav.Pose{{Position: sitenav.Point{}, Yaw: 0, Received: at}}
			switch fault {
			case "old":
				hint.At -= 4
			case "future":
				hint.At += 2
			case "nonfinite":
				hint.At = math.NaN()
			case "unpaired":
				history[0].Received = at.Add(time.Second)
			case "few-inliers":
				hint.Inliers = 11
			case "bad-residual":
				hint.Residual = 6
			}
			result := pairedVisualHint(&hint, history, sitenav.Pose{Position: sitenav.Point{X: 1, Y: 0}, Yaw: .2}, now)
			if fault != "valid" {
				if result != nil {
					t.Fatal("invalid image evidence accepted")
				}
				return
			}
			if result == nil || math.Abs(result.Position.X-2) > 1e-6 || math.Abs(result.Position.Y-4) > 1e-6 || math.Abs(result.Yaw-(math.Pi/2+.2)) > 1e-6 {
				t.Fatalf("wrong compensated camera pose: %+v", result)
			}
			if hint != original {
				t.Fatal("modified original image pose")
			}
		})
	}
}

func TestFusionWaitsForFreshMeasurementsWithoutRetainingCandidates(t *testing.T) {
	for _, fault := range []string{"odometry-arrival", "odometry-capture", "lidar"} {
		t.Run(fault, func(t *testing.T) {
			r, commands := readySiteRobot()
			r.localizations = []siteRobotLocalization{{frame: "old"}}
			stale := time.Now().Add(-time.Second)
			switch fault {
			case "odometry-arrival":
				r.observed.Pose.Received = stale
			case "odometry-capture":
				r.observed.Pose.Captured = stale
			case "lidar":
				r.observed.CloudAt = stale
			}
			report, err := r.FuseLocalization(1, SiteFusionRequest{Frame: "site/revision"})
			if err != nil || !report.WaitingForObservations || report.Tracking.Selected != -1 || len(report.Candidates) != 0 {
				t.Fatalf("expected recoverable wait without pose: %+v, %v", report, err)
			}
			if len(r.localizations) != 0 || len(commands.commands) != 0 || r.Snapshot().MeasurementsReady {
				t.Fatal("waiting retained a candidate, moved, or reported fresh measurements")
			}
			// Fresh readings can resume acquisition, even before there is enough
			// room structure to match. Timing must not hide a geometry failure.
			now := time.Now()
			r.observed.Pose.Received, r.observed.Pose.Captured, r.observed.CloudAt = now, now, now
			if s := r.Snapshot(); !s.MeasurementsReady || s.ScanReady {
				t.Fatal("fresh sparse scan was confused with delayed measurements")
			}
			if report, err = r.FuseLocalization(2, SiteFusionRequest{Frame: "site/revision"}); err == nil || report.WaitingForObservations {
				t.Fatal("fresh insufficient geometry was reported as a timing wait")
			}
			// Waiting does not permit a stale request to supersede Stop.
			r.Stop(3)
			if _, err = r.FuseLocalization(2, SiteFusionRequest{Frame: "site/revision"}); err == nil {
				t.Fatal("superseded request resumed after Stop")
			}
		})
	}
}
