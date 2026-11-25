package tasking

import (
	"bytes"
	"encoding/json"
	"errors"
	"github.com/wendylabsinc/wendy/go/world/internal/mission"
	"github.com/wendylabsinc/wendy/go/world/internal/missioncontrol"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func controlRig(t *testing.T) (*rig, Spec) {
	t.Helper()
	r := &rig{wall: time.Unix(1700000000, 0), mono: time.Second, path: filepath.Join(t.TempDir(), "tasks.json")}
	c := missioncontrol.SignedContext{Context: missioncontrol.Context{Version: 1, Tenant: "tenant", Site: "office", Audience: "node", RunID: "run", PackageSHA256: strings.Repeat("a", 64), Assignee: "robot", Frame: world.FrameRef{ID: "office", Revision: "r1"}, Type: InspectType, Class: mission.Move, Mode: mission.Autonomous, Area: missioncontrol.Bounds{Min: [3]float64{-2, 0, -1}, Max: [3]float64{2, 1, 1}}, RequiredSensors: []string{"lidar"}, MinimumEnergy: .2, Fallback: mission.Hold, SpeedMPS: .25, ToleranceM: .1, StopSpeedMPS: .05, MaxDurationMS: 60000, NotBeforeUnixMS: r.wall.Add(-time.Minute).UnixMilli(), NotAfterUnixMS: r.wall.Add(time.Hour).UnixMilli()}, Signature: make([]byte, 64)}
	r.cfg = Config{ControlContext: &c, RunID: "run", Site: "office", Assignee: "robot", ExecutorPrincipal: "app:nav", Domain: mission.Simulation, Catalog: map[string]mission.SafetyClass{InspectType: mission.Move}, Now: func() time.Time { return r.wall }, Monotonic: func() time.Duration { return r.mono }, MaxLease: time.Second}
	var err error
	r.s, err = Open(r.path, r.cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if r.s != nil {
			r.s.Close()
		}
	})
	r.epoch, err = r.s.BeginExecutor("app:nav")
	if err != nil {
		t.Fatal(err)
	}
	s := r.spec("native")
	s.Type = InspectType
	s.ControlContext = &c
	s.Area = Bounds{Min: c.Context.Area.Min, Max: c.Context.Area.Max}
	s.Target = [3]float64{1, .2, 0}
	s.SpeedMPS = c.Context.SpeedMPS
	return r, s
}
func TestControlContextPolicyCannotBeSubstituted(t *testing.T) {
	for name, change := range map[string]func(*Spec){
		"speed": func(s *Spec) { s.SpeedMPS = .3 }, "mode": func(s *Spec) { s.Mode = mission.Supervised }, "sensor": func(s *Spec) { s.RequiredSensors = []string{"camera"} }, "energy": func(s *Spec) { s.MinimumEnergy = 0 }, "area": func(s *Spec) { s.Area.Max[0] = 3 }, "target": func(s *Spec) { s.Target[0] = 3 }, "fallback": func(s *Spec) { s.Fallback = mission.Stop }, "frame": func(s *Spec) { s.Frame.Revision = "other" }, "deadline": func(s *Spec) { s.NotAfterUnixMS = s.ControlContext.Context.NotAfterUnixMS + 1 }, "package": func(s *Spec) { s.ControlContext.Context.PackageSHA256 = strings.Repeat("b", 64) }, "run": func(s *Spec) { s.ControlContext.Context.RunID = "other" },
	} {
		t.Run(name, func(t *testing.T) {
			r, s := controlRig(t)
			s = copyRecord(Record{Spec: s}).Spec
			change(&s)
			if _, err := r.s.Accept(s, r.auth()); err == nil {
				t.Fatal("changed control policy admitted")
			}
		})
	}
	r, s := controlRig(t)
	r.s.cfg.ControlContext = nil
	if _, err := r.s.Accept(s, r.auth()); err == nil {
		t.Fatal("unconfigured control context admitted")
	}
}
func TestControlAdmissionPostwriteFenceRetainsFailedID(t *testing.T) {
	for _, kind := range []string{"proposal", "deadline", "authority", "late-authority"} {
		t.Run(kind, func(t *testing.T) {
			r, s := controlRig(t)
			a := r.auth()
			calls := 0
			a.ValidateAdmission = func() error {
				calls++
				if kind == "proposal" && calls > 1 {
					return errors.New("proposal expired")
				}
				return nil
			}
			writer := r.s.write
			r.s.write = func(p string, b []byte) error {
				err := writer(p, b)
				if err == nil && kind == "deadline" {
					r.wall = r.wall.Add(61 * time.Second)
					r.mono += 61 * time.Second
				}
				return err
			}
			if kind == "authority" {
				r.s.cfg.ValidateAuthority = func(Record) error { return errors.New("revoked during commit") }
			}
			if kind == "late-authority" {
				r.s.cfg.ValidateAuthority = func(Record) error { r.wall = r.wall.Add(61 * time.Second); r.mono += 61 * time.Second; return nil }
			}
			if _, err := r.s.Accept(s, a); err == nil {
				t.Fatal("expired admission acknowledged")
			}
			record, err := r.s.Get(s.ID)
			if err != nil || record.Status.Phase != mission.Failed {
				t.Fatal("late admission not durably failed", record, err)
			}
			if len(r.s.state.Audit) < 2 || !bytes.Equal(record.Proof, a.Proof) {
				t.Fatal("admission history lost")
			}
			r.s.write = writer
			r.s.Close()
			r.s, err = Open(r.path, r.cfg)
			if err != nil {
				t.Fatal(err)
			}
			record, err = r.s.Get(s.ID)
			if err != nil || record.Status.Phase != mission.Failed {
				t.Fatal("failure did not survive reopen", record, err)
			}
		})
	}
}
func TestControlExactRetryDoesNotRefreshExpiredProposal(t *testing.T) {
	r, s := controlRig(t)
	a := r.auth()
	calls := 0
	a.ValidateAdmission = func() error { calls++; return nil }
	first, err := r.s.Accept(s, a)
	if err != nil {
		t.Fatal(err)
	}
	a.ValidateAdmission = func() error { t.Fatal("proposal was reconsidered on accepted retry"); return nil }
	again, err := r.s.Accept(s, a)
	if err != nil || calls != 2 || SpecDigest(first.Spec) != SpecDigest(again.Spec) {
		t.Fatal(again, err, calls)
	}
	// Mutating caller-owned policy after Open cannot change commissioned policy.
	r.cfg.ControlContext.Context.RequiredSensors[0] = "other"
	if err := r.s.CheckControlContext(first.Spec); err != nil {
		t.Fatal("configuration alias mutated policy", err)
	}
	r.wall = r.wall.Add(2 * time.Hour)
	r.mono += 2 * time.Hour
	if _, err = r.s.Stop(s.ID, r.auth()); err != nil {
		t.Fatal("expired context blocked stop", err)
	}
}
func TestControlPayloadCanonicalAndLegacyBytes(t *testing.T) {
	_, s := controlRig(t)
	raw, _ := json.Marshal(s)
	if ValidateControlPayload(raw, s) != nil {
		t.Fatal("canonical payload rejected")
	}
	for _, changed := range [][]byte{bytes.Replace(raw, []byte(`"target":[1,0.2,0]`), []byte(`"target":[1,0.2]`), 1), bytes.Replace(raw, []byte(`"minimum_energy":0.2`), []byte(`"minimum_energy":null`), 1)} {
		if ValidateControlPayload(changed, s) == nil {
			t.Fatal("ambiguous payload accepted")
		}
	}
	s.ControlContext = nil
	s.Proposal = nil
	legacy, _ := json.Marshal(s)
	if bytes.Contains(legacy, []byte("control_context")) || bytes.Contains(legacy, []byte("proposal")) || bytes.Contains(legacy, []byte("inspection_destination")) {
		t.Fatal("legacy bytes gained fields")
	}
}
