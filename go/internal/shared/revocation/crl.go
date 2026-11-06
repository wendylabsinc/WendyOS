// Package revocation checks issuer-signed full CRLs for live peer authentication.
// Callers must authenticate a certificate path before passing it to CheckPath.
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
)

var (
	ErrRevoked     = errors.New("certificate revoked")
	ErrUnavailable = errors.New("fresh certificate revocation list unavailable")
	Default        = New(nil)
)

type entry struct {
	fetchMu     sync.Mutex
	mu          sync.Mutex
	issuer      *x509.Certificate
	url         string
	crl         *x509.RevocationList
	nextAttempt time.Time
	err         error
}

type snapshot struct {
	crl     *x509.RevocationList
	revoked map[string]bool
}

// Manager holds a bounded, process-local cache. Run refreshes registered CRLs
// until cancellation. A nil client selects the public-address-only transport.
// A supplied client is for controlled transports, such as an in-process test.
type Manager struct {
	latest   map[[sha256.Size]byte]snapshot
	mu       sync.Mutex
	entries  map[string]*entry
	bytes    int
	client   *http.Client
	revision atomic.Uint64
	slots    chan struct{}
}

func New(client *http.Client) *Manager {
	if client == nil {
		client = publicClient()
	}
	return &Manager{entries: make(map[string]*entry), latest: make(map[[sha256.Size]byte]snapshot), client: client, slots: make(chan struct{}, 4)}
}

// Revision changes whenever accepted CRL contents change. Cryptographic path
// caches must include this revision and must not outlive CheckPath's deadline.
func (m *Manager) Revision() uint64 { return m.revision.Load() }

// CheckPath checks every certificate except the locally trusted anchor. It
// returns the earliest certificate/CRL expiry, so cached authorization cannot
// outlive signed evidence. Certificates without a CDP retain legacy policy.
func (m *Manager) CheckPath(path []*x509.Certificate, now time.Time) (time.Time, error) {
	if len(path) == 0 {
		return time.Time{}, errors.New("missing verified path")
	}
	until := path[0].NotAfter
	for _, c := range path {
		if c.NotAfter.Before(until) {
			until = c.NotAfter
		}
	}
	for i := 0; i+1 < len(path); i++ {
		child, issuer := path[i], path[i+1]
		if len(child.CRLDistributionPoints) == 0 {
			continue
		}
		var last error
		accepted := false
		// Bound work even for an authenticated certificate with many distribution points.
		for j, endpoint := range child.CRLDistributionPoints {
			if j == 4 {
				break
			}
			expiry, err := m.check(child, issuer, endpoint, now)
			if errors.Is(err, ErrRevoked) {
				return time.Time{}, err
			}
			if err != nil {
				last = err
				continue
			}
			if expiry.Before(until) {
				until = expiry
			}
			accepted = true
			break
		}
		if !accepted {
			return time.Time{}, fmt.Errorf("%w: %v", ErrUnavailable, last)
		}
	}
	return until, nil
}

func (m *Manager) check(child, issuer *x509.Certificate, endpoint string, now time.Time) (time.Time, error) {
	if err := validateURL(endpoint); err != nil {
		return time.Time{}, err
	}
	key := fmt.Sprintf("%x/%s", issuerCacheKey(issuer), endpoint)
	m.mu.Lock()
	e := m.entries[key]
	if e == nil {
		if len(m.entries) >= maxEntries {
			m.mu.Unlock()
			return time.Time{}, errors.New("CRL cache capacity reached")
		}
		e = &entry{issuer: issuer, url: endpoint}
		m.entries[key] = e
	}
	m.mu.Unlock()

	e.mu.Lock()
	initial := e.crl == nil && e.err == nil
	e.mu.Unlock()
	if initial {
		m.refresh(context.Background(), e, false)
	}
	e.mu.Lock()
	defer e.mu.Unlock()

	m.mu.Lock()
	current := m.latest[issuerCacheKey(issuer)]
	m.mu.Unlock()
	if current.crl != nil && current.revoked[child.SerialNumber.String()] {
		return time.Time{}, fmt.Errorf("%w: serial %s", ErrRevoked, child.SerialNumber)
	}
	if current.crl == nil {
		if e.err != nil {
			return time.Time{}, fmt.Errorf("%w: %v", ErrUnavailable, e.err)
		}
		return time.Time{}, ErrUnavailable
	}
	if now.Before(current.crl.ThisUpdate) || !now.Before(current.crl.NextUpdate) {
		return time.Time{}, fmt.Errorf("%w: valid from %s until %s", ErrUnavailable, current.crl.ThisUpdate.UTC().Format(time.RFC3339), current.crl.NextUpdate.UTC().Format(time.RFC3339))
	}
	return current.crl.NextUpdate, nil
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
// outage cannot erase known revocations. Network I/O never holds the read lock.
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
		e.mu.Lock()
		e.err = ctx.Err()
		e.mu.Unlock()
		return
	}
	defer func() { <-m.slots }()
	err := m.fetch(ctx, e)
	e.mu.Lock()
	e.err = err
	e.mu.Unlock()
}

func (m *Manager) fetch(ctx context.Context, e *entry) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, e.url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/pkix-crl")
	// A CDN's max-age must not hide new revocations until NextUpdate.
	req.Header.Set("Cache-Control", "no-cache")
	resp, err := m.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("CRL HTTP status %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxCRLBytes+1))
	if err != nil {
		return err
	}
	if len(raw) > maxCRLBytes {
		return errors.New("oversized CRL")
	}
	crl, err := x509.ParseRevocationList(raw)
	if err != nil {
		return err
	}
	now := time.Now()
	if err := validateCRL(crl, e.issuer, now); err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	m.mu.Lock()
	defer m.mu.Unlock()
	issuerKey := issuerCacheKey(e.issuer)
	previous := m.latest[issuerKey]
	if previous.crl != nil {
		if crl.Number.Cmp(previous.crl.Number) < 0 || crl.ThisUpdate.Before(previous.crl.ThisUpdate) {
			return errors.New("CRL rollback")
		}
		if crl.Number.Cmp(previous.crl.Number) == 0 && !bytes.Equal(crl.Raw, previous.crl.Raw) {
			return errors.New("conflicting CRL number")
		}
	}

	size := len(raw)
	if e.crl != nil {
		size -= len(e.crl.Raw)
	}
	if m.bytes+size > maxCacheBytes {
		return errors.New("CRL cache byte capacity reached")
	}
	m.bytes += size
	changed := previous.crl == nil || !bytes.Equal(previous.crl.Raw, crl.Raw)
	revoked := make(map[string]bool, len(crl.RevokedCertificateEntries))
	for _, r := range crl.RevokedCertificateEntries {
		revoked[r.SerialNumber.String()] = true
	}
	e.crl = crl
	m.latest[issuerKey] = snapshot{crl: crl, revoked: revoked}
	e.nextAttempt = now.Add(min(RefreshInterval, max(time.Second, crl.NextUpdate.Sub(now)/2)))
	if changed {
		m.revision.Add(1)
	}
	return nil
}

func validateCRL(crl *x509.RevocationList, issuer *x509.Certificate, now time.Time) error {
	if !bytes.Equal(crl.RawIssuer, issuer.RawSubject) || len(crl.AuthorityKeyId) != 0 && !bytes.Equal(crl.AuthorityKeyId, issuerKeyID(issuer)) {
		return errors.New("CRL issuer mismatch")
	}
	// The repository's Go toolchain supports native ML-DSA as well as classical signatures.
	if err := crl.CheckSignatureFrom(issuer); err != nil {
		return fmt.Errorf("CRL signature: %w", err)
	}
	if crl.Number == nil || crl.Number.Sign() < 0 || now.Before(crl.ThisUpdate) || !now.Before(crl.NextUpdate) || !crl.ThisUpdate.Before(crl.NextUpdate) {
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

// CheckPaths accepts any authenticated path with current revocation evidence.
// Unrelated peer-supplied certificates are never checked as path members.
func (m *Manager) CheckPaths(paths [][]*x509.Certificate, now time.Time) (time.Time, error) {
	var last error = ErrUnavailable
	var latest time.Time
	for _, path := range paths {
		until, err := m.CheckPath(path, now)
		if err != nil {
			last = err
			continue
		}
		if until.After(latest) {
			latest = until
		}
	}
	if latest.IsZero() {
		return time.Time{}, last
	}
	return latest, nil
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
