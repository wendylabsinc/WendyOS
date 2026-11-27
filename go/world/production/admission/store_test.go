package admission

import (
	"crypto/mldsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/production/internal/testidentity"
	"github.com/wendylabsinc/wendy/go/world/production/jws"
	"github.com/wendylabsinc/wendy/go/world/production/trustedtime"
	"github.com/wendylabsinc/wendy/go/world/production/wendycert"
)

type fixture struct {
	store                                *Store
	cfg                                  Config
	path                                 string
	window                               trustedtime.Interval
	policy                               Policy
	revocations                          Revocations
	policySigner, revocationSigner, user *testidentity.Fixture
	request                              Request
}

func principal(name string) string {
	return "spiffe://wendy.sh/tenant/" + testidentity.Tenant + "/operator/" + name
}
func proof(t *testing.T, typ string, value any, signer *testidentity.Fixture) string {
	t.Helper()
	b, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	p, err := jws.Sign(typ, b, signer.ChainDER, signer.Key)
	if err != nil {
		t.Fatal(err)
	}
	return p
}
func newFixture(t *testing.T) *fixture {
	t.Helper()
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	f := &fixture{path: filepath.Join(t.TempDir(), "ledger.json"), window: trustedtime.Interval{Earliest: now, Latest: now.Add(time.Millisecond), BootID: "test-boot", Monotonic: time.Second, Domain: "physical", Authenticated: true, Source: "test-authenticated-provider"}}
	f.policySigner = testidentity.NewWith(t, now, principal("policy-admin"))
	f.revocationSigner = testidentity.NewWith(t, now, principal("deny-admin"))
	f.user = testidentity.NewWith(t, now, principal("reader"))
	v, err := wendycert.New(wendycert.Config{Tenant: testidentity.Tenant, Roots: [][]byte{f.policySigner.RootDER, f.revocationSigner.RootDER, f.user.RootDER}})
	if err != nil {
		t.Fatal(err)
	}
	identity := func(x *testidentity.Fixture) wendycert.Identity {
		id, e := v.Verify(x.ChainDER, now.Add(-time.Second), now.Add(time.Second))
		if e != nil {
			t.Fatal(e)
		}
		return id
	}
	pi, ri, ui := identity(f.policySigner), identity(f.revocationSigner), identity(f.user)
	f.cfg = Config{Tenant: testidentity.Tenant, Site: "office", Audience: "gateway/world", PolicyIssuer: Pin{pi.Principal, pi.SigningKeySHA256}, RevocationIssuer: Pin{ri.Principal, ri.SigningKeySHA256}, Certificates: v, MaxRevocationAge: 10 * time.Minute}
	f.policy = Policy{Version: 1, Domain: "physical", Tenant: f.cfg.Tenant, Site: f.cfg.Site, Audience: f.cfg.Audience, IssuerPrincipal: pi.Principal, IssuerKeySHA256: pi.SigningKeySHA256, PolicyID: "office-policy", Revision: 1, IssuedAtUnixMS: now.Add(-time.Minute).UnixMilli(), ExpiresAtUnixMS: now.Add(30 * time.Minute).UnixMilli(), Entries: []PolicyEntry{{ui.Principal, []string{ReadScope}}}}
	f.revocations = Revocations{Version: 1, Domain: "physical", Tenant: f.cfg.Tenant, Site: f.cfg.Site, Audience: f.cfg.Audience, IssuerPrincipal: ri.Principal, IssuerKeySHA256: ri.SigningKeySHA256, Revision: 1, IssuedAtUnixMS: now.Add(-time.Minute).UnixMilli(), ExpiresAtUnixMS: now.Add(5 * time.Minute).UnixMilli(), RevokedPrincipals: []string{}, RevokedKeySHA256: []string{}, RevokedCertificateSHA256: []string{}, RevokedPolicyIDs: []string{}}
	f.request = Request{Identity: ui, Tenant: f.cfg.Tenant, Site: f.cfg.Site, Audience: f.cfg.Audience, Principal: ui.Principal, Operation: ReadScope, PolicyRevision: 1, Nonce: base64.RawURLEncoding.EncodeToString(make([]byte, 24)), IssuedAtUnixMS: now.Add(-time.Second).UnixMilli(), ExpiresAtUnixMS: now.Add(30 * time.Second).UnixMilli()}
	f.store, err = Open(f.path, f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.store.Close() })
	if err = f.store.ApplyPolicy(proof(t, PolicyTyp, f.policy, f.policySigner), f.window); err != nil {
		t.Fatal(err)
	}
	if err = f.store.ApplyRevocations(proof(t, RevocationsTyp, f.revocations, f.revocationSigner), f.window); err != nil {
		t.Fatal(err)
	}
	return f
}
func (f *fixture) reopen(t *testing.T) {
	t.Helper()
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	var err error
	f.store, err = Open(f.path, f.cfg)
	if err != nil {
		t.Fatal(err)
	}
}

func TestDurableNonceSurvivesRenewalAndRestart(t *testing.T) {
	f := newFixture(t)
	if err := f.store.Admit(f.request, f.window); err != nil {
		t.Fatal(err)
	}
	if err := f.store.Check(f.request, f.window); err != nil {
		t.Fatal("post-commit permission check consumed twice", err)
	}
	f.reopen(t)
	template := *f.user.Leaf
	template.SerialNumber = big.NewInt(77)
	renewed := testidentity.Issue(t, &template, f.user.Issuer, f.user.Key.Public(), f.user.IssuerKey)
	id, err := f.cfg.Certificates.Verify([][]byte{renewed.Raw, f.user.Issuer.Raw}, f.window.Earliest, f.window.Latest)
	if err != nil {
		t.Fatal(err)
	}
	if id.SigningKeySHA256 != f.request.Identity.SigningKeySHA256 || id.CertificateSHA256 == f.request.Identity.CertificateSHA256 {
		t.Fatal("test did not renew same signing key")
	}
	f.request.Identity = id
	if err = f.store.Admit(f.request, f.window); !errors.Is(err, ErrReplay) {
		t.Fatal("renewed certificate reset replay protection", err)
	}
	if snapshot, err := f.store.Snapshot(); err != nil || snapshot.NonceCount != 1 {
		t.Fatal(snapshot, err)
	}
}

func TestAuthorityRevisionRollbackConflictAndCommissioning(t *testing.T) {
	f := newFixture(t)
	old := proof(t, PolicyTyp, f.policy, f.policySigner)
	f.policy.Revision = 2
	if err := f.store.ApplyPolicy(proof(t, PolicyTyp, f.policy, f.policySigner), f.window); err != nil {
		t.Fatal(err)
	}
	f.reopen(t)
	if err := f.store.ApplyPolicy(old, f.window); !errors.Is(err, ErrRollback) {
		t.Fatal("policy rollback admitted", err)
	}
	f.policy.PolicyID = "different-policy"
	if err := f.store.ApplyPolicy(proof(t, PolicyTyp, f.policy, f.policySigner), f.window); !errors.Is(err, ErrConflict) {
		t.Fatal("same revision equivocation admitted", err)
	}
	// A fully trusted tenant operator is not a commissioned policy issuer.
	f.policy.Revision = 3
	f.policy.IssuerPrincipal = f.user.Principal
	f.policy.IssuerKeySHA256 = f.request.Identity.SigningKeySHA256
	if err := f.store.ApplyPolicy(proof(t, PolicyTyp, f.policy, f.user), f.window); err == nil {
		t.Fatal("trusted operator became policy administrator")
	}
	revOld := proof(t, RevocationsTyp, f.revocations, f.revocationSigner)
	f.revocations.Revision = 2
	if err := f.store.ApplyRevocations(proof(t, RevocationsTyp, f.revocations, f.revocationSigner), f.window); err != nil {
		t.Fatal(err)
	}
	f.reopen(t)
	if err := f.store.ApplyRevocations(revOld, f.window); !errors.Is(err, ErrRollback) {
		t.Fatal("revocation rollback admitted", err)
	}
	f.revocations.RevokedPolicyIDs = []string{"something"}
	if err := f.store.ApplyRevocations(proof(t, RevocationsTyp, f.revocations, f.revocationSigner), f.window); !errors.Is(err, ErrConflict) {
		t.Fatal("revocation equivocation admitted", err)
	}
	if _, err := Open(f.path, f.cfg); err == nil {
		t.Fatal("second writer acquired process-owned ledger")
	}
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	other := f.cfg
	other.Site = "other-site"
	if s, err := Open(f.path, other); err == nil {
		_ = s.Close()
		t.Fatal("ledger reopened under another scope")
	}
}

func TestAllRevocationClassesAndCandidateCAs(t *testing.T) {
	for _, kind := range []string{"principal", "key", "certificate", "policy", "policy-issuer", "revocation-issuer", "unused-root-key", "unused-root-certificate", "unused-presented-issuer"} {
		t.Run(kind, func(t *testing.T) {
			f := newFixture(t)
			r := f.revocations
			r.Revision++
			switch kind {
			case "principal":
				r.RevokedPrincipals = []string{f.request.Principal}
			case "key":
				r.RevokedKeySHA256 = []string{f.request.Identity.SigningKeySHA256}
			case "certificate":
				r.RevokedCertificateSHA256 = []string{f.request.Identity.CertificateSHA256}
			case "policy":
				r.RevokedPolicyIDs = []string{f.policy.PolicyID}
			case "policy-issuer":
				r.RevokedPrincipals = []string{f.cfg.PolicyIssuer.Principal}
			case "revocation-issuer":
				r.RevokedPrincipals = []string{f.cfg.RevocationIssuer.Principal}
			case "unused-root-key":
				r.RevokedKeySHA256 = []string{digest(f.policySigner.Root.RawSubjectPublicKeyInfo)}
			case "unused-root-certificate":
				r.RevokedCertificateSHA256 = []string{digest(f.policySigner.Root.Raw)}
			case "unused-presented-issuer":
				extra := testidentity.NewWith(t, f.window.Earliest, principal("unrelated"))
				chain := append(append([][]byte{}, f.user.ChainDER...), extra.Issuer.Raw)
				id, err := f.cfg.Certificates.Verify(chain, f.window.Earliest, f.window.Latest)
				if err != nil {
					t.Fatal(err)
				}
				f.request.Identity = id
				r.RevokedCertificateSHA256 = []string{digest(extra.Issuer.Raw)}
			}
			if err := f.store.ApplyRevocations(proof(t, RevocationsTyp, r, f.revocationSigner), f.window); err != nil {
				t.Fatal(err)
			}
			if err := f.store.Admit(f.request, f.window); err == nil {
				t.Fatal("revocation ignored", kind)
			}
			f.reopen(t)
			if err := f.store.Check(f.request, f.window); err == nil {
				t.Fatal("restart dropped revocation", kind)
			}
			if kind == "revocation-issuer" {
				clear := f.revocations
				clear.Revision = r.Revision + 1
				if err := f.store.ApplyRevocations(proof(t, RevocationsTyp, clear, f.revocationSigner), f.window); err == nil {
					t.Fatal("revoked issuer cleared its own denial")
				}
			}
		})
	}
}

func TestPersistenceFailureNeverReturnsAdmission(t *testing.T) {
	for _, committed := range []bool{false, true} {
		t.Run(map[bool]string{false: "before write", true: "uncertain after commit"}[committed], func(t *testing.T) {
			f := newFixture(t)
			f.store.write = func(path string, data []byte) error {
				if committed {
					if err := atomicWrite(path, data); err != nil {
						return err
					}
				}
				return errors.New("injected write/sync failure")
			}
			if err := f.store.Admit(f.request, f.window); err == nil {
				t.Fatal("failed persistence returned admission")
			}
			if len(f.store.state.Nonces) != 0 {
				t.Fatal("failed write mutated accepted in-memory state")
			}
			if err := f.store.Admit(f.request, f.window); !errors.Is(err, ErrFaulted) {
				t.Fatal("uncertain store kept accepting", err)
			}
			f.reopen(t)
			err := f.store.Admit(f.request, f.window)
			if committed && !errors.Is(err, ErrReplay) {
				t.Fatal("uncertain durable nonce disappeared", err)
			}
			if !committed && err != nil {
				t.Fatal("uncommitted failed request became accepted", err)
			}
		})
	}
}

func TestTrustedIntervalsStalenessAndOverlap(t *testing.T) {
	f := newFixture(t)
	if err := f.store.Admit(f.request, f.window); err != nil {
		t.Fatal(err)
	}
	overlap := f.window
	overlap.Earliest = overlap.Earliest.Add(-500 * time.Millisecond)
	overlap.Latest = overlap.Latest.Add(500 * time.Millisecond)
	overlap.Monotonic += time.Second
	if err := f.store.Check(f.request, overlap); err != nil {
		t.Fatal("legitimate uncertainty overlap falsely treated as rollback", err)
	}
	f.reopen(t)
	old := f.window
	old.Earliest = old.Earliest.Add(-time.Second)
	old.Latest = old.Earliest.Add(time.Millisecond)
	old.BootID = "new-authenticated-boot"
	if err := f.store.Check(f.request, old); !errors.Is(err, ErrRollback) {
		t.Fatal("restart accepted interval wholly before durable lower bound", err)
	}
	back := f.window
	back.Monotonic--
	if err := f.store.Check(f.request, back); !errors.Is(err, ErrRollback) {
		t.Fatal("same-boot monotonic rollback accepted", err)
	}
	for _, kind := range []string{"synthetic", "unknown", "request-boundary", "revocation-boundary", "certificate-boundary"} {
		t.Run(kind, func(t *testing.T) {
			w := f.window
			req := f.request
			switch kind {
			case "synthetic":
				w.Domain = "simulation"
			case "unknown":
				w.Authenticated = false
			case "request-boundary":
				w.Latest = time.UnixMilli(req.ExpiresAtUnixMS)
			case "revocation-boundary":
				w.Latest = time.UnixMilli(f.revocations.ExpiresAtUnixMS)
			case "certificate-boundary":
				req.Identity.ValidUntil = w.Latest
			}
			if err := f.store.Check(req, w); err == nil {
				t.Fatal("invalid interval admitted", kind)
			}
		})
	}
	// Refreshing stale deny state requires a higher valid snapshot, not wall time.
	freshWindow := f.window
	freshWindow.Earliest = freshWindow.Earliest.Add(6 * time.Minute)
	freshWindow.Latest = freshWindow.Earliest.Add(time.Millisecond)
	freshWindow.Monotonic += 6 * time.Minute
	fresh := f.revocations
	fresh.Revision++
	fresh.IssuedAtUnixMS = freshWindow.Earliest.Add(-time.Second).UnixMilli()
	fresh.ExpiresAtUnixMS = freshWindow.Latest.Add(5 * time.Minute).UnixMilli()
	if err := f.store.ApplyRevocations(proof(t, RevocationsTyp, fresh, f.revocationSigner), freshWindow); err != nil {
		t.Fatal("valid fresh snapshot could not replace expired one", err)
	}
}

func TestStrictPolicyScopeAndMalformedLedger(t *testing.T) {
	f := newFixture(t)
	for _, kind := range []string{"simulation", "site", "scope", "duplicate-principal", "issuer-key", "expired", "future"} {
		t.Run(kind, func(t *testing.T) {
			p := f.policy
			p.Revision++
			switch kind {
			case "simulation":
				p.Domain = "simulation"
			case "site":
				p.Site = "elsewhere"
			case "scope":
				p.Entries = []PolicyEntry{{f.user.Principal, []string{"world.move"}}}
			case "duplicate-principal":
				p.Entries = append(append([]PolicyEntry{}, p.Entries...), p.Entries[0])
			case "issuer-key":
				p.IssuerKeySHA256 = f.request.Identity.SigningKeySHA256
			case "expired":
				p.ExpiresAtUnixMS = f.window.Earliest.UnixMilli()
			case "future":
				p.IssuedAtUnixMS = f.window.Latest.Add(time.Second).UnixMilli()
			}
			if err := f.store.ApplyPolicy(proof(t, PolicyTyp, p, f.policySigner), f.window); err == nil {
				t.Fatal("invalid policy accepted", kind)
			}
		})
	}
	payload, _ := json.Marshal(f.policy)
	payload = append([]byte(`{"revision":2,`), payload[1:]...)
	// The honest signing helper rejects duplicates too. Sign the exact malformed
	// bytes directly to exercise hostile-but-cryptographically-valid input.
	parts := strings.Split(proof(t, PolicyTyp, f.policy, f.policySigner), ".")
	input := parts[0] + "." + base64.RawURLEncoding.EncodeToString(payload)
	signature, err := f.policySigner.Key.Sign(nil, []byte(input), &mldsa.Options{})
	if err != nil {
		t.Fatal(err)
	}
	malformed := input + "." + base64.RawURLEncoding.EncodeToString(signature)
	if err = f.store.ApplyPolicy(malformed, f.window); err == nil {
		t.Fatal("duplicate signed policy field accepted")
	}
	if err = f.store.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(f.path)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(f.path, data[:len(data)/2], 0600); err != nil {
		t.Fatal(err)
	}
	if s, err := Open(f.path, f.cfg); err == nil {
		_ = s.Close()
		t.Fatal("truncated ledger accepted")
	}
}

func TestLedgerSpecialFileRefusedWithoutWaiting(t *testing.T) {
	f := newFixture(t)
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "ledger-fifo")
	if err := syscall.Mkfifo(path, 0600); err != nil {
		t.Fatal(err)
	}
	if s, err := Open(path, f.cfg); err == nil {
		_ = s.Close()
		t.Fatal("FIFO ledger accepted")
	}
}

func TestNonceQuotaConcurrentAdmissionAndPostCommitExpiry(t *testing.T) {
	f := newFixture(t)
	var accepted atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := f.store.Admit(f.request, f.window)
			if err == nil {
				accepted.Add(1)
			} else if !errors.Is(err, ErrReplay) {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if accepted.Load() != 1 {
		t.Fatal("concurrent duplicate admitted more than once", accepted.Load())
	}
	end := f.window
	end.Latest = time.UnixMilli(f.request.ExpiresAtUnixMS)
	if err := f.store.Check(f.request, end); err == nil {
		t.Fatal("postcommit expiry did not refuse result")
	}
	if err := f.store.Admit(f.request, f.window); !errors.Is(err, ErrReplay) {
		t.Fatal("refused postcommit result released consumed nonce", err)
	}
	// A separately commissioned small ledger fails closed rather than evicting
	// live nonces to make space for a new request.
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	f.cfg.MaxNonces = 1
	f.path = filepath.Join(t.TempDir(), "small.json")
	var err error
	f.store, err = Open(f.path, f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.store.ApplyPolicy(proof(t, PolicyTyp, f.policy, f.policySigner), f.window); err != nil {
		t.Fatal(err)
	}
	if err = f.store.ApplyRevocations(proof(t, RevocationsTyp, f.revocations, f.revocationSigner), f.window); err != nil {
		t.Fatal(err)
	}
	if err = f.store.Admit(f.request, f.window); err != nil {
		t.Fatal(err)
	}
	other := f.request
	nonce := make([]byte, 24)
	nonce[0] = 1
	other.Nonce = base64.RawURLEncoding.EncodeToString(nonce)
	if err = f.store.Admit(other, f.window); !errors.Is(err, ErrCapacity) {
		t.Fatal("quota evicted a live nonce", err)
	}
}

func TestOneOperatorMayHoldBothExplicitReadControlRoles(t *testing.T) {
	for _, kind := range []string{"principal", "key"} {
		t.Run(kind, func(t *testing.T) {
			f := newFixture(t)
			if err := f.store.Close(); err != nil {
				t.Fatal(err)
			}
			f.cfg.RevocationIssuer = f.cfg.PolicyIssuer
			f.revocationSigner = f.policySigner
			f.revocations.IssuerPrincipal = f.cfg.RevocationIssuer.Principal
			f.revocations.IssuerKeySHA256 = f.cfg.RevocationIssuer.SigningKeySHA256
			f.path = filepath.Join(t.TempDir(), "shared-role-ledger.json")
			var err error
			f.store, err = Open(f.path, f.cfg)
			if err != nil {
				t.Fatal("explicit shared read roles refused", err)
			}
			if err = f.store.ApplyPolicy(proof(t, PolicyTyp, f.policy, f.policySigner), f.window); err != nil {
				t.Fatal(err)
			}
			if err = f.store.ApplyRevocations(proof(t, RevocationsTyp, f.revocations, f.revocationSigner), f.window); err != nil {
				t.Fatal(err)
			}
			if err = f.store.Admit(f.request, f.window); err != nil {
				t.Fatal("shared-role policy did not admit listed reader", err)
			}
			selfRevoked := f.revocations
			selfRevoked.Revision++
			if kind == "principal" {
				selfRevoked.RevokedPrincipals = []string{f.cfg.PolicyIssuer.Principal}
			} else {
				selfRevoked.RevokedKeySHA256 = []string{f.cfg.PolicyIssuer.SigningKeySHA256}
			}
			if err = f.store.ApplyRevocations(proof(t, RevocationsTyp, selfRevoked, f.revocationSigner), f.window); err != nil {
				t.Fatal(err)
			}
			f.reopen(t)
			if err = f.store.Check(f.request, f.window); !errors.Is(err, ErrDenied) {
				t.Fatal("self-revocation did not deny shared issuer's policy", err)
			}
			clear := f.revocations
			clear.Revision = selfRevoked.Revision + 1
			if err = f.store.ApplyRevocations(proof(t, RevocationsTyp, clear, f.revocationSigner), f.window); !errors.Is(err, ErrDenied) {
				t.Fatal("shared issuer cleared its own denial", err)
			}
			later := f.window
			later.Earliest = later.Earliest.Add(6 * time.Minute)
			later.Latest = later.Latest.Add(6 * time.Minute)
			later.Monotonic += 6 * time.Minute
			clear.IssuedAtUnixMS = later.Earliest.Add(-time.Second).UnixMilli()
			clear.ExpiresAtUnixMS = later.Latest.Add(time.Minute).UnixMilli()
			if err = f.store.ApplyRevocations(proof(t, RevocationsTyp, clear, f.revocationSigner), later); !errors.Is(err, ErrDenied) {
				t.Fatal("expired snapshot let shared issuer clear its denial", err)
			}
			for _, missing := range []string{"policy", "revocation"} {
				cfg := f.cfg
				if missing == "policy" {
					cfg.PolicyIssuer = Pin{}
				} else {
					cfg.RevocationIssuer = Pin{}
				}
				if s, e := Open(filepath.Join(t.TempDir(), "missing-role.json"), cfg); e == nil {
					_ = s.Close()
					t.Fatal("implicit role inferred from other pin", missing)
				}
			}
		})
	}
}
