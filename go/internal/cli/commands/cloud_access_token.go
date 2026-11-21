package commands

import (
	"context"

	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

// EnsureCloudAccessToken refreshes a supplied Cloud session using the existing
// serialized OAuth/keychain persistence path. It does not prompt or select an account.
// Shared Cloud transport uses it for both app and command callers.
func EnsureCloudAccessToken(ctx context.Context, auth *config.AuthConfig) error {
	return ensureOAuthAccessToken(ctx, auth)
}
