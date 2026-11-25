package livecapture

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func planarFixture(now time.Time) Snapshot {
	s := fixture(now)
	s.Sequence = 1 << 32
	s.State.SchemaVersion, s.State.RobotID = PlanarStateSchema, "atlas"
	s.State.RunID = s.RunID
	s.State.Position = [3]float64{-1, .24, -3}
	s.State.CommandedVelocityVector = [3]float64{.1, 0, -.2}
	for i := range s.Observations {
		o := &s.Observations[i]
		o.Sequence = s.Sequence
		o.ID = fmt.Sprintf("%s/%s/%d", s.RunID, o.SensorID, o.Sequence)
		o.PositionM = append([]float64(nil), s.State.Position[:]...)
		if o.SensorID == "camera-b" {
			o.PositionM[1] += .45
		}
		if o.Camera != nil {
			o.Camera.CameraToSite[3], o.Camera.CameraToSite[7], o.Camera.CameraToSite[11] = o.PositionM[0], o.PositionM[1], o.PositionM[2]
		}
	}
	return s
}

func TestPlanarCaptureExplicitSchemaOriginalSamplesAndEndpoint(t *testing.T) {
	now := time.Unix(1800000000, 0)
	s := planarFixture(now)
	cfg := testConfig(&now)
	if _, err := Decode(encode(t, s), cfg); err == nil {
		t.Fatal("default decoder accepted planar capture")
	}
	cfg.Planar = &PlanarConfig{RobotID: "atlas", FloorY: .24}
	got, err := Decode(encode(t, s), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got.State.CommandedVelocityVector != s.State.CommandedVelocityVector || got.Observations[0].ID != s.Observations[0].ID || got.SampleUnixMS != s.SampleUnixMS || got.SimulationTimeNS != s.SimulationTimeNS {
		t.Fatal("original planar capture changed")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/inspection/samples" {
			t.Error("wrong endpoint", r.URL.Path)
		}
		_, _ = w.Write(encode(t, s))
	}))
	defer server.Close()
	cfg.URL = server.URL
	client, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	first, err := client.Capture(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	again, err := client.Capture(context.Background())
	if err != nil || again.SampleUnixMS != first.SampleUnixMS {
		t.Fatal("duplicate rebased", err)
	}
	now = now.Add(501 * time.Millisecond)
	if _, err = client.Capture(context.Background()); err == nil {
		t.Fatal("expired capture became fresh")
	}
}

func TestPlanarCaptureRefusesChangedRobotMountAndEvidence(t *testing.T) {
	now := time.Unix(1800000000, 0)
	cfg := testConfig(&now)
	cfg.Planar = &PlanarConfig{RobotID: "atlas", FloorY: .24}
	for _, mode := range []string{"run", "robot", "height", "vertical", "mount", "sequence", "media", "extra", "scalar-command", "short-command", "null-command", "unknown-state-field"} {
		t.Run(mode, func(t *testing.T) {
			s := planarFixture(now)
			switch mode {
			case "run":
				s.State.RunID = "other"
			case "robot":
				s.State.RobotID = "iris"
			case "height":
				s.State.Position[1] = .2
			case "vertical":
				s.State.CommandedVelocityVector[1] = .1
			case "mount":
				s.Observations[1].PositionM[2]++
				s.Observations[1].Camera.CameraToSite[11]++
			case "sequence":
				s.Observations[2].Sequence++
			case "media":
				s.Media[0].Data[0] ^= 1
			case "extra":
				s.Observations = append(s.Observations, s.Observations[0])
			}
			raw := encode(t, s)
			var fields map[string]json.RawMessage
			_ = json.Unmarshal(raw, &fields)
			var state map[string]json.RawMessage
			_ = json.Unmarshal(fields["state"], &state)
			switch mode {
			case "scalar-command":
				state["commanded_velocity_mps"] = []byte("0")
			case "short-command":
				state["commanded_velocity_mps"] = []byte("[0,0]")
			case "null-command":
				state["commanded_velocity_mps"] = []byte("[0,null,0]")
			case "unknown-state-field":
				state["invented"] = []byte("1")
			}
			fields["state"], _ = json.Marshal(state)
			raw, _ = json.Marshal(fields)
			if _, err := Decode(raw, cfg); err == nil {
				t.Fatal("invalid planar capture accepted")
			}
		})
	}
}
