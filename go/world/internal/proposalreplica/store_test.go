package proposalreplica

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/mission"
	"github.com/wendylabsinc/wendy/go/world/internal/proposals"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

type rig struct {
	now                       time.Time
	root, agentKey, originKey ed25519.PrivateKey
	agent, origin             authority.SignedGrant
	v                         *authority.Verifier
	cfg                       Config
	s                         *Store
	path                      string
	records                   *proposals.Store
}

func newRig(t *testing.T) *rig {
	t.Helper()
	r := &rig{now: time.Date(2026, 11, 25, 0, 0, 0, 0, time.UTC), path: filepath.Join(t.TempDir(), "inbox")}
	_, r.root, _ = ed25519.GenerateKey(rand.Reader)
	r.agent, r.agentKey = r.grant(t, "planner", "agent", "propose")
	r.origin, r.originKey = r.grant(t, "gateway-a", "device", "replicate")
	var err error
	r.v, err = authority.NewVerifier(authority.Config{RootPublicKey: r.root.Public().(ed25519.PublicKey), Tenant: "tenant", Site: "office", Domain: "simulation", Audience: "gateway-b", Now: func() time.Time { return r.now }, Replay: authority.NewMemoryReplay(10000)})
	if err != nil {
		t.Fatal(err)
	}
	r.revoke(t, nil, 1)
	o := Origin{Principal: "gateway-a", Assignee: "robot/navigation", RunID: "run", Catalog: map[string]mission.SafetyClass{"wendy.nav.go_to.v1": mission.Move}}
	r.cfg = Config{Tenant: "tenant", Site: "office", Origins: map[string]Origin{"gateway-a": o}, Verifier: r.v, Now: func() time.Time { return r.now }}
	r.s, err = Open(r.path, r.cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.s.Close() })
	r.records, err = proposals.Open(filepath.Join(t.TempDir(), "origin"), proposals.Config{Tenant: "tenant", Site: "office", RunID: "run", Audience: "gateway-a", Assignee: o.Assignee, Catalog: o.Catalog, Now: func() time.Time { return r.now }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.records.Close() })
	return r
}
func (r *rig) grant(t *testing.T, principal, kind, scope string) (authority.SignedGrant, ed25519.PrivateKey) {
	t.Helper()
	pub, key, _ := ed25519.GenerateKey(rand.Reader)
	g, e := authority.SignGrant(authority.Grant{Version: 1, ID: principal + "-grant", Tenant: "tenant", Site: "office", Domain: "simulation", Principal: principal, Kind: kind, Source: world.Source{Device: principal, App: "world", Service: principal, Stream: "proposals"}, PublicKey: pub, Scopes: []string{scope}, NotBeforeUnixMS: r.now.Add(-time.Minute).UnixMilli(), NotAfterUnixMS: r.now.Add(time.Hour).UnixMilli()}, r.root)
	if e != nil {
		t.Fatal(e)
	}
	return g, key
}
func (r *rig) revoke(t *testing.T, ids []string, revision uint64) {
	t.Helper()
	rev, e := authority.SignRevocations(authority.Revocations{Version: 1, Tenant: "tenant", Site: "office", Domain: "simulation", Revision: revision, IssuedAtUnixMS: r.now.Add(-time.Second).UnixMilli(), ExpiresAtUnixMS: r.now.Add(30 * time.Minute).UnixMilli(), GrantIDs: ids}, r.root)
	if e != nil {
		t.Fatal(e)
	}
	if e = r.v.UpdateRevocations(rev); e != nil {
		t.Fatal(e)
	}
}
func (r *rig) receipt(t *testing.T, id string) Receipt {
	t.Helper()
	q := proposals.Request{Version: 1, ID: id, Tenant: "tenant", Site: "office", RunID: "run", Assignee: "robot/navigation", Type: "wendy.nav.go_to.v1", Parameters: json.RawMessage(`{"target":[1,0,0]}`), Summary: "Review an inspection", NotAfterUnixMS: r.now.Add(10 * time.Minute).UnixMilli()}
	b, _ := json.Marshal(q)
	p, e := authority.SignRequest(r.agent, r.agentKey, "gateway-a", "propose", b, "original-proposal-nonce", r.now, time.Minute)
	if e != nil {
		t.Fatal(e)
	}
	record, _, e := r.records.Submit(context.Background(), b, p, func() error { return nil })
	if e != nil {
		t.Fatal(e)
	}
	body, _ := ReceiptBody("gateway-a", record)
	proof, e := authority.SignRequest(r.origin, r.originKey, "gateway-a", "replicate", body, "origin-receipt-nonce", r.now, time.Minute)
	if e != nil {
		t.Fatal(e)
	}
	return Receipt{record, proof}
}
func page(rs ...Receipt) Page {
	after := uint64(0)
	if len(rs) > 0 {
		after = rs[0].Record.Sequence - 1
	}
	return Page{Origin: "gateway-a", After: after, Through: after + uint64(len(rs)), Records: rs}
}
func TestPartitionRestartHistoryAndImmutableDuplicates(t *testing.T) {
	r := newRig(t)
	first := r.receipt(t, "inspection")
	p := page(first)
	ctx := context.Background()
	if e := r.s.Import(ctx, p, "gateway-a"); e != nil {
		t.Fatal(e)
	}
	v, e := r.s.Get(ctx, "gateway-a", "inspection")
	if e != nil || v.Executable || v.Expired || v.CurrentVerification != "current_grants_verified" {
		t.Fatal(v, e)
	}
	r.now = r.now.Add(2 * time.Minute) // Original request and receipt transport windows are now expired.
	if e = r.s.Import(ctx, p, "gateway-b"); e != nil {
		t.Fatal(e)
	}
	_ = r.s.Close()
	r.s, e = Open(r.path, r.cfg)
	if e != nil {
		t.Fatal(e)
	}
	got, e := r.s.Get(ctx, "gateway-a", "inspection")
	if e != nil || !reflect.DeepEqual(got.Record, first.Record) || !reflect.DeepEqual(got.OriginProof, first.OriginProof) || got.Progress.Through != 1 || got.Progress.Via != "gateway-b" {
		t.Fatal(got, e)
	}
	exported, e := r.s.Export(ctx, "gateway-a", 0)
	if e != nil || !reflect.DeepEqual(exported.Records[0], first) {
		t.Fatal(exported, e)
	}
	r.now = r.now.Add(9 * time.Minute)
	got, e = r.s.Get(ctx, "gateway-a", "inspection")
	if e != nil || !got.Expired || got.Executable {
		t.Fatal(got, e)
	}
	if got.Record.Proposal.NotAfterUnixMS != first.Record.Proposal.NotAfterUnixMS {
		t.Fatal("relay renewed expiry")
	}
}
func TestTamperingAndScopeCannotAdvanceCursor(t *testing.T) {
	mutations := map[string]func(*Page){"origin": func(p *Page) { p.Origin = "other" }, "run": func(p *Page) { p.Records[0].Record.Proposal.RunID = "other" }, "site": func(p *Page) { p.Records[0].Record.Proposal.Site = "other" }, "body": func(p *Page) { p.Records[0].Record.Body = append(p.Records[0].Record.Body, ' ') }, "sequence": func(p *Page) { p.Records[0].Record.Sequence++ }, "acceptance time": func(p *Page) { p.Records[0].Record.AcceptedAtUnixMS++ }, "origin signature": func(p *Page) { p.Records[0].OriginProof.Signature[0] ^= 1 }, "proposer signature": func(p *Page) { p.Records[0].Record.Proof.Signature[0] ^= 1 }, "audience": func(p *Page) { p.Records[0].OriginProof.Audience = "other" }, "cursor gap": func(p *Page) { p.After = 1; p.Through = 2 }, "false through": func(p *Page) { p.Through++ }, "empty continuation": func(p *Page) { p.Records = nil; p.Through = 0; p.More = true }}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			p := page(r.receipt(t, "one"))
			before, _ := os.ReadFile(r.path)
			mutate(&p)
			if e := r.s.Import(context.Background(), p, "relay"); e == nil {
				t.Fatal("tampered page accepted")
			}
			after, _ := os.ReadFile(r.path)
			if !reflect.DeepEqual(before, after) {
				t.Fatal("invalid page changed records/cursor")
			}
		})
	}
}
func TestQuotaAndPersistenceFailureAreAtomic(t *testing.T) {
	for _, mode := range []string{"quota", "before rename", "after rename"} {
		t.Run(mode, func(t *testing.T) {
			r := newRig(t)
			one, two := r.receipt(t, "one"), r.receipt(t, "two")
			if e := r.s.Import(context.Background(), page(one), "relay"); e != nil {
				t.Fatal(e)
			}
			before, _ := os.ReadFile(r.path)
			if mode == "quota" {
				r.s.cfg.MaxEntries = 1
			} else {
				r.s.write = func(path string, b []byte) error {
					if mode == "after rename" {
						if e := writeAtomic(path, b); e != nil {
							return e
						}
					}
					return errors.New("lost process acknowledgement")
				}
			}
			if e := r.s.Import(context.Background(), page(two), "relay"); e == nil {
				t.Fatal("failure not surfaced")
			}
			after, _ := os.ReadFile(r.path)
			if mode != "after rename" && !reflect.DeepEqual(before, after) {
				t.Fatal("failed page erased previous state")
			}
			r.s.Close()
			var e error
			r.s, e = Open(r.path, r.cfg)
			if e != nil {
				t.Fatal(e)
			}
			cursor, e := r.s.Cursor("gateway-a")
			want := uint64(1)
			if mode == "after rename" {
				want = 2
			}
			if e != nil || cursor != want {
				t.Fatal(cursor, e)
			}
			if _, e = r.s.Get(context.Background(), "gateway-a", "one"); e != nil {
				t.Fatal(e)
			}
		})
	}
}
func TestCurrentVerificationDoesNotRewriteHistoricalProof(t *testing.T) {
	for _, kind := range []string{"revoked origin", "revoked proposer", "expired grants", "stale revocations"} {
		t.Run(kind, func(t *testing.T) {
			r := newRig(t)
			p := page(r.receipt(t, "one"))
			if e := r.s.Import(context.Background(), p, "relay"); e != nil {
				t.Fatal(e)
			}
			switch kind {
			case "revoked origin":
				r.revoke(t, []string{r.origin.Grant.ID}, 2)
			case "revoked proposer":
				r.revoke(t, []string{r.agent.Grant.ID}, 2)
			case "expired grants":
				r.now = r.now.Add(61 * time.Minute)
				r.revoke(t, nil, 2)
			case "stale revocations":
				r.now = r.now.Add(31 * time.Minute)
			}
			v, e := r.s.Get(context.Background(), "gateway-a", "one")
			if e != nil || !v.HistoricalProvenanceVerified || v.CurrentVerification != "not_currently_verified" || v.Executable || !reflect.DeepEqual(v.Record, p.Records[0].Record) {
				t.Fatal(v, e)
			}
			if kind != "revoked proposer" {
				if e = r.s.Import(context.Background(), p, "relay"); e == nil {
					t.Fatal("unverifiable origin imported")
				}
			}
			r.s.Close()
			r.s, e = Open(r.path, r.cfg)
			if e != nil {
				t.Fatal("historical records must remain readable", e)
			}
		})
	}
}
func TestOriginIdentityAndPurposeArePinned(t *testing.T) {
	r := newRig(t)
	p := page(r.receipt(t, "one"))
	b, _ := json.Marshal(p.Records[0].Record)
	bad, e := authority.SignRequest(r.origin, r.originKey, "gateway-a", "replicate", b, "not-history-envelope", r.now, time.Minute)
	if e != nil {
		t.Fatal(e)
	}
	p.Records[0].OriginProof = bad
	if e = r.s.Import(context.Background(), p, "relay"); e == nil {
		t.Fatal("unbound receipt purpose accepted")
	}
	p = page(r.receipt(t, "one"))
	other, key := r.grant(t, "other-origin", "device", "replicate")
	b, _ = ReceiptBody("gateway-a", p.Records[0].Record)
	p.Records[0].OriginProof, e = authority.SignRequest(other, key, "gateway-a", "replicate", b, "other-origin-receipt", r.now, time.Minute)
	if e != nil {
		t.Fatal(e)
	}
	if e = r.s.Import(context.Background(), p, "relay"); e == nil {
		t.Fatal("wrong origin principal accepted")
	}
}
func TestReorderConflictsAndOriginQualifiedIDs(t *testing.T) {
	r := newRig(t)
	one, two := r.receipt(t, "one"), r.receipt(t, "two")
	ctx := context.Background()
	if e := r.s.Import(ctx, page(two), "relay"); e == nil {
		t.Fatal("gap accepted")
	}
	if e := r.s.Import(ctx, page(one, two), "relay"); e != nil {
		t.Fatal(e)
	}
	if e := r.s.Import(ctx, page(one), "relay"); e != nil {
		t.Fatal("old exact prefix refused", e)
	}
	p := page(one)
	p.Records[0].Record.Proposal.Summary = "changed"
	p.Records[0].Record.Body, _ = json.Marshal(p.Records[0].Record.Proposal)
	p.Records[0].Record.BodySHA256 = hash(p.Records[0].Record.Body)
	p.Records[0].Record.Proof, _ = authority.SignRequest(r.agent, r.agentKey, "gateway-a", "propose", p.Records[0].Record.Body, "changed-proposal-nonce", r.now, time.Minute)
	b, _ := ReceiptBody("gateway-a", p.Records[0].Record)
	p.Records[0].OriginProof, _ = authority.SignRequest(r.origin, r.originKey, "gateway-a", "replicate", b, "changed-origin-receipt", r.now, time.Minute)
	if e := r.s.Import(ctx, p, "relay"); !errors.Is(e, ErrConflict) {
		t.Fatal("validly signed changed history replaced immutable row", e)
	}
	// A second explicitly configured origin can use the same proposal ID.
	r.s.Close()
	r.cfg.Origins["gateway-d"] = r.cfg.Origins["gateway-a"]
	r.path = filepath.Join(t.TempDir(), "multi")
	var e error
	r.s, e = Open(r.path, r.cfg)
	if e != nil {
		t.Fatal(e)
	}
	if e = r.s.Import(ctx, page(one), "relay"); e != nil {
		t.Fatal(e)
	}
	other := clone(one)
	other.Record.Proof, _ = authority.SignRequest(r.agent, r.agentKey, "gateway-d", "propose", other.Record.Body, "second-audience-nonce", r.now, time.Minute)
	b, _ = ReceiptBody("gateway-d", other.Record)
	other.OriginProof, _ = authority.SignRequest(r.origin, r.originKey, "gateway-d", "replicate", b, "second-origin-receipt", r.now, time.Minute)
	p = page(other)
	p.Origin = "gateway-d"
	if e = r.s.Import(ctx, p, "relay"); e != nil {
		t.Fatal(e)
	}
	for _, origin := range []string{"gateway-a", "gateway-d"} {
		if _, e = r.s.Get(ctx, origin, "one"); e != nil {
			t.Fatal(e)
		}
	}
}
func TestConfiguredScopeCannotChangeOnReopen(t *testing.T) {
	r := newRig(t)
	r.s.Close()
	r.cfg.Origins["gateway-a"] = Origin{Principal: "substituted", Assignee: "robot/navigation", RunID: "run", Catalog: map[string]mission.SafetyClass{"wendy.nav.go_to.v1": mission.Move}}
	if s, e := Open(r.path, r.cfg); e == nil {
		s.Close()
		t.Fatal("changed origin policy admitted")
	}
}

func TestOriginKeyRotationDoesNotRewriteEarlierReceipts(t *testing.T) {
	r := newRig(t)
	first := r.receipt(t, "one")
	if e := r.s.Import(context.Background(), page(first), "relay"); e != nil {
		t.Fatal(e)
	}
	oldGrant := r.origin.Grant.ID
	r.origin, r.originKey = r.grant(t, "gateway-a", "device", "replicate")
	r.origin.Grant.ID = "rotated-origin-grant"
	var e error
	r.origin, e = authority.SignGrant(r.origin.Grant, r.root)
	if e != nil {
		t.Fatal(e)
	}
	second := r.receipt(t, "two")
	r.revoke(t, []string{oldGrant}, 2)
	if e = r.s.Import(context.Background(), page(second), "relay"); e != nil {
		t.Fatal("pinned principal rotation refused", e)
	}
	before, e := r.s.Get(context.Background(), "gateway-a", "one")
	if e != nil || before.OriginVerification != "not_currently_verified" || !reflect.DeepEqual(before.OriginProof, first.OriginProof) {
		t.Fatal(before, e)
	}
	current, e := r.s.Get(context.Background(), "gateway-a", "two")
	if e != nil || current.OriginVerification != "current_grant_verified" {
		t.Fatal(current, e)
	}
}
func TestHistoricalVerificationCannotBecomeLiveAdmission(t *testing.T) {
	r := newRig(t)
	first := r.receipt(t, "one")
	for _, operation := range []string{"mission", "stop", "publish", "execute"} {
		if _, e := r.v.VerifyProposalHistory(context.Background(), first.Record.Proof, operation, "gateway-a", first.Record.Body); e == nil {
			t.Fatal("historical helper admitted operation", operation)
		}
	}
	r.now = r.now.Add(2 * time.Minute)
	for i := 0; i < 2; i++ {
		if _, e := r.v.VerifyProposalHistory(context.Background(), first.Record.Proof, "propose", "gateway-a", first.Record.Body); e != nil {
			t.Fatal(e)
		}
	}
	// Expired transport proofs remain unusable as live requests even though their
	// exact historical signatures have just been verified twice.
	if _, e := r.v.Verify(first.Record.Proof, "propose", first.Record.Body); e == nil {
		t.Fatal("historical verification admitted a live request")
	}
}

func TestExportedReceiptCannotMutateStoredHistory(t *testing.T) {
	r := newRig(t)
	first := r.receipt(t, "one")
	ctx := context.Background()
	if e := r.s.Import(ctx, page(first), "relay"); e != nil {
		t.Fatal(e)
	}
	exported, e := r.s.Export(ctx, "gateway-a", 0)
	if e != nil {
		t.Fatal(e)
	}
	exported.Records[0].Record.Body[0] = 'x'
	exported.Records[0].OriginProof.Signature[0] ^= 1
	exported.Records[0].Record.Proof.Grant.Grant.Scopes[0] = "mission"
	got, e := r.s.Get(ctx, "gateway-a", "one")
	if e != nil || !reflect.DeepEqual(got.Record, first.Record) || !reflect.DeepEqual(got.OriginProof, first.OriginProof) {
		t.Fatal("caller mutated retained history", e)
	}
}
