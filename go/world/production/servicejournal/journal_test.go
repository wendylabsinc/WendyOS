package servicejournal

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"github.com/wendylabsinc/wendy/go/world/production/internal/testidentity"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
	"github.com/wendylabsinc/wendy/go/world/production/trustedtime"
	"github.com/wendylabsinc/wendy/go/world/production/wendycert"
	bolt "go.etcd.io/bbolt"
)

type clock struct {
	mu sync.Mutex
	w  trustedtime.Interval
}

func (c *clock) Now() (trustedtime.Interval, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.w, nil
}
func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.w.Earliest = c.w.Earliest.Add(d)
	c.w.Latest = c.w.Latest.Add(d)
	c.w.Monotonic += d
}

type environment struct {
	cfg             Config
	j               *Journal
	clock           *clock
	sourceNow       time.Time
	scope           serviceorigin.Scope
	publisher       *serviceorigin.Publisher
	publisherConfig serviceorigin.PublisherConfig
	request         serviceorigin.PublishRequest
	allow           bool
}

func setup(t *testing.T) *environment {
	t.Helper()
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	f := testidentity.NewWith(t, now, "spiffe://wendy.sh/tenant/"+testidentity.Tenant+"/device/22222222-2222-4222-8222-222222222222")
	certs, err := wendycert.NewDevice(wendycert.Config{Tenant: f.Tenant, Roots: [][]byte{f.RootDER}})
	if err != nil {
		t.Fatal(err)
	}
	e := &environment{clock: &clock{w: trustedtime.Interval{Earliest: now, Latest: now.Add(time.Millisecond), BootID: "test-boot", Monotonic: time.Second, Domain: "physical", Authenticated: true, Source: "test-time-authority"}}, sourceNow: time.Unix(10, 0).UTC(), allow: true}
	e.scope = serviceorigin.Scope{Tenant: f.Tenant, Site: "office", Device: f.Principal, App: "com.example.camera", Service: "detector", DeploymentGeneration: 7, ExecutionDomain: "simulation", Publish: []serviceorigin.PublishScope{{ComponentType: "wendy.pose.v1", Stream: "people"}}}
	verifyCfg := serviceorigin.VerifierConfig{Tenant: e.scope.Tenant, Site: e.scope.Site, ExecutionDomain: "simulation", Clock: e.clock, Certificates: certs, Authorize: func(_ context.Context, claims serviceorigin.DelegationClaims, id wendycert.DeviceIdentity) error {
		if !e.allow || !reflect.DeepEqual(claims.Scope, e.scope) || id.Principal != f.Principal {
			return serviceorigin.ErrDenied
		}
		return nil
	}}
	verifier, err := serviceorigin.NewVerifier(verifyCfg)
	if err != nil {
		t.Fatal(err)
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	e.cfg = Config{Path: filepath.Join(root, "private", "world.db"), Replica: world.Config{Tenant: e.scope.Tenant, Site: e.scope.Site, ClockDomain: "simulation:run-1", Now: func() time.Time { return e.sourceNow }, MaxFutureSkew: time.Millisecond, MaxEntries: 8, AllowSynthetic: true, Frames: []world.FrameRef{{ID: "floor", Revision: "r1"}}}, Verifier: verifier, PhysicalClock: e.clock, MaxEntries: 8, MaxProofBytes: 2 << 20, AdmitSchema: func(_ context.Context, v serviceorigin.VerifiedRecord) error {
		if v.Record.Publication.ComponentType != "wendy.pose.v1" {
			return serviceorigin.ErrDenied
		}
		return nil
	}}
	e.j, err = Create(e.cfg)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { e.j.Close() })
	epoch, err := e.j.ReserveEpoch(context.Background(), f.Tenant, f.Principal, e.scope.App, e.scope.Service)
	if err != nil {
		t.Fatalf("reserve epoch: %v", err)
	}
	e.publisherConfig = serviceorigin.PublisherConfig{Scope: e.scope, Epoch: epoch, Lifetime: 10 * time.Minute, Clock: e.clock, Certificates: certs, ChainDER: f.ChainDER, DeviceKey: f.Key}
	e.publisher, err = serviceorigin.NewPublisher(e.publisherConfig)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.publisher.Close() })
	e.request = serviceorigin.PublishRequest{Entity: "person", ComponentType: "wendy.pose.v1", Stream: "people", Frame: world.FrameRef{ID: "floor", Revision: "r1"}, Timing: world.Timing{SampleTime: e.sourceNow, ClockDomain: "simulation:run-1", ErrorKnown: true}, TTL: 2 * time.Second, Provenance: world.Provenance{Origin: world.Simulated, AppVersion: "1", Simulation: &world.SimulationContext{RunID: "run-1", ScenarioVersion: "scenario-1", Seed: 10, ClockDomain: "simulation:run-1"}}, Payload: []byte("{ \"position\" : [1,2,3] }\n")}
	return e
}
func (e *environment) sign(t *testing.T) serviceorigin.SignedRecord {
	t.Helper()
	signed, err := e.publisher.Publish(context.Background(), e.request)
	if err != nil {
		t.Fatal(err)
	}
	return signed
}
func (e *environment) reopen(t *testing.T) {
	t.Helper()
	if err := e.j.Close(); err != nil {
		t.Fatal(err)
	}
	j, err := Open(e.cfg)
	if err != nil {
		t.Fatal(err)
	}
	e.j = j
}
func (e *environment) admit(t *testing.T, s serviceorigin.SignedRecord, want world.Result) {
	t.Helper()
	got, err := e.j.Admit(context.Background(), s)
	if err != nil || got != want {
		t.Fatalf("admit=%q %v, want %q", got, err, want)
	}
}

func TestDurableOriginalProofEpochAndExpiry(t *testing.T) {
	e := setup(t)
	original := e.sign(t)
	e.admit(t, original, world.Applied)
	before, _ := encode(original)
	e.reopen(t)
	got, err := e.j.Snapshot(context.Background())
	if err != nil || len(got) != 1 {
		t.Fatalf("snapshot=%d %v", len(got), err)
	}
	after, _ := encode(got[0])
	if !bytes.Equal(before, after) {
		t.Fatal("restore rewrote original proof")
	}
	if !bytes.Equal(got[0].Record, original.Record) || !bytes.Equal(got[0].Delegation.Claims, original.Delegation.Claims) {
		t.Fatal("signed bytes changed")
	}
	e.admit(t, original, world.Duplicate)
	next, err := e.j.ReserveEpoch(context.Background(), e.scope.Tenant, e.scope.Device, e.scope.App, e.scope.Service)
	if err != nil || next != 2 {
		t.Fatalf("epoch=%d %v", next, err)
	}
	if _, err = e.j.Admit(context.Background(), original); !errors.Is(err, ErrEpoch) {
		t.Fatalf("old publisher committed after replacement: %v", err)
	}
	// Rotation does not renew or immediately erase an already admitted sample.
	got, err = e.j.Snapshot(context.Background())
	if err != nil || len(got) != 1 {
		t.Fatalf("retained snapshot=%d %v", len(got), err)
	}
	e.sourceNow = e.sourceNow.Add(3 * time.Second)
	got, err = e.j.Snapshot(context.Background())
	if err != nil || len(got) != 0 {
		t.Fatalf("expired snapshot=%d %v", len(got), err)
	}
	e.reopen(t)
	got, err = e.j.Snapshot(context.Background())
	if err != nil || len(got) != 0 {
		t.Fatal("reopen revived expired observation")
	}
	next, err = e.j.ReserveEpoch(context.Background(), e.scope.Tenant, e.scope.Device, e.scope.App, e.scope.Service)
	if err != nil || next != 3 {
		t.Fatalf("reopened epoch=%d %v", next, err)
	}
}

func TestHistoricalExpiredProofOpensButIsNeverLive(t *testing.T) {
	e := setup(t)
	e.request.TTL = time.Hour
	original := e.sign(t)
	e.admit(t, original, world.Applied)
	e.clock.advance(11 * time.Minute)
	e.reopen(t)
	got, err := e.j.Snapshot(context.Background())
	if err != nil || len(got) != 0 {
		t.Fatalf("expired authorization visible: %d %v", len(got), err)
	}
	if len(e.j.proofs) != 1 {
		t.Fatal("expired proof high-water was deleted")
	}
	if _, err = e.j.Admit(context.Background(), original); !errors.Is(err, serviceorigin.ErrExpired) {
		t.Fatalf("expired proof readmitted: %v", err)
	}
}

func TestReplicaConflictObsoleteTombstoneAndMissingFreshness(t *testing.T) {
	e := setup(t)
	first := e.sign(t)
	e.admit(t, first, world.Applied)
	p, err := serviceorigin.NewPublisher(e.publisherConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	conflict, err := p.Publish(context.Background(), e.request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = e.j.Admit(context.Background(), conflict); !errors.Is(err, world.ErrConflict) {
		t.Fatalf("same source version accepted conflicting proof: %v", err)
	}
	e.request.Payload = []byte(`{"position":[4,5,6]}`)
	second := e.sign(t)
	e.admit(t, second, world.Applied)
	e.admit(t, first, world.Obsolete)
	e.request.Timing.ErrorKnown = false
	unknown := e.sign(t)
	if _, err = e.j.Admit(context.Background(), unknown); !errors.Is(err, world.ErrUnknownFreshness) {
		t.Fatalf("unknown sample freshness admitted: %v", err)
	}
	e.request.Timing.ErrorKnown = true
	e.request.Tombstone = true
	e.request.Payload = nil
	tombstone := e.sign(t)
	e.admit(t, tombstone, world.Applied)
	e.reopen(t)
	got, err := e.j.Snapshot(context.Background())
	if err != nil || len(got) != 0 {
		t.Fatal("tombstone became visible")
	}
	e.admit(t, second, world.Obsolete)
}

func TestPolicySchemaAndSnapshotExpiryAreRechecked(t *testing.T) {
	e := setup(t)
	first := e.sign(t)
	e.admit(t, first, world.Applied)
	e.allow = false
	if _, err := e.j.Admit(context.Background(), first); !errors.Is(err, serviceorigin.ErrDenied) {
		t.Fatal("revocation ignored")
	}
	got, err := e.j.Snapshot(context.Background())
	if err != nil || len(got) != 0 {
		t.Fatal("revoked proof visible")
	}
	e.allow = true
	e.j.cfg.AdmitSchema = func(_ context.Context, v serviceorigin.VerifiedRecord) error {
		v.Record.Publication.Payload[0] = 'x'
		v.Delegation.Scope.Publish[0].Stream = "changed"
		v.Bytes[0] = 'x'
		return nil
	}
	e.admit(t, first, world.Duplicate)
	got, err = e.j.Snapshot(context.Background())
	if err != nil || len(got) != 1 || !bytes.Equal(got[0].Record, first.Record) {
		t.Fatal("schema callback changed committed proof")
	}
	calls := 0
	e.j.cfg.AdmitSchema = func(context.Context, serviceorigin.VerifiedRecord) error {
		calls++
		if calls == 2 {
			e.sourceNow = e.sourceNow.Add(3 * time.Second)
		}
		return nil
	}
	got, err = e.j.Snapshot(context.Background())
	if err != nil || len(got) != 0 {
		t.Fatal("sample expired during snapshot remained visible")
	}
}

func TestUncertainCommitFaultsAndRetainsDurableOutcome(t *testing.T) {
	e := setup(t)
	signed := e.sign(t)
	e.j.commit = func(fn func(*bolt.Tx) error) error {
		if err := e.j.db.Update(fn); err != nil {
			return err
		}
		return errors.New("simulated post-commit fsync uncertainty")
	}
	if _, err := e.j.Admit(context.Background(), signed); !errors.Is(err, ErrFaulted) {
		t.Fatalf("uncertain outcome=%v", err)
	}
	if _, err := e.j.Snapshot(context.Background()); !errors.Is(err, ErrFaulted) {
		t.Fatal("faulted journal served data")
	}
	e.reopen(t)
	got, err := e.j.Snapshot(context.Background())
	if err != nil || len(got) != 1 {
		t.Fatalf("committed outcome lost: %d %v", len(got), err)
	}
	e.admit(t, signed, world.Duplicate)
}

func TestPostCommitRevocationFaultsWithoutRewritingProof(t *testing.T) {
	e := setup(t)
	signed := e.sign(t)
	e.j.commit = func(fn func(*bolt.Tx) error) error { err := e.j.db.Update(fn); e.allow = false; return err }
	if _, err := e.j.Admit(context.Background(), signed); !errors.Is(err, ErrFaulted) {
		t.Fatalf("postcommit revocation=%v", err)
	}
	e.reopen(t)
	got, err := e.j.Snapshot(context.Background())
	if err != nil || len(got) != 0 || len(e.j.proofs) != 1 {
		t.Fatal("revoked committed outcome incorrectly exposed/deleted")
	}
}

func TestMissingMarkersCorruptionAndConfigurationNeverResetEpochs(t *testing.T) {
	for _, mode := range []string{"missing database", "missing marker", "marker mismatch", "missing proof", "corrupt proof", "changed checkpoint", "changed config"} {
		t.Run(mode, func(t *testing.T) {
			e := setup(t)
			signed := e.sign(t)
			e.admit(t, signed, world.Applied)
			e.j.Close()
			switch mode {
			case "missing database":
				if err := os.Remove(e.cfg.Path); err != nil {
					t.Fatal(err)
				}
			case "missing marker":
				if err := os.Remove(e.cfg.Path + ".installation"); err != nil {
					t.Fatal(err)
				}
			case "marker mismatch":
				raw, err := os.ReadFile(e.cfg.Path + ".installation")
				if err != nil {
					t.Fatal(err)
				}
				raw[len(raw)-5] ^= 1
				if err = os.WriteFile(e.cfg.Path+".installation", raw, 0600); err != nil {
					t.Fatal(err)
				}
			case "changed config":
				e.cfg.MaxEntries++
				e.cfg.Replica.MaxEntries++
			default:
				db, err := openDB(e.cfg.Path)
				if err != nil {
					t.Fatal(err)
				}
				err = db.Update(func(tx *bolt.Tx) error {
					switch mode {
					case "missing proof":
						k, _ := tx.Bucket(proofsBucket).Cursor().First()
						return tx.Bucket(proofsBucket).Delete(k)
					case "corrupt proof":
						k, v := tx.Bucket(proofsBucket).Cursor().First()
						copy := bytes.Clone(v)
						copy[len(copy)-4] ^= 1
						return tx.Bucket(proofsBucket).Put(k, copy)
					default:
						var cp world.Checkpoint
						if err := decodeChecked(tx.Bucket(metaBucket).Get(checkpointKey), &cp, e.cfg.MaxProofBytes); err != nil {
							return err
						}
						cp.Entries[0].Component.Key.Entity = "wrong-entity"
						return putChecked(tx.Bucket(metaBucket), checkpointKey, cp)
					}
				})
				db.Close()
				if err != nil {
					t.Fatal(err)
				}
			}
			if j, err := Open(e.cfg); err == nil {
				j.Close()
				t.Fatal("damaged journal silently opened/reset")
			}
			if j, err := Create(e.cfg); err == nil {
				j.Close()
				t.Fatal("existing history silently replaced")
			}
		})
	}
}

func TestDurablePhysicalAndSourceClockFloors(t *testing.T) {
	for _, mode := range []string{"physical", "source"} {
		t.Run(mode, func(t *testing.T) {
			e := setup(t)
			e.admit(t, e.sign(t), world.Applied)
			e.clock.advance(time.Minute)
			e.sourceNow = e.sourceNow.Add(time.Second)
			if _, err := e.j.Snapshot(context.Background()); err != nil {
				t.Fatal(err)
			}
			e.j.Close()
			if mode == "physical" {
				e.clock.advance(-time.Second)
			} else {
				e.sourceNow = e.sourceNow.Add(-time.Millisecond)
			}
			if j, err := Open(e.cfg); err == nil {
				j.Close()
				t.Fatal("durable clock floor rolled backwards")
			}
		})
	}
}

func TestUnreservedEpochCancellationAndCapacity(t *testing.T) {
	e := setup(t)
	cfg := e.publisherConfig
	cfg.Epoch++
	p, err := serviceorigin.NewPublisher(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	signed, err := p.Publish(context.Background(), e.request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = e.j.Admit(context.Background(), signed); !errors.Is(err, ErrEpoch) {
		t.Fatal("unreserved epoch admitted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = e.j.ReserveEpoch(ctx, e.scope.Tenant, e.scope.Device, e.scope.App, e.scope.Service); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if e.j.epochs[sourceKey(fromScope(e.scope))] != 1 {
		t.Fatal("canceled reservation consumed an epoch")
	}
	for n := 0; n < e.cfg.MaxEntries; n++ {
		e.request.Entity = string(rune('a' + n))
		e.admit(t, e.sign(t), world.Applied)
	}
	e.request.Entity = "overflow"
	if _, err = e.j.Admit(context.Background(), e.sign(t)); !errors.Is(err, world.ErrCapacity) && !errors.Is(err, ErrCapacity) {
		t.Fatalf("quota not enforced: %v", err)
	}
	e.reopen(t)
	if len(e.j.proofs) != e.cfg.MaxEntries {
		t.Fatal("quota failure evicted high-water history")
	}
}

func TestSnapshotUsesFinalExpiryForWholeGroup(t *testing.T) {
	for _, mode := range []string{"source", "physical"} {
		t.Run(mode, func(t *testing.T) {
			e := setup(t)
			e.admit(t, e.sign(t), world.Applied)
			e.request.Entity = "second"
			e.admit(t, e.sign(t), world.Applied)
			calls := 0
			e.j.cfg.AdmitSchema = func(context.Context, serviceorigin.VerifiedRecord) error {
				calls++
				if calls == 4 {
					if mode == "source" {
						e.sourceNow = e.sourceNow.Add(3 * time.Second)
					} else {
						e.clock.advance(11 * time.Minute)
					}
				}
				return nil
			}
			got, err := e.j.Snapshot(context.Background())
			if err != nil || len(got) != 0 {
				t.Fatalf("group retained an expired earlier member: %d %v", len(got), err)
			}
		})
	}
}
