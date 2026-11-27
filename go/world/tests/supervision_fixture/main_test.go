package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/client"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"golang.org/x/sys/unix"
)

var fixtureNow = time.Date(2026, 11, 10, 12, 0, 0, 123000000, time.UTC)

type testFixture struct {
	root        issuer
	credentials client.Config
	options     options
}

func fixture(t *testing.T) testFixture {
	t.Helper()
	rootPub, rootKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	g, err := authority.SignGrant(authority.Grant{Version: 1, ID: "lab/supervisor-b", Tenant: "lab", Site: "synthetic-office", Domain: "simulation", Principal: "lab/supervisor-b", Kind: "operator", Source: world.Source{Device: "supervisor-b", App: "world", Service: "supervisor", Stream: "presence"}, PublicKey: pub, Scopes: []string{"supervise", "stop", "read"}, NotBeforeUnixMS: fixtureNow.Add(-time.Minute).UnixMilli(), NotAfterUnixMS: fixtureNow.Add(59 * time.Minute).UnixMilli()}, rootKey)
	if err != nil {
		t.Fatal(err)
	}
	rev, err := authority.SignRevocations(authority.Revocations{Version: 1, Tenant: "lab", Site: "synthetic-office", Domain: "simulation", Revision: 2, IssuedAtUnixMS: fixtureNow.Add(-time.Second).UnixMilli(), ExpiresAtUnixMS: fixtureNow.Add(59 * time.Minute).UnixMilli(), Principals: []string{"lab/previously-revoked"}, GrantIDs: []string{"old-grant"}, KeyIDs: []string{"old-key"}}, rootKey)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	f := testFixture{root: issuer{Domain: "simulation", PrivateKey: rootKey, PublicKey: rootPub, Revocations: rev}, credentials: client.Config{URL: "http://world-node-1:8080", Audience: "node-1", Grant: g, PrivateKey: key, AllowInsecureSimulation: true}, options: options{issuer: filepath.Join(dir, "issuer.json"), credentials: filepath.Join(dir, "supervisor.json"), out: filepath.Join(dir, "new.json"), ttl: 5 * time.Second}}
	f.save(t)
	return f
}
func writeFixtureJSON(t *testing.T, path string, v any) {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(data)
	if err = os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}
func (f testFixture) save(t *testing.T) {
	t.Helper()
	writeFixtureJSON(t, f.options.issuer, f.root)
	writeFixtureJSON(t, f.options.credentials, f.credentials)
}
func (f testFixture) verifier(t *testing.T) *authority.Verifier {
	t.Helper()
	v, err := authority.NewVerifier(authority.Config{RootPublicKey: f.root.PublicKey, Tenant: "lab", Site: "synthetic-office", Domain: "simulation", Audience: "node-1", Now: func() time.Time { return fixtureNow }, Replay: authority.NewMemoryReplay(8)})
	if err != nil {
		t.Fatal(err)
	}
	if err = v.UpdateRevocations(f.root.Revocations); err != nil {
		t.Fatal(err)
	}
	return v
}
func readFixtureOutput(t *testing.T, path string, out any) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(data)
	if err = authority.DecodeStrict(data, out); err != nil {
		t.Fatal(err)
	}
	stat, err := os.Stat(path)
	if err != nil || stat.Mode().Perm()&0077 != 0 {
		t.Fatal("fixture output is not private")
	}
}
func TestGrantShorteningPreservesIdentityAndInputFiles(t *testing.T) {
	f := fixture(t)
	beforeRoot, _ := os.ReadFile(f.options.issuer)
	beforeProfile, _ := os.ReadFile(f.options.credentials)
	if err := execute(f.options, fixtureNow); err != nil {
		t.Fatal(err)
	}
	var got client.Config
	readFixtureOutput(t, f.options.out, &got)
	g, want := got.Grant.Grant, f.credentials.Grant.Grant
	if g.NotBeforeUnixMS != fixtureNow.UnixMilli() || g.NotAfterUnixMS != fixtureNow.Add(5*time.Second).UnixMilli() || g.ID == want.ID || !strings.HasPrefix(g.ID, want.ID+"/short-") {
		t.Fatal("grant was not freshly shortened")
	}
	restored := got
	restored.Grant = f.credentials.Grant
	if !reflect.DeepEqual(restored, f.credentials) {
		t.Fatal("profile identity, source, key or endpoint changed")
	}
	g.ID, g.NotBeforeUnixMS, g.NotAfterUnixMS = want.ID, want.NotBeforeUnixMS, want.NotAfterUnixMS
	if !reflect.DeepEqual(g, want) {
		t.Fatal("shortening changed grant authority")
	}
	v := f.verifier(t)
	for _, scope := range []string{"supervise", "stop", "read"} {
		if _, err := v.VerifyGrant(got.Grant, scope); err != nil {
			t.Fatal("new grant is not verifiable", err)
		}
	}
	afterRoot, _ := os.ReadFile(f.options.issuer)
	afterProfile, _ := os.ReadFile(f.options.credentials)
	if !bytes.Equal(beforeRoot, afterRoot) || !bytes.Equal(beforeProfile, afterProfile) {
		t.Fatal("helper changed its inputs")
	}
	f.options.out += "-second"
	if err := execute(f.options, fixtureNow); err != nil {
		t.Fatal(err)
	}
	var second client.Config
	readFixtureOutput(t, f.options.out, &second)
	if second.Grant.Grant.ID == got.Grant.Grant.ID {
		t.Fatal("short grant ID reused")
	}
}
func TestShortGrantCannotExtendOriginalExpiry(t *testing.T) {
	f := fixture(t)
	g := f.credentials.Grant.Grant
	g.NotAfterUnixMS = fixtureNow.Add(2 * time.Second).UnixMilli()
	signed, err := authority.SignGrant(g, f.root.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	f.credentials.Grant = signed
	f.save(t)
	if err = execute(f.options, fixtureNow); err != nil {
		t.Fatal(err)
	}
	var got client.Config
	readFixtureOutput(t, f.options.out, &got)
	if got.Grant.Grant.NotAfterUnixMS != g.NotAfterUnixMS {
		t.Fatal("extended existing grant deadline")
	}
}
func TestRevocationPreservesExistingDenialsAndBindsPrincipal(t *testing.T) {
	f := fixture(t)
	f.options.ttl = 0
	f.options.revoke = true
	if err := execute(f.options, fixtureNow); err != nil {
		t.Fatal(err)
	}
	var got authority.SignedRevocations
	readFixtureOutput(t, f.options.out, &got)
	r, old := got.Revocations, f.root.Revocations.Revocations
	if r.Revision != old.Revision+1 || r.IssuedAtUnixMS != fixtureNow.UnixMilli() || r.ExpiresAtUnixMS != old.ExpiresAtUnixMS || r.Domain != "simulation" || r.Site != old.Site || r.Tenant != old.Tenant || !reflect.DeepEqual(r.GrantIDs, old.GrantIDs) || !reflect.DeepEqual(r.KeyIDs, old.KeyIDs) || !reflect.DeepEqual(r.Principals, append(append([]string(nil), old.Principals...), f.credentials.Grant.Grant.Principal)) {
		t.Fatal("revocation changed scope, erased denials or selected wrong principal")
	}
	v := f.verifier(t)
	if _, err := v.VerifyGrant(f.credentials.Grant, "supervise"); err != nil {
		t.Fatal(err)
	}
	if err := v.UpdateRevocations(got); err != nil {
		t.Fatal(err)
	}
	if _, err := v.VerifyGrant(f.credentials.Grant, "supervise"); err == nil {
		t.Fatal("signed revocation did not deny selected principal")
	}
}
func TestUnsafeCredentialsAndAuthorityRefused(t *testing.T) {
	cases := []string{"physical issuer", "physical grant", "wrong root", "broad scope", "missing read", "missing stop", "wrong key", "inconsistent seed", "expired grant", "future grant", "revoked principal", "stale revocations", "foreign tenant", "non-lab principal", "write binding", "insecure disabled"}
	for _, kind := range cases {
		t.Run(kind, func(t *testing.T) {
			f := fixture(t)
			g := f.credentials.Grant.Grant
			switch kind {
			case "physical issuer":
				f.root.Domain = "physical"
			case "physical grant":
				g.Domain = "physical"
			case "wrong root":
				pub, key, err := ed25519.GenerateKey(rand.Reader)
				if err != nil {
					t.Fatal(err)
				}
				f.root.PublicKey, f.root.PrivateKey = pub, key
				f.root.Revocations, err = authority.SignRevocations(f.root.Revocations.Revocations, key)
				if err != nil {
					t.Fatal(err)
				}
			case "broad scope":
				g.Scopes = append(g.Scopes, "mission")
			case "missing read":
				g.Scopes = []string{"supervise", "stop"}
			case "missing stop":
				g.Scopes = []string{"supervise", "read"}
			case "wrong key":
				_, key, err := ed25519.GenerateKey(rand.Reader)
				if err != nil {
					t.Fatal(err)
				}
				f.credentials.PrivateKey = key
			case "inconsistent seed":
				f.credentials.PrivateKey[0] ^= 1
			case "expired grant":
				g.NotBeforeUnixMS = fixtureNow.Add(-time.Minute).UnixMilli()
				g.NotAfterUnixMS = fixtureNow.UnixMilli()
			case "future grant":
				g.NotBeforeUnixMS = fixtureNow.Add(time.Second).UnixMilli()
			case "revoked principal":
				r := f.root.Revocations.Revocations
				r.Principals = append(r.Principals, g.Principal)
				var err error
				f.root.Revocations, err = authority.SignRevocations(r, f.root.PrivateKey)
				if err != nil {
					t.Fatal(err)
				}
			case "stale revocations":
				r := f.root.Revocations.Revocations
				r.IssuedAtUnixMS = fixtureNow.Add(-time.Minute).UnixMilli()
				r.ExpiresAtUnixMS = fixtureNow.UnixMilli()
				var err error
				f.root.Revocations, err = authority.SignRevocations(r, f.root.PrivateKey)
				if err != nil {
					t.Fatal(err)
				}
			case "foreign tenant":
				g.Tenant = "other"
			case "non-lab principal":
				g.Principal = "real/operator"
			case "write binding":
				g.PublishAudience = "node-1"
			case "insecure disabled":
				f.credentials.AllowInsecureSimulation = false
			}
			// Where representable, use validly signed forbidden grants to test policy
			// rejection rather than relying only on a broken signature.
			if kind != "wrong root" {
				if signed, err := authority.SignGrant(g, f.root.PrivateKey); err == nil {
					f.credentials.Grant = signed
				} else {
					f.credentials.Grant.Grant = g
				}
			}
			f.save(t)
			if err := execute(f.options, fixtureNow); err == nil {
				t.Fatal("unsafe fixture input accepted")
			}
			if _, err := os.Stat(f.options.out); !os.IsNotExist(err) {
				t.Fatal("failure left an output file")
			}
		})
	}
}
func TestExclusiveOutputsNeverReplaceExistingFiles(t *testing.T) {
	for _, kind := range []string{"regular", "symlink", "fifo", "input"} {
		t.Run(kind, func(t *testing.T) {
			f := fixture(t)
			marker := []byte("existing fixture data")
			target := filepath.Join(filepath.Dir(f.options.out), "existing")
			if err := os.WriteFile(target, marker, 0600); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "regular":
				if err := os.WriteFile(f.options.out, marker, 0600); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Symlink(target, f.options.out); err != nil {
					t.Fatal(err)
				}
			case "fifo":
				if err := unix.Mkfifo(f.options.out, 0600); err != nil {
					t.Fatal(err)
				}
			case "input":
				f.options.out = f.options.credentials
			}
			before, _ := os.ReadFile(f.options.credentials)
			if err := execute(f.options, fixtureNow); err == nil {
				t.Fatal("existing output replaced")
			}
			after, _ := os.ReadFile(f.options.credentials)
			if !bytes.Equal(before, after) {
				t.Fatal("input overwritten")
			}
			after, _ = os.ReadFile(target)
			if !bytes.Equal(marker, after) {
				t.Fatal("symlink target changed")
			}
			if kind == "regular" {
				after, _ = os.ReadFile(f.options.out)
				if !bytes.Equal(marker, after) {
					t.Fatal("existing file changed")
				}
			}
		})
	}
}
func TestConcurrentCreationHasExactlyOneWinner(t *testing.T) {
	f := fixture(t)
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); results <- execute(f.options, fixtureNow) }()
	}
	wg.Wait()
	close(results)
	success := 0
	for err := range results {
		if err == nil {
			success++
		}
	}
	if success != 1 {
		t.Fatal("exclusive output did not select one writer")
	}
	var got client.Config
	readFixtureOutput(t, f.options.out, &got)
	if _, err := f.verifier(t).VerifyGrant(got.Grant, "supervise"); err != nil {
		t.Fatal("winner output invalid", err)
	}
}
func TestBoundedPrivateInputs(t *testing.T) {
	for _, kind := range []string{"symlink", "fifo", "directory", "public permissions", "over budget", "duplicate JSON"} {
		t.Run(kind, func(t *testing.T) {
			f := fixture(t)
			switch kind {
			case "symlink":
				path := f.options.issuer + ".link"
				if err := os.Symlink(f.options.issuer, path); err != nil {
					t.Fatal(err)
				}
				f.options.issuer = path
			case "fifo":
				f.options.issuer += ".pipe"
				if err := unix.Mkfifo(f.options.issuer, 0600); err != nil {
					t.Fatal(err)
				}
			case "directory":
				f.options.issuer = filepath.Dir(f.options.issuer)
			case "public permissions":
				if err := os.Chmod(f.options.credentials, 0644); err != nil {
					t.Fatal(err)
				}
			case "over budget":
				if err := os.WriteFile(f.options.issuer, make([]byte, maxInputBytes+1), 0600); err != nil {
					t.Fatal(err)
				}
			case "duplicate JSON":
				if err := os.WriteFile(f.options.issuer, []byte(`{"domain":"simulation","domain":"physical"}`), 0600); err != nil {
					t.Fatal(err)
				}
			}
			done := make(chan error, 1)
			go func() { done <- execute(f.options, fixtureNow) }()
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("unsafe file accepted")
				}
			case <-time.After(time.Second):
				t.Fatal("nonregular file blocked")
			}
			if _, err := os.Stat(f.options.out); !os.IsNotExist(err) {
				t.Fatal("unsafe input left output")
			}
		})
	}
}
func TestOptionBounds(t *testing.T) {
	for _, mutate := range []func(*options){func(o *options) { o.ttl = 0 }, func(o *options) { o.ttl = time.Minute + time.Millisecond }, func(o *options) { o.revoke = true }, func(o *options) { o.issuer = "" }, func(o *options) { o.credentials = "" }, func(o *options) { o.out = "" }} {
		f := fixture(t)
		mutate(&f.options)
		if err := execute(f.options, fixtureNow); err == nil {
			t.Fatal("invalid options accepted")
		}
	}
}

func missionIssuerFixture(t *testing.T) testFixture {
	t.Helper()
	f := fixture(t)
	g := f.credentials.Grant.Grant
	g.ID, g.Principal = "lab/review-run/sim-operator", "lab/sim-operator"
	g.Source = world.Source{Device: "sim-operator", App: "wendy-world", Service: "sim-operator", Stream: "state"}
	g.Scopes = []string{"mission", "stop", "read"}
	var err error
	f.credentials.Grant, err = authority.SignGrant(g, f.root.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	f.options.ttl = 0
	f.options.revokeMissionIssuer = true
	f.save(t)
	return f
}

func TestMissionIssuerRevocationPreservesScopeDenialsExpiryAndInputs(t *testing.T) {
	f := missionIssuerFixture(t)
	beforeRoot, err := os.ReadFile(f.options.issuer)
	if err != nil {
		t.Fatal(err)
	}
	beforeProfile, err := os.ReadFile(f.options.credentials)
	if err != nil {
		t.Fatal(err)
	}
	if err = execute(f.options, fixtureNow); err != nil {
		t.Fatal(err)
	}
	var got authority.SignedRevocations
	readFixtureOutput(t, f.options.out, &got)
	want := f.root.Revocations.Revocations
	want.Revision++
	want.IssuedAtUnixMS = fixtureNow.UnixMilli()
	want.Principals = append(append([]string(nil), want.Principals...), "lab/sim-operator")
	if !reflect.DeepEqual(got.Revocations, want) {
		t.Fatal("mission revocation changed unrelated denial, authority scope or expiry")
	}
	v := f.verifier(t)
	if _, err = v.VerifyGrant(f.credentials.Grant, "mission"); err != nil {
		t.Fatal("mission issuer was not initially authorized", err)
	}
	if err = v.UpdateRevocations(got); err != nil {
		t.Fatal(err)
	}
	if _, err = v.VerifyGrant(f.credentials.Grant, "mission"); err == nil {
		t.Fatal("mission issuer not revoked")
	}
	for path, before := range map[string][]byte{f.options.issuer: beforeRoot, f.options.credentials: beforeProfile} {
		after, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(after, before) {
			t.Fatal("revocation changed its private inputs", err)
		}
	}
}

func TestMissionIssuerRevocationRejectsModeOrAuthorityBroadening(t *testing.T) {
	for _, kind := range []string{"ttl", "explicit zero ttl", "both revoke modes", "supervisor mode", "shorten mode", "wrong principal", "wrong id", "empty run", "wrong source", "wrong site", "wrong audience", "missing scope", "extra scope", "expired", "revoked", "revision exhausted", "physical", "wrong key", "bad signature"} {
		t.Run(kind, func(t *testing.T) {
			f := missionIssuerFixture(t)
			g := f.credentials.Grant.Grant
			r := f.root.Revocations.Revocations
			switch kind {
			case "ttl":
				f.options.ttl = time.Millisecond
			case "explicit zero ttl":
				f.options.ttlSet = true
			case "both revoke modes":
				f.options.revoke = true
			case "supervisor mode":
				f.options.revokeMissionIssuer, f.options.revoke = false, true
			case "shorten mode":
				f.options.revokeMissionIssuer, f.options.ttl = false, time.Second
			case "wrong principal":
				g.Principal = "lab/other-operator"
			case "wrong id":
				g.ID = "lab/review-run/supervisor-a"
			case "empty run":
				g.ID = "lab//sim-operator"
			case "wrong source":
				g.Source.Stream = "other"
			case "wrong site":
				g.Site, r.Site = "other-office", "other-office"
			case "wrong audience":
				f.credentials.Audience = "node-2"
			case "missing scope":
				g.Scopes = []string{"mission", "stop"}
			case "extra scope":
				g.Scopes = append(g.Scopes, "supervise")
			case "expired":
				g.NotAfterUnixMS = fixtureNow.UnixMilli()
			case "revoked":
				r.Principals = append(r.Principals, g.Principal)
			case "revision exhausted":
				r.Revision = math.MaxUint64
			case "physical":
				g.Domain, r.Domain, f.root.Domain = "physical", "physical", "physical"
			case "wrong key":
				f.credentials.PrivateKey[0] ^= 1
			}
			var err error
			f.credentials.Grant, err = authority.SignGrant(g, f.root.PrivateKey)
			if err != nil {
				t.Fatal(err)
			}
			f.root.Revocations, err = authority.SignRevocations(r, f.root.PrivateKey)
			if err != nil {
				t.Fatal(err)
			}
			if kind == "bad signature" {
				f.credentials.Grant.Signature[0] ^= 1
			}
			f.save(t)
			if err = execute(f.options, fixtureNow); err == nil {
				t.Fatal("unsafe mission revocation accepted")
			}
			if _, err := os.Stat(f.options.out); !os.IsNotExist(err) {
				t.Fatal("refused operation created output")
			}
		})
	}
	// The new explicit mode cannot select an otherwise valid supervisor.
	f := fixture(t)
	f.options.ttl, f.options.revokeMissionIssuer = 0, true
	if err := execute(f.options, fixtureNow); err == nil {
		t.Fatal("mission revocation accepted supervisor credentials")
	}
}
