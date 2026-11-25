package sensorrecord

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/sensing"
)

func TestLidarSnapshotPublicationPreservesInlineCapture(t *testing.T) {
	snapshot := snapshotFixture(t)
	s := snapshot.Observations[2]
	s.ID, s.SensorID, s.Kind = "run-a/lidar-front/17", "lidar-front", "lidar"
	s.SensorModel, s.Gas = "mujoco-raycast-v1", nil
	s.PositionM = []float64{4, 1.2, 2}
	s.UncertaintyM = .03
	s.Lidar = &sensing.Lidar{PointsM: [][3]float64{{5, 1.2, 2}, {4.5, .2, 3}}, MaxRangeM: 6.1}
	snapshot.Observations = append(snapshot.Observations, s)
	p, media, err := FromSnapshot(snapshot, s.SensorID, testAnchor, 2*time.Second)
	if err != nil || len(media) != 0 {
		t.Fatalf("inline conversion: %v", err)
	}
	capture, err := Decode(p.Payload)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := json.Marshal(s)
	if !bytes.Equal(capture.SampleJSON, want) || capture.CapturedUnixNS != testWall.UnixNano() || !p.Timing.SampleTime.Equal(testAnchor.Add(time.Duration(s.SampleNS))) {
		t.Fatal("LiDAR sample bytes or original clocks changed")
	}
	from, err := FromComponent(testComponent(p))
	if err != nil {
		t.Fatal(err)
	}
	got, err := from.Sample()
	if err != nil || !reflect.DeepEqual(got, s) {
		t.Fatal("site frame, native sequence, placement, uncertainty or cloud changed", err)
	}
	ref, err := capture.Evidence()
	if err != nil || ref != nil || ValidateMedia(capture, nil) != nil {
		t.Fatal("inline cloud requires media", err)
	}
	if ValidateMedia(capture, []byte("invented-media")) == nil {
		t.Fatal("accepted media for inline cloud")
	}
	from.SampleJSON[0] = ' '
	if !bytes.Equal(capture.SampleJSON, want) {
		t.Fatal("returned capture aliased original")
	}
}

func TestLidarCaptureRetainsOriginalSampleEncoding(t *testing.T) {
	s, _ := testSample(t, "gas")
	s.ID, s.SensorID, s.Kind, s.Gas = "run-a/lidar-1/17", "lidar-1", "lidar", nil
	s.Lidar = &sensing.Lidar{PointsM: [][3]float64{}, MaxRangeM: 6.1}
	raw, _ := json.MarshalIndent(s, "", "  ")
	c := Capture{Version: Version, SceneBootID: "scene-boot", CapturedUnixNS: testWall.UnixNano(), SimulationAnchorUnixNS: testAnchor.UnixNano(), SampleJSON: raw}
	p, err := Publication(c, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := Decode(p.Payload)
	if err != nil || !bytes.Equal(decoded.SampleJSON, raw) {
		t.Fatal("publication rewrote original inline sample JSON", err)
	}
}
