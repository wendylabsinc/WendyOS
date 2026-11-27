package livecapture

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/sensing"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

func fixture(now time.Time) Snapshot {
	frame := world.FrameRef{ID: "office-floor", Revision: "office-frame-v1"}
	p := strings.Repeat("a", 64)
	s := Snapshot{SchemaVersion: Schema, RunID: "test", BootID: "boot-1", Sequence: 1, SampleUnixMS: now.UnixMilli(), SimulationTimeNS: 250000000, Observations: []sensing.Sample{}, Media: []Media{}}
	s.State = State{SchemaVersion: "wendy.sim.actuator.v0", Synthetic: true, BootID: s.BootID, Session: "session-1", SiteID: "synthetic-office", Frame: frame, Position: [3]float64{-2, .2, 0}, SampleUnixMS: s.SampleUnixMS, SimulationTimeS: .25, Sensors: map[string]bool{"gas": true, "local-obstacles": true}, ObstacleClear: true, Energy: 1, WatchdogStopped: true, Provenance: Provenance{Mode: "simulated", PackageSHA256: p, RobotModel: "single-axis-puck-ideal-brake", GasModel: "analytic-gaussian-fixture"}}
	for _, id := range []string{"camera-a", "camera-b", "gas-1", "audio-1"} {
		x := sensing.Sample{SchemaVersion: sensing.Schema, ID: "test/" + id + "/1", SiteID: "synthetic-office", Frame: frame, SensorID: id, Sequence: 1, SampleNS: 250000000, RunID: "test", ScenarioVersion: "live-v1", PackageSHA256: p, SensorModel: "fixture", PositionM: []float64{-2, .2, 0}, UncertaintyM: .01, CalibrationRevision: "cal-v1"}
		if id == "camera-b" {
			x.PositionM = []float64{-2, 2, 2}
		}
		if id == "gas-1" {
			x.Kind = "gas"
			x.Gas = &sensing.Gas{ValuePPM: 60, UncertaintyPPM: 1, Species: "synthetic-test-gas"}
		} else {
			data := []byte("opaque test capture " + id)
			sum := sha256.Sum256(data)
			ref := sensing.EvidenceRef{SHA256: hex.EncodeToString(sum[:]), Size: int64(len(data))}
			if strings.HasPrefix(id, "camera") {
				x.Kind = "camera"
				ref.ContentType = "image/png"
				x.Camera = &sensing.Camera{Width: 1, Height: 1, FX: 1, FY: 1, CX: .5, CY: .5, CameraToSite: []float64{1, 0, 0, x.PositionM[0], 0, 1, 0, x.PositionM[1], 0, 0, 1, x.PositionM[2], 0, 0, 0, 1}, CalibrationValidForNS: int64(time.Minute)}
			} else {
				x.Kind = "audio"
				ref.ContentType = "audio/wav"
				x.Audio = &sensing.Audio{SampleRateHz: 8000, Channels: 1, DurationNS: 250000000}
			}
			x.Evidence = &ref
			s.Media = append(s.Media, Media{SHA256: ref.SHA256, Data: data})
		}
		s.Observations = append(s.Observations, x)
	}
	return s
}
func testConfig(now *time.Time) Config {
	return Config{URL: "http://127.0.0.1", Token: strings.Repeat("x", 32), RunID: "test", Site: "synthetic-office", Frame: world.FrameRef{ID: "office-floor", Revision: "office-frame-v1"}, Now: func() time.Time { return *now }}
}
func encode(t *testing.T, s Snapshot) []byte {
	t.Helper()
	raw, e := json.Marshal(s)
	if e != nil {
		t.Fatal(e)
	}
	return raw
}
func TestDecodeShapeScopeAndIntegrity(t *testing.T) {
	now := time.Unix(1800000000, 0)
	good := fixture(now)
	cfg := testConfig(&now)
	if _, e := Decode(encode(t, good), cfg); e != nil {
		t.Fatal(e)
	}
	cases := map[string]func(*Snapshot){
		"wrong run":           func(s *Snapshot) { s.RunID = "other" },
		"wrong frame":         func(s *Snapshot) { s.State.Frame.Revision = "other" },
		"future":              func(s *Snapshot) { s.SampleUnixMS++ },
		"stale":               func(s *Snapshot) { s.SampleUnixMS -= 501; s.State.SampleUnixMS = s.SampleUnixMS },
		"state time":          func(s *Snapshot) { s.State.SampleUnixMS-- },
		"state boot":          func(s *Snapshot) { s.State.BootID = "different" },
		"simulation mismatch": func(s *Snapshot) { s.SimulationTimeNS++; s.SimulationTimeNS++ },
		"sequence mismatch":   func(s *Snapshot) { s.Observations[0].Sequence++ },
		"corruption":          func(s *Snapshot) { s.Media[0].Data[0] ^= 1 },
		"missing blob":        func(s *Snapshot) { s.Media = s.Media[:2] },
		"extra blob":          func(s *Snapshot) { s.Media = append(s.Media, s.Media[0]) },
		"duplicate sensor":    func(s *Snapshot) { s.Observations[1] = s.Observations[0] },
		"unavailable gas":     func(s *Snapshot) { s.State.Sensors["gas"] = false },
		"package changed":     func(s *Snapshot) { s.Observations[1].PackageSHA256 = strings.Repeat("b", 64) },
		"empty observations":  func(s *Snapshot) { s.Observations = nil; s.Media = nil },
		"missing gas":         func(s *Snapshot) { s.Observations = append(s.Observations[:2], s.Observations[3:]...) },
		"missing audio":       func(s *Snapshot) { s.Observations = s.Observations[:3]; s.Media = s.Media[:2] },
		"gas pose":            func(s *Snapshot) { s.Observations[2].PositionM[0]++ },
		"audio pose":          func(s *Snapshot) { s.Observations[3].PositionM[0]++ },
		"camera pose": func(s *Snapshot) {
			s.Observations[1].PositionM[0]++
			s.Observations[1].Camera.CameraToSite[3]++
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			s := fixture(now)
			change(&s)
			if _, e := Decode(encode(t, s), cfg); e == nil {
				t.Fatal("accepted invalid capture")
			}
		})
	}
	fault := fixture(now)
	fault.State.Sensors["gas"] = false
	fault.Observations = append(fault.Observations[:2], fault.Observations[3:]...)
	if _, e := Decode(encode(t, fault), cfg); e != nil {
		t.Fatalf("gas fault omission rejected: %v", e)
	}
	var object map[string]any
	if e := json.Unmarshal(encode(t, good), &object); e != nil {
		t.Fatal(e)
	}
	state := object["state"].(map[string]any)
	for _, value := range []any{nil, []any{-2, .2}, []any{-2, .2, nil}} {
		state["position_m"] = value
		raw, _ := json.Marshal(object)
		if _, e := Decode(raw, cfg); e == nil {
			t.Fatal("accepted invalid vector")
		}
	}
	raw := encode(t, good)
	raw = bytes.Replace(raw, []byte(`"sequence":1`), []byte(`"sequence":1,"sequence":1`), 1)
	if _, e := Decode(raw, cfg); e == nil {
		t.Fatal("accepted duplicate fields")
	}
	if _, e := Decode(bytes.Repeat([]byte(" "), MaxResponseBytes+1), cfg); e == nil {
		t.Fatal("accepted oversized response")
	}
}
func TestCaptureFencesAndDuplicateFreshness(t *testing.T) {
	now := time.Unix(1800000000, 0)
	snapshot := fixture(now)
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/samples" || r.Header.Get("Authorization") != "Bearer "+strings.Repeat("x", 32) {
			t.Error("wrong request")
		}
		mu.Lock()
		defer mu.Unlock()
		json.NewEncoder(w).Encode(snapshot)
	}))
	defer server.Close()
	cfg := testConfig(&now)
	cfg.URL = server.URL
	c, e := New(cfg)
	if e != nil {
		t.Fatal(e)
	}
	first, e := c.Capture(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	duplicate, e := c.Capture(context.Background())
	if e != nil || duplicate.SampleUnixMS != first.SampleUnixMS {
		t.Fatalf("duplicate changed capture time: %v", e)
	}
	now = now.Add(501 * time.Millisecond)
	if _, e = c.Capture(context.Background()); !errors.Is(e, ErrStale) {
		t.Fatalf("duplicate renewed freshness: %v", e)
	}
	now = now.Add(-501 * time.Millisecond)
	mu.Lock()
	snapshot.BootID = "boot-2"
	snapshot.State.BootID = "boot-2"
	mu.Unlock()
	if _, e = c.Capture(context.Background()); !errors.Is(e, ErrBootChanged) {
		t.Fatalf("boot not fenced: %v", e)
	}
	mu.Lock()
	snapshot = fixture(now)
	mu.Unlock()
	if _, e = c.Capture(context.Background()); !errors.Is(e, ErrBootChanged) {
		t.Fatal("boot fault unlatched")
	}
}
func TestIntegrityFailureRequiresNewClient(t *testing.T) {
	now := time.Unix(1800000000, 0)
	snapshot := fixture(now)
	snapshot.Media[0].Data[0] ^= 1
	raw := encode(t, snapshot)
	var mu sync.Mutex
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		requests++
		w.Write(raw)
	}))
	defer server.Close()
	cfg := testConfig(&now)
	cfg.URL = server.URL
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.Capture(context.Background()); err == nil {
		t.Fatal("accepted corrupt media")
	}
	mu.Lock()
	raw = encode(t, fixture(now))
	mu.Unlock()
	if _, err = c.Capture(context.Background()); err == nil {
		t.Fatal("integrity failure did not latch")
	}
	mu.Lock()
	defer mu.Unlock()
	if requests != 1 {
		t.Fatal("failed client fetched another snapshot")
	}
}
func TestSameSequenceMutationAndRegressionLatch(t *testing.T) {
	for _, mode := range []string{"mutation", "regression"} {
		t.Run(mode, func(t *testing.T) {
			now := time.Unix(1800000000, 0)
			snapshot := fixture(now)
			var mu sync.Mutex
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				json.NewEncoder(w).Encode(snapshot)
			}))
			defer server.Close()
			cfg := testConfig(&now)
			cfg.URL = server.URL
			c, _ := New(cfg)
			if _, e := c.Capture(context.Background()); e != nil {
				t.Fatal(e)
			}
			mu.Lock()
			if mode == "mutation" {
				snapshot.State.GasPPM = 1
			} else {
				snapshot.SampleUnixMS--
				snapshot.State.SampleUnixMS = snapshot.SampleUnixMS
			}
			mu.Unlock()
			if _, e := c.Capture(context.Background()); e == nil {
				t.Fatal("accepted changed snapshot")
			}
			mu.Lock()
			snapshot = fixture(now)
			mu.Unlock()
			if _, e := c.Capture(context.Background()); e == nil {
				t.Fatal("fault did not latch")
			}
		})
	}
}
func TestClientRejectsRedirectWithoutForwardingToken(t *testing.T) {
	touched := false
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { touched = true }))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, 302) }))
	defer source.Close()
	now := time.Now()
	cfg := testConfig(&now)
	cfg.URL = source.URL
	c, e := New(cfg)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = c.Capture(context.Background()); e == nil || touched {
		t.Fatal("redirect followed")
	}
}
func TestLiveComponentPreservesOriginalWallCapture(t *testing.T) {
	now := time.Unix(1800000000, 0)
	s := fixture(now)
	cfg := sensing.Config{Tenant: "lab", Site: "synthetic-office", Frame: s.State.Frame, Source: world.Source{Device: "sim-sensor-camera-a", App: "world", Service: "camera-a", Stream: "samples"}, Epoch: 7, RunID: "test", TTL: 5 * time.Second, Anchor: now.Add(time.Hour)}
	c, e := s.Component("camera-a", cfg)
	if e != nil {
		t.Fatal(e)
	}
	if !c.Timing.SampleTime.Equal(now) || c.Version.Sequence != s.Sequence || c.Version.Epoch != 7 {
		t.Fatal("capture restamped")
	}
	decoded, e := sensing.FromComponent(c)
	if e != nil || decoded.SampleNS != s.SimulationTimeNS {
		t.Fatalf("lost original sim time: %v", e)
	}
	data, e := s.Evidence(*decoded.Evidence)
	if e != nil {
		t.Fatal(e)
	}
	data[0] ^= 1
	if _, e = s.Evidence(*decoded.Evidence); e != nil {
		t.Fatal("returned media alias changed snapshot")
	}
}
