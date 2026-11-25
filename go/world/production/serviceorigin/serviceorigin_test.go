package serviceorigin

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"github.com/wendylabsinc/wendy/go/world/production/internal/testidentity"
	"github.com/wendylabsinc/wendy/go/world/production/trustedtime"
	"github.com/wendylabsinc/wendy/go/world/production/wendycert"
)

type testClock struct {
	mu    sync.Mutex
	w     trustedtime.Interval
	err   error
	calls int
	hook  func(*testClock)
}

func (c *testClock) Now() (trustedtime.Interval, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	if c.hook != nil {
		c.hook(c)
	}
	return c.w, c.err
}
func (c *testClock) advance(d time.Duration) { c.mu.Lock(); defer c.mu.Unlock(); c.shift(d) }
func (c *testClock) shift(d time.Duration) {
	c.w.Earliest = c.w.Earliest.Add(d)
	c.w.Latest = c.w.Latest.Add(d)
	c.w.Monotonic += d
}

type environment struct {
	cfg       PublisherConfig
	publisher *Publisher
	verifier  *Verifier
	clock     *testClock
	request   PublishRequest
	fixture   *testidentity.Fixture
}

func setup(t *testing.T) *environment {
	t.Helper()
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	f := testidentity.NewWith(t, now, "spiffe://wendy.sh/tenant/"+testidentity.Tenant+"/device/22222222-2222-4222-8222-222222222222")
	certs, err := wendycert.NewDevice(wendycert.Config{Tenant: f.Tenant, Roots: [][]byte{f.RootDER}})
	if err != nil {
		t.Fatal(err)
	}
	clock := &testClock{w: trustedtime.Interval{Earliest: now, Latest: now.Add(time.Millisecond), BootID: "test-boot", Monotonic: time.Second, Domain: "physical", Authenticated: true, Source: "test-authenticated-anchor"}}
	scope := Scope{Tenant: f.Tenant, Site: "office", Device: f.Principal, App: "com.example.camera", Service: "detector", DeploymentGeneration: 7, ExecutionDomain: "simulation", Publish: []PublishScope{{ComponentType: "wendy.pose.v1", Stream: "people"}}}
	cfg := PublisherConfig{Scope: scope, Epoch: 19, Lifetime: 10 * time.Minute, Clock: clock, Certificates: certs, ChainDER: f.ChainDER, DeviceKey: f.Key}
	p, err := NewPublisher(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close() })
	allow := func(_ context.Context, claims DelegationClaims, id wendycert.DeviceIdentity) error {
		if !reflect.DeepEqual(claims.Scope, scope) || claims.Epoch != cfg.Epoch || id.Principal != f.Principal {
			return ErrDenied
		}
		return nil
	}
	v, err := NewVerifier(VerifierConfig{Tenant: scope.Tenant, Site: scope.Site, ExecutionDomain: scope.ExecutionDomain, Certificates: certs, Clock: clock, Authorize: allow})
	if err != nil {
		t.Fatal(err)
	}
	request := PublishRequest{Entity: "person-1", ComponentType: "wendy.pose.v1", Stream: "people", Frame: world.FrameRef{ID: "office-floor", Revision: "revision-1"}, Timing: world.Timing{SampleTime: time.Unix(10, 123).UTC(), ClockDomain: "simulation:run-1", ErrorKnown: true, AgeHint: time.Millisecond}, TTL: 2 * time.Second, Provenance: world.Provenance{Origin: world.Simulated, AppVersion: "v1", Simulation: &world.SimulationContext{RunID: "run-1", ScenarioVersion: "scenario-1", Seed: 42, ClockDomain: "simulation:run-1"}}, Payload: []byte("{ \"position\" : [1,2,3], \"note\": \"原本\" }\n")}
	return &environment{cfg: cfg, publisher: p, verifier: v, clock: clock, request: request, fixture: f}
}
func (e *environment) publish(t *testing.T) SignedRecord {
	t.Helper()
	signed, err := e.publisher.Publish(context.Background(), e.request)
	if err != nil {
		t.Fatal(err)
	}
	return signed
}

func TestExactPublicationBytesIdentityAndProvenance(t *testing.T) {
	e := setup(t)
	signed := e.publish(t)
	wire, err := json.Marshal(signed)
	if err != nil {
		t.Fatal(err)
	}
	var transferred SignedRecord
	if err = json.Unmarshal(wire, &transferred); err != nil {
		t.Fatal(err)
	}
	got, err := e.verifier.Verify(context.Background(), transferred)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.Bytes, signed.Record) || !bytes.Equal(got.Record.Publication.Payload, e.request.Payload) || !reflect.DeepEqual(got.Record.Publication, e.request) {
		t.Fatal("changed immutable publication or source provenance")
	}
	if got.Record.Scope.Device != e.fixture.Principal || got.Record.Scope.DeploymentGeneration != 7 || got.Record.Epoch != 19 || got.Record.Sequence != 1 || got.Record.AuthorityDomain != "physical" || got.Record.Scope.ExecutionDomain != "simulation" {
		t.Fatal("wrong authenticated source/domain")
	}
	c := got.Component()
	if c.Key.Source.Device != e.fixture.Principal || c.Key.Source.App != e.cfg.Scope.App || c.Key.Source.Service != "detector" || c.Version.Epoch != 19 || !bytes.Equal(c.Payload, e.request.Payload) || c.Provenance.Origin != world.Simulated || !c.Timing.SampleTime.Equal(e.request.Timing.SampleTime) {
		t.Fatal("component projection rewrote origin")
	}
	c.Payload[0] = 'x'
	c.Provenance.Simulation.RunID = "changed"
	if got.Record.Publication.Payload[0] == 'x' || got.Record.Publication.Provenance.Simulation.RunID == "changed" {
		t.Fatal("projection shares mutable slices/pointers")
	}
	signed.Record[0] = 'x'
	signed.Delegation.Claims[0] = 'x'
	signed.Delegation.ChainDER[0][0] ^= 1
	second := e.publish(t)
	if _, err = e.verifier.Verify(context.Background(), second); err != nil {
		t.Fatalf("caller modified publisher state: %v", err)
	}
}

func TestScopeAndPayloadRejectionsDoNotGrantExtraAuthority(t *testing.T) {
	e := setup(t)
	for name, change := range map[string]func(*PublishRequest){
		"stream":                 func(p *PublishRequest) { p.Stream = "other" },
		"type":                   func(p *PublishRequest) { p.ComponentType = "wendy.task.v1" },
		"duplicate payload key":  func(p *PublishRequest) { p.Payload = []byte(`{"value":1,"value":2}`) },
		"payload Unicode":        func(p *PublishRequest) { p.Payload = []byte{'{', '"', 'x', '"', ':', '"', 0xff, '"', '}'} },
		"physical ancestry":      func(p *PublishRequest) { p.Provenance = world.Provenance{Origin: world.Measured, AppVersion: "v1"} },
		"wrong simulation clock": func(p *PublishRequest) { p.Timing.ClockDomain = "physical" },
		"tombstone payload":      func(p *PublishRequest) { p.Tombstone = true },
	} {
		t.Run(name, func(t *testing.T) {
			p := cloneRequest(e.request)
			change(&p)
			if _, err := e.publisher.Publish(context.Background(), p); err == nil {
				t.Fatal("invalid publication signed")
			}
		})
	}
	if e.publisher.Err() != nil {
		t.Fatal("an invalid app request faulted the publisher")
	}
	for name, change := range map[string]func(*PublisherConfig){
		"wildcard":           func(c *PublisherConfig) { c.Scope.Publish[0].Stream = "*" },
		"missing epoch":      func(c *PublisherConfig) { c.Epoch = 0 },
		"missing generation": func(c *PublisherConfig) { c.Scope.DeploymentGeneration = 0 },
		"wrong device": func(c *PublisherConfig) {
			c.Scope.Device = "spiffe://wendy.sh/tenant/" + c.Scope.Tenant + "/device/33333333-3333-4333-8333-333333333333"
		},
		"long key": func(c *PublisherConfig) { c.Lifetime = MaxLifetime + time.Millisecond },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := e.cfg
			cfg.Scope = cloneScope(cfg.Scope)
			change(&cfg)
			if p, err := NewPublisher(cfg); err == nil {
				p.Close()
				t.Fatal("invalid delegation created")
			}
		})
	}
}

func TestTamperedProofsAndProperlySignedScopeMismatch(t *testing.T) {
	e := setup(t)
	original := e.publish(t)
	for name, change := range map[string]func(*SignedRecord){
		"record bytes":     func(s *SignedRecord) { s.Record[len(s.Record)-2] ^= 1 },
		"record signature": func(s *SignedRecord) { s.Signature[0] ^= 1 },
		"delegation":       func(s *SignedRecord) { s.Delegation.Claims[5] ^= 1 },
		"device signature": func(s *SignedRecord) { s.Delegation.Signature[0] ^= 1 },
		"missing chain":    func(s *SignedRecord) { s.Delegation.ChainDER = nil },
		"duplicate chain":  func(s *SignedRecord) { s.Delegation.ChainDER = append(s.Delegation.ChainDER, s.Delegation.ChainDER[0]) },
	} {
		t.Run(name, func(t *testing.T) {
			s := SignedRecord{Delegation: cloneDelegation(original.Delegation), Record: bytes.Clone(original.Record), Signature: bytes.Clone(original.Signature)}
			change(&s)
			if _, err := e.verifier.Verify(context.Background(), s); err == nil {
				t.Fatal("tampered proof accepted")
			}
		})
	}
	for name, change := range map[string]func(*Record){
		"service":         func(r *Record) { r.Scope.Service = "sibling" },
		"generation":      func(r *Record) { r.Scope.DeploymentGeneration++ },
		"epoch":           func(r *Record) { r.Epoch++ },
		"scope":           func(r *Record) { r.Publication.Stream = "admin" },
		"extended expiry": func(r *Record) { r.ExpiresAtUnixMS++ },
		"zero sequence":   func(r *Record) { r.Sequence = 0 },
		"domain":          func(r *Record) { r.AuthorityDomain = "simulation" },
	} {
		t.Run(name, func(t *testing.T) {
			var r Record
			if err := json.Unmarshal(original.Record, &r); err != nil {
				t.Fatal(err)
			}
			change(&r)
			raw, err := json.Marshal(r)
			if err != nil {
				t.Fatal(err)
			}
			s := SignedRecord{Delegation: original.Delegation, Record: raw, Signature: ed25519.Sign(e.publisher.key, append([]byte(recordPrefix), raw...))}
			if _, err = e.verifier.Verify(context.Background(), s); err == nil {
				t.Fatal("delegated key escaped certified scope")
			}
		})
	}
}

func TestMandatoryCommissionedAuthorityAndFinalTimeCheck(t *testing.T) {
	e := setup(t)
	signed := e.publish(t)
	cfg := e.verifier.cfg
	cfg.Authorize = nil
	if _, err := NewVerifier(cfg); err == nil {
		t.Fatal("certificate enrollment granted site access")
	}
	cfg.Authorize = func(context.Context, DelegationClaims, wendycert.DeviceIdentity) error {
		return errors.New("revoked service")
	}
	v, _ := NewVerifier(cfg)
	if _, err := v.Verify(context.Background(), signed); err == nil {
		t.Fatal("revocation ignored")
	}
	calls := 0
	cfg.Authorize = func(_ context.Context, claims DelegationClaims, _ wendycert.DeviceIdentity) error {
		calls++
		claims.Scope.Publish[0].Stream = "mutated"
		claims.PublicKey[0] ^= 1
		if calls == 2 {
			e.clock.advance(11 * time.Minute)
		}
		return nil
	}
	v, _ = NewVerifier(cfg)
	if _, err := v.Verify(context.Background(), signed); !errors.Is(err, ErrExpired) {
		t.Fatalf("authority delay did not expire proof: %v", err)
	}
	if calls != 2 {
		t.Fatalf("authorization calls = %d", calls)
	}
}

func TestPublisherClockFaultsCloseKeyAndNeverResume(t *testing.T) {
	for _, mode := range []string{"rollback", "boot", "untrusted", "expired", "post-sign expiry"} {
		t.Run(mode, func(t *testing.T) {
			e := setup(t)
			e.clock.mu.Lock()
			switch mode {
			case "rollback":
				e.clock.shift(-time.Millisecond)
			case "boot":
				e.clock.w.BootID = "another-boot"
			case "untrusted":
				e.clock.w.Authenticated = false
			case "expired":
				e.clock.shift(11 * time.Minute)
			case "post-sign expiry":
				target := e.clock.calls + 2
				e.clock.hook = func(c *testClock) {
					if c.calls == target {
						c.shift(11 * time.Minute)
					}
				}
			}
			e.clock.mu.Unlock()
			if _, err := e.publisher.Publish(context.Background(), e.request); err == nil {
				t.Fatal("clock fault signed a publication")
			}
			select {
			case <-e.publisher.Done():
			default:
				t.Fatal("fault did not signal invalidation")
			}
			if len(e.publisher.key) != 0 || e.publisher.Err() == nil {
				t.Fatal("fault retained signing authority")
			}
			if _, err := e.publisher.Publish(context.Background(), e.request); err == nil {
				t.Fatal("faulted publisher resumed")
			}
		})
	}
}

func TestCloseCancellationAndConcurrentSequences(t *testing.T) {
	e := setup(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := e.publisher.Publish(ctx, e.request); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	const count = 12
	sequences := make(chan uint64, count)
	errs := make(chan error, count)
	var wg sync.WaitGroup
	jobs := make(chan struct{}, count)
	for i := 0; i < count; i++ {
		jobs <- struct{}{}
	}
	close(jobs)
	worker := func() {
		defer wg.Done()
		for range jobs {
			s, err := e.publisher.Publish(context.Background(), e.request)
			if err != nil {
				errs <- err
				continue
			}
			var r Record
			err = json.Unmarshal(s.Record, &r)
			if err != nil {
				errs <- err
				continue
			}
			sequences <- r.Sequence
		}
	}
	wg.Add(3)
	go worker()
	go worker()
	go worker()
	wg.Wait()
	close(sequences)
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	seen := map[uint64]bool{}
	for s := range sequences {
		if s == 0 || seen[s] {
			t.Fatal("sequence reused")
		}
		seen[s] = true
	}
	if len(seen) != count {
		t.Fatal("missing publication")
	}
	key := e.publisher.key
	e.publisher.Close()
	e.publisher.Close()
	for _, b := range key {
		if b != 0 {
			t.Fatal("closed publisher retained private key bytes")
		}
	}
	if _, err := e.publisher.Publish(context.Background(), e.request); !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}
	select {
	case <-e.publisher.Done():
	default:
		t.Fatal("close not signaled")
	}
}

func TestPhysicalPublicationCannotPromoteSimulationAncestry(t *testing.T) {
	e := setup(t)
	cfg := e.cfg
	cfg.Scope = cloneScope(cfg.Scope)
	cfg.Scope.ExecutionDomain = "physical"
	p, err := NewPublisher(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if _, err = p.Publish(context.Background(), e.request); err == nil {
		t.Fatal("promoted simulated data")
	}
	request := cloneRequest(e.request)
	request.Timing.ClockDomain = "physical"
	request.Provenance = world.Provenance{Origin: world.Measured, AppVersion: "v1"}
	if _, err = p.Publish(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	request.Provenance = world.Provenance{Origin: world.Inferred, AppVersion: "v1", ModelID: "fusion", ModelVersion: "1", Inputs: []world.Input{{ID: "simulated-source", Origin: world.Simulated, Simulation: e.request.Provenance.Simulation}}}
	if _, err = p.Publish(context.Background(), request); err == nil {
		t.Fatal("promoted inferred simulation ancestry")
	}
}
