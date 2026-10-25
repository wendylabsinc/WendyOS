package liteenroll

import (
	"context"
	"net/http"
	"sync"
	"time"

	wendy "github.com/wendylabsinc/wendy-go"
	sdk "github.com/wendylabsinc/wendy-go/lite/enrollment"
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

func discoverLiteDeviceCAChainUncached(ctx context.Context, pki, tenant string) (string, error) {
	endpoint, err := sdk.DeviceCAEndpoint(pki, tenant)
	if err != nil {
		return "", err
	}
	return fetchLiteDeviceCAChain(ctx, endpoint)
}

// fetchLiteDeviceCAChain is replaced in tests.
var fetchLiteDeviceCAChain = func(ctx context.Context, endpoint string) (string, error) {
	return discoverLiteDeviceCAChain(ctx, http.DefaultClient, endpoint)
}

func discoverLiteDeviceCAChain(ctx context.Context, c *http.Client, endpoint string) (string, error) {
	host, err := newDiscoveryHTTP(c, "Lite device CA discovery redirects are not allowed")
	if err != nil {
		return "", err
	}
	return sdk.DiscoverLiteDeviceCAChain(ctx, host, wendy.GoLiteCertificateInspector{}, endpoint)
}
