//go:build darwin || linux || windows

package commands

import (
	"context"
	"crypto"
	"fmt"

	"google.golang.org/grpc"

	"github.com/wendylabsinc/wendy/go/internal/cli/cloudrequest"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
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
// Unbound sessions get nil and keep Bearer. Installed by withCloudRequestSigning
// so it rides every commands-package cloud dial.
func dpopDialOptions(auth *config.AuthConfig) []grpc.DialOption {
	return cloudrequest.DPoPDialOptions(auth, cliDPoPTokenProvider(auth))
}

// cliDPoPTokenProvider refreshes the OAuth access token if needed and returns it
// with its bound ML-DSA key. It snapshots the session so concurrent RPCs on one
// dialed conn do not race on shared state.
func cliDPoPTokenProvider(auth *config.AuthConfig) cloudrequest.DPoPTokenProvider {
	return func(ctx context.Context) (string, crypto.Signer, error) {
		local := *auth
		local.Certificates = append([]config.CertificateInfo(nil), auth.Certificates...)
		if err := ensureOAuthAccessToken(ctx, &local); err != nil {
			return "", nil, err
		}
		keyPEM, err := local.OAuthDPoPKey()
		if err != nil {
			return "", nil, fmt.Errorf("DPoP: loading bound key: %w", err)
		}
		key, err := certs.ParseSigningPrivateKeyPEM([]byte(keyPEM))
		if err != nil {
			return "", nil, fmt.Errorf("DPoP: parsing bound key: %w", err)
		}
		return local.APIKey, key, nil
	}
}
