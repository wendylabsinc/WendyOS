package commands

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

// PKI issues NotBefore = now without backdating. This allowance never extends
// NotAfter and never turns a future certificate into a reason to mint again.
const certificateIssuanceSkew = time.Minute

const certificateReissueTimeout = 30 * time.Second

// Shared by preflight and connection recovery. Keep the outcome, including a
// permanent refusal, so repeated targets in one invocation cannot spend again.
// The config's auth-refresh lock also covers reload, issuance and persistence.
var automaticCertReissue struct {
	sync.Mutex
	attempted bool
	err       error
}

// reissueCurrentCertificate runs with the auth-refresh lock held. An explicit
// refresh-certs command is independent of this automatic recovery budget.
func reissueCurrentCertificate(ctx context.Context, auth *config.AuthConfig) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	current, err := config.ResolveAuth(cfg, "", nil)
	if err != nil {
		return err
	}
	if current.Name != auth.Name || !sameOAuthSession(current, auth) {
		// A saved selector can use its own still-valid certificate without
		// spending another context's sign-in or reporting a failed operation.
		notAfter, expiryErr := leafNotAfterFn(auth.Certificates[0].PemCertificate)
		if expiryErr == nil && timeNowFn().Before(notAfter) {
			return nil
		}
		return fmt.Errorf("certificate belongs to context %q; run 'wendy auth use %s', then 'wendy auth refresh-certs', then retry", auth.Name, auth.Name)
	}
	automaticCertReissue.Lock()
	defer automaticCertReissue.Unlock()
	if automaticCertReissue.attempted {
		if automaticCertReissue.err != nil {
			return automaticCertReissue.err
		}
		return errors.New("automatic certificate re-issue already attempted in this invocation; run 'wendy auth refresh-certs' explicitly")
	}
	automaticCertReissue.attempted = true
	reissueCtx, cancel := context.WithTimeout(ctx, certificateReissueTimeout)
	defer cancel()
	// Post-expiry re-issuance goes ahead of Sem agreeing the AAA §5.2 contract sentence.
	// If he objects, follow up: renew only before expiry, otherwise ask the user to sign in again.
	// https://linear.app/wendylabsinc/issue/WDY-4006
	err = refreshDefaultCloudCertificateFn(reissueCtx, auth)
	// Auth may have rotated the handle before issuance or validation failed.
	// A persistence failure must remain fatal even if the held leaf is valid.
	// SECURITY: persistOAuthSession uses config.Save's existing platform
	// secret store and atomic 0600 config replacement under the auth lock.
	if saveErr := persistOAuthSession(auth); saveErr != nil {
		err = certificatePersistenceError{errors.Join(err, saveErr)}
	}
	automaticCertReissue.err = err
	return err
}

type certificatePersistenceError struct{ error }

func (e certificatePersistenceError) Unwrap() error { return e.error }

func isOIDCSignInRefused(err error) bool {
	var refusal oidcSignInRefusedError
	return errors.As(err, &refusal)
}

type oidcSignInRefusedError struct{ error }

func (e oidcSignInRefusedError) Error() string {
	return fmt.Sprintf("Auth refused the sign-in session; run 'wendy auth login': %v", e.error)
}

func (e oidcSignInRefusedError) Unwrap() error { return e.error }
