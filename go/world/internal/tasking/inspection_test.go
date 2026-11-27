package tasking

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/mission"
	"github.com/wendylabsinc/wendy/go/world/internal/sensing"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

func inspectSetup(t *testing.T) (*rig, Spec, Telemetry) {
	r := setup(t)
	r.s.Close()
	r.cfg.Catalog[InspectType] = mission.Move
	r.cfg.RunID = "run"
	r.cfg.ValidateInspection = func(Inspection) error { return nil }
	var err error
	r.s, err = Open(r.path, r.cfg)
	if err != nil {
		t.Fatal(err)
	}
	r.epoch, err = r.s.BeginExecutor("app:nav")
	if err != nil {
		t.Fatal(err)
	}
	spec := r.spec("inspect")
	spec.Type = InspectType
	if _, err = r.s.Accept(spec, r.auth()); err != nil {
		t.Fatal(err)
	}
	tele := r.telemetry()
	tele.BootID = "boot"
	tele.Session = "session"
	tele.Position = spec.Target
	tele.Inspection = &Inspection{TaskID: spec.ID, RunID: "run", BootID: "boot", Session: "session", Sequence: 1, CapturedUnixMS: r.wall.UnixMilli(), SettledSinceUnixMS: r.wall.Add(-time.Second).UnixMilli(), SimulationTimeNS: 250000000, Frame: spec.Frame, Position: spec.Target, Sensors: tele.Sensors, ObstacleClear: true, Energy: 1}
	for _, id := range []string{"camera-a", "camera-b", "gas-1", "audio-1"} {
		x := sensing.Sample{SchemaVersion: sensing.Schema, ID: "run/" + id + "/1", SiteID: spec.Site, Frame: spec.Frame, SensorID: id, Sequence: 1, SampleNS: 250000000, RunID: "run", ScenarioVersion: "v1", PackageSHA256: strings.Repeat("a", 64), SensorModel: "fixture", PositionM: []float64{2, 0, 0}, UncertaintyM: .01, CalibrationRevision: "c1"}
		switch id {
		case "gas-1":
			x.Kind = "gas"
			x.Gas = &sensing.Gas{ValuePPM: 60, UncertaintyPPM: 1, Species: "synthetic"}
		case "audio-1":
			x.Kind = "audio"
			x.Audio = &sensing.Audio{SampleRateHz: 8000, Channels: 1, DurationNS: 250000000}
			x.Evidence = &sensing.EvidenceRef{SHA256: strings.Repeat("c", 64), ContentType: "audio/wav", Size: 10}
		default:
			x.Kind = "camera"
			x.Camera = &sensing.Camera{Width: 1, Height: 1, FX: 1, FY: 1, CX: .5, CY: .5, CameraToSite: []float64{1, 0, 0, 2, 0, 1, 0, 0, 0, 0, 1, 0, 0, 0, 0, 1}, CalibrationValidForNS: int64(time.Minute)}
			x.Evidence = &sensing.EvidenceRef{SHA256: strings.Repeat("b", 64), ContentType: "image/png", Size: 10}
		}
		tele.Inspection.Samples = append(tele.Inspection.Samples, x)
	}
	return r, spec, tele
}
func TestInspectionNeedsEvidenceAndPersistsImmutableResult(t *testing.T) {
	r, spec, tele := inspectSetup(t)
	empty := tele
	empty.Inspection = nil
	d := r.evaluate(t, spec.ID, empty)
	if d.Record.Status.Phase != mission.Running || d.Record.Result != nil {
		t.Fatal("arrival without evidence completed")
	}
	d = r.evaluate(t, spec.ID, tele)
	if d.Record.Status.Phase != mission.Succeeded || d.Record.Result == nil {
		t.Fatal(d)
	}
	tele.Inspection.Position[0] = 4
	got, _ := r.s.Get(spec.ID)
	if got.Result.Position[0] != 2 {
		t.Fatal("caller mutated stored result")
	}
	r.s.Close()
	var err error
	r.s, err = Open(r.path, r.cfg)
	if err != nil {
		t.Fatal(err)
	}
	got, err = r.s.Get(spec.ID)
	if err != nil || got.Result == nil || got.Status.Phase != mission.Succeeded {
		t.Fatal(got, err)
	}
	if _, err = r.s.Stop(spec.ID, r.auth()); err != nil {
		t.Fatal(err)
	}
	got, _ = r.s.Get(spec.ID)
	if got.Status.Phase != mission.Succeeded || got.Result.Position[0] != 2 {
		t.Fatal("terminal result changed")
	}
}
func TestInspectionRejectsInvalidCapture(t *testing.T) {
	cases := map[string]func(*Telemetry){
		"stale":                     func(v *Telemetry) { v.Inspection.CapturedUnixMS -= 501 },
		"audio interval":            func(v *Telemetry) { v.Inspection.CapturedUnixMS -= 251 },
		"simulation audio interval": func(v *Telemetry) { v.Inspection.SettledSinceSimulationNS = 1 },
		"negative simulation fence": func(v *Telemetry) { v.Inspection.SettledSinceSimulationNS = -1 },
		"future":                    func(v *Telemetry) { v.Inspection.CapturedUnixMS++ },
		"boot":                      func(v *Telemetry) { v.Inspection.BootID = "other" },
		"session":                   func(v *Telemetry) { v.Inspection.Session = "other" },
		"pose":                      func(v *Telemetry) { v.Inspection.Position[0] += .01 },
		"moving":                    func(v *Telemetry) { v.Inspection.Velocity[0] = .2 },
		"missing camera":            func(v *Telemetry) { v.Inspection.Samples = v.Inspection.Samples[1:] },
		"mixed package":             func(v *Telemetry) { v.Inspection.Samples[1].PackageSHA256 = strings.Repeat("c", 64) },
		"run":                       func(v *Telemetry) { v.Inspection.RunID = "other" },
		"sensor":                    func(v *Telemetry) { v.Inspection.Sensors = map[string]bool{} },
		"calibration": func(v *Telemetry) {
			v.Inspection.CapturedUnixMS--
			v.Inspection.Samples[0].Camera.CalibrationValidForNS = 1
		},
		"local sensor pose": func(v *Telemetry) { v.Inspection.Samples[2].PositionM[0]++ },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			r, spec, tele := inspectSetup(t)
			change(&tele)
			d, err := r.s.Evaluate(spec.ID, "app:nav", r.epoch, tele)
			if err == nil || d.Record.Result != nil || d.Record.Status.Phase == mission.Succeeded {
				t.Fatal(d, err)
			}
		})
	}
}
func TestInspectionVerificationAndStopCannotProduceSuccess(t *testing.T) {
	for _, mode := range []string{"missing media", "expired during verification", "stop", "restart", "write failure", "wrong configured run"} {
		t.Run(mode, func(t *testing.T) {
			r, spec, tele := inspectSetup(t)
			switch mode {
			case "missing media":
				r.s.cfg.ValidateInspection = func(Inspection) error { return errors.New("missing") }
			case "expired during verification":
				r.s.cfg.ValidateInspection = func(Inspection) error { r.wall = r.wall.Add(time.Second); r.mono += time.Second; return nil }
			case "stop":
				if _, err := r.s.Stop(spec.ID, r.auth()); err != nil {
					t.Fatal(err)
				}
			case "restart":
				if _, err := r.s.BeginExecutor("app:nav"); err != nil {
					t.Fatal(err)
				}
			case "write failure":
				r.s.write = func(string, []byte) error { return errors.New("disk full") }
			case "wrong configured run":
				r.s.cfg.RunID = "different"
			}
			d, _ := r.s.Evaluate(spec.ID, "app:nav", r.epoch, tele)
			if d.Record.Result != nil || d.Record.Status.Phase == mission.Succeeded {
				t.Fatal(d)
			}
		})
	}
}
func TestInspectionStrictNestedSamples(t *testing.T) {
	_, _, tele := inspectSetup(t)
	raw, err := json.Marshal(tele.Inspection)
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{strings.Replace(string(raw), `"value_ppm":60`, `"value_ppm":null`, 1), strings.Replace(string(raw), `"position_m":[2,0,0]`, `"position_m":[2,0]`, 1)} {
		var i Inspection
		if err := json.Unmarshal([]byte(bad), &i); err == nil {
			t.Fatal("accepted invented capture")
		}
	}
}

func TestTriggerNormalizationAndSignedSnapshotBinding(t *testing.T) {
	r, spec, tele := inspectSetup(t)
	c := world.Component{Key: world.Key{Tenant: "tenant", Site: spec.Site, Entity: "hazard", Type: "wendy.hazard.v1", Source: world.Source{Device: "gateway", App: "fusion", Service: "fusion", Stream: "hazards"}}, Version: world.Version{Epoch: 1, Sequence: 2}, Frame: spec.Frame, Timing: world.Timing{SampleTime: r.wall, ClockDomain: "simulation:run", AgeHint: time.Second}, TTL: time.Second, Payload: json.RawMessage(`{"label":"gas"}`), Provenance: world.Provenance{Origin: world.Inferred, AppVersion: "v1", ModelID: "fusion", ModelVersion: "v1", Inputs: []world.Input{{ID: "run/gas-1/1", Origin: world.Simulated, Simulation: &world.SimulationContext{RunID: "run", ScenarioVersion: "v1", ClockDomain: "simulation:run"}}}}}
	trigger, err := TriggerFromComponent(c)
	if err != nil {
		t.Fatal(err)
	}
	c.Timing.AgeHint = 2 * time.Second
	again, err := TriggerFromComponent(c)
	if err != nil || trigger.SHA256 != again.SHA256 {
		t.Fatal("unsigned age changed trigger identity", err)
	}
	spec.ID = "with-trigger"
	spec.Trigger = trigger
	if _, err = r.s.Accept(spec, r.auth()); err != nil {
		t.Fatal(err)
	}
	tele.Inspection.TaskID = spec.ID
	tele.Inspection.Trigger = again
	if d, err := r.s.Evaluate(spec.ID, "app:nav", r.epoch, tele); err != nil || d.Record.Result == nil {
		t.Fatal(d, err)
	}
	c.Version.Sequence++
	mutated, err := TriggerFromComponent(c)
	if err != nil {
		t.Fatal(err)
	}
	if mutated.SHA256 == trigger.SHA256 {
		t.Fatal("source version not bound")
	}
	spec.ID = "changed-trigger"
	if _, err = r.s.Accept(spec, r.auth()); err != nil {
		t.Fatal(err)
	}
	tele.Inspection.TaskID = spec.ID
	tele.Inspection.Trigger = mutated
	if d, err := r.s.Evaluate(spec.ID, "app:nav", r.epoch, tele); err == nil || d.Record.Result != nil {
		t.Fatal("substituted trigger accepted", d, err)
	}
	mutated.Component.Payload = json.RawMessage(`{"label":"clear"}`)
	if mutated.Validate(spec.Site, spec.Frame) == nil {
		t.Fatal("tampered trigger digest accepted")
	}
}

func TestInspectionRunBindingSurvivesReopen(t *testing.T) {
	r, spec, tele := inspectSetup(t)
	if d := r.evaluate(t, spec.ID, tele); d.Record.Result == nil {
		t.Fatal(d)
	}
	r.s.Close()
	cfg := r.cfg
	cfg.RunID = "other"
	if reopened, err := Open(r.path, cfg); err == nil {
		reopened.Close()
		t.Fatal("reopened inspection results in another simulation run")
	}
	var err error
	r.s, err = Open(r.path, r.cfg)
	if err != nil {
		t.Fatal(err)
	}
	got, err := r.s.Get(spec.ID)
	if err != nil || got.Result == nil || got.Result.RunID != "run" {
		t.Fatal(got, err)
	}
}
