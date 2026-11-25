package proofarchive

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
	"github.com/wendylabsinc/wendy/go/world/production/internal/testidentity"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
	"github.com/wendylabsinc/wendy/go/world/production/trustedtime"
	"github.com/wendylabsinc/wendy/go/world/production/wendycert"
	bolt "go.etcd.io/bbolt"
)

type testClock struct {
	mu   sync.Mutex
	w    trustedtime.Interval
	fail bool
}

func (c *testClock) Now() (trustedtime.Interval, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.fail {
		return trustedtime.Interval{}, ErrClock
	}
	return c.w, nil
}
func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.w.Earliest = c.w.Earliest.Add(d)
	c.w.Latest = c.w.Latest.Add(d)
	c.w.Monotonic += d
}

type environment struct {
	a             *Archive
	cfg           Config
	clock         *testClock
	source, claim *serviceorigin.Publisher
	verifier      *serviceorigin.Verifier
	identity      *testidentity.Fixture
	certificates  *wendycert.DeviceVerifier
	mu            sync.Mutex
	denySource    bool
	authHook      func()
	pinCalls      int
	pinHook       func(int, []Descriptor) error
}

func setup(t *testing.T, change ...func(*Config)) *environment {
	t.Helper()
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	id := testidentity.NewWith(t, now, "spiffe://wendy.sh/tenant/"+testidentity.Tenant+"/device/22222222-2222-4222-8222-222222222222")
	certs, err := wendycert.NewDevice(wendycert.Config{Tenant: id.Tenant, Roots: [][]byte{id.RootDER}})
	if err != nil {
		t.Fatal(err)
	}
	e := &environment{clock: &testClock{w: trustedtime.Interval{Earliest: now, Latest: now.Add(time.Millisecond), BootID: "test-boot", Monotonic: time.Second, Domain: "physical", Authenticated: true, Source: "test-only"}}}
	e.identity, e.certificates = id, certs
	e.verifier, err = serviceorigin.NewVerifier(serviceorigin.VerifierConfig{Tenant: id.Tenant, Site: "office", ExecutionDomain: "simulation", Clock: e.clock, Certificates: certs, Authorize: func(_ context.Context, c serviceorigin.DelegationClaims, _ wendycert.DeviceIdentity) error {
		e.mu.Lock()
		defer e.mu.Unlock()
		if e.denySource && c.Scope.Service == "source" {
			return serviceorigin.ErrDenied
		}
		if e.authHook != nil {
			e.authHook()
		}
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	for _, service := range []string{"source", "claim"} {
		component, stream := "wendy.sensor.scalar.v1", "gas"
		if service == "claim" {
			component, stream = "wendy.inference.claim.v2", "claims"
		}
		p, err := serviceorigin.NewPublisher(serviceorigin.PublisherConfig{Scope: serviceorigin.Scope{Tenant: id.Tenant, Site: "office", Device: id.Principal, App: "com.example.sensor", Service: service, DeploymentGeneration: 1, ExecutionDomain: "simulation", Publish: []serviceorigin.PublishScope{{ComponentType: component, Stream: stream}}}, Epoch: 7, Lifetime: time.Hour, Clock: e.clock, Certificates: certs, ChainDER: id.ChainDER, DeviceKey: id.Key})
		if err != nil {
			t.Fatal(err)
		}
		if service == "source" {
			e.source = p
		} else {
			e.claim = p
		}
		t.Cleanup(func() { _ = p.Close() })
	}
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	e.cfg = Config{Path: filepath.Join(dir, "private", "proofs.db"), Tenant: id.Tenant, Site: "office", Device: id.Principal, Clock: e.clock, Verifier: e.verifier, RecentWindow: 10 * time.Second, MaxRetention: 6 * time.Hour, QuotaBytes: 4 << 20, MaxEntries: 32, MaxPins: 8, MaxRefsPerPin: 8}
	e.cfg.ValidatePin = func(ctx context.Context, v serviceorigin.VerifiedRecord, inputs []Descriptor, _ time.Time) error {
		var body struct {
			Inputs []Reference `json:"inputs"`
		}
		if json.Unmarshal(v.Record.Publication.Payload, &body) != nil || len(body.Inputs) != len(inputs) {
			return ErrDenied
		}
		for i, d := range inputs {
			if d.Reference != body.Inputs[i] {
				return ErrDenied
			}
			signed, ref, err := e.a.decodeEnvelope(d.Envelope)
			if err != nil || ref != d.Reference {
				return ErrDenied
			}
			if _, err = e.verifier.Verify(ctx, signed); err != nil {
				return err
			}
		}
		e.mu.Lock()
		e.pinCalls++
		count, hook := e.pinCalls, e.pinHook
		e.mu.Unlock()
		if hook != nil {
			return hook(count, inputs)
		}
		return nil
	}
	for _, f := range change {
		f(&e.cfg)
	}
	e.a, err = Create(e.cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.a.Close() })
	return e
}
func (e *environment) raw(t *testing.T, claim bool, entity string, refs ...Reference) []byte {
	t.Helper()
	p := e.source
	typ, stream, payload := "wendy.sensor.scalar.v1", "gas", []byte("{ \"value\" : 1 }\n")
	if claim {
		p = e.claim
		typ, stream = "wendy.inference.claim.v2", "claims"
		payload = encode(struct {
			Inputs []Reference `json:"inputs"`
		}{refs})
	}
	signed, err := p.Publish(context.Background(), serviceorigin.PublishRequest{Entity: entity, ComponentType: typ, Stream: stream, Frame: world.FrameRef{ID: "floor", Revision: "r1"}, Timing: world.Timing{SampleTime: time.Unix(10, 0).UTC(), ClockDomain: "simulation:run", ErrorKnown: true}, TTL: 2 * time.Second, Provenance: world.Provenance{Origin: world.Simulated, AppVersion: "test", Simulation: &world.SimulationContext{RunID: "run", ScenarioVersion: "test", Seed: 1, ClockDomain: "simulation:run"}}, Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.MarshalIndent(signed, "", " ")
	if err != nil {
		t.Fatal(err)
	}
	return append(raw, '\n')
}
func remember(t *testing.T, e *environment, raw []byte) Reference {
	t.Helper()
	ref, err := e.a.RememberAccepted(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	return ref
}
func get(t *testing.T, e *environment, r Reference) HistoricalProof {
	t.Helper()
	got, err := e.a.Get(context.Background(), r.Source(), r)
	if err != nil {
		t.Fatal(err)
	}
	return got
}
func (e *environment) until(d time.Duration) int64 {
	w, _ := e.clock.Now()
	return w.Earliest.Add(d).UnixMilli()
}
func reopen(t *testing.T, e *environment) {
	t.Helper()
	if err := e.a.Close(); err != nil {
		t.Fatal(err)
	}
	var err error
	e.a, err = Open(e.cfg)
	if err != nil {
		t.Fatal(err)
	}
}

func TestReplacementKeepsExactOlderInputAndPinsAllBeforeRestart(t *testing.T) {
	e := setup(t)
	first := e.raw(t, false, "gas")
	a := remember(t, e, first)
	second := e.raw(t, false, "gas")
	b := remember(t, e, second)
	if a.Key != b.Key || a.Version.Compare(b.Version) >= 0 {
		t.Fatal("fixture did not replace one latest key")
	}
	claim := e.raw(t, true, "hazard", a, b)
	until := e.until(2 * time.Hour)
	pin, err := e.a.Pin(context.Background(), PinRequest{ClaimEnvelope: claim, Inputs: []Reference{a, b}, RetainUntilUnixMS: until})
	if err != nil {
		t.Fatal(err)
	}
	claimRef := remember(t, e, claim)
	if pin.Claim != claimRef {
		t.Fatal("claim pin did not bind exact envelope")
	}
	for i, ref := range []Reference{a, b, claimRef} {
		got := get(t, e, ref)
		want := [][]byte{first, second, claim}[i]
		if !got.Historical || !bytes.Equal(got.Envelope, want) || got.Reference.EnvelopeSHA256 != digest(want) || got.RetainUntilUnixMS != until {
			t.Fatal("original proof bytes or requested retention changed")
		}
	}
	reopen(t, e)
	got := get(t, e, a)
	got.Envelope[0] = 'x'
	if !bytes.Equal(get(t, e, a).Envelope, first) {
		t.Fatal("caller mutated archived proof")
	}
	if current, err := e.a.CheckPin(context.Background(), claimRef); err != nil || !reflect.DeepEqual(current, pin) {
		t.Fatal("durable all-input pin changed", err)
	}
	// The original publication authority expires after one hour. Historical
	// retrieval still uses the root's independent current reader grant.
	e.clock.advance(90 * time.Minute)
	reopen(t, e)
	if got := get(t, e, a); !got.Historical || !bytes.Equal(got.Envelope, first) {
		t.Fatal("expired signer deleted audit bytes")
	}
	if _, err := e.a.RememberAccepted(context.Background(), first); err == nil {
		t.Fatal("expired source authority renewed a record")
	}
}

func TestExactReferenceAndOwnerAreRequiredWithoutFaultingStore(t *testing.T) {
	e := setup(t)
	ref := remember(t, e, e.raw(t, false, "gas"))
	expected := ref.Source()
	wrong := expected
	wrong.Service = "other"
	if got, err := e.a.Get(context.Background(), wrong, ref); !errors.Is(err, ErrUnavailable) || len(got.Envelope) != 0 {
		t.Fatal("hash disclosed another source")
	}
	for _, change := range []func(*Reference){func(r *Reference) { r.Version.Sequence++ }, func(r *Reference) { r.Key.Entity = "different" }, func(r *Reference) { r.Key.Type = "other.v1" }, func(r *Reference) { r.RecordSHA256 = digest([]byte("wrong")) }} {
		bad := ref
		change(&bad)
		got, err := e.a.Get(context.Background(), bad.Source(), bad)
		if !errors.Is(err, ErrUnavailable) || len(got.Envelope) != 0 {
			t.Fatal("partial reference matched", err)
		}
		get(t, e, ref)
	}
}

func TestDuplicateRecentWindowAndPrunedVersionCannotRenew(t *testing.T) {
	e := setup(t)
	raw := e.raw(t, false, "gas")
	ref := remember(t, e, raw)
	original := get(t, e, ref)
	e.clock.advance(5 * time.Second)
	if got := remember(t, e, raw); got != ref {
		t.Fatal("duplicate changed reference")
	}
	if got := get(t, e, ref); got.RecentUntilUnixMS != original.RecentUntilUnixMS || got.Admitted != original.Admitted {
		t.Fatal("duplicate refreshed eligibility")
	}
	// Formatting the outer envelope differently is still a different original
	// envelope and cannot replace an existing source version.
	var signed serviceorigin.SignedRecord
	_ = json.Unmarshal(raw, &signed)
	if _, err := e.a.RememberAccepted(context.Background(), encode(signed)); !errors.Is(err, ErrConflict) {
		t.Fatal("same version changed original envelope", err)
	}
	e.clock.advance(6 * time.Second)
	if err := e.a.Prune(context.Background()); err != nil {
		t.Fatal(err)
	}
	reopen(t, e)
	if _, err := e.a.RememberAccepted(context.Background(), raw); !errors.Is(err, ErrUnavailable) {
		t.Fatal("pruned old version opened a new recent window", err)
	}
	remember(t, e, e.raw(t, false, "gas"))
}

func TestPinRefusalIsAtomicForMissingQuotaAndCurrentAuthority(t *testing.T) {
	for _, mode := range []string{"missing", "quota", "source-denied", "callback-denied", "callback-expired"} {
		t.Run(mode, func(t *testing.T) {
			e := setup(t)
			one := remember(t, e, e.raw(t, false, "one"))
			two := remember(t, e, e.raw(t, false, "two"))
			before := get(t, e, one)
			refs := []Reference{one, two}
			if mode == "missing" {
				refs[1].EnvelopeSHA256 = digest([]byte("missing"))
			}
			claim := e.raw(t, true, "claim", refs...)
			until := e.until(time.Minute)
			switch mode {
			case "quota":
				var u usage
				if err := e.a.db.View(func(tx *bolt.Tx) error { var err error; u, err = e.a.usage(tx); return err }); err != nil {
					t.Fatal(err)
				}
				// Only pin/index metadata is new: keeping proof bytes constant must
				// still consume quota and roll back the entire retention extension.
				e.a.cfg.QuotaBytes = u.Bytes + 1
			case "source-denied":
				e.mu.Lock()
				e.denySource = true
				e.mu.Unlock()
			case "callback-denied":
				e.pinHook = func(int, []Descriptor) error { return ErrDenied }
			case "callback-expired":
				e.pinHook = func(n int, _ []Descriptor) error {
					if n == 1 {
						e.clock.advance(11 * time.Second)
					}
					return nil
				}
			}
			if out, err := e.a.Pin(context.Background(), PinRequest{ClaimEnvelope: claim, Inputs: refs, RetainUntilUnixMS: until}); err == nil || len(out.Inputs) != 0 {
				t.Fatal("failed pin acknowledged")
			}
			e.a.cfg.QuotaBytes = e.cfg.QuotaBytes
			if err := e.a.db.View(func(tx *bolt.Tx) error {
				if tx.Bucket(pinBucket).Stats().KeyN != 0 || tx.Bucket(pinWaterBucket).Stats().KeyN != 0 {
					t.Fatal("partial pin survived refusal")
				}
				d, err := e.a.readProof(tx, one)
				if err != nil {
					return err
				}
				if d.RetainUntilUnixMS != before.RetainUntilUnixMS {
					t.Fatal("failed pin extended one input")
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestPinIsImmutableAndOldPinnedInputDoesNotQualifyForNewClaim(t *testing.T) {
	e := setup(t)
	source := remember(t, e, e.raw(t, false, "gas"))
	claim := e.raw(t, true, "claim", source)
	until := e.until(time.Minute)
	request := PinRequest{ClaimEnvelope: claim, Inputs: []Reference{source}, RetainUntilUnixMS: until}
	first, err := e.a.Pin(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	e.clock.advance(11 * time.Second)
	if retry, err := e.a.Pin(context.Background(), request); err != nil || !reflect.DeepEqual(retry, first) {
		t.Fatal("exact existing pin lost idempotence", err)
	}
	request.RetainUntilUnixMS++
	if _, err := e.a.Pin(context.Background(), request); !errors.Is(err, ErrConflict) {
		t.Fatal("pin deadline renewed", err)
	}
	newClaim := e.raw(t, true, "claim", source)
	if _, err := e.a.Pin(context.Background(), PinRequest{ClaimEnvelope: newClaim, Inputs: []Reference{source}, RetainUntilUnixMS: until}); !errors.Is(err, ErrUnavailable) {
		t.Fatal("retained old input reopened recent eligibility", err)
	}
	get(t, e, source)
}

func TestUnknownPinCommitAndPostCommitAuthorityFaultRequireReopen(t *testing.T) {
	for _, mode := range []string{"fsync-unknown", "post-clock", "post-authority"} {
		t.Run(mode, func(t *testing.T) {
			e := setup(t)
			source := remember(t, e, e.raw(t, false, "gas"))
			claim := e.raw(t, true, "claim", source)
			request := PinRequest{ClaimEnvelope: claim, Inputs: []Reference{source}, RetainUntilUnixMS: e.until(time.Minute)}
			if mode == "post-authority" {
				e.pinHook = func(n int, _ []Descriptor) error {
					if n == 3 {
						return ErrDenied
					}
					return nil
				}
			} else {
				original := e.a.commit
				e.a.commit = func(fn func(*bolt.Tx) error) error {
					err := original(fn)
					if err != nil {
						return err
					}
					if mode == "fsync-unknown" {
						return errors.New("injected ambiguous fsync")
					}
					e.clock.mu.Lock()
					e.clock.fail = true
					e.clock.mu.Unlock()
					return nil
				}
			}
			if out, err := e.a.Pin(context.Background(), request); !errors.Is(err, ErrUncertain) || len(out.Inputs) != 0 {
				t.Fatal("uncertain commit acknowledged", err)
			}
			if _, err := e.a.Get(context.Background(), source.Source(), source); !errors.Is(err, ErrFaulted) {
				t.Fatal("faulted archive kept serving", err)
			}
			e.clock.mu.Lock()
			e.clock.fail = false
			e.clock.mu.Unlock()
			e.pinHook = nil
			reopen(t, e)
			_, claimRef, err := e.a.decodeEnvelope(claim)
			if err != nil {
				t.Fatal(err)
			}
			if pin, err := e.a.CheckPin(context.Background(), claimRef); err != nil || pin.RetainUntilUnixMS != request.RetainUntilUnixMS {
				t.Fatal("reopen lost complete orphan pin", err)
			}
			if _, err := e.a.Get(context.Background(), claimRef.Source(), claimRef); !errors.Is(err, ErrUnavailable) {
				t.Fatal("orphan pin invented admitted claim proof", err)
			}
		})
	}
}

func TestExpiredReadPersistsFloorBeforeReopen(t *testing.T) {
	e := setup(t)
	ref := remember(t, e, e.raw(t, false, "gas"))
	old, _ := e.clock.Now()
	e.clock.advance(11 * time.Second)
	if _, err := e.a.Get(context.Background(), ref.Source(), ref); !errors.Is(err, ErrUnavailable) {
		t.Fatal("expired record readable", err)
	}
	if err := e.a.Close(); err != nil {
		t.Fatal(err)
	}
	e.clock.mu.Lock()
	e.clock.w = old
	e.clock.w.BootID = "new-boot"
	e.clock.w.Monotonic = 0
	e.clock.mu.Unlock()
	if a, err := Open(e.cfg); err == nil {
		a.Close()
		t.Fatal("failed expired read allowed durable clock rollback")
	}
}

func TestRememberFinalAuthorityCheckCannotAcknowledgeExpiredRetention(t *testing.T) {
	e := setup(t)
	raw := e.raw(t, false, "gas")
	var once sync.Once
	original := e.a.commit
	e.a.commit = func(fn func(*bolt.Tx) error) error {
		err := original(fn)
		if err == nil {
			e.mu.Lock()
			e.authHook = func() { once.Do(func() { e.clock.advance(11 * time.Second) }) }
			e.mu.Unlock()
		}
		return err
	}
	if ref, err := e.a.RememberAccepted(context.Background(), raw); !errors.Is(err, ErrUncertain) || ref != (Reference{}) {
		t.Fatal("final authority wait outlived retained source bytes", err)
	}
	e.mu.Lock()
	e.authHook = nil
	e.mu.Unlock()
	reopen(t, e)
	_, ref, err := e.a.decodeEnvelope(raw)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = e.a.Get(context.Background(), ref.Source(), ref); !errors.Is(err, ErrUnavailable) {
		t.Fatal("expired retained proof became readable after reopen", err)
	}
}

func TestLatePinCannotReviveExpiredRememberedClaim(t *testing.T) {
	for _, prune := range []bool{false, true} {
		t.Run(map[bool]string{false: "retained-row", true: "pruned-row"}[prune], func(t *testing.T) {
			e := setup(t)
			// Delaying an input's archival admission makes its storage window
			// newer than this already remembered claim, isolating claim expiry.
			sourceRaw := e.raw(t, false, "gas")
			_, source, err := e.a.decodeEnvelope(sourceRaw)
			if err != nil {
				t.Fatal(err)
			}
			oldClaim := e.raw(t, true, "late", source)
			oldRef := remember(t, e, oldClaim)
			e.clock.advance(5 * time.Second)
			remember(t, e, sourceRaw)
			e.clock.advance(6 * time.Second)
			if prune {
				if err := e.a.Prune(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			if get(t, e, source).RecentUntilUnixMS <= e.until(0) {
				t.Fatal("input is not independently eligible")
			}
			if _, err := e.a.Pin(context.Background(), PinRequest{ClaimEnvelope: oldClaim, Inputs: []Reference{source}, RetainUntilUnixMS: e.until(time.Minute)}); !errors.Is(err, ErrUnavailable) {
				t.Fatal("late pin revived expired claim", err)
			}
			if _, err := e.a.Get(context.Background(), oldRef.Source(), oldRef); !errors.Is(err, ErrUnavailable) {
				t.Fatal("expired claim became readable", err)
			}
		})
	}
}

func TestCorruptOrMissingStorageNeverReinitializes(t *testing.T) {
	for _, mode := range []string{"envelope", "metadata", "missing-marker", "missing-database", "changed-config", "symlink-parent"} {
		t.Run(mode, func(t *testing.T) {
			e := setup(t)
			ref := remember(t, e, e.raw(t, false, "gas"))
			if mode == "envelope" || mode == "metadata" {
				if err := e.a.db.Update(func(tx *bolt.Tx) error {
					if mode == "metadata" {
						return tx.Bucket(metaBucket).Put(usageKey, []byte("broken"))
					}
					var d Descriptor
					if err := unwrap(tx.Bucket(proofBucket).Get([]byte(ref.EnvelopeSHA256)), &d, 1<<20); err != nil {
						return err
					}
					d.Envelope[0] = 'x'
					return put(tx.Bucket(proofBucket), []byte(ref.EnvelopeSHA256), d)
				}); err != nil {
					t.Fatal(err)
				}
			}
			if err := e.a.Close(); err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "missing-marker":
				if err := os.Remove(e.cfg.Path + ".installation"); err != nil {
					t.Fatal(err)
				}
			case "missing-database":
				if err := os.Remove(e.cfg.Path); err != nil {
					t.Fatal(err)
				}
			case "changed-config":
				e.cfg.Site = "other"
			case "symlink-parent":
				actual := filepath.Dir(e.cfg.Path)
				link := actual + "-link"
				if err := os.Symlink(actual, link); err != nil {
					t.Fatal(err)
				}
				e.cfg.Path = filepath.Join(link, "proofs.db")
			}
			if a, err := Open(e.cfg); err == nil {
				a.Close()
				t.Fatal("damaged history opened")
			}
			if a, err := Create(e.cfg); err == nil {
				a.Close()
				t.Fatal("damaged history silently recreated")
			}
		})
	}
}

func TestCallbackCannotMutateStoredEnvelopeAndPinQuotaIsBounded(t *testing.T) {
	e := setup(t, func(c *Config) { c.MaxPins = 1 })
	raw := e.raw(t, false, "gas")
	source := remember(t, e, raw)
	e.pinHook = func(_ int, inputs []Descriptor) error {
		inputs[0].Envelope[0] = 'x'
		inputs[0].Reference.Key.Entity = "forged"
		return nil
	}
	claim := e.raw(t, true, "claim", source)
	if _, err := e.a.Pin(context.Background(), PinRequest{ClaimEnvelope: claim, Inputs: []Reference{source}, RetainUntilUnixMS: e.until(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(get(t, e, source).Envelope, raw) {
		t.Fatal("callback rewrote original proof")
	}
	if _, err := e.a.Pin(context.Background(), PinRequest{ClaimEnvelope: e.raw(t, true, "other", source), Inputs: []Reference{source}, RetainUntilUnixMS: e.until(time.Minute)}); !errors.Is(err, ErrQuota) {
		t.Fatal("pin count unbounded", err)
	}
}
