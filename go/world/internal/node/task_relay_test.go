package node

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/mission"
	"github.com/wendylabsinc/wendy/go/world/internal/tasking"
	"github.com/wendylabsinc/wendy/go/world/internal/taskrelay"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

type relayFixtures struct {
	robot, middle, reader      *fixture
	operator, executor, status Credentials
	paths                      map[*fixture]string
	configs                    map[*fixture]taskrelay.Config
}

func relayStatusCredentials(t *testing.T, f *fixture, id string) Credentials {
	t.Helper()
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	g := authority.Grant{Version: authority.Version, ID: id, Principal: "robot-status", Kind: "device", Tenant: "tenant", Site: "office", Domain: "simulation", Source: world.Source{Device: "robot-node", App: "world", Service: "tasks", Stream: "status"}, PublicKey: pub, Scopes: []string{"task-status"}, TaskStatusAudience: "robot-node", TaskStatusAssignee: "robot/navigation", NotBeforeUnixMS: time.Now().Add(-time.Minute).UnixMilli(), NotAfterUnixMS: time.Now().Add(time.Hour).UnixMilli()}
	grant, err := authority.SignGrant(g, f.root)
	if err != nil {
		t.Fatal(err)
	}
	return Credentials{Grant: grant, PrivateKey: key}
}

func newRelayFixtures(t *testing.T) *relayFixtures {
	t.Helper()
	r, op, ex, _, _ := taskFixture(t)
	f := &relayFixtures{robot: r, middle: newFixture(t, r.root, "middle"), reader: newFixture(t, r.root, "reader"), operator: op, executor: ex, paths: map[*fixture]string{}, configs: map[*fixture]taskrelay.Config{}}
	f.status = relayStatusCredentials(t, r, "status-grant-1")
	routes := map[string]taskrelay.Route{"robot-node": {Assignee: "robot/navigation", StatusPrincipal: "robot-status"}}
	for _, node := range []*fixture{f.robot, f.middle, f.reader} {
		cfg := taskrelay.Config{Tenant: "tenant", Site: "office", Domain: "simulation", Routes: routes, Verifier: node.v, Now: time.Now, MaxEntries: 32}
		path := filepath.Join(t.TempDir(), "relay.json")
		s, err := taskrelay.Open(path, cfg)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = s.Close() })
		node.n.cfg.TaskRelay = s
		node.n.cfg.TaskRoutes = routes
		node.n.cfg.Audience = node.audience
		f.paths[node], f.configs[node] = path, cfg
	}
	r.n.cfg.TaskStatusCredentials = f.status
	return f
}

func pullTaskPeer(t *testing.T, destination, source *fixture) {
	t.Helper()
	if err := destination.n.Sync(context.Background(), Peer{URL: source.server.URL, Audience: source.audience}, destination.credentials); err != nil {
		t.Fatal(err)
	}
}

func readRelay(t *testing.T, f *fixture) TaskRelayResponse {
	t.Helper()
	code, body := call(t, f, f.credentials, http.MethodGet, "/v1/task-relay", "read", nil)
	if code != http.StatusOK {
		t.Fatalf("relay read: %d %s", code, body)
	}
	var out TaskRelayResponse
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	if !out.LastKnown {
		t.Fatal("replicated state presented as live execution")
	}
	return out
}

func submitRelay(t *testing.T, f *relayFixtures, operation string, payload any) []byte {
	t.Helper()
	body := instructionBody(t, f.robot, f.operator, operation, payload)
	code, response := call(t, f.reader, f.operator, http.MethodPost, "/v1/task-relay", operation, body)
	if code != http.StatusAccepted {
		t.Fatalf("relay ingress: %d %s", code, response)
	}
	var ack struct {
		State string `json:"state"`
	}
	if err := json.Unmarshal(response, &ack); err != nil || ack.State != "queued" {
		t.Fatalf("ingress claimed admission: %s %v", response, err)
	}
	return body
}

func statusRecord(t *testing.T, reply TaskRelayResponse, id string) (authority.SignedTaskStatus, tasking.Record) {
	t.Helper()
	for _, e := range reply.Entries {
		if e.TaskID != id || e.Status == nil {
			continue
		}
		var record tasking.Record
		if err := json.Unmarshal(e.Status.Record, &record); err != nil {
			t.Fatal(err)
		}
		return *e.Status, record
	}
	t.Fatalf("missing signed status for %s: %+v", id, reply)
	return authority.SignedTaskStatus{}, tasking.Record{}
}

func TestTaskRelayThreeGatewayDeliveryAndTerminalRepair(t *testing.T) {
	f := newRelayFixtures(t)
	epoch := startExecutor(t, f.robot, f.executor)
	body := submitRelay(t, f, "mission", taskSpec("through-peers"))
	if _, err := f.robot.n.cfg.Tasks.Get("through-peers"); err == nil {
		t.Fatal("ingress bypassed peer delivery")
	}
	pullTaskPeer(t, f.middle, f.reader)
	if f.middle.n.cfg.Tasks != nil || f.reader.n.cfg.Tasks != nil {
		t.Fatal("relay owns execution store")
	}
	pullTaskPeer(t, f.robot, f.middle)
	admitted, err := f.robot.n.cfg.Tasks.Get("through-peers")
	if err != nil || admitted.Status.Phase != mission.Accepted || !bytes.Equal(admitted.Proof, body) {
		t.Fatalf("assignee did not preserve operator admission: %+v %v", admitted, err)
	}
	req := EvaluateRequest{TaskID: "through-peers", ExecutorEpoch: epoch, Telemetry: telemetry()}
	code, response := call(t, f.robot, f.executor, http.MethodPost, "/v1/tasks/evaluate", "execute", jsonBody(t, req))
	if code != 200 {
		t.Fatal(code, string(response))
	}
	req.Telemetry = telemetry()
	req.Telemetry.Position = [3]float64{1, .2, 0}
	code, response = call(t, f.robot, f.executor, http.MethodPost, "/v1/tasks/evaluate", "execute", jsonBody(t, req))
	if code != 200 {
		t.Fatal(code, string(response))
	}
	if err = f.robot.n.ProcessTaskRelay(); err != nil {
		t.Fatal(err)
	}
	pullTaskPeer(t, f.middle, f.robot)
	pullTaskPeer(t, f.reader, f.middle)
	signed, record := statusRecord(t, readRelay(t, f.reader), "through-peers")
	if record.Status.Phase != mission.Succeeded || !signed.Terminal || signed.Grant.Grant.Principal != "robot-status" || !bytes.Equal(record.Proof, body) {
		t.Fatal("wrong signed terminal outcome", record.Status)
	}
	if _, err = f.reader.v.VerifyTaskStatus(signed); err != nil {
		t.Fatal(err)
	}
	// The reader's durable history remains available without contacting peers.
	if err = f.reader.n.cfg.TaskRelay.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := taskrelay.Open(f.paths[f.reader], f.configs[f.reader])
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	f.reader.n.cfg.TaskRelay = reopened
	_, after := statusRecord(t, readRelay(t, f.reader), "through-peers")
	if !bytes.Equal(jsonBody(t, record), jsonBody(t, after)) {
		t.Fatal("reader restart changed terminal history")
	}
}

func TestTaskRelayIngressBindsTransportOperatorAndFinalAudience(t *testing.T) {
	f := newRelayFixtures(t)
	body := instructionBody(t, f.robot, f.operator, "mission", taskSpec("signed"))
	other := missionCredentials(t, f.robot, "operator", "other-operator", "", "mission", "stop", "read")
	code, _ := call(t, f.reader, other, "POST", "/v1/task-relay", "mission", body)
	if code != http.StatusUnauthorized {
		t.Fatal("transport substituted instruction issuer", code)
	}
	wrongAudience := instructionBody(t, f.reader, f.operator, "mission", taskSpec("wrong-destination"))
	code, _ = call(t, f.reader, f.operator, "POST", "/v1/task-relay", "mission", wrongAudience)
	if code != http.StatusForbidden {
		t.Fatal("unknown final audience accepted", code)
	}
	var changed authority.SignedInstruction
	if err := json.Unmarshal(body, &changed); err != nil {
		t.Fatal(err)
	}
	changed.Payload = jsonBody(t, taskSpec("altered"))
	code, _ = call(t, f.reader, f.operator, "POST", "/v1/task-relay", "mission", jsonBody(t, changed))
	if code != http.StatusUnauthorized {
		t.Fatal("altered immutable instruction accepted", code)
	}
	if got := readRelay(t, f.reader); len(got.Entries) != 0 {
		t.Fatal("failed ingress persisted work")
	}
}

func TestTaskRelayReadDoesNotDispatchQueuedMission(t *testing.T) {
	f := newRelayFixtures(t)
	body := instructionBody(t, f.robot, f.operator, "mission", taskSpec("wait-for-worker"))
	code, response := call(t, f.robot, f.operator, "POST", "/v1/task-relay", "mission", body)
	if code != http.StatusAccepted {
		t.Fatal(code, string(response))
	}
	readRelay(t, f.robot)
	if _, err := f.robot.n.cfg.Tasks.Get("wait-for-worker"); err == nil {
		t.Fatal("read request admitted queued mission")
	}
	if err := f.robot.n.ProcessTaskRelay(); err != nil {
		t.Fatal(err)
	}
	got, err := f.robot.n.cfg.Tasks.Get("wait-for-worker")
	if err != nil || got.Status.Phase != mission.Accepted {
		t.Fatal("worker did not admit queued instruction", got, err)
	}
}

func TestTaskRelayStopBeforeMissionDominatesAcrossPeers(t *testing.T) {
	f := newRelayFixtures(t)
	submitRelay(t, f, "stop", StopRequest{TaskID: "never-start"})
	submitRelay(t, f, "mission", taskSpec("never-start"))
	pullTaskPeer(t, f.middle, f.reader)
	pullTaskPeer(t, f.robot, f.middle)
	got, err := f.robot.n.cfg.Tasks.Get("never-start")
	if err != nil || got.Status.Phase != mission.Canceled || got.Spec.Type != "" {
		t.Fatal("delayed mission defeated stop", got, err)
	}
	audit, err := f.robot.n.cfg.Tasks.AuditLog()
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range audit {
		if event.TaskID == "never-start" && event.Event == "accepted" {
			t.Fatal("stopped task entered executor")
		}
	}
	pullTaskPeer(t, f.middle, f.robot)
	pullTaskPeer(t, f.reader, f.middle)
	_, replicated := statusRecord(t, readRelay(t, f.reader), "never-start")
	if replicated.Status.Phase != mission.Canceled {
		t.Fatal("stop outcome not replicated")
	}
}

func TestTaskRelayBadPeerStatusCannotOverwriteAssigneeAndWorldFailureDoesNotBlockRepair(t *testing.T) {
	f := newRelayFixtures(t)
	submitRelay(t, f, "stop", StopRequest{TaskID: "stopped"})
	pullTaskPeer(t, f.middle, f.reader)
	pullTaskPeer(t, f.robot, f.middle)
	entries, err := f.robot.n.cfg.TaskRelay.Export()
	if err != nil {
		t.Fatal(err)
	}
	var fake TaskRelayResponse
	if err = json.Unmarshal(jsonBody(t, TaskRelayResponse{Entries: entries, LastKnown: true}), &fake); err != nil {
		t.Fatal(err)
	}
	for j := range fake.Entries {
		if fake.Entries[j].Status != nil {
			fake.Entries[j].Status.Signature[0] ^= 1
		}
	}
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/repair/inventory" {
			http.Error(w, "world plane unavailable", 503)
			return
		}
		writeJSON(w, 200, fake)
	}))
	defer peer.Close()
	if err = f.reader.n.Sync(context.Background(), Peer{URL: peer.URL, Audience: "bad-peer"}, f.reader.credentials); err == nil {
		t.Fatal("forged status and world failure silently succeeded")
	}
	for _, e := range readRelay(t, f.reader).Entries {
		if e.Status != nil {
			t.Fatal("tampered assignee status entered replica")
		}
	}
	// The same unavailable observation endpoint must not prevent a valid task
	// record from the independent relay endpoint reaching the reader.
	validPeer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/repair/inventory" {
			http.Error(w, "world plane unavailable", 503)
			return
		}
		writeJSON(w, 200, TaskRelayResponse{Entries: entries, LastKnown: true})
	}))
	defer validPeer.Close()
	if err = f.reader.n.Sync(context.Background(), Peer{URL: validPeer.URL, Audience: "valid-peer"}, f.reader.credentials); err == nil {
		t.Fatal("world error lost")
	}
	_, got := statusRecord(t, readRelay(t, f.reader), "stopped")
	if got.Status.Phase != mission.Canceled {
		t.Fatal("independent task repair starved")
	}
}

func TestTaskRelayReadSuppressesRevokedGatewayStatusAndRenewalRepairsIt(t *testing.T) {
	f := newRelayFixtures(t)
	submitRelay(t, f, "stop", StopRequest{TaskID: "history"})
	pullTaskPeer(t, f.middle, f.reader)
	pullTaskPeer(t, f.robot, f.middle)
	pullTaskPeer(t, f.reader, f.robot)
	statusRecord(t, readRelay(t, f.reader), "history")
	now := time.Now()
	rev, err := authority.SignRevocations(authority.Revocations{Version: 1, Tenant: "tenant", Site: "office", Domain: "simulation", Revision: 2, IssuedAtUnixMS: now.Add(-time.Second).UnixMilli(), ExpiresAtUnixMS: now.Add(30 * time.Minute).UnixMilli(), GrantIDs: []string{f.status.Grant.Grant.ID}}, f.robot.root)
	if err != nil {
		t.Fatal(err)
	}
	for _, node := range []*fixture{f.robot, f.reader} {
		if err = node.v.UpdateRevocations(rev); err != nil {
			t.Fatal(err)
		}
	}
	for _, e := range readRelay(t, f.reader).Entries {
		if e.Status != nil {
			t.Fatal("revoked cached status presented as trusted")
		}
	}
	for _, e := range readRelay(t, f.robot).Entries {
		if e.Status != nil {
			t.Fatal("revoked local status presented as trusted")
		}
	}
	f.robot.n.cfg.TaskStatusCredentials = relayStatusCredentials(t, f.robot, "status-grant-2")
	// Signed observation times are millisecond timestamps. Move past the prior
	// observation so renewal can produce a distinct newer attestation.
	time.Sleep(2 * time.Millisecond)
	if err = f.robot.n.ProcessTaskRelay(); err != nil {
		t.Fatal(err)
	}
	pullTaskPeer(t, f.reader, f.robot)
	signed, record := statusRecord(t, readRelay(t, f.reader), "history")
	if signed.Grant.Grant.ID != "status-grant-2" || record.Status.Phase != mission.Canceled {
		t.Fatal("renewed owner could not reattest immutable outcome")
	}
}
