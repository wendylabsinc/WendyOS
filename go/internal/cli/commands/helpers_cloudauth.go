package commands

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/proto/gen/cloudpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// cloudCertError wraps the structured CertificateError that the cloud returns in
// a *successful* RPC response body. IssueCertificate and RefreshCertificate
// report failures this way (an error field on the response) rather than as a
// gRPC status, so the machine-readable code would otherwise be lost when the
// caller only inspects the certificate field. Carrying the code lets callers
// distinguish an expired/absent session (unauthorized) from other failures.
type cloudCertError struct {
	code    cloudpb.CertificateErrorCode
	message string
}

func (e cloudCertError) Error() string {
	if e.message != "" {
		return e.message
	}
	return e.code.String()
}

// isUnauthenticatedCloudError reports whether err means the caller's stored
// identity is no longer usable and logging in again is the remedy. It matches
// the gRPC Unauthenticated status code (transport-level rejection), the
// structured CertificateError unauthorized code carried in a response body, and
// a pki-core renewal refusal that only a freshly minted certificate resolves.
func isUnauthenticatedCloudError(err error) bool {
	if err == nil {
		return false
	}
	var ce cloudCertError
	if errors.As(err, &ce) {
		return ce.code == cloudpb.CertificateErrorCode_CERTIFICATE_ERROR_UNAUTHORIZED
	}
	// pki-core reports the same situation as a renewal refusal: the lineage is
	// spent, gone, or was never its own, and only a fresh mint replaces it.
	var ru renewUnavailableError
	if errors.As(err, &ru) {
		return ru.needsFreshCert
	}
	// Unwrap toward a gRPC status; status.FromError does not unwrap %w chains on
	// every grpc-go version, so walk the chain ourselves.
	for e := err; e != nil; e = errors.Unwrap(e) {
		if se, ok := e.(interface{ GRPCStatus() *status.Status }); ok {
			return se.GRPCStatus().Code() == codes.Unauthenticated
		}
	}
	return false
}

// loginTargetsForAuth returns the (dashboard, gRPC) endpoints to re-authenticate
// against, preferring the failed auth entry's own endpoints and falling back to
// the built-in defaults. The dashboard is normalized to include a scheme so the
// browser-login flow can open it.
func loginTargetsForAuth(auth *config.AuthConfig) (dashboard, grpc string) {
	dashboard = defaultCloudDashboard
	grpc = defaultCloudGRPC
	if auth != nil {
		if auth.CloudDashboard != "" {
			dashboard = auth.CloudDashboard
		}
		if auth.CloudGRPC != "" {
			grpc = auth.CloudGRPC
		}
	}
	if !strings.HasPrefix(dashboard, "http://") && !strings.HasPrefix(dashboard, "https://") {
		dashboard = "https://" + dashboard
	}
	return dashboard, grpc
}

// reloadAuthEntry re-reads the stored auth entry matching the dashboard, gRPC
// endpoint, and organization after re-login, so a retry uses the
// fresh credentials rather than the stale pointer the caller still holds.
// Returns nil when nothing matches.
func reloadAuthEntry(prev *config.AuthConfig) *config.AuthConfig {
	if prev == nil {
		return nil
	}
	cfg, err := config.Load()
	if err != nil {
		return nil
	}
	for i := range cfg.Auth {
		if cfg.Auth[i].CloudDashboard == prev.CloudDashboard && cfg.Auth[i].CloudGRPC == prev.CloudGRPC && cloudAuthOrgID(&cfg.Auth[i]) == cloudAuthOrgID(prev) {
			return &cfg.Auth[i]
		}
	}
	return nil
}

// performLoginFn and performOIDCLoginFn are the login entry points, indirected
// so tests can stub them.
var (
	performLoginFn     = performLogin
	performOIDCLoginFn = performOIDCLogin
)

// relogin signs in again to the cloud the stored session targets: a
// service-account session re-mints its token, an OAuth session goes back
// through its wendy-auth realm, and anything else (no session, or a legacy
// one) uses the legacy dashboard flow.
func relogin(ctx context.Context, auth *config.AuthConfig) error {
	switch {
	case auth != nil && auth.ServiceAccount != "":
		unlock, err := acquireAuthRefreshLock(ctx)
		if err != nil {
			return err
		}
		defer unlock()
		local := *auth
		if err := reloadOAuthSession(&local); err != nil {
			return err
		}
		return remintServiceAccountSession(ctx, &local)
	case auth != nil && auth.OAuthIssuer != "":
		return performOIDCLoginFn(ctx, oidcReloginOptions(auth))
	}
	dashboard, grpc := loginTargetsForAuth(auth)
	return performLoginFn(ctx, dashboard, grpc)
}

// oidcReloginOptions keeps the stored session's realm and deployment settings
// for a fresh OIDC login. Sessions from before the PKI fields were stored get
// the defaults the login command would apply.
func oidcReloginOptions(auth *config.AuthConfig) oidcLoginOptions {
	opts := oidcLoginOptions{
		Issuer:           auth.OAuthIssuer,
		ClientID:         auth.OAuthClientID,
		CloudResource:    auth.OAuthResource,
		IdentityResource: auth.PKIResource,
		IdentityEndpoint: auth.PKIEndpoint,
		CloudURL:         auth.CloudDashboard,
		CloudGRPC:        auth.CloudGRPC,
	}
	if opts.IdentityResource == "" {
		opts.IdentityResource = defaultPKIIdentityResource
	}
	if opts.IdentityEndpoint == "" {
		opts.IdentityEndpoint = defaultDevPKIIdentityEndpoint
	}
	return opts
}

// offerReloginOnUnauthenticated detects an unauthenticated cloud error (an
// expired or missing session) and, in an interactive terminal, tells the user
// their session expired and offers to run the login flow. It returns true only
// when the user accepted and a fresh login succeeded — in which case the caller
// may proceed or retry with the new credentials. Non-interactive or JSON runs
// never prompt (they surface the original error to the caller unchanged).
func offerReloginOnUnauthenticated(ctx context.Context, auth *config.AuthConfig, err error) bool {
	if !isUnauthenticatedCloudError(err) {
		return false
	}
	if jsonOutput || !isInteractiveTerminal() {
		return false
	}
	fmt.Fprintln(os.Stderr, tui.WarningMessage("Your session has expired. You need to log in again."))
	if !confirmFn("Log in again now?") {
		return false
	}
	if lerr := relogin(ctx, auth); lerr != nil {
		fmt.Fprintln(os.Stderr, tui.ErrorMessage(fmt.Sprintf("Login failed: %v", lerr)))
		return false
	}
	return true
}
