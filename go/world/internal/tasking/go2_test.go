package tasking

import (
	"bytes"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/mission"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

func go2BindingFixture() Go2Binding {
	d := strings.Repeat("a", 64)
	return Go2Binding{Schema: "wendy.world.go2-execution.v1", PackageSHA256: d, ConversionSHA256: d,
		LossSHA256: d, ProjectRevision: d, ProjectID: strings.Repeat("b", 32), VMName: "explicit-vm",
		SourceDigest: "sha256:" + d, PolicyBundle: "go2-moe-cts-164k-0.6715-v1", WorldEpoch: 2,
		RunID: "run", Frame: world.FrameRef{ID: "office", Revision: "r1"}, AlignmentSHA256: d, ProfileSHA256: d}
}

func go2Rig(t *testing.T) *rig {
	t.Helper()
	r := &rig{wall: time.Unix(1700000000, 0), mono: time.Second, path: filepath.Join(t.TempDir(), "go2-tasks.json")}
	b := go2BindingFixture()
	r.cfg = Config{Go2Binding: &b, RunID: "run", Site: "office", Assignee: "robot", ExecutorPrincipal: "app:nav",
		Domain: mission.Simulation, Catalog: map[string]mission.SafetyClass{Go2GoToType: mission.Move},
		Now: func() time.Time { return r.wall }, Monotonic: func() time.Duration { return r.mono },
		MaxLease: 500 * time.Millisecond, MaxTelemetryAge: 250 * time.Millisecond}
	var err error
	r.s, err = Open(r.path, r.cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if r.s != nil {
			_ = r.s.Close()
		}
	})
	r.epoch, err = r.s.BeginExecutor(r.cfg.ExecutorPrincipal)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func go2Spec(r *rig, id string) Spec {
	s := r.spec(id)
	b := go2BindingFixture()
	s.Go2 = &b
	s.Type, s.Fallback = Go2GoToType, mission.Stop
	s.Area = Bounds{Min: [3]float64{-3, -.1, -3}, Max: [3]float64{3, .1, 3}}
	s.RequiredSensors, s.MinimumEnergy = []string{"odometry", "local-obstacles"}, 0
	s.Target, s.SpeedMPS, s.ToleranceM, s.StopSpeedMPS = [3]float64{1, 0, .6}, .3, .25, .05
	return s
}

func go2Telemetry(r *rig) Telemetry {
	t := r.telemetry()
	t.Sensors, t.Energy = map[string]bool{"odometry": true, "local-obstacles": true}, 0
	return t
}

// These tests exercise local task admission and persistence. The existing
// authority/node suites cover real signed proofs; this trusted fixture does not
// pretend that its placeholder Authorization.Proof is a cryptographic signature.
func TestGo2AdmissionRejectsWrongModeDomainAndShape(t *testing.T) {
	cases := map[string]func(*Spec){
		"physical":               func(s *Spec) { s.Domain = mission.Physical },
		"wrong catalog type":     func(s *Spec) { s.Type = "wendy.nav.go_to.v1" },
		"missing binding":        func(s *Spec) { s.Go2 = nil },
		"observe downgrade":      func(s *Spec) { s.Class = mission.Observe },
		"supervised":             func(s *Spec) { s.Mode = mission.Supervised },
		"teleop":                 func(s *Spec) { s.Mode = mission.Teleop },
		"hold fallback":          func(s *Spec) { s.Fallback = mission.Hold },
		"unobserved battery":     func(s *Spec) { s.MinimumEnergy = .1 },
		"wrong sensor":           func(s *Spec) { s.RequiredSensors = []string{"gas", "local-obstacles"} },
		"duplicate sensor":       func(s *Spec) { s.RequiredSensors = []string{"odometry", "odometry"} },
		"height":                 func(s *Spec) { s.Target[1] = .01 },
		"epsilon height":         func(s *Spec) { s.Target[1] = 1e-9 },
		"speed":                  func(s *Spec) { s.SpeedMPS = .351 },
		"nan speed":              func(s *Spec) { s.SpeedMPS = math.NaN() },
		"stop speed":             func(s *Spec) { s.StopSpeedMPS = 0 },
		"loose tolerance":        func(s *Spec) { s.ToleranceM = .36 },
		"area width":             func(s *Spec) { s.Area.Max[0] = 6 },
		"area depth":             func(s *Spec) { s.Area.Max[2] = 6 },
		"reversed area":          func(s *Spec) { s.Area.Min[0] = 4 },
		"out of area":            func(s *Spec) { s.Target[2] = 4 },
		"wrong site":             func(s *Spec) { s.Site = "other" },
		"wrong assignee":         func(s *Spec) { s.Assignee = "other" },
		"binding frame mismatch": func(s *Spec) { s.Go2.Frame.Revision = "r2" },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			r := go2Rig(t)
			s := go2Spec(r, "refused")
			change(&s)
			before, _ := os.ReadFile(r.path)
			if _, err := r.s.Accept(s, r.auth()); err == nil {
				t.Fatal("invalid Go2 instruction admitted")
			}
			after, _ := os.ReadFile(r.path)
			if !bytes.Equal(before, after) {
				t.Fatal("refused instruction changed durable state")
			}
			if _, err := r.s.Get(s.ID); err == nil {
				t.Fatal("refusal appeared as an accepted task")
			}
		})
	}
}

func TestGo2EveryCommissionedPinIsImmutable(t *testing.T) {
	mutations := map[string]func(*Go2Binding){
		"package":          func(b *Go2Binding) { b.PackageSHA256 = strings.Repeat("c", 64) },
		"conversion":       func(b *Go2Binding) { b.ConversionSHA256 = strings.Repeat("c", 64) },
		"loss":             func(b *Go2Binding) { b.LossSHA256 = strings.Repeat("c", 64) },
		"project revision": func(b *Go2Binding) { b.ProjectRevision = strings.Repeat("c", 64) },
		"project id":       func(b *Go2Binding) { b.ProjectID = strings.Repeat("c", 32) },
		"vm":               func(b *Go2Binding) { b.VMName = "other-vm" },
		"source":           func(b *Go2Binding) { b.SourceDigest = "sha256:" + strings.Repeat("c", 64) },
		"epoch":            func(b *Go2Binding) { b.WorldEpoch++ },
		"run":              func(b *Go2Binding) { b.RunID = "other-run" },
		"alignment":        func(b *Go2Binding) { b.AlignmentSHA256 = strings.Repeat("c", 64) },
		"profile":          func(b *Go2Binding) { b.ProfileSHA256 = strings.Repeat("c", 64) },
	}
	for name, change := range mutations {
		t.Run(name, func(t *testing.T) {
			r := go2Rig(t)
			original := go2Spec(r, "immutable")
			accepted, err := r.s.Accept(original, r.auth())
			if err != nil {
				t.Fatal(err)
			}
			changed := copyRecord(accepted).Spec
			change(changed.Go2)
			if SpecDigest(changed) == SpecDigest(original) {
				t.Fatal("binding mutation did not affect signed body identity")
			}
			if _, err = r.s.Accept(changed, r.auth()); err == nil {
				t.Fatal("same task ID accepted changed binding")
			}
			changed.ID = "new-id"
			if _, err = r.s.Accept(changed, r.auth()); err == nil {
				t.Fatal("new task bypassed commissioned binding")
			}
			got, err := r.s.Get(original.ID)
			if err != nil || !reflect.DeepEqual(got, accepted) {
				t.Fatal("original task changed", err)
			}
		})
	}
}

func TestGo2BindingValidationAndConfiguration(t *testing.T) {
	for name, change := range map[string]func(*Go2Binding){
		"unsigned observation schema": func(b *Go2Binding) { b.Schema = "wendy.world.go2-project-binding.v1" },
		"wrong policy":                func(b *Go2Binding) { b.PolicyBundle = "uncommissioned" },
		"zero epoch":                  func(b *Go2Binding) { b.WorldEpoch = 0 },
		"unrepresentable epoch":       func(b *Go2Binding) { b.WorldEpoch = 1 << 53 },
		"empty frame":                 func(b *Go2Binding) { b.Frame = world.FrameRef{} },
		"malformed hash":              func(b *Go2Binding) { b.PackageSHA256 = strings.Repeat("A", 64) },
		"empty runtime digest":        func(b *Go2Binding) { b.SourceDigest = "" },
	} {
		t.Run(name, func(t *testing.T) {
			b := go2BindingFixture()
			change(&b)
			if b.Validate() == nil {
				t.Fatal("invalid binding accepted")
			}
		})
	}
	r := go2Rig(t)
	for name, change := range map[string]func(*Config){
		"missing binding": func(c *Config) { c.Go2Binding = nil }, "wrong run": func(c *Config) { c.RunID = "other" },
		"physical": func(c *Config) { c.Domain = mission.Physical }, "class": func(c *Config) { c.Catalog = map[string]mission.SafetyClass{Go2GoToType: mission.Observe} },
		"mixed catalog": func(c *Config) {
			c.Catalog = map[string]mission.SafetyClass{Go2GoToType: mission.Move, "go_to": mission.Move}
		},
		"binding without catalog": func(c *Config) { c.Catalog = map[string]mission.SafetyClass{"go_to": mission.Move} },
		"long lease":              func(c *Config) { c.MaxLease = 501 * time.Millisecond }, "old telemetry": func(c *Config) { c.MaxTelemetryAge = 251 * time.Millisecond },
	} {
		t.Run(name, func(t *testing.T) {
			c := r.cfg
			change(&c)
			path := filepath.Join(t.TempDir(), "refused.json")
			s, e := Open(path, c)
			if e == nil {
				_ = s.Close()
				t.Fatal("invalid Go2 configuration opened")
			}
			if _, e = os.Stat(path); !os.IsNotExist(e) {
				t.Fatal("invalid configuration wrote task store")
			}
		})
	}
}

func TestGo2ConfigurationAndReturnedRecordsDoNotAlias(t *testing.T) {
	r := go2Rig(t)
	original := go2Spec(r, "copied")
	r.cfg.Go2Binding.WorldEpoch = 99
	accepted, err := r.s.Accept(original, r.auth())
	if err != nil {
		t.Fatal("external config mutation changed store", err)
	}
	want := copyRecord(accepted)
	original.Go2.ProjectID = strings.Repeat("f", 32)
	accepted.Spec.Go2.WorldEpoch = 55
	got, err := r.s.Get("copied")
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatal("caller-owned binding aliases retained task", err)
	}
	got.Spec.Go2.ProfileSHA256 = strings.Repeat("f", 64)
	again, _ := r.s.Get("copied")
	if !reflect.DeepEqual(again, want) {
		t.Fatal("Get exposed mutable binding")
	}
}

func TestGo2ReopenRequiresActiveBindingButPreservesTerminalHistory(t *testing.T) {
	for _, running := range []bool{false, true} {
		t.Run(map[bool]string{false: "accepted", true: "running"}[running], func(t *testing.T) {
			r := go2Rig(t)
			s := go2Spec(r, "durable")
			if _, e := r.s.Accept(s, r.auth()); e != nil {
				t.Fatal(e)
			}
			if running {
				d, e := r.s.Evaluate(s.ID, r.cfg.ExecutorPrincipal, r.epoch, go2Telemetry(r))
				if e != nil || d.Action != mission.Continue {
					t.Fatal(d, e)
				}
			}
			_ = r.s.Close()
			before, _ := os.ReadFile(r.path)
			changed := r.cfg
			b := *changed.Go2Binding
			b.WorldEpoch++
			changed.Go2Binding = &b
			if reopened, e := Open(r.path, changed); e == nil {
				_ = reopened.Close()
				t.Fatal("active task opened under a different runtime binding")
			}
			after, _ := os.ReadFile(r.path)
			if !bytes.Equal(before, after) {
				t.Fatal("failed reopen altered original task history")
			}
			var e error
			r.s, e = Open(r.path, r.cfg)
			if e != nil {
				t.Fatal("failed reopen retained lock or corrupted store", e)
			}
			terminal, e := r.s.Get(s.ID)
			if e != nil || terminal.Status.Phase != mission.Failed {
				t.Fatal("interrupted task was not failed", terminal, e)
			}
			_ = r.s.Close()
			r.s, e = Open(r.path, changed)
			if e != nil {
				t.Fatal("terminal history blocked new commissioning", e)
			}
			got, e := r.s.Get(s.ID)
			if e != nil || !reflect.DeepEqual(got, terminal) {
				t.Fatal("terminal old binding was rewritten", e)
			}
		})
	}
}

func TestGo2AbsentPreservesHistoricalSignedBodyBytes(t *testing.T) {
	r := setup(t)
	s := r.spec("legacy")
	got, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	const old = `{"id":"legacy","type":"go_to","site":"office","assignee":"robot","domain":"simulation","class":"MOVE","mode":"autonomous","frame":{"id":"office","revision":"r1"},"not_after_unix_ms":1700000060000,"area":{"min":[-5,-5,-5],"max":[5,5,5]},"required_sensors":["lidar"],"minimum_energy":0.2,"fallback":"hold","target":[2,0,0],"speed_mps":1,"tolerance_m":0.1,"stop_speed_mps":0.05}`
	if string(got) != old {
		t.Fatalf("pre-Go2 signed wire body changed\ngot %s", got)
	}
	if _, err = r.s.Accept(s, r.auth()); err != nil {
		t.Fatal("legacy mission rejected", err)
	}
	d, err := r.s.Evaluate(s.ID, r.cfg.ExecutorPrincipal, r.epoch, r.telemetry())
	if err != nil || d.Action != mission.Continue {
		t.Fatal("legacy execution changed", d, err)
	}
}
