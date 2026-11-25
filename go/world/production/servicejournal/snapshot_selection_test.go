package servicejournal

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"github.com/wendylabsinc/wendy/go/world/production/proofref"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
	"github.com/wendylabsinc/wendy/go/world/production/trustedtime"
	"github.com/wendylabsinc/wendy/go/world/production/wendycert"
	bolt "go.etcd.io/bbolt"
)

func snapshotSource(e *environment, stream string) proofref.Source {
	return proofref.Source{Tenant: e.scope.Tenant, Site: e.scope.Site, Device: e.scope.Device, App: e.scope.App, Service: e.scope.Service, Component: e.request.ComponentType, Stream: stream}
}

func selectedFixture(t *testing.T) (*environment, []proofref.Source, []serviceorigin.SignedRecord) {
	t.Helper()
	e := setup(t)
	e.publisher.Close()
	e.scope.Publish = append(e.scope.Publish, serviceorigin.PublishScope{ComponentType: e.request.ComponentType, Stream: "unrelated"})
	e.publisherConfig.Scope = e.scope
	var err error
	e.publisher, err = serviceorigin.NewPublisher(e.publisherConfig)
	if err != nil {
		t.Fatal(err)
	}
	var proofs []serviceorigin.SignedRecord
	var sources []proofref.Source
	for _, stream := range []string{"people", "unrelated"} {
		e.request.Stream = stream
		signed := e.sign(t)
		e.admit(t, signed, world.Applied)
		proofs = append(proofs, signed)
		sources = append(sources, snapshotSource(e, stream))
	}
	return e, sources, proofs
}

func TestSnapshotSelectedSkipsUnrelatedProofAndBlockingSchema(t *testing.T) {
	e, sources, originals := selectedFixture(t)
	verifications, schemas := 0, 0
	var err error
	e.j.cfg.Verifier, err = serviceorigin.NewVerifier(serviceorigin.VerifierConfig{Tenant: e.scope.Tenant, Site: e.scope.Site, ExecutionDomain: "simulation", Certificates: e.publisherConfig.Certificates, Clock: e.clock, Authorize: func(_ context.Context, c serviceorigin.DelegationClaims, d wendycert.DeviceIdentity) error {
		verifications++
		if !reflect.DeepEqual(c.Scope, e.scope) || d.Principal != e.scope.Device {
			return serviceorigin.ErrDenied
		}
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	e.j.cfg.AdmitSchema = func(ctx context.Context, v serviceorigin.VerifiedRecord) error {
		schemas++
		if v.Record.Publication.Stream == "unrelated" {
			// Represents an unrelated schema's blocking evidence work. Exact
			// source selection must never enter it, even in either final pass.
			<-ctx.Done()
			return serviceorigin.ErrDenied
		}
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	got, err := e.j.SnapshotSelected(ctx, sources[:1])
	if err != nil || len(got) != 1 || !reflect.DeepEqual(got[0], originals[0]) || verifications != 4 || schemas != 2 {
		t.Fatal("selection performed unrelated authority/schema work", err, len(got), verifications, schemas)
	}
	// Decoding is also skipped: a corrupt unrelated envelope cannot be read
	// through a selected source. The unfiltered API still detects corruption.
	for key, stored := range e.j.proofs {
		_, record, err := decodeProof(stored.Envelope)
		if err != nil {
			t.Fatal(err)
		}
		if record.Publication.Stream == "unrelated" {
			stored.Envelope = []byte("invalid")
			e.j.proofs[key] = stored
		}
	}
	got, err = e.j.SnapshotSelected(context.Background(), sources[:1])
	if err != nil || len(got) != 1 || !reflect.DeepEqual(got[0], originals[0]) {
		t.Fatal("unrelated proof decoded", err)
	}
	if _, err = e.j.Snapshot(context.Background()); !errors.Is(err, ErrFaulted) {
		t.Fatal("unfiltered snapshot semantics changed", err)
	}
}

func TestSnapshotSelectedPreservesOriginalBytesAndCheckCounts(t *testing.T) {
	e, sources, _ := selectedFixture(t)
	clock := &timingClock{inner: e.j.cfg.PhysicalClock}
	e.j.cfg.PhysicalClock = clock
	schema, commit := e.j.cfg.AdmitSchema, e.j.commit
	schemaCalls, commits := 0, 0
	e.j.cfg.AdmitSchema = func(ctx context.Context, v serviceorigin.VerifiedRecord) error { schemaCalls++; return schema(ctx, v) }
	e.j.commit = func(fn func(*bolt.Tx) error) error { commits++; return commit(fn) }
	all, err := e.j.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	counts := [3]int{clock.calls, schemaCalls, commits}
	clock.calls, schemaCalls, commits = 0, 0, 0
	selected, err := e.j.SnapshotSelected(context.Background(), sources)
	if err != nil || !reflect.DeepEqual(all, selected) || counts != [3]int{clock.calls, schemaCalls, commits} || schemaCalls != 4 || commits != 1 {
		t.Fatal("selection changed checks/commit/original bytes", err, counts, clock.calls, schemaCalls, commits)
	}
	before, _ := encode(selected)
	selected[0].Record[0] ^= 1
	again, err := e.j.SnapshotSelected(context.Background(), sources)
	after, _ := encode(again)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("returned selected proof aliased storage", err)
	}
}

func TestSnapshotSelectedRejectsInvalidSelectorsBeforeWork(t *testing.T) {
	e, sources, _ := selectedFixture(t)
	clock := &timingClock{inner: e.j.cfg.PhysicalClock}
	e.j.cfg.PhysicalClock = clock
	schemas, commits := 0, 0
	e.j.cfg.AdmitSchema = func(context.Context, serviceorigin.VerifiedRecord) error { schemas++; return nil }
	e.j.commit = func(func(*bolt.Tx) error) error { commits++; return errors.New("unexpected persistence") }
	for _, mode := range []string{"nil", "empty", "duplicate", "oversized", "zero", "wildcard", "foreign-tenant", "foreign-site", "nil-context"} {
		t.Run(mode, func(t *testing.T) {
			selectors := append([]proofref.Source(nil), sources[:1]...)
			ctx := context.Background()
			switch mode {
			case "nil":
				selectors = nil
			case "empty":
				selectors = []proofref.Source{}
			case "duplicate":
				selectors = append(selectors, selectors[0])
			case "oversized":
				selectors = make([]proofref.Source, 65)
			case "zero":
				selectors[0] = proofref.Source{}
			case "wildcard":
				selectors[0].Stream = "*"
			case "foreign-tenant":
				selectors[0].Tenant = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
			case "foreign-site":
				selectors[0].Site = "another-site"
			case "nil-context":
				ctx = nil
			}
			if got, err := e.j.SnapshotSelected(ctx, selectors); err == nil || len(got) != 0 {
				t.Fatal("invalid selection became wildcard", err)
			}
		})
	}
	if clock.calls != 0 || schemas != 0 || commits != 0 {
		t.Fatal("invalid selectors reached expensive work", clock.calls, schemas, commits)
	}
	var missing *Journal
	if _, err := missing.SnapshotSelected(context.Background(), sources); !errors.Is(err, ErrClosed) {
		t.Fatal("nil journal", err)
	}
}

func TestSnapshotSelectedMatchesEverySourceField(t *testing.T) {
	for _, field := range []string{"device", "app", "service", "component", "stream"} {
		t.Run(field, func(t *testing.T) {
			e, sources, _ := selectedFixture(t)
			selected := sources[0]
			switch field {
			case "device":
				selected.Device = "spiffe://wendy.sh/tenant/" + selected.Tenant + "/device/44444444-4444-4444-8444-444444444444"
			case "app":
				selected.App = "com.other.app"
			case "service":
				selected.Service = "other"
			case "component":
				selected.Component = "wendy.other.v1"
			case "stream":
				selected.Stream = "absent"
			}
			calls := 0
			e.j.cfg.AdmitSchema = func(context.Context, serviceorigin.VerifiedRecord) error { calls++; return nil }
			got, err := e.j.SnapshotSelected(context.Background(), []proofref.Source{selected})
			if err != nil || len(got) != 0 || calls != 0 {
				t.Fatal("partial source match", field, len(got), calls, err)
			}
		})
	}
}

func TestSnapshotSelectedRetainsCurrentAuthorityAndFinalGroupTime(t *testing.T) {
	for _, mode := range []string{"source-expiry", "signer-expiry", "revoked-after-commit", "schema-denied", "tombstone", "source-regression"} {
		t.Run(mode, func(t *testing.T) {
			e := setup(t)
			if mode == "signer-expiry" {
				e.request.TTL = time.Hour
			}
			e.admit(t, e.sign(t), world.Applied)
			e.request.Entity = "second"
			e.admit(t, e.sign(t), world.Applied)
			selectors := []proofref.Source{snapshotSource(e, e.request.Stream)}
			calls := 0
			e.j.cfg.AdmitSchema = func(context.Context, serviceorigin.VerifiedRecord) error {
				calls++
				if mode == "schema-denied" {
					return serviceorigin.ErrDenied
				}
				if calls == 4 {
					switch mode {
					case "source-expiry":
						e.sourceNow = e.sourceNow.Add(3 * time.Second)
					case "signer-expiry":
						e.clock.advance(11 * time.Minute)
					case "source-regression":
						e.sourceNow = e.sourceNow.Add(-time.Second)
					}
				}
				return nil
			}
			if mode == "revoked-after-commit" {
				e.j.commit = func(fn func(*bolt.Tx) error) error { err := e.j.db.Update(fn); e.allow = false; return err }
			}
			if mode == "tombstone" {
				e.request.Tombstone = true
				e.request.Payload = nil
				e.admit(t, e.sign(t), world.Applied)
			}
			got, err := e.j.SnapshotSelected(context.Background(), selectors)
			if mode == "source-regression" {
				if !errors.Is(err, ErrFaulted) || len(got) != 0 {
					t.Fatal("regressed final clock returned proof", err)
				}
				return
			}
			want := 0
			if mode == "tombstone" {
				want = 1
			}
			if err != nil || len(got) != want {
				t.Fatal("selected lifetime/current schema bypass", mode, len(got), err)
			}
			if len(e.j.proofs) != 2 {
				t.Fatal("selection removed high-water history")
			}
		})
	}
}

func TestSnapshotSelectedUncertainCommitAndCancellation(t *testing.T) {
	for _, mode := range []string{"pre-cancel", "post-cancel", "uncertain"} {
		t.Run(mode, func(t *testing.T) {
			e, sources, originals := selectedFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if mode == "pre-cancel" {
				cancel()
			} else {
				e.j.commit = func(fn func(*bolt.Tx) error) error {
					if err := e.j.db.Update(fn); err != nil {
						return err
					}
					if mode == "post-cancel" {
						cancel()
						return nil
					}
					return errors.New("test uncertain committed snapshot")
				}
			}
			got, err := e.j.SnapshotSelected(ctx, sources[:1])
			if len(got) != 0 || err == nil {
				t.Fatal("failed selected read acknowledged", err)
			}
			if mode == "pre-cancel" {
				if !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
			} else if !errors.Is(err, ErrFaulted) {
				t.Fatal("postcommit failure was not uncertain", err)
			}
			e.reopen(t)
			got, err = e.j.SnapshotSelected(context.Background(), sources[:1])
			if err != nil || len(got) != 1 || !reflect.DeepEqual(got[0], originals[0]) {
				t.Fatal("failed read changed original durable proof", err)
			}
		})
	}
}

type selectionGateClock struct {
	inner            trustedtime.Provider
	entered, release chan struct{}
	first            bool
}

func (c *selectionGateClock) Now() (trustedtime.Interval, error) {
	if !c.first {
		c.first = true
		close(c.entered)
		<-c.release
	}
	return c.inner.Now()
}
func TestSnapshotSelectedOwnsSelectorBeforeWaiting(t *testing.T) {
	e, sources, originals := selectedFixture(t)
	gate := &selectionGateClock{inner: e.clock, entered: make(chan struct{}), release: make(chan struct{})}
	e.j.cfg.PhysicalClock = gate
	selected := append([]proofref.Source(nil), sources[:1]...)
	type outcome struct {
		proofs []serviceorigin.SignedRecord
		err    error
	}
	done := make(chan outcome, 1)
	go func() { got, err := e.j.SnapshotSelected(context.Background(), selected); done <- outcome{got, err} }()
	<-gate.entered
	selected[0] = sources[1]
	close(gate.release)
	got := <-done
	if got.err != nil || len(got.proofs) != 1 || !reflect.DeepEqual(got.proofs[0], originals[0]) {
		t.Fatal("caller rewrote selection during authorization", got.err)
	}
}
