package node

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/client"
	"github.com/wendylabsinc/wendy/go/world/internal/evidence"
	"github.com/wendylabsinc/wendy/go/world/internal/mission"
	"github.com/wendylabsinc/wendy/go/world/internal/missionclock"
	"github.com/wendylabsinc/wendy/go/world/internal/missioncontrol"
	"github.com/wendylabsinc/wendy/go/world/internal/proposals"
	"github.com/wendylabsinc/wendy/go/world/internal/tasking"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func nativeReviewFixture(t *testing.T) (*proposalReviewFixture, Credentials, tasking.Spec) {
	return nativeReviewFixtureArea(t, missioncontrol.Bounds{Min: [3]float64{-2, 0, -1}, Max: [3]float64{2, 1, 1}})
}

func nativeReviewFixtureArea(t *testing.T, area missioncontrol.Bounds) (*proposalReviewFixture, Credentials, tasking.Spec) {
	t.Helper()
	r := newProposalReviewFixture(t)
	f := r.f
	f.n.cfg.Tasks.Close()
	f.n.cfg.Proposals.Close()
	c, err := authority.SignMissionControlContext(missioncontrol.Context{Version: 1, Tenant: "tenant", Site: "office", Audience: f.audience, RunID: r.cfg.RunID, PackageSHA256: strings.Repeat("a", 64), Assignee: r.cfg.Assignee, Frame: f.wc.Frames[0], Type: tasking.InspectType, Class: mission.Move, Mode: mission.Autonomous, Area: area, RequiredSensors: []string{"obstacles"}, MinimumEnergy: .1, Fallback: mission.Hold, SpeedMPS: .25, ToleranceM: .1, StopSpeedMPS: .05, MaxDurationMS: 60000, NotBeforeUnixMS: time.Now().Add(-time.Minute).UnixMilli(), NotAfterUnixMS: time.Now().Add(time.Hour).UnixMilli()}, f.root)
	if err != nil {
		t.Fatal(err)
	}
	tc := tasking.Config{ControlContext: &c, RunID: r.cfg.RunID, Site: "office", Assignee: r.cfg.Assignee, ExecutorPrincipal: r.executor.Grant.Grant.Principal, Domain: mission.Simulation, Catalog: map[string]mission.SafetyClass{tasking.InspectType: mission.Move}, Now: time.Now, ValidateAuthority: MissionAuthority(f.v)}
	f.n.cfg.Tasks, err = tasking.Open(filepath.Join(t.TempDir(), "native-tasks.json"), tc)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.n.cfg.Tasks.Close() })
	r.cfg.Catalog = tc.Catalog
	r.path = filepath.Join(t.TempDir(), "native-proposals.jsonl")
	f.n.cfg.Proposals, err = proposals.Open(r.path, r.cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.n.cfg.Proposals.Close() })
	f.n.cfg.Evidence, err = evidence.Open(t.TempDir(), evidence.Config{Tenant: "tenant", Site: "office"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.n.cfg.Evidence.Close() })
	op := missionCredentials(t, f, "operator", "native-operator", "", "read", "mission", "stop")
	s := taskSpec("native")
	s.Type = tasking.InspectType
	s.ControlContext = &c
	s.Area = tasking.Bounds{Min: c.Context.Area.Min, Max: c.Context.Area.Max}
	s.Fallback = mission.Hold
	s.SpeedMPS = .25
	s.NotAfterUnixMS = time.Now().Add(30 * time.Second).UnixMilli()
	return r, op, s
}
func nativeProposal(t *testing.T, r *proposalReviewFixture, s tasking.Spec, parameters string) *missioncontrol.ProposalRef {
	t.Helper()
	p := r.request("proposal-" + s.ID)
	p.Type = tasking.InspectType
	p.Parameters = json.RawMessage(parameters)
	code, body := reviewProposalPost(t, r, r.agent, jsonBody(t, p))
	if code != http.StatusCreated {
		t.Fatal(code, string(body))
	}
	var v proposals.View
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatal(err)
	}
	return &missioncontrol.ProposalRef{Audience: r.f.audience, ID: p.ID, BodySHA256: v.Record.BodySHA256}
}
func TestNativeProposalApprovalRetainsProofAfterProposalExpiry(t *testing.T) {
	r, op, s := nativeReviewFixture(t)
	s.Proposal = nativeProposal(t, r, s, `{"target":[1,0.2,0]}`)
	epoch := startExecutor(t, r.f, r.executor)
	body := instructionBody(t, r.f, op, "mission", s)
	code, reply := call(t, r.f, op, "POST", "/v1/tasks", "mission", body)
	if code != 200 {
		t.Fatal(code, string(reply))
	}
	var before tasking.Record
	_ = json.Unmarshal(reply, &before)
	r.now.Store(time.Now().Add(20 * time.Minute).UnixMilli())
	code, reply = call(t, r.f, op, "POST", "/v1/tasks", "mission", body)
	if code != 200 {
		t.Fatal("exact retry after proposal expiry", code, string(reply))
	}
	var after tasking.Record
	_ = json.Unmarshal(reply, &after)
	if !bytes.Equal(before.Proof, after.Proof) || tasking.SpecDigest(before.Spec) != tasking.SpecDigest(after.Spec) {
		t.Fatal("retry changed original approval")
	}
	tele := telemetry()
	tele.Position = [3]float64{0, .2, 0}
	code, reply = call(t, r.f, r.executor, "POST", "/v1/tasks/evaluate", "execute", jsonBody(t, EvaluateRequest{TaskID: s.ID, ExecutorEpoch: epoch, Telemetry: tele}))
	if code != 200 {
		t.Fatal("proposal expiry canceled approved mission", code, string(reply))
	}
	var d tasking.Decision
	_ = json.Unmarshal(reply, &d)
	if d.Action != mission.Continue {
		t.Fatal("proposal expiry stopped mission", d.Action, d.Record.Status)
	}
	s.ID = "another-task"
	code, reply = call(t, r.f, op, "POST", "/v1/tasks", "mission", instructionBody(t, r.f, op, "mission", s))
	if code == 200 {
		t.Fatal("expired proposal created new approval", string(reply))
	}
}
func TestNativeProposalTypedParametersAndContextPolicy(t *testing.T) {
	for name, parameters := range map[string]string{"extra": `{"target":[1,0.2,0],"speed_mps":10}`, "short": `{"target":[1,0.2]}`, "null": `{"target":[1,0.2,null]}`, "wrong": `{"target":[0,0.2,0]}`} {
		t.Run(name, func(t *testing.T) {
			r, op, s := nativeReviewFixture(t)
			s.Proposal = nativeProposal(t, r, s, parameters)
			code, body := call(t, r.f, op, "POST", "/v1/tasks", "mission", instructionBody(t, r.f, op, "mission", s))
			if code == 200 {
				t.Fatal("arbitrary proposal executed", string(body))
			}
		})
	}
	for _, kind := range []string{"context", "deadline", "hash", "audience", "signature"} {
		t.Run(kind, func(t *testing.T) {
			r, op, s := nativeReviewFixture(t)
			s.Proposal = nativeProposal(t, r, s, `{"target":[1,0.2,0]}`)
			switch kind {
			case "context":
				s.ControlContext.Context.PackageSHA256 = strings.Repeat("b", 64)
			case "deadline":
				s.NotAfterUnixMS = time.Now().Add(90 * time.Second).UnixMilli()
			case "hash":
				s.Proposal.BodySHA256 = strings.Repeat("f", 64)
			case "audience":
				s.Proposal.Audience = "other"
			case "signature":
				s.ControlContext.Signature[0] ^= 1
			}
			code, body := call(t, r.f, op, "POST", "/v1/tasks", "mission", instructionBody(t, r.f, op, "mission", s))
			if code == 200 {
				t.Fatal("changed binding admitted", string(body))
			}
		})
	}
}
func enableMissionClock(t *testing.T, f *fixture) missionclock.Pins {
	t.Helper()
	cred := issueCredentials(t, f.root, "mission-clock")
	g := cred.Grant.Grant
	g.Kind = "device"
	g.PublishAudience = f.audience
	g.Scopes = []string{"mission-clock-server"}
	var err error
	cred.Grant, err = authority.SignGrant(g, f.root)
	if err != nil {
		t.Fatal(err)
	}
	f.n.cfg.Audience = f.audience
	f.n.cfg.MissionClock = &MissionClockConfig{Credentials: cred, RunID: "live", ClockDomain: "simulation:live"}
	if err = f.n.configureMissionClock(); err != nil {
		t.Fatal(err)
	}
	return missionclock.Pins{RootPublicKey: f.root.Public().(ed25519.PublicKey), GatewayPrincipal: g.Principal, GatewayKeySHA256: authority.KeyID(g.PublicKey), RunID: "live", ClockDomain: "simulation:live"}
}
func TestMissionClockHTTPAndExpiredStopClient(t *testing.T) {
	f, op, _, _, _ := taskFixture(t)
	pins := enableMissionClock(t, f)
	g := op.Grant.Grant
	g.NotAfterUnixMS = time.Now().Add(-time.Second).UnixMilli()
	op.Grant, _ = authority.SignGrant(g, f.root)
	c, err := client.New(client.Config{URL: f.server.URL, Audience: f.audience, Grant: op.Grant, PrivateKey: op.PrivateKey, AllowInsecureSimulation: true})
	if err != nil {
		t.Fatal(err)
	}
	origin := time.Now()
	elapsed := func() time.Duration { return time.Since(origin) }
	clock, err := c.ObserveMissionClock(context.Background(), pins, "stop", elapsed)
	if err != nil {
		t.Fatal("expired stop clock unavailable", err)
	}
	interval, err := clock.Interval()
	if err != nil {
		t.Fatal(err)
	}
	stop, err := authority.SignInstruction(op.Grant, op.PrivateKey, f.audience, "stop", json.RawMessage(`{"task_id":"unseen-native"}`), time.UnixMilli(interval.LowerUnixMS), 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	var record tasking.Record
	err = c.DoWithMissionClock(context.Background(), clock, "stop", "POST", "/v1/tasks/stop", jsonBody(t, stop), &record)
	if err != nil {
		t.Fatal("expired stop failed", err)
	}
	if record.Status.Phase != mission.Canceled {
		t.Fatal(record.Status)
	}
	f.n.missionClockLast = time.Time{}
	if _, err = c.ObserveMissionClock(context.Background(), pins, "mission", elapsed); err == nil {
		t.Fatal("expired mission grant bootstrapped")
	}
	for _, path := range []string{"/v1/tasks/clock?x=1", "/v1/tasks/clock?"} {
		w := httptest.NewRecorder()
		req := httptest.NewRequest("POST", path, strings.NewReader(`{}`))
		f.n.Handler().ServeHTTP(w, req)
		if w.Code != 400 {
			t.Fatal(path, w.Code)
		}
	}
	f.n.missionClockLast = time.Time{}
	f.n.missionClockMu.Lock()
	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/tasks/clock", strings.NewReader(`{}`))
	f.n.Handler().ServeHTTP(w, req)
	f.n.missionClockMu.Unlock()
	if w.Code != 429 {
		t.Fatal("unbounded concurrent clock", w.Code)
	}
}
