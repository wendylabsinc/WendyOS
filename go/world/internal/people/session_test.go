package people

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/pose"
	"github.com/wendylabsinc/wendy/go/world/internal/sensing"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

func input(sequence uint64, ns int64) Request {
	frame := world.FrameRef{ID: "floor", Revision: "1"}
	r := Request{ParticipantID: "hub", RunID: "people-test", Frame: frame, NowNS: ns, Images: []Image{}}
	for i, name := range []string{"front", "oblique"} {
		x := float64(i)
		c := &sensing.Camera{Width: 320, Height: 240, FX: 200, FY: 200, CX: 160, CY: 120, CameraToSite: []float64{1, 0, 0, x, 0, 0, -1, 2, 0, 1, 0, 0, 0, 0, 0, 1}, CalibrationValidForNS: int64(time.Minute)}
		s := sensing.Sample{SchemaVersion: sensing.Schema, ID: fmt.Sprintf("people-test/%s/%d", name, sequence), SiteID: "studio-01", Frame: frame, SensorID: name, Kind: "camera", Sequence: sequence, SampleNS: ns, RunID: "people-test", ScenarioVersion: "1", Seed: 7, PackageSHA256: strings.Repeat("a", 64), SensorModel: "camera-test", PositionM: []float64{x, 2, 0}, UncertaintyM: .02, CalibrationRevision: "1", Camera: c, Evidence: &sensing.EvidenceRef{SHA256: strings.Repeat("b", 64), ContentType: "image/png", Size: 100}}
		p := pose.ImageResult{ModelID: "pose-test", ModelVersion: "1", ModelSHA256: strings.Repeat("c", 64), ImageSHA256: s.Evidence.SHA256, ImageWidth: 320, ImageHeight: 240, Detections: []pose.Person{{Score: .9, LeftAnkle: pose.Keypoint{X: 150 - 100*x, Y: 140, Confidence: .9}, RightAnkle: pose.Keypoint{X: 170 - 100*x, Y: 140, Confidence: .9}}}}
		r.Images = append(r.Images, Image{s, p})
	}
	return r
}
func TestCameraFusionPreservesOriginalImageAndModelLineage(t *testing.T) {
	s := new(Session)
	r, err := s.Step(input(1, 0))
	if err != nil || len(r.Tracks) != 1 || len(r.Tracks[0].Members) != 2 {
		t.Fatalf("%+v %v", r, err)
	}
	track := r.Tracks[0]
	for _, m := range track.Members {
		if m.ModelSHA256 != strings.Repeat("c", 64) || m.Evidence[0].SHA256 != strings.Repeat("b", 64) || m.RunID != "people-test" {
			t.Fatalf("lineage lost: %+v", m)
		}
	}
	r, err = s.Step(input(2, int64(250*time.Millisecond)))
	if err != nil || len(r.Tracks) != 1 || r.Tracks[0].ID != track.ID {
		t.Fatalf("continuity lost: %+v %v", r, err)
	}
}
func TestExpiryUsesCapturedTimeAndDuplicateDoesNotRefresh(t *testing.T) {
	s := new(Session)
	req := input(1, 0)
	if _, err := s.Step(req); err != nil {
		t.Fatal(err)
	}
	req.NowNS = int64(time.Second)
	r, err := s.Step(req)
	if err != nil || len(r.Tracks) != 1 || !r.Tracks[0].ValidUntil.Equal(Anchor.Add(Lifetime)) {
		t.Fatalf("%+v %v", r, err)
	}
	req.NowNS = int64(Lifetime)
	r, err = s.Step(req)
	if err != nil || len(r.Tracks) != 0 || len(r.Cameras) != 0 {
		t.Fatalf("expired capture survived: %+v %v", r, err)
	}
}
func TestImageAndFrameBinding(t *testing.T) {
	for _, name := range []string{"digest", "frame", "future", "same_camera"} {
		t.Run(name, func(t *testing.T) {
			r := input(1, 0)
			switch name {
			case "digest":
				r.Images[0].Poses.ImageSHA256 = strings.Repeat("d", 64)
			case "frame":
				r.Frame.Revision = "2"
			case "future":
				r.Images[0].Sample.SampleNS = 1
			case "same_camera":
				r.Images[1] = r.Images[0]
			}
			if _, err := new(Session).Step(r); err == nil {
				t.Fatal("unbound image accepted")
			}
		})
	}
}
func TestOccludedFeetProduceNoFloorPosition(t *testing.T) {
	r := input(1, 0)
	for i := range r.Images {
		r.Images[i].Poses.Detections[0].LeftAnkle.Confidence = .1
	}
	v, err := new(Session).Step(r)
	if err != nil || len(v.Tracks) != 0 || len(v.Cameras) != 2 || len(v.Cameras[0].Rejected) != 1 {
		t.Fatalf("%+v %v", v, err)
	}
}
