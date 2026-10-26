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
