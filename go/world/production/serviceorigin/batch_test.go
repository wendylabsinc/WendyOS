package serviceorigin

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/production/trustedtime"
	"github.com/wendylabsinc/wendy/go/world/production/wendycert"
)

func batchConfig(e *environment, auth AuthorizeBatch) BatchVerifierConfig {
	return BatchVerifierConfig{Tenant: e.cfg.Scope.Tenant, Site: e.cfg.Scope.Site, ExecutionDomain: e.cfg.Scope.ExecutionDomain, Certificates: e.cfg.Certificates, Clock: e.clock, Authorize: auth}
}

func copySigned(s SignedRecord) SignedRecord {
	return SignedRecord{Delegation: cloneDelegation(s.Delegation), Record: bytes.Clone(s.Record), Signature: bytes.Clone(s.Signature)}
}

func recordSize(s SignedRecord) int {
	n := len(s.Record) + len(s.Signature) + len(s.Delegation.Claims) + len(s.Delegation.Signature)
	for _, der := range s.Delegation.ChainDER {
		n += len(der)
	}
	return n
}

func signedMutation(t *testing.T, e *environment, s SignedRecord, mutate func(*Record)) SignedRecord {
	t.Helper()
	s = copySigned(s)
	var r Record
	if err := json.Unmarshal(s.Record, &r); err != nil {
		t.Fatal(err)
	}
	mutate(&r)
	var err error
	s.Record, err = json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	s.Signature = ed25519.Sign(e.publisher.key, append([]byte(recordPrefix), s.Record...))
	return s
}

func TestBatchOwnsOriginalsAndChecksWholeSetTwice(t *testing.T) {
	e := setup(t)
	input := []SignedRecord{e.publish(t), e.publish(t), e.publish(t), e.publish(t)}
	original := make([]SignedRecord, len(input))
	want := make([]VerifiedRecord, len(input))
	for i := range input {
		original[i] = copySigned(input[i])
		var err error
		want[i], err = e.verifier.Verify(context.Background(), input[i])
		if err != nil {
			t.Fatal(err)
		}
	}
	calls := 0
	clockCalls := e.clock.calls
	v, err := NewBatchVerifier(batchConfig(e, func(_ context.Context, auth BatchAuthorization) error {
		calls++
		if len(auth.Claims) != len(input) || len(auth.Devices) != len(input) {
			t.Fatal("authorizer received partial set")
		}
		for i := range auth.Claims {
			if !reflect.DeepEqual(auth.Claims[i], want[i].Delegation) || auth.Devices[i].Principal != want[i].Device.Principal || auth.Devices[i].CertificateSHA256 != want[i].Device.CertificateSHA256 {
				t.Fatal("callback input changed or reordered")
			}
			auth.Claims[i].Scope.Publish[0].Stream = "mutated"
			auth.Claims[i].PublicKey[0] ^= 1
			auth.Devices[i].Principal = "mutated"
		}
		if calls == 1 {
			// Caller-owned bytes may change after ownership transfers. This is
			// synchronous, not an unsupported concurrent caller data race.
			for i := range input {
				input[i].Record[0] ^= 1
				input[i].Signature[0] ^= 1
				input[i].Delegation.Claims[0] ^= 1
				input[i].Delegation.Signature[0] ^= 1
				input[i].Delegation.ChainDER[0][0] ^= 1
			}
		}
		return nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	got, err := v.VerifyBatch(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 || e.clock.calls-clockCalls != 3 || len(got) != 4 {
		t.Fatalf("auth=%d clocks=%d results=%d", calls, e.clock.calls-clockCalls, len(got))
	}
	for i := range got {
		if !bytes.Equal(got[i].Bytes, original[i].Record) || !reflect.DeepEqual(got[i].Record, want[i].Record) || !reflect.DeepEqual(got[i].Delegation, want[i].Delegation) || !reflect.DeepEqual(got[i].Component(), want[i].Component()) {
			t.Fatal("batch changed immutable bytes, version, timing or provenance")
		}
	}
	got[0].Delegation.PublicKey[0] ^= 1
	got[0].Record.Scope.Publish[0].Stream = "mutated"
	got[0].Record.Publication.Provenance.Simulation.RunID = "mutated"
	if !reflect.DeepEqual(got[1].Delegation, want[1].Delegation) || !reflect.DeepEqual(got[1].Record, want[1].Record) {
		t.Fatal("results share mutable projections")
	}
}

func TestBatchKeepsDistinctServiceDelegationsBound(t *testing.T) {
	e := setup(t)
	names := []string{"north-camera", "south-camera", "gas", "audio"}
	input := make([]SignedRecord, len(names))
	scopes := make([]Scope, len(names))
	for i, name := range names {
		cfg := e.cfg
		cfg.Scope = cloneScope(cfg.Scope)
		cfg.Scope.Service = name
		cfg.Scope.DeploymentGeneration += uint64(i)
		cfg.Epoch += uint64(i)
		p, err := NewPublisher(cfg)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { p.Close() })
		input[i], err = p.Publish(context.Background(), e.request)
		if err != nil {
			t.Fatal(err)
		}
		scopes[i] = cfg.Scope
	}
	calls := 0
	cfg := batchConfig(e, func(_ context.Context, auth BatchAuthorization) error {
		calls++
		if len(auth.Claims) != len(names) || len(auth.Devices) != len(names) {
			return ErrDenied
		}
		for i, claims := range auth.Claims {
			if !reflect.DeepEqual(claims.Scope, scopes[i]) || claims.Epoch != e.cfg.Epoch+uint64(i) || auth.Devices[i].Principal != scopes[i].Device {
				return ErrDenied
			}
		}
		return nil
	})
	cfg.MaxRecords = 4
	v, err := NewBatchVerifier(cfg)
	if err != nil {
		t.Fatal(err)
	}
	got, err := v.VerifyBatch(context.Background(), input)
	if err != nil || len(got) != len(names) || calls != 2 {
		t.Fatalf("complete commissioned services not verified: %v", err)
	}
	for i, r := range got {
		if r.Component().Key.Source.Service != names[i] || !reflect.DeepEqual(r.Record.Scope, scopes[i]) || r.Record.Epoch != e.cfg.Epoch+uint64(i) || !bytes.Equal(r.Bytes, input[i].Record) {
			t.Fatal("batch confused original service, generation, epoch or bytes")
		}
	}
}

func TestBatchRejectsEveryInvalidOriginalBeforeAuthorization(t *testing.T) {
	e := setup(t)
	first, last := e.publish(t), e.publish(t)
	for name, change := range map[string]func(SignedRecord) SignedRecord{
		"record signature":          func(s SignedRecord) SignedRecord { s.Signature[0] ^= 1; return s },
		"device signature":          func(s SignedRecord) SignedRecord { s.Delegation.Signature[0] ^= 1; return s },
		"claims signature mismatch": func(s SignedRecord) SignedRecord { s.Delegation.Claims[5] ^= 1; return s },
		"wrong root":                func(s SignedRecord) SignedRecord { s.Delegation.ChainDER[0][0] ^= 1; return s },
		"signed scope mismatch": func(s SignedRecord) SignedRecord {
			return signedMutation(t, e, s, func(r *Record) { r.Scope.Service = "other" })
		},
		"signed future record": func(s SignedRecord) SignedRecord {
			return signedMutation(t, e, s, func(r *Record) { r.IssuedAtUnixMS += 1000 })
		},
		"signed extended expiry": func(s SignedRecord) SignedRecord {
			return signedMutation(t, e, s, func(r *Record) { r.ExpiresAtUnixMS++ })
		},
		"signed duplicate JSON field": func(s SignedRecord) SignedRecord {
			s.Record = bytes.Replace(s.Record, []byte(`"sequence":2`), []byte(`"sequence":2,"sequence":2`), 1)
			s.Signature = ed25519.Sign(e.publisher.key, append([]byte(recordPrefix), s.Record...))
			return s
		},
		"signed unknown JSON field": func(s SignedRecord) SignedRecord {
			s.Record = append([]byte(`{"extra":0,`), s.Record[1:]...)
			s.Signature = ed25519.Sign(e.publisher.key, append([]byte(recordPrefix), s.Record...))
			return s
		},
	} {
		t.Run(name, func(t *testing.T) {
			calls := 0
			v, err := NewBatchVerifier(batchConfig(e, func(context.Context, BatchAuthorization) error { calls++; return nil }))
			if err != nil {
				t.Fatal(err)
			}
			got, err := v.VerifyBatch(context.Background(), []SignedRecord{first, change(copySigned(last))})
			if err == nil || got != nil || calls != 0 {
				t.Fatalf("invalid later original exposed earlier trust: results=%d auth=%d err=%v", len(got), calls, err)
			}
		})
	}
}

func TestBatchRefusesDuplicateLogicalVersionAndConflictingProof(t *testing.T) {
	e := setup(t)
	original := e.publish(t)
	conflict := signedMutation(t, e, original, func(r *Record) { r.Publication.Payload = []byte(`{"other":true}`) })
	for name, second := range map[string]SignedRecord{"duplicate": copySigned(original), "conflict": conflict} {
		t.Run(name, func(t *testing.T) {
			calls := 0
			v, _ := NewBatchVerifier(batchConfig(e, func(context.Context, BatchAuthorization) error { calls++; return nil }))
			if got, err := v.VerifyBatch(context.Background(), []SignedRecord{original, second}); !errors.Is(err, ErrDenied) || got != nil || calls != 0 {
				t.Fatalf("duplicate full key/version accepted: %v", err)
			}
		})
	}
	// A generic batch can contain different versions; the fusion integration
	// separately requires its four commissioned, distinct capture sources.
	v, _ := NewBatchVerifier(batchConfig(e, func(context.Context, BatchAuthorization) error { return nil }))
	if got, err := v.VerifyBatch(context.Background(), []SignedRecord{original, e.publish(t)}); err != nil || len(got) != 2 {
		t.Fatalf("distinct versions refused: %v", err)
	}
}

func TestBatchBoundsFailBeforeClockOrAuthority(t *testing.T) {
	e := setup(t)
	original := e.publish(t)
	allow := func(context.Context, BatchAuthorization) error {
		t.Fatal("out-of-bounds input reached authority")
		return nil
	}
	for name, mutate := range map[string]func(*BatchVerifierConfig){
		"no authority":    func(c *BatchVerifierConfig) { c.Authorize = nil },
		"no clock":        func(c *BatchVerifierConfig) { c.Clock = nil },
		"no certificates": func(c *BatchVerifierConfig) { c.Certificates = nil },
		"bad tenant":      func(c *BatchVerifierConfig) { c.Tenant = "other" },
		"bad site":        func(c *BatchVerifierConfig) { c.Site = "" },
		"bad domain":      func(c *BatchVerifierConfig) { c.ExecutionDomain = "replay" },
		"negative count":  func(c *BatchVerifierConfig) { c.MaxRecords = -1 },
		"large count":     func(c *BatchVerifierConfig) { c.MaxRecords = MaxBatchRecords + 1 },
		"negative bytes":  func(c *BatchVerifierConfig) { c.MaxBytes = -1 },
		"large bytes":     func(c *BatchVerifierConfig) { c.MaxBytes = MaxBatchBytes + 1 },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := batchConfig(e, allow)
			mutate(&cfg)
			if _, err := NewBatchVerifier(cfg); !errors.Is(err, ErrDenied) {
				t.Fatalf("bad constructor accepted: %v", err)
			}
		})
	}
	for name, input := range map[string][]SignedRecord{
		"empty":         nil,
		"large count":   make([]SignedRecord, MaxBatchRecords+1),
		"record cap":    {{Delegation: original.Delegation, Record: make([]byte, MaxRecordBytes+1), Signature: original.Signature}},
		"claims cap":    {{Delegation: SignedDelegation{Claims: make([]byte, MaxClaimsBytes+1), Signature: original.Delegation.Signature, ChainDER: original.Delegation.ChainDER}, Record: original.Record, Signature: original.Signature}},
		"missing chain": {{Record: original.Record, Signature: original.Signature, Delegation: SignedDelegation{Claims: original.Delegation.Claims, Signature: original.Delegation.Signature}}},
	} {
		t.Run(name, func(t *testing.T) {
			v, _ := NewBatchVerifier(batchConfig(e, allow))
			before := e.clock.calls
			if got, err := v.VerifyBatch(context.Background(), input); !errors.Is(err, ErrDenied) || got != nil || e.clock.calls != before {
				t.Fatalf("unbounded input reached clock or returned trust: %v", err)
			}
		})
	}
	for name, cfg := range map[string]BatchVerifierConfig{
		"count": batchConfig(e, allow), "bytes": batchConfig(e, allow),
	} {
		t.Run(name, func(t *testing.T) {
			input := []SignedRecord{original}
			if name == "count" {
				cfg.MaxRecords = 1
				input = append(input, original)
			} else {
				cfg.MaxBytes = recordSize(original) - 1
			}
			v, _ := NewBatchVerifier(cfg)
			before := e.clock.calls
			if got, err := v.VerifyBatch(context.Background(), input); !errors.Is(err, ErrDenied) || got != nil || e.clock.calls != before {
				t.Fatalf("configured bound ignored: %v", err)
			}
		})
	}
	cfg := batchConfig(e, func(context.Context, BatchAuthorization) error { return nil })
	cfg.MaxBytes = recordSize(original)
	v, _ := NewBatchVerifier(cfg)
	if _, err := v.VerifyBatch(context.Background(), []SignedRecord{original}); err != nil {
		t.Fatalf("exact byte limit refused: %v", err)
	}
}

func TestBatchRechecksEarlierMemberAtBothFinalIntervals(t *testing.T) {
	for _, pass := range []int{1, 2} {
		t.Run(string(rune('0'+pass)), func(t *testing.T) {
			e := setup(t)
			first := e.publish(t)
			// The first member expires before the other. Expiry at either
			// whole-set boundary must refuse even that already processed member.
			first = signedMutation(t, e, first, func(r *Record) { r.ExpiresAtUnixMS = r.IssuedAtUnixMS + 1000 })
			last := e.publish(t)
			calls := 0
			v, _ := NewBatchVerifier(batchConfig(e, func(context.Context, BatchAuthorization) error {
				calls++
				if calls == pass {
					e.clock.advance(time.Second)
				}
				return nil
			}))
			if got, err := v.VerifyBatch(context.Background(), []SignedRecord{first, last}); !errors.Is(err, ErrExpired) || got != nil || calls != pass {
				t.Fatalf("expired early member escaped final interval %d: %v", pass, err)
			}
		})
	}
}

func TestBatchRevocationDuringFinalAuthorityIsAtomic(t *testing.T) {
	e := setup(t)
	input := []SignedRecord{e.publish(t), e.publish(t)}
	entered, release := make(chan struct{}), make(chan struct{})
	var revoked atomic.Bool
	calls := 0
	v, _ := NewBatchVerifier(batchConfig(e, func(ctx context.Context, auth BatchAuthorization) error {
		calls++
		if len(auth.Claims) != 2 || len(auth.Devices) != 2 {
			return ErrDenied
		}
		if calls == 2 {
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		if revoked.Load() {
			return ErrDenied
		}
		return nil
	}))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	type outcome struct {
		records []VerifiedRecord
		err     error
	}
	finished := make(chan outcome, 1)
	go func() { records, err := v.VerifyBatch(ctx, input); finished <- outcome{records, err} }()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("final authority callback was not reached")
	}
	revoked.Store(true)
	close(release)
	got := <-finished
	if !errors.Is(got.err, ErrDenied) || got.records != nil || calls != 2 {
		t.Fatalf("revoked member allowed partial batch: %v", got.err)
	}
	// Revocation is a current policy decision, not a verifier clock fault.
	revoked.Store(false)
	if got, err := v.VerifyBatch(context.Background(), input); err != nil || len(got) != 2 {
		t.Fatalf("policy refusal permanently faulted verifier: %v", err)
	}
}

func TestBatchCancellationAtEveryBoundaryReturnsNoPartialTrust(t *testing.T) {
	for _, phase := range []string{"before", "w0", "auth1", "w1", "auth2", "w2"} {
		t.Run(phase, func(t *testing.T) {
			e := setup(t)
			input := []SignedRecord{e.publish(t), e.publish(t)}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls, clocks := 0, 0
			e.clock.hook = func(*testClock) {
				clocks++
				if phase == "w0" && clocks == 1 || phase == "w1" && clocks == 2 || phase == "w2" && clocks == 3 {
					cancel()
				}
			}
			v, _ := NewBatchVerifier(batchConfig(e, func(context.Context, BatchAuthorization) error {
				calls++
				if phase == "auth1" && calls == 1 || phase == "auth2" && calls == 2 {
					cancel()
				}
				return nil
			}))
			if phase == "before" {
				cancel()
			}
			if got, err := v.VerifyBatch(ctx, input); !errors.Is(err, context.Canceled) || got != nil {
				t.Fatalf("cancellation at %s returned trust: %v", phase, err)
			}
			e.clock.hook = nil
			if got, err := v.VerifyBatch(context.Background(), input); err != nil || len(got) != 2 {
				t.Fatalf("cancellation poisoned verifier: %v", err)
			}
		})
	}
}

func TestBatchClockFaultsAreStickyAcrossCalls(t *testing.T) {
	for _, phase := range []int{1, 2, 3} {
		for _, fault := range []string{"boot", "untrusted", "regression", "provider"} {
			t.Run(string(rune('0'+phase))+"/"+fault, func(t *testing.T) {
				e := setup(t)
				input := []SignedRecord{e.publish(t), e.publish(t)}
				v, _ := NewBatchVerifier(batchConfig(e, func(context.Context, BatchAuthorization) error { return nil }))
				if _, err := v.VerifyBatch(context.Background(), input); err != nil {
					t.Fatal(err)
				}
				initial := e.clock.w
				clocks := 0
				e.clock.hook = func(c *testClock) {
					clocks++
					if clocks != phase {
						return
					}
					switch fault {
					case "boot":
						c.w.BootID = "changed"
					case "untrusted":
						c.w.Authenticated = false
					case "regression":
						c.shift(-time.Millisecond)
					case "provider":
						c.err = errors.New("time unavailable")
					}
				}
				if got, err := v.VerifyBatch(context.Background(), input); !errors.Is(err, ErrClock) || got != nil {
					t.Fatalf("clock fault accepted: %v", err)
				}
				e.clock.hook, e.clock.err, e.clock.w = nil, nil, initial
				before := e.clock.calls
				if got, err := v.VerifyBatch(context.Background(), input); !errors.Is(err, ErrClock) || got != nil || e.clock.calls != before {
					t.Fatalf("faulted verifier resumed: %v", err)
				}
			})
		}
	}
}

func TestOneRecordBatchMatchesSingleVerifierBoundaries(t *testing.T) {
	e := setup(t)
	original := e.publish(t)
	for _, mode := range []string{"valid", "invalid signature", "wrong site", "before issued", "expiry at w0", "expiry at w1", "expiry at w2", "auth1", "auth2", "canceled", "untrusted", "rollback"} {
		t.Run(mode, func(t *testing.T) {
			type result struct {
				record       VerifiedRecord
				err          error
				clocks, auth int
			}
			run := func(batch bool) result {
				input := copySigned(original)
				clock := &testClock{w: e.clock.w}
				clock.hook = func(c *testClock) {
					if mode == "expiry at w0" && c.calls == 1 || mode == "expiry at w1" && c.calls == 2 || mode == "expiry at w2" && c.calls == 3 {
						c.shift(11 * time.Minute)
					}
					if mode == "before issued" && c.calls == 1 {
						c.shift(-time.Second)
					}
					if mode == "untrusted" && c.calls == 2 {
						c.w.Authenticated = false
					}
					if mode == "rollback" && c.calls == 3 {
						c.shift(-time.Millisecond)
					}
				}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				calls := 0
				auth := func() error {
					calls++
					if mode == "auth1" && calls == 1 || mode == "auth2" && calls == 2 {
						return ErrDenied
					}
					if mode == "canceled" && calls == 2 {
						cancel()
					}
					return nil
				}
				if mode == "invalid signature" {
					input.Signature[0] ^= 1
				}
				site := e.cfg.Scope.Site
				if mode == "wrong site" {
					site = "other"
				}
				var got VerifiedRecord
				var err error
				if batch {
					cfg := batchConfig(e, func(context.Context, BatchAuthorization) error { return auth() })
					cfg.Clock, cfg.Site = clock, site
					v, constructorErr := NewBatchVerifier(cfg)
					if constructorErr != nil {
						t.Fatal(constructorErr)
					}
					var records []VerifiedRecord
					records, err = v.VerifyBatch(ctx, []SignedRecord{input})
					if err == nil {
						got = records[0]
					} else if records != nil {
						t.Fatal("partial result")
					}
				} else {
					v, constructorErr := NewVerifier(VerifierConfig{Tenant: e.cfg.Scope.Tenant, Site: site, ExecutionDomain: e.cfg.Scope.ExecutionDomain, Certificates: e.cfg.Certificates, Clock: clock, Authorize: func(context.Context, DelegationClaims, wendycert.DeviceIdentity) error { return auth() }})
					if constructorErr != nil {
						t.Fatal(constructorErr)
					}
					got, err = v.Verify(ctx, input)
				}
				return result{got, err, clock.calls, calls}
			}
			single, batch := run(false), run(true)
			if !errors.Is(batch.err, single.err) || !errors.Is(single.err, batch.err) || batch.clocks != single.clocks || batch.auth != single.auth {
				t.Fatalf("boundary mismatch single=(%v,%d,%d) batch=(%v,%d,%d)", single.err, single.clocks, single.auth, batch.err, batch.clocks, batch.auth)
			}
			if batch.err == nil && (!bytes.Equal(single.record.Bytes, batch.record.Bytes) || !reflect.DeepEqual(single.record.Component(), batch.record.Component())) {
				t.Fatal("one-item batch changed original publication")
			}
		})
	}
}

// Keep the provider contract explicit in tests; no wall/simulation clock is
// substituted to make a completed batch reusable.
var _ trustedtime.Provider = (*testClock)(nil)
