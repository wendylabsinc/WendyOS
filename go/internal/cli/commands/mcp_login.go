package commands

import (
	"context"
	"time"

	wendymcp "github.com/wendylabsinc/wendy/go/internal/cli/mcp"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

// The wendy-auth sign-in is what auth_login runs.
var _ wendymcp.LoginSession = (*oidcLoginSession)(nil)

// mcpLoginStarter starts the production Wendy Cloud sign-in that `wendy auth
// login` runs, for the MCP auth_login tool. beginOIDCLogin writes nothing to
// stdout, which carries the protocol. It returns a nil interface (not a typed
// nil) on error.
func mcpLoginStarter(ctx context.Context) (wendymcp.LoginSession, error) {
	session, err := beginOIDCLogin(ctx, productionOIDCLoginOptions(), adoptLoginWhenCurrentUnusable)
	if err != nil {
		return nil, err
	}
	return session, nil
}

// productionOIDCLoginOptions is the sign-in `wendy auth login` starts with no
// flags: wendy-auth's universal page for the production Wendy Cloud.
func productionOIDCLoginOptions() oidcLoginOptions {
	target := productionCloudLoginTarget
	return oidcLoginOptions{
		AuthorizationBase: target.authBase,
		ClientID:          "wendy-cli",
		CloudResource:     target.cloudResource,
		IdentityResource:  defaultPKIIdentityResource,
		IdentityEndpoint:  target.identityEndpoint,
		CloudURL:          target.cloudDashboard,
		CloudGRPC:         target.cloudGRPC,
	}
}

// adoptLoginWhenCurrentUnusable makes the new session current when cloud tools
// could not use the current one: none resolves, or its certificate has
// expired. Agents call auth_login to recover from AUTH_REQUIRED, and a session
// that is not current would leave every cloud tool on the failing one. A
// usable current session stays current, as after `wendy auth login`.
func adoptLoginWhenCurrentUnusable(cfg *config.Config, added config.AuthConfig) {
	current, err := config.ResolveAuth(cfg, "", nil)
	if err == nil && len(current.Certificates) > 0 && !certExpired(current.Certificates[0], time.Now()) {
		return
	}
	for _, auth := range cfg.Auth {
		if len(auth.Certificates) > 0 && auth.Certificates[0].PemCertificate == added.Certificates[0].PemCertificate {
			cfg.CurrentContext = auth.Name
			return
		}
	}
}
