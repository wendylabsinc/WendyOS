package commands

import (
	"context"
	"errors"
	"net"
	"strings"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

var connectUnresolvedCloudNameFn = connectToCloudAgentWithAuthExpecting

// A failed Cloud lookup must not fall through to another, unpinned Cloud session.
type cloudNameResolutionError struct{ error }

func (e *cloudNameResolutionError) Unwrap() error { return e.error }

// An explicit Cloud display name or UUID need not exist in local DNS. After
// local name resolution fails, look up that same selection in Cloud. Addresses
// and typed transport selectors keep their original connection semantics.
func connectUnresolvedCloudName(ctx context.Context, device string, cause error, opts ...resolveOption) (conn *grpcclient.AgentConnection, err error, attempted bool) {
	defer func() {
		if attempted && err != nil {
			err = &cloudNameResolutionError{err}
		}
	}()
	if device == "" || strings.ContainsAny(device, ".:/[]") || net.ParseIP(device) != nil ||
		cause == nil || ctx.Err() != nil || cloudFallbackDisabled("") ||
		stopConnectionRecovery(cause) || errors.Is(cause, errProvisionedAgentUnauthorized) ||
		errors.Is(cause, errTLSHandshakeRejected) || !isResolverMiss(cause) {
		return nil, cause, false
	}
	if _, cloud := cloudDeviceConfigFromContext(ctx); cloud {
		return nil, cause, false
	}
	cfg, err := config.Load()
	if err != nil || len(cfg.Auth) == 0 {
		return nil, cause, false
	}
	var auth *config.AuthConfig
	var expected *certs.WendyIdentity
	if pin, key, pinned := lookupPin(cfg, device); pinned {
		expected = expectedIdentityForPin(pin)
		auth, err = resolvePinnedCloudAuth(ctx, cfg, key, pin)
	} else {
		auth, err = pickAuthEntry("")
	}
	if err != nil {
		return nil, err, true
	}
	conn, err = connectUnresolvedCloudNameFn(ctx, auth, cloudDefaultSelector(device, expected), "", expected)
	if err != nil {
		// Cloud's not-found, offline and duplicate-name errors name the next
		// useful action. Do not replace them with the earlier DNS failure.
		return nil, err, true
	}
	options := resolveConfig{excludeProviderKeys: make(map[string]bool)}
	for _, option := range opts {
		option(&options)
	}
	if !options.suppressUpdateCheck && !options.readOnlyMonitoring {
		conn, err = checkAndOfferUpdateFn(ctx, conn)
	}
	return conn, err, true
}

// A device identity alone omits the Cloud endpoint and may be absent on older
// pins. Select the session using the full pin before querying Cloud devices.
func resolvePinnedCloudAuth(ctx context.Context, cfg *config.Config, key string, pin config.DevicePin) (*config.AuthConfig, error) {
	filtered := *cfg
	filtered.Auth = nil
	for _, auth := range cfg.Auth {
		if cloudAuthMatchesDevicePin(&auth, pin) {
			filtered.Auth = append(filtered.Auth, auth)
		}
	}
	if len(filtered.Auth) == 0 {
		return nil, refuseIdentity("No Cloud session matches the organization and endpoint saved for device %q. Log in to its saved Cloud organization before retrying.", key)
	}
	auth, err := config.ResolveAuth(&filtered, pin.CloudGRPC, nil)
	if err != nil {
		return nil, err
	}
	if err := prepareDefaultCloudCertificate(ctx, auth); err != nil {
		return nil, err
	}
	// Certificate preparation reloads and can renew the selected session.
	if !cloudAuthMatchesDevicePin(auth, pin) {
		return nil, refuseIdentity("Cloud session no longer matches the organization and endpoint saved for device %q. Log in to its saved Cloud organization before retrying.", key)
	}
	return auth, nil
}

func cloudAuthMatchesDevicePin(auth *config.AuthConfig, pin config.DevicePin) bool {
	if auth == nil || len(auth.Certificates) == 0 || auth.CloudGRPC != pin.CloudGRPC {
		return false
	}
	if pin.Principal != "" {
		identity, err := certs.ParsePrincipal(pin.Principal)
		return err == nil && identity.TenantUUID != "" && auth.Certificates[0].TenantUUID() == identity.TenantUUID
	}
	return auth.Certificates[0].TenantUUID() == "" && cloudAuthOrgID(auth) == int32(pin.OrgID)
}
