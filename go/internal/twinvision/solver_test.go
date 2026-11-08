package twinvision

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"math"
	"os"
	"testing"
)

func solution(estimated bool) Result {
	r := Result{Success: 1, Inliers: 30, FX: 500, FY: 500, CX: 320, CY: 240, RMS: 1, FittedLens: 1}
	if estimated {
		r.FittedLens = 2
	}
	r.Transform = [16]float64{1, 0, 0, 0, 0, 1, 0, 0, 0, 0, 1, 0, 0, 0, 0, 1}
	return r
}
func TestSharedTwoFrameDecision(t *testing.T) {
	for _, estimated := range []bool{false, true} {
		for _, change := range []string{"none", "position", "rotation", "reflection", "rms", "inliers", "nonfinite", "failure", "lens", "provenance", "homogeneous"} {
			t.Run(change+map[bool]string{false: "Measured", true: "Estimated"}[estimated], func(t *testing.T) {
				a, b := solution(estimated), solution(estimated)
				if !estimated {
					b.FittedLens = 0
				}
				switch change {
				case "position":
					b.Transform[12] = .5
				case "rotation":
					b.Transform[0] = 0
				case "reflection":
					b.Transform[0] = -1
				case "rms":
					b.RMS = 20
				case "inliers":
					b.Inliers = 3
				case "nonfinite":
					b.FX = math.NaN()
				case "failure":
					b.Success = 0
				case "lens":
					b.FX += 1
				case "provenance":
					b.FittedLens = 2 - b.FittedLens
				case "homogeneous":
					b.Transform[3] = 1
				}
				result, e := Verify(a, b, 640, 480)
				if (e == nil) != (change == "none") {
					t.Fatalf("%s: %v", change, e)
				}
				if e == nil && result.FittedLens != a.FittedLens {
					t.Fatal("lens provenance changed")
				}
			})
		}
	}
}
func TestValidationMatchesMeasuredAndEstimatedLimits(t *testing.T) {
	a, b := solution(false), solution(false)
	b.FittedLens = 0
	b.Transform[12] = .1
	if _, e := Verify(a, b, 640, 480); e == nil {
		t.Fatal("measured drift accepted")
	}
	a.FittedLens = 2
	b.FittedLens = 2
	if _, e := Verify(a, b, 640, 480); e != nil {
		t.Fatal(e)
	}
	a.FittedLens = 1
	b.FittedLens = 0
	b.Transform[12] = 0
	b.Inliers = 12
	if _, e := Verify(a, b, 640, 480); e == nil {
		t.Fatal("joint fit lost required support")
	}
}
func TestFrameValidation(t *testing.T) {
	good := Frame{Width: 32, Height: 32, Codec: "H.264", Sequence: 1, Gray: make([]byte, 1024)}
	for _, field := range []string{"valid", "age", "sequence", "format", "size", "pixels"} {
		f := good
		switch field {
		case "age":
			f.AgeMS = 1501
		case "sequence":
			f.Sequence = 0
		case "format":
			f.Codec = "unknown"
		case "size":
			f.Width = 9000
		case "pixels":
			f.Gray = f.Gray[:100]
		}
		if (f.Validate() == nil) != (field == "valid") {
			t.Fatal(field)
		}
	}
}
func TestQuaternion(t *testing.T) {
	for _, q := range [][4]float64{{0, 0, 0, 1}, {1, 0, 0, 0}, {0, 1, 0, 0}, {0, 0, 1, 0}, {.5, .5, .5, .5}} {
		x, y, z, w := q[0], q[1], q[2], q[3]
		r := [16]float64{1 - 2*y*y - 2*z*z, 2*x*y + 2*z*w, 2*x*z - 2*y*w, 0, 2*x*y - 2*z*w, 1 - 2*x*x - 2*z*z, 2*y*z + 2*x*w, 0, 2*x*z + 2*y*w, 2*y*z - 2*x*w, 1 - 2*x*x - 2*y*y, 0, 0, 0, 0, 1}
		out := Quaternion(r)
		dot := 0.
		for i, v := range out {
			dot += v * q[i]
		}
		if math.Abs(math.Abs(dot)-1) > 1e-8 {
			t.Fatal(q, out)
		}
	}
}

// The same fixture is fed through the HTTP service and can be replayed against
// the packaged native worker on either Linux architecture without a robot.
func TestNativeReplay(t *testing.T) {
	path := os.Getenv("WENDY_TWIN_REPLAY")
	if path == "" {
		t.Skip("set WENDY_TWIN_REPLAY and WENDY_TWIN_VISION_WORKER for native replay")
	}
	raw, e := os.ReadFile(path)
	if e != nil {
		t.Fatal(e)
	}
	var fixture struct {
		Landmarks  string
		Frame      Frame
		Lens       [9]float64
		Provenance int32
	}
	if e = json.Unmarshal(raw, &fixture); e != nil {
		t.Fatal(e)
	}
	landmarks, e := base64.StdEncoding.DecodeString(fixture.Landmarks)
	if e != nil {
		t.Fatal(e)
	}
	var previous Result
	for i := 0; i < 3; i++ {
		r, e := Solve(context.Background(), landmarks, fixture.Frame, fixture.Lens, fixture.Provenance, false)
		if e != nil {
			t.Fatal(e)
		}
		if i > 0 && r != previous {
			t.Fatalf("nondeterministic replay: %+v vs %+v", previous, r)
		}
		previous = r
	}
}

func TestWorkerTransport(t *testing.T) {
	if os.Getenv("WENDY_TWIN_VISION_WORKER") == "" {
		t.Skip("requires packaged native worker")
	}
	frame := Frame{Width: 32, Height: 32, Codec: "H.264", Sequence: 1, Gray: make([]byte, 1024)}
	var previous Result
	for i := 0; i < 3; i++ {
		r, e := Solve(context.Background(), make([]byte, 120*131*4), frame, [9]float64{}, 0, false)
		if e != nil {
			t.Fatal(e)
		}
		if r.Success != 0 || r.Reason != 1 || r.Source != 3 {
			t.Fatalf("blank frame must never produce alignment: %+v", r)
		}
		if i > 0 && r != previous {
			t.Fatal("worker replay changed its decision")
		}
		previous = r
	}
}
