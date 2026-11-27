package node

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/mission"
	"github.com/wendylabsinc/wendy/go/world/internal/tasking"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"io"
	"net/http"
	"path/filepath"
	"testing"
	"time"
)

func missionCredentials(t *testing.T, f *fixture, kind, principal, assignee string, scopes ...string) Credentials {
	t.Helper()
	pub, key, _ := ed25519.GenerateKey(rand.Reader)
	g := authority.Grant{Version: 1, ID: principal, Principal: principal, Kind: kind, ExecuteAssignee: assignee, Tenant: "tenant", Site: "office", Domain: "simulation", Source: world.Source{Device: principal, App: "tasks", Service: "tasks", Stream: "tasks"}, PublicKey: pub, Scopes: scopes, NotBeforeUnixMS: time.Now().Add(-time.Minute).UnixMilli(), NotAfterUnixMS: time.Now().Add(time.Hour).UnixMilli()}
	signed, err := authority.SignGrant(g, f.root)
	if err != nil {
		t.Fatal(err)
	}
	return Credentials{signed, key}
}
func taskFixture(t *testing.T) (*fixture, Credentials, Credentials, string, tasking.Config) {
	t.Helper()
	f := newFixture(t, nil, "robot-node")
	op := missionCredentials(t, f, "operator", "operator", "", "mission", "stop", "read")
	ex := missionCredentials(t, f, "app", "executor", "robot/navigation", "execute", "read")
	path := filepath.Join(t.TempDir(), "tasks.json")
	cfg := tasking.Config{Site: "office", Assignee: "robot/navigation", ExecutorPrincipal: "executor", Domain: mission.Simulation, Catalog: map[string]mission.SafetyClass{"wendy.nav.go_to.v1": mission.Move}, Now: time.Now, MaxLease: 500 * time.Millisecond, MaxTelemetryAge: 500 * time.Millisecond, ValidateAuthority: MissionAuthority(f.v)}
	tasks, err := tasking.Open(path, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tasks.Close() })
	f.n.cfg.Tasks = tasks
	f.n.cfg.TaskAssignee = cfg.Assignee
	f.n.cfg.TaskFrames = f.wc.Frames
	return f, op, ex, path, cfg
}
func taskSpec(id string) tasking.Spec {
	return tasking.Spec{ID: id, Type: "wendy.nav.go_to.v1", Site: "office", Assignee: "robot/navigation", Domain: mission.Simulation, Class: mission.Move, Mode: mission.Autonomous, Frame: world.FrameRef{ID: "floor", Revision: "1"}, NotAfterUnixMS: time.Now().Add(5 * time.Minute).UnixMilli(), Area: tasking.Bounds{Min: [3]float64{-10, -10, -10}, Max: [3]float64{10, 10, 10}}, RequiredSensors: []string{"obstacles"}, MinimumEnergy: .1, Fallback: mission.Stop, Target: [3]float64{1, .2, 0}, SpeedMPS: .5, ToleranceM: .1, StopSpeedMPS: .05}
}
func instructionBody(t *testing.T, f *fixture, op Credentials, operation string, payload any) []byte {
	t.Helper()
	p, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	i, err := authority.SignInstruction(op.Grant, op.PrivateKey, f.audience, operation, p, time.Now(), 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(i)
	return b
}
func jsonBody(t *testing.T, v any) []byte {
	t.Helper()
	b, e := json.Marshal(v)
	if e != nil {
		t.Fatal(e)
	}
	return b
}
func startExecutor(t *testing.T, f *fixture, ex Credentials) uint64 {
	t.Helper()
	status, b := call(t, f, ex, "POST", "/v1/tasks/executor/start", "execute", []byte(`{}`))
	if status != 200 {
		t.Fatalf("start: %d %s", status, b)
	}
	var out ExecutorStartResponse
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out.ExecutorEpoch
}
func telemetry() tasking.Telemetry {
	return tasking.Telemetry{Frame: world.FrameRef{ID: "floor", Revision: "1"}, SampleUnixMS: time.Now().UnixMilli(), ObstacleClear: true, Energy: 1, Sensors: map[string]bool{"obstacles": true}}
}
func TestSignedTaskExecutionStopAndReopen(t *testing.T) {
	f, op, ex, path, cfg := taskFixture(t)
	epoch := startExecutor(t, f, ex)
	body := instructionBody(t, f, op, "mission", taskSpec("move-1"))
	code, b := call(t, f, op, "POST", "/v1/tasks", "mission", body)
	if code != 200 {
		t.Fatalf("accept: %d %s", code, b)
	}
	var admitted tasking.Record
	_ = json.Unmarshal(b, &admitted)
	if len(admitted.Proof) == 0 {
		t.Fatal("missing durable operator proof")
	}
	code, b = call(t, f, ex, "POST", "/v1/tasks/evaluate", "execute", jsonBody(t, EvaluateRequest{TaskID: "move-1", ExecutorEpoch: epoch, Telemetry: telemetry()}))
	if code != 200 {
		t.Fatalf("evaluate: %d %s", code, b)
	}
	var d tasking.Decision
	_ = json.Unmarshal(b, &d)
	if d.Action != mission.Continue {
		t.Fatalf("not moving: %+v", d)
	}
	// A reporting adapter cannot declare motion complete without measured arrival.
	code, _ = call(t, f, ex, "POST", "/v1/tasks/report", "execute", jsonBody(t, ReportRequest{TaskID: "move-1", ExecutorEpoch: epoch, Version: d.Record.Status.Version + 1, Phase: mission.Succeeded}))
	if code == 200 {
		t.Fatal("unmeasured success accepted")
	}
	if err := f.n.cfg.Tasks.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := tasking.Open(path, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	f.n.cfg.Tasks = reopened
	code, b = call(t, f, op, "GET", "/v1/tasks", "read", nil)
	if code != 200 {
		t.Fatal(code, string(b))
	}
	var result TasksResponse
	_ = json.Unmarshal(b, &result)
	if len(result.Tasks) != 1 || result.Tasks[0].Status.Phase != mission.Failed {
		t.Fatalf("reopen resumed motion: %s", b)
	}
	if !bytes.Equal(result.Tasks[0].Proof, admitted.Proof) {
		t.Fatal("proof changed across reopen")
	}
	stop := instructionBody(t, f, op, "stop", StopRequest{TaskID: "future"})
	code, b = call(t, f, op, "POST", "/v1/tasks/stop", "stop", stop)
	if code != 200 {
		t.Fatal(code, string(b))
	}
	code, _ = call(t, f, op, "POST", "/v1/tasks", "mission", instructionBody(t, f, op, "mission", taskSpec("future")))
	if code == 200 {
		t.Fatal("stop before task lost")
	}
}
func TestTaskForgeryExecutorBindingAndCatalogDowngrade(t *testing.T) {
	f, op, ex, _, _ := taskFixture(t)
	epoch := startExecutor(t, f, ex)
	body := instructionBody(t, f, op, "mission", taskSpec("move"))
	var i authority.SignedInstruction
	_ = json.Unmarshal(body, &i)
	spec := taskSpec("forged")
	i.Payload = jsonBody(t, spec)
	code, _ := call(t, f, op, "POST", "/v1/tasks", "mission", jsonBody(t, i))
	if code != 401 {
		t.Fatalf("forgery: %d", code)
	}
	spec = taskSpec("downgraded")
	spec.Class = mission.Observe
	code, _ = call(t, f, op, "POST", "/v1/tasks", "mission", instructionBody(t, f, op, "mission", spec))
	if code == 200 {
		t.Fatal("catalog class downgrade")
	}
	code, b := call(t, f, op, "POST", "/v1/tasks", "mission", body)
	if code != 200 {
		t.Fatal(code, string(b))
	}
	other := missionCredentials(t, f, "app", "wrong-executor", "other/navigation", "execute")
	req := jsonBody(t, EvaluateRequest{TaskID: "move", ExecutorEpoch: epoch, Telemetry: telemetry()})
	code, _ = call(t, f, other, "POST", "/v1/tasks/evaluate", "execute", req)
	if code != 403 {
		t.Fatal("wrong assignee", code)
	}
	other = missionCredentials(t, f, "app", "wrong-principal", "robot/navigation", "execute")
	code, _ = call(t, f, other, "POST", "/v1/tasks/evaluate", "execute", req)
	if code == 200 {
		t.Fatal("wrong principal accepted")
	}
	code, _ = call(t, f, op, "POST", "/v1/tasks/evaluate", "execute", req)
	if code != 401 {
		t.Fatal("operator executed", code)
	}
	code, _ = call(t, f, ex, "POST", "/v1/tasks", "mission", body)
	if code != 401 {
		t.Fatal("app admitted mission", code)
	}
	// Start a replacement executor: old epoch cannot continue an interrupted task.
	startExecutor(t, f, ex)
	code, _ = call(t, f, ex, "POST", "/v1/tasks/evaluate", "execute", req)
	if code == 200 {
		t.Fatal("stale executor epoch accepted")
	}
}
func TestTaskTransportReplayAndExpiredInstruction(t *testing.T) {
	f, op, _, _, _ := taskFixture(t)
	body := instructionBody(t, f, op, "mission", taskSpec("move"))
	req, err := http.NewRequest("POST", f.server.URL+"/v1/tasks", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if err = AuthorizeRequest(req, op, f.audience, "mission", body); err != nil {
		t.Fatal(err)
	}
	for index, want := range []int{200, 401} {
		clone := req.Clone(req.Context())
		clone.Body = http.NoBody
		if len(body) > 0 {
			clone.Body = io.NopCloser(bytes.NewReader(body))
		}
		response, e := http.DefaultClient.Do(clone)
		if e != nil {
			t.Fatal(e)
		}
		response.Body.Close()
		if response.StatusCode != want {
			t.Fatalf("delivery %d: %d want %d", index, response.StatusCode, want)
		}
	}
	p, _ := json.Marshal(taskSpec("expired"))
	i, err := authority.SignInstruction(op.Grant, op.PrivateKey, f.audience, "mission", p, time.Now().Add(-2*time.Minute), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	code, _ := call(t, f, op, "POST", "/v1/tasks", "mission", jsonBody(t, i))
	if code != 401 {
		t.Fatal("expired instruction", code)
	}
}

func TestTaskRevocationAndStaleTelemetryStopMotion(t *testing.T) {
	for _, scenario := range []string{"revoked", "stale-telemetry"} {
		t.Run(scenario, func(t *testing.T) {
			f, op, ex, _, _ := taskFixture(t)
			epoch := startExecutor(t, f, ex)
			code, b := call(t, f, op, "POST", "/v1/tasks", "mission", instructionBody(t, f, op, "mission", taskSpec("move")))
			if code != 200 {
				t.Fatal(code, string(b))
			}
			request := EvaluateRequest{TaskID: "move", ExecutorEpoch: epoch, Telemetry: telemetry()}
			code, b = call(t, f, ex, "POST", "/v1/tasks/evaluate", "execute", jsonBody(t, request))
			if code != 200 {
				t.Fatal(code, string(b))
			}
			if scenario == "revoked" {
				now := time.Now()
				rev, err := authority.SignRevocations(authority.Revocations{Version: 1, Tenant: "tenant", Site: "office", Domain: "simulation", Revision: 2, IssuedAtUnixMS: now.Add(-time.Second).UnixMilli(), ExpiresAtUnixMS: now.Add(30 * time.Minute).UnixMilli(), GrantIDs: []string{op.Grant.Grant.ID}}, f.root)
				if err != nil {
					t.Fatal(err)
				}
				if err = f.v.UpdateRevocations(rev); err != nil {
					t.Fatal(err)
				}
			} else {
				request.Telemetry.SampleUnixMS = time.Now().Add(-time.Minute).UnixMilli()
			}
			code, b = call(t, f, ex, "POST", "/v1/tasks/evaluate", "execute", jsonBody(t, request))
			if code == 200 {
				var decision tasking.Decision
				if err := json.Unmarshal(b, &decision); err != nil {
					t.Fatal(err)
				}
				if decision.Action == mission.Continue {
					t.Fatal("unsafe continuation")
				}
			}
			record, err := f.n.cfg.Tasks.Get("move")
			if err != nil {
				t.Fatal(err)
			}
			if record.Status.Phase == mission.Running || record.Status.Phase == mission.Accepted {
				t.Fatal("unsafe state", record.Status)
			}
		})
	}
}

func TestSimulationAdapterRejectsUnexecutableMissionsBeforeReservation(t *testing.T) {
	f, op, ex, _, _ := taskFixture(t)
	startExecutor(t, f, ex)
	for name, change := range map[string]func(*tasking.Spec){
		"supervised": func(s *tasking.Spec) { s.Mode = mission.Supervised },
		"teleop":     func(s *tasking.Spec) { s.Mode = mission.Teleop },
		"fast":       func(s *tasking.Spec) { s.SpeedMPS = .501 },
		"outside-x":  func(s *tasking.Spec) { s.Target[0] = 2.01 },
		"wrong-y":    func(s *tasking.Spec) { s.Target[1] = .21 },
		"wrong-z":    func(s *tasking.Spec) { s.Target[2] = .01 },
		"other-type": func(s *tasking.Spec) { s.Type = "other.nav" },
	} {
		s := taskSpec(name)
		change(&s)
		code, b := call(t, f, op, "POST", "/v1/tasks", "mission", instructionBody(t, f, op, "mission", s))
		if code != 400 {
			t.Fatalf("%s accepted: %d %s", name, code, b)
		}
	}
	records, err := f.n.cfg.Tasks.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 7 {
		t.Fatal("refusals were not retained", records)
	}
	for _, record := range records {
		if record.Status.Phase != mission.Rejected {
			t.Fatal("invalid task reserved actuator", record)
		}
	}
	s := taskSpec("valid")
	s.Target[0] = 2
	s.Target[1] += .5e-9
	code, b := call(t, f, op, "POST", "/v1/tasks", "mission", instructionBody(t, f, op, "mission", s))
	if code != 200 {
		t.Fatal(code, string(b))
	}
}
