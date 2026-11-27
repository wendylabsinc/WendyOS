package sensorrecord

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/livecapture"
	"github.com/wendylabsinc/wendy/go/world/internal/sensing"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
)

var testAnchor = time.Unix(1700000000, 0).UTC()
var testWall = time.Unix(1800000000, 123000000).UTC()

func testImage(t *testing.T, kind string) []byte {
	t.Helper()
	im := image.NewNRGBA(image.Rect(0, 0, 4, 3))
	for y := 0; y < 3; y++ {
		for x := 0; x < 4; x++ {
			im.Set(x, y, color.NRGBA{R: 240, G: uint8(x * 10), B: uint8(y * 10), A: 255})
		}
	}
	var output bytes.Buffer
	var err error
	if kind == "image/png" {
		err = png.Encode(&output, im)
	} else {
		err = jpeg.Encode(&output, im, &jpeg.Options{Quality: 90})
	}
	if err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func testWAV() []byte {
	const samples = 2000
	raw := make([]byte, 44+samples*2)
	copy(raw, "RIFF")
	binary.LittleEndian.PutUint32(raw[4:], uint32(len(raw)-8))
	copy(raw[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(raw[16:], 16)
	binary.LittleEndian.PutUint16(raw[20:], 1)
	binary.LittleEndian.PutUint16(raw[22:], 1)
	binary.LittleEndian.PutUint32(raw[24:], 8000)
	binary.LittleEndian.PutUint32(raw[28:], 16000)
	binary.LittleEndian.PutUint16(raw[32:], 2)
	binary.LittleEndian.PutUint16(raw[34:], 16)
	copy(raw[36:], "data")
	binary.LittleEndian.PutUint32(raw[40:], samples*2)
	for i := range samples {
		value := int16(10000 * math.Sin(2*math.Pi*880*float64(i)/8000))
		binary.LittleEndian.PutUint16(raw[44+i*2:], uint16(value))
	}
	return raw
}

func testSample(t *testing.T, kind string) (sensing.Sample, []byte) {
	t.Helper()
	id := map[string]string{"camera": "camera-a", "gas": "gas-1", "audio": "audio-1"}[kind]
	s := sensing.Sample{SchemaVersion: sensing.Schema, ID: "run-a/" + id + "/17", SiteID: "synthetic-office", Frame: world.FrameRef{ID: "floor", Revision: "frame-v1"}, SensorID: id, Kind: kind, Sequence: 17, SampleNS: int64(250 * time.Millisecond), RunID: "run-a", ScenarioVersion: "mujoco-4-live-v1", Seed: 7, PackageSHA256: strings.Repeat("a", 64), SensorModel: "test-" + kind, PositionM: []float64{-2, .2, 0}, UncertaintyM: .04, CalibrationRevision: "cal-v1"}
	var data []byte
	contentType := ""
	switch kind {
	case "camera":
		s.Camera = &sensing.Camera{Width: 4, Height: 3, FX: 4, FY: 4, CX: 2, CY: 1.5, CameraToSite: []float64{1, 0, 0, -2, 0, 1, 0, .2, 0, 0, 1, 0, 0, 0, 0, 1}, CalibrationValidForNS: int64(time.Minute)}
		data, contentType = testImage(t, "image/png"), "image/png"
	case "audio":
		s.Audio = &sensing.Audio{SampleRateHz: 8000, Channels: 1, DurationNS: int64(250 * time.Millisecond)}
		data, contentType = testWAV(), "audio/wav"
	case "gas":
		s.Gas = &sensing.Gas{ValuePPM: 73.25, UncertaintyPPM: 2.7, Species: "synthetic-test-gas"}
	}
	if contentType != "" {
		s.Evidence = &sensing.EvidenceRef{SHA256: digest(data), ContentType: contentType, Size: int64(len(data))}
	}
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
	return s, data
}

func testCapture(t *testing.T, kind string) (Capture, []byte) {
	t.Helper()
	sample, data := testSample(t, kind)
	raw, err := json.MarshalIndent(sample, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return Capture{Version: Version, SceneBootID: "boot-original", CapturedUnixNS: testWall.UnixNano(), SimulationAnchorUnixNS: testAnchor.UnixNano(), SampleJSON: append(append([]byte("\n  "), raw...), '\n')}, data
}

func testPublication(t *testing.T, kind string) serviceorigin.PublishRequest {
	t.Helper()
	capture, _ := testCapture(t, kind)
	request, err := Publication(capture, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return request
}

func testComponent(p serviceorigin.PublishRequest) world.Component {
	return world.Component{Key: world.Key{Tenant: "test-tenant", Site: "synthetic-office", Entity: p.Entity, Type: p.ComponentType, Source: world.Source{Device: "actual-device", App: "actual-app", Service: "actual-reader-service", Stream: p.Stream}}, Version: world.Version{Epoch: 29, Sequence: 3}, Frame: p.Frame, Timing: p.Timing, TTL: p.TTL, Provenance: p.Provenance, Payload: p.Payload, Tombstone: p.Tombstone}
}

func TestRoundTripPreservesOriginalBytesAndNativeSequence(t *testing.T) {
	for _, kind := range []string{"camera", "gas", "audio"} {
		t.Run(kind, func(t *testing.T) {
			capture, media := testCapture(t, kind)
			raw, err := Encode(capture)
			if err != nil {
				t.Fatal(err)
			}
			decoded, err := Decode(raw)
			if err != nil || !reflect.DeepEqual(decoded, capture) {
				t.Fatalf("capture changed: %v", err)
			}
			if err := ValidateMedia(decoded, media); err != nil {
				t.Fatal(err)
			}
			request := testPublication(t, kind)
			component := testComponent(request)
			before, _ := json.Marshal(component)
			from, err := FromComponent(component)
			if err != nil {
				t.Fatal(err)
			}
			sample, err := from.Sample()
			if err != nil || sample.Sequence != 17 || component.Version.Sequence != 3 || component.Key.Source.Service == sample.SensorID || request.Entity != "sensor/"+sample.SensorID || !bytes.Equal(from.SampleJSON, capture.SampleJSON) {
				t.Fatalf("native/agent identities or original bytes changed: %v", err)
			}
			after, _ := json.Marshal(component)
			if !bytes.Equal(before, after) {
				t.Fatal("validator mutated signed component")
			}
			sample.PositionM[0]++
			from.SampleJSON[0] = ' '
			again, _ := FromComponent(component)
			original, _ := again.Sample()
			if original.PositionM[0] != -2 || !bytes.Equal(again.SampleJSON, capture.SampleJSON) {
				t.Fatal("returned values alias original record")
			}
		})
	}
}

func TestStrictCaptureShapeAndPayloadBudget(t *testing.T) {
	capture, _ := testCapture(t, "gas")
	raw, _ := Encode(capture)
	for _, field := range []string{"version", "scene_boot_id", "captured_unix_ns", "simulation_anchor_unix_ns", "sample_json"} {
		for _, mode := range []string{"missing", "null"} {
			t.Run(field+"/"+mode, func(t *testing.T) {
				var fields map[string]json.RawMessage
				_ = json.Unmarshal(raw, &fields)
				if mode == "missing" {
					delete(fields, field)
				} else {
					fields[field] = []byte("null")
				}
				bad, _ := json.Marshal(fields)
				if _, err := Decode(bad); err == nil {
					t.Fatal("accepted missing/null field")
				}
			})
		}
	}
	for name, bad := range map[string][]byte{
		"duplicate":     bytes.Replace(raw, []byte(`"version":1`), []byte(`"version":1,"version":1`), 1),
		"alias":         bytes.Replace(raw, []byte(`"version"`), []byte(`"Version"`), 1),
		"unknown":       append(append([]byte{}, raw[:len(raw)-1]...), []byte(`,"sequence":3}`)...),
		"null optional": append(append([]byte{}, raw[:len(raw)-1]...), []byte(`,"sensorlink":null}`)...),
		"trailing":      append(append([]byte{}, raw...), []byte(` {}`)...),
		"oversize":      bytes.Repeat([]byte(" "), MaxPayloadBytes+1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Decode(bad); err == nil {
				t.Fatal("accepted ambiguous capture JSON")
			}
		})
	}
	for name, change := range map[string]func(*Capture){
		"missing boot":            func(c *Capture) { c.SceneBootID = "" },
		"invalid boot Unicode":    func(c *Capture) { c.SceneBootID = string([]byte{0xff}) },
		"zero wall":               func(c *Capture) { c.CapturedUnixNS = 0 },
		"zero anchor":             func(c *Capture) { c.SimulationAnchorUnixNS = 0 },
		"overflow anchor":         func(c *Capture) { c.SimulationAnchorUnixNS = math.MaxInt64 },
		"sample alias":            func(c *Capture) { c.SampleJSON = bytes.Replace(c.SampleJSON, []byte(`"seed"`), []byte(`"Seed"`), 1) },
		"missing native sequence": func(c *Capture) { c.SampleJSON = bytes.Replace(c.SampleJSON, []byte(`"sequence": 17,`), nil, 1) },
		"invalid Unicode": func(c *Capture) {
			c.SampleJSON = bytes.Replace(c.SampleJSON, []byte(`"test-gas"`), []byte(`"\ud800"`), 1)
		},
		"base64 expansion budget": func(c *Capture) { c.SampleJSON = append(bytes.Repeat([]byte(" "), 25<<10), c.SampleJSON...) },
	} {
		t.Run(name, func(t *testing.T) {
			bad, _ := testCapture(t, "gas")
			change(&bad)
			if _, err := Encode(bad); err == nil {
				t.Fatal("accepted invalid capture")
			}
		})
	}
}

func TestPublicationRejectsMetadataMismatch(t *testing.T) {
	for name, change := range map[string]func(*serviceorigin.PublishRequest){
		"wrong type":              func(p *serviceorigin.PublishRequest) { p.ComponentType = sensing.ComponentType },
		"unstable entity":         func(p *serviceorigin.PublishRequest) { p.Entity = "run-a/gas-1/17" },
		"wrong stream":            func(p *serviceorigin.PublishRequest) { p.Stream = "gas-2" },
		"wrong frame":             func(p *serviceorigin.PublishRequest) { p.Frame.Revision += "-new" },
		"tombstone":               func(p *serviceorigin.PublishRequest) { p.Tombstone = true },
		"zero ttl":                func(p *serviceorigin.PublishRequest) { p.TTL = 0 },
		"long ttl":                func(p *serviceorigin.PublishRequest) { p.TTL = MaxLifetime + 1 },
		"wall time as simulation": func(p *serviceorigin.PublishRequest) { p.Timing.SampleTime = testWall },
		"receipt restamp":         func(p *serviceorigin.PublishRequest) { p.Timing.SampleTime = p.Timing.SampleTime.Add(time.Nanosecond) },
		"wrong domain":            func(p *serviceorigin.PublishRequest) { p.Timing.ClockDomain = "physical" },
		"unknown error":           func(p *serviceorigin.PublishRequest) { p.Timing.ErrorKnown = false },
		"negative error":          func(p *serviceorigin.PublishRequest) { p.Timing.ErrorBound = -1 },
		"negative age":            func(p *serviceorigin.PublishRequest) { p.Timing.AgeHint = -1 },
		"error beyond ttl":        func(p *serviceorigin.PublishRequest) { p.Timing.ErrorBound = p.TTL },
		"wrong run":               func(p *serviceorigin.PublishRequest) { p.Provenance.Simulation.RunID = "run-b" },
		"wrong scenario":          func(p *serviceorigin.PublishRequest) { p.Provenance.Simulation.ScenarioVersion += "-bad" },
		"wrong seed":              func(p *serviceorigin.PublishRequest) { p.Provenance.Simulation.Seed++ },
		"wrong model":             func(p *serviceorigin.PublishRequest) { p.Provenance.ModelID += "-bad" },
		"wrong model version":     func(p *serviceorigin.PublishRequest) { p.Provenance.ModelVersion += "-bad" },
		"hardware claim": func(p *serviceorigin.PublishRequest) {
			p.Provenance = world.Provenance{Origin: world.Measured, AppVersion: "v1"}
		},
		"forged ancestry": func(p *serviceorigin.PublishRequest) {
			p.Provenance.Inputs = []world.Input{{ID: "fake", Origin: world.Measured}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			request := testPublication(t, "gas")
			change(&request)
			if _, err := ValidatePublication(request); err == nil {
				t.Fatal("accepted inconsistent publication")
			}
		})
	}
	for name, change := range map[string]func(*world.Component){
		"site":     func(c *world.Component) { c.Key.Site = "other" },
		"tenant":   func(c *world.Component) { c.Key.Tenant = "" },
		"service":  func(c *world.Component) { c.Key.Source.Service = "" },
		"epoch":    func(c *world.Component) { c.Version.Epoch = 0 },
		"sequence": func(c *world.Component) { c.Version.Sequence = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			component := testComponent(testPublication(t, "gas"))
			change(&component)
			if _, err := FromComponent(component); err == nil {
				t.Fatal("accepted incomplete agent component")
			}
		})
	}
}

func snapshotFixture(t *testing.T) livecapture.Snapshot {
	t.Helper()
	frame := world.FrameRef{ID: "floor", Revision: "frame-v1"}
	snapshot := livecapture.Snapshot{SchemaVersion: livecapture.Schema, RunID: "run-a", BootID: "boot-original", Sequence: 17, SampleUnixMS: testWall.UnixMilli(), SimulationTimeNS: int64(250 * time.Millisecond), Observations: []sensing.Sample{}, Media: []livecapture.Media{}}
	snapshot.State = livecapture.State{SchemaVersion: "wendy.sim.actuator.v0", Synthetic: true, BootID: snapshot.BootID, Session: "session-1", SiteID: "synthetic-office", Frame: frame, Position: [3]float64{-2, .2, 0}, SampleUnixMS: snapshot.SampleUnixMS, SimulationTimeS: .25, Sensors: map[string]bool{"gas": true, "local-obstacles": true}, ObstacleClear: true, Energy: 1, WatchdogStopped: true, Provenance: livecapture.Provenance{Mode: "simulated", PackageSHA256: strings.Repeat("a", 64), RobotModel: "single-axis-puck-ideal-brake", GasModel: "analytic-gaussian-fixture"}}
	seen := map[string]bool{}
	for _, id := range []string{"camera-a", "camera-b", "gas-1", "audio-1"} {
		kind := map[string]string{"camera-a": "camera", "camera-b": "camera", "gas-1": "gas", "audio-1": "audio"}[id]
		sample, media := testSample(t, kind)
		sample.ID, sample.SensorID = "run-a/"+id+"/17", id
		if id == "camera-b" {
			sample.PositionM[1], sample.PositionM[2] = 2, 2
			sample.Camera.CameraToSite[7], sample.Camera.CameraToSite[11] = 2, 2
		}
		if sample.Evidence != nil && !seen[sample.Evidence.SHA256] {
			snapshot.Media = append(snapshot.Media, livecapture.Media{SHA256: sample.Evidence.SHA256, Data: media})
			seen[sample.Evidence.SHA256] = true
		}
		snapshot.Observations = append(snapshot.Observations, sample)
	}
	return snapshot
}

func TestSnapshotPreservesMovingCalibrationAndIndependentClocks(t *testing.T) {
	first := snapshotFixture(t)
	p1, image1, err := FromSnapshot(first, "camera-b", testAnchor, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	next := snapshotFixture(t)
	next.Sequence++
	next.SimulationTimeNS += int64(250 * time.Millisecond)
	next.SampleUnixMS += 4000 // physics advances 250ms while wall time advances 4s
	next.State.SampleUnixMS, next.State.SimulationTimeS = next.SampleUnixMS, .5
	next.State.Position[0] += .1
	for i := range next.Observations {
		s := &next.Observations[i]
		s.Sequence, s.SampleNS = next.Sequence, next.SimulationTimeNS
		s.ID = fmt.Sprintf("%s/%s/%d", s.RunID, s.SensorID, s.Sequence)
		if s.SensorID != "camera-a" {
			s.PositionM[0] += .1
			if s.Camera != nil {
				s.Camera.CameraToSite[3] += .1
			}
		}
	}
	p2, image2, err := FromSnapshot(next, "camera-b", testAnchor, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	c1, _ := Decode(p1.Payload)
	c2, _ := Decode(p2.Payload)
	s1, _ := c1.Sample()
	s2, _ := c2.Sample()
	h1, _ := CalibrationDigest(s1)
	h2, _ := CalibrationDigest(s2)
	if c1.SimulationAnchorUnixNS != c2.SimulationAnchorUnixNS || c2.CapturedUnixNS-c1.CapturedUnixNS != int64(4*time.Second) || p2.Timing.SampleTime.Sub(p1.Timing.SampleTime) != 250*time.Millisecond || s1.Camera.CameraToSite[3] == s2.Camera.CameraToSite[3] || h1 == h2 || s2.Sequence != 18 || !bytes.Equal(image1, image2) {
		t.Fatal("conversion rebased time, lost native sequence or froze moving calibration")
	}
	if !reflect.DeepEqual(s2, next.Observations[1]) {
		t.Fatal("sample metadata did not survive conversion")
	}
	audio, wav, err := FromSnapshot(next, "audio-1", testAnchor, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	audioCapture, _ := Decode(audio.Payload)
	sound, _ := audioCapture.Sample()
	if sound.Audio.DurationNS != int64(250*time.Millisecond) || !audio.Timing.SampleTime.Add(-time.Duration(sound.Audio.DurationNS)).Equal(testAnchor.Add(250*time.Millisecond)) || !bytes.Equal(wav, testWAV()) {
		t.Fatal("audio capture interval or bytes changed")
	}
	gas, media, err := FromSnapshot(next, "gas-1", testAnchor, 2*time.Second)
	if err != nil || len(media) != 0 {
		t.Fatalf("gas conversion: %v", err)
	}
	gasCapture, _ := Decode(gas.Payload)
	gasSample, _ := gasCapture.Sample()
	if !reflect.DeepEqual(gasSample.Gas, next.Observations[2].Gas) || !reflect.DeepEqual(gasSample.PositionM, next.State.Position[:]) {
		t.Fatal("gas value, uncertainty or placement changed")
	}
}

func TestSnapshotRejectsMissingMediaAndMismatchedCapture(t *testing.T) {
	for name, change := range map[string]func(*livecapture.Snapshot){
		"missing media":  func(s *livecapture.Snapshot) { s.Media = s.Media[:1] },
		"corrupt media":  func(s *livecapture.Snapshot) { s.Media[0].Data[0] ^= 1 },
		"sequence":       func(s *livecapture.Snapshot) { s.Observations[0].Sequence++ },
		"boot":           func(s *livecapture.Snapshot) { s.State.BootID += "-changed" },
		"time":           func(s *livecapture.Snapshot) { s.SampleUnixMS++ },
		"lineage":        func(s *livecapture.Snapshot) { s.Observations[1].Seed++ },
		"audio interval": func(s *livecapture.Snapshot) { s.Observations[3].Audio.DurationNS += 1 },
		"missing sensor": func(s *livecapture.Snapshot) { s.Observations = s.Observations[:3] },
		"media shape":    func(s *livecapture.Snapshot) { s.Observations[0].Camera.Width++ },
	} {
		t.Run(name, func(t *testing.T) {
			snapshot := snapshotFixture(t)
			change(&snapshot)
			if _, _, err := FromSnapshot(snapshot, "camera-a", testAnchor, time.Second); err == nil {
				t.Fatal("accepted invalid snapshot")
			}
		})
	}
	if _, _, err := FromSnapshot(snapshotFixture(t), "absent", testAnchor, time.Second); err == nil {
		t.Fatal("accepted missing selected sensor")
	}
}

func TestSensorLinkBindsOriginalCaptureAndReceivedJPEG(t *testing.T) {
	capture, originalPNG := testCapture(t, "camera")
	frame := SensorLinkFrame{ChannelID: 4, Seq: 0, TsUs: uint64(capture.CapturedUnixNS / 1000), Flags: 1, Payload: testImage(t, "image/jpeg")}
	wire, err := BindSensorLink(capture, frame)
	if err != nil {
		t.Fatal(err)
	}
	original, _ := wire.Sample()
	interpreted, _ := wire.NormalizedSample()
	ref, _ := wire.Evidence()
	if !bytes.Equal(wire.SampleJSON, capture.SampleJSON) || original.Evidence.ContentType != "image/png" || original.Evidence.SHA256 != digest(originalPNG) || interpreted.Evidence.ContentType != "image/jpeg" || *ref != wire.SensorLink.Evidence || ref.SHA256 != digest(frame.Payload) || wire.SensorLink.CaptureSequence != 17 || wire.SensorLink.FrameSequence != 0 || interpreted.Sequence != 17 || interpreted.SensorLink != nil {
		t.Fatal("wire bytes replaced the original source or invented sequence/legacy clock mapping")
	}
	if err := ValidateMedia(wire, originalPNG); err == nil {
		t.Fatal("accepted original PNG as received JPEG")
	}
	p, err := Publication(wire, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	component := testComponent(p)
	if _, err := FromComponent(component); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*Capture){
		"boot":               func(c *Capture) { c.SensorLink.SceneBootID += "-wrong" },
		"native sequence":    func(c *Capture) { c.SensorLink.CaptureSequence++ },
		"wall":               func(c *Capture) { c.SensorLink.CapturedUnixNS++ },
		"timestamp":          func(c *Capture) { c.SensorLink.TimestampUS++ },
		"sample digest":      func(c *Capture) { c.SensorLink.SampleSHA256 = strings.Repeat("b", 64) },
		"calibration digest": func(c *Capture) { c.SensorLink.CalibrationSHA256 = strings.Repeat("b", 64) },
		"frame flags":        func(c *Capture) { c.SensorLink.Flags = 2 },
		"channel":            func(c *Capture) { c.SensorLink.ChannelID = 0 },
		"missing digest":     func(c *Capture) { c.SensorLink.Evidence.SHA256 = "" },
		"wrong format":       func(c *Capture) { c.SensorLink.Evidence.ContentType = "image/png" },
	} {
		t.Run(name, func(t *testing.T) {
			encoded, _ := Encode(wire)
			bad, _ := Decode(encoded)
			change(&bad)
			if _, err := Encode(bad); err == nil {
				t.Fatal("accepted inconsistent wire binding")
			}
		})
	}
	for _, name := range []string{"frame_sequence", "flags", "captured_unix_ns", "calibration_sha256", "sample_sha256"} {
		encoded, _ := Encode(wire)
		var fields map[string]json.RawMessage
		_ = json.Unmarshal(encoded, &fields)
		var nested map[string]json.RawMessage
		_ = json.Unmarshal(fields["sensorlink"], &nested)
		delete(nested, name)
		fields["sensorlink"], _ = json.Marshal(nested)
		encoded, _ = json.Marshal(fields)
		if _, err := Decode(encoded); err == nil {
			t.Fatalf("accepted missing wire field %s", name)
		}
	}
	for _, bad := range []SensorLinkFrame{
		{ChannelID: frame.ChannelID, Seq: frame.Seq, TsUs: frame.TsUs, Flags: frame.Flags},
		{ChannelID: frame.ChannelID, Seq: frame.Seq, TsUs: frame.TsUs + 1, Flags: frame.Flags, Payload: frame.Payload},
		{ChannelID: frame.ChannelID, Seq: frame.Seq, TsUs: frame.TsUs, Flags: frame.Flags, Payload: originalPNG},
	} {
		if _, err := BindSensorLink(capture, bad); err == nil {
			t.Fatal("accepted incomplete, mismatched or non-JPEG frame")
		}
	}
}

func TestMediaInterpretationAndAudioInterval(t *testing.T) {
	for _, kind := range []string{"camera", "audio"} {
		capture, data := testCapture(t, kind)
		if err := ValidateMedia(capture, nil); err == nil {
			t.Fatal("accepted missing media")
		}
		bad := append([]byte(nil), data...)
		bad[len(bad)-1] ^= 1
		if err := ValidateMedia(capture, bad); err == nil {
			t.Fatal("accepted corrupted media")
		}
		sample, _ := capture.Sample()
		if kind == "camera" {
			sample.Camera.Width++
		} else {
			sample.Audio.DurationNS -= 1 // still a valid interval, different PCM length
		}
		capture.SampleJSON, _ = json.Marshal(sample)
		if err := ValidateMedia(capture, data); err == nil {
			t.Fatal("accepted bytes inconsistent with image/audio metadata")
		}
	}
	capture, _ := testCapture(t, "gas")
	if err := ValidateMedia(capture, []byte("extra")); err == nil {
		t.Fatal("accepted gas evidence bytes")
	}
}
