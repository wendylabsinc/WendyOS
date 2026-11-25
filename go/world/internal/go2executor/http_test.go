package go2executor

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// Produced by sim.go2_bridge using a fake transport and clock. This is a wire
// compatibility fixture, not a live ROS/simulator or clock-accuracy observation.
func TestPythonBridgeStateFixture(t *testing.T) {
	b, err := os.ReadFile("testdata/python-state.json")
	if err != nil {
		t.Fatal(err)
	}
	s, err := DecodeState(b)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Binding.Validate(); err != nil {
		t.Fatal(err)
	}
	if !s.Synthetic || !s.Ready || s.AutoControl || s.Owner != s.CommandPublisher || s.CaptureSequence != s.Scan.Sequence || s.ScanCapturedUnixNS != s.Scan.CapturedUnixNS || s.SampleUnixMS != min(s.OdomCapturedUnixNS, s.ScanCapturedUnixNS)/int64(time.Millisecond) {
		t.Fatal("Python state binding changed")
	}
}

func stateJSON(t *testing.T, s State) []byte {
	t.Helper()
	b, e := json.Marshal(s)
	if e != nil {
		t.Fatal(e)
	}
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	m["pose"] = wirePose{s.Pose.X, s.Pose.Y, s.Pose.Yaw}
	ranges := make([]*float64, len(s.Scan.Ranges))
	for i, v := range s.Scan.Ranges {
		if finite(v) {
			x := v
			ranges[i] = &x
		}
	}
	x := s.Scan
	m["scan"] = wireScan{x.TiltedPose, x.Sequence, x.CapturedUnixNS, x.WorldEpoch, x.Frame, ranges, x.AngleMin, x.AngleIncrement, x.RangeMin, x.RangeMax, wirePose{x.Robot.X, x.Robot.Y, x.Robot.Yaw}, wirePose{x.SensorMount.X, x.SensorMount.Y, x.SensorMount.Yaw}, x.MountKnown, x.PositionUncertaintyM, x.YawUncertaintyRad, x.UncertaintyKnown}
	b, e = json.Marshal(m)
	if e != nil {
		t.Fatal(e)
	}
	return b
}
func TestStrictBridgeStateAndUnknownRays(t *testing.T) {
	c, _, a, _ := fixture(t)
	if e := c.Start(context.Background()); e != nil {
		t.Fatal(e)
	}
	s, _ := a.State(context.Background(), c.session)
	s.Scan.Ranges[0] = math.Inf(1)
	b := stateJSON(t, s)
	decoded, e := DecodeState(b)
	if e != nil {
		t.Fatal(e)
	}
	if !math.IsInf(decoded.Scan.Ranges[0], 1) {
		t.Fatal("unknown ray invented free range")
	}
	if e = c.validateState(decoded); e != nil {
		t.Fatal(e)
	}
	for _, mode := range []string{"missing-auto", "null-ready", "short-velocity", "null-pose", "missing-yaw", "duplicate", "extra", "short-covariance", "missing-capture"} {
		t.Run(mode, func(t *testing.T) {
			var m map[string]any
			_ = json.Unmarshal(b, &m)
			switch mode {
			case "missing-auto":
				delete(m, "auto_control")
			case "null-ready":
				m["ready"] = nil
			case "short-velocity":
				m["body_velocity"] = []float64{0}
			case "null-pose":
				m["pose"] = nil
			case "missing-yaw":
				delete(m["pose"].(map[string]any), "yaw")
			case "extra":
				m["trusted"] = true
			case "short-covariance":
				m["pose_covariance"] = []float64{0}
			case "missing-capture":
				delete(m, "odom_captured_unix_ns")
			}
			bad, _ := json.Marshal(m)
			if mode == "duplicate" {
				bad = append([]byte(`{"ready":true,`), bad[1:]...)
			}
			if _, e := DecodeState(bad); e == nil {
				t.Fatal("malformed bridge state accepted")
			}
		})
	}
}
func TestBridgeHTTPBindsRequestsAndRefusesRedirects(t *testing.T) {
	c, _, a, _ := fixture(t)
	if e := c.Start(context.Background()); e != nil {
		t.Fatal(e)
	}
	s, _ := a.State(context.Background(), c.session)
	body := stateJSON(t, s)
	token := strings.Repeat("a", 32)
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != "POST" || r.URL.Path != "/v1/state" || r.Header.Get("Authorization") != "Bearer "+token {
			t.Errorf("request binding lost")
		}
		_, _ = w.Write(body)
	}))
	defer server.Close()
	adapter, e := NewHTTPAdapter(server.URL, token)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = adapter.State(context.Background(), c.session); e != nil {
		t.Fatal(e)
	}
	if calls != 1 {
		t.Fatal("unexpected request count")
	}
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, server.URL+"/v1/state", http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()
	adapter, _ = NewHTTPAdapter(redirect.URL, token)
	if _, e = adapter.State(context.Background(), c.session); e == nil {
		t.Fatal("redirect followed")
	}
	if calls != 1 {
		t.Fatal("redirect leaked request")
	}
	for _, url := range []string{"http://localhost:1234", "http://192.168.1.2:1234", "http://127.0.0.1:1234/path", "http://127.0.0.1:1234?x=y"} {
		if _, e := NewHTTPAdapter(url, token); e == nil {
			t.Fatal("ambiguous/nonlocal address accepted")
		}
	}
}
func TestBridgeHTTPResponseAndContextBounds(t *testing.T) {
	for _, mode := range []string{"oversize", "compressed", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			release := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch mode {
				case "oversize":
					_, _ = w.Write([]byte(strings.Repeat(" ", maxResponse+1)))
				case "compressed":
					w.Header().Set("Content-Encoding", "gzip")
					_, _ = w.Write([]byte("{}"))
				case "cancel":
					select {
					case <-r.Context().Done():
					case <-release:
					}
				}
			}))
			defer server.Close()
			defer close(release)
			a, _ := NewHTTPAdapter(server.URL, strings.Repeat("a", 32))
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
			defer cancel()
			if _, e := a.State(ctx, "test"); e == nil {
				t.Fatal("response bound bypassed")
			}
		})
	}
}
func TestCommandAcknowledgmentNeverRefreshesDeadline(t *testing.T) {
	for _, mode := range []string{"valid", "changed-deadline", "late-source", "changed-sequence", "not-accepted"} {
		t.Run(mode, func(t *testing.T) {
			r := CommandRequest{Session: "s", Sequence: 1, NotAfterUnixMS: 1_000_000}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				a := map[string]any{"accepted": true, "sequence": r.Sequence, "not_after_unix_ms": r.NotAfterUnixMS, "source_timestamp_ns": (r.NotAfterUnixMS - 450) * int64(time.Millisecond)}
				switch mode {
				case "changed-deadline":
					a["not_after_unix_ms"] = r.NotAfterUnixMS + 1
				case "late-source":
					a["source_timestamp_ns"] = (r.NotAfterUnixMS - 449) * int64(time.Millisecond)
				case "changed-sequence":
					a["sequence"] = 2
				case "not-accepted":
					a["accepted"] = false
				}
				_ = json.NewEncoder(w).Encode(a)
			}))
			defer server.Close()
			a, _ := NewHTTPAdapter(server.URL, strings.Repeat("a", 32))
			e := a.Command(context.Background(), r)
			if (mode == "valid") != (e == nil) {
				t.Fatalf("unexpected acknowledgment result %v", e)
			}
		})
	}
}
