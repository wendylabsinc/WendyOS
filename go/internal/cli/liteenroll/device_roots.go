package liteenroll

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/cloudenroll"
	"github.com/wendylabsinc/wendy/go/internal/cli/cloudrequest"
	"github.com/wendylabsinc/wendy/go/internal/shared/literelay"
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
	host := cloudenroll.PKISiblingHost(pkiEndpoint, "est")
	if host == "" {
		return "", fmt.Errorf("selected session has no PKI CA discovery endpoint")
	}
	endpoint, err := literelay.LiteDeviceCAChainURL(host, tenantID)
	if err != nil {
		return "", err
	}
	return fetchLiteDeviceCAChain(ctx, endpoint)
}

// fetchLiteDeviceCAChain is replaced in tests.
var fetchLiteDeviceCAChain = func(ctx context.Context, endpoint string) (string, error) {
	return discoverLiteDeviceCAChain(ctx, http.DefaultClient, endpoint)
}

// discoverLiteDeviceCAChain is replaced in tests.
func discoverLiteDeviceCAChain(ctx context.Context, client *http.Client, endpoint string) (string, error) {
	return literelay.FetchLiteDeviceCAChain(ctx, client, endpoint)
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
	host := cloudenroll.PKISiblingHost(pkiEndpoint, "est")
	if host == "" {
		return "", fmt.Errorf("missing Lite discovery authority")
	}
	endpoint, err := literelay.LiteDeviceCAChainURL(host, tenantID)
	if err != nil {
		return "", err
	}
	return discoverLiteDeviceCAChain(ctx, client, endpoint)
}
