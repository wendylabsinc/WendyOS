package fusiondemo

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/fusion"
	"github.com/wendylabsinc/wendy/go/world/internal/sensing"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

func config(node string) Config {
	return Config{Version: 1, ParticipantID: node, RunID: "office", Frame: world.FrameRef{ID: "floor", Revision: "1"}, Sensors: []Sensor{{"camera-a", "camera", "hub"}, {"camera-b", "camera", "hub"}, {"gas-1", "gas", "hub"}, {"audio-1", "audio", "hub"}, {"lidar-atlas", "lidar", "atlas"}, {"lidar-iris", "lidar", "iris"}}}
}
func samples(t *testing.T, node string, sequence uint64, at time.Duration) ([]json.RawMessage, map[string][]byte) {
	t.Helper()
	out := []json.RawMessage{}
	media := map[string][]byte{}
	for _, entry := range config(node).Sensors {
		if entry.Owner != node {
			continue
		}
		s := sensing.Sample{SchemaVersion: sensing.Schema, ID: fmt.Sprintf("office/%s/%d", entry.SensorID, sequence), SiteID: Site, Frame: config(node).Frame, SensorID: entry.SensorID, Kind: entry.Kind, Sequence: sequence, SampleNS: int64(at), RunID: "office", ScenarioVersion: "office-v1", Seed: 3, PackageSHA256: strings.Repeat("a", 64), SensorModel: "fixture", PositionM: []float64{0, 0, 0}, UncertaintyM: .01, CalibrationRevision: "c1"}
		var data []byte
		content := ""
		switch entry.Kind {
		case "camera":
			x, pixel := -1., 5
			if entry.SensorID == "camera-b" {
				x, pixel = 1, 1
			}
			s.PositionM = []float64{x, 0, 0}
			s.Camera = &sensing.Camera{Width: 8, Height: 6, FX: 8, FY: 8, CX: 4, CY: 3, CameraToSite: []float64{1, 0, 0, x, 0, 1, 0, 0, 0, 0, 1, 0, 0, 0, 0, 1}, CalibrationValidForNS: int64(time.Minute)}
			im := image.NewRGBA(image.Rect(0, 0, 8, 6))
			for y := 2; y < 4; y++ {
				for u := pixel; u < pixel+2; u++ {
					im.Set(u, y, color.RGBA{R: 255, A: 255})
				}
			}
			var b bytes.Buffer
			if err := png.Encode(&b, im); err != nil {
				t.Fatal(err)
			}
			data, content = b.Bytes(), "image/png"
		case "gas":
			s.Gas = &sensing.Gas{ValuePPM: 100, UncertaintyPPM: 1, Species: "synthetic"}
		case "audio":
			s.Audio = &sensing.Audio{SampleRateHz: 8000, Channels: 1, DurationNS: int64(250 * time.Millisecond)}
			data = make([]byte, 4044)
			copy(data, "RIFF")
			binary.LittleEndian.PutUint32(data[4:], 4036)
			copy(data[8:], "WAVEfmt ")
			binary.LittleEndian.PutUint32(data[16:], 16)
			binary.LittleEndian.PutUint16(data[20:], 1)
			binary.LittleEndian.PutUint16(data[22:], 1)
			binary.LittleEndian.PutUint32(data[24:], 8000)
			binary.LittleEndian.PutUint32(data[28:], 16000)
			binary.LittleEndian.PutUint16(data[32:], 2)
			binary.LittleEndian.PutUint16(data[34:], 16)
			copy(data[36:], "data")
			binary.LittleEndian.PutUint32(data[40:], 4000)
			for i := 0; i < 2000; i++ {
				binary.LittleEndian.PutUint16(data[44+i*2:], uint16(int16(10000*math.Sin(2*math.Pi*880*float64(i)/8000))))
			}
			content = "audio/wav"
		case "lidar":
			s.Lidar = &sensing.Lidar{PointsM: [][3]float64{{0, 0, 4}, {.02, 0, 4}}, MaxRangeM: 6}
		}
		if data != nil {
			sum := sha256.Sum256(data)
			hash := hex.EncodeToString(sum[:])
			s.Evidence = &sensing.EvidenceRef{SHA256: hash, Size: int64(len(data)), ContentType: content}
			media[hash] = data
		}
		raw, err := json.Marshal(s)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, raw)
	}
	return out, media
}
func openParticipant(t *testing.T, node, dir string) *Evaluator {
	t.Helper()
	e, err := New(config(node), dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Close() })
	return e
}
func tick(t *testing.T, e *Evaluator, in Tick) Output {
	t.Helper()
	out, err := e.Tick(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestParticipantsLocalSensingDuplicateExpiryRejoinAndReopen(t *testing.T) {
	root := t.TempDir()
	hub := openParticipant(t, "hub", filepath.Join(root, "hub"))
	atlasDir := filepath.Join(root, "atlas")
	atlas := openParticipant(t, "atlas", atlasDir)
	hubSamples, media := samples(t, "hub", 1, 250*time.Millisecond)
	scan, _ := samples(t, "atlas", 1, 250*time.Millisecond)
	h := tick(t, hub, Tick{NowNS: 250000000, LocalSamples: hubSamples, Media: media})
	a := tick(t, atlas, Tick{NowNS: 250000000, LocalSamples: scan})
	if h.Result.State != fusion.InsufficientEvidence || h.Result.Hazard != nil || len(h.Observations) != 4 || len(a.Observations) != 1 {
		t.Fatal("missing remote observations became a local/global result")
	}
	if h.Result.Spatial == nil || h.Result.Spatial.Associated {
		t.Fatal("missing LiDAR disabled spatial mode")
	}
	a = tick(t, atlas, Tick{NowNS: 250000000, Received: h.ReplicaRecords, Media: h.Media})
	if a.Result.State != fusion.HazardDetected || len(a.Observations) != 5 || a.Applied != 4 {
		t.Fatalf("received fusion: %+v", a.Result)
	}
	firstDigest := a.Result.InputDigest
	for _, status := range a.ObservationStatus {
		if (status.SourceNode == "atlas") != (status.Delivery == "local") {
			t.Fatal("wrong local/received status")
		}
	}
	repeated := tick(t, atlas, Tick{NowNS: 250000000, LocalSamples: scan, Received: h.ReplicaRecords, Media: h.Media})
	if repeated.Applied != 0 || repeated.Duplicates != 5 || repeated.Result.InputDigest != firstDigest {
		t.Fatal("identical reads changed native versions or original digest")
	}
	newScan, _ := samples(t, "atlas", 2, 3*time.Second)
	a = tick(t, atlas, Tick{NowNS: int64(3 * time.Second), LocalSamples: newScan})
	if len(a.Observations) != 1 || len(a.ReplicaRecords) != 5 || a.Result.State != fusion.InsufficientEvidence || a.Applied != 1 {
		t.Fatal("isolated participant stopped local sensing or kept remote data live")
	}
	for _, m := range a.MissingObservations {
		if m.SourceNode == "hub" && m.Reason != "expired" {
			t.Fatal("expired remote observation mislabeled")
		}
	}
	freshHub, freshMedia := samples(t, "hub", 2, 3250*time.Millisecond)
	freshScan, _ := samples(t, "atlas", 3, 3250*time.Millisecond)
	h = tick(t, hub, Tick{NowNS: int64(3250 * time.Millisecond), LocalSamples: freshHub, Media: freshMedia})
	a = tick(t, atlas, Tick{NowNS: int64(3250 * time.Millisecond), LocalSamples: freshScan, Received: h.ReplicaRecords, Media: h.Media})
	if a.Result.State != fusion.HazardDetected || a.Applied != 5 {
		t.Fatal("fresh rejoin did not restore evidence")
	}
	if err := atlas.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openParticipant(t, "atlas", atlasDir)
	restored := tick(t, reopened, Tick{NowNS: int64(3350 * time.Millisecond)})
	if restored.Result.State != fusion.HazardDetected || len(restored.Observations) != 5 || len(restored.Media) != 3 || restored.Applied != 0 || restored.Result.InputDigest != a.Result.InputDigest {
		t.Fatal("reopen lost records/media or invented new input versions")
	}
	for i, c := range restored.ReplicaRecords {
		prior := a.ReplicaRecords[i]
		c.Timing.AgeHint = 0
		prior.Timing.AgeHint = 0
		if !reflect.DeepEqual(c, prior) {
			t.Fatal("restored original changed")
		}
	}
	expired := tick(t, reopened, Tick{NowNS: int64(6 * time.Second), Received: h.ReplicaRecords})
	if len(expired.Observations) != 0 || len(expired.Media) != 0 || len(expired.ReplicaRecords) != 5 || expired.Duplicates != 4 {
		t.Fatal("duplicates renewed expired observations")
	}
}

func TestReceivedMediaIsLocalAndCatalogCannotBeReboundOnReopen(t *testing.T) {
	root := t.TempDir()
	hub := openParticipant(t, "hub", filepath.Join(root, "hub"))
	dir := filepath.Join(root, "atlas")
	atlas := openParticipant(t, "atlas", dir)
	s, m := samples(t, "hub", 1, 250*time.Millisecond)
	scan, _ := samples(t, "atlas", 1, 250*time.Millisecond)
	h := tick(t, hub, Tick{NowNS: 250000000, LocalSamples: s, Media: m})
	a := tick(t, atlas, Tick{NowNS: 250000000, LocalSamples: scan, Received: h.ReplicaRecords})
	if a.Result.State != fusion.InsufficientEvidence || len(a.Media) != 0 {
		t.Fatal("receiver invented missing local media")
	}
	a = tick(t, atlas, Tick{NowNS: 250000000, Media: h.Media})
	if a.Result.State != fusion.HazardDetected {
		t.Fatal("late delivered exact media not used")
	}
	atlas.Close()
	other := config("iris")
	e, err := New(other, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	if _, err = e.Tick(context.Background(), Tick{NowNS: 250000000}); err == nil {
		t.Fatal("state rebound to another participant")
	}
}

func TestNativeDuplicateConflictOlderUpdateAndClockReopen(t *testing.T) {
	dir := t.TempDir()
	e := openParticipant(t, "atlas", dir)
	s, _ := samples(t, "atlas", 2, time.Second)
	first := tick(t, e, Tick{NowNS: int64(time.Second), LocalSamples: s})
	old, _ := samples(t, "atlas", 1, 500*time.Millisecond)
	got := tick(t, e, Tick{NowNS: int64(time.Second), LocalSamples: old})
	if got.Obsolete != 1 || got.ReplicaRecords[0].Version != first.ReplicaRecords[0].Version {
		t.Fatal("old native sample replaced newer version")
	}
	var changed sensing.Sample
	json.Unmarshal(s[0], &changed)
	changed.Lidar.PointsM[0][0] = 1
	raw, _ := json.Marshal(changed)
	if _, err := e.Tick(context.Background(), Tick{NowNS: int64(time.Second), LocalSamples: []json.RawMessage{raw}}); err == nil {
		t.Fatal("conflicting native duplicate accepted")
	}
	e.Close()
	e2 := openParticipant(t, "atlas", dir)
	if _, err := e2.Tick(context.Background(), Tick{NowNS: 0}); err == nil {
		t.Fatal("reopen reset durable simulation clock")
	}
}
