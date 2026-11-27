package taskhistory

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/mission"
	"github.com/wendylabsinc/wendy/go/world/internal/tasking"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type rig struct {
	now                 time.Time
	root, key, operator ed25519.PrivateKey
	grant, op           authority.SignedGrant
	v                   *authority.Verifier
	s                   *Store
	cfg                 Config
	path                string
	last                string
	seq                 uint64
}

func newRig(t *testing.T) *rig {
	t.Helper()
	r := &rig{now: time.Now().Truncate(time.Millisecond), last: zero()}
	_, r.root, _ = ed25519.GenerateKey(rand.Reader)
	pub, key, _ := ed25519.GenerateKey(rand.Reader)
	r.key = key
	g := authority.Grant{Version: 1, ID: "origin", Principal: "gateway-a", Kind: "device", Tenant: "tenant", Site: "office", Domain: "simulation", Source: world.Source{Device: "gateway-a", App: "world", Service: "tasks", Stream: "audit"}, PublicKey: pub, Scopes: []string{"task-status", "replicate", "read"}, TaskStatusAudience: "gateway-a", TaskStatusAssignee: "robot/navigation", NotBeforeUnixMS: r.now.Add(-time.Minute).UnixMilli(), NotAfterUnixMS: r.now.Add(time.Minute).UnixMilli()}
	var e error
	r.grant, e = authority.SignGrant(g, r.root)
	if e != nil {
		t.Fatal(e)
	}
	pub, r.operator, _ = ed25519.GenerateKey(rand.Reader)
	g.ID = "operator"
	g.Principal = "operator"
	g.Kind = "operator"
	g.PublicKey = pub
	g.Scopes = []string{"mission", "stop"}
	g.TaskStatusAudience = ""
	g.TaskStatusAssignee = ""
	r.op, e = authority.SignGrant(g, r.root)
	if e != nil {
		t.Fatal(e)
	}
	r.v, e = authority.NewVerifier(authority.Config{RootPublicKey: r.root.Public().(ed25519.PublicKey), Tenant: "tenant", Site: "office", Domain: "simulation", Audience: "reader", Now: func() time.Time { return r.now }, Replay: authority.NewMemoryReplay(100)})
	if e != nil {
		t.Fatal(e)
	}
	r.revoke(t, nil, 1)
	r.cfg = Config{Tenant: "tenant", Site: "office", Origins: map[string]Origin{"gateway-a": {"gateway-a", "robot/navigation", "run"}}, Verifier: r.v, Now: func() time.Time { return r.now }}
	r.path = filepath.Join(t.TempDir(), "history")
	r.s, e = Open(r.path, r.cfg)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { r.s.Close() })
	return r
}
func (r *rig) revoke(t *testing.T, ids []string, revision uint64) {
	t.Helper()
	rev, e := authority.SignRevocations(authority.Revocations{Version: 1, Tenant: "tenant", Site: "office", Domain: "simulation", Revision: revision, IssuedAtUnixMS: r.now.Add(-time.Millisecond).UnixMilli(), ExpiresAtUnixMS: r.now.Add(30 * time.Minute).UnixMilli(), GrantIDs: ids}, r.root)
	if e != nil {
		t.Fatal(e)
	}
	if e = r.v.UpdateRevocations(rev); e != nil {
		t.Fatal(e)
	}
}
func (r *rig) receipt(t *testing.T, id string) Receipt {
	t.Helper()
	r.seq++
	payload, _ := json.Marshal(map[string]string{"id": id, "site": "office", "assignee": "robot/navigation"})
	proof, e := authority.SignInstruction(r.op, r.operator, "gateway-a", "mission", payload, r.now.Add(-time.Millisecond), 30*time.Second)
	if e != nil {
		t.Fatal(e)
	}
	raw, _ := json.Marshal(proof)
	a := tasking.Audit{Sequence: r.seq, UnixMS: r.now.UnixMilli(), TaskID: id, Principal: "operator", Event: "accepted", Status: mission.Status{Version: 1, Phase: mission.Accepted}, Proof: raw}
	b, _ := json.Marshal(a)
	s, e := authority.SignTaskAudit(Receipt{Grant: r.grant, Audience: "gateway-a", Assignee: "robot/navigation", RunID: "run", Incarnation: strings.Repeat("a", 64), Sequence: r.seq, PreviousSHA256: r.last, Event: b, ExportedUnixMS: r.now.UnixMilli()}, r.key)
	if e != nil {
		t.Fatal(e)
	}
	r.last = tasking.AuditEventHash(r.last, a)
	return s
}
func page(rs ...Receipt) Page {
	first, last := rs[0], rs[len(rs)-1]
	return Page{Origin: first.Audience, RunID: first.RunID, Incarnation: first.Incarnation, After: first.Sequence - 1, Through: last.Sequence, PreviousSHA256: first.PreviousSHA256, Records: rs}
}
func TestHistoryScopeProofPrefixCursorAndNoTaskState(t *testing.T) {
	r := newRig(t)
	one, two := r.receipt(t, "one"), r.receipt(t, "two")
	p := page(one, two)
	if e := r.s.Import(context.Background(), p, "relay"); e != nil {
		t.Fatal(e)
	}
	if e := r.s.Import(context.Background(), p, "relay"); e != nil {
		t.Fatal("exact duplicate", e)
	}
	list, e := r.s.List(context.Background(), "gateway-a", "", 1, "")
	if e != nil || len(list.Events) != 1 || list.Next == "" || list.Events[0].Executable {
		t.Fatal(list, e)
	}
	next, e := r.s.List(context.Background(), "gateway-a", "", 1, list.Next)
	if e != nil || len(next.Events) != 1 || next.Events[0].Event.TaskID != "two" {
		t.Fatal(next, e)
	}
	if _, e = r.s.List(context.Background(), "gateway-a", "two", 1, list.Next); e == nil {
		t.Fatal("cursor crossed task scope")
	}
	for _, mutate := range []func(*Page){func(p *Page) { p.RunID = "other" }, func(p *Page) { p.Incarnation = strings.Repeat("b", 64) }, func(p *Page) { p.PreviousSHA256 = strings.Repeat("f", 64) }, func(p *Page) { p.Records[0].Signature[0] ^= 1 }, func(p *Page) { p.Records[0].ExportedUnixMS-- }, func(p *Page) { p.Records[0].Event = json.RawMessage(`{"sequence":1}`) }} {
		changed := clone(p)
		mutate(&changed)
		if e = r.s.Import(context.Background(), changed, "relay"); e == nil {
			t.Fatal("tampering accepted")
		}
	}
	r.s.Close()
	r.s, e = Open(r.path, r.cfg)
	if e != nil {
		t.Fatal(e)
	}
	cursor, e := r.s.Cursor("gateway-a")
	if e != nil || cursor.After != 2 || cursor.PreviousSHA256 != r.last {
		t.Fatal(cursor, e)
	}
	changed := r.cfg
	changed.Origins = map[string]Origin{"gateway-a": {"other", "robot/navigation", "run"}}
	r.s.Close()
	if _, e = Open(r.path, changed); e == nil {
		t.Fatal("origin changed on reopen")
	}
}
func TestHistoryExpiryRevocationRetainsExplicitHistoricalReads(t *testing.T) {
	r := newRig(t)
	one := r.receipt(t, "one")
	if e := r.s.Import(context.Background(), page(one), "relay"); e != nil {
		t.Fatal(e)
	}
	r.now = r.now.Add(2 * time.Minute)
	v, e := r.s.List(context.Background(), "gateway-a", "one", 20, "")
	if e != nil || v.Events[0].CurrentVerification != "origin_grant_expired" || !v.Events[0].HistoricalProvenanceVerified {
		t.Fatal(v, e)
	}
	if e = r.s.Import(context.Background(), page(one), "relay"); e == nil {
		t.Fatal("new expired import")
	}
	q, _ := r.s.Cursor("gateway-a")
	if _, e = r.s.Export(context.Background(), q); e == nil {
		t.Fatal("expired empty export")
	}
	r.s.Close()
	r.s, e = Open(r.path, r.cfg)
	if e != nil {
		t.Fatal("history reopened after expiry", e)
	}
	r2 := newRig(t)
	row := r2.receipt(t, "one")
	if e = r2.s.Import(context.Background(), page(row), "relay"); e != nil {
		t.Fatal(e)
	}
	r2.revoke(t, []string{"origin"}, 2)
	v, e = r2.s.List(context.Background(), "gateway-a", "one", 20, "")
	if e != nil || v.Events[0].CurrentVerification != "not_currently_verified" {
		t.Fatal(v, e)
	}
	if e = r2.s.Import(context.Background(), page(row), "relay"); e == nil {
		t.Fatal("revoked import")
	}
}
func TestCapacityFailureDoesNotMoveCursorOrDelete(t *testing.T) {
	r := newRig(t)
	r.s.cfg.MaxEntries = 1
	one, two := r.receipt(t, "one"), r.receipt(t, "two")
	if e := r.s.Import(context.Background(), page(one), "relay"); e != nil {
		t.Fatal(e)
	}
	before, _ := os.ReadFile(r.path)
	if e := r.s.Import(context.Background(), page(two), "relay"); e != ErrCapacity {
		t.Fatal(e)
	}
	after, _ := os.ReadFile(r.path)
	if !bytesEqual(before, after) {
		t.Fatal("quota changed durable bytes")
	}
	cursor, _ := r.s.Cursor("gateway-a")
	if cursor.After != 1 {
		t.Fatal(cursor)
	}
}
func bytesEqual(a, b []byte) bool { return string(a) == string(b) }

func TestImportRechecksCurrentAuthorityBeforeCommit(t *testing.T) {
	r := newRig(t)
	one, two := r.receipt(t, "one"), r.receipt(t, "two")
	before, _ := os.ReadFile(r.path)
	calls := 0
	r.s.cfg.Now = func() time.Time {
		calls++
		n := r.now
		if calls == 1 {
			r.now = time.UnixMilli(one.Grant.Grant.NotAfterUnixMS)
		}
		return n
	}
	if e := r.s.Import(context.Background(), page(one, two), "relay"); e == nil {
		t.Fatal("expired page committed")
	}
	after, _ := os.ReadFile(r.path)
	if string(before) != string(after) {
		t.Fatal("failed import changed durable state")
	}
	q, _ := r.s.Cursor("gateway-a")
	if q.After != 0 {
		t.Fatal(q)
	}
}
func TestEmptyImportValidatesCursorWithoutPinningIdentity(t *testing.T) {
	r := newRig(t)
	p := Page{Origin: "gateway-a", RunID: "run", Incarnation: strings.Repeat("a", 64), PreviousSHA256: zero()}
	if e := r.s.Import(context.Background(), p, "relay"); e != nil {
		t.Fatal(e)
	}
	q, _ := r.s.Cursor("gateway-a")
	if q.Incarnation != "" {
		t.Fatal("unsigned empty page pinned identity")
	}
	p.After = 1
	p.Through = 1
	if e := r.s.Import(context.Background(), p, "relay"); e == nil {
		t.Fatal("future empty cursor accepted")
	}
	p.After = 0
	p.Through = 0
	p.PreviousSHA256 = strings.Repeat("b", 64)
	if e := r.s.Import(context.Background(), p, "relay"); e == nil {
		t.Fatal("wrong empty prefix accepted")
	}
}

func TestOriginCannotReattestChangedInstructionOrCrossScope(t *testing.T) {
	r := newRig(t)
	one := r.receipt(t, "one")
	mutations := []func(*Receipt){
		func(x *Receipt) { x.RunID = "other" },
		func(x *Receipt) { a := event(*x); a.TaskID = "changed-task"; x.Event, _ = json.Marshal(a) },
		func(x *Receipt) {
			a := event(*x)
			var instruction authority.SignedInstruction
			json.Unmarshal(a.Proof, &instruction)
			instruction.Payload = json.RawMessage(`{"id":"one","site":"other","assignee":"robot/navigation"}`)
			a.Proof, _ = json.Marshal(instruction)
			x.Event, _ = json.Marshal(a)
		},
		func(x *Receipt) {
			a := event(*x)
			var instruction authority.SignedInstruction
			json.Unmarshal(a.Proof, &instruction)
			instruction.Signature[0] ^= 1
			a.Proof, _ = json.Marshal(instruction)
			x.Event, _ = json.Marshal(a)
		},
		func(x *Receipt) { a := event(*x); a.Principal = "another-operator"; x.Event, _ = json.Marshal(a) },
	}
	for _, mutate := range mutations {
		x := clone(one)
		mutate(&x)
		signed, e := authority.SignTaskAudit(x, r.key)
		if e != nil {
			t.Fatal(e)
		}
		if e = r.s.Import(context.Background(), page(signed), "relay"); e == nil {
			t.Fatal("origin reattestation bypassed original instruction/scope")
		}
	}
	q, _ := r.s.Cursor("gateway-a")
	if q.After != 0 {
		t.Fatal(q)
	}
	if e := r.s.Import(context.Background(), page(one), "relay"); e != nil {
		t.Fatal(e)
	}
	r.now = r.now.Add(time.Millisecond)
	again := clone(one)
	again.ExportedUnixMS = r.now.UnixMilli()
	again, e := authority.SignTaskAudit(again, r.key)
	if e != nil {
		t.Fatal(e)
	}
	if e = r.s.Import(context.Background(), page(again), "other-relay"); e != nil {
		t.Fatal(e)
	}
	list, _ := r.s.List(context.Background(), "gateway-a", "", 20, "")
	if list.Events[0].Receipt.ExportedUnixMS != one.ExportedUnixMS {
		t.Fatal("duplicate replaced original receipt")
	}
}

type expiryReplay struct {
	authority.ReplayStore
	checks int
	after  func(int)
}

func (r *expiryReplay) Checkpoint(revision uint64, digest string, now time.Time) error {
	if e := r.ReplayStore.Checkpoint(revision, digest, now); e != nil {
		return e
	}
	r.checks++
	if r.after != nil {
		r.after(r.checks)
	}
	return nil
}
func TestMixedGrantPageExpiresEarlyRowDuringLaterVerification(t *testing.T) {
	r := newRig(t)
	one, two := r.receipt(t, "short"), r.receipt(t, "long")
	short := one.Grant.Grant
	short.ID = "short-origin"
	short.NotAfterUnixMS = r.now.Add(time.Second).UnixMilli()
	var e error
	one.Grant, e = authority.SignGrant(short, r.root)
	if e != nil {
		t.Fatal(e)
	}
	one, e = authority.SignTaskAudit(one, r.key)
	if e != nil {
		t.Fatal(e)
	}
	replay := &expiryReplay{ReplayStore: authority.NewMemoryReplay(100)}
	r.v, e = authority.NewVerifier(authority.Config{RootPublicKey: r.root.Public().(ed25519.PublicKey), Tenant: "tenant", Site: "office", Domain: "simulation", Audience: "reader", Now: func() time.Time { return r.now }, Replay: replay})
	if e != nil {
		t.Fatal(e)
	}
	r.revoke(t, nil, 1)
	replay.checks = 0
	r.s.cfg.Verifier = r.v
	// Two initial checks, then the final page's two current checks. Advancing
	// after the later row's durable checkpoint models time spent verifying it.
	replay.after = func(n int) {
		if n == 4 {
			r.now = r.now.Add(2 * time.Second)
		}
	}
	before, _ := os.ReadFile(r.path)
	if e = r.s.Import(context.Background(), page(one, two), "relay"); e == nil {
		t.Fatal("early expired row committed while later row remained valid")
	}
	if replay.checks != 4 {
		t.Fatalf("did not reach later final verification: %d", replay.checks)
	}
	replay.after = nil
	if _, e = r.v.VerifyTaskAudit(context.Background(), two, true); e != nil {
		t.Fatal("later row should remain valid", e)
	}
	if _, e = r.v.VerifyTaskAudit(context.Background(), one, true); e == nil {
		t.Fatal("early row should be expired")
	}
	after, _ := os.ReadFile(r.path)
	if string(before) != string(after) {
		t.Fatal("mixed-deadline failure changed durable state")
	}
	q, _ := r.s.Cursor("gateway-a")
	if q.After != 0 {
		t.Fatal(q)
	}
}
