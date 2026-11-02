// Package localmesh implements transport-independent wendy net facilities.
package localmesh

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

// maxPrincipalBytes bounds a device principal carried in a key, a file, or a
// control message. A canonical SPIFFE device principal is far shorter.
const maxPrincipalBytes = 512

// IdentityVerifier must validate the complete chain against CURRENT trust and
// return the authorized device identity and earliest certificate expiry. It is
// called on every lookup, including cache hits. A cache is never a trust store.
type IdentityVerifier func(chain [][]byte, now time.Time) (Identity, error)

// Identity is a verified device principal with the earliest expiry in the chain
// that produced it.
type Identity struct {
	Device   meshid.Device
	NotAfter time.Time
}

type CacheLimits struct {
	Entries, Bytes int
	Age            time.Duration
}

func DefaultCacheLimits() CacheLimits {
	return CacheLimits{Entries: 1024, Bytes: 8 << 20, Age: 24 * time.Hour}
}

type cachedIdentity struct {
	Chain [][]byte  `json:"chain" cbor:"c"`
	Added time.Time `json:"added" cbor:"a"`
	Used  time.Time `json:"used" cbor:"u"`
}

// IdentityCache persists public certificate bundles only; presence and service
// leases are deliberately absent. The file is scoped by caller to provisioning.
//
// Entries are keyed by the full device principal
// "spiffe://wendy.sh/tenant/<tenant-uuid>/device/<device-uuid>", so the same
// device UUID in two tenants is two entries and neither lookup can reach the
// other. There is no certificate-hash key and no numeric fallback key.
type IdentityCache struct {
	mu      sync.Mutex
	path    string
	limits  CacheLimits
	verify  IdentityVerifier
	entries map[string]cachedIdentity
	parts   *CertificatePool
	// A failed write must be retried even when a later Put is byte-identical.
	pendingSave bool
}

// principalOf returns the cache key a chain authorizes, rejecting chains whose
// leaf is not a device principal.
func principalOf(id Identity) (string, error) {
	principal := id.Device.Principal()
	if principal == "" || len(principal) > maxPrincipalBytes {
		return "", errors.New("identity has no device principal")
	}
	return principal, nil
}

func OpenIdentityCache(path string, limits CacheLimits, verify IdentityVerifier, now time.Time) (*IdentityCache, error) {
	return OpenIdentityCacheWithCertificates(path, limits, verify, now, NewCertificatePool())
}

// OpenIdentityCacheWithCertificates reuses public bytes from another cache
// without sharing that cache's identities or trust decisions.
func OpenIdentityCacheWithCertificates(path string, limits CacheLimits, verify IdentityVerifier, now time.Time, parts *CertificatePool) (*IdentityCache, error) {
	if limits.Entries < 1 || limits.Bytes < 1 || limits.Age <= 0 || verify == nil {
		return nil, errors.New("invalid identity cache configuration")
	}
	if parts == nil {
		return nil, errors.New("missing certificate pool")
	}
	c := &IdentityCache{path: path, limits: limits, verify: verify, entries: make(map[string]cachedIdentity), parts: parts}
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
	for principal, entry := range disk.Entries {
		if len(principal) > maxPrincipalBytes {
			continue
		}
		// A key that is not a device principal cannot be addressed by any peer.
		if _, err := meshid.DeviceFromPrincipal(principal); err != nil {
			continue
		}
		if entry.Used.After(now) {
			entry.Used = now
		}
		if _, ok := c.valid(principal, entry, now); ok {
			c.entries[principal] = entry
		}
	}
	c.prune(now)
	return c, nil
}

// valid re-runs current trust on the stored chain and requires the verified
// principal to be the entry's key. A chain that verifies for a different device
// is never a hit, whatever the file claimed.
func (c *IdentityCache) valid(principal string, e cachedIdentity, now time.Time) (Identity, bool) {
	if len(e.Chain) == 0 || len(e.Chain) > 16 || e.Added.IsZero() || now.Before(e.Added) || now.Sub(e.Added) >= c.limits.Age || e.Used.Before(e.Added) {
		log.Printf("wendy net identity cache unavailable: principal=%q stage=retention parts=%d added=%s used=%s now=%s", principal, len(e.Chain), e.Added.Format(time.RFC3339Nano), e.Used.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano))
		return Identity{}, false
	}
	id, err := c.verify(e.Chain, now)
	if err != nil {
		log.Printf("wendy net identity cache unavailable: principal=%q stage=verification reason=%q", principal, err)
		return Identity{}, false
	}
	key, err := principalOf(id)
	if err != nil || key != principal {
		log.Printf("wendy net identity cache unavailable: principal=%q stage=principal verified=%q reason=%v", principal, key, err)
		return Identity{}, false
	}
	if !now.Before(id.NotAfter) {
		log.Printf("wendy net identity cache unavailable: principal=%q stage=expiry expires=%s now=%s", principal, id.NotAfter.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano))
		return Identity{}, false
	}
	return id, true
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
	// Bytes bounds all retained certificate data, independently of the entry
	// count and whether this cache has a backing file.
	totalBytes := 0
	for principal, entry := range c.entries {
		if _, ok := c.valid(principal, entry, now); !ok {
			delete(c.entries, principal)
			c.pendingSave = true
		} else {
			totalBytes += chainSize(entry.Chain)
		}
	}
	for len(c.entries) > c.limits.Entries || totalBytes > c.limits.Bytes {
		oldest := ""
		var when time.Time
		for principal, entry := range c.entries {
			if oldest == "" || entry.Used.Before(when) || entry.Used.Equal(when) && principal < oldest {
				oldest, when = principal, entry.Used
			}
		}
		if oldest == "" {
			return
		}
		totalBytes -= chainSize(c.entries[oldest].Chain)
		delete(c.entries, oldest)
		c.pendingSave = true
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

// Put stores a chain under the principal its leaf authorizes. A renewed chain
// for the same device replaces the previous one; a chain for a different device
// never touches this device's entry.
func (c *IdentityCache) Put(chain [][]byte, now time.Time) (string, error) {
	return c.put(chain, "", now)
}

func (c *IdentityCache) put(chain [][]byte, expected string, now time.Time) (string, error) {
	if len(chain) == 0 || len(chain) > 16 || chainSize(chain) > c.limits.Bytes {
		return "", errors.New("identity bundle exceeds cache limits")
	}
	id, err := c.verify(chain, now)
	if err != nil {
		return "", err
	}
	principal, err := principalOf(id)
	if err != nil {
		return "", err
	}
	if expected != "" && principal != expected {
		return "", errors.New("certificate chain principal mismatch")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	old, ok := c.entries[principal]
	if ok {
		if sameChain(old.Chain, chain) {
			// The chain verified above is the stored chain, so its own expiry is
			// the validity check; re-verifying would double the cost of the
			// common case. Retention still runs from when it was added.
			if !now.Before(id.NotAfter) {
				return "", errors.New("identity bundle is not currently valid")
			}
			old.Used = now
			c.entries[principal] = old
			c.pendingSave = true
			c.rememberCertificates(chain, id.NotAfter, now)
			return principal, nil
		}
		// A chain still live under current trust is only replaced by one that
		// expires no earlier, so a replayed older bundle cannot displace it.
		// An entry that is already expired or untrusted is simply replaced.
		if previous, valid := c.valid(principal, old, now); valid && previous.NotAfter.After(id.NotAfter) {
			return "", errors.New("identity bundle is not a renewal")
		}
	}
	entry := cachedIdentity{Chain: cloneChain(chain), Added: now, Used: now}
	if _, valid := c.valid(principal, entry, now); !valid {
		return "", errors.New("identity bundle is not currently valid")
	}
	c.entries[principal] = entry
	c.prune(now)
	if err := c.save(); err != nil {
		if ok {
			c.entries[principal] = old
		} else {
			delete(c.entries, principal)
		}
		return "", err
	}
	c.rememberCertificates(chain, id.NotAfter, now)
	return principal, nil
}

func (c *IdentityCache) rememberCertificates(chain [][]byte, expires, now time.Time) {
	for _, data := range chain {
		c.parts.put(data, minTime(expires, now.Add(c.limits.Age)), now)
	}
}

// Certificates exposes only the bounded public-byte pool. All authorization
// continues through this cache's own verifier and principal-keyed entries.
func (c *IdentityCache) Certificates() *CertificatePool { return c.parts }

func (c *IdentityCache) certificate(hash string, now time.Time) []byte {
	if data := c.parts.get(hash, now); data != nil {
		return data
	}
	// Reuse a still-retained identity even if its element left the byte pool.
	// This also seeds our own enrolled issuers and public disk-cache entries.
	c.mu.Lock()
	defer c.mu.Unlock()
	for principal, entry := range c.entries {
		for _, data := range entry.Chain {
			if certificateHash(data) != hash {
				continue
			}
			if id, valid := c.valid(principal, entry, now); valid {
				c.parts.put(data, minTime(id.NotAfter, entry.Added.Add(c.limits.Age)), now)
				return append([]byte(nil), data...)
			}
		}
	}
	return nil
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

// Get returns the chain and identity stored for a device principal. The key is
// the principal, not a hash: a lookup for tenant A's device cannot resolve to
// tenant B's entry even when the device UUIDs are identical.
func (c *IdentityCache) Get(principal string, now time.Time) ([][]byte, Identity, bool) {
	if principal == "" || len(principal) > maxPrincipalBytes {
		return nil, Identity{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[principal]
	if !ok {
		return nil, Identity{}, false
	}
	id, ok := c.valid(principal, entry, now)
	if !ok {
		delete(c.entries, principal)
		c.pendingSave = true
		return nil, Identity{}, false
	}
	entry.Used = now
	c.entries[principal] = entry
	c.pendingSave = true
	return cloneChain(entry.Chain), id, true
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
		if _, valid := c.valid(fp, entry, now); valid {
			out = append(out, Fingerprint(entry.Chain))
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
	if !c.pendingSave {
		return nil
	}
	return c.save()
}

func (c *IdentityCache) save() error {
	if c.path == "" {
		c.pendingSave = false
		return nil
	}
	disk := struct {
		Version int
		Entries map[string]cachedIdentity
	}{Version: 1, Entries: c.entries}
	data, err := json.Marshal(disk)
	if err != nil {
		return err
	}
	if len(data) > c.limits.Bytes*2+c.limits.Entries*512 {
		return errors.New("oversized identity cache")
	}
	dir := filepath.Dir(c.path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	temp, err := os.CreateTemp(dir, ".identity-*")
	if err != nil {
		return err
	}
	name := temp.Name()
	defer os.Remove(name)
	if _, err = temp.Write(data); err != nil {
		temp.Close()
		return err
	}
	if err = temp.Sync(); err != nil {
		temp.Close()
		return err
	}
	if err = temp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(name, 0600); err != nil {
		return err
	}
	if err := os.Rename(name, c.path); err != nil {
		return err
	}
	c.pendingSave = false
	return nil
}
