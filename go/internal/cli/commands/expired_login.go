package commands

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

// expiredLogin is the stored login whose operator certificate a device needs,
// found expired by this computer's clock when that device refused the
// handshake.
//
// A device enrolled in a pki-core tenant only accepts that tenant's operator
// certificates. Its certificate names the tenant, so the CLI can tell which
// stored login it needed even when the dial ladder presented another one: the
// ladder skips expired logins whenever some other login is still valid, and on
// 2026-10-28 mesh-1 was offered a valid certificate for a different tenant
// while its own tenant's login had expired. The rejection was then reported as
// a possible device clock error and answered with time proofs.
type expiredLogin struct {
	tenant   string
	context  string // auth context holding the login; "" when it is not stored
	label    string // organization name, or the tenant when no name is cached
	notAfter time.Time
	// renewable is an OAuth session: its refresh token mints a new certificate
	// without a browser, as 'wendy auth refresh-certs' does.
	renewable bool
	// loginStep is the one next step when renewal is impossible or fails.
	loginStep string
}

func (l *expiredLogin) message() string {
	who := l.label
	if l.context != "" {
		who = fmt.Sprintf("%s (auth context %q)", l.label, l.context)
	}
	return fmt.Sprintf("Your Wendy login for %s expired at %s.", who, l.notAfter.UTC().Format("2006-01-02 15:04 UTC"))
}

// expiredLoginFrom returns the expired login a handshake rejection carries.
func expiredLoginFrom(err error) *expiredLogin {
	var rejection tlsHandshakeRejectedError
	if errors.As(err, &rejection) {
		return rejection.expiredLogin
	}
	return nil
}

// pinnedRefusalError returns failure, or the expired login that explains it.
// A pinned device that rejected our certificate at some candidate address
// ends in the pinned refusal, which says nothing about why. If every stored
// login for the pinned tenant has expired, that is why: recovery must renew it
// rather than treat the rejection as a clock error. The refusal stays in the
// chain, so the Bluetooth fallback it blocks stays blocked.
func pinnedRefusalError(failure *noAuthenticatedEndpointError, tenant string, credentials []config.CertificateInfo, now time.Time) error {
	if failure.certSeen && tenant != "" {
		if login := expiredLoginForTenant(tenant, credentials, now); login != nil {
			return tlsHandshakeRejectedError{cause: failure, expiredLogin: login}
		}
	}
	return failure
}

// expiredLoginForTenant reports the stored login for tenant when every
// certificate it holds for that tenant has expired. Credentials is the
// snapshot the dial used, expired ones included. It returns nil when there is
// no login for the tenant, when one of them is still valid (expiry is then not
// why the device refused), or when a certificate cannot be read.
func expiredLoginForTenant(tenant string, credentials []config.CertificateInfo, now time.Time) *expiredLogin {
	var latest *config.CertificateInfo
	var latestNotAfter time.Time
	for i := range credentials {
		credential := &credentials[i]
		if credential.TenantUUID() != tenant {
			continue
		}
		notAfter, ok := certificateNotAfter(*credential)
		if !ok || !now.After(notAfter) {
			return nil
		}
		if latest == nil || notAfter.After(latestNotAfter) {
			latest, latestNotAfter = credential, notAfter
		}
	}
	if latest == nil {
		return nil
	}
	login := &expiredLogin{tenant: tenant, label: "tenant " + tenant, notAfter: latestNotAfter}
	login.loginStep = "Run 'wendy auth login' and sign in to " + login.label + ", then retry."
	// The context name, organization name and login kind are for the message
	// and the renewal only; the expiry above comes from the dial's snapshot.
	if cfg, err := config.Load(); err == nil {
		if auth := authHoldingCertificate(cfg, *latest); auth != nil {
			login.describe(auth)
		}
	}
	return login
}

func (l *expiredLogin) describe(auth *config.AuthConfig) {
	l.context = auth.Name
	if name := cachedCloudOrganizationName(auth); name != "" {
		l.label = name
	}
	l.renewable = auth.OAuthIssuer != "" && auth.ServiceAccount == ""
	login := "wendy auth login"
	switch {
	case auth.ServiceAccount != "":
		login = "wendy auth login --service-account <key-file>"
	case auth.CloudGRPC == developmentCloudLoginTarget.cloudGRPC:
		login = "wendy auth login --development"
	}
	l.loginStep = fmt.Sprintf("Run '%s' and sign in to %s, then retry.", login, l.label)
}

// authHoldingCertificate finds the stored session the dial took cert from: the
// same certificate, or failing that (another process renewed it meanwhile) the
// session for the same tenant.
func authHoldingCertificate(cfg *config.Config, cert config.CertificateInfo) *config.AuthConfig {
	var sameTenant *config.AuthConfig
	tenant := cert.TenantUUID()
	for i := range cfg.Auth {
		auth := &cfg.Auth[i]
		if len(auth.Certificates) == 0 {
			continue
		}
		if auth.Certificates[0].PemCertificate == cert.PemCertificate {
			return auth
		}
		if sameTenant == nil && tenant != "" && auth.Certificates[0].TenantUUID() == tenant {
			sameTenant = auth
		}
	}
	return sameTenant
}

func certificateNotAfter(c config.CertificateInfo) (time.Time, bool) {
	leaves, _ := certs.ParseCertsFromPEM([]byte(c.PemCertificate))
	if len(leaves) == 0 {
		return time.Time{}, false
	}
	return leaves[0].NotAfter, true
}

// expiredLoginRenewalAttempted limits renewal to one attempt per invocation, as
// clockSkewSyncAttempted does for time proofs: each one spends the session's
// rotating refresh token.
var expiredLoginRenewalAttempted bool

// renewExpiredLoginFn is indirected so recovery tests do not reach the network
// or the stored credentials.
var renewExpiredLoginFn = renewExpiredLogin

// renewExpiredLogin mints a new certificate for the expired OAuth login with
// its refresh token, as prepareDefaultCloudCertificate does for a saved Cloud
// device. It renews that login only, never the current context (WDY-3864), and
// holds the auth refresh lock through persistence like 'wendy auth
// refresh-certs'.
func renewExpiredLogin(ctx context.Context, login *expiredLogin) error {
	unlock, err := acquireAuthRefreshLock(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}
	auth, ok := cfg.ContextByName(login.context)
	if !ok || len(auth.Certificates) == 0 || auth.Certificates[0].TenantUUID() != login.tenant {
		return fmt.Errorf("auth context %q no longer holds this login", login.context)
	}
	auth.InvalidateCachedSecrets()
	if notAfter, ok := certificateNotAfter(auth.Certificates[0]); ok && time.Now().Before(notAfter) {
		return nil // another process renewed it while this one waited for the lock
	}
	if auth.OAuthIssuer == "" || auth.ServiceAccount != "" {
		return errors.New("this login has no refresh token to renew it with")
	}
	err = refreshCertsForAuthFn(ctx, auth)
	// Issuance can fail after the refresh token rotates. Persist the session
	// even then, so the next attempt can still use it.
	if saveErr := persistOAuthSession(auth); err == nil {
		err = saveErr
	}
	return err
}

// renewExpiredLoginAndRetry renews the login a device refused as expired and
// retries the same endpoint once. retried reports whether that retry ran;
// without it the outcome is cause, whose message names the next step.
func renewExpiredLoginAndRetry(ctx context.Context, login *expiredLogin, cause error, retry func() (*grpcclient.AgentConnection, error)) (conn *grpcclient.AgentConnection, err error, retried bool) {
	if !login.renewable || expiredLoginRenewalAttempted {
		return nil, cause, false
	}
	expiredLoginRenewalAttempted = true
	fmt.Fprintf(os.Stderr, "%s Renewing it...\n", login.message())
	if err := renewExpiredLoginFn(ctx, login); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err(), false
		}
		fmt.Fprintf(os.Stderr, "Could not renew the login: %s\n", tui.StripControl(err.Error()))
		return nil, cause, false
	}
	conn, err = retry()
	if err != nil {
		fmt.Fprintln(os.Stderr, "Login renewed, but the connection retry failed.")
		return nil, err, true
	}
	fmt.Fprintln(os.Stderr, "Login renewed. Connected.")
	return conn, nil, true
}
