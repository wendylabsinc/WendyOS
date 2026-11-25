package tasking

import (
	"encoding/json"
	"errors"
	"github.com/wendylabsinc/wendy/go/world/internal/mission"
	"math"
	"testing"
	"time"
)

func TestTelemetryFailureReasonsPersistWithoutSpatialClaim(t *testing.T) {
	cases := map[string]func(*Telemetry){"stale": func(v *Telemetry) { v.SampleUnixMS -= 251 }, "future": func(v *Telemetry) { v.SampleUnixMS++ }, "frame": func(v *Telemetry) { v.Frame.Revision = "other" }, "position": func(v *Telemetry) { v.Position[0] = math.NaN() }, "velocity": func(v *Telemetry) { v.Velocity[1] = math.Inf(1) }, "energy": func(v *Telemetry) { v.Energy = math.NaN() }, "local": func(v *Telemetry) { v.LocalFault = true }, "local-stale": func(v *Telemetry) { v.LocalFault = true; v.SampleUnixMS -= 251 }, "outside": func(v *Telemetry) { v.Position[0] = 6 }}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			r := setup(t)
			r.cfg.MaxTelemetryAge = 250 * time.Millisecond
			r.s.cfg.MaxTelemetryAge = r.cfg.MaxTelemetryAge
			spec := r.spec("a")
			spec.Fallback = mission.Stop
			original, e := r.s.Accept(spec, r.auth())
			if e != nil {
				t.Fatal(e)
			}
			if d := r.evaluate(t, "a", r.telemetry()); d.Action != mission.Continue {
				t.Fatal(d)
			}
			tele := r.telemetry()
			mutate(&tele)
			stamp := tele.SampleUnixMS
			d := r.evaluate(t, "a", tele)
			want := "local observations invalid or stale"
			if tele.LocalFault {
				want = "executor reported local fault"
			}
			if name == "outside" {
				want = "outside authorized area"
			}
			if d.Action != mission.Stop || d.Record.Status.Phase != mission.Failed || d.Record.Status.Reason != want || tele.SampleUnixMS != stamp {
				t.Fatalf("misreported negative telemetry %+v", d)
			}
			r.s.Close()
			r.s, e = Open(r.path, r.cfg)
			if e != nil {
				t.Fatal(e)
			}
			got, e := r.s.Get("a")
			if e != nil || got.Status != d.Record.Status || string(got.Proof) != string(original.Proof) {
				t.Fatal("terminal reason/proof changed on reopen")
			}
			if d2 := r.evaluate(t, "a", r.telemetry()); d2.Record.Status != got.Status || d2.Action != mission.Stop {
				t.Fatal("fresh telemetry changed terminal decision")
			}
		})
	}
}
func TestPostwriteStaleTelemetryKeepsTruthfulDurableFailure(t *testing.T) {
	for _, failSecond := range []bool{false, true} {
		t.Run(map[bool]string{false: "durable", true: "failure"}[failSecond], func(t *testing.T) {
			r := setup(t)
			r.cfg.MaxTelemetryAge = 250 * time.Millisecond
			r.s.cfg.MaxTelemetryAge = r.cfg.MaxTelemetryAge
			spec := r.spec("a")
			spec.Fallback = mission.Stop
			if _, e := r.s.Accept(spec, r.auth()); e != nil {
				t.Fatal(e)
			}
			writer := r.s.write
			writes := 0
			r.s.write = func(p string, b []byte) error {
				writes++
				if writes == 2 && failSecond {
					return errors.New("second write failed")
				}
				e := writer(p, b)
				if writes == 1 {
					r.wall = r.wall.Add(251 * time.Millisecond)
					r.mono += 251 * time.Millisecond
				}
				return e
			}
			d, e := r.s.Evaluate("a", "app:nav", r.epoch, r.telemetry())
			if d.Action != mission.Stop || writes != 2 {
				t.Fatalf("late stale released motion: %+v %v writes=%d", d, e, writes)
			}
			if failSecond {
				if e == nil {
					t.Fatal("failed failure-status persistence acknowledged")
				}
				if _, e = r.s.Get("a"); e == nil {
					t.Fatal("uncertain store not poisoned")
				}
				if d.Record.Status.Version != 0 {
					t.Fatal("uncommitted failure status returned")
				}
				r.s.Close()
				r.s, e = Open(r.path, r.cfg)
				if e != nil {
					t.Fatal(e)
				}
				got, e := r.s.Get("a")
				if e != nil || got.Status.Phase != mission.Failed {
					t.Fatal("interrupted persisted Running resumed after failed failure-write")
				}
				return
			}
			if e != nil || d.Record.Status.Reason != "local observations invalid or stale" {
				t.Fatalf("late stale reason %+v %v", d, e)
			}
			r.s.Close()
			r.s, e = Open(r.path, r.cfg)
			if e != nil {
				t.Fatal(e)
			}
			got, e := r.s.Get("a")
			if e != nil || got.Status != d.Record.Status {
				t.Fatal("late-stale terminal not durable")
			}
		})
	}
}
func TestLocalFaultWireDefaultsToNoAdditionalPermission(t *testing.T) {
	b, e := json.Marshal(Telemetry{})
	if e != nil {
		t.Fatal(e)
	}
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	if _, ok := m["local_fault"]; ok {
		t.Fatal("false optional flag emitted")
	}
	b, e = json.Marshal(Telemetry{LocalFault: true})
	if e != nil {
		t.Fatal(e)
	}
	var got Telemetry
	if json.Unmarshal(b, &got) != nil || !got.LocalFault {
		t.Fatal("negative flag lost")
	}
	r := setup(t)
	r.accept(t, "a")
	tele := r.telemetry()
	tele.LocalFault = true
	if d, e := r.s.Evaluate("a", "wrong-executor", r.epoch, tele); e == nil || d.Action != mission.Stop {
		t.Fatal("local fault bypassed executor admission")
	}
}
