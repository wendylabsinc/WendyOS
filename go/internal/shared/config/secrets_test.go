package config

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// fakeStore is an in-memory secretstore.Store counting reads.
type fakeStore struct {
	mu      sync.Mutex
	m       map[string][]byte
	gets    int
	putErr  error
	deletes []string
}

func newFakeStore() *fakeStore { return &fakeStore{m: map[string][]byte{}} }

func (s *fakeStore) Get(account string) []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gets++
	return s.m[account]
}

func (s *fakeStore) Put(account string, secret []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.putErr != nil {
		return s.putErr
	}
	s.m[account] = secret
	return nil
}

func (s *fakeStore) Delete(account string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, account)
	s.deletes = append(s.deletes, account)
}

// useFakeStore swaps in a fake credential store and clears the memoization
// cache; restores both on cleanup.
func useFakeStore(t *testing.T, s *fakeStore) {
	t.Helper()
	origNew := newCredentialStore
	newCredentialStore = func() secretStoreIface { return s }
	resetSecretCacheForTest()
	t.Cleanup(func() {
		newCredentialStore = origNew
		resetSecretCacheForTest()
	})
}

func TestAccessorsInlineValues(t *testing.T) {
	useFakeStore(t, newFakeStore())
	c := CertificateInfo{PemPrivateKey: "-----BEGIN PRIVATE KEY-----\nabc"}
	got, err := c.PrivateKeyPEM()
	if err != nil || got != c.PemPrivateKey {
		t.Fatalf("inline PrivateKeyPEM = %q, %v", got, err)
	}
	a := AuthConfig{APIKey: "tok-123"}
	tok, err := a.BearerToken()
	if err != nil || tok != "tok-123" {
		t.Fatalf("inline BearerToken = %q, %v", tok, err)
	}
	if !c.HasPrivateKey() || !a.HasAPIKey() {
		t.Error("Has* = false for inline values")
	}
	if (CertificateInfo{}).HasPrivateKey() || (AuthConfig{}).HasAPIKey() {
		t.Error("Has* = true for empty values")
	}
}

func TestInvalidateCachedSecretsAfterExternalRotation(t *testing.T) {
	store := newFakeStore()
	useFakeStore(t, store)
	refs := []string{"api", "refresh", "key", "cert-key"}
	for _, account := range refs {
		_ = store.Put(account, []byte("old-"+account))
		if _, err := resolveSecret(refPrefixV1 + account); err != nil {
			t.Fatal(err)
		}
		// A different process updates the Keychain; this process's memoized
		// value and the reference in config.json remain unchanged.
		_ = store.Put(account, []byte("new-"+account))
	}
	auth := AuthConfig{
		APIKey: refPrefixV1 + "api", RefreshToken: refPrefixV1 + "refresh", DPoPPrivateKey: refPrefixV1 + "key",
		Certificates: []CertificateInfo{{PemPrivateKey: refPrefixV1 + "cert-key"}},
	}
	auth.InvalidateCachedSecrets()
	for _, account := range refs {
		got, err := resolveSecret(refPrefixV1 + account)
		if err != nil || got != "new-"+account {
			t.Fatalf("stale cached %s after reload: %v", account, err)
		}
	}
	if len(store.deletes) != 0 {
		t.Fatal("cache invalidation must not delete credentials")
	}
}

func TestAccessorsResolveRefsMemoized(t *testing.T) {
	store := newFakeStore()
	store.m["key-abc"] = []byte("PEMDATA")
	useFakeStore(t, store)
	c := CertificateInfo{PemPrivateKey: refPrefixV1 + "key-abc"}
	for i := 0; i < 5; i++ {
		got, err := c.PrivateKeyPEM()
		if err != nil || got != "PEMDATA" {
			t.Fatalf("resolve #%d = %q, %v", i, got, err)
		}
	}
	if store.gets != 1 {
		t.Errorf("store reads = %d, want 1 (memoized)", store.gets)
	}
	if !c.HasPrivateKey() {
		t.Error("HasPrivateKey = false for a reference")
	}
}

func TestAccessorErrorTextActionable(t *testing.T) {
	useFakeStore(t, newFakeStore()) // empty store → miss
	c := CertificateInfo{PemPrivateKey: refPrefixV1 + "key-missing"}
	_, err := c.PrivateKeyPEM()
	if err == nil {
		t.Fatal("expected error for unresolvable ref")
	}
	for _, frag := range []string{"macOS Keychain", "security unlock-keychain", "WENDY_SECRET_STORE=file", "wendy auth login"} {
		if !strings.Contains(err.Error(), frag) {
			t.Errorf("error %q missing %q", err.Error(), frag)
		}
	}
}

func TestAccessorUnknownRefVersion(t *testing.T) {
	useFakeStore(t, newFakeStore())
	c := CertificateInfo{PemPrivateKey: "keychain:v9:key-abc"}
	if _, err := c.PrivateKeyPEM(); err == nil {
		t.Fatal("expected error for unknown ref version")
	}
}

func TestAccountDerivationDeterministic(t *testing.T) {
	a1 := keyAccount("grpc.wendy.com:443", 7, "user-1", 0, "")
	a2 := keyAccount("grpc.wendy.com:443", 7, "user-1", 0, "")
	b := keyAccount("grpc.wendy.com:443", 8, "user-1", 0, "")
	if a1 != a2 {
		t.Errorf("same inputs → different accounts: %q vs %q", a1, a2)
	}
	if a1 == b {
		t.Error("different org → same account")
	}
	if !strings.HasPrefix(a1, "key-") || len(a1) != len("key-")+16 {
		t.Errorf("account %q not key-<hex16>", a1)
	}
	tk1 := tokenAccount("https://cloud.wendy.dev", "grpc.wendy.com:443", 7)
	tk2 := tokenAccount("https://cloud.wendy.dev", "grpc.wendy.com:443", 7)
	tkOther := tokenAccount("https://cloud.wendy.dev", "grpc.wendy.com:443", 8)
	if tk1 != tk2 {
		t.Errorf("same (dashboard, endpoint, org) → different token accounts: %q vs %q", tk1, tk2)
	}
	if tk1 == tkOther {
		t.Error("different org on the same endpoint → same token account")
	}
	if !strings.HasPrefix(tk1, "token-") || len(tk1) != len("token-")+16 {
		t.Errorf("token account %q not token-<hex16>", tk1)
	}
}

// TestAccountDerivationDashboardVariance is the regression test for the
// missing-CloudDashboard finding: AddAuth dedups auth entries on
// (CloudDashboard, CloudGRPC, orgID), so a browser login (CloudDashboard
// set) and an --api-key login (CloudDashboard empty, see performLocalLogin
// in auth.go) against the same endpoint+org coexist as two distinct
// entries. tokenAccount must include CloudDashboard or those two entries'
// dehydrated tokens collide on one Keychain account.
func TestAccountDerivationDashboardVariance(t *testing.T) {
	withDashboard := tokenAccount("https://cloud.wendy.dev", "grpc.wendy.com:443", 7)
	withoutDashboard := tokenAccount("", "grpc.wendy.com:443", 7)
	if withDashboard == withoutDashboard {
		t.Error("differing CloudDashboard (same endpoint+org) → same token account")
	}
}

// TestAccountDerivationAssetVariance is the regression test for the
// missing-AssetID finding: asset certs minted by performLocalLogin carry no
// UserID, only an AssetID, so two asset certs on the same endpoint+org with
// different AssetID must not collide on one key account.
func TestAccountDerivationAssetVariance(t *testing.T) {
	a := keyAccount("grpc.wendy.com:443", 7, "", 100, "")
	b := keyAccount("grpc.wendy.com:443", 7, "", 200, "")
	if a == b {
		t.Error("differing AssetID with empty UserID (same endpoint+org) → same key account")
	}
}

func TestAccountDerivationSPIFFEPrincipalVariance(t *testing.T) {
	a := keyAccount("grpc.wendy.com:443", 0, "", 0, "spiffe://wendy.sh/tenant/a/operator/alice")
	b := keyAccount("grpc.wendy.com:443", 0, "", 0, "spiffe://wendy.sh/tenant/b/operator/alice")
	if a == b {
		t.Error("differing SPIFFE principals → same key account")
	}
}

func TestResolveErrorWhenNoBackend(t *testing.T) {
	origNew := newCredentialStore
	newCredentialStore = func() secretStoreIface { return nil }
	resetSecretCacheForTest()
	t.Cleanup(func() {
		newCredentialStore = origNew
		resetSecretCacheForTest()
	})
	c := CertificateInfo{PemPrivateKey: refPrefixV1 + "key-abc"}
	if _, err := c.PrivateKeyPEM(); err == nil {
		t.Fatal("expected error resolving a ref with no platform backend")
	}
}

func TestSaveDehydratesAndLoadResolves(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("WENDY_SECRET_STORE", "")
	store := newFakeStore()
	useFakeStore(t, store)
	origDefault := secretsPlatformDefault
	secretsPlatformDefault = true
	t.Cleanup(func() { secretsPlatformDefault = origDefault })

	cfg := &Config{Auth: []AuthConfig{{
		CloudGRPC:      "grpc.wendy.com:443",
		APIKey:         "tok-123",
		OAuthIssuer:    "https://auth.dev.wendy.sh/realms/acme",
		OAuthClientID:  "wendy-cli",
		RefreshToken:   "refresh-123",
		DPoPPrivateKey: "-----BEGIN EC PRIVATE KEY-----\ndpop-secret-material",
		Certificates: []CertificateInfo{{
			PemCertificate: "CERT",
			PemPrivateKey:  "-----BEGIN PRIVATE KEY-----\nabc",
			OrganizationID: 7,
			UserID:         "user-1",
		}},
	}}}
	if err := Save(cfg); err != nil {
		t.Fatalf("Save: %v", err)
	}
	// Caller's struct must be untouched (dehydration happens on a clone).
	if isRef(cfg.Auth[0].APIKey) || isRef(cfg.Auth[0].Certificates[0].PemPrivateKey) {
		t.Fatal("Save mutated the caller's config")
	}
	// On-disk JSON must contain no secret material.
	path, _ := configPath()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	for _, secret := range []string{"tok-123", "refresh-123", "dpop-secret-material", "BEGIN PRIVATE KEY"} {
		if strings.Contains(string(raw), secret) {
			t.Errorf("config.json still contains secret %q", secret)
		}
	}
	// Reload → fields are refs → accessors resolve.
	loaded, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !isRef(loaded.Auth[0].APIKey) {
		t.Fatalf("APIKey on disk = %q, want a reference", loaded.Auth[0].APIKey)
	}
	if !isRef(loaded.Auth[0].RefreshToken) || !isRef(loaded.Auth[0].DPoPPrivateKey) {
		t.Fatalf("OAuth secrets on disk = %q / %q, want references", loaded.Auth[0].RefreshToken, loaded.Auth[0].DPoPPrivateKey)
	}
	tok, err := loaded.Auth[0].BearerToken()
	if err != nil || tok != "tok-123" {
		t.Fatalf("BearerToken = %q, %v", tok, err)
	}
	refresh, err := loaded.Auth[0].OAuthRefreshToken()
	if err != nil || refresh != "refresh-123" {
		t.Fatalf("OAuthRefreshToken = %q, %v", refresh, err)
	}
	dpopKey, err := loaded.Auth[0].OAuthDPoPKey()
	if err != nil || !strings.Contains(dpopKey, "BEGIN EC PRIVATE KEY") {
		t.Fatalf("OAuthDPoPKey = %q, %v", dpopKey, err)
	}
	key, err := loaded.Auth[0].Certificates[0].PrivateKeyPEM()
	if err != nil || !strings.Contains(key, "BEGIN PRIVATE KEY") {
		t.Fatalf("PrivateKeyPEM = %q, %v", key, err)
	}
	// Certificates stayed inline (public material).
	if loaded.Auth[0].Certificates[0].PemCertificate != "CERT" {
		t.Error("public certificate was not left inline")
	}
}

func TestSavePutFailureKeepsSecretInline(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("WENDY_SECRET_STORE", "")
	store := newFakeStore()
	store.putErr = errors.New("keychain locked")
	useFakeStore(t, store)
	origDefault := secretsPlatformDefault
	secretsPlatformDefault = true
	t.Cleanup(func() { secretsPlatformDefault = origDefault })

	cfg := &Config{Auth: []AuthConfig{{CloudGRPC: "g", APIKey: "tok-123"}}}
	if err := Save(cfg); err != nil {
		t.Fatalf("Save: %v", err)
	}
	loaded, _ := Load()
	if loaded.Auth[0].APIKey != "tok-123" {
		t.Fatalf("APIKey = %q, want inline tok-123 after Put failure", loaded.Auth[0].APIKey)
	}
}

func TestSaveFileModeSkipsAndDeMigrates(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	store := newFakeStore()
	store.m["token-cafebabe0000dead"] = []byte("tok-999") // seeded ref target
	useFakeStore(t, store)
	origDefault := secretsPlatformDefault
	secretsPlatformDefault = true
	t.Cleanup(func() { secretsPlatformDefault = origDefault })

	t.Setenv("WENDY_SECRET_STORE", "file")
	cfg := &Config{Auth: []AuthConfig{{
		CloudGRPC: "g1",
		APIKey:    "tok-inline", // must STAY inline
	}, {
		CloudGRPC: "g2",
		APIKey:    refPrefixV1 + "token-cafebabe0000dead", // must be inlined back
	}}}
	if err := Save(cfg); err != nil {
		t.Fatalf("Save: %v", err)
	}
	loaded, _ := Load()
	if loaded.Auth[0].APIKey != "tok-inline" {
		t.Errorf("file mode dehydrated anyway: %q", loaded.Auth[0].APIKey)
	}
	if loaded.Auth[1].APIKey != "tok-999" {
		t.Errorf("file mode did not de-migrate ref: %q", loaded.Auth[1].APIKey)
	}
}

func TestSaveFileModeKeepsRefOnFailedRead(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	useFakeStore(t, newFakeStore()) // empty store → ref unresolvable
	origDefault := secretsPlatformDefault
	secretsPlatformDefault = true
	t.Cleanup(func() { secretsPlatformDefault = origDefault })

	t.Setenv("WENDY_SECRET_STORE", "file")
	ref := refPrefixV1 + "token-0123456789abcdef"
	cfg := &Config{Auth: []AuthConfig{{CloudGRPC: "g", APIKey: ref}}}
	if err := Save(cfg); err != nil {
		t.Fatalf("Save: %v", err)
	}
	loaded, _ := Load()
	if loaded.Auth[0].APIKey != ref {
		t.Errorf("unresolvable ref was rewritten to %q; must keep the reference", loaded.Auth[0].APIKey)
	}
}

// TestSaveTokenAccountPerOrgOnSharedEndpoint is a regression test for a
// Keychain account collision: AddAuth deliberately keeps one auth entry per
// (cloudGRPC, orgID) pair so several orgs on the same cloud endpoint each
// get their own entry. tokenAccount must key on org too, or dehydrating the
// second org's token would Put it under the same account as the first,
// destroying the first org's stored token even though both references
// still look distinct on disk.
func TestSaveTokenAccountPerOrgOnSharedEndpoint(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("WENDY_SECRET_STORE", "")
	store := newFakeStore()
	useFakeStore(t, store)
	origDefault := secretsPlatformDefault
	secretsPlatformDefault = true
	t.Cleanup(func() { secretsPlatformDefault = origDefault })

	cfg := &Config{Auth: []AuthConfig{
		{
			CloudGRPC:    "grpc.wendy.com:443",
			APIKey:       "tok-org-a",
			Certificates: []CertificateInfo{{OrganizationID: 1}},
		},
		{
			CloudGRPC:    "grpc.wendy.com:443",
			APIKey:       "tok-org-b",
			Certificates: []CertificateInfo{{OrganizationID: 2}},
		},
	}}
	if err := Save(cfg); err != nil {
		t.Fatalf("Save: %v", err)
	}

	loaded, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !isRef(loaded.Auth[0].APIKey) || !isRef(loaded.Auth[1].APIKey) {
		t.Fatalf("expected both APIKeys to be dehydrated refs, got %q and %q", loaded.Auth[0].APIKey, loaded.Auth[1].APIKey)
	}
	if loaded.Auth[0].APIKey == loaded.Auth[1].APIKey {
		t.Fatalf("both orgs on the shared endpoint got the same keychain reference: %q", loaded.Auth[0].APIKey)
	}

	// Clear the in-process memoization cache dehydrate() seeded, so these
	// resolutions must come from the fake store itself — proving each org's
	// Keychain item independently holds its own token, not a shared one.
	resetSecretCacheForTest()
	tokA, err := loaded.Auth[0].BearerToken()
	if err != nil || tokA != "tok-org-a" {
		t.Errorf("org A BearerToken = %q, %v, want %q", tokA, err, "tok-org-a")
	}
	tokB, err := loaded.Auth[1].BearerToken()
	if err != nil || tokB != "tok-org-b" {
		t.Errorf("org B BearerToken = %q, %v, want %q", tokB, err, "tok-org-b")
	}
}

// TestSaveTokenAccountPerDashboardOnSharedEndpoint is the sibling regression
// test for the missing-CloudDashboard finding at the Save/Load level:
// AddAuth dedups on (CloudDashboard, CloudGRPC, orgID), so a browser login
// (CloudDashboard set) and an --api-key login (CloudDashboard empty, see
// performLocalLogin in auth.go) against the same endpoint+org coexist as two
// separate entries. Before the fix, tokenAccount ignored CloudDashboard, so
// dehydrating the second entry's token would Put it under the first
// entry's account, destroying the first token even though both references
// still looked distinct on disk.
func TestSaveTokenAccountPerDashboardOnSharedEndpoint(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("WENDY_SECRET_STORE", "")
	store := newFakeStore()
	useFakeStore(t, store)
	origDefault := secretsPlatformDefault
	secretsPlatformDefault = true
	t.Cleanup(func() { secretsPlatformDefault = origDefault })

	cfg := &Config{Auth: []AuthConfig{
		{
			CloudDashboard: "https://cloud.wendy.dev",
			CloudGRPC:      "grpc.wendy.com:443",
			APIKey:         "tok-browser",
			Certificates:   []CertificateInfo{{OrganizationID: 1}},
		},
		{
			CloudDashboard: "", // --api-key login leaves this empty
			CloudGRPC:      "grpc.wendy.com:443",
			APIKey:         "tok-apikey",
			Certificates:   []CertificateInfo{{OrganizationID: 1}},
		},
	}}
	if err := Save(cfg); err != nil {
		t.Fatalf("Save: %v", err)
	}

	loaded, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !isRef(loaded.Auth[0].APIKey) || !isRef(loaded.Auth[1].APIKey) {
		t.Fatalf("expected both APIKeys to be dehydrated refs, got %q and %q", loaded.Auth[0].APIKey, loaded.Auth[1].APIKey)
	}
	if loaded.Auth[0].APIKey == loaded.Auth[1].APIKey {
		t.Fatalf("browser login and --api-key login on the shared endpoint got the same keychain reference: %q", loaded.Auth[0].APIKey)
	}

	// Cold cache: resolutions must come from the fake store itself, proving
	// each entry independently holds its own token in the Keychain.
	resetSecretCacheForTest()
	tokBrowser, err := loaded.Auth[0].BearerToken()
	if err != nil || tokBrowser != "tok-browser" {
		t.Errorf("browser-login BearerToken = %q, %v, want %q", tokBrowser, err, "tok-browser")
	}
	tokAPIKey, err := loaded.Auth[1].BearerToken()
	if err != nil || tokAPIKey != "tok-apikey" {
		t.Errorf("api-key-login BearerToken = %q, %v, want %q", tokAPIKey, err, "tok-apikey")
	}
}

// TestSaveKeyAccountPerAssetOnEmptyUserID is the sibling regression test for
// the missing-AssetID finding: asset certs minted by performLocalLogin
// (device/asset identities) carry no UserID, only an AssetID. Before the
// fix, keyAccount ignored AssetID, so two asset certs on the same
// endpoint+org would dehydrate their private keys to the same Keychain
// account, and the second Save would destroy the first key.
func TestSaveKeyAccountPerAssetOnEmptyUserID(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("WENDY_SECRET_STORE", "")
	store := newFakeStore()
	useFakeStore(t, store)
	origDefault := secretsPlatformDefault
	secretsPlatformDefault = true
	t.Cleanup(func() { secretsPlatformDefault = origDefault })

	cfg := &Config{Auth: []AuthConfig{
		{
			CloudGRPC: "grpc.wendy.com:443",
			Certificates: []CertificateInfo{{
				OrganizationID: 1,
				AssetID:        100,
				PemPrivateKey:  "-----BEGIN PRIVATE KEY-----\nasset-100",
			}},
		},
		{
			CloudGRPC: "grpc.wendy.com:443",
			Certificates: []CertificateInfo{{
				OrganizationID: 1,
				AssetID:        200,
				PemPrivateKey:  "-----BEGIN PRIVATE KEY-----\nasset-200",
			}},
		},
	}}
	if err := Save(cfg); err != nil {
		t.Fatalf("Save: %v", err)
	}

	loaded, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	key0 := loaded.Auth[0].Certificates[0].PemPrivateKey
	key1 := loaded.Auth[1].Certificates[0].PemPrivateKey
	if !isRef(key0) || !isRef(key1) {
		t.Fatalf("expected both private keys to be dehydrated refs, got %q and %q", key0, key1)
	}
	if key0 == key1 {
		t.Fatalf("two asset certs (empty UserID, differing AssetID) got the same keychain reference: %q", key0)
	}

	resetSecretCacheForTest()
	pem0, err := loaded.Auth[0].Certificates[0].PrivateKeyPEM()
	if err != nil || !strings.Contains(pem0, "asset-100") {
		t.Errorf("asset 100 PrivateKeyPEM = %q, %v, want to contain %q", pem0, err, "asset-100")
	}
	pem1, err := loaded.Auth[1].Certificates[0].PrivateKeyPEM()
	if err != nil || !strings.Contains(pem1, "asset-200") {
		t.Errorf("asset 200 PrivateKeyPEM = %q, %v, want to contain %q", pem1, err, "asset-200")
	}
}

func TestHasInlineSecrets(t *testing.T) {
	if hasInlineSecrets(&Config{}) {
		t.Error("empty config reports inline secrets")
	}
	if hasInlineSecrets(&Config{Auth: []AuthConfig{{APIKey: refPrefixV1 + "token-x"}}}) {
		t.Error("all-refs config reports inline secrets")
	}
	if !hasInlineSecrets(&Config{Auth: []AuthConfig{{APIKey: "tok"}}}) {
		t.Error("inline APIKey not detected")
	}
	if !hasInlineSecrets(&Config{Auth: []AuthConfig{{Certificates: []CertificateInfo{{PemPrivateKey: "PEM"}}}}}) {
		t.Error("inline key not detected")
	}
}

func TestMigrateSecretsIfNeeded(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("WENDY_SECRET_STORE", "")
	store := newFakeStore()
	useFakeStore(t, store)
	origDefault := secretsPlatformDefault
	secretsPlatformDefault = true
	t.Cleanup(func() { secretsPlatformDefault = origDefault })

	cfg := &Config{Auth: []AuthConfig{{CloudGRPC: "g", APIKey: "tok-123"}}}
	if err := Save(cfg); err != nil { // simulate a pre-existing config...
		t.Fatalf("seed Save: %v", err)
	}
	// ...that was written by an OLD cli: rewrite it inline via file mode.
	t.Setenv("WENDY_SECRET_STORE", "file")
	if err := Save(cfg); err != nil {
		t.Fatalf("inline Save: %v", err)
	}
	t.Setenv("WENDY_SECRET_STORE", "")

	loaded, _ := Load()
	if !hasInlineSecrets(loaded) {
		t.Fatal("test setup failed: config should hold inline secrets")
	}
	if !MigrateSecretsIfNeeded(loaded) {
		t.Fatal("MigrateSecretsIfNeeded = false, want true (migration ran)")
	}
	reloaded, _ := Load()
	if hasInlineSecrets(reloaded) {
		t.Error("config still holds inline secrets after migration")
	}
	// Second call: nothing left to migrate.
	if MigrateSecretsIfNeeded(reloaded) {
		t.Error("second MigrateSecretsIfNeeded = true, want false")
	}
}

func TestMigrateSecretsNoOpOffPlatformAndFileMode(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	store := newFakeStore()
	useFakeStore(t, store)
	cfg := &Config{Auth: []AuthConfig{{CloudGRPC: "g", APIKey: "tok"}}}

	origDefault := secretsPlatformDefault
	secretsPlatformDefault = false // non-darwin
	if MigrateSecretsIfNeeded(cfg) {
		t.Error("migrated on a platform without a store")
	}
	secretsPlatformDefault = true
	t.Setenv("WENDY_SECRET_STORE", "file")
	if MigrateSecretsIfNeeded(cfg) {
		t.Error("reported migration with no Keychain references")
	}
	secretsPlatformDefault = origDefault
}

func TestMigrateSecretsIfNeededBackToFile(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("WENDY_SECRET_STORE", "file")
	store := newFakeStore()
	store.m["token-old"] = []byte("tok-123")
	store.m["key-old"] = []byte("PEM-123")
	useFakeStore(t, store)

	cfg := &Config{Auth: []AuthConfig{{
		APIKey: refPrefixV1 + "token-old",
		Certificates: []CertificateInfo{{
			PemPrivateKey: refPrefixV1 + "key-old",
		}},
	}}}
	path, err := configPath()
	if err != nil {
		t.Fatalf("configPath: %v", err)
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("seed config: %v", err)
	}

	if !MigrateSecretsIfNeeded(cfg) {
		t.Fatal("MigrateSecretsIfNeeded = false, want true")
	}
	reloaded, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := reloaded.Auth[0].APIKey; got != "tok-123" {
		t.Errorf("APIKey = %q, want inline token", got)
	}
	if got := reloaded.Auth[0].Certificates[0].PemPrivateKey; got != "PEM-123" {
		t.Errorf("PemPrivateKey = %q, want inline key", got)
	}
	if MigrateSecretsIfNeeded(reloaded) {
		t.Error("second migration reported a change")
	}
}

func TestMigrateSecretsIfNeededKeepsUnreadableRefs(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("WENDY_SECRET_STORE", "file")
	useFakeStore(t, newFakeStore())

	cfg := &Config{Auth: []AuthConfig{{APIKey: refPrefixV1 + "missing"}}}
	if err := Save(cfg); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if MigrateSecretsIfNeeded(cfg) {
		t.Error("reported a migration for an unreadable reference")
	}
	reloaded, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := reloaded.Auth[0].APIKey; got != refPrefixV1+"missing" {
		t.Errorf("APIKey = %q, want original reference", got)
	}
}

func TestDeleteStoredSecrets(t *testing.T) {
	store := newFakeStore()
	store.m["token-aaaa"] = []byte("tok")
	store.m["key-bbbb"] = []byte("pem")
	useFakeStore(t, store)
	cfg := &Config{Auth: []AuthConfig{{
		APIKey: refPrefixV1 + "token-aaaa",
		Certificates: []CertificateInfo{
			{PemPrivateKey: refPrefixV1 + "key-bbbb"},
			{PemPrivateKey: "-----BEGIN PRIVATE KEY-----\ninline"}, // inline: nothing to delete
		},
	}}}
	DeleteStoredSecrets(cfg)
	if len(store.m) != 0 {
		t.Errorf("store still holds %d items after DeleteStoredSecrets", len(store.m))
	}
	if got := len(store.deletes); got != 2 {
		t.Errorf("deletes = %d, want 2 (refs only, not the inline value)", got)
	}
}
