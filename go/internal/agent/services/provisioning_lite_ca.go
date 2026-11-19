package services

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/literelay"
	"go.uber.org/zap"
)

const (
	liteDeviceCAChainTTL   = 15 * time.Minute
	liteDeviceCAChainRetry = 30 * time.Second
	// liteDeviceCAChainDir keeps the last good chain per tenant, so SensorLink
	// to Lite boards survives a restart while PKI is unreachable.
	liteDeviceCAChainDir = "lite-device-ca"
)

// estHostFor derives the EST frontend from the ACME directory the device
// enrolled against, as renewEndpointFor derives renew.
func estHostFor(acmeDirectoryURL string) string {
	u, err := url.Parse(acmeDirectoryURL)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Port() != "" {
		return ""
	}
	rest, ok := strings.CutPrefix(u.Hostname(), "acme.")
	if !ok || rest == "" {
		return ""
	}
	return "est." + rest
}

// LiteDeviceCAChain returns the platform-chained Lite Device CA of this
// device's tenant. It is public CA material, used only as untrusted
// intermediates when verifying a SensorLink source on the classical Lite chain.
func (s *ProvisioningService) LiteDeviceCAChain(ctx context.Context) (string, error) {
	s.mu.Lock()
	enrolled, principal, directory := s.enrolled && s.resetRecoveryErr == nil, s.principalURI, s.acmeDirectoryURL
	s.mu.Unlock()
	if !enrolled {
		return "", errors.New("device is not enrolled")
	}
	identity, err := certs.ParsePrincipal(principal)
	if err != nil || identity.TenantUUID == "" {
		return "", errors.New("device identity names no tenant")
	}
	host := estHostFor(directory)
	if host == "" {
		return "", errors.New("enrollment names no PKI CA discovery endpoint")
	}
	endpoint, err := literelay.LiteDeviceCAChainURL(host, identity.TenantUUID)
	if err != nil {
		return "", err
	}
	stored := filepath.Join(s.configPath, liteDeviceCAChainDir, identity.TenantUUID+".pem")
	return s.liteCA.get(ctx, endpoint, stored, s.logger)
}

// liteCAChains caches one endpoint's chain. A caller waits for a fetch only
// when no chain is at hand: an expired chain, or the copy on disk after a
// restart, is served at once while one shared fetch refreshes it. A failed
// fetch is retried after liteDeviceCAChainRetry.
type liteCAChains struct {
	mu      sync.Mutex
	url     string
	chain   string    // last good chain, possibly stale
	fresh   time.Time // zero when chain came from disk
	err     error
	failed  time.Time
	pending chan struct{}

	now   func() time.Time
	fetch func(ctx context.Context, url string) (string, error)
}

func (c *liteCAChains) clock() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

func (c *liteCAChains) fetcher() func(context.Context, string) (string, error) {
	if c.fetch != nil {
		return c.fetch
	}
	return func(ctx context.Context, url string) (string, error) {
		return literelay.FetchLiteDeviceCAChain(ctx, http.DefaultClient, url)
	}
}

func (c *liteCAChains) get(ctx context.Context, endpoint, stored string, logger *zap.Logger) (string, error) {
	c.mu.Lock()
	if c.url != endpoint {
		c.url, c.chain, c.fresh, c.err, c.failed, c.pending = endpoint, "", time.Time{}, nil, time.Time{}, nil
		if data, err := os.ReadFile(stored); err == nil && len(data) > 0 {
			c.chain = string(data)
		}
	}
	now := c.clock()
	if c.chain != "" && !c.fresh.IsZero() && now.Sub(c.fresh) < liteDeviceCAChainTTL {
		defer c.mu.Unlock()
		return c.chain, nil
	}
	if c.pending == nil && (c.err == nil || now.Sub(c.failed) >= liteDeviceCAChainRetry) {
		done := make(chan struct{})
		c.pending = done
		fetch := c.fetcher()
		// Shared by every waiter, so one caller giving up must not cancel it;
		// the fetch's own timeout bounds it.
		go func() {
			chain, err := fetch(context.WithoutCancel(ctx), endpoint)
			c.mu.Lock()
			defer c.mu.Unlock()
			defer close(done)
			if c.pending == done {
				c.pending = nil
			}
			if c.url != endpoint {
				return
			}
			c.settle(chain, err, stored, logger)
		}()
	}
	// A source's handshake waits on this answer; an unreachable PKI must not
	// stall it while a chain is at hand.
	if c.chain != "" || c.pending == nil {
		defer c.mu.Unlock()
		return c.result()
	}
	done := c.pending
	c.mu.Unlock()
	select {
	case <-done:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.url != endpoint {
		return "", errors.New("device enrollment changed during Lite device CA discovery")
	}
	return c.result()
}

// settle records a finished fetch. Call with mu held.
func (c *liteCAChains) settle(chain string, err error, stored string, logger *zap.Logger) {
	now := c.clock()
	if err == nil && chain == "" {
		err = errors.New("empty Lite device CA chain")
	}
	if err == nil {
		c.chain, c.fresh, c.err = chain, now, nil
		if storeErr := storeLiteDeviceCAChain(stored, chain); storeErr != nil {
			logger.Warn("storing Lite device CA chain failed", zap.Error(storeErr))
		}
		return
	}
	c.err, c.failed = fmt.Errorf("discovering Lite device CA chain: %w", err), now
	if c.chain != "" {
		logger.Warn("Lite device CA discovery failed; using the last good chain", zap.Error(err))
	}
}

// result is the cached answer. Call with mu held.
func (c *liteCAChains) result() (string, error) {
	if c.chain != "" {
		return c.chain, nil
	}
	return "", c.err
}

// storeLiteDeviceCAChain replaces path in one rename, so a power loss leaves
// the previous chain or none, never a truncated one.
func storeLiteDeviceCAChain(path, chain string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".chain-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op once the rename below succeeds
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.WriteString(chain); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
