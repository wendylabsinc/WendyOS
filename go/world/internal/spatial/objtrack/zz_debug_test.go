package objtrack

import (
	"os"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/spatial"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/geom"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/spatialsim"
)

func TestDebugBias(t *testing.T) {
	if os.Getenv("OBJTRACK_DEBUG") == "" {
		t.Skip()
	}
	for _, seed := range seeds(t) {
		s, _ := spatialsim.Office(seed, spatialsim.Options{})
		r := newRig(t, s)
		r.floors(mapFrom, windowFrom, true)
		tr := s.Truth()
		n := 0
		r.feed(windowFrom, windowTo, nil, func(at time.Time) {
			n++
			if n%50 != 0 {
				return
			}
			for _, cam := range []spatial.DeviceID{spatialsim.CamYard, spatialsim.CamLobby} {
				ref := spatial.SensorRef{Device: cam, Sensor: "camera"}
				b := r.tk.biases[ref]
				if b == nil {
					t.Logf("%.0f %s no bias learned", at.Sub(s.Epoch()).Seconds(), cam)
					continue
				}
				t.Logf("%.0f %s u=(%.2f,%.2f) sig=(%.2f,%.2f)", at.Sub(s.Epoch()).Seconds(), cam, b.u.X, b.u.Z, sqrt(b.p[0]), sqrt(b.p[3]))
			}
		})
		at := s.At(230 * time.Second)
		for _, cam := range []spatial.DeviceID{spatialsim.CamYard, spatialsim.CamLobby} {
			c, _ := r.ref.Calibration(cam, "camera", at)
			in, pose, _ := tr.Camera(cam, at)
			w := geom.RotLog(pose.R.T().Mul(c.SensorToParent.R))
			t.Logf("seed %d %s rot err (optical frame) = (%.4f, %.4f, %.4f) rad = (%.1f, %.1f, %.1f) sigma; dT=%v dfx=%.1f dcx=%.1f dcy=%.1f dk1=%.4f", seed, cam, w.X, w.Y, w.Z, w.X/c.RotationSigmaRad, w.Y/c.RotationSigmaRad, w.Z/c.RotationSigmaRad, c.SensorToParent.T.Sub(pose.T), c.Intrinsics.FX-in.FX, c.Intrinsics.CX-in.CX, c.Intrinsics.CY-in.CY, c.Intrinsics.K1-in.K1)
		}
	}
}

func sqrt(v float64) float64 {
	if v < 0 {
		return -1
	}
	x := v
	for i := 0; i < 40 && x > 0; i++ {
		x = (x + v/x) / 2
	}
	return x
}
