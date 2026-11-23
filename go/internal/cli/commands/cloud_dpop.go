//go:build darwin || linux || windows

package commands

import (
	"google.golang.org/grpc"

	"github.com/wendylabsinc/wendy/go/internal/cli/cloudrequest"
	"github.com/wendylabsinc/wendy/go/internal/shared/cloudsession"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

// authIsDPoPBound reports whether the session's access token must be presented
// as DPoP rather than Bearer (WDY-3107). Thin alias over the shared predicate so
// cloudContext and the DPoP wiring agree.
func authIsDPoPBound(auth *config.AuthConfig) bool {
	return cloudrequest.IsDPoPBound(auth)
}

// dpopDialOptions installs the shared per-RPC DPoP interceptor for a bound
// session, driven by the CLI token provider (which refreshes an expired token).
// Unbound sessions get nil and keep Bearer. Every commands-package Cloud dial
// appends it.
func dpopDialOptions(auth *config.AuthConfig) []grpc.DialOption {
	return cloudrequest.DPoPDialOptions(auth, cliDPoPTokenProvider(auth))
}

// cliDPoPTokenProvider refreshes the OAuth access token if needed and returns it
// with its bound ML-DSA key. It snapshots the session so concurrent RPCs on one
// dialed conn do not race on shared state.
func cliDPoPTokenProvider(auth *config.AuthConfig) cloudrequest.DPoPTokenProvider {
	return cloudsession.TokenProvider(auth, ensureOAuthAccessToken)
}
