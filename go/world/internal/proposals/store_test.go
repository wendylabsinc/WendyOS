package proposals

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/mission"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"golang.org/x/sys/unix"
)

type proposalRig struct {
	s     *Store
	cfg   Config
	path  string
	now   time.Time
	grant authority.SignedGrant
	key   ed25519.PrivateKey
}

func newProposalRig(t *testing.T) *proposalRig {
	t.Helper()
	r := &proposalRig{now: time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC), path: filepath.Join(t.TempDir(), "proposals")}
	r.cfg = Config{Tenant: "tenant", Site: "office", RunID: "test-run", Audience: "node", Assignee: "robot/navigation", Catalog: map[string]mission.SafetyClass{"wendy.nav.go_to.v1": mission.Move}, Now: func() time.Time { return r.now }}
	_, root, _ := ed25519.GenerateKey(rand.Reader)
	pub, key, _ := ed25519.GenerateKey(rand.Reader)
	r.key = key
	g := authority.Grant{Version: authority.Version, ID: "agent-grant", Tenant: r.cfg.Tenant, Site: r.cfg.Site, Domain: "simulation", Principal: "planner", Kind: "agent", Source: world.Source{Device: "planner", App: "agent", Service: "planner", Stream: "proposals"}, PublicKey: pub, Scopes: []string{"read", "propose"}, NotBeforeUnixMS: r.now.Add(-time.Minute).UnixMilli(), NotAfterUnixMS: r.now.Add(time.Hour).UnixMilli()}
	var err error
	r.grant, err = authority.SignGrant(g, root)
	if err != nil {
		t.Fatal(err)
	}
	r.s, err = Open(r.path, r.cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.s.Close() })
	return r
}
func (r *proposalRig) request(id string) Request {
	return Request{Version: 1, ID: id, Tenant: r.cfg.Tenant, Site: r.cfg.Site, RunID: r.cfg.RunID, Assignee: r.cfg.Assignee, Type: "wendy.nav.go_to.v1", Parameters: json.RawMessage(`{"target":[1,0,0]}`), Summary: "Look at the pump", NotAfterUnixMS: r.now.Add(10 * time.Minute).UnixMilli()}
}
func (r *proposalRig) signed(t *testing.T, req Request) ([]byte, authority.SignedRequest) {
	t.Helper()
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	proof, err := authority.SignRequest(r.grant, r.key, r.cfg.Audience, "propose", body, "unique_nonce_123456789", r.now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	return body, proof
}
func (r *proposalRig) submit(t *testing.T, req Request) (Record, bool, error) {
	t.Helper()
	b, p := r.signed(t, req)
	return r.s.Submit(context.Background(), b, p, func() error { return nil })
}

func TestDurableProposalIdempotencyExpiryAndOwnership(t *testing.T) {
	r := newProposalRig(t)
	req := r.request("p-1")
	first, created, err := r.submit(t, req)
	if err != nil || !created {
		t.Fatal(first, created, err)
	}
	if first.Class != mission.Move || first.Proposal.NotAfterUnixMS != req.NotAfterUnixMS {
		t.Fatal(first)
	}
	size, _ := os.Stat(r.path)
	r.now = r.now.Add(time.Second)
	again, created, err := r.submit(t, req)
	if err != nil || created || !reflect.DeepEqual(first, again) {
		t.Fatal(again, created, err)
	}
	if after, _ := os.Stat(r.path); after.Size() != size.Size() {
		t.Fatal("idempotent read appended another audit entry")
	}
	changed := req
	changed.Summary = "Different task"
	if _, _, err = r.submit(t, changed); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	b, p := r.signed(t, req)
	p.Grant.Grant.Principal = "other"
	if _, _, err = r.s.Submit(context.Background(), b, p, func() error { return nil }); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	_ = r.s.Close()
	r.s, err = Open(r.path, r.cfg)
	if err != nil {
		t.Fatal(err)
	}
	view, err := r.s.Get(req.ID)
	if err != nil || view.Expired || view.AdapterValidated || !reflect.DeepEqual(view.Record, first) {
		t.Fatal(view, err)
	}
	view.Record.Body[0] = 'x'
	view.Record.Proposal.Parameters[0] = 'x'
	again, _, err = r.submit(t, req)
	if err != nil || !reflect.DeepEqual(again, first) {
		t.Fatal("caller changed retained record", err)
	}
	r.now = time.UnixMilli(req.NotAfterUnixMS)
	view, err = r.s.Get(req.ID)
	if err != nil || !view.Expired {
		t.Fatal(view, err)
	}
	if _, _, err = r.submit(t, req); !errors.Is(err, ErrAuthority) {
		t.Fatal("expired proposal reused", err)
	}
}

func TestProposalStoreScopeLockCorruptionAndPartialWrite(t *testing.T) {
	for _, kind := range []string{"scope", "catalog", "lock", "corrupt", "partial"} {
		t.Run(kind, func(t *testing.T) {
			r := newProposalRig(t)
			_, _, err := r.submit(t, r.request("first"))
			if err != nil {
				t.Fatal(err)
			}
			if kind == "lock" {
				if other, err := Open(r.path, r.cfg); err == nil {
					other.Close()
					t.Fatal("second writer admitted")
				}
				return
			}
			_ = r.s.Close()
			cfg := r.cfg
			switch kind {
			case "scope":
				cfg.RunID = "other-run"
			case "catalog":
				cfg.Catalog = map[string]mission.SafetyClass{"wendy.nav.go_to.v1": mission.Observe}
			case "corrupt":
				b, _ := os.ReadFile(r.path)
				b[len(b)-10] ^= 1
				_ = os.WriteFile(r.path, b, 0600)
			case "partial":
				f, _ := os.OpenFile(r.path, os.O_WRONLY|os.O_APPEND, 0600)
				_, _ = f.WriteString(`{"version":1`)
				_ = f.Close()
			}
			if other, err := Open(r.path, cfg); err == nil {
				other.Close()
				t.Fatal("invalid journal reopened")
			}
		})
	}
}

func TestUncertainAppendPoisonsWriterAndPreservesPartialEvidence(t *testing.T) {
	r := newProposalRig(t)
	r.s.append = func(b []byte) error { _, _ = r.s.file.Write(b[:len(b)/2]); return errors.New("disk failure") }
	if _, _, err := r.submit(t, r.request("partial")); !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}
	if _, err := r.s.List(20, ""); !errors.Is(err, ErrUnavailable) {
		t.Fatal("poisoned store remained available", err)
	}
	_ = r.s.Close()
	if s, err := Open(r.path, r.cfg); err == nil {
		s.Close()
		t.Fatal("partial append discarded")
	}
}

func TestExpiryDuringAppendRetainsHistoryWithoutSuccess(t *testing.T) {
	r := newProposalRig(t)
	req := r.request("expiry")
	original := r.s.append
	r.s.append = func(b []byte) error { err := original(b); r.now = r.now.Add(time.Minute); return err }
	if _, created, err := r.submit(t, req); !created || !errors.Is(err, ErrAuthority) {
		t.Fatal(created, err)
	}
	view, err := r.s.Get(req.ID)
	if err != nil || view.Record.Proposal.NotAfterUnixMS != req.NotAfterUnixMS {
		t.Fatal(view, err)
	}
	_ = r.s.Close()
	r.s, err = Open(r.path, r.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.s.Get(req.ID); err != nil {
		t.Fatal("historical record lost", err)
	}
}

func TestFreshAuthorityRequiredAtAppendAndIdempotentRetry(t *testing.T) {
	r := newProposalRig(t)
	b, p := r.signed(t, r.request("denied"))
	calls := 0
	check := func() error {
		calls++
		if calls >= 2 {
			return errors.New("revoked")
		}
		return nil
	}
	if _, _, err := r.s.Submit(context.Background(), b, p, check); !errors.Is(err, ErrAuthority) {
		t.Fatal(err)
	}
	if _, err := r.s.Get("denied"); !errors.Is(err, ErrNotFound) {
		t.Fatal("revoked proposal appended", err)
	}
	_, _, err := r.submit(t, r.request("stored"))
	if err != nil {
		t.Fatal(err)
	}
	b, p = r.signed(t, r.request("stored"))
	if _, _, err = r.s.Submit(context.Background(), b, p, func() error { return errors.New("revoked") }); !errors.Is(err, ErrAuthority) {
		t.Fatal(err)
	}
}

func TestProposalBoundsPaginationAndCapacity(t *testing.T) {
	r := newProposalRig(t)
	r.s.cfg.MaxEntries = 3
	for _, id := range []string{"c", "a", "b"} {
		if _, _, err := r.submit(t, r.request(id)); err != nil {
			t.Fatal(err)
		}
	}
	page, err := r.s.List(2, "")
	if err != nil || len(page.Proposals) != 2 || page.Proposals[0].ID != "a" || page.Next != "b" {
		t.Fatal(page, err)
	}
	page, err = r.s.List(2, page.Next)
	if err != nil || len(page.Proposals) != 1 || page.Proposals[0].ID != "c" || page.Next != "" {
		t.Fatal(page, err)
	}
	if _, _, err = r.submit(t, r.request("d")); !errors.Is(err, ErrCapacity) {
		t.Fatal(err)
	}
	for _, lim := range []int{-1, 0, 101} {
		if _, err = r.s.List(lim, ""); err == nil {
			t.Fatal("invalid page limit")
		}
	}
	for _, params := range []string{`null`, `[]`, `{"x":1,"x":2}`, `{"x":"` + strings.Repeat("a", MaxParametersBytes) + `"}`} {
		q := r.request("invalid")
		q.Parameters = json.RawMessage(params)
		if _, _, err = r.submit(t, q); !errors.Is(err, ErrInvalid) {
			t.Fatal(err)
		}
	}
}

func TestConcurrentProposalIDsHaveOneDurableCreation(t *testing.T) {
	r := newProposalRig(t)
	body, proof := r.signed(t, r.request("shared"))
	type result struct {
		created bool
		err     error
	}
	results := make(chan result, 2)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	submit := func() {
		_, fresh, err := r.s.Submit(ctx, body, proof, func() error { return nil })
		results <- result{fresh, err}
	}
	go submit()
	go submit()
	created := 0
	for i := 0; i < 2; i++ {
		select {
		case got := <-results:
			if got.err != nil {
				t.Fatal(got.err)
			}
			if got.created {
				created++
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	if created != 1 {
		t.Fatal("multiple durable creations", created)
	}
	page, err := r.s.List(100, "")
	if err != nil || len(page.Proposals) != 1 {
		t.Fatal(page, err)
	}
}

func TestProposalStoreRefusesUnsafeFileTypes(t *testing.T) {
	for _, kind := range []string{"data-symlink", "lock-symlink", "fifo", "public-file"} {
		t.Run(kind, func(t *testing.T) {
			r := newProposalRig(t)
			_ = r.s.Close()
			path := filepath.Join(t.TempDir(), "journal")
			switch kind {
			case "data-symlink":
				if err := os.Symlink(r.path, path); err != nil {
					t.Fatal(err)
				}
			case "lock-symlink":
				if err := os.Symlink(r.path, path+".lock"); err != nil {
					t.Fatal(err)
				}
			case "fifo":
				if err := unix.Mkfifo(path, 0600); err != nil {
					t.Fatal(err)
				}
			case "public-file":
				if err := os.WriteFile(path, nil, 0644); err != nil {
					t.Fatal(err)
				}
			}
			if s, err := Open(path, r.cfg); err == nil {
				s.Close()
				t.Fatal("unsafe path accepted")
			}
		})
	}
}
