package go2executor

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func rejectedFixture(t *testing.T) []byte {
	t.Helper()
	b, e := os.ReadFile("testdata/python-rejected-odom-v1.json")
	if e != nil {
		t.Fatal(e)
	}
	return b
}
func TestPythonRejectedDiagnosticStrictNullableCodec(t *testing.T) {
	raw := rejectedFixture(t)
	d, e := DecodeBridgeDiagnostics(raw)
	if e != nil {
		t.Fatal(e)
	}
	if d.RejectedSensor == nil || d.RejectedSensor.RollRad == nil || *d.RejectedSensor.RollRad <= .2 {
		t.Fatal("fixture does not retain actual first rejected tilt")
	}
	for _, name := range []string{"missing", "null_required", "unknown", "short_quaternion", "null_element", "bad_normalized", "oversized", "wrong_domain", "bad_reason"} {
		t.Run(name, func(t *testing.T) {
			var v map[string]any
			_ = json.Unmarshal(raw, &v)
			r := v["rejected_sensor"].(map[string]any)
			switch name {
			case "missing":
				delete(r, "odom_position_xyz")
			case "null_required":
				r["topic"] = nil
			case "unknown":
				r["extra"] = true
			case "short_quaternion":
				r["quaternion_wxyz"] = []float64{1, 0}
			case "null_element":
				r["odom_position_xyz"] = []any{nil, 0, 0}
			case "bad_normalized":
				r["normalized_quaternion_wxyz"] = []float64{2, 0, 0, 0}
			case "wrong_domain":
				v["operational_authority"] = true
			case "bad_reason":
				r["reason"] = "arbitrary exception details"
			}
			b, _ := json.Marshal(v)
			if name == "oversized" {
				b = append(b, []byte(strings.Repeat(" ", maxStartupDiagnosticBytes))...)
			}
			if _, e := DecodeBridgeDiagnostics(b); e == nil {
				t.Fatal("invalid diagnostic accepted")
			}
		})
	}
	var v map[string]any
	_ = json.Unmarshal(raw, &v)
	r := v["rejected_sensor"].(map[string]any)
	for _, k := range []string{"world_epoch", "publisher_gid", "captured_unix_ns", "quaternion_wxyz", "normalized_quaternion_wxyz", "odom_position_xyz", "roll_rad", "pitch_rad"} {
		r[k] = nil
	}
	b, _ := json.Marshal(v)
	d, e = DecodeBridgeDiagnostics(b)
	if e != nil {
		t.Fatal(e)
	}
	if d.RejectedSensor.WorldEpoch != nil || d.RejectedSensor.QuaternionWXYZ != nil || d.RejectedSensor.OdomPositionXYZ != nil || d.RejectedSensor.CapturedUnixNS != nil {
		t.Fatal("unknown observation fabricated zero")
	}
	v["rejected_sensor"] = nil
	b, _ = json.Marshal(v)
	if _, e := DecodeBridgeDiagnostics(b); e != nil {
		t.Fatal("no rejected sample should remain available", e)
	}
}

type diagnosticAdapter struct {
	*fakeAdapter
	onDiagnostic func(context.Context) (*BridgeDiagnostics, error)
}

func (a *diagnosticAdapter) StartupDiagnostics(ctx context.Context) (*BridgeDiagnostics, error) {
	return a.onDiagnostic(ctx)
}
func TestStartupDiagnosticRunsAfterLatchAndBrake(t *testing.T) {
	c, _, a, _ := fixture(t)
	a.beginErr = errors.New("original begin failed")
	d, e := DecodeBridgeDiagnostics(rejectedFixture(t))
	if e != nil {
		t.Fatal(e)
	}
	calls := 0
	c.adapter = &diagnosticAdapter{fakeAdapter: a, onDiagnostic: func(ctx context.Context) (*BridgeDiagnostics, error) {
		calls++
		if c.fault == nil || a.stops != 1 || c.faultSnapshot == nil {
			t.Fatal("diagnostic delayed primary latch or brake")
		}
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 100*time.Millisecond {
			t.Fatal("diagnostic budget absent")
		}
		return d, nil
	}}
	// Failure after a known session exercises an actual stop attempt before the
	// diagnostic. Uncertain/missing session failures retain the old brake rule.
	a.beginErr = nil
	g := c.gateway.(*fakeGateway)
	g.beginErr = errors.New("original begin failed")
	err := c.Start(context.Background())
	if err == nil || err.Error() != "original begin failed" || calls != 1 {
		t.Fatalf("primary failure lost: %v calls%d", err, calls)
	}
	snap, ok := c.FaultSnapshot()
	if !ok || snap.BridgeDiagnostics == nil {
		t.Fatal("rejected sample not preserved")
	}
	before := *snap.BridgeDiagnostics.RejectedSensor.RollRad
	*d.RejectedSensor.RollRad = 0
	*snap.BridgeDiagnostics.RejectedSensor.RollRad = 0
	again, _ := c.FaultSnapshot()
	if *again.BridgeDiagnostics.RejectedSensor.RollRad != before {
		t.Fatal("diagnostic aliases source or caller")
	}
	if c.Generation() != 0 || len(a.commands) != 0 {
		t.Fatal("diagnostic created executor readiness or motion")
	}
	_ = c.Start(context.Background())
	if calls != 1 {
		t.Fatal("startup diagnostic repeated")
	}
}
func TestStartupDiagnosticTimeoutCannotReplaceOriginalFailure(t *testing.T) {
	c, g, a, _ := fixture(t)
	g.beginErr = errors.New("original begin failed")
	c.adapter = &diagnosticAdapter{fakeAdapter: a, onDiagnostic: func(ctx context.Context) (*BridgeDiagnostics, error) {
		if a.stops != 1 || c.fault == nil {
			t.Fatal("unsafe capture order")
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	if e := c.Start(context.Background()); e == nil || e.Error() != "original begin failed" {
		t.Fatal("diagnostic timeout replaced failure", e)
	}
	snap, _ := c.FaultSnapshot()
	if snap.BridgeDiagnostics != nil {
		t.Fatal("timed-out sample retained")
	}
}
func TestHTTPStartupDiagnosticAuthenticatedBounded(t *testing.T) {
	raw := rejectedFixture(t)
	token := strings.Repeat("a", 32)
	for _, name := range []string{"valid", "malformed", "oversized", "timeout"} {
		t.Run(name, func(t *testing.T) {
			release := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "POST" || r.URL.Path != "/v1/diagnostics" || r.Header.Get("Authorization") != "Bearer "+token {
					t.Error("wrong authenticated endpoint")
					w.WriteHeader(403)
					return
				}
				switch name {
				case "timeout":
					select {
					case <-r.Context().Done():
					case <-release:
					}
					return
				case "oversized":
					_, _ = w.Write([]byte(strings.Repeat(" ", maxStartupDiagnosticBytes+1)))
				case "malformed":
					_, _ = w.Write([]byte(`{"schema":"unexpected"}`))
				default:
					_, _ = w.Write(raw)
				}
			}))
			defer server.Close()
			defer close(release)
			a, e := NewHTTPAdapter(server.URL, token)
			if e != nil {
				t.Fatal(e)
			}
			d, e := a.StartupDiagnostics(context.Background())
			if name == "valid" {
				if e != nil || d == nil {
					t.Fatal(e)
				}
			} else if e == nil {
				t.Fatal("unbounded/malformed diagnostic accepted")
			}
		})
	}
}
