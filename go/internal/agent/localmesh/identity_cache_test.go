package localmesh

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

// fakeVerifier keys each synthetic chain by its first entry, so one test can
// hold several device identities at once.
func fakeVerifier(devices map[string]meshid.Device, notAfter time.Time) IdentityVerifier {
	return func(chain [][]byte, _ time.Time) (Identity, error) {
		if len(chain) == 0 || len(chain) > 16 {
			return Identity{}, errors.New("untrusted")
		}
		device, ok := devices[string(chain[0])]
		if !ok {
			return Identity{}, errors.New("untrusted")
		}
		return Identity{Device: device, NotAfter: notAfter}, nil
	}
}

func TestIdentityCachePersistenceBoundsAndTrust(t *testing.T) {
	now := time.Unix(1800000000, 0)
	trusted := true
	devices := map[string]meshid.Device{
		"aaa": testDevice(t, testPrincipal(testTenantUUID, testDeviceAID)),
		"bbb": testDevice(t, testPrincipal(testTenantUUID, testDeviceBID)),
		"ddd": testDevice(t, testPrincipal(testTenantUUID, testDeviceCID)),
	}
	verify := func(chain [][]byte, at time.Time) (Identity, error) {
		if !trusted {
			return Identity{}, errors.New("untrusted")
		}
		return fakeVerifier(devices, now.Add(time.Hour))(chain, at)
	}
	path := filepath.Join(t.TempDir(), "identities.json")
	limits := CacheLimits{2, 6, 30 * time.Minute}
	c, err := OpenIdentityCache(path, limits, verify, now)
	if err != nil {
		t.Fatal(err)
	}
	put := func(s string, at time.Time) string {
		t.Helper()
		principal, err := c.Put([][]byte{[]byte(s)}, at)
		if err != nil {
			t.Fatal(err)
		}
		return principal
	}
	a := put("aaa", now)
	b := put("bbb", now.Add(time.Second))
	chain, _, ok := c.Get(a, now.Add(2*time.Second))
	if !ok {
		t.Fatal("missing cached identity")
	}
	chain[0][0] = 'X'
	d := put("ddd", now.Add(3*time.Second))
	if _, _, ok := c.Get(b, now.Add(4*time.Second)); ok {
		t.Fatal("LRU entry retained")
	}
	if err := c.Flush(now.Add(4 * time.Second)); err != nil {
		t.Fatal(err)
	}
	c, err = OpenIdentityCache(path, limits, verify, now.Add(5*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	chain, _, ok = c.Get(a, now.Add(6*time.Second))
	if !ok || string(chain[0]) != "aaa" {
		t.Fatal("restart or copy isolation failed")
	}
	trusted = false
	if _, _, ok := c.Get(d, now.Add(7*time.Second)); ok {
		t.Fatal("trust change ignored")
	}
	trusted = true
	if _, _, ok := c.Get(a, now.Add(30*time.Minute)); ok {
		t.Fatal("expired retention")
	}
}

func TestIdentityCacheEvictsByTotalCertificateBytes(t *testing.T) {
	for _, persistent := range []bool{false, true} {
		t.Run(map[bool]string{false: "memory", true: "disk"}[persistent], func(t *testing.T) {
			now := time.Unix(1800000000, 0)
			devices := map[string]meshid.Device{
				"aaa": testDevice(t, testPrincipal(testTenantUUID, testDeviceAID)),
				"bbb": testDevice(t, testPrincipal(testTenantUUID, testDeviceBID)),
				"ddd": testDevice(t, testPrincipal(testTenantUUID, testDeviceCID)),
			}
			path := ""
			if persistent {
				path = filepath.Join(t.TempDir(), "identities.json")
			}
			limits := CacheLimits{Entries: 3, Bytes: 6, Age: time.Hour}
			verify := fakeVerifier(devices, now.Add(time.Hour))
			c, err := OpenIdentityCache(path, limits, verify, now)
			if err != nil {
				t.Fatal(err)
			}
			put := func(chain string, at time.Time) {
				t.Helper()
				if _, err := c.Put([][]byte{[]byte(chain)}, at); err != nil {
					t.Fatal(err)
				}
			}
			put("aaa", now)
			put("bbb", now.Add(time.Second))
			if _, _, ok := c.Get(devices["aaa"].Principal(), now.Add(2*time.Second)); !ok {
				t.Fatal("first bundle missing")
			}
			put("ddd", now.Add(3*time.Second))
			if persistent {
				c, err = OpenIdentityCache(path, limits, verify, now.Add(4*time.Second))
				if err != nil {
					t.Fatal(err)
				}
			}
			if _, _, ok := c.Get(devices["bbb"].Principal(), now.Add(5*time.Second)); ok {
				t.Fatal("least recently used bundle exceeded the aggregate byte budget")
			}
			for _, chain := range []string{"aaa", "ddd"} {
				if _, _, ok := c.Get(devices[chain].Principal(), now.Add(5*time.Second)); !ok {
					t.Fatalf("admitted bundle %s was lost instead of the LRU entry", chain)
				}
			}
		})
	}
}

// TestIdentityCacheKeysByFullPrincipal is the identity contract: the key is the
// complete principal, the same device UUID under two tenants is two entries, and
// a file key that is not a device principal is never loaded.
func TestIdentityCacheKeysByFullPrincipal(t *testing.T) {
	now := time.Unix(1800000000, 0)
	own := testDevice(t, testPrincipal(testTenantUUID, testDeviceAID))
	sameUUIDOtherTenant := testDevice(t, testPrincipal(testOtherTenant, testDeviceAID))
	devices := map[string]meshid.Device{"own": own, "other": sameUUIDOtherTenant}
	c, err := OpenIdentityCache("", DefaultCacheLimits(), fakeVerifier(devices, now.Add(time.Hour)), now)
	if err != nil {
		t.Fatal(err)
	}
	ownKey, err := c.Put([][]byte{[]byte("own")}, now)
	if err != nil {
		t.Fatal(err)
	}
	otherKey, err := c.Put([][]byte{[]byte("other")}, now)
	if err != nil {
		t.Fatal(err)
	}
	if ownKey != own.Principal() || otherKey != sameUUIDOtherTenant.Principal() {
		t.Fatalf("cache key is not the full principal: %s / %s", ownKey, otherKey)
	}
	if ownKey == otherKey {
		t.Fatal("one device UUID in two tenants collapsed into one entry")
	}
	chain, id, ok := c.Get(own.Principal(), now)
	if !ok || id.Device.Principal() != own.Principal() || string(chain[0]) != "own" {
		t.Fatal("own principal did not resolve to its own bundle")
	}
	if chain, _, ok = c.Get(sameUUIDOtherTenant.Principal(), now); !ok || string(chain[0]) != "other" {
		t.Fatal("tenant-mate lookup crossed identities")
	}
	if _, _, ok = c.Get(own.UUID().String(), now); ok {
		t.Fatal("device UUID alone addressed a cache entry")
	}

	// A persisted key that is not a device principal - a legacy numeric identity
	// or a bare UUID - is dropped rather than adopted.
	path := filepath.Join(t.TempDir(), "identities.json")
	disk := map[string]any{"Version": 1, "Entries": map[string]cachedIdentity{
		own.Principal():                 {Chain: [][]byte{[]byte("own")}, Added: now, Used: now},
		"urn:wendy:org:64:asset:445":    {Chain: [][]byte{[]byte("own")}, Added: now, Used: now},
		own.UUID().String():             {Chain: [][]byte{[]byte("own")}, Added: now, Used: now},
		sameUUIDOtherTenant.Principal(): {Chain: [][]byte{[]byte("other")}, Added: now, Used: now},
	}}
	data, err := json.Marshal(disk)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenIdentityCache(path, DefaultCacheLimits(), fakeVerifier(devices, now.Add(time.Hour)), now)
	if err != nil {
		t.Fatal(err)
	}
	if len(reopened.entries) != 2 {
		t.Fatalf("non-principal cache keys survived: %d entries", len(reopened.entries))
	}
	if _, _, ok = reopened.Get(own.Principal(), now); !ok {
		t.Fatal("principal key lost")
	}
}

func TestIdentityCacheCertificateExpiryAndClockRollback(t *testing.T) {
	now := time.Unix(1800000000, 0)
	device := testDevice(t, testPrincipal(testTenantUUID, testDeviceAID))
	c, err := OpenIdentityCache("", DefaultCacheLimits(), fakeVerifier(map[string]meshid.Device{"cert": device}, now.Add(time.Minute)), now)
	if err != nil {
		t.Fatal(err)
	}
	principal, err := c.Put([][]byte{[]byte("cert")}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, ok := c.Get(principal, now.Add(time.Minute)); ok {
		t.Fatal("expired certificate")
	}
	if _, err = c.Put([][]byte{[]byte("cert")}, now.Add(time.Minute)); err == nil {
		t.Fatal("admitted expired certificate")
	}
	principal, err = c.Put([][]byte{[]byte("cert")}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, ok := c.Get(principal, now.Add(-time.Second)); ok {
		t.Fatal("rollback extended cache")
	}
}

func TestIdentityCacheDuplicateDoesNotRenewRetention(t *testing.T) {
	now := time.Unix(1800000000, 0)
	device := testDevice(t, testPrincipal(testTenantUUID, testDeviceAID))
	c, _ := OpenIdentityCache("", CacheLimits{1, 10, time.Minute}, fakeVerifier(map[string]meshid.Device{"cert": device}, now.Add(time.Hour)), now)
	principal, _ := c.Put([][]byte{[]byte("cert")}, now)
	if _, err := c.Put([][]byte{[]byte("cert")}, now.Add(59*time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := c.Get(principal, now.Add(time.Minute)); ok {
		t.Fatal("duplicate renewed retention")
	}
	if _, err := c.Put([][]byte{make([]byte, 11)}, now); err == nil {
		t.Fatal("oversized bundle admitted")
	}
}

func TestIdentityCacheDuplicateOnlyValidatesSelectedIdentity(t *testing.T) {
	now := time.Unix(1800000000, 0)
	devices := map[string]meshid.Device{
		"one":   testDevice(t, testPrincipal(testTenantUUID, testDeviceAID)),
		"two":   testDevice(t, testPrincipal(testTenantUUID, testDeviceBID)),
		"three": testDevice(t, testPrincipal(testTenantUUID, testDeviceCID)),
	}
	calls := 0
	trusted := true
	verify := func(chain [][]byte, at time.Time) (Identity, error) {
		calls++
		if !trusted {
			return Identity{}, errors.New("revoked")
		}
		return fakeVerifier(devices, now.Add(time.Hour))(chain, at)
	}
	c, _ := OpenIdentityCache("", DefaultCacheLimits(), verify, now)
	for _, chain := range []string{"one", "two", "three"} {
		if _, err := c.Put([][]byte{[]byte(chain)}, now); err != nil {
			t.Fatal(err)
		}
	}
	calls = 0
	if _, err := c.Put([][]byte{[]byte("two")}, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("duplicate verified %d identities, want selected identity only", calls)
	}
	trusted = false
	if _, err := c.Put([][]byte{[]byte("two")}, now.Add(2*time.Second)); err == nil {
		t.Fatal("identical untrusted bundle accepted")
	}
}

func TestIdentityCacheDuplicateAvoidsWriteButRetriesPersistenceFailure(t *testing.T) {
	now := time.Unix(1800000000, 0)
	devices := map[string]meshid.Device{
		"one": testDevice(t, testPrincipal(testTenantUUID, testDeviceAID)),
		"two": testDevice(t, testPrincipal(testTenantUUID, testDeviceBID)),
	}
	verify := fakeVerifier(devices, now.Add(time.Hour))
	dir := t.TempDir()
	path := filepath.Join(dir, "identities.json")
	c, _ := OpenIdentityCache(path, DefaultCacheLimits(), verify, now)
	chain := [][]byte{[]byte("one")}
	principal, err := c.Put(chain, now)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.Put(chain, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("duplicate rewrote persistent LRU metadata")
	}
	if !c.entries[principal].Used.Equal(now.Add(time.Second)) {
		t.Fatal("duplicate failed to update in-memory LRU")
	}
	if err = c.Flush(now.Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	after, _ = os.ReadFile(path)
	if bytes.Equal(before, after) {
		t.Fatal("Flush did not persist LRU activity")
	}
	// Force rename failure without relying on permissions (tests may run root).
	badPath := filepath.Join(dir, "is-a-directory")
	if err = os.Mkdir(badPath, 0700); err != nil {
		t.Fatal(err)
	}
	c.path = badPath
	second := [][]byte{[]byte("two")}
	if _, err = c.Put(second, now.Add(3*time.Second)); err == nil {
		t.Fatal("expected persistence failure")
	}
	if _, err = c.Put(second, now.Add(4*time.Second)); err == nil {
		t.Fatal("duplicate hid failed persistence")
	}
	c.path = path
	secondPrincipal, err := c.Put(second, now.Add(5*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if secondPrincipal != devices["two"].Principal() {
		t.Fatal("retry stored the bundle under the wrong key")
	}
	reopened, err := OpenIdentityCache(path, DefaultCacheLimits(), verify, now.Add(6*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, ok := reopened.Get(secondPrincipal, now.Add(6*time.Second)); !ok {
		t.Fatal("retry did not durably admit bundle")
	}
}

func TestIdentityCacheSameLeafChangedChainIsNotDuplicate(t *testing.T) {
	now := time.Unix(1800000000, 0)
	device := testDevice(t, testPrincipal(testTenantUUID, testDeviceAID))
	devices := map[string]meshid.Device{"leaf": device}
	path := filepath.Join(t.TempDir(), "identities.json")
	c, _ := OpenIdentityCache(path, DefaultCacheLimits(), fakeVerifier(devices, now.Add(time.Hour)), now)
	old := [][]byte{[]byte("leaf"), []byte("old issuer")}
	changed := [][]byte{[]byte("leaf"), []byte("new issuer")}
	principal, err := c.Put(old, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.Put(changed, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenIdentityCache(path, DefaultCacheLimits(), fakeVerifier(devices, now.Add(time.Hour)), now.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	got, _, ok := reopened.Get(principal, now.Add(2*time.Second))
	if !ok || len(got) != 2 || !bytes.Equal(got[1], changed[1]) {
		t.Fatal("changed issuer chain was not persisted")
	}
}

// TestIdentityCacheOlderChainIsNotARenewal: a bundle that verifies for the same
// device but expires earlier cannot displace the live one.
func TestIdentityCacheOlderChainIsNotARenewal(t *testing.T) {
	now := time.Unix(1800000000, 0)
	device := testDevice(t, testPrincipal(testTenantUUID, testDeviceAID))
	path := filepath.Join(t.TempDir(), "identities.json")
	// The issuer entry decides the bundle's expiry, so one verifier can present
	// a live chain and an older one for the same device.
	verify := func(chain [][]byte, _ time.Time) (Identity, error) {
		if len(chain) != 2 {
			return Identity{}, errors.New("untrusted")
		}
		notAfter := now.Add(2 * time.Hour)
		if string(chain[1]) == "older" {
			notAfter = now.Add(time.Hour)
		}
		return Identity{Device: device, NotAfter: notAfter}, nil
	}
	c, _ := OpenIdentityCache(path, DefaultCacheLimits(), verify, now)
	principal, err := c.Put([][]byte{[]byte("leaf"), []byte("current")}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.Put([][]byte{[]byte("leaf"), []byte("older")}, now.Add(time.Second)); err == nil {
		t.Fatal("older bundle replaced a live one")
	}
	_, id, ok := c.Get(principal, now.Add(time.Minute))
	if !ok || !id.NotAfter.Equal(now.Add(2*time.Hour)) {
		t.Fatal("live chain was displaced", ok)
	}
}

func TestIdentityCachePrunesLoadedCertificateBytes(t *testing.T) {
	now := time.Unix(1800000000, 0)
	devices := map[string]meshid.Device{
		"aaa": testDevice(t, testPrincipal(testTenantUUID, testDeviceAID)),
		"bbb": testDevice(t, testPrincipal(testTenantUUID, testDeviceBID)),
		"ddd": testDevice(t, testPrincipal(testTenantUUID, testDeviceCID)),
	}
	verify := fakeVerifier(devices, now.Add(time.Hour))
	path := filepath.Join(t.TempDir(), "identities.json")
	c, err := OpenIdentityCache(path, CacheLimits{Entries: 3, Bytes: 9, Age: time.Hour}, verify, now)
	if err != nil {
		t.Fatal(err)
	}
	for i, chain := range []string{"aaa", "bbb", "ddd"} {
		if _, err := c.Put([][]byte{[]byte(chain)}, now.Add(time.Duration(i)*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	c, err = OpenIdentityCache(path, CacheLimits{Entries: 3, Bytes: 6, Age: time.Hour}, verify, now.Add(3*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, ok := c.Get(devices["aaa"].Principal(), now.Add(3*time.Second)); ok {
		t.Fatal("oldest bundle survived reduced byte budget on reopen")
	}
	for _, chain := range []string{"bbb", "ddd"} {
		if _, _, ok := c.Get(devices[chain].Principal(), now.Add(3*time.Second)); !ok {
			t.Fatalf("newer bundle %s lost", chain)
		}
	}
}
