// Package revocation checks issuer-signed full CRLs for live peer authentication.
// Callers must authenticate a certificate path before checking it: the issuer
// passed to Check keys the evidence, so an unauthenticated issuer could vouch
// for itself with a CRL it signed.
package revocation

import (
	"bytes"
	"context"
	"crypto/sha1" // RFC 5280 key identifier, not a signature hash.
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	RefreshInterval = 5 * time.Minute
	retryInterval   = 30 * time.Second
	maxCRLBytes     = 4 << 20
	maxEntries      = 128
	maxCacheBytes   = 32 << 20
	// A stapled CRL travels in the TLS Certificate message, which Go caps at
	// 256 KiB together with the chain. Larger lists are fetched, not stapled.
	// ponytail: roughly 3000 revoked serials per issuer; a delta or OCSP-style
	// staple is the upgrade path if a tenant outgrows it.
	maxStapleBytes = 128 << 10
	maxPending     = 16
)

var (
	ErrRevoked     = errors.New("certificate revoked")
	ErrUnavailable = errors.New("certificate revocation evidence unavailable")
	Default        = New(nil)
)

// Level is how much revocation evidence a decision needs. A certificate that
// a verified CRL lists is refused at every level.
type Level int

const (
	// RevokedOnly never fails on missing or expired evidence. It is for
	// established connections and for device and server certificates, whose
	// revocation has its own authority channel.
	RevokedOnly Level = iota
	// Session admits new operator authority when evidence is fresh or stale by
	// no more than the configured grace.
	Session
	// Fresh additionally refuses stale evidence. It gates destructive operations.
	Fresh
)

// Config is set once at process start. The zero value keeps evidence in
// memory, fetches the certificate's own distribution URL and enforces no grace.
type Config struct {
	// Dir persists the newest verified CRL per issuer so a restart keeps both
	// the evidence and its CRL-number floor.
	Dir string
	// Mirror replaces scheme and host of certificate distribution URLs, for
	// self-hosted, proxied and NAT64 sites. It is local configuration, so the
	// public-address restriction does not apply to it.
	Mirror string
	// Grace is how long past NextUpdate evidence still admits new operator
	// sessions. Zero requires fresh evidence. Negative never refuses on age:
	// stale and missing evidence are logged and accepted.
	Grace time.Duration
	// Logf receives rate-limited warnings about stale or missing evidence.
	Logf func(format string, args ...any)
}

// ConfigFromEnv reads WENDY_CRL_MIRROR and WENDY_CRL_GRACE (a Go duration).
// An unset or invalid grace never refuses on age.
func ConfigFromEnv(dir string) Config {
	c := Config{Dir: dir, Mirror: os.Getenv("WENDY_CRL_MIRROR"), Grace: -1}
	if grace, err := time.ParseDuration(os.Getenv("WENDY_CRL_GRACE")); err == nil && grace >= 0 {
		c.Grace = grace
	}
	return c
}

type entry struct {
	fetchMu     sync.Mutex
	mu          sync.Mutex
	issuer      *x509.Certificate
	url         string
	nextAttempt time.Time
}

type snapshot struct {
	crl     *x509.RevocationList
	revoked map[string]bool
	warned  time.Time
}

// Manager holds a bounded cache of the newest verified CRL per issuer. Run
// refreshes registered CRLs until cancellation. A nil client selects the
// public-address-only transport. A supplied client is for controlled
// transports, such as an in-process test.
type Manager struct {
	mu       sync.Mutex
	cfg      Config
	latest   map[[sha256.Size]byte]snapshot
	loaded   map[[sha256.Size]byte]bool   // Disk consulted once per issuer.
	pending  map[[sha256.Size]byte][]byte // Stapled, unverified, by issuer name.
	entries  map[string]*entry
	bytes    int
	client   *http.Client
	mirror   *http.Client
	revision atomic.Uint64
	slots    chan struct{}
}

func New(client *http.Client) *Manager {
	mirror := client
	if client == nil {
		client = publicClient()
		mirror = &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
			Transport: &http.Transport{TLSHandshakeTimeout: 5 * time.Second, MaxResponseHeaderBytes: 16 << 10, DisableKeepAlives: true}}
	}
	return &Manager{cfg: Config{Grace: -1}, entries: make(map[string]*entry), latest: make(map[[sha256.Size]byte]snapshot),
		loaded: make(map[[sha256.Size]byte]bool), pending: make(map[[sha256.Size]byte][]byte), client: client, mirror: mirror, slots: make(chan struct{}, 4)}
}

// Configure must run before the first check.
func (m *Manager) Configure(c Config) error {
	if c.Mirror != "" {
		u, err := url.Parse(c.Mirror)
		if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
			return errors.New("invalid CRL mirror URL")
		}
	}
	if c.Dir != "" {
		if err := os.MkdirAll(c.Dir, 0o700); err != nil {
			return err
		}
	}
	m.mu.Lock()
	m.cfg = c
	m.mu.Unlock()
	return nil
}

// Enforcing reports whether a grace is configured, so that Session and Fresh
// can refuse where RevokedOnly would not.
func (m *Manager) Enforcing() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cfg.Grace >= 0
}

// Revision changes whenever accepted CRL contents change. Cryptographic path
// caches must include this revision.
func (m *Manager) Revision() uint64 { return m.revision.Load() }

// CheckPath checks every certificate except the locally trusted anchor.
// Certificates without a distribution point retain legacy policy unless a
// known CRL from their issuer lists them.
func (m *Manager) CheckPath(path []*x509.Certificate, now time.Time, level Level) error {
	if len(path) == 0 {
		return errors.New("missing verified path")
	}
	for i := 0; i+1 < len(path); i++ {
		if err := m.Check(path[i], path[i+1], now, level); err != nil {
			return err
		}
	}
	return nil
}

// CheckPaths accepts any authenticated path that passes. Unrelated
// peer-supplied certificates are never checked as path members.
func (m *Manager) CheckPaths(paths [][]*x509.Certificate, now time.Time, level Level) error {
	var last error = ErrUnavailable
	for _, path := range paths {
		if last = m.CheckPath(path, now, level); last == nil {
			return nil
		}
	}
	return last
}

// Check decides one authenticated child/issuer pair. It never blocks on the
// network unless the answer would otherwise be a refusal for lack of evidence.
func (m *Manager) Check(child, issuer *x509.Certificate, now time.Time, level Level) error {
	key := issuerCacheKey(issuer)
	m.restore(issuer, key)
	entries := m.register(child, issuer, key)
	err := m.decide(child, key, now, level)
	// One bounded attempt per endpoint, throttled by its retry deadline.
	for _, e := range entries {
		if !errors.Is(err, ErrUnavailable) {
			break
		}
		m.refresh(context.Background(), e, false)
		err = m.decide(child, key, now, level)
	}
	return err
}

func (m *Manager) decide(child *x509.Certificate, key [sha256.Size]byte, now time.Time, level Level) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	current := m.latest[key]
	if current.revoked[child.SerialNumber.String()] {
		return fmt.Errorf("%w: serial %s", ErrRevoked, child.SerialNumber)
	}
	if len(child.CRLDistributionPoints) == 0 || current.crl != nil && now.Before(current.crl.NextUpdate) {
		return nil
	}
	state := "missing"
	stale := time.Duration(-1)
	if current.crl != nil {
		stale = now.Sub(current.crl.NextUpdate)
		state = "stale since " + current.crl.NextUpdate.UTC().Format(time.RFC3339)
	}
	grace := m.cfg.Grace
	refuse := level == Fresh && grace >= 0 || level == Session && grace >= 0 && (stale < 0 || stale > grace)
	if refuse {
		return fmt.Errorf("%w: CRL for %q is %s", ErrUnavailable, child.Issuer.String(), state)
	}
	if m.cfg.Logf != nil && time.Since(current.warned) > time.Hour {
		current.warned = time.Now()
		m.latest[key] = current
		m.cfg.Logf("revocation evidence for issuer %q is %s; accepting without a current CRL", child.Issuer.String(), state)
	}
	return nil
}

// register schedules background refresh of the child's distribution points.
func (m *Manager) register(child, issuer *x509.Certificate, key [sha256.Size]byte) []*entry {
	var entries []*entry
	m.mu.Lock()
	defer m.mu.Unlock()
	// Bound work even for an authenticated certificate with many distribution points.
	for i, endpoint := range child.CRLDistributionPoints {
		if i == 4 {
			break
		}
		if validateURL(endpoint) != nil {
			continue
		}
		id := fmt.Sprintf("%x/%s", key, endpoint)
		e := m.entries[id]
		if e == nil {
			if len(m.entries) >= maxEntries {
				continue
			}
			e = &entry{issuer: issuer, url: endpoint}
			if info, err := os.Stat(m.path(key)); err == nil && m.latest[key].crl != nil {
				// A short-lived CLI must not refetch what it persisted moments ago.
				e.nextAttempt = info.ModTime().Add(refreshAfter(m.latest[key].crl, info.ModTime()))
			}
			m.entries[id] = e
		}
		entries = append(entries, e)
	}
	return entries
}

func refreshAfter(crl *x509.RevocationList, now time.Time) time.Duration {
	half := crl.NextUpdate.Sub(now) / 2
	if half <= 0 {
		return retryInterval
	}
	return min(RefreshInterval, max(time.Second, half))
}

// restore adopts evidence this process has not verified yet: the persisted
// CRL once per issuer, and any stapled CRL naming this issuer.
func (m *Manager) restore(issuer *x509.Certificate, key [sha256.Size]byte) {
	name := sha256.Sum256(issuer.RawSubject)
	m.mu.Lock()
	first := !m.loaded[key]
	m.loaded[key] = true
	stapled := m.pending[name]
	delete(m.pending, name)
	file := m.path(key)
	m.mu.Unlock()
	if first && file != "" {
		if info, err := os.Stat(file); err == nil && info.Size() <= maxCRLBytes {
			if raw, err := os.ReadFile(file); err == nil {
				_ = m.adopt(issuer, raw, false)
			}
		}
	}
	if stapled != nil {
		_ = m.adopt(issuer, stapled, false)
	}
}

func (m *Manager) path(key [sha256.Size]byte) string {
	if m.cfg.Dir == "" {
		return ""
	}
	return filepath.Join(m.cfg.Dir, fmt.Sprintf("%x.crl", key))
}

// Offer accepts CRLs a peer stapled into its TLS handshake, concatenated DER.
// They stay unverified until an authenticated path names their issuer, so an
// offer can never vouch for a certificate chain on its own.
func (m *Manager) Offer(stapled []byte) {
	for len(stapled) > 0 && len(stapled) <= maxStapleBytes {
		var raw asn1.RawValue
		rest, err := asn1.Unmarshal(stapled, &raw)
		if err != nil {
			return
		}
		stapled = rest
		crl, err := x509.ParseRevocationList(raw.FullBytes)
		if err != nil {
			continue
		}
		m.mu.Lock()
		if len(m.pending) >= maxPending {
			clear(m.pending)
		}
		m.pending[sha256.Sum256(crl.RawIssuer)] = raw.FullBytes
		m.mu.Unlock()
	}
}

// Staple returns the known CRLs covering a local credential chain, for
// tls.Certificate.OCSPStaple. Go sends that field with client and server
// certificates in TLS 1.3 without interpreting it; Wendy peers read it through
// Offer. It also schedules refresh so an online machine keeps the staple fresh.
func (m *Manager) Staple(chain []*x509.Certificate) []byte {
	var out []byte
	for i, child := range chain {
		if len(child.CRLDistributionPoints) == 0 {
			continue
		}
		for _, issuer := range chain[i+1:] {
			if !bytes.Equal(child.RawIssuer, issuer.RawSubject) {
				continue
			}
			key := issuerCacheKey(issuer)
			m.restore(issuer, key)
			entries := m.register(child, issuer, key)
			go func() {
				for _, e := range entries {
					m.refresh(context.Background(), e, false)
				}
			}()
			m.mu.Lock()
			if crl := m.latest[key].crl; crl != nil && len(out)+len(crl.Raw) <= maxStapleBytes {
				out = append(out, crl.Raw...)
			}
			m.mu.Unlock()
			break
		}
	}
	return out
}

// Run performs bounded concurrent refreshes independently of handshakes and
// traffic. No endpoints are fetched until an authenticated path registers them.
func (m *Manager) Run(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.refreshEntries(ctx, false)
		}
	}
}

// Refresh immediately refreshes all registered endpoints. Run uses the same
// bounded worker pool but respects each endpoint's refresh/retry deadline.
func (m *Manager) Refresh(ctx context.Context) { m.refreshEntries(ctx, true) }

func (m *Manager) refreshEntries(ctx context.Context, force bool) {
	m.mu.Lock()
	entries := make([]*entry, 0, len(m.entries))
	for _, e := range m.entries {
		entries = append(entries, e)
	}
	m.mu.Unlock()
	var wg sync.WaitGroup
	for _, e := range entries {
		if ctx.Err() != nil {
			break
		}
		e.mu.Lock()
		due := force || !time.Now().Before(e.nextAttempt)
		e.mu.Unlock()
		if !due {
			continue
		}
		wg.Add(1)
		go func(e *entry) { defer wg.Done(); m.refresh(ctx, e, force) }(e)
	}
	wg.Wait()
}

// Keep the last authenticated CRL on every failure, including rollback, so an
// outage cannot erase known revocations. Network I/O holds no shared lock.
func (m *Manager) refresh(parent context.Context, e *entry, force bool) {
	e.fetchMu.Lock()
	defer e.fetchMu.Unlock()
	e.mu.Lock()
	if !force && time.Now().Before(e.nextAttempt) {
		e.mu.Unlock()
		return
	}
	e.nextAttempt = time.Now().Add(retryInterval)
	e.mu.Unlock()
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	select {
	case m.slots <- struct{}{}:
	case <-ctx.Done():
		return
	}
	defer func() { <-m.slots }()
	crl, err := m.fetch(ctx, e)
	if err == nil {
		e.mu.Lock()
		e.nextAttempt = time.Now().Add(refreshAfter(crl, time.Now()))
		e.mu.Unlock()
	}
}

func (m *Manager) fetch(ctx context.Context, e *entry) (*x509.RevocationList, error) {
	target, client := e.url, m.client
	m.mu.Lock()
	mirror := m.cfg.Mirror
	m.mu.Unlock()
	if mirror != "" {
		u, err := url.Parse(e.url)
		if err != nil {
			return nil, err
		}
		target, client = strings.TrimRight(mirror, "/")+u.EscapedPath(), m.mirror
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/pkix-crl")
	// A CDN's max-age must not hide new revocations until NextUpdate.
	req.Header.Set("Cache-Control", "no-cache")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("CRL HTTP status %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxCRLBytes+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxCRLBytes {
		return nil, errors.New("oversized CRL")
	}
	if err := m.adopt(e.issuer, raw, true); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.latest[issuerCacheKey(e.issuer)].crl, nil
}

// adopt verifies a CRL against its authenticated issuer and keeps it when it
// is newer than what is known. Only a fetched CRL older than the known one is
// an error; stapled and persisted copies are routinely older and are ignored.
func (m *Manager) adopt(issuer *x509.Certificate, raw []byte, fetched bool) error {
	crl, err := x509.ParseRevocationList(raw)
	if err != nil {
		return err
	}
	if err := validateCRL(crl, issuer); err != nil {
		return err
	}
	key := issuerCacheKey(issuer)
	m.mu.Lock()
	previous := m.latest[key]
	size := len(crl.Raw)
	if previous.crl != nil {
		older := crl.Number.Cmp(previous.crl.Number) < 0 || crl.ThisUpdate.Before(previous.crl.ThisUpdate)
		same := crl.Number.Cmp(previous.crl.Number) == 0
		switch {
		case older && fetched:
			err = errors.New("CRL rollback")
		case same && !bytes.Equal(crl.Raw, previous.crl.Raw):
			err = errors.New("conflicting CRL number")
		}
		if err != nil || older || same {
			m.mu.Unlock()
			return err
		}
		size -= len(previous.crl.Raw)
	} else if len(m.latest) >= maxEntries {
		m.mu.Unlock()
		return errors.New("CRL cache capacity reached")
	}
	if m.bytes+size > maxCacheBytes {
		m.mu.Unlock()
		return errors.New("CRL cache byte capacity reached")
	}
	m.bytes += size
	revoked := make(map[string]bool, len(crl.RevokedCertificateEntries))
	for _, r := range crl.RevokedCertificateEntries {
		revoked[r.SerialNumber.String()] = true
	}
	m.latest[key] = snapshot{crl: crl, revoked: revoked}
	m.revision.Add(1)
	file := m.path(key)
	m.mu.Unlock()
	if file != "" {
		// Best effort: evidence in memory still applies when the disk is full.
		tmp := file + ".tmp"
		if os.WriteFile(tmp, crl.Raw, 0o600) == nil {
			_ = os.Rename(tmp, file)
		}
	}
	return nil
}

// validateCRL deliberately has no clock: an authentic CRL is adopted for its
// revocations whatever the local time, and Check measures freshness against
// the caller's trusted time. A CRL issued after a lagging local clock is fresh.
func validateCRL(crl *x509.RevocationList, issuer *x509.Certificate) error {
	if !bytes.Equal(crl.RawIssuer, issuer.RawSubject) || len(crl.AuthorityKeyId) != 0 && !bytes.Equal(crl.AuthorityKeyId, issuerKeyID(issuer)) {
		return errors.New("CRL issuer mismatch")
	}
	// The repository's Go toolchain supports native ML-DSA as well as classical signatures.
	if err := crl.CheckSignatureFrom(issuer); err != nil {
		return fmt.Errorf("CRL signature: %w", err)
	}
	if crl.Number == nil || crl.Number.Sign() < 0 || !crl.ThisUpdate.Before(crl.NextUpdate) {
		return errors.New("missing CRL number or invalid CRL validity window")
	}
	for _, ext := range crl.Extensions {
		// Delta and scoped/indirect CRLs require semantics this full-CRL cache
		// deliberately does not implement, even if incorrectly marked noncritical.
		if ext.Id.String() == "2.5.29.27" || ext.Id.String() == "2.5.29.28" || ext.Critical {
			return errors.New("unsupported CRL extension")
		}
	}
	for _, entry := range crl.RevokedCertificateEntries {
		if entry.SerialNumber == nil || entry.SerialNumber.Sign() <= 0 || entry.ReasonCode == 8 {
			return errors.New("invalid full CRL entry")
		}
		for _, ext := range entry.Extensions {
			if ext.Critical || ext.Id.String() == "2.5.29.29" {
				return errors.New("unsupported CRL entry extension")
			}
		}
	}
	return nil
}

// PKI uses the issuer SKI or RFC 5280 method 1 for older CA certificates that
// omit it. Bind the CRL to that same identifier before checking its signature.
func issuerKeyID(issuer *x509.Certificate) []byte {
	if len(issuer.SubjectKeyId) != 0 {
		return issuer.SubjectKeyId
	}
	var spki struct {
		Algorithm pkix.AlgorithmIdentifier
		PublicKey asn1.BitString
	}
	if _, err := asn1.Unmarshal(issuer.RawSubjectPublicKeyInfo, &spki); err != nil {
		return nil
	}
	hash := sha1.Sum(spki.PublicKey.RightAlign())
	return hash[:]
}

// Cross-signed or renewed CA certificates with the same subject and public key
// share revocation history. Their certificate wrappers must not reset it.
func issuerCacheKey(issuer *x509.Certificate) [sha256.Size]byte {
	h := sha256.New()
	h.Write(issuer.RawSubject)
	h.Write(issuer.RawSubjectPublicKeyInfo)
	var key [sha256.Size]byte
	copy(key[:], h.Sum(nil))
	return key
}
