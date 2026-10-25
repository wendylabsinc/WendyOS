package services

import (
	"context"
	"errors"
	"io"
	"math"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	"github.com/wendylabsinc/wendy/go/internal/agent/data/live"
	"github.com/wendylabsinc/wendy/go/internal/agent/inference"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"github.com/wendylabsinc/wendy/go/proto/gen/datapb"
	"google.golang.org/protobuf/proto"
)

func near(a float32, b float64) bool { return math.Abs(float64(a)-b) < 1e-6 }

// The cases wendy-data-app placed journal boxes with, now from every frame.
func TestDetectionMarkersNormaliseAgainstThePicture(t *testing.T) {
	markers := detectionMarkers([]inference.Detection{
		{Label: "person", Score: .87, Box: [4]float64{200, 150, 400, 450}},
		// Reaches above and right of the picture: clamped, not dropped.
		{Label: "person", Score: .61, Box: [4]float64{700, -5, 900, 100}},
		// Entirely outside the picture, and empty: dropped.
		{Label: "person", Score: .9, Box: [4]float64{810, 10, 900, 20}},
		{Label: "person", Score: .9, Box: [4]float64{10, 10, 10, 20}},
	}, 800, 600)
	if len(markers) != 2 {
		t.Fatalf("got %d markers: %v", len(markers), markers)
	}
	m := markers[0]
	if m.Kind != datapb.Marker_KIND_BOX || !near(m.X, .375) || !near(m.Y, .5) || !near(m.Width, .25) || !near(m.Height, .5) {
		t.Errorf("box = %v", m)
	}
	if m.Label != "person" || math.Abs(m.GetValue()-87) > 1e-9 || m.Unit != "%" || m.Color != "" {
		t.Errorf("label = %v", m)
	}
	c := markers[1]
	if !near(c.X, 750.0/800) || !near(c.Width, 100.0/800) || !near(c.Y, 50.0/600) || !near(c.Height, 100.0/600) {
		t.Errorf("clamped box = %v", c)
	}
}

func TestMarkersCameraIsTheViewersName(t *testing.T) {
	for source, want := range map[string]string{"v4l2:/dev/video96": "/dev/video96", "ipcamera:1000000": "1000000", "audio:hw:0": ""} {
		if got := markersCamera(source); got != want {
			t.Errorf("%s: got %q, want %q", source, got, want)
		}
	}
	if markersValidFor(1) != 3*time.Second || markersValidFor(30) != 2*time.Second {
		t.Errorf("valid for %v at 1 Hz, %v at 30 Hz", markersValidFor(1), markersValidFor(30))
	}
}

func nextMarkers(t *testing.T, sub *live.Subscriber) (*live.Record, *datapb.VideoMarkers) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	d, err := sub.Next(ctx)
	if err != nil {
		t.Fatalf("no live markers: %v", err)
	}
	markers := &datapb.VideoMarkers{}
	if err := proto.Unmarshal(d.Record.Payload, markers); err != nil {
		t.Fatal(err)
	}
	return d.Record, markers
}

// Every scored frame reaches viewers as markers, stamped with its camera
// sample's time, whether or not an episode records it. Once one does, the
// episode holds the same record.
func TestAgentInferencePublishesLiveMarkers(t *testing.T) {
	service, factory, _, _ := newInferenceTestService(t, false)
	sub, err := service.manager.Live().Subscribe(live.Selection{})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	session := receiveInference(t, factory.sessions)
	input := receiveInference(t, session.inputs)
	if input.Sample == nil || input.Sample.ID != 1 {
		t.Fatalf("the worker is not told which sample it decodes: %+v", input.Sample)
	}
	stream := "agent/campaign/people-all-cameras/markers/" + input.SourceID
	const frameTime = int64(1_792_000_000_000_000_000)
	result := func(id uint64, detections ...inference.Detection) inference.Result {
		return inference.Result{Type: "prediction", SourceID: input.SourceID, Generation: input.Generation, Detections: detections,
			Width: 800, Height: 600, Sample: &inference.Sample{ID: id, UnixNanos: frameTime + int64(id)}}
	}

	// Below the campaign's threshold: nothing to draw, so the frame clears
	// the camera, and no episode starts.
	session.results <- result(1, inference.Detection{Label: "person", Score: .5, Box: [4]float64{1, 2, 3, 4}})
	record, markers := nextMarkers(t, sub)
	if record.StreamID != stream || record.Schema != "wendy.data.v1.VideoMarkers" || record.TimestampUnixNanos != frameTime+1 {
		t.Fatalf("live record: %+v", record)
	}
	if len(markers.Markers) != 0 || markers.Camera != markersCamera(input.SourceID) || markers.ValidForNanos != int64(3*time.Second) {
		t.Fatalf("markers of an empty frame: %v", markers)
	}
	if keys := service.manager.ActiveEpisodeKeys(); len(keys) != 0 {
		t.Fatalf("an empty frame started an episode: %v", keys)
	}

	// A person: a box on the live stream, and an episode starts.
	person := inference.Detection{Label: "person", Score: .97, Box: [4]float64{200, 150, 400, 450}}
	session.results <- result(2, person)
	if record, markers = nextMarkers(t, sub); record.TimestampUnixNanos != frameTime+2 || len(markers.Markers) != 1 || !near(markers.Markers[0].X, .375) {
		t.Fatalf("live record %+v with markers %v", record, markers)
	}

	// The next frame reaches the viewer and the episode alike.
	session.results <- result(3, person)
	record, _ = nextMarkers(t, sub)
	episode, err := service.stopCapture(context.Background(), "people-all-cameras")
	if err != nil {
		t.Fatal(err)
	}
	f, _, err := service.manager.OpenFile(episode.Id, data.RecordingLogFile, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for {
		recorded, _, err := data.ReadRecording(f)
		if errors.Is(err, io.EOF) {
			t.Fatal("the episode does not hold the frame's markers")
		}
		if err != nil {
			t.Fatal(err)
		}
		if in := recorded.Record.GetInputs(); "agent/"+recorded.Stream != stream || len(in) != 1 || in[0].SampleId != 3 {
			continue
		}
		if string(recorded.Record.Payload) != string(record.Payload) {
			t.Fatal("the episode and the viewer got different markers")
		}
		break
	}
}

// A campaign's markers stream for each camera it scores is in the live
// catalog while its inference runs, active once a frame is scored, and gone
// when inference stops.
func TestAgentInferenceAdvertisesMarkers(t *testing.T) {
	service, factory, _, _, stop := startInferenceTestService(t, false)
	const prefix = "agent/campaign/people-all-cameras/markers/"
	catalog, err := service.manager.Live().Subscribe(live.Selection{StreamPrefixes: []string{live.CatalogPrefix + prefix}, SendLatest: true})
	if err != nil {
		t.Fatal(err)
	}
	defer catalog.Close()
	// await reads catalog entries until stream reaches state. An entry may
	// arrive both with the current catalog and live, so earlier states of the
	// same stream may repeat.
	await := func(stream string, state agentpbv2.DataStreamInfo_State) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		for {
			d, err := catalog.Next(ctx)
			if err != nil {
				t.Fatalf("%s never became %v: %v", stream, state, err)
			}
			var info agentpbv2.DataStreamInfo
			if err := proto.Unmarshal(d.Record.Payload, &info); err != nil {
				t.Fatal(err)
			}
			if info.GetPublisher() != "agent" || info.GetSchema() != "wendy.data.v1.VideoMarkers" || info.GetMediaType() != "application/protobuf" {
				t.Fatalf("catalog entry: %v", &info)
			}
			if info.GetStreamId() == stream && info.GetState() == state {
				return
			}
		}
	}

	session := receiveInference(t, factory.sessions)
	first, second := receiveInference(t, session.inputs), receiveInference(t, session.inputs)
	await(prefix+first.SourceID, agentpbv2.DataStreamInfo_STATE_IDLE)
	session.results <- inference.Result{Type: "prediction", SourceID: first.SourceID, Generation: first.Generation, Width: 800, Height: 600, Sample: &inference.Sample{ID: 1}}
	await(prefix+first.SourceID, agentpbv2.DataStreamInfo_STATE_ACTIVE)

	stop()
	gone := map[string]bool{}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	for !gone[prefix+first.SourceID] || !gone[prefix+second.SourceID] {
		d, err := catalog.Next(ctx)
		if err != nil {
			t.Fatalf("after inference stopped, gone: %v (%v)", gone, err)
		}
		var info agentpbv2.DataStreamInfo
		if err := proto.Unmarshal(d.Record.Payload, &info); err != nil {
			t.Fatal(err)
		}
		if info.GetState() == agentpbv2.DataStreamInfo_STATE_GONE {
			gone[info.GetStreamId()] = true
		}
	}
}
