package commands

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

var enrollmentOIDCLoginFn = performOIDCLogin

// Check again at the enrollment boundary: callers other than the command must
// also stop before prompting for a name or minting a one-use enrollment token.
func validateEnrollmentCertificate(auth *config.AuthConfig) error {
	if auth == nil || len(auth.Certificates) == 0 {
		return fmt.Errorf("selected auth entry has no certificates; run 'wendy auth login --email <your-email>'")
	}
	notAfter, err := leafNotAfter(auth.Certificates[0].PemCertificate)
	if err != nil {
		return fmt.Errorf("cannot read enrollment certificate; sign in again with 'wendy auth login --email <your-email>': %w", err)
	}
	if !timeNowFn().Before(notAfter) {
		return fmt.Errorf("cannot enroll with %s: %w (expired on %s); run 'wendy auth login --email <your-email>' and retry enrollment; no logout is needed", auth.CloudGRPC, errCertExpired, notAfter.UTC().Format("2006-01-02 15:04 UTC"))
	}
	return nil
}

func prepareEnrollmentAuth(ctx context.Context, auth *config.AuthConfig) (*config.AuthConfig, error) {
	// Preserve opportunistic renewal for certificates that are still valid.
	// Expired certificates cannot use renewal and need a fresh login instead.
	err := validateEnrollmentCertificate(auth)
	if err == nil {
		if rerr := ensureFreshCertificateFn(ctx, auth); rerr != nil {
			reportStaleCertificate(rerr)
		}
		err = validateEnrollmentCertificate(auth)
	}
	if err == nil {
		return auth, nil
	}
	if !errors.Is(err, errCertExpired) || auth.OAuthIssuer == "" || jsonOutput || !isInteractiveTerminal() {
		return nil, err
	}

	fmt.Fprintln(os.Stderr, tui.WarningMessage("Your certificate has expired. Sign in again to continue enrollment."))
	if !confirmFn("Sign in again now and continue enrollment?") {
		return nil, err
	}
	// Keep the selected realm and custom deployment settings. The legacy
	// dashboard login would discard the OIDC session and use a different API.
	if lerr := enrollmentOIDCLoginFn(ctx, oidcLoginOptions{
		Issuer: auth.OAuthIssuer, ClientID: auth.OAuthClientID,
		CloudResource: auth.OAuthResource, IdentityResource: auth.PKIResource,
		IdentityEndpoint: auth.PKIEndpoint,
		CloudURL:         auth.CloudDashboard, CloudGRPC: auth.CloudGRPC,
	}); lerr != nil {
		return nil, fmt.Errorf("signing in before enrollment: %w", lerr)
	}
	cfg, err := config.Load()
	if err != nil {
		return nil, fmt.Errorf("reloading enrollment session: %w", err)
	}
	for i := range cfg.Auth {
		fresh := &cfg.Auth[i]
		if authSessionKey(fresh) != authSessionKey(auth) || strings.TrimSuffix(fresh.OAuthIssuer, "/") != strings.TrimSuffix(auth.OAuthIssuer, "/") {
			continue
		}
		if err := validateEnrollmentCertificate(fresh); err != nil {
			return nil, err
		}
		return fresh, nil
	}
	return nil, fmt.Errorf("sign-in did not replace the selected enrollment session; run 'wendy device enroll' again to select a session")
}
