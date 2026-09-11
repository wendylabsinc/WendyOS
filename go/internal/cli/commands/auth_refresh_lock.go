package commands

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/flock"
)

// All OAuth refreshes share a lock because different sessions also write the
// same config.json. Hold it across reload, token rotation and persistence.
// Opening a separate file handle for each acquisition also serializes goroutines.
func acquireAuthRefreshLock(ctx context.Context) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	dir, err := config.ConfigDir()
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, "auth-refresh.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("opening authentication refresh lock: %w", err)
	}
	if err := blockLockFile(ctx, f); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("waiting for authentication refresh: %w", err)
	}
	return func() {
		_ = flock.Unlock(f)
		_ = f.Close()
		// Keep the inode: unlinking it could let another process lock a new
		// file while a waiter still holds the old one.
	}, nil
}

// Match AddAuth's full identity so refreshing one realm/dashboard/tenant never
// overwrites another session on the same Cloud endpoint.
func sameOAuthSession(a, b *config.AuthConfig) bool {
	return a.CloudDashboard == b.CloudDashboard && a.CloudGRPC == b.CloudGRPC &&
		a.OAuthIssuer == b.OAuthIssuer && a.OrganizationKey() == b.OrganizationKey()
}

func reloadOAuthSession(auth *config.AuthConfig) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("reloading OAuth session: %w", err)
	}
	for i := range cfg.Auth {
		if sameOAuthSession(&cfg.Auth[i], auth) {
			*auth = cfg.Auth[i]
			// Another process may have rotated secrets without changing their
			// Keychain references. Do not reuse this process's cached values.
			auth.InvalidateCachedSecrets()
			return nil
		}
	}
	return fmt.Errorf("OAuth session is no longer present in config; run 'wendy auth login' again")
}
