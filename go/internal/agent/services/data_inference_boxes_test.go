package services

import (
	"bufio"
	"context"
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	"github.com/wendylabsinc/wendy/go/internal/agent/inference"
)

// A detection as a reader decodes it from a recorded prediction.
type recordedDetection struct {
	Label   string      `json:"label"`
	Score   float64     `json:"score"`
	Box     [4]float64  `json:"box"`
	BoxNorm *[4]float64 `json:"box_norm"`
}

type recordedPrediction struct {
	Width      *int                `json:"width"`
	Height     *int                `json:"height"`
	Detections []recordedDetection `json:"detections"`
}

func nearBox(got [4]float64, want [4]float64) bool {
	for i := range got {
		// box_norm is float32: about seven significant digits.
		if math.Abs(got[i]-want[i]) > 1e-7 {
			return false
		}
	}
	return true
}

func TestNormalisedBoxClampsToThePicture(t *testing.T) {
	for _, c := range []struct {
		name          string
		box           [4]float64
		width, height int
		want          *[4]float64
	}{
		// The record seen on Mesh 1: a C920 at 1280x720.
		{"live c920 person", [4]float64{0, 184.68881225585938, 845.9542236328125, 693.4185791015625}, 1280, 720,
			&[4]float64{0, 184.68881225585938 / 720, 845.9542236328125 / 1280, 693.4185791015625 / 720}},
		{"whole picture", [4]float64{0, 0, 1280, 720}, 1280, 720, &[4]float64{0, 0, 1, 1}},
		{"touches the right and bottom edges", [4]float64{640, 360, 1280, 720}, 1280, 720, &[4]float64{.5, .5, 1, 1}},
		{"reaches past every edge", [4]float64{-10, -5, 1300, 800}, 1280, 720, &[4]float64{0, 0, 1, 1}},
		{"one pixel picture", [4]float64{0, 0, 1, 1}, 1, 1, &[4]float64{0, 0, 1, 1}},
		// Not 16:9: the two axes divide by their own sizes.
		{"4:3", [4]float64{160, 120, 480, 360}, 640, 480, &[4]float64{.25, .25, .75, .75}},
		{"portrait", [4]float64{0, 320, 240, 640}, 480, 640, &[4]float64{0, .5, .5, 1}},
		// The boxes wendy-data-app normalises event journal detections with.
		{"inside", [4]float64{200, 150, 400, 450}, 800, 600, &[4]float64{.25, .25, .5, .75}},
		{"above and right of the picture", [4]float64{700, -5, 900, 100}, 800, 600, &[4]float64{.875, 0, 1, 100.0 / 600}},
		{"entirely outside", [4]float64{810, 10, 900, 20}, 800, 600, nil},
		{"no width", [4]float64{10, 10, 10, 20}, 800, 600, nil},
		{"no height", [4]float64{10, 10, 20, 10}, 800, 600, nil},
		{"inverted", [4]float64{20, 10, 10, 20}, 800, 600, nil},
		{"along the bottom edge only", [4]float64{0, 720, 1280, 800}, 1280, 720, nil},
		{"unknown size", [4]float64{1, 2, 3, 4}, 0, 0, nil},
		{"unknown height", [4]float64{1, 2, 3, 4}, 640, 0, nil},
	} {
		got, ok := normalisedBox(c.box, c.width, c.height)
		if c.want == nil {
			if ok {
				t.Errorf("%s: box_norm %v, want none", c.name, got)
			}
			continue
		}
		g := [4]float64{float64(got[0]), float64(got[1]), float64(got[2]), float64(got[3])}
		if !ok || !nearBox(g, *c.want) {
			t.Errorf("%s: box_norm %v (%v), want %v", c.name, got, ok, *c.want)
		}
		for _, v := range g {
			if v < 0 || v > 1 {
				t.Errorf("%s: box_norm %v leaves [0, 1]", c.name, got)
			}
		}
	}
}

// The pixel box stays exactly what the model returned, whether or not a
// normalised box can be added beside it.
func TestAddPictureKeepsThePixelBox(t *testing.T) {
	detections := []inference.Detection{
		{Label: "person", Score: .9225323796272278, Box: [4]float64{0, 184.68881225585938, 845.9542236328125, 693.4185791015625}},
		{Label: "person", Score: .5, Box: [4]float64{1290, 10, 1400, 20}},
	}
	attributes := map[string]any{"detections": detections}
	addPicture(attributes, detections, 1280, 720)
	raw, err := json.Marshal(attributes)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`"box":[0,184.68881225585938,845.9542236328125,693.4185791015625]`,
		`"width":1280`, `"height":720`,
		`"box":[1290,10,1400,20]}`, // outside the picture: no box_norm
	} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("attributes %s lack %s", raw, want)
		}
	}
	var got recordedPrediction
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Detections) != 2 || got.Detections[0].Box != detections[0].Box || got.Detections[0].Score != detections[0].Score || got.Detections[0].BoxNorm == nil {
		t.Fatalf("detections %+v", got.Detections)
	}

	// A worker that does not report its picture: the record is today's.
	unknown := map[string]any{"detections": detections}
	addPicture(unknown, detections, 0, 0)
	raw, err = json.Marshal(unknown)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "box_norm") || strings.Contains(string(raw), "width") || strings.Contains(string(raw), "height") {
		t.Fatalf("a picture of unknown size added fields: %s", raw)
	}
	before, _ := json.Marshal(map[string]any{"detections": detections})
	if string(raw) != string(before) {
		t.Fatalf("a picture of unknown size changed the record:\n%s\n%s", raw, before)
	}
}

// What a campaign records for a scored frame: the episode's prediction, and
// the detection event in the device's event journal, both carry the pixel box
// unchanged, box_norm beside it, and the picture's size.
func TestAgentInferenceRecordsNormalisedBoxesAndPictureSize(t *testing.T) {
	service, factory, sender, _ := newInferenceTestService(t, false)
	session := receiveInference(t, factory.sessions)
	input := receiveInference(t, session.inputs)
	box := [4]float64{0, 184.68881225585938, 845.9542236328125, 693.4185791015625}
	wantNorm := [4]float64{0, box[1] / 720, box[2] / 1280, box[3] / 720}
	frame := inference.Result{Type: "prediction", SourceID: input.SourceID, Generation: input.Generation, Width: 1280, Height: 720,
		Detections: []inference.Detection{{Label: "person", Score: .9225323796272278, Box: box}}}
	check := func(where string, attributes []byte) {
		t.Helper()
		var got recordedPrediction
		if err := json.Unmarshal(attributes, &got); err != nil {
			t.Fatalf("%s: %v", where, err)
		}
		if got.Width == nil || *got.Width != 1280 || got.Height == nil || *got.Height != 720 {
			t.Fatalf("%s: picture size missing: %s", where, attributes)
		}
		if len(got.Detections) != 1 {
			t.Fatalf("%s: detections %s", where, attributes)
		}
		d := got.Detections[0]
		if d.Label != "person" || d.Score != .9225323796272278 || d.Box != box {
			t.Fatalf("%s: the pixel box changed: %+v", where, d)
		}
		if d.BoxNorm == nil || !nearBox(*d.BoxNorm, wantNorm) {
			t.Fatalf("%s: box_norm %v, want %v", where, d.BoxNorm, wantNorm)
		}
	}

	// The person arrives: an event, an episode and a notification.
	session.results <- frame
	receiveInference(t, sender.requests)
	events, _, _, err := service.manager.DeviceEvents(data.CampaignAppID("people-all-cameras"), "person_detected", "", true)
	if err != nil || len(events) != 1 {
		t.Fatalf("device events %v, %v", events, err)
	}
	attributes, err := json.Marshal(events[0].Attributes)
	if err != nil {
		t.Fatal(err)
	}
	check("device event", attributes)

	// The next frame is recorded into the episode. The job handles results in
	// order, so once the source error after it shows, the frame is recorded.
	session.results <- frame
	session.results <- inference.Result{Type: "source_error", SourceID: input.SourceID, Generation: input.Generation, Error: "frame handled"}
	campaign, err := service.manager.Campaign("people-all-cameras")
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(8 * time.Second)
	for service.inference.snapshot(campaign.Name, campaign.Revision).Sources[input.SourceID] != "frame handled" {
		if time.Now().After(deadline) {
			t.Fatal("the job never handled the frame")
		}
		time.Sleep(10 * time.Millisecond)
	}
	episode, err := service.stopCapture(context.Background(), "people-all-cameras")
	if err != nil {
		t.Fatal(err)
	}
	f, _, err := service.manager.OpenFile(episode.Id, "events.jsonl", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	predictions := 0
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	for scanner.Scan() {
		var line struct {
			Type       string          `json:"type"`
			Model      string          `json:"model"`
			Attributes json.RawMessage `json:"attributes"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &line); err != nil {
			t.Fatal(err)
		}
		if line.Type == "prediction" {
			predictions++
			check("episode prediction", line.Attributes)
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if predictions == 0 {
		t.Fatal("the episode holds no prediction")
	}
}
