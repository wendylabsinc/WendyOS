package authority

import (
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"github.com/wendylabsinc/wendy/go/world/internal/annotationclock"
	"github.com/wendylabsinc/wendy/go/world/internal/annotations"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"strings"
	"testing"
	"time"
)

func clockFixture(t *testing.T) (*fixture, SignedGrant, ed25519.PrivateKey, annotationclock.SignedRequest, annotationclock.SignedObservation, annotationclock.Pins) {
	t.Helper()
	f := newFixture(t, nil)
	g := f.grant.Grant
	g.Kind = "operator"
	g.Scopes = []string{"read", "annotation-write"}
	f.grant, _ = SignGrant(g, f.root)
	key := ed25519.NewKeyFromSeed(make([]byte, 32))
	sg := g
	sg.Kind = "device"
	sg.ID = "clock"
	sg.Principal = "gateway-clock"
	sg.PublicKey = key.Public().(ed25519.PublicKey)
	sg.Scopes = []string{"annotation-clock-server"}
	server, err := SignGrant(sg, f.root)
	if err != nil {
		t.Fatal(err)
	}
	req, err := SignAnnotationClockRequest(f.grant, f.key, annotationclock.Request{Tenant: g.Tenant, Site: g.Site, Audience: g.PublishAudience, RunID: "run", ClockDomain: "simulation:run", Nonce: strings.Repeat("ab", 32)})
	if err != nil {
		t.Fatal(err)
	}
	obs, err := f.verifier.IssueAnnotationClock(req, server, key, "run", "simulation:run", strings.Repeat("cd", 32))
	if err != nil {
		t.Fatal(err)
	}
	pins := annotationclock.Pins{RootPublicKey: f.root.Public().(ed25519.PublicKey), GatewayPrincipal: sg.Principal, GatewayKeySHA256: KeyID(sg.PublicKey), RunID: "run", ClockDomain: "simulation:run"}
	return f, server, key, req, obs, pins
}
func clockRecord(t *testing.T, f *fixture, req annotationclock.SignedRequest, obs annotationclock.SignedObservation) SignedRecord {
	t.Helper()
	e := annotationclock.Evidence{Request: req, Observation: obs, RTTNS: int64(20 * time.Millisecond), ElapsedNS: 0}
	interval, err := e.Interval()
	if err != nil {
		t.Fatal(err)
	}
	c, err := annotations.Build(annotations.Note{ID: "12345678-1234-4123-8123-123456789abc", AuthorPrincipal: f.grant.Grant.Principal, Text: "clock note", PositionMM: [3]int64{}, AuthoredAtUnixMS: interval.LowerUnixMS, ExpiresAtUnixMS: interval.LowerUnixMS + 3600000, Clock: &e}, annotations.Config{Tenant: f.grant.Grant.Tenant, Site: f.grant.Grant.Site, RunID: "run", Source: f.grant.Grant.Source, Frame: world.FrameRef{ID: "floor", Revision: "1"}, Epoch: 1, Sequence: 1})
	if err != nil {
		t.Fatal(err)
	}
	record, err := SignAnnotation(f.grant, f.key, c)
	if err != nil {
		t.Fatal(err)
	}
	return record
}
func TestAnnotationClockSkewBootstrapNoNonceConsumption(t *testing.T) {
	f, server, key, req, obs, pins := clockFixture(t)
	for _, skew := range []time.Duration{-24 * time.Hour, 24 * time.Hour} {
		r, _ := SignRequest(f.grant, f.key, "node-1", "annotation-write", nil, "long_enough_nonce", f.now.Add(skew), time.Minute)
		if _, err := f.verifier.Verify(r, "annotation-write", nil); err == nil {
			t.Fatal("ordinary skewed request accepted")
		}
	}
	for i := 0; i < 2; i++ {
		if _, err := f.verifier.IssueAnnotationClock(req, server, key, "run", "simulation:run", strings.Repeat("cd", 32)); err != nil {
			t.Fatal(err)
		}
	}
	if len(f.verifier.cfg.Replay.(*ReplayCache).state.Nonces) != 0 {
		t.Fatal("clock exchange consumed request nonce")
	}
	if _, err := VerifyAnnotationClockObservation(req, obs, pins); err != nil {
		t.Fatal(err)
	}
	record := clockRecord(t, f, req, obs)
	if _, err := f.verifier.VerifyRecord(record); err != nil {
		t.Fatal(err)
	}
	if _, err := f.verifier.VerifyRecords([]SignedRecord{record}); err != nil {
		t.Fatal(err)
	}
}
func TestAnnotationClockForgeryScopeAndPins(t *testing.T) {
	_, _, _, req, obs, pins := clockFixture(t)
	cases := map[string]func(*annotationclock.SignedRequest, *annotationclock.SignedObservation, *annotationclock.Pins){"nonce": func(r *annotationclock.SignedRequest, _ *annotationclock.SignedObservation, _ *annotationclock.Pins) {
		r.Request.Nonce = strings.Repeat("a", 64)
	}, "sample": func(_ *annotationclock.SignedRequest, o *annotationclock.SignedObservation, _ *annotationclock.Pins) {
		o.Observation.SampleUnixMS++
	}, "policy": func(_ *annotationclock.SignedRequest, o *annotationclock.SignedObservation, _ *annotationclock.Pins) {
		o.Observation.DriftPPM = 0
	}, "signature": func(_ *annotationclock.SignedRequest, o *annotationclock.SignedObservation, _ *annotationclock.Pins) {
		o.Signature[0] ^= 1
	}, "pin": func(_ *annotationclock.SignedRequest, _ *annotationclock.SignedObservation, p *annotationclock.Pins) {
		p.GatewayPrincipal = "other"
	}, "key": func(_ *annotationclock.SignedRequest, _ *annotationclock.SignedObservation, p *annotationclock.Pins) {
		p.GatewayKeySHA256 = strings.Repeat("0", 64)
	}, "root": func(_ *annotationclock.SignedRequest, _ *annotationclock.SignedObservation, p *annotationclock.Pins) {
		p.RootPublicKey = make([]byte, 32)
	}, "domain": func(_ *annotationclock.SignedRequest, _ *annotationclock.SignedObservation, p *annotationclock.Pins) {
		p.ClockDomain = "physical"
	}}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			clone := func(in, out any) {
				b, _ := json.Marshal(in)
				if json.Unmarshal(b, out) != nil {
					t.Fatal("clone")
				}
			}
			var r annotationclock.SignedRequest
			var o annotationclock.SignedObservation
			var p annotationclock.Pins
			clone(req, &r)
			clone(obs, &o)
			clone(pins, &p)
			mutate(&r, &o, &p)
			if _, err := VerifyAnnotationClockObservation(r, o, p); err == nil {
				t.Fatal("forged clock accepted")
			}
		})
	}
}
func TestAnnotationClockHistoricalEvidenceAndCurrentRevocations(t *testing.T) {
	for _, kind := range []string{"operator", "gateway", "gateway-key", "stale"} {
		t.Run(kind, func(t *testing.T) {
			f, server, key, req, obs, _ := clockFixture(t)
			record := clockRecord(t, f, req, obs)
			r := f.rev.Revocations
			r.Revision++
			switch kind {
			case "operator":
				r.Principals = []string{f.grant.Grant.Principal}
			case "gateway":
				r.Principals = []string{server.Grant.Principal}
			case "gateway-key":
				r.KeyIDs = []string{KeyID(server.Grant.PublicKey)}
			case "stale":
				f.now = f.now.Add(time.Hour)
			}
			if kind != "stale" {
				signed, _ := SignRevocations(r, f.root)
				if err := f.verifier.UpdateRevocations(signed); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := f.verifier.IssueAnnotationClock(req, server, key, "run", "simulation:run", strings.Repeat("cd", 32)); err == nil {
				t.Fatal("denied issuer issued")
			}
			if _, err := f.verifier.VerifyRecord(record); err == nil {
				t.Fatal("denied evidence visible")
			}
			if _, err := f.verifier.VerifyRecords([]SignedRecord{record}); err == nil {
				t.Fatal("denied evidence repairable")
			}
			if err := f.verifier.VerifyAnnotationHistory(record); err != nil {
				t.Fatal("history lost", err)
			}
		})
	}
}
func TestAnnotationClockCompletionFenceAndWriteFailure(t *testing.T) {
	f, server, key, req, _, _ := clockFixture(t)
	cache := f.verifier.cfg.Replay.(*ReplayCache)
	cache.beforeCommit = func() { f.now = f.now.Add(2 * time.Hour) }
	f.now = f.now.Add(time.Millisecond)
	// Memory replay commits do not invoke the disk seam. Use a delegated store
	// that advances time after the authority checkpoint instead.
	f.verifier.cfg.Replay = &clockAdvanceReplay{ReplayStore: cache, after: func() { f.now = f.now.Add(2 * time.Hour) }}
	if _, err := f.verifier.IssueAnnotationClock(req, server, key, "run", "simulation:run", strings.Repeat("cd", 32)); err == nil {
		t.Fatal("expired after durable check")
	}
	f, server, key, req, _, _ = clockFixture(t)
	sentinel := errors.New("storage unavailable")
	f.verifier.cfg.Replay = &clockAdvanceReplay{ReplayStore: f.verifier.cfg.Replay, failure: sentinel}
	if _, err := f.verifier.IssueAnnotationClock(req, server, key, "run", "simulation:run", strings.Repeat("cd", 32)); !errors.Is(err, sentinel) {
		t.Fatal(err)
	}
}

type clockAdvanceReplay struct {
	ReplayStore
	after   func()
	failure error
}

func (r *clockAdvanceReplay) Checkpoint(v uint64, d string, now time.Time) error {
	if r.failure != nil {
		return r.failure
	}
	err := r.ReplayStore.Checkpoint(v, d, now)
	if r.after != nil {
		r.after()
	}
	return err
}
