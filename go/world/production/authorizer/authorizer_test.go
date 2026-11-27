package authorizer

import (
	"context"
	"crypto/mldsa"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"net/url"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/production/admission"
	"github.com/wendylabsinc/wendy/go/world/production/internal/testidentity"
	"github.com/wendylabsinc/wendy/go/world/production/jws"
	"github.com/wendylabsinc/wendy/go/world/production/request"
	"github.com/wendylabsinc/wendy/go/world/production/trustedtime"
	"github.com/wendylabsinc/wendy/go/world/production/wendycert"
)

// These intervals are test injections, not evidence of authenticated time.
type testClock struct {
	mu              sync.Mutex
	w               trustedtime.Interval
	calls, failCall int
	advanceCall     int
	advance         time.Duration
}

func (c *testClock) Now() (trustedtime.Interval, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	if c.calls == c.failCall {
		return trustedtime.Interval{}, trustedtime.ErrUntrusted
	}
	if c.calls == c.advanceCall {
		c.w.Earliest = c.w.Earliest.Add(c.advance)
		c.w.Latest = c.w.Latest.Add(c.advance)
		c.w.Monotonic += c.advance
	}
	return c.w, nil
}
func (c *testClock) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = 0
	c.failCall = 0
	c.advanceCall = 0
}

type environment struct {
	t                             *testing.T
	now                           time.Time
	reader, policyIssuer, revoker *testidentity.Fixture
	clock                         *testClock
	cfg                           Config
	path                          string
	a                             *Authorizer
	policy                        admission.Policy
	revocations                   admission.Revocations
}

func sibling(t *testing.T, base *testidentity.Fixture, principal string, serial int64, sameKey bool) *testidentity.Fixture {
	t.Helper()
	key := base.Key
	if !sameKey {
		var err error
		key, err = mldsa.GenerateKey(mldsa.MLDSA65())
		if err != nil {
			t.Fatal(err)
		}
	}
	template := *base.Leaf
	template.SerialNumber = big.NewInt(serial)
	u, err := url.Parse(principal)
	if err != nil {
		t.Fatal(err)
	}
	template.URIs = []*url.URL{u}
	leaf := testidentity.Issue(t, &template, base.Issuer, key.Public(), base.IssuerKey)
	f := *base
	f.Principal = principal
	f.Key = key
	f.Leaf = leaf
	f.ChainDER = [][]byte{leaf.Raw, base.Issuer.Raw}
	return &f
}

func setup(t *testing.T) *environment {
	t.Helper()
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	e := &environment{t: t, now: now, path: filepath.Join(t.TempDir(), "admission.json")}
	e.reader = testidentity.New(t, now)
	e.policyIssuer = sibling(t, e.reader, "spiffe://wendy.sh/tenant/"+testidentity.Tenant+"/operator/policy-issuer", 4, false)
	e.revoker = sibling(t, e.reader, "spiffe://wendy.sh/tenant/"+testidentity.Tenant+"/operator/revocation-issuer", 5, false)
	v, err := wendycert.New(wendycert.Config{Tenant: testidentity.Tenant, Roots: [][]byte{e.reader.RootDER}})
	if err != nil {
		t.Fatal(err)
	}
	e.clock = &testClock{w: trustedtime.Interval{Earliest: now.Add(-10 * time.Millisecond), Latest: now.Add(10 * time.Millisecond), BootID: "untrusted-test-boot", Monotonic: time.Second, Domain: "physical", Authenticated: true, Source: "test-injection-no-external-proof"}}
	pin := func(f *testidentity.Fixture) admission.Pin {
		id, err := v.Verify(f.ChainDER, e.clock.w.Earliest, e.clock.w.Latest)
		if err != nil {
			t.Fatal(err)
		}
		return admission.Pin{Principal: id.Principal, SigningKeySHA256: id.SigningKeySHA256}
	}
	e.cfg = Config{Clock: e.clock, Admission: admission.Config{Tenant: testidentity.Tenant, Site: "test-office", Audience: "test-gateway/world", Certificates: v, PolicyIssuer: pin(e.policyIssuer), RevocationIssuer: pin(e.revoker), MaxRevocationAge: 10 * time.Minute}}
	e.policy = admission.Policy{Version: 1, Domain: "physical", Tenant: e.cfg.Admission.Tenant, Site: e.cfg.Admission.Site, Audience: e.cfg.Admission.Audience, IssuerPrincipal: e.policyIssuer.Principal, IssuerKeySHA256: e.cfg.Admission.PolicyIssuer.SigningKeySHA256, PolicyID: "test-read-policy", Revision: 1, IssuedAtUnixMS: now.Add(-time.Second).UnixMilli(), ExpiresAtUnixMS: now.Add(30 * time.Minute).UnixMilli(), Entries: []admission.PolicyEntry{{Principal: e.reader.Principal, Scopes: []string{admission.ReadScope}}}}
	e.revocations = admission.Revocations{Version: 1, Domain: "physical", Tenant: e.policy.Tenant, Site: e.policy.Site, Audience: e.policy.Audience, IssuerPrincipal: e.revoker.Principal, IssuerKeySHA256: e.cfg.Admission.RevocationIssuer.SigningKeySHA256, Revision: 1, IssuedAtUnixMS: now.Add(-time.Second).UnixMilli(), ExpiresAtUnixMS: now.Add(2 * time.Minute).UnixMilli()}
	e.revocations.RevokedPrincipals = []string{}
	e.revocations.RevokedKeySHA256 = []string{}
	e.revocations.RevokedCertificateSHA256 = []string{}
	e.revocations.RevokedPolicyIDs = []string{}
	e.a, err = Open(e.path, e.cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if e.a != nil {
			_ = e.a.Close()
		}
	})
	if err = e.a.ApplyPolicy(e.policyProof()); err != nil {
		t.Fatalf("initial policy: %v", err)
	}
	if err = e.a.ApplyRevocations(e.revocationProof()); err != nil {
		t.Fatalf("initial revocations: %v", err)
	}
	e.clock.reset()
	return e
}

func (e *environment) sign(typ string, payload any, identity *testidentity.Fixture) string {
	e.t.Helper()
	b, err := json.Marshal(payload)
	if err != nil {
		e.t.Fatal(err)
	}
	proof, err := jws.Sign(typ, b, identity.ChainDER, identity.Key)
	if err != nil {
		e.t.Fatal(err)
	}
	return proof
}
func (e *environment) policyProof() string {
	return e.sign(admission.PolicyTyp, e.policy, e.policyIssuer)
}
func (e *environment) revocationProof() string {
	return e.sign(admission.RevocationsTyp, e.revocations, e.revoker)
}
func (e *environment) proof(f *testidentity.Fixture, nonce string, body []byte) string {
	e.t.Helper()
	if nonce == "" {
		b := make([]byte, 24)
		if _, err := rand.Read(b); err != nil {
			e.t.Fatal(err)
		}
		nonce = base64.RawURLEncoding.EncodeToString(b)
	}
	h, err := request.BodyDigest(body)
	if err != nil {
		e.t.Fatal(err)
	}
	d := request.Descriptor{Version: 1, Domain: "physical", Tenant: e.policy.Tenant, Site: e.policy.Site, Audience: e.policy.Audience, Principal: f.Principal, Operation: request.OperationRead, Method: request.Method, Path: request.Path, BodySHA256: h, PolicyRevision: e.policy.Revision, IssuedAtUnixMS: e.now.Add(-time.Second).UnixMilli(), ExpiresAtUnixMS: e.now.Add(30 * time.Second).UnixMilli(), Nonce: nonce}
	p, err := request.Sign(d, body, f.ChainDER, f.Key)
	if err != nil {
		e.t.Fatal(err)
	}
	return p
}
func (e *environment) reopen() {
	e.t.Helper()
	if err := e.a.Close(); err != nil {
		e.t.Fatal(err)
	}
	var err error
	e.a, err = Open(e.path, e.cfg)
	if err != nil {
		e.t.Fatal(err)
	}
}

func TestCompleteAdmissionRestartAndCertificateRenewal(t *testing.T) {
	e := setup(t)
	body := []byte(`{"kind":"asset"}`)
	nonce := base64.RawURLEncoding.EncodeToString(make([]byte, 24))
	proof := e.proof(e.reader, nonce, body)
	d, err := e.a.Authorize(proof, request.Method, request.Path, body)
	if err != nil || d.Principal != e.reader.Principal || d.PolicyRevision != 1 || d.RevocationRevision != 1 {
		t.Fatalf("admit: %+v %v", d, err)
	}
	e.reopen()
	if _, err = e.a.Authorize(proof, request.Method, request.Path, body); !errors.Is(err, admission.ErrReplay) {
		t.Fatalf("replay after reopen: %v", err)
	}
	renewed := sibling(t, e.reader, e.reader.Principal, 99, true)
	if _, err = e.a.Authorize(e.proof(renewed, nonce, body), request.Method, request.Path, body); !errors.Is(err, admission.ErrReplay) {
		t.Fatalf("same-key renewal reset nonce: %v", err)
	}
	if _, err = e.a.Authorize(e.proof(renewed, "", body), request.Method, request.Path, body); err != nil {
		t.Fatal(err)
	}
}

func TestConcurrentDuplicateHasExactlyOneAdmission(t *testing.T) {
	e := setup(t)
	body := []byte(`{}`)
	proof := e.proof(e.reader, "", body)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	results := make(chan error, 2)
	worker := func() {
		_, err := e.a.Authorize(proof, request.Method, request.Path, body)
		select {
		case results <- err:
		case <-ctx.Done():
		}
	}
	// Exactly two workers and two buffered results; the wait has a deadline.
	go worker()
	go worker()
	accepted := 0
	for i := 0; i < 2; i++ {
		select {
		case err := <-results:
			if err == nil {
				accepted++
			} else if !errors.Is(err, admission.ErrReplay) {
				t.Fatal(err)
			}
		case <-ctx.Done():
			t.Fatal("duplicate admission did not finish")
		}
	}
	if accepted != 1 {
		t.Fatalf("accepted %d duplicates", accepted)
	}
}

func TestChangedBodyRouteAndUnlistedTenantOperatorDenied(t *testing.T) {
	e := setup(t)
	body := []byte(`{}`)
	proof := e.proof(e.reader, "", body)
	for _, c := range []struct {
		method, path string
		body         []byte
	}{{"GET", request.Path, body}, {request.Method, request.Path + "?x=1", body}, {request.Method, "/v1/world/%71uery", body}, {request.Method, request.Path, []byte(`{"changed":true}`)}} {
		if _, err := e.a.Authorize(proof, c.method, c.path, c.body); err == nil {
			t.Fatal("altered request admitted")
		}
	}
	unlisted := sibling(t, e.reader, "spiffe://wendy.sh/tenant/"+testidentity.Tenant+"/operator/unlisted", 10, false)
	if _, err := e.a.Authorize(e.proof(unlisted, "", body), request.Method, request.Path, body); err == nil {
		t.Fatal("tenant identity became site permission")
	}
	if _, err := e.a.Authorize(proof, request.Method, request.Path, body); err != nil {
		t.Fatalf("refusal consumed valid nonce: %v", err)
	}
}

func TestRevocationAndPolicyReplacementSurviveRestart(t *testing.T) {
	e := setup(t)
	body := []byte(`{}`)
	old := e.policyProof()
	e.policy.Revision = 2
	if err := e.a.ApplyPolicy(e.policyProof()); err != nil {
		t.Fatal(err)
	}
	id, err := e.cfg.Admission.Certificates.Verify(e.reader.ChainDER, e.clock.w.Earliest, e.clock.w.Latest)
	if err != nil {
		t.Fatal(err)
	}
	e.revocations.Revision = 2
	e.revocations.RevokedKeySHA256 = []string{id.SigningKeySHA256}
	if err = e.a.ApplyRevocations(e.revocationProof()); err != nil {
		t.Fatal(err)
	}
	e.reopen()
	if err = e.a.ApplyPolicy(old); err == nil {
		t.Fatal("old policy restored after restart")
	}
	if _, err = e.a.Authorize(e.proof(e.reader, "", body), request.Method, request.Path, body); err == nil {
		t.Fatal("revoked reader admitted after restart")
	}
}

func TestUnavailableFinalTimeConsumesNonceWithoutDecision(t *testing.T) {
	e := setup(t)
	body := []byte(`{}`)
	proof := e.proof(e.reader, "", body)
	e.clock.failCall = 3
	if d, err := e.a.Authorize(proof, request.Method, request.Path, body); err == nil || d.Principal != "" {
		t.Fatal("untrusted final clock returned admission")
	}
	e.clock.reset()
	e.reopen()
	if _, err := e.a.Authorize(proof, request.Method, request.Path, body); !errors.Is(err, admission.ErrReplay) {
		t.Fatalf("committed nonce lost: %v", err)
	}
}

func TestExpiryDuringVerificationOrCommitRefusesDecision(t *testing.T) {
	for _, sample := range []int{2, 3} {
		t.Run(string(rune('0'+sample)), func(t *testing.T) {
			e := setup(t)
			body := []byte(`{}`)
			proof := e.proof(e.reader, "", body)
			e.clock.advanceCall = sample
			e.clock.advance = 31 * time.Second
			if d, err := e.a.Authorize(proof, request.Method, request.Path, body); err == nil || d.Principal != "" {
				t.Fatal("expired proof admitted")
			}
			s, err := e.a.Snapshot()
			if err != nil {
				t.Fatal(err)
			}
			want := 0
			if sample == 3 {
				want = 1
			}
			if s.NonceCount != want {
				t.Fatalf("nonce count %d want %d", s.NonceCount, want)
			}
		})
	}
}

func TestClockRollbackAndSyntheticClockRefused(t *testing.T) {
	e := setup(t)
	body := []byte(`{}`)
	e.clock.w.Domain = "simulation"
	if _, err := e.a.Authorize(e.proof(e.reader, "", body), request.Method, request.Path, body); err == nil {
		t.Fatal("simulation clock accepted")
	}
	e.clock.w.Domain = "physical"
	if _, err := e.a.Authorize(e.proof(e.reader, "", body), request.Method, request.Path, body); err != nil {
		t.Fatal(err)
	}
	e.clock.w.Monotonic = 0
	if _, err := e.a.Authorize(e.proof(e.reader, "", body), request.Method, request.Path, body); err == nil {
		t.Fatal("boot monotonic rollback accepted")
	}
	e.clock.w.Monotonic = time.Second
	if _, err := e.a.Authorize(e.proof(e.reader, "", body), request.Method, request.Path, body); err == nil {
		t.Fatal("rollback fault silently reset")
	}
}

func TestRevocationExpiresAfterNonceCommit(t *testing.T) {
	e := setup(t)
	body := []byte(`{}`)
	proof := e.proof(e.reader, "", body)
	e.revocations.Revision = 2
	e.revocations.ExpiresAtUnixMS = e.now.Add(10 * time.Second).UnixMilli()
	if err := e.a.ApplyRevocations(e.revocationProof()); err != nil {
		t.Fatal(err)
	}
	e.clock.reset()
	e.clock.advanceCall, e.clock.advance = 3, 11*time.Second
	if d, err := e.a.Authorize(proof, request.Method, request.Path, body); err == nil || d.Principal != "" {
		t.Fatal("expired revocation snapshot returned a decision")
	}
	s, err := e.a.Snapshot()
	if err != nil || s.NonceCount != 1 {
		t.Fatalf("nonce was not durably committed before final denial: %+v %v", s, err)
	}
	e.clock.reset()
	e.reopen()
	e.revocations.Revision = 3
	e.revocations.ExpiresAtUnixMS = e.now.Add(2 * time.Minute).UnixMilli()
	if err = e.a.ApplyRevocations(e.revocationProof()); err != nil {
		t.Fatal(err)
	}
	if _, err = e.a.Authorize(proof, request.Method, request.Path, body); !errors.Is(err, admission.ErrReplay) {
		t.Fatalf("refresh lost consumed nonce: %v", err)
	}
}

func TestNoImplicitClockOrCertificateTrust(t *testing.T) {
	if _, err := Open(filepath.Join(t.TempDir(), "ledger"), Config{}); err == nil {
		t.Fatal("implicit trust")
	}
}
