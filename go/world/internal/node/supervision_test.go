package node

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/mission"
	"github.com/wendylabsinc/wendy/go/world/internal/supervision"
	"github.com/wendylabsinc/wendy/go/world/internal/tasking"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

type supervisionReplayCounter struct {
	authority.ReplayStore
	checkpoints int
}

func (c *supervisionReplayCounter) Checkpoint(revision uint64, digest string, now time.Time) error {
	c.checkpoints++
	return c.ReplayStore.Checkpoint(revision, digest, now)
}

func TestUnauthenticatedSupervisionCannotWriteReplayState(t *testing.T) {
	f, _, _, _, _, _, _ := supervisedFixture(t)
	counter := &supervisionReplayCounter{ReplayStore: authority.NewMemoryReplay(64)}
	v, err := authority.NewVerifier(authority.Config{RootPublicKey: f.root.Public().(ed25519.PublicKey), Tenant: "tenant", Site: "office", Domain: "simulation", Audience: f.audience, Now: time.Now, Replay: counter})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	rev, err := authority.SignRevocations(authority.Revocations{Version: 1, Tenant: "tenant", Site: "office", Domain: "simulation", Revision: 1, IssuedAtUnixMS: now.Add(-time.Second).UnixMilli(), ExpiresAtUnixMS: now.Add(time.Minute).UnixMilli()}, f.root)
	if err != nil {
		t.Fatal(err)
	}
	if err = v.UpdateRevocations(rev); err != nil {
		t.Fatal(err)
	}
	counter.checkpoints = 0
	f.n.verifier = v
	for _, endpoint := range []string{"challenge", "ack"} {
		r := httptest.NewRequest("POST", "/v1/tasks/supervision/"+endpoint, strings.NewReader(`{}`))
		w := httptest.NewRecorder()
		f.n.Handler().ServeHTTP(w, r)
		if w.Code != 401 {
			t.Fatal("anonymous request not refused", endpoint, w.Code)
		}
	}
	if counter.checkpoints != 0 {
		t.Fatal("anonymous requests changed authority state", counter.checkpoints)
	}
}

func supervisedFixture(t *testing.T) (*fixture, Credentials, Credentials, Credentials, Credentials, tasking.Spec, uint64) {
	return supervisedFixtureConfigured(t, nil)
}
func supervisedFixtureConfigured(t *testing.T, configure func(*tasking.Config)) (*fixture, Credentials, Credentials, Credentials, Credentials, tasking.Spec, uint64) {
	if configure == nil {
		return supervisedFixtureWithSetup(t, nil)
	}
	return supervisedFixtureWithSetup(t, func(_ *fixture, _ Credentials, cfg *tasking.Config) { configure(cfg) })
}
func supervisedFixtureWithSetup(t *testing.T, configure func(*fixture, Credentials, *tasking.Config)) (*fixture, Credentials, Credentials, Credentials, Credentials, tasking.Spec, uint64) {
	t.Helper()
	f, op, ex, _, cfg := taskFixture(t)
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	g, err := authority.SignGrant(authority.Grant{Version: 1, ID: "gateway", Principal: "gateway", Kind: "device", Tenant: "tenant", Site: "office", Domain: "simulation", TaskStatusAudience: f.audience, TaskStatusAssignee: cfg.Assignee, Source: world.Source{Device: "gateway", App: "world", Service: "world", Stream: "state"}, PublicKey: pub, Scopes: []string{"task-status", "supervision-server", "read"}, NotBeforeUnixMS: time.Now().Add(-time.Minute).UnixMilli(), NotAfterUnixMS: time.Now().Add(time.Hour).UnixMilli()}, f.root)
	if err != nil {
		t.Fatal(err)
	}
	server := Credentials{Grant: g, PrivateKey: key}
	if err = f.n.cfg.Tasks.Close(); err != nil {
		t.Fatal(err)
	}
	cfg.RunID = "live"
	cfg.Tenant = "tenant"
	cfg.Audience = f.audience
	cfg.GatewayPrincipal = "gateway"
	cfg.ValidateSupervisor = SupervisorAuthority(f.v, g)
	if configure != nil {
		cfg.ValidateAuthorityContext = MissionAuthorityContext(f.v)
		cfg.ValidateSupervisorContext = SupervisorAuthorityContext(f.v, g)
		configure(f, server, &cfg)
	}
	s, err := tasking.Open(filepath.Join(t.TempDir(), "supervised.tasks"), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	f.n.cfg.Tasks = s
	f.n.cfg.Audience = f.audience
	f.n.cfg.SupervisionEnabled = true
	f.n.cfg.TaskStatusCredentials = server
	if _, err = New(f.n.cfg, f.s, f.v); err != nil {
		t.Fatal("supervision configuration", err)
	}
	a := missionCredentials(t, f, "operator", "supervisor-a", "", "supervise", "stop", "read")
	b := missionCredentials(t, f, "operator", "supervisor-b", "", "supervise", "stop", "read")
	epoch := startExecutor(t, f, ex)
	spec := taskSpec("supervised")
	spec.Mode = mission.Supervised
	spec.Fallback = mission.Hold
	code, body := call(t, f, op, "POST", "/v1/tasks", "mission", instructionBody(t, f, op, "mission", spec))
	if code != 200 {
		t.Fatalf("supervised admission %d %s", code, body)
	}
	return f, op, ex, a, b, spec, epoch
}
func challenge(t *testing.T, f *fixture, operator Credentials, spec tasking.Spec) authority.SignedSupervisionChallenge {
	t.Helper()
	nonce := make([]byte, 24)
	if _, err := rand.Read(nonce); err != nil {
		t.Fatal(err)
	}
	body := jsonBody(t, SupervisionChallengeRequest{TaskID: spec.ID, TaskDigest: tasking.SpecDigest(spec), RequestID: hex.EncodeToString(nonce)})
	code, body := call(t, f, operator, "POST", "/v1/tasks/supervision/challenge", "supervise", body)
	if code != 200 {
		t.Fatalf("challenge %d %s", code, body)
	}
	var proof authority.SignedSupervisionChallenge
	if err := authority.DecodeStrict(body, &proof); err != nil {
		t.Fatal(err)
	}
	if _, err := f.v.VerifySupervisionChallenge(proof, "gateway"); err != nil {
		t.Fatal(err)
	}
	if proof.Challenge.TaskID != spec.ID || proof.Challenge.SpecSHA256 != tasking.SpecDigest(spec) || proof.Challenge.OperatorPrincipal != operator.Grant.Grant.Principal || proof.Challenge.RequestID != hex.EncodeToString(nonce) {
		t.Fatal("challenge binding changed")
	}
	return proof
}
func acknowledge(t *testing.T, f *fixture, operator Credentials, c authority.SignedSupervisionChallenge) []byte {
	t.Helper()
	proof, err := authority.SignSupervisionAck(operator.Grant, operator.PrivateKey, supervision.Ack{Challenge: c.Challenge})
	if err != nil {
		t.Fatal(err)
	}
	body := jsonBody(t, proof)
	code, response := call(t, f, operator, "POST", "/v1/tasks/supervision/ack", "supervise", body)
	if code != 200 {
		t.Fatalf("ACK %d %s", code, response)
	}
	var receipt authority.SignedSupervisionReceipt
	if err = authority.DecodeStrict(response, &receipt); err != nil {
		t.Fatal(err)
	}
	if _, err = f.v.VerifySupervisionReceipt(receipt, "gateway"); err != nil || receipt.Receipt.Challenge != c.Challenge || receipt.Receipt.AckSHA256 != authority.SupervisionAckDigest(proof) {
		t.Fatal("receipt did not bind ACK", err)
	}
	return body
}
func evaluateSupervised(t *testing.T, f *fixture, ex Credentials, spec tasking.Spec, epoch uint64) tasking.Decision {
	t.Helper()
	sample := telemetry()
	if f.requestNow != nil {
		sample.SampleUnixMS = f.requestNow().UnixMilli()
	}
	sample.SupervisionUntilUnixMS = time.Now().Add(time.Hour).UnixMilli()
	sample.CommandUntilUnixMS = sample.SupervisionUntilUnixMS
	code, body := call(t, f, ex, "POST", "/v1/tasks/evaluate", "execute", jsonBody(t, EvaluateRequest{TaskID: spec.ID, ExecutorEpoch: epoch, Telemetry: sample}))
	if code != 200 {
		t.Fatalf("evaluate %d %s", code, body)
	}
	var decision tasking.Decision
	if err := json.Unmarshal(body, &decision); err != nil {
		t.Fatal(err)
	}
	return decision
}
func revokeSupervisors(t *testing.T, f *fixture, revision uint64, principals ...string) {
	t.Helper()
	now := time.Now()
	rev, err := authority.SignRevocations(authority.Revocations{Version: 1, Tenant: "tenant", Site: "office", Domain: "simulation", Revision: revision, IssuedAtUnixMS: now.Add(-time.Millisecond).UnixMilli(), ExpiresAtUnixMS: now.Add(30 * time.Minute).UnixMilli(), Principals: principals}, f.root)
	if err != nil {
		t.Fatal(err)
	}
	code, body := call(t, f, f.credentials, "POST", "/v1/revocations", "replicate", jsonBody(t, rev))
	if code != 200 {
		t.Fatalf("revocation %d %s", code, body)
	}
}
func TestHTTPSupervisionTwoOperatorsRevocationAndReplay(t *testing.T) {
	f, op, ex, a, b, spec, epoch := supervisedFixture(t)
	d := evaluateSupervised(t, f, ex, spec, epoch)
	if d.Action != mission.Hold || d.Record.Status.Phase != mission.Accepted {
		t.Fatal("telemetry invented supervision", d)
	}
	request := jsonBody(t, SupervisionChallengeRequest{TaskID: spec.ID, TaskDigest: tasking.SpecDigest(spec), RequestID: "unauthorized-request"})
	if code, _ := call(t, f, op, "POST", "/v1/tasks/supervision/challenge", "supervise", request); code != 401 {
		t.Fatal("ordinary mission operator gained supervision", code)
	}
	ca := challenge(t, f, a, spec)
	ackA := acknowledge(t, f, a, ca)
	cb := challenge(t, f, b, spec)
	acknowledge(t, f, b, cb)
	if d = evaluateSupervised(t, f, ex, spec, epoch); d.Action != mission.Continue || d.Record.Status.Phase != mission.Running {
		t.Fatal("valid roundtrip did not start", d)
	}
	if code, _ := call(t, f, a, "POST", "/v1/tasks/supervision/ack", "supervise", ackA); code == 200 {
		t.Fatal("duplicate ACK renewed lease")
	}
	if code, _ := call(t, f, b, "POST", "/v1/tasks/supervision/ack", "supervise", ackA); code != 403 {
		t.Fatal("transport principal substituted for ACK signer", code)
	}
	revokeSupervisors(t, f, 2, a.Grant.Grant.Principal)
	if d = evaluateSupervised(t, f, ex, spec, epoch); d.Action != mission.Continue {
		t.Fatal("one revoked operator stopped remaining valid supervisor", d)
	}
	revokeSupervisors(t, f, 3, a.Grant.Grant.Principal, b.Grant.Grant.Principal)
	if d = evaluateSupervised(t, f, ex, spec, epoch); d.Action != mission.Hold || d.Record.Status.Phase != mission.Held {
		t.Fatal("final revoked operator did not latch HOLD", d)
	}
	c := missionCredentials(t, f, "operator", "supervisor-c", "", "supervise", "stop", "read")
	if code, _ := call(t, f, c, "POST", "/v1/tasks/supervision/challenge", "supervise", request); code != 409 {
		t.Fatal("fresh supervisor resumed held mission", code)
	}
}
func TestHTTPSupervisionGatewayRevocationAndExecutorFence(t *testing.T) {
	for _, kind := range []string{"gateway-revoked", "executor-restarted"} {
		t.Run(kind, func(t *testing.T) {
			f, _, ex, a, _, spec, epoch := supervisedFixture(t)
			proof := acknowledge(t, f, a, challenge(t, f, a, spec))
			if d := evaluateSupervised(t, f, ex, spec, epoch); d.Action != mission.Continue {
				t.Fatal(d)
			}
			if kind == "gateway-revoked" {
				revokeSupervisors(t, f, 2, "gateway")
				if d := evaluateSupervised(t, f, ex, spec, epoch); d.Action != mission.Hold {
					t.Fatal("revoked gateway sustained leases", d)
				}
			} else {
				fresh := startExecutor(t, f, ex)
				if fresh == epoch {
					t.Fatal("executor epoch unchanged")
				}
				if d := evaluateSupervised(t, f, ex, spec, fresh); d.Record.Status.Phase != mission.Failed {
					t.Fatal("restart resumed motion", d)
				}
			}
			if code, _ := call(t, f, a, "POST", "/v1/tasks/supervision/ack", "supervise", proof); code == 200 {
				t.Fatal("old ACK survived authority/generation loss")
			}
		})
	}
}
