package taskrelay

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/mission"
	"github.com/wendylabsinc/wendy/go/world/internal/tasking"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

type fixture struct {
	t                            *testing.T
	now                          time.Time
	root, operatorKey, deviceKey ed25519.PrivateKey
	operator, device             authority.SignedGrant
	verifier                     *authority.Verifier
	cfg                          Config
	path                         string
	store                        *Store
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{t: t, now: time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC), path: filepath.Join(t.TempDir(), "relay")}
	rootPub, root, _ := ed25519.GenerateKey(rand.Reader)
	f.root = root
	opPub, opKey, _ := ed25519.GenerateKey(rand.Reader)
	f.operatorKey = opKey
	devPub, devKey, _ := ed25519.GenerateKey(rand.Reader)
	f.deviceKey = devKey
	base := authority.Grant{Version: authority.Version, ID: "operator-grant", Tenant: "tenant", Site: "office", Domain: "simulation", Principal: "operator", Kind: "operator", Source: world.Source{Device: "laptop", App: "world", Service: "operator", Stream: "tasks"}, PublicKey: opPub, Scopes: []string{"mission", "stop", "read"}, NotBeforeUnixMS: f.now.Add(-time.Minute).UnixMilli(), NotAfterUnixMS: f.now.Add(time.Hour).UnixMilli()}
	var err error
	f.operator, err = authority.SignGrant(base, root)
	if err != nil {
		t.Fatal(err)
	}
	base.ID = "gateway-grant"
	base.Principal = "gateway-1"
	base.Kind = "device"
	base.PublicKey = devPub
	base.Scopes = []string{"task-status", "replicate"}
	base.TaskStatusAudience = "node-1"
	base.TaskStatusAssignee = "robot/nav"
	f.device, err = authority.SignGrant(base, root)
	if err != nil {
		t.Fatal(err)
	}
	f.verifier, err = authority.NewVerifier(authority.Config{RootPublicKey: rootPub, Tenant: "tenant", Site: "office", Domain: "simulation", Audience: "node-3", Now: func() time.Time { return f.now }, Replay: authority.NewMemoryReplay(1000)})
	if err != nil {
		t.Fatal(err)
	}
	f.revocations(1, nil)
	f.cfg = Config{Tenant: "tenant", Site: "office", Domain: "simulation", Routes: map[string]Route{"node-1": {Assignee: "robot/nav", StatusPrincipal: "gateway-1"}, "node-2": {Assignee: "other/nav", StatusPrincipal: "gateway-2"}}, Verifier: f.verifier, Now: func() time.Time { return f.now }}
	f.store, err = Open(f.path, f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.store.Close() })
	return f
}
func (f *fixture) revocations(revision uint64, revoked []string) {
	r, err := authority.SignRevocations(authority.Revocations{Version: authority.Version, Tenant: "tenant", Site: "office", Domain: "simulation", Revision: revision, IssuedAtUnixMS: f.now.Add(-time.Second).UnixMilli(), ExpiresAtUnixMS: f.now.Add(59 * time.Minute).UnixMilli(), Principals: revoked}, f.root)
	if err != nil {
		f.t.Fatal(err)
	}
	if err = f.verifier.UpdateRevocations(r); err != nil {
		f.t.Fatal(err)
	}
}
func (f *fixture) instruction(id, op string) authority.SignedInstruction {
	var payload []byte
	if op == "mission" {
		payload, _ = json.Marshal(tasking.Spec{ID: id, Type: "wendy.nav.go_to.v1", Site: "office", Assignee: "robot/nav", Domain: mission.Simulation, Class: mission.Move, Mode: mission.Autonomous, Frame: world.FrameRef{ID: "floor", Revision: "v1"}, NotAfterUnixMS: f.now.Add(30 * time.Minute).UnixMilli(), Area: tasking.Bounds{Min: [3]float64{-2, -2, -2}, Max: [3]float64{2, 2, 2}}, RequiredSensors: []string{"gas"}, Fallback: mission.Hold, Target: [3]float64{1, .2, 0}, SpeedMPS: .3, ToleranceM: .03, StopSpeedMPS: .01})
	} else {
		payload, _ = json.Marshal(map[string]string{"task_id": id})
	}
	i, err := authority.SignInstruction(f.operator, f.operatorKey, "node-1", op, payload, f.now, 40*time.Minute)
	if err != nil {
		f.t.Fatal(err)
	}
	return i
}
func (f *fixture) status(in authority.SignedInstruction, version uint64, phase mission.Phase) authority.SignedTaskStatus {
	var spec tasking.Spec
	if in.Operation == "mission" {
		if err := authority.DecodeStrict(in.Payload, &spec); err != nil {
			f.t.Fatal(err)
		}
	} else {
		var payload struct {
			TaskID string `json:"task_id"`
		}
		_ = json.Unmarshal(in.Payload, &payload)
		spec = tasking.Spec{ID: payload.TaskID, Site: "office", Assignee: "robot/nav", Domain: mission.Simulation}
	}
	proof, _ := json.Marshal(in)
	record := tasking.Record{Spec: spec, Status: mission.Status{Version: version, Phase: phase}, Proof: proof, AuthorizationUntilUnixMS: in.ExpiresAtUnixMS}
	return f.signRecord(record)
}
func (f *fixture) signRecord(record tasking.Record) authority.SignedTaskStatus {
	raw, err := json.Marshal(record)
	if err != nil {
		f.t.Fatal(err)
	}
	s, err := authority.SignTaskStatus(f.device, f.deviceKey, "node-1", "robot/nav", record.Spec.ID, record.Status.Version, record.Status.Phase.Terminal(), raw, f.now)
	if err != nil {
		f.t.Fatal(err)
	}
	return s
}
func (f *fixture) reopen() {
	f.store.Close()
	var err error
	f.store, err = Open(f.path, f.cfg)
	if err != nil {
		f.t.Fatal(err)
	}
}

func TestDurableQueueStopOrderingAndExpiry(t *testing.T) {
	f := newFixture(t)
	for _, id := range []string{"a-go", "b-stop"} {
		if _, err := f.store.PutInstruction(f.instruction(id, "mission")); err != nil {
			t.Fatal(err)
		}
	}
	stop := f.instruction("b-stop", "stop")
	if _, err := f.store.PutInstruction(stop); err != nil {
		t.Fatal(err)
	}
	f.reopen()
	pending, err := f.store.Pending("node-1")
	if err != nil || len(pending) != 2 || pending[0].Operation != "stop" || pending[1].Operation != "mission" {
		t.Fatalf("wrong pending order: %+v %v", pending, err)
	}
	// Mutating a returned proof must not alter retained authority.
	pending[0].Signature[0] ^= 1
	again, _ := f.store.Pending("node-1")
	if !same(again[0], stop) {
		t.Fatal("caller mutated stored proof")
	}
	f.now = f.now.Add(41 * time.Minute)
	pending, err = f.store.Pending("node-1")
	if err != nil || len(pending) != 0 {
		t.Fatalf("expired delivery escaped: %+v %v", pending, err)
	}
	exported, err := f.store.Export()
	if err != nil || len(exported) != 0 {
		t.Fatalf("expired proof exported: %+v %v", exported, err)
	}
	f.reopen()
	entries, err := f.store.List()
	if err != nil || len(entries) != 2 || entries[1].Stop == nil {
		t.Fatal("expiry erased durable history", err)
	}
}

func TestConflictsDoNotOverrideAssigneeState(t *testing.T) {
	f := newFixture(t)
	first := f.instruction("task", "mission")
	if _, err := f.store.PutInstruction(first); err != nil {
		t.Fatal(err)
	}
	var altered tasking.Spec
	_ = json.Unmarshal(first.Payload, &altered)
	altered.Target[0] = -.7
	raw, _ := json.Marshal(altered)
	second, err := authority.SignInstruction(f.operator, f.operatorKey, "node-1", "mission", raw, f.now, 40*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.store.PutInstruction(second); err == nil {
		t.Fatal("queued ID rebind accepted")
	}
	// The final assignee may have accepted the second valid request before this relay saw the first.
	authoritative := f.status(second, 1, mission.Accepted)
	if _, err = f.store.PutStatus(authoritative); err != nil {
		t.Fatal("local queue blocked assignee truth", err)
	}
	if _, err = f.store.PutInstruction(first); err == nil {
		t.Fatal("redelivery disagrees with authoritative task")
	}
	pending, _ := f.store.Pending("node-1")
	if len(pending) != 0 {
		t.Fatal("known task dispatched again")
	}
	exported, _ := f.store.Export()
	if len(exported) != 1 || exported[0].Mission != nil || exported[0].Status == nil {
		t.Fatal("conflicting queued mission exported")
	}
	if _, err = f.store.PutStatus(f.status(second, 3, mission.Succeeded)); err != nil {
		t.Fatal(err)
	}
	if _, err = f.store.PutStatus(f.status(second, 2, mission.Running)); err != nil {
		t.Fatal("older status should be ignored", err)
	}
	if _, err = f.store.PutStatus(f.status(second, 4, mission.Running)); err == nil {
		t.Fatal("terminal state changed")
	}
	if _, err = f.store.PutStatus(f.status(second, 3, mission.Failed)); err == nil {
		t.Fatal("same version equivocation accepted")
	}
	f.reopen()
	entries, _ := f.store.List()
	if entries[0].Status.StatusVersion != 3 || !entries[0].Status.Terminal {
		t.Fatal("terminal state not retained")
	}
}

func TestNewObservationProofPreservesImmutableVersion(t *testing.T) {
	f := newFixture(t)
	in := f.instruction("task", "mission")
	a := f.status(in, 1, mission.Accepted)
	if _, err := f.store.PutStatus(a); err != nil {
		t.Fatal(err)
	}
	f.now = f.now.Add(time.Second)
	b := f.status(in, 1, mission.Accepted)
	entry, err := f.store.PutStatus(b)
	if err != nil || entry.Status.SignedAtUnixMS != b.SignedAtUnixMS {
		t.Fatal("new origin observation lost", err)
	}
	entry, err = f.store.PutStatus(a)
	if err != nil || entry.Status.SignedAtUnixMS != b.SignedAtUnixMS {
		t.Fatal("observation time regressed", err)
	}
}

func TestStatusBindingStopShapeAndRejected(t *testing.T) {
	f := newFixture(t)
	in := f.instruction("task", "mission")
	valid := f.status(in, 1, mission.Accepted)
	for _, change := range []func(*tasking.Record){
		func(r *tasking.Record) { r.Spec.Target[0] = -.2 },
		func(r *tasking.Record) { r.Spec.Assignee = "other/nav" },
		func(r *tasking.Record) { r.AuthorizationUntilUnixMS++ },
		func(r *tasking.Record) { r.Status.Phase = "imaginary" },
		func(r *tasking.Record) { r.Result = &tasking.Inspection{} },
	} {
		var r tasking.Record
		_ = json.Unmarshal(valid.Record, &r)
		change(&r)
		raw, _ := json.Marshal(r)
		bad := clone(valid)
		bad.Record = raw
		bad.Signature = nil
		// Sign directly because malformed Inspection cannot survive the strict helper decoder.
		bad, err := authority.SignTaskStatus(f.device, f.deviceKey, "node-1", "robot/nav", "task", r.Status.Version, r.Status.Phase.Terminal(), raw, f.now)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = f.store.PutStatus(bad); err == nil {
			t.Fatal("malformed record accepted")
		}
	}
	stop := f.instruction("stopped", "stop")
	s := f.status(stop, 1, mission.Canceled)
	if _, err := f.store.PutStatus(s); err != nil {
		t.Fatal(err)
	}
	var r tasking.Record
	_ = json.Unmarshal(s.Record, &r)
	r.Spec.Target[0] = 1
	if _, err := f.store.PutStatus(f.signRecord(r)); err == nil {
		t.Fatal("stop placeholder smuggled parameters")
	}
	if _, err := f.store.PutStatus(f.status(in, 1, mission.Rejected)); err != nil {
		t.Fatal("assignee rejection not retained", err)
	}
	bad := clone(valid)
	bad.Grant.Grant.Principal = "untrusted"
	if _, err := f.store.PutStatus(bad); err == nil {
		t.Fatal("wrong status owner accepted")
	}
}

func TestAuthorityExpiryRevocationAndReopenHistory(t *testing.T) {
	f := newFixture(t)
	in := f.instruction("task", "mission")
	status := f.status(in, 2, mission.Succeeded)
	if _, err := f.store.PutStatus(status); err != nil {
		t.Fatal(err)
	}
	f.now = f.now.Add(2 * time.Hour)
	f.revocations(2, nil)
	f.reopen()
	out, err := f.store.Export()
	if err != nil || len(out) != 1 || out[0].Status == nil {
		t.Fatal("expired grant erased authenticated result", err)
	}
	f.revocations(3, []string{"gateway-1"})
	if _, err = f.store.PutStatus(status); err == nil {
		t.Fatal("revoked status imported")
	}
	f.reopen()
	out, err = f.store.Export()
	if err != nil || len(out) != 0 {
		t.Fatal("revoked status exported as current", err)
	}
	history, err := f.store.List()
	if err != nil || len(history) != 1 {
		t.Fatal("revocation destroyed history", err)
	}
}

func TestLockQuotasWriteFailureAndClockRollback(t *testing.T) {
	f := newFixture(t)
	if other, err := Open(f.path, f.cfg); err == nil {
		other.Close()
		t.Fatal("second writer acquired store")
	}
	f.store.cfg.MaxEntries = 1
	if _, err := f.store.PutInstruction(f.instruction("one", "mission")); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.PutInstruction(f.instruction("two", "mission")); err == nil {
		t.Fatal("quota ignored")
	}
	list, err := f.store.List()
	if err != nil || len(list) != 1 {
		t.Fatal("quota failure mutated state", err)
	}
	f.store.write = func(string, []byte) error { return errors.New("disk failure") }
	if _, err = f.store.PutInstruction(f.instruction("one", "stop")); err == nil {
		t.Fatal("failed write acknowledged")
	}
	if _, err = f.store.List(); err == nil {
		t.Fatal("faulted store still readable")
	}
	f.reopen()
	list, _ = f.store.List()
	if list[0].Stop != nil {
		t.Fatal("failed stop was committed")
	}
	f.now = f.now.Add(-time.Millisecond)
	if _, err = f.store.List(); err == nil {
		t.Fatal("clock rollback ignored")
	}
	if other, err := Open(f.path+"-changed", Config{}); err == nil {
		other.Close()
		t.Fatal("bad config accepted")
	}
}

func TestPersistedStatusSignatureRechecked(t *testing.T) {
	f := newFixture(t)
	if _, err := f.store.PutStatus(f.status(f.instruction("task", "mission"), 1, mission.Accepted)); err != nil {
		t.Fatal(err)
	}
	f.store.Close()
	b, err := os.ReadFile(f.path)
	if err != nil {
		t.Fatal(err)
	}
	var env envelope
	_ = json.Unmarshal(b, &env)
	var state state
	_ = json.Unmarshal(env.State, &state)
	state.Entries[0].Status.Signature[0] ^= 1
	env.State, _ = json.Marshal(state)
	sum := sha256.Sum256(env.State)
	env.SHA256 = hex.EncodeToString(sum[:])
	b, _ = json.Marshal(env)
	if err = os.WriteFile(f.path, b, 0600); err != nil {
		t.Fatal(err)
	}
	if reopened, err := Open(f.path, f.cfg); err == nil {
		reopened.Close()
		t.Fatal("forged persisted status accepted")
	}
}

func TestFreshStopCanReplaceRevokedProof(t *testing.T) {
	f := newFixture(t)
	old := f.instruction("task", "stop")
	if _, err := f.store.PutInstruction(old); err != nil {
		t.Fatal(err)
	}
	g := f.operator.Grant
	g.ID, g.Principal = "second-operator", "second-operator"
	newGrant, err := authority.SignGrant(g, f.root)
	if err != nil {
		t.Fatal(err)
	}
	f.revocations(2, []string{"operator"})
	fresh, err := authority.SignInstruction(newGrant, f.operatorKey, "node-1", "stop", old.Payload, f.now, 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	entry, err := f.store.PutInstruction(fresh)
	if err != nil || entry.Stop.Grant.Grant.Principal != "second-operator" {
		t.Fatal("old revoked proof blocked valid stop", err)
	}
	pending, err := f.store.Pending("node-1")
	if err != nil || len(pending) != 1 || !same(pending[0], fresh) {
		t.Fatal("fresh stop not deliverable", err)
	}
}

func TestStatusCannotPredateOriginalInstruction(t *testing.T) {
	f := newFixture(t)
	valid := f.status(f.instruction("task", "mission"), 1, mission.Accepted)
	early, err := authority.SignTaskStatus(f.device, f.deviceKey, "node-1", "robot/nav", "task", 1, false, valid.Record, f.now.Add(-time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.store.PutStatus(early); err == nil {
		t.Fatal("status predates its mission")
	}
}

func TestDestinationScopeAndDistinctTaskIDs(t *testing.T) {
	f := newFixture(t)
	one := f.instruction("same-id", "mission")
	if _, err := f.store.PutInstruction(one); err != nil {
		t.Fatal(err)
	}
	two, err := authority.SignInstruction(f.operator, f.operatorKey, "node-2", "mission", one.Payload, f.now, 40*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.store.PutInstruction(two); err == nil {
		t.Fatal("audience and assignee mismatch accepted")
	}
	var spec tasking.Spec
	_ = json.Unmarshal(one.Payload, &spec)
	spec.Assignee = "other/nav"
	raw, _ := json.Marshal(spec)
	two, err = authority.SignInstruction(f.operator, f.operatorKey, "node-2", "mission", raw, f.now, 40*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.store.PutInstruction(two); err != nil {
		t.Fatal("IDs collided across final audiences", err)
	}
	entries, _ := f.store.List()
	if len(entries) != 2 {
		t.Fatal("destination key lost")
	}
	bad := clone(one)
	bad.Signature[0] ^= 1
	if _, err = f.store.PutInstruction(bad); err == nil {
		t.Fatal("forged proof accepted")
	}
}

func TestStatusRetainsLargeOriginalInstruction(t *testing.T) {
	f := newFixture(t)
	first := f.instruction("large", "mission")
	var spec tasking.Spec
	_ = json.Unmarshal(first.Payload, &spec)
	// The record repeats the immutable spec and its signed proof. Both must fit
	// even when each individual instruction is within its smaller ingress cap.
	spec.RequiredSensors = []string{""}
	base, _ := json.Marshal(spec)
	spec.RequiredSensors = []string{strings.Repeat("x", authority.MaxEnvelopeBytes/2-len(base)-1)}
	raw, _ := json.Marshal(spec)
	in, err := authority.SignInstruction(f.operator, f.operatorKey, "node-1", "mission", raw, f.now, 40*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	status := f.status(in, 1, mission.Rejected)
	if len(status.Record) <= 64<<10 {
		t.Fatal("fixture did not exceed old record cap")
	}
	if _, err = f.store.PutStatus(status); err != nil {
		t.Fatal("valid original proof cannot be retained", err)
	}
	f.reopen()
	exported, err := f.store.Export()
	if err != nil || len(exported) != 1 || !same(exported[0].Status.Record, status.Record) {
		t.Fatal("large proof did not survive repair", err)
	}
}
