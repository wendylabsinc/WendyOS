package node

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/annotationclock"
	"github.com/wendylabsinc/wendy/go/world/internal/annotations"
	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/store"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

func enableClock(t *testing.T, f *fixture) annotationclock.Pins {
	t.Helper()
	c := issueCredentials(t, f.root, "clock")
	g := c.Grant.Grant
	g.Kind = "device"
	g.PublishAudience = f.audience
	g.Scopes = []string{"annotation-clock-server"}
	var err error
	c.Grant, err = authority.SignGrant(g, f.root)
	if err != nil {
		t.Fatal(err)
	}
	f.cfg.Audience = f.audience
	f.cfg.AnnotationClock = &AnnotationClockConfig{Credentials: c, RunID: "live", ClockDomain: "simulation:live"}
	f.n, err = New(f.cfg, f.s, f.v)
	if err != nil {
		t.Fatal(err)
	}
	return annotationclock.Pins{RootPublicKey: f.root.Public().(ed25519.PublicKey), GatewayPrincipal: g.Principal, GatewayKeySHA256: authority.KeyID(g.PublicKey), RunID: "live", ClockDomain: "simulation:live"}
}
func nodeClockRequest(t *testing.T, f *fixture) annotationclock.SignedRequest {
	t.Helper()
	r, err := authority.SignAnnotationClockRequest(f.credentials.Grant, f.credentials.PrivateKey, annotationclock.Request{Tenant: f.cfg.Tenant, Site: f.cfg.Site, Audience: f.audience, RunID: "live", ClockDomain: "simulation:live", Nonce: strings.Repeat("ef", 32)})
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func nodeClockExchange(t *testing.T, f *fixture, req annotationclock.SignedRequest) annotationclock.SignedObservation {
	t.Helper()
	body, _ := json.Marshal(req)
	r, err := http.Post(f.server.URL+"/v1/annotations/clock", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	raw, _ := io.ReadAll(r.Body)
	if r.StatusCode != 200 {
		t.Fatalf("clock status %d: %s", r.StatusCode, raw)
	}
	var out annotationclock.SignedObservation
	if err = authority.DecodeStrict(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}
func TestAnnotationClockHTTPBoundsAndTimestampFreeBootstrap(t *testing.T) {
	f := newFixture(t, nil, "node-1")
	annotationOperator(t, f)
	pins := enableClock(t, f)
	req := nodeClockRequest(t, f)
	obs := nodeClockExchange(t, f, req)
	if _, err := authority.VerifyAnnotationClockObservation(req, obs, pins); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(req)
	for _, tt := range []struct {
		name, path, body string
		busy             bool
		status           int
	}{{"rate", "/v1/annotations/clock", string(body), false, 429}, {"concurrency", "/v1/annotations/clock", string(body), true, 429}, {"query", "/v1/annotations/clock?x=1", string(body), false, 400}, {"oversize", "/v1/annotations/clock", strings.Repeat("x", 16<<10+1), false, 400}, {"unknown", "/v1/annotations/clock", `{"extra":true}`, false, 400}} {
		t.Run(tt.name, func(t *testing.T) {
			if tt.name != "rate" {
				f.n.annotationClockLast = time.Time{}
			}
			if tt.busy {
				f.n.annotationClockMu.Lock()
				defer f.n.annotationClockMu.Unlock()
			}
			r := httptest.NewRequest("POST", tt.path, strings.NewReader(tt.body))
			w := httptest.NewRecorder()
			f.n.Handler().ServeHTTP(w, r)
			if w.Code != tt.status {
				t.Fatalf("status %d", w.Code)
			}
		})
	}
}
func TestAnnotationV2LiveRepairExpiryAndReopen(t *testing.T) {
	a := newFixture(t, nil, "node-1")
	annotationOperator(t, a)
	pins := enableClock(t, a)
	req := nodeClockRequest(t, a)
	obs := nodeClockExchange(t, a, req)
	if _, err := authority.VerifyAnnotationClockObservation(req, obs, pins); err != nil {
		t.Fatal(err)
	}
	epoch := annotationEpoch(t, a)
	e := annotationclock.Evidence{Request: req, Observation: obs, RTTNS: int64(10 * time.Millisecond), ElapsedNS: 0}
	interval, _ := e.Interval()
	note := annotations.Note{ID: "12345678-1234-4123-8123-123456789abc", AuthorPrincipal: a.credentials.Grant.Grant.Principal, Text: "offline bounded note", AuthoredAtUnixMS: interval.LowerUnixMS, ExpiresAtUnixMS: interval.LowerUnixMS + 60000, Clock: &e}
	c, err := annotations.Build(note, annotations.Config{Tenant: a.cfg.Tenant, Site: a.cfg.Site, RunID: "live", Source: a.credentials.Grant.Grant.Source, Frame: a.wc.Frames[0], Epoch: epoch, Sequence: 1})
	if err != nil {
		t.Fatal(err)
	}
	record, err := authority.SignAnnotation(a.credentials.Grant, a.credentials.PrivateKey, c)
	if err != nil {
		t.Fatal(err)
	}
	status, body := postAnnotation(t, a, record)
	if status != 200 {
		t.Fatalf("publish %d %s", status, body)
	}
	status, body = postAnnotation(t, a, record)
	if status != 200 {
		t.Fatalf("retry %d %s", status, body)
	}
	b := newFixture(t, a.root, "node-2")
	if _, err = b.n.apply(record, false); err != nil {
		t.Fatal(err)
	}
	shortened := record
	shortened.Component.Timing.AgeHint = 30 * time.Second
	if _, err = b.n.apply(shortened, false); err != nil {
		t.Fatal(err)
	}
	before, err := b.s.RepairSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if err = b.s.Close(); err != nil {
		t.Fatal(err)
	}
	b.s, err = store.Open(filepath.Join(filepath.Dir(b.cfg.ProofPath), "store.json"), b.wc)
	if err != nil {
		t.Fatal(err)
	}
	b.n, err = New(b.cfg, b.s, b.v)
	if err != nil {
		t.Fatal("reopen", err)
	}
	after, err := b.s.RepairSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(before.Entries) != 1 || len(after.Entries) != 1 || !after.Entries[0].ExpiresAt.Equal(before.Entries[0].ExpiresAt) {
		t.Fatal("reopen renewed cap")
	}
	records, err := b.s.Records()
	if err != nil || len(records) != 1 || records[0].Key.Type != annotations.TypeV2 {
		t.Fatal("lost v2 record", err)
	}
}

func TestAnnotationCrossSchemaIdentityImmutable(t *testing.T) {
	a := newFixture(t, nil, "node-1")
	annotationOperator(t, a)
	enableClock(t, a)
	req := nodeClockRequest(t, a)
	obs := nodeClockExchange(t, a, req)
	epoch := annotationEpoch(t, a)
	v1 := annotationRecord(t, a, epoch)
	e := annotationclock.Evidence{Request: req, Observation: obs, RTTNS: int64(time.Millisecond)}
	interval, _ := e.Interval()
	note := annotations.Note{ID: strings.TrimPrefix(v1.Component.Key.Entity, annotations.EntityPrefix), AuthorPrincipal: a.credentials.Grant.Grant.Principal, Text: "replacement forbidden", AuthoredAtUnixMS: interval.LowerUnixMS, ExpiresAtUnixMS: interval.LowerUnixMS + 60000, Clock: &e}
	c, err := annotations.Build(note, annotations.Config{Tenant: a.cfg.Tenant, Site: a.cfg.Site, RunID: "live", Source: a.credentials.Grant.Grant.Source, Frame: a.wc.Frames[0], Epoch: epoch, Sequence: 1})
	if err != nil {
		t.Fatal(err)
	}
	v2, err := authority.SignAnnotation(a.credentials.Grant, a.credentials.PrivateKey, c)
	if err != nil {
		t.Fatal(err)
	}
	repair := func(r authority.SignedRecord) RepairRecord {
		entry, err := repairEntry(world.CheckpointEntry{Component: r.Component, ExpiresAt: time.Now().Add(30 * time.Second)}, r)
		if err != nil {
			t.Fatal(err)
		}
		return RepairRecord{Entry: entry, Record: r}
	}
	t.Run("local-and-repair", func(t *testing.T) {
		if status, raw := postAnnotation(t, a, v1); status != 200 {
			t.Fatal(status, string(raw))
		}
		if status, _ := postAnnotation(t, a, v2); status != 409 {
			t.Fatal("cross-schema replacement", status)
		}
		if _, err := a.n.applyRepairPage([]RepairRecord{repair(v2)}); !errors.Is(err, ErrAnnotationConflict) {
			t.Fatal(err)
		}
	})
	t.Run("atomic-page", func(t *testing.T) {
		b := newFixture(t, a.root, "node-2")
		if got, err := b.n.applyRepairPage([]RepairRecord{repair(v1), repair(v2)}); !errors.Is(err, ErrAnnotationConflict) || len(got) != 0 {
			t.Fatal(got, err)
		}
		records, err := b.s.Records()
		if err != nil || len(records) != 0 {
			t.Fatal("partial conflicting page", err)
		}
	})
	t.Run("reopen-refuses-conflicting-history", func(t *testing.T) {
		b := newFixture(t, a.root, "node-2")
		for _, r := range []authority.SignedRecord{v1, v2} {
			if _, err := b.s.Apply(r.Component.Key.Source, r.Component); err != nil {
				t.Fatal(err)
			}
			d, _ := recordDigest(r.Component)
			b.n.proofs[d] = r
		}
		if err := b.n.saveProofs(b.n.proofs); err != nil {
			t.Fatal(err)
		}
		if _, err := New(b.cfg, b.s, b.v); !errors.Is(err, ErrAnnotationConflict) {
			t.Fatal("conflicting history accepted", err)
		}
	})
}
