package liteenroll

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/wendylabsinc/wendy/go/internal/cli/cloudenroll"
	"github.com/wendylabsinc/wendy/go/internal/cli/cloudrequest"
)

const chainAlgorithmHeader = "X-Wendy-Chain-Algorithm"

// DiscoverLiteDeviceCAChain fetches the selected tenant's ML-DSA-signed Lite
// Device CA with its tenant and customer Intermediates. This is public CA
// material: callers use it only as untrusted intermediates that must chain to
// the operator session's trust bundle, never as trust anchors. Answers are
// reused per PKI endpoint and tenant for liteDeviceCAChainTTL, so a re-signed
// or revoked CA is picked up without a new login. A failure is reused for
// liteDeviceCAChainRetry, so an offline LAN fails fast instead of re-waiting.
// Concurrent callers share one in-flight fetch; each waits under its own ctx.
func DiscoverLiteDeviceCAChain(ctx context.Context, pkiEndpoint, tenantID string) (string, error) {
	key := pkiEndpoint + " " + tenantID
	liteDeviceCAChains.Lock()
	f, ok := liteDeviceCAChains.byKey[key]
	if !ok || f.stale() {
		f = &liteDeviceCAChainFetch{done: make(chan struct{})}
		liteDeviceCAChains.byKey[key] = f
		// Shared by every waiter, so one caller giving up must not cancel it;
		// discovery's own timeout bounds it.
		go func() {
			f.chain, f.err = discoverLiteDeviceCAChainUncached(context.WithoutCancel(ctx), pkiEndpoint, tenantID)
			f.fetched = time.Now()
			close(f.done)
		}()
	}
	liteDeviceCAChains.Unlock()
	select {
	case <-f.done:
		return f.chain, f.err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

const (
	liteDeviceCAChainTTL   = 15 * time.Minute
	liteDeviceCAChainRetry = 30 * time.Second
)

type liteDeviceCAChainFetch struct {
	done    chan struct{} // closed once chain, err and fetched are set
	chain   string
	err     error
	fetched time.Time
}

func (f *liteDeviceCAChainFetch) stale() bool {
	select {
	case <-f.done:
	default:
		return false // in flight
	}
	ttl := liteDeviceCAChainTTL
	if f.err != nil {
		ttl = liteDeviceCAChainRetry
	}
	return time.Since(f.fetched) >= ttl
}

var liteDeviceCAChains = struct {
	sync.Mutex
	byKey map[string]*liteDeviceCAChainFetch
}{byKey: map[string]*liteDeviceCAChainFetch{}}

func discoverLiteDeviceCAChainUncached(ctx context.Context, pkiEndpoint, tenantID string) (string, error) {
	id, err := uuid.Parse(tenantID)
	if err != nil || id.String() != tenantID || id == uuid.Nil {
		return "", fmt.Errorf("invalid tenant for Lite device CA discovery")
	}
	host := cloudenroll.PKISiblingHost(pkiEndpoint, "est")
	if host == "" {
		return "", fmt.Errorf("selected session has no PKI CA discovery endpoint")
	}
	endpoint := "https://" + host + "/.well-known/est/" + tenantID + "/cacerts?chain_algorithm=" + cloudrequest.EnrollmentChainP256 + "&full_chain=true"
	return fetchLiteDeviceCAChain(ctx, endpoint)
}

// fetchLiteDeviceCAChain is replaced in tests.
var fetchLiteDeviceCAChain = func(ctx context.Context, endpoint string) (string, error) {
	return discoverLiteDeviceCAChain(ctx, http.DefaultClient, endpoint)
}

// Fetched certs are intermediates only, never anchors, so X-Wendy-Chain-Algorithm is not checked here.
func discoverLiteDeviceCAChain(ctx context.Context, client *http.Client, endpoint string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err
	}
	if req.URL.Scheme != "https" || req.URL.User != nil || req.URL.Fragment != "" {
		return "", fmt.Errorf("Lite device CA discovery requires HTTPS without credentials or fragment")
	}
	secureClient := *client
	secureClient.CheckRedirect = func(*http.Request, []*http.Request) error {
		return fmt.Errorf("Lite device CA discovery redirects are not allowed")
	}
	resp, err := secureClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("discovering Lite device CA chain: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("Lite device CA discovery returned HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 128*1024+1))
	if err != nil {
		return "", err
	}
	if len(body) > 128*1024 {
		return "", fmt.Errorf("Lite device CA discovery response too large")
	}
	certs, err := discoveryCertificates(body, resp.Header.Get("Content-Type"))
	if err != nil {
		return "", fmt.Errorf("discovering Lite device CA chain: %w", err)
	}
	var chain []byte
	chained := false
	for _, cert := range certs {
		if !cert.IsCA {
			continue
		}
		chained = chained || !bytes.Equal(cert.RawIssuer, cert.RawSubject)
		chain = append(chain, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw})...)
	}
	// A PKI without full_chain support answers with the self-signed twin only.
	if !chained {
		return "", fmt.Errorf("PKI did not return the platform-chained Lite device CA; update PKI to support full_chain")
	}
	return string(chain), nil
}

func p256DiscoveryRoots(body []byte, header http.Header) ([]byte, error) {
	roots, err := discoveryRootsForProfile(body, header.Get("Content-Type"), TrustProfile{})
	if err != nil {
		return nil, fmt.Errorf("discovering P-256 device trust: %w", err)
	}
	// Older PKI servers may ignore unknown query parameters. Do not reserve a
	// credential for a new issuer after discovering a different CA hierarchy.
	values := header.Values(chainAlgorithmHeader)
	if len(values) != 1 || values[0] != cloudrequest.EnrollmentChainP256 {
		return nil, fmt.Errorf("PKI did not confirm the requested P-256 device chain; update PKI to support chain_algorithm=ECDSA_P256")
	}
	for rest := roots; len(rest) > 0; {
		block, tail := pem.Decode(rest)
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, err
		}
		key, ok := cert.PublicKey.(*ecdsa.PublicKey)
		if !ok || key.Curve != elliptic.P256() || cert.SignatureAlgorithm != x509.ECDSAWithSHA256 || cert.CheckSignatureFrom(cert) != nil {
			return nil, fmt.Errorf("PKI did not return a self-signed P-256 device CA")
		}
		rest = tail
	}
	return roots, nil
}

// DiscoverLiteDeviceCAChainWithClient uses the caller's verified HTTPS transport.
// Returned certificates remain intermediates, never new trust anchors.
func DiscoverLiteDeviceCAChainWithClient(ctx context.Context, client *http.Client, pkiEndpoint, tenantID string) (string, error) {
	id, err := uuid.Parse(tenantID)
	if err != nil || id.String() != tenantID || id == uuid.Nil {
		return "", fmt.Errorf("invalid Lite tenant")
	}
	host := cloudenroll.PKISiblingHost(pkiEndpoint, "est")
	if host == "" {
		return "", fmt.Errorf("missing Lite discovery authority")
	}
	return discoverLiteDeviceCAChain(ctx, client, "https://"+host+"/.well-known/est/"+tenantID+"/cacerts?chain_algorithm="+cloudrequest.EnrollmentChainP256+"&full_chain=true")
}
