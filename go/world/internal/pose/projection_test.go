package pose

import (
	"math"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/world/internal/sensing"
)

func fixture() (sensing.Camera, ImageResult) {
	c := sensing.Camera{Width: 320, Height: 240, FX: 200, FY: 200, CX: 160, CY: 120, CameraToSite: []float64{1, 0, 0, 0, 0, 0, -1, 2, 0, 1, 0, 0, 0, 0, 0, 1}}
	i := ImageResult{ModelID: "test-pose", ModelVersion: "1", ModelSHA256: strings.Repeat("a", 64), ImageSHA256: strings.Repeat("b", 64), ImageWidth: 320, ImageHeight: 240, Detections: []Person{{Score: .9, LeftAnkle: Keypoint{150, 140, .8}, RightAnkle: Keypoint{170, 140, .8}}}}
	return c, i
}
func TestProjectionUsesBothAnklesAndCalibration(t *testing.T) {
	c, i := fixture()
	r, err := Project(c, i, Config{CameraUncertaintyM: .03})
	if err != nil || len(r.Detections) != 1 {
		t.Fatalf("%+v %v", r, err)
	}
	d := r.Detections[0]
	if math.Abs(d.PositionXZ[0]) > 1e-10 || math.Abs(d.PositionXZ[1]-.2) > 1e-10 {
		t.Fatalf("position %+v", d)
	}
	if d.Covariance[0] < .001 || d.Covariance[3] < .001 || d.Covariance[1] != d.Covariance[2] {
		t.Fatalf("covariance %+v", d)
	}
	// Rotate 90 degrees around World Y, translate, and project the same pixels.
	c.CameraToSite = []float64{0, 1, 0, 3, 0, 0, -1, 2, -1, 0, 0, -4, 0, 0, 0, 1}
	r, err = Project(c, i, Config{})
	if err != nil || math.Abs(r.Detections[0].PositionXZ[0]-3.2) > 1e-10 || math.Abs(r.Detections[0].PositionXZ[1]+4) > 1e-10 {
		t.Fatalf("transformed %+v %v", r, err)
	}
}
func TestMissingOrUnusableFeetDoNotMakeTracks(t *testing.T) {
	for _, name := range []string{"occluded", "outside", "wide", "horizon", "behind"} {
		t.Run(name, func(t *testing.T) {
			c, i := fixture()
			switch name {
			case "occluded":
				i.Detections[0].RightAnkle.Confidence = .1
			case "outside":
				i.Detections[0].LeftAnkle.Y = 240
			case "wide":
				i.Detections[0].LeftAnkle.X = 20
				i.Detections[0].RightAnkle.X = 300
			case "horizon":
				c.CameraToSite = []float64{1, 0, 0, 0, 0, 1, 0, 2, 0, 0, 1, 0, 0, 0, 0, 1}
			case "behind":
				c.CameraToSite[7] = -2
			}
			r, err := Project(c, i, Config{})
			if err != nil || len(r.Detections) != 0 || len(r.Rejected) != 1 {
				t.Fatalf("%+v %v", r, err)
			}
		})
	}
}
func TestProjectionRejectsBadImageAndPoseMetadata(t *testing.T) {
	for _, name := range []string{"digest", "dimensions", "nonfinite", "reflection", "null"} {
		t.Run(name, func(t *testing.T) {
			c, i := fixture()
			switch name {
			case "digest":
				i.ImageSHA256 = "wrong"
			case "dimensions":
				i.ImageWidth++
			case "nonfinite":
				i.Detections[0].LeftAnkle.X = math.NaN()
			case "reflection":
				c.CameraToSite[0] = -1
			case "null":
				i.Detections = nil
			}
			if _, err := Project(c, i, Config{}); err == nil {
				t.Fatal("accepted invalid metadata")
			}
		})
	}
}
func TestUncertaintyGrowsWithPixelAndCalibrationError(t *testing.T) {
	c, i := fixture()
	a, _ := Project(c, i, Config{})
	b, _ := Project(c, i, Config{PixelUncertainty: 5, CameraUncertaintyM: .2, FloorUncertaintyM: .1})
	for _, k := range []int{0, 3} {
		if b.Detections[0].Covariance[k] <= a.Detections[0].Covariance[k] {
			t.Fatal("uncertainty did not grow")
		}
	}
}
