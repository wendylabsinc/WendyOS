package mtls

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/asn1"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	circlSign "github.com/cloudflare/circl/sign"
)

const revocationRefresh = 30 * time.Second
const revocationTimeout = 5 * time.Second
const maxCRLBytes = 8 << 20

// Revocation checks use the issuer-signed distribution points, never an RPC
// argument. The issuer controls outbound CRL destinations, including private
// addresses for self-hosted PKI. Redirects and environment HTTP proxies are not
// used. A missing distribution point or unavailable fresh CRL denies access.
type revocationChecker struct {
	roots    []*x509.Certificate
	client   *http.Client
	now      func() time.Time
	mu       chan struct{}
	cache    map[[32]byte]cachedCRL
	interval time.Duration
}
type cachedCRL struct {
	list    *x509.RevocationList
	fetched time.Time
}

func newRevocationChecker(roots []*x509.Certificate) *revocationChecker {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	return &revocationChecker{
		roots:    roots,
		now:      time.Now,
		mu:       make(chan struct{}, 1),
		interval: revocationRefresh,
		cache:    make(map[[32]byte]cachedCRL),
		client: &http.Client{
			Timeout:       revocationTimeout,
			Transport:     transport,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

func (c *revocationChecker) check(ctx context.Context, state tls.ConnectionState) error {
	ctx, cancel := context.WithTimeout(ctx, revocationTimeout)
	defer cancel()
	if len(state.PeerCertificates) == 0 {
		return errors.New("revocation: missing peer certificate")
	}
	leaf := state.PeerCertificates[0]
	now := c.now()
	if now.Before(leaf.NotBefore) || !now.Before(leaf.NotAfter) {
		return errors.New("revocation: peer certificate is outside its validity window")
	}
	var issuer *x509.Certificate
	for _, ca := range append(append([]*x509.Certificate(nil), c.roots...), state.PeerCertificates[1:]...) {
		if !bytes.Equal(leaf.RawIssuer, ca.RawSubject) {
			continue
		}
		err := leaf.CheckSignatureFrom(ca)
		if err != nil {
			err = verifyMLDSASignature(ca, leaf)
		}
		if err == nil {
			issuer = ca
			break
		}
	}
	if issuer == nil {
		return errors.New("revocation: authenticated certificate issuer not found")
	}
	if now.Before(issuer.NotBefore) || !now.Before(issuer.NotAfter) {
		return errors.New("revocation: issuer certificate expired or not yet valid")
	}
	if len(leaf.CRLDistributionPoints) == 0 {
		return errors.New("revocation: certificate has no CRL distribution point; reissue it with revocation information")
	}
	// A context-aware lock coalesces fetches, bounding cache size and preventing
	// concurrent handshakes from flooding the same distribution endpoint.
	select {
	case c.mu <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-c.mu }()
	now = c.now()
	key := sha256.Sum256(issuer.Raw)
	cached, ok := c.cache[key]
	if !ok || now.Sub(cached.fetched) >= c.interval || !now.Before(cached.list.NextUpdate) || now.Before(cached.list.ThisUpdate) {
		var lastErr error
		var list *x509.RevocationList
		for _, endpoint := range leaf.CRLDistributionPoints {
			list, lastErr = c.fetch(ctx, endpoint, issuer, now)
			if lastErr == nil {
				break
			}
		}
		if lastErr != nil {
			return fmt.Errorf("revocation: fresh CRL unavailable: %w", lastErr)
		}
		if cached.list != nil && (list.Number.Cmp(cached.list.Number) < 0 || list.ThisUpdate.Before(cached.list.ThisUpdate)) {
			return errors.New("revocation: CRL rollback")
		}
		if len(c.cache) >= 16 {
			c.cache = make(map[[32]byte]cachedCRL)
		}
		cached = cachedCRL{list: list, fetched: now}
		c.cache[key] = cached
	}
	if now = c.now(); !now.Before(leaf.NotAfter) || now.Before(cached.list.ThisUpdate) || !now.Before(cached.list.NextUpdate) {
		return errors.New("revocation: evidence expired during check")
	}
	for _, entry := range cached.list.RevokedCertificateEntries {
		if leaf.SerialNumber.Cmp(entry.SerialNumber) == 0 {
			return errors.New("revocation: peer certificate revoked")
		}
	}
	return nil
}

func (c *revocationChecker) fetch(ctx context.Context, endpoint string, issuer *x509.Certificate, now time.Time) (*x509.RevocationList, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" || u.User != nil || u.Fragment != "" || (u.Scheme != "https" && u.Scheme != "http") {
		return nil, errors.New("invalid CRL distribution URL")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("CRL HTTP status %d", resp.StatusCode)
	}
	der, err := io.ReadAll(io.LimitReader(resp.Body, maxCRLBytes+1))
	if err != nil {
		return nil, err
	}
	if len(der) > maxCRLBytes {
		return nil, errors.New("CRL exceeds size limit")
	}
	list, err := x509.ParseRevocationList(der)
	if err != nil {
		return nil, err
	}
	if err = verifyCRL(list, issuer, now); err != nil {
		return nil, err
	}
	return list, nil
}

func verifyCRL(list *x509.RevocationList, issuer *x509.Certificate, now time.Time) error {
	if !issuer.IsCA || issuer.KeyUsage&x509.KeyUsageCRLSign == 0 || !bytes.Equal(list.RawIssuer, issuer.RawSubject) || len(list.AuthorityKeyId) == 0 || !bytes.Equal(list.AuthorityKeyId, issuer.SubjectKeyId) {
		return errors.New("CRL issuer mismatch or unauthorized signer")
	}
	if list.Number == nil || list.Number.Sign() < 0 || list.NextUpdate.IsZero() || now.Before(list.ThisUpdate) || !now.Before(list.NextUpdate) {
		return errors.New("CRL is stale or has invalid freshness metadata")
	}
	for _, ext := range list.Extensions {
		// Only full, direct CRLs are supported. Reject indirect, delta and
		// partitioned CRLs, whose omissions must never mean a cert is good.
		if ext.Id.String() == "2.5.29.27" || ext.Id.String() == "2.5.29.28" || ext.Critical {
			return errors.New("unsupported CRL extension")
		}
	}
	for _, entry := range list.RevokedCertificateEntries {
		for _, ext := range entry.Extensions {
			if ext.Critical || ext.Id.String() == "2.5.29.29" {
				return errors.New("unsupported CRL entry extension")
			}
		}
	}
	if list.SignatureAlgorithm != x509.UnknownSignatureAlgorithm {
		return list.CheckSignatureFrom(issuer)
	}
	var outer certOuter
	rest, err := asn1.Unmarshal(list.Raw, &outer)
	if err != nil || len(rest) != 0 {
		return errors.New("invalid CRL signature encoding")
	}
	scheme, err := mldsaScheme(outer.SignatureAlgorithm.Algorithm)
	if err != nil {
		return err
	}
	oid, rawKey, err := issuerPublicKeyBytes(issuer)
	if err != nil {
		return err
	}
	if !oid.Equal(outer.SignatureAlgorithm.Algorithm) || len(outer.SignatureAlgorithm.Parameters.FullBytes) != 0 || outer.Signature.BitLength != len(outer.Signature.Bytes)*8 {
		return errors.New("invalid ML-DSA CRL algorithm parameters")
	}
	key, err := scheme.UnmarshalBinaryPublicKey(rawKey)
	if err != nil {
		return err
	}
	if !scheme.Verify(key, list.RawTBSRevocationList, list.Signature, &circlSign.SignatureOpts{Context: ""}) {
		return errors.New("invalid ML-DSA CRL signature")
	}
	return nil
}
