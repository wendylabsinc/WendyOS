package localmesh

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestIdentityCachePersistenceBoundsAndTrust(t *testing.T) {
	now := time.Unix(1800000000, 0)
	trusted := true
	verify := func(chain [][]byte, at time.Time) (Identity, error) {
		if !trusted || len(chain) != 1 {
			return Identity{}, errors.New("untrusted")
		}
		return Identity{64, 445, now.Add(time.Hour)}, nil
	}
	path := filepath.Join(t.TempDir(), "identities.json")
	limits := CacheLimits{2, 6, 30 * time.Minute}
	c, err := OpenIdentityCache(path, limits, verify, now)
	if err != nil {
		t.Fatal(err)
	}
	put := func(s string, at time.Time) string {
		t.Helper()
		fp, err := c.Put([][]byte{[]byte(s)}, at)
		if err != nil {
			t.Fatal(err)
		}
		return fp
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

func TestIdentityCacheCertificateExpiryAndClockRollback(t *testing.T) {
	now := time.Unix(1800000000, 0)
	verify := func(_ [][]byte, _ time.Time) (Identity, error) { return Identity{64, 445, now.Add(time.Minute)}, nil }
	c, err := OpenIdentityCache("", DefaultCacheLimits(), verify, now)
	if err != nil {
		t.Fatal(err)
	}
	fp, err := c.Put([][]byte{[]byte("cert")}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, ok := c.Get(fp, now.Add(time.Minute)); ok {
		t.Fatal("expired certificate")
	}
	if _, err = c.Put([][]byte{[]byte("cert")}, now.Add(time.Minute)); err == nil {
		t.Fatal("admitted expired certificate")
	}
	fp, err = c.Put([][]byte{[]byte("cert")}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, ok := c.Get(fp, now.Add(-time.Second)); ok {
		t.Fatal("rollback extended cache")
	}
}

func TestIdentityCacheDuplicateDoesNotRenewRetention(t *testing.T) {
	now := time.Unix(1800000000, 0)
	verify := func(_ [][]byte, _ time.Time) (Identity, error) { return Identity{64, 445, now.Add(time.Hour)}, nil }
	c, _ := OpenIdentityCache("", CacheLimits{1, 10, time.Minute}, verify, now)
	fp, _ := c.Put([][]byte{[]byte("cert")}, now)
	if _, err := c.Put([][]byte{[]byte("cert")}, now.Add(59*time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := c.Get(fp, now.Add(time.Minute)); ok {
		t.Fatal("duplicate renewed retention")
	}
	if _, err := c.Put([][]byte{make([]byte, 11)}, now); err == nil {
		t.Fatal("oversized bundle admitted")
	}
}

func TestIdentityCacheDuplicateOnlyValidatesSelectedIdentity(t *testing.T) {
	now := time.Unix(1800000000, 0)
	calls := 0
	trusted := true
	verify := func(_ [][]byte, _ time.Time) (Identity, error) {
		calls++
		if !trusted {
			return Identity{}, errors.New("revoked")
		}
		return Identity{64, 445, now.Add(time.Hour)}, nil
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
	verify := func(_ [][]byte, _ time.Time) (Identity, error) { return Identity{64, 445, now.Add(time.Hour)}, nil }
	dir := t.TempDir()
	path := filepath.Join(dir, "identities.json")
	c, _ := OpenIdentityCache(path, DefaultCacheLimits(), verify, now)
	chain := [][]byte{[]byte("one")}
	fp, err := c.Put(chain, now)
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
	if !c.entries[fp].Used.Equal(now.Add(time.Second)) {
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
	if _, err = c.Put(second, now.Add(5*time.Second)); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenIdentityCache(path, DefaultCacheLimits(), verify, now.Add(6*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, ok := reopened.Get(Fingerprint(second), now.Add(6*time.Second)); !ok {
		t.Fatal("retry did not durably admit bundle")
	}
}

func TestIdentityCacheSameLeafChangedChainIsNotDuplicate(t *testing.T) {
	now := time.Unix(1800000000, 0)
	verify := func(_ [][]byte, _ time.Time) (Identity, error) { return Identity{64, 445, now.Add(time.Hour)}, nil }
	path := filepath.Join(t.TempDir(), "identities.json")
	c, _ := OpenIdentityCache(path, DefaultCacheLimits(), verify, now)
	old := [][]byte{[]byte("leaf"), []byte("old issuer")}
	changed := [][]byte{[]byte("leaf"), []byte("new issuer")}
	fp, err := c.Put(old, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.Put(changed, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenIdentityCache(path, DefaultCacheLimits(), verify, now.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	got, _, ok := reopened.Get(fp, now.Add(2*time.Second))
	if !ok || len(got) != 2 || !bytes.Equal(got[1], changed[1]) {
		t.Fatal("changed issuer chain was not persisted")
	}
}
