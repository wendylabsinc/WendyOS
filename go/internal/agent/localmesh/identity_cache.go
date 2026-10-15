// Package localmesh implements transport-independent local mesh facilities.
package localmesh

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// IdentityVerifier must validate the complete chain against CURRENT trust and
// return the authorized identity and earliest certificate expiry. It is called
// on every lookup, including cache hits. A cache is never a trust store.
type IdentityVerifier func(chain [][]byte, now time.Time) (Identity, error)

type Identity struct {
	Org, Asset int32
	NotAfter   time.Time
}

type CacheLimits struct {
	Entries, Bytes int
	Age            time.Duration
}

func DefaultCacheLimits() CacheLimits {
	return CacheLimits{Entries: 1024, Bytes: 8 << 20, Age: 24 * time.Hour}
}

type cachedIdentity struct {
	Chain [][]byte  `json:"chain"`
	Added time.Time `json:"added"`
	Used  time.Time `json:"used"`
}

// IdentityCache persists public certificate bundles only; presence and service
// leases are deliberately absent. The file is scoped by caller to provisioning.
type IdentityCache struct {
	mu      sync.Mutex
	path    string
	limits  CacheLimits
	verify  IdentityVerifier
	entries map[string]cachedIdentity
	// A failed write must be retried even when a later Put is byte-identical.
	pendingSave bool
}

func Fingerprint(chain [][]byte) string {
	if len(chain) == 0 {
		return ""
	}
	sum := sha256.Sum256(chain[0])
	return hex.EncodeToString(sum[:])
}

func OpenIdentityCache(path string, limits CacheLimits, verify IdentityVerifier, now time.Time) (*IdentityCache, error) {
	if limits.Entries < 1 || limits.Bytes < 1 || limits.Age <= 0 || verify == nil {
		return nil, errors.New("invalid identity cache configuration")
	}
	c := &IdentityCache{path: path, limits: limits, verify: verify, entries: make(map[string]cachedIdentity)}
	if path == "" {
		return c, nil
	}
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	// Bound the encoded file too: base64 plus bounded per-entry metadata.
	data, err := io.ReadAll(io.LimitReader(f, int64(limits.Bytes)*2+int64(limits.Entries)*512+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > int64(limits.Bytes)*2+int64(limits.Entries)*512 {
		return nil, errors.New("oversized identity cache")
	}
	var disk struct {
		Version int
		Entries map[string]cachedIdentity
	}
	if err = json.Unmarshal(data, &disk); err != nil {
		return nil, err
	}
	if disk.Version != 1 {
		return nil, errors.New("unsupported identity cache version")
	}
	for fp, entry := range disk.Entries {
		if fp != Fingerprint(entry.Chain) || len(fp) != 64 {
			continue
		}
		if entry.Used.After(now) {
			entry.Used = now
		}
		if _, ok := c.valid(entry, now); ok {
			c.entries[fp] = entry
		}
	}
	c.prune(now)
	return c, nil
}

func (c *IdentityCache) valid(e cachedIdentity, now time.Time) (Identity, bool) {
	if len(e.Chain) == 0 || len(e.Chain) > 16 || e.Added.IsZero() || now.Before(e.Added) || now.Sub(e.Added) >= c.limits.Age || e.Used.Before(e.Added) {
		return Identity{}, false
	}
	id, err := c.verify(e.Chain, now)
	return id, err == nil && id.Org > 0 && id.Asset > 0 && now.Before(id.NotAfter)
}

func chainSize(chain [][]byte) int {
	n := 0
	for _, cert := range chain {
		n += len(cert)
	}
	return n
}

func cloneChain(chain [][]byte) [][]byte {
	out := make([][]byte, len(chain))
	for i := range chain {
		out[i] = append([]byte(nil), chain[i]...)
	}
	return out
}

func (c *IdentityCache) prune(now time.Time) {
	total := 0
	for fp, e := range c.entries {
		if _, ok := c.valid(e, now); !ok {
			delete(c.entries, fp)
			continue
		}
		total += chainSize(e.Chain)
	}
	keys := make([]string, 0, len(c.entries))
	for fp := range c.entries {
		keys = append(keys, fp)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := c.entries[keys[i]], c.entries[keys[j]]
		if a.Used.Equal(b.Used) {
			return keys[i] < keys[j]
		}
		return a.Used.Before(b.Used)
	})
	for _, fp := range keys {
		if len(c.entries) <= c.limits.Entries && total <= c.limits.Bytes {
			break
		}
		total -= chainSize(c.entries[fp].Chain)
		delete(c.entries, fp)
	}
}

func sameChain(a, b [][]byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !bytes.Equal(a[i], b[i]) {
			return false
		}
	}
	return true
}

func (c *IdentityCache) Put(chain [][]byte, now time.Time) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(chain) == 0 || len(chain) > 16 || chainSize(chain) > c.limits.Bytes {
		return "", errors.New("invalid identity bundle size")
	}
	fp := Fingerprint(chain)
	// Repeated snapshots from different peers commonly contain exactly the same
	// public bundle. Revalidate that identity against current trust, but do not
	// rescan every other identity or fsync the whole cache for an LRU touch.
	// Flush persists LRU activity. Never bypass retry of a failed durable write.
	if old, ok := c.entries[fp]; ok && sameChain(old.Chain, chain) && !c.pendingSave {
		if _, valid := c.valid(old, now); valid {
			if now.After(old.Used) {
				old.Used = now
			}
			c.entries[fp] = old
			return fp, nil
		}
	}
	e := cachedIdentity{cloneChain(chain), now, now}
	if _, ok := c.valid(e, now); !ok {
		return "", errors.New("invalid identity bundle")
	}
	// Receiving a duplicate is not a retention renewal. A genuine fetch after
	// expiry may re-admit it, but never beyond current certificate validity.
	if old, ok := c.entries[fp]; ok {
		if _, valid := c.valid(old, now); valid {
			e.Added = old.Added
		}
	}
	c.entries[fp] = e
	c.prune(now)
	if err := c.save(); err != nil {
		c.pendingSave = true
		return "", err
	}
	c.pendingSave = false
	return fp, nil
}

func (c *IdentityCache) Get(fp string, now time.Time) ([][]byte, Identity, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[fp]
	if !ok {
		return nil, Identity{}, false
	}
	// A caller may have sampled its clock before another goroutine admitted
	// this bundle. Treat that snapshot as a miss, not a cache invalidation.
	if now.Before(e.Added) {
		return nil, Identity{}, false
	}
	id, ok := c.valid(e, now)
	if !ok {
		delete(c.entries, fp)
		return nil, Identity{}, false
	}
	if now.After(e.Used) {
		e.Used = now
	}
	c.entries[fp] = e
	return cloneChain(e.Chain), id, true
}

// Fingerprints returns a bounded snapshot of identities available under current
// trust and retention. It grants no lease: Get revalidates every later lookup.
func (c *IdentityCache) Fingerprints(now time.Time, limit int) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if limit <= 0 {
		return nil
	}
	out := make([]string, 0, min(limit, len(c.entries)))
	for fp, entry := range c.entries {
		if _, valid := c.valid(entry, now); valid {
			out = append(out, fp)
		}
	}
	sort.Strings(out)
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

// Flush persists LRU activity and removes expired entries. Call periodically and
// on orderly shutdown, not on every signed announcement.
func (c *IdentityCache) Flush(now time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.prune(now)
	err := c.save()
	c.pendingSave = err != nil
	return err
}

func (c *IdentityCache) save() error {
	if c.path == "" {
		return nil
	}
	data, err := json.Marshal(struct {
		Version int
		Entries map[string]cachedIdentity
	}{1, c.entries})
	if err != nil {
		return err
	}
	dir := filepath.Dir(c.path)
	if err = os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".identity-cache-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(f.Name(), c.path); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	if err = d.Sync(); err != nil {
		return fmt.Errorf("sync identity cache directory: %w", err)
	}
	return nil
}
