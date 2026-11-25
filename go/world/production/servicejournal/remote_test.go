package servicejournal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"github.com/wendylabsinc/wendy/go/world/production/proofref"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
	"github.com/wendylabsinc/wendy/go/world/production/trustedtime"
	"github.com/wendylabsinc/wendy/go/world/production/wendycert"
	bolt "go.etcd.io/bbolt"
)

type remoteEnvironment struct {
	*environment
	r              *RemoteJournal
	remoteCfg      RemoteConfig
	fence          sync.Mutex
	allowed        bool
	authorityUntil time.Time
	epoch          uint64
	authorizations int
	authorizeHook  func(int, serviceorigin.VerifiedRecord)
}

func remoteSetup(t *testing.T) *remoteEnvironment {
	t.Helper()
	e := setup(t)
	f := &remoteEnvironment{environment: e, allowed: true, epoch: 1, authorityUntil: e.clock.w.Earliest.Add(time.Hour)}
	verifier, err := serviceorigin.NewVerifier(serviceorigin.VerifierConfig{Tenant: e.scope.Tenant, Site: e.scope.Site, ExecutionDomain: "simulation", Certificates: e.publisherConfig.Certificates, Clock: e.clock, Authorize: func(_ context.Context, c serviceorigin.DelegationClaims, id wendycert.DeviceIdentity) error {
		if id.Principal != e.scope.Device || c.Scope.Device != e.scope.Device {
			return serviceorigin.ErrDenied
		}
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	f.remoteCfg = RemoteConfig{Path: filepath.Join(filepath.Dir(e.cfg.Path), "remote.db"), LocalDevice: "spiffe://wendy.sh/tenant/" + e.scope.Tenant + "/device/33333333-3333-4333-8333-333333333333", Replica: e.cfg.Replica, Verifier: verifier, PhysicalClock: e.clock, AdmitSchema: e.cfg.AdmitSchema, MaxProofBytes: e.cfg.MaxProofBytes, MaxEntries: e.cfg.MaxEntries,
		Guard: func(ctx context.Context, fn func() error) error {
			f.fence.Lock()
			defer f.fence.Unlock()
			if err := ctx.Err(); err != nil {
				return err
			}
			return fn()
		},
		Authorize: func(_ context.Context, v serviceorigin.VerifiedRecord, w trustedtime.Interval) (AuthorityWindow, error) {
			f.authorizations++
			if f.authorizeHook != nil {
				f.authorizeHook(f.authorizations, v)
			}
			if !f.allowed || !reflect.DeepEqual(v.Record.Scope, e.scope) || v.Record.Epoch != f.epoch {
				return AuthorityWindow{}, serviceorigin.ErrDenied
			}
			return AuthorityWindow{NotBefore: time.UnixMilli(v.Record.IssuedAtUnixMS), NotAfter: f.authorityUntil}, nil
		},
	}
	f.r, err = CreateRemote(f.remoteCfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.r.Close() })
	return f
}
func (f *remoteEnvironment) raw(t *testing.T) []byte {
	t.Helper()
	s := f.sign(t)
	b, e := json.MarshalIndent(s, "", " ")
	if e != nil {
		t.Fatal(e)
	}
	return append(b, '\n')
}
func (f *remoteEnvironment) cap() int64 {
	return f.request.Timing.SampleTime.Add(f.request.TTL).UnixNano()
}
func accept(_ context.Context) error { return nil }
func (f *remoteEnvironment) admitRemote(t *testing.T, raw []byte, want world.Result) {
	t.Helper()
	got, err := f.r.AdmitRemote(context.Background(), raw, f.cap(), accept)
	if err != nil || got != want {
		t.Fatalf("remote=%s want=%s error=%v", got, want, err)
	}
}
func (f *remoteEnvironment) reopenRemote(t *testing.T) {
	t.Helper()
	if e := f.r.Close(); e != nil {
		t.Fatal(e)
	}
	var e error
	f.r, e = OpenRemote(f.remoteCfg)
	if e != nil {
		t.Fatal(e)
	}
}
func exportAccess(f *remoteEnvironment) ExportAccess {
	return ExportAccess{Guard: f.remoteCfg.Guard, Authorize: func(_ context.Context, s []proofref.Source, w trustedtime.Interval) (AuthorityWindow, error) {
		if !f.allowed {
			return AuthorityWindow{}, serviceorigin.ErrDenied
		}
		for _, v := range s {
			if v.Device != f.scope.Device {
				return AuthorityWindow{}, serviceorigin.ErrDenied
			}
		}
		return AuthorityWindow{w.Earliest.Add(-time.Second), f.authorityUntil}, nil
	}}
}
func (f *remoteEnvironment) source() proofref.Source {
	return proofref.Source{Tenant: f.scope.Tenant, Site: f.scope.Site, Device: f.scope.Device, App: f.scope.App, Service: f.scope.Service, Component: f.request.ComponentType, Stream: f.request.Stream}
}

func TestRemoteExactBytesDuplicateCapsAndReopen(t *testing.T) {
	f := remoteSetup(t)
	raw := f.raw(t)
	f.admitRemote(t, raw, world.Applied)
	f.reopenRemote(t)
	live, err := f.r.Snapshot(context.Background())
	if err != nil || len(live) != 1 || !bytes.Equal(live[0].Envelope, raw) {
		t.Fatal("original outer/signed bytes changed", err, len(live))
	}
	initial := live[0].Descriptor
	if initial.Reference.EnvelopeSHA256 != digest(raw) || initial.ExpiresAtUnixNS != f.cap() {
		t.Fatal("wrong original deadline/hash")
	}
	f.sourceNow = f.sourceNow.Add(time.Second)
	if got, err := f.r.AdmitRemote(context.Background(), raw, f.cap()+int64(time.Hour), accept); err != nil || got != world.Duplicate {
		t.Fatal(got, err)
	}
	live, err = f.r.Snapshot(context.Background())
	if err != nil || len(live) != 1 || live[0].Descriptor != initial {
		t.Fatal("duplicate renewed original expiry", err)
	}
	if need, err := f.r.Need(context.Background(), initial); err != nil || need {
		t.Fatal("duplicate hint demanded replay", need, err)
	}
	shorter := initial
	shorter.ExpiresAtUnixNS -= int64(500 * time.Millisecond)
	if need, err := f.r.Need(context.Background(), shorter); err != nil || !need {
		t.Fatal("shortening cap was ignored", need, err)
	}
	if _, err = f.r.AdmitRemote(context.Background(), raw, shorter.ExpiresAtUnixNS, accept); err != nil {
		t.Fatal(err)
	}
	f.sourceNow = time.Unix(0, shorter.ExpiresAtUnixNS).Add(time.Nanosecond)
	f.reopenRemote(t)
	if live, err = f.r.Snapshot(context.Background()); err != nil || len(live) != 0 {
		t.Fatal("shortened duplicate revived after reopen", len(live), err)
	}
	// Returned copies cannot alter retained envelopes or floors.
	page, err := f.r.base.ExportInventory(context.Background(), []proofref.Source{f.source()}, "", 64, exportAccess(f))
	if err != nil || len(page.Entries) != 1 {
		t.Fatal(err)
	}
	body, ok, err := f.r.base.ExportFetch(context.Background(), page.Entries[0], exportAccess(f))
	if err != nil || !ok || !bytes.Equal(body, raw) {
		t.Fatal("expired state not exportable", err)
	}
	clear(body)
	body, ok, err = f.r.base.ExportFetch(context.Background(), page.Entries[0], exportAccess(f))
	if err != nil || !ok || !bytes.Equal(body, raw) {
		t.Fatal("export aliases storage")
	}
}

func TestRemotePartitionReorderTombstoneAndExpiredSigner(t *testing.T) {
	f := remoteSetup(t)
	old := f.raw(t)
	f.request.Payload = []byte(`{"position":[4,5,6]}`)
	newer := f.raw(t)
	f.admitRemote(t, newer, world.Applied)
	f.admitRemote(t, old, world.Obsolete)
	f.request.Tombstone = true
	f.request.Payload = nil
	removed := f.raw(t)
	f.admitRemote(t, removed, world.Applied)
	f.admitRemote(t, newer, world.Obsolete)
	f.admitRemote(t, removed, world.Duplicate)
	if live, e := f.r.Snapshot(context.Background()); e != nil || len(live) != 0 {
		t.Fatal("tombstone visible", e)
	}
	page, e := f.r.base.ExportInventory(context.Background(), []proofref.Source{f.source()}, "", 1, exportAccess(f))
	if e != nil || len(page.Entries) != 1 || !page.Entries[0].Tombstone {
		t.Fatal("tombstone high water lost", e)
	}
	// A valid new signature may carry an already-expired sample only as a
	// register floor, never as live state.
	f.request.Entity = "expired"
	f.request.Tombstone = false
	f.request.Payload = []byte(`{"position":[7,8,9]}`)
	expiredSample := f.raw(t)
	f.sourceNow = f.sourceNow.Add(10 * time.Second)
	f.admitRemote(t, expiredSample, world.Applied)
	f.request.Entity = "unseen"
	unseen := f.raw(t)
	f.clock.advance(11 * time.Minute)
	f.reopenRemote(t)
	if live, e := f.r.Snapshot(context.Background()); e != nil || len(live) != 0 {
		t.Fatal("historical expired signer became live", e)
	}
	if _, e = f.r.AdmitRemote(context.Background(), unseen, f.cap(), accept); !errors.Is(e, serviceorigin.ErrExpired) {
		t.Fatal("unseen expired signature admitted", e)
	}
}

func TestRemoteConflictAndLocalEpochIsolation(t *testing.T) {
	f := remoteSetup(t)
	raw := f.raw(t)
	f.admitRemote(t, raw, world.Applied)
	signed, _, e := decodeRemoteProof(raw)
	if e != nil {
		t.Fatal(e)
	}
	different, _ := json.Marshal(signed)
	if _, e = f.r.AdmitRemote(context.Background(), different, f.cap(), accept); !errors.Is(e, world.ErrConflict) {
		t.Fatal("same-version different exact envelope accepted", e)
	}
	if _, ok := reflect.TypeOf(f.r).MethodByName("ReserveEpoch"); ok {
		t.Fatal("remote journal exposes local epoch reservation")
	}
	next, e := f.j.ReserveEpoch(context.Background(), f.scope.Tenant, f.scope.Device, f.scope.App, f.scope.Service)
	if e != nil || next != 2 {
		t.Fatal("remote admission advanced origin local reservation", next, e)
	}
	// A newer source incarnation creates a receiver floor without reserving
	// any local epoch or retaining live access to its older incarnation.
	f.scope.DeploymentGeneration++
	f.publisherConfig.Scope = f.scope
	f.publisherConfig.Epoch = 2
	f.epoch = 2
	f.publisher.Close()
	f.publisher, e = serviceorigin.NewPublisher(f.publisherConfig)
	if e != nil {
		t.Fatal(e)
	}
	f.request.Entity = "new-incarnation"
	fresh := f.raw(t)
	f.admitRemote(t, fresh, world.Applied)
	f.reopenRemote(t)
	live, e := f.r.Snapshot(context.Background())
	if e != nil || len(live) != 1 || live[0].Descriptor.Reference.Version.Epoch != 2 {
		t.Fatal("old epoch remained live", len(live), e)
	}
	if _, e = f.r.AdmitRemote(context.Background(), raw, f.cap(), accept); e == nil {
		t.Fatal("replaced source regained authority")
	}
	// The two purpose-separated files cannot be opened through the other API.
	local := f.cfg
	local.Path = f.remoteCfg.Path
	if j, e := Open(local); e == nil {
		j.Close()
		t.Fatal("local opener accepted remote store")
	}
	remote := f.remoteCfg
	remote.Path = f.cfg.Path
	if j, e := OpenRemote(remote); e == nil {
		j.Close()
		t.Fatal("remote opener accepted local store")
	}
}

func TestRemoteCommitAuthorityWindowAndUncertainOutcome(t *testing.T) {
	for _, mode := range []string{"pre-denied", "pre-expired", "post-denied", "post-cancel", "uncertain"} {
		t.Run(mode, func(t *testing.T) {
			f := remoteSetup(t)
			raw := f.raw(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls := 0
			check := func(context.Context) error {
				calls++
				switch mode {
				case "pre-denied":
					return serviceorigin.ErrDenied
				case "pre-expired":
					f.clock.advance(2 * time.Second)
				case "post-denied":
					if calls == 2 {
						return serviceorigin.ErrDenied
					}
				case "post-cancel":
					if calls == 2 {
						cancel()
						return ctx.Err()
					}
				}
				return nil
			}
			if mode == "pre-expired" {
				f.authorityUntil = f.clock.w.Latest.Add(time.Second)
			}
			if mode == "uncertain" {
				f.r.base.commit = func(fn func(*bolt.Tx) error) error {
					if e := f.r.base.db.Update(fn); e != nil {
						return e
					}
					return errors.New("injected commit response failure")
				}
			}
			result, e := f.r.AdmitRemote(ctx, raw, f.cap(), check)
			if e == nil || result != "" {
				t.Fatal("failed boundary acknowledged", result, e)
			}
			committed := mode == "post-denied" || mode == "post-cancel" || mode == "uncertain"
			if committed && !errors.Is(e, ErrFaulted) {
				t.Fatal("committed failure not uncertain", e)
			}
			if committed {
				if _, e = f.r.Snapshot(context.Background()); !errors.Is(e, ErrFaulted) {
					t.Fatal("uncertain journal remained usable", e)
				}
			}
			f.authorityUntil = f.clock.w.Earliest.Add(time.Hour)
			f.reopenRemote(t)
			if (len(f.r.base.proofs) == 1) != committed {
				t.Fatal("unexpected durable state", len(f.r.base.proofs), committed)
			}
		})
	}
}

func TestRemoteGroupDeadlineAfterLaterAuthorization(t *testing.T) {
	f := remoteSetup(t)
	f.admitRemote(t, f.raw(t), world.Applied)
	f.request.Entity = "second"
	f.admitRemote(t, f.raw(t), world.Applied)
	start := f.clock.w.Earliest
	f.authorityUntil = start.Add(5 * time.Second)
	f.authorizations = 0
	f.authorizeHook = func(n int, v serviceorigin.VerifiedRecord) {
		if n == 4 {
			f.clock.advance(6 * time.Second)
			f.authorityUntil = start.Add(time.Hour)
		}
	}
	live, e := f.r.Snapshot(context.Background())
	if e != nil || len(live) != 1 {
		t.Fatal("common final interval did not remove earlier expired authority", len(live), e)
	}
	if live[0].Descriptor.Reference.Key.Entity != "second" {
		t.Fatal("wrong group member survived")
	}
}

func TestRemoteGuardBeforeJournalAndConcurrentRevocation(t *testing.T) {
	f := remoteSetup(t)
	original := f.remoteCfg.Guard
	inside := make(chan struct{})
	release := make(chan struct{})
	once := sync.Once{}
	f.r.cfg.Guard = func(ctx context.Context, fn func() error) error {
		if !f.r.base.mu.TryLock() {
			t.Error("journal locked before host guard")
			return serviceorigin.ErrDenied
		}
		f.r.base.mu.Unlock()
		return original(ctx, func() error { once.Do(func() { close(inside); <-release }); return fn() })
	}
	raw := f.raw(t)
	done := make(chan error, 1)
	go func() { _, e := f.r.AdmitRemote(context.Background(), raw, f.cap(), accept); done <- e }()
	<-inside
	revoked := make(chan struct{})
	go func() { f.fence.Lock(); f.allowed = false; f.fence.Unlock(); close(revoked) }()
	select {
	case <-revoked:
		t.Fatal("revocation escaped the shared fence")
	default:
	}
	close(release)
	if e := <-done; e != nil {
		t.Fatal(e)
	}
	<-revoked
	if live, e := f.r.Snapshot(context.Background()); e != nil || len(live) != 0 {
		t.Fatal("revoked remote records remained live", e)
	}
}

func TestRemoteCapacityCorruptionAndRequiredCallbacks(t *testing.T) {
	t.Run("quota", func(t *testing.T) {
		f := remoteSetup(t)
		f.r.base.cfg.MaxEntries = 1
		f.admitRemote(t, f.raw(t), world.Applied)
		f.request.Entity = "second"
		if _, e := f.r.AdmitRemote(context.Background(), f.raw(t), f.cap(), accept); !errors.Is(e, ErrCapacity) {
			t.Fatal("quota evicted high water", e)
		}
	})
	t.Run("corruption", func(t *testing.T) {
		f := remoteSetup(t)
		f.admitRemote(t, f.raw(t), world.Applied)
		if e := f.r.base.db.Update(func(tx *bolt.Tx) error {
			c := tx.Bucket(proofsBucket).Cursor()
			k, v := c.First()
			var p storedProof
			if e := json.Unmarshal(v, &p); e != nil {
				return e
			}
			p.Envelope[0] = '!'
			b, _ := json.Marshal(p)
			return tx.Bucket(proofsBucket).Put(k, b)
		}); e != nil {
			t.Fatal(e)
		}
		f.r.Close()
		if j, e := OpenRemote(f.remoteCfg); e == nil {
			j.Close()
			t.Fatal("corrupt proof accepted")
		}
	})
	t.Run("required", func(t *testing.T) {
		f := remoteSetup(t)
		for _, drop := range []string{"guard", "authorize", "verifier", "clock", "schema", "local"} {
			cfg := f.remoteCfg
			cfg.Path = filepath.Join(filepath.Dir(cfg.Path), drop+".db")
			switch drop {
			case "guard":
				cfg.Guard = nil
			case "authorize":
				cfg.Authorize = nil
			case "verifier":
				cfg.Verifier = nil
			case "clock":
				cfg.PhysicalClock = nil
			case "schema":
				cfg.AdmitSchema = nil
			case "local":
				cfg.LocalDevice = ""
			}
			if j, e := CreateRemote(cfg); e == nil {
				j.Close()
				t.Fatal("missing dependency accepted", drop)
			}
			if _, e := os.Stat(cfg.Path); !errors.Is(e, os.ErrNotExist) {
				t.Fatal("invalid config created storage", drop)
			}
		}
	})
}
