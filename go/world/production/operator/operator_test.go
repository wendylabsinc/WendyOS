package operator

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/production/admission"
	"github.com/wendylabsinc/wendy/go/world/production/internal/testidentity"
	"github.com/wendylabsinc/wendy/go/world/production/jws"
	"github.com/wendylabsinc/wendy/go/world/production/request"
	"github.com/wendylabsinc/wendy/go/world/production/trustedtime"
	"github.com/wendylabsinc/wendy/go/world/production/wendycert"
	"golang.org/x/sys/unix"
)

var testNow = time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)

func testWindow() trustedtime.Interval {
	return trustedtime.Interval{Earliest: testNow, Latest: testNow.Add(100 * time.Millisecond), BootID: "unit-test-boot", Monotonic: time.Second, Domain: "physical", Authenticated: true, Source: "test-only-authenticated-clock"}
}

type fakeClock struct {
	window trustedtime.Interval
	calls  int
	after  func(trustedtime.Interval) trustedtime.Interval
	err    error
}

func (c *fakeClock) Now() (trustedtime.Interval, error) {
	c.calls++
	w := c.window
	if c.calls > 1 && c.after != nil {
		w = c.after(w)
	}
	return w, c.err
}

type fakeCustody struct {
	calls   int
	account string
	get     func() ([]byte, error)
}

func (c *fakeCustody) Get(_ context.Context, account string) ([]byte, error) {
	c.calls++
	c.account = account
	return c.get()
}
func pemCert(der []byte) string {
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}
func pemKey(t testing.TB, key any) string {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(der)
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
}
func record(t testing.TB, f *testidentity.Fixture) map[string]any {
	return map[string]any{"pemCertificate": pemCert(f.ChainDER[0]), "pemCertificateChain": pemCert(f.ChainDER[1]) + pemCert(f.RootDER), "pemPrivateKey": pemKey(t, f.Key), "organizationId": "unused-legacy-metadata", "userId": "unused-user", "principalUri": f.Principal}
}
func config(record map[string]any) map[string]any {
	return map[string]any{"currentContext": "wrong-default", "apiKey": "TEST-TOKEN-NOT-REAL", "auth": []any{map[string]any{"name": "chosen", "cloudGRPC": "example.invalid:443", "apiKey": "TEST-TOKEN-NOT-REAL", "certificates": []any{record}}, map[string]any{"name": "wrong-default", "certificates": []any{}}}}
}
func writeConfig(t testing.TB, path string, cfg map[string]any) {
	t.Helper()
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(data)
	if err = os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}
func setup(t testing.TB) (*testidentity.Fixture, Selection, Signer, map[string]any) {
	t.Helper()
	f := testidentity.New(t, testNow)
	path := filepath.Join(t.TempDir(), "fixture.json")
	cfg := config(record(t, f))
	writeConfig(t, path, cfg)
	v, err := wendycert.New(wendycert.Config{Tenant: f.Tenant, Roots: [][]byte{f.RootDER}})
	if err != nil {
		t.Fatal(err)
	}
	return f, Selection{ConfigPath: path, Context: "chosen", Tenant: f.Tenant, Principal: f.Principal}, Signer{Certificates: v, Clock: &fakeClock{window: testWindow()}}, cfg
}
func chosenRecord(cfg map[string]any) map[string]any {
	return cfg["auth"].([]any)[0].(map[string]any)["certificates"].([]any)[0].(map[string]any)
}
func verifyRead(t testing.TB, s Signer, sel Selection, proof string, body []byte) request.Proof {
	t.Helper()
	w := testWindow()
	p, err := request.Verify(proof, request.Expected{Tenant: sel.Tenant, Site: "office", Audience: "local-read", Operation: request.OperationRead, Method: request.Method, Path: request.Path, PolicyRevision: 7}, body, s.Certificates, w.Earliest, w.Latest)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestInlineReadAndMetadataOnly(t *testing.T) {
	_, sel, s, _ := setup(t)
	before, _ := os.ReadFile(sel.ConfigPath)
	meta, err := Describe(sel)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(meta)
	if meta.Context != "chosen" || meta.KeyStorage != "inline-pkcs8" || !strings.Contains(meta.Status, "not checked") {
		t.Fatalf("unexpected public metadata: %+v", meta)
	}
	for _, secret := range []string{"TEST-TOKEN-NOT-REAL", "pemPrivateKey", sel.ConfigPath, "PRIVATE KEY", "organizationId"} {
		if bytes.Contains(b, []byte(secret)) {
			t.Fatal("metadata leaked credential fields")
		}
	}
	body := []byte(`{"limit":10}`)
	one, err := s.SignRead(context.Background(), sel, ReadTarget{"office", "local-read", 7}, body)
	if err != nil {
		t.Fatal(err)
	}
	p := verifyRead(t, s, sel, one, body)
	two, err := s.SignRead(context.Background(), sel, ReadTarget{"office", "local-read", 7}, body)
	if err != nil {
		t.Fatal(err)
	}
	p2 := verifyRead(t, s, sel, two, body)
	if p.Descriptor.Nonce == p2.Descriptor.Nonce {
		t.Fatal("nonce reused")
	}
	if p.Descriptor.ExpiresAtUnixMS-p.Descriptor.IssuedAtUnixMS > 60000 {
		t.Fatal("read validity exceeds bound")
	}
	after, _ := os.ReadFile(sel.ConfigPath)
	if !bytes.Equal(before, after) {
		t.Fatal("credential configuration changed")
	}
}
func TestKeychainReadAndNoCaching(t *testing.T) {
	f, sel, s, cfg := setup(t)
	key := pemKey(t, f.Key)
	chosenRecord(cfg)["pemPrivateKey"] = "keychain:v1:key-0123456789abcdef"
	writeConfig(t, sel.ConfigPath, cfg)
	custody := &fakeCustody{get: func() ([]byte, error) { return []byte(key), nil }}
	s.Keychain = custody
	m, err := Describe(sel)
	if err != nil || m.KeyStorage != "macos-keychain" || custody.calls != 0 {
		t.Fatal("describe accessed custody or failed")
	}
	for i := 0; i < 2; i++ {
		if _, err = s.SignRead(context.Background(), sel, ReadTarget{"office", "local-read", 7}, []byte(`{}`)); err != nil {
			t.Fatal(err)
		}
	}
	if custody.calls != 2 || custody.account != "key-0123456789abcdef" {
		t.Fatal("custody lookup not fresh and explicit")
	}
}
func TestSelectionFailures(t *testing.T) {
	tests := map[string]func(*Selection, map[string]any){
		"missing context":              func(s *Selection, _ map[string]any) { s.Context = "missing" },
		"unnamed context no migration": func(s *Selection, c map[string]any) { c["auth"].([]any)[0].(map[string]any)["name"] = "" },
		"duplicate context":            func(s *Selection, c map[string]any) { c["auth"] = append(c["auth"].([]any), c["auth"].([]any)[0]) },
		"duplicate identity rotation": func(s *Selection, c map[string]any) {
			entry := c["auth"].([]any)[0].(map[string]any)
			entry["certificates"] = append(entry["certificates"].([]any), chosenRecord(c))
		},
		"metadata mismatch":      func(s *Selection, c map[string]any) { chosenRecord(c)["principalUri"] = s.Principal + "-other" },
		"foreign tenant":         func(s *Selection, _ map[string]any) { s.Tenant = "22222222-2222-4222-8222-222222222222" },
		"noncanonical principal": func(s *Selection, _ map[string]any) { s.Principal += "?read=1" },
		"unknown keychain reference": func(s *Selection, c map[string]any) {
			chosenRecord(c)["pemPrivateKey"] = "keychain:v1:token-0123456789abcdef"
		},
		"key preamble": func(s *Selection, c map[string]any) {
			r := chosenRecord(c)
			r["pemPrivateKey"] = "skipped text\n" + r["pemPrivateKey"].(string)
		},
		"key trailer": func(s *Selection, c map[string]any) {
			r := chosenRecord(c)
			r["pemPrivateKey"] = r["pemPrivateKey"].(string) + "trailer"
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			_, sel, s, cfg := setup(t)
			mutate(&sel, cfg)
			writeConfig(t, sel.ConfigPath, cfg)
			if _, err := s.SignRead(context.Background(), sel, ReadTarget{"office", "local-read", 7}, []byte(`{}`)); err == nil {
				t.Fatal("invalid selection accepted")
			}
		})
	}
	t.Run("missing legacy principal resolved without write", func(t *testing.T) {
		_, sel, _, cfg := setup(t)
		delete(chosenRecord(cfg), "principalUri")
		writeConfig(t, sel.ConfigPath, cfg)
		before, _ := os.ReadFile(sel.ConfigPath)
		m, err := Describe(sel)
		after, _ := os.ReadFile(sel.ConfigPath)
		if err != nil || m.Principal != sel.Principal || !bytes.Equal(before, after) {
			t.Fatal("SAN selection failed or migrated configuration")
		}
	})
}
func TestTrustAndRotationBeforeOutput(t *testing.T) {
	for _, name := range []string{"wrong root", "expired certificate", "untrusted time", "wrong key", "EC fallback", "changed configuration", "custody denial"} {
		t.Run(name, func(t *testing.T) {
			f, sel, s, cfg := setup(t)
			key := pemKey(t, f.Key)
			chosenRecord(cfg)["pemPrivateKey"] = "keychain:v1:key-0123456789abcdef"
			writeConfig(t, sel.ConfigPath, cfg)
			custody := &fakeCustody{get: func() ([]byte, error) { return []byte(key), nil }}
			s.Keychain = custody
			switch name {
			case "wrong root":
				other := testidentity.New(t, testNow)
				s.Certificates, _ = wendycert.New(wendycert.Config{Tenant: f.Tenant, Roots: [][]byte{other.RootDER}})
			case "expired certificate":
				s.Clock = &fakeClock{window: func() trustedtime.Interval {
					w := testWindow()
					w.Earliest = w.Earliest.Add(2 * time.Hour)
					w.Latest = w.Latest.Add(2 * time.Hour)
					return w
				}()}
			case "untrusted time":
				w := testWindow()
				w.Authenticated = false
				s.Clock = &fakeClock{window: w}
			case "wrong key":
				other := testidentity.New(t, testNow)
				key = pemKey(t, other.Key)
			case "EC fallback":
				ec, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
				if err != nil {
					t.Fatal(err)
				}
				key = pemKey(t, ec)
			case "changed configuration":
				custody.get = func() ([]byte, error) {
					cfg["newSetting"] = true
					writeConfig(t, sel.ConfigPath, cfg)
					return []byte(key), nil
				}
			case "custody denial":
				custody.get = func() ([]byte, error) { return []byte("must-clear"), errors.New("SECRET diagnostic") }
			}
			proof, err := s.SignRead(context.Background(), sel, ReadTarget{"office", "local-read", 7}, []byte(`{}`))
			if err == nil || proof != "" {
				t.Fatal("invalid identity returned proof")
			}
			if strings.Contains(err.Error(), "SECRET") {
				t.Fatal("custody error leaked")
			}
			if (name == "wrong root" || name == "expired certificate" || name == "untrusted time") && custody.calls != 0 {
				t.Fatal("custody accessed before trust/time check")
			}
		})
	}
}
func TestClockRecheck(t *testing.T) {
	for _, kind := range []string{"expired", "boot changed", "monotonic rollback", "interval rollback", "width invalid"} {
		t.Run(kind, func(t *testing.T) {
			_, sel, s, _ := setup(t)
			s.Clock = &fakeClock{window: testWindow(), after: func(w trustedtime.Interval) trustedtime.Interval {
				switch kind {
				case "expired":
					w.Earliest = w.Earliest.Add(2 * time.Minute)
					w.Latest = w.Latest.Add(2 * time.Minute)
				case "boot changed":
					w.BootID = "other"
				case "monotonic rollback":
					w.Monotonic = 0
				case "interval rollback":
					w.Earliest = w.Earliest.Add(-time.Second)
				case "width invalid":
					w.Latest = w.Latest.Add(25 * time.Hour)
				}
				return w
			}}
			if proof, err := s.SignRead(context.Background(), sel, ReadTarget{"office", "local-read", 7}, []byte(`{}`)); err == nil || proof != "" {
				t.Fatal("clock fault returned a proof")
			}
		})
	}
}
func TestTargetBoundsBeforeCustody(t *testing.T) {
	_, sel, s, cfg := setup(t)
	chosenRecord(cfg)["pemPrivateKey"] = "keychain:v1:key-0123456789abcdef"
	writeConfig(t, sel.ConfigPath, cfg)
	custody := &fakeCustody{get: func() ([]byte, error) { t.Fatal("invalid target opened custody"); return nil, nil }}
	s.Keychain = custody
	calls := []func() (string, error){
		func() (string, error) {
			return s.SignRead(context.Background(), sel, ReadTarget{"bad site", "local-read", 7}, []byte(`{}`))
		},
		func() (string, error) {
			return s.SignRead(context.Background(), sel, ReadTarget{"office", "local-read", 0}, []byte(`{}`))
		},
		func() (string, error) {
			return s.SignRead(context.Background(), sel, ReadTarget{"office", "local-read", 7}, make([]byte, 65537))
		},
		func() (string, error) {
			return s.SignReadPolicy(context.Background(), sel, PolicyTarget{"office", "local-read", "bad policy", 1, time.Minute})
		},
		func() (string, error) {
			return s.SignReadPolicy(context.Background(), sel, PolicyTarget{"office", "local-read", "policy", 1, 11 * time.Minute})
		},
		func() (string, error) {
			return s.SignRevocations(context.Background(), sel, RevocationTarget{Site: "office", Audience: "local-read", PolicyID: "unused", Revision: 1, Lifetime: time.Minute})
		},
	}
	for i, call := range calls {
		if _, err := call(); err == nil {
			t.Fatalf("invalid target %d accepted", i)
		}
	}
}
func TestCommissioningSelfOnly(t *testing.T) {
	_, sel, s, _ := setup(t)
	w := testWindow()
	proof, err := s.SignReadPolicy(context.Background(), sel, PolicyTarget{"office", "local-read", "local-policy", 3, 10 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	raw, id, err := jws.Verify(proof, admission.PolicyTyp, s.Certificates, w.Earliest, w.Latest)
	if err != nil {
		t.Fatal(err)
	}
	var policy admission.Policy
	if err = jws.DecodeStrict(raw, &policy); err != nil {
		t.Fatal(err)
	}
	if len(policy.Entries) != 1 || policy.Entries[0].Principal != sel.Principal || !reflect.DeepEqual(policy.Entries[0].Scopes, []string{admission.ReadScope}) || policy.IssuerKeySHA256 != id.SigningKeySHA256 || policy.ExpiresAtUnixMS-policy.IssuedAtUnixMS > 600000 {
		t.Fatal("commissioning escaped self/read/time bounds")
	}
	proof, err = s.SignRevocations(context.Background(), sel, RevocationTarget{Site: "office", Audience: "local-read", PolicyID: "local-policy", Revision: 4, Lifetime: time.Minute, RevokeSelf: true, RevokeKey: true, RevokeCertificate: true, RevokePolicy: true})
	if err != nil {
		t.Fatal(err)
	}
	raw, id, err = jws.Verify(proof, admission.RevocationsTyp, s.Certificates, w.Earliest, w.Latest)
	if err != nil {
		t.Fatal(err)
	}
	var r admission.Revocations
	if err = jws.DecodeStrict(raw, &r); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(r.RevokedPrincipals, []string{sel.Principal}) || !reflect.DeepEqual(r.RevokedKeySHA256, []string{id.SigningKeySHA256}) || !reflect.DeepEqual(r.RevokedCertificateSHA256, []string{id.CertificateSHA256}) || !reflect.DeepEqual(r.RevokedPolicyIDs, []string{"local-policy"}) {
		t.Fatal("revocation output escaped explicit self selections")
	}
	proof, err = s.SignRevocations(context.Background(), sel, RevocationTarget{Site: "office", Audience: "local-read", Revision: 5, Lifetime: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	raw, _, _ = jws.Verify(proof, admission.RevocationsTyp, s.Certificates, w.Earliest, w.Latest)
	if err = jws.DecodeStrict(raw, &r); err != nil {
		t.Fatal(err)
	}
	if len(r.RevokedPrincipals)+len(r.RevokedKeySHA256)+len(r.RevokedCertificateSHA256)+len(r.RevokedPolicyIDs) != 0 {
		t.Fatal("bootstrap snapshot unexpectedly revoked identities")
	}
}
func TestRegularFileAndJSONBounds(t *testing.T) {
	for _, kind := range []string{"symlink", "fifo", "permissions", "oversize", "duplicate JSON", "invalid surrogate"} {
		t.Run(kind, func(t *testing.T) {
			_, sel, _, _ := setup(t)
			switch kind {
			case "symlink":
				original := sel.ConfigPath
				sel.ConfigPath += "-link"
				if err := os.Symlink(original, sel.ConfigPath); err != nil {
					t.Fatal(err)
				}
			case "fifo":
				sel.ConfigPath += "-pipe"
				if err := unix.Mkfifo(sel.ConfigPath, 0600); err != nil {
					t.Fatal(err)
				}
			case "permissions":
				if err := os.Chmod(sel.ConfigPath, 0644); err != nil {
					t.Fatal(err)
				}
			case "oversize":
				if err := os.WriteFile(sel.ConfigPath, make([]byte, MaxConfigBytes+1), 0600); err != nil {
					t.Fatal(err)
				}
			case "duplicate JSON":
				if err := os.WriteFile(sel.ConfigPath, []byte(`{"auth":[],"auth":[]}`), 0600); err != nil {
					t.Fatal(err)
				}
			case "invalid surrogate":
				if err := os.WriteFile(sel.ConfigPath, []byte(`{"ignored":"\ud800","auth":[]}`), 0600); err != nil {
					t.Fatal(err)
				}
			}
			done := make(chan error, 1)
			go func() { _, err := Describe(sel); done <- err }()
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("invalid file accepted")
				}
			case <-time.After(time.Second):
				t.Fatal("nonregular input blocked")
			}
		})
	}
	f, sel, _, _ := setup(t)
	path := filepath.Join(filepath.Dir(sel.ConfigPath), "roots.pem")
	if err := os.WriteFile(path, []byte(pemCert(f.RootDER)), 0600); err != nil {
		t.Fatal(err)
	}
	roots, err := LoadTrustRoots(path)
	if err != nil || len(roots) != 1 || !bytes.Equal(roots[0], f.RootDER) {
		t.Fatal("explicit public root load failed")
	}
	if err = os.WriteFile(path, []byte(pemCert(f.ChainDER[0])), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = LoadTrustRoots(path); err == nil {
		t.Fatal("leaf silently accepted as root")
	}
	if err = os.WriteFile(path, make([]byte, 65537), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = ReadBodyFile(path); err == nil {
		t.Fatal("oversized body accepted")
	}
}
func TestPinnedKeychainReadProtocol(t *testing.T) {
	expected := []string{"find-generic-password", "-s", "wendy-credentials", "-a", "key-0123456789abcdef", "-w"}
	k := commandKeychain{run: func(ctx context.Context, args ...string) ([]byte, error) {
		if !reflect.DeepEqual(args, expected) {
			t.Fatal("custody arguments differ")
		}
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 5*time.Second {
			t.Fatal("custody lookup unbounded")
		}
		return []byte(base64.StdEncoding.EncodeToString([]byte("generated-test-value")) + "\n"), nil
	}}
	value, err := k.Get(context.Background(), "key-0123456789abcdef")
	if err != nil || string(value) != "generated-test-value" {
		t.Fatal("existing keychain wire encoding not supported")
	}
	clear(value)
	for _, kind := range []string{"encoding", "oversized", "failure", "cancelled"} {
		t.Run(kind, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			k.run = func(_ context.Context, _ ...string) ([]byte, error) {
				switch kind {
				case "encoding":
					return []byte("not-base64?"), nil
				case "oversized":
					return make([]byte, base64.StdEncoding.EncodedLen(MaxKeyPEMBytes)+129), nil
				case "failure":
					return nil, errors.New("SECRET-CUSTODY-ERROR")
				}
				return nil, nil
			}
			if kind == "cancelled" {
				cancel()
			}
			_, err := k.Get(ctx, "key-0123456789abcdef")
			if err == nil || strings.Contains(err.Error(), "SECRET") {
				t.Fatal("invalid lookup accepted or leaked diagnostics")
			}
		})
	}
	if _, err = k.Get(context.Background(), "oauth-token"); err == nil {
		t.Fatal("non-key account accepted")
	}
}

func TestIndependentIntervalsAndFixedExpiry(t *testing.T) {
	_, sel, s, _ := setup(t)
	body := []byte(`{}`)
	w := testWindow()
	w.Latest = w.Earliest.Add(5 * time.Second)
	s.Clock = &fakeClock{window: w}
	compact, err := s.SignRead(context.Background(), sel, ReadTarget{"office", "local-read", 7}, body)
	if err != nil {
		t.Fatal(err)
	}
	expected := request.Expected{Tenant: sel.Tenant, Site: "office", Audience: "local-read", Operation: request.OperationRead, Method: request.Method, Path: request.Path, PolicyRevision: 7}
	for _, offset := range []time.Duration{-4 * time.Second, 0, 4 * time.Second, 50 * time.Second} {
		lo := testNow.Add(offset)
		hi := lo.Add(5*time.Second - time.Millisecond)
		proof, err := request.Verify(compact, expected, body, s.Certificates, lo, hi)
		if err != nil {
			t.Fatalf("independent valid interval %s refused: %v", offset, err)
		}
		if proof.Descriptor.IssuedAtUnixMS != testNow.Add(-5*time.Second).UnixMilli() || proof.Descriptor.ExpiresAtUnixMS != testNow.Add(55*time.Second).UnixMilli() {
			t.Fatal("issuance backdate or fixed expiry changed")
		}
	}
	for _, offset := range []time.Duration{-6 * time.Second, 55 * time.Second} {
		lo := testNow.Add(offset)
		if _, err := request.Verify(compact, expected, body, s.Certificates, lo, lo.Add(time.Millisecond)); err == nil {
			t.Fatal("outside fixed signed window accepted")
		}
	}
	w.Latest = w.Latest.Add(time.Nanosecond)
	s.Clock = &fakeClock{window: w}
	if proof, err := s.SignRead(context.Background(), sel, ReadTarget{"office", "local-read", 7}, body); err == nil || proof != "" {
		t.Fatal("over-budget signing uncertainty accepted")
	}
}
