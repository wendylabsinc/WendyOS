package node

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/mission"
	"github.com/wendylabsinc/wendy/go/world/internal/tasking"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

func go2HTTPBinding() tasking.Go2Binding {
	d := strings.Repeat("a", 64)
	return tasking.Go2Binding{Schema: "wendy.world.go2-execution.v1", PackageSHA256: d, ConversionSHA256: d, LossSHA256: d, ProjectRevision: d, ProjectID: strings.Repeat("b", 32), VMName: "test-vm", SourceDigest: "sha256:" + d, PolicyBundle: "go2-moe-cts-164k-0.6715-v1", WorldEpoch: 6, RunID: "live", Frame: world.FrameRef{ID: "floor", Revision: "1"}, AlignmentSHA256: d, ProfileSHA256: d}
}
func go2HTTPSpec(id string) tasking.Spec {
	s := taskSpec(id)
	b := go2HTTPBinding()
	s.Go2 = &b
	s.Type = tasking.Go2GoToType
	s.Target = [3]float64{1, 0, .6}
	s.SpeedMPS = .25
	s.MinimumEnergy = 0
	s.RequiredSensors = []string{"odometry", "local-obstacles"}
	s.Area = tasking.Bounds{Min: [3]float64{-2, -.1, -2}, Max: [3]float64{2, .1, 2}}
	return s
}
func newGo2TaskHTTPFixture(t *testing.T) (*fixture, Credentials, Credentials, string, tasking.Config) {
	t.Helper()
	f := newFixture(t, nil, "go2-node")
	op := missionCredentials(t, f, "operator", "operator", "", "mission", "stop", "read")
	ex := missionCredentials(t, f, "app", "executor", "robot/navigation", "execute", "read")
	b := go2HTTPBinding()
	path := filepath.Join(t.TempDir(), "go2-tasks.json")
	cfg := tasking.Config{Site: "office", Assignee: "robot/navigation", ExecutorPrincipal: "executor", Domain: mission.Simulation, RunID: b.RunID, Go2Binding: &b, Catalog: map[string]mission.SafetyClass{tasking.Go2GoToType: mission.Move}, Now: time.Now, MaxLease: 500 * time.Millisecond, MaxTelemetryAge: 250 * time.Millisecond, ValidateAuthority: MissionAuthority(f.v)}
	s, e := tasking.Open(path, cfg)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = s.Close() })
	f.n.cfg.Tasks = s
	f.n.cfg.TaskAssignee = cfg.Assignee
	f.n.cfg.TaskFrames = f.wc.Frames
	return f, op, ex, path, cfg
}

func TestGo2SignedHTTPAdmissionExecutionAndReopen(t *testing.T) {
	f, op, ex, path, cfg := newGo2TaskHTTPFixture(t)
	epoch := startExecutor(t, f, ex)
	spec := go2HTTPSpec("off-axis")
	body := instructionBody(t, f, op, "mission", spec)
	code, data := call(t, f, op, "POST", "/v1/tasks", "mission", body)
	if code != 200 {
		t.Fatalf("signed Go2 admission: HTTP%d %s", code, data)
	}
	var admitted tasking.Record
	if e := json.Unmarshal(data, &admitted); e != nil {
		t.Fatal(e)
	}
	if admitted.Status.Phase != mission.Accepted || admitted.Spec.Go2 == nil || *admitted.Spec.Go2 != *cfg.Go2Binding || !bytes.Equal(admitted.Proof, body) {
		t.Fatal("admission lost exact commissioned operator proof")
	}
	tm := tasking.Telemetry{Frame: spec.Frame, SampleUnixMS: time.Now().UnixMilli(), Position: [3]float64{}, ObstacleClear: true, Energy: 0, Sensors: map[string]bool{"odometry": true, "local-obstacles": true}}
	code, data = call(t, f, ex, "POST", "/v1/tasks/evaluate", "execute", jsonBody(t, EvaluateRequest{TaskID: spec.ID, ExecutorEpoch: epoch, Telemetry: tm}))
	if code != 200 {
		t.Fatalf("Go2 evaluation: HTTP%d %s", code, data)
	}
	var d tasking.Decision
	_ = json.Unmarshal(data, &d)
	if d.Action != mission.Continue || d.Record.Status.Phase != mission.Running || d.Supervision != nil {
		t.Fatalf("unexpected autonomous decision %+v", d)
	}
	if e := f.n.cfg.Tasks.Close(); e != nil {
		t.Fatal(e)
	}
	reopened, e := tasking.Open(path, cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer reopened.Close()
	f.n.cfg.Tasks = reopened
	r, e := reopened.Get(spec.ID)
	if e != nil || r.Status.Phase != mission.Failed || !bytes.Equal(r.Proof, admitted.Proof) {
		t.Fatalf("restart did not retain failed mission/proof: %+v %v", r.Status, e)
	}
}

func TestGo2SignedHTTPRejectsUnsupportedAndUncommissionedSpecs(t *testing.T) {
	cases := map[string]func(*tasking.Spec){
		"wrong-binding":   func(s *tasking.Spec) { s.Go2.ProjectRevision = strings.Repeat("c", 64) },
		"wrong-package":   func(s *tasking.Spec) { s.Go2.PackageSHA256 = strings.Repeat("c", 64) },
		"wrong-epoch":     func(s *tasking.Spec) { s.Go2.WorldEpoch++ },
		"physical":        func(s *tasking.Spec) { s.Domain = mission.Physical },
		"supervised":      func(s *tasking.Spec) { s.Mode = mission.Supervised },
		"teleop":          func(s *tasking.Spec) { s.Mode = mission.Teleop },
		"wrong-type":      func(s *tasking.Spec) { s.Type = "wendy.other.go_to.v1" },
		"legacy-type":     func(s *tasking.Spec) { s.Type = "wendy.nav.go_to.v1"; s.Go2 = nil; s.Target = [3]float64{1, .2, 0} },
		"missing-binding": func(s *tasking.Spec) { s.Go2 = nil },
		"wrong-sensor":    func(s *tasking.Spec) { s.RequiredSensors = []string{"gas", "local-obstacles"} },
		"battery":         func(s *tasking.Spec) { s.MinimumEnergy = .1 },
		"wrong-frame":     func(s *tasking.Spec) { s.Frame.Revision = "other"; s.Go2.Frame = s.Frame },
		"wrong-assignee":  func(s *tasking.Spec) { s.Assignee = "another/navigation" },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			f, op, _, _, _ := newGo2TaskHTTPFixture(t)
			s := go2HTTPSpec("refused")
			change(&s)
			code, data := call(t, f, op, "POST", "/v1/tasks", "mission", instructionBody(t, f, op, "mission", s))
			if code == 200 {
				t.Fatalf("unsupported signed mission accepted: %s", data)
			}
			r, e := f.n.cfg.Tasks.Get(s.ID)
			if e == nil && r.Status.Phase != mission.Rejected {
				t.Fatalf("refused task became executable: %+v", r.Status)
			}
			// In-scope authenticated denials may be durably Rejected for audit. They
			// must never become Accepted/Running or borrow pins from their own body.
			records, e := f.n.cfg.Tasks.List()
			if e != nil {
				t.Fatal(e)
			}
			for _, r := range records {
				if !r.Status.Phase.Terminal() {
					t.Fatal("denial retained executable task")
				}
			}
			audit, e := f.n.cfg.Tasks.AuditLog()
			if e != nil {
				t.Fatal(e)
			}
			for _, event := range audit {
				if event.TaskID == s.ID && event.Event == "accepted" {
					t.Fatal("refused HTTP mission was durably accepted before denial")
				}
			}
		})
	}
}

func TestGo2SignedHTTPDoesNotCommissionLegacyStore(t *testing.T) {
	f, op, _, _, _ := taskFixture(t)
	s := go2HTTPSpec("uncommissioned")
	code, data := call(t, f, op, "POST", "/v1/tasks", "mission", instructionBody(t, f, op, "mission", s))
	if code == 200 {
		t.Fatalf("request commissioned its own binding: %s", data)
	}
	r, e := f.n.cfg.Tasks.Get(s.ID)
	if e == nil && r.Status.Phase != mission.Rejected {
		t.Fatal("Go2 shape gate bypassed configured catalog")
	}
}

func TestGo2SignedHTTPStopBeforeDeliveryAndGenerationOrder(t *testing.T) {
	f, op, ex, _, _ := newGo2TaskHTTPFixture(t)
	code, data := call(t, f, op, "POST", "/v1/tasks/stop", "stop", instructionBody(t, f, op, "stop", StopRequest{TaskID: "stopped-before"}))
	if code != 200 {
		t.Fatalf("stop: %d %s", code, data)
	}
	code, data = call(t, f, op, "POST", "/v1/tasks", "mission", instructionBody(t, f, op, "mission", go2HTTPSpec("stopped-before")))
	if code == 200 {
		t.Fatalf("stop-before-task bypassed: %s", data)
	}
	s := go2HTTPSpec("before-generation")
	code, data = call(t, f, op, "POST", "/v1/tasks", "mission", instructionBody(t, f, op, "mission", s))
	if code != 200 {
		t.Fatalf("pre-generation admission: %d %s", code, data)
	}
	epoch := startExecutor(t, f, ex)
	r, e := f.n.cfg.Tasks.Get(s.ID)
	if e != nil || r.Status.Phase != mission.Failed {
		t.Fatal("new executor resumed previously accepted task")
	}
	s = go2HTTPSpec("after-generation")
	code, data = call(t, f, op, "POST", "/v1/tasks", "mission", instructionBody(t, f, op, "mission", s))
	if code != 200 {
		t.Fatalf("post-generation admission: %d %s", code, data)
	}
	tm := tasking.Telemetry{Frame: s.Frame, SampleUnixMS: time.Now().UnixMilli(), ObstacleClear: true, Sensors: map[string]bool{"odometry": true, "local-obstacles": true}}
	code, data = call(t, f, ex, "POST", "/v1/tasks/evaluate", "execute", jsonBody(t, EvaluateRequest{TaskID: s.ID, ExecutorEpoch: epoch, Telemetry: tm}))
	if code != 200 {
		t.Fatalf("post-generation evaluate: %d %s", code, data)
	}
	var d tasking.Decision
	_ = json.Unmarshal(data, &d)
	if d.Action != mission.Continue {
		t.Fatal("newly delivered task did not run")
	}
	if next := startExecutor(t, f, ex); next <= epoch {
		t.Fatal("executor generation did not advance")
	}
	r, e = f.n.cfg.Tasks.Get(s.ID)
	if e != nil || r.Status.Phase != mission.Failed {
		t.Fatal("new executor resumed running task")
	}
}
