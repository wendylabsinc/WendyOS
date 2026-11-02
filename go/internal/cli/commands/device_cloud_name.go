package commands

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

var connectCloudLookupFn = connectToCloudAgentWithAuthExpecting

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
	conn, err = connectToPinnedCloudName(ctx, cfg, device)
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

// connectToPinnedCloudName looks up a device name in Cloud. A reused name in
// another organization or after re-enrollment must not silently redirect a
// command to a different asset, so a pinned name is looked up with the session
// for its saved organization and must have its saved identity. An unpinned
// name uses the current session.
func connectToPinnedCloudName(ctx context.Context, cfg *config.Config, name string) (*grpcclient.AgentConnection, error) {
	var auth *config.AuthConfig
	var expected *certs.WendyIdentity
	var err error
	if pin, key, pinned := lookupPin(cfg, pinKeyForAddr(name)); pinned {
		expected = expectedIdentityForPin(pin)
		auth, err = resolvePinnedCloudAuth(ctx, cfg, key, pin)
	} else {
		auth, err = pickAuthEntry("")
	}
	if err != nil {
		return nil, err
	}
	return connectCloudLookupFn(ctx, auth, cloudNameSelector(name, expected), "", expected)
}

// A device identity alone omits the Cloud endpoint and may be absent on older
// pins. Select the session using the full pin before querying Cloud devices.
//
// A principal pin belongs to its tenant on any endpoint, preferring the saved
// one: LAN pins written before tenant-aware endpoint mapping can record another
// tenant's endpoint. An unparseable principal leaves neither a tenant nor an
// organization to match.
func resolvePinnedCloudAuth(ctx context.Context, cfg *config.Config, key string, pin config.DevicePin) (*config.AuthConfig, error) {
	orgID, tenant := int32(pin.OrgID), ""
	if pin.Principal != "" {
		identity, _ := certs.ParsePrincipal(pin.Principal)
		orgID, tenant = 0, identity.TenantUUID
	}
	filtered := *cfg
	filtered.Auth = nil
	endpoint := ""
	for _, auth := range cfg.Auth {
		if !cloudAuthInOrganization(&auth, pin.CloudGRPC, orgID, tenant) {
			continue
		}
		filtered.Auth = append(filtered.Auth, auth)
		if endpoint == "" || auth.CloudGRPC == pin.CloudGRPC {
			endpoint = auth.CloudGRPC
		}
	}
	if len(filtered.Auth) == 0 {
		return nil, noPinnedCloudLogin("No Cloud login matches the organization saved for device %q.", key)
	}
	auth, err := config.ResolveAuth(&filtered, endpoint, nil)
	if err != nil {
		return nil, err
	}
	if err := prepareDefaultCloudCertificate(ctx, auth); err != nil {
		return nil, err
	}
	// Certificate preparation reloads and can renew the selected session.
	if !cloudAuthInOrganization(auth, pin.CloudGRPC, orgID, tenant) {
		return nil, noPinnedCloudLogin("The Cloud login for device %q no longer matches its saved organization.", key)
	}
	return auth, nil
}

// noPinnedCloudLogin formats lead with the pin key. No device answered, so a
// missing login is an authentication failure rather than an identity refusal.
// An organization-only pin can never match a tenant login, so unpinning is the
// way forward once its organization has migrated.
func noPinnedCloudLogin(lead, key string) error {
	return commandErrorf(config.ErrNotLoggedIn, "%s Log in to that organization, or if the device moved to another organization, run: wendy device unpin %s", fmt.Sprintf(lead, key), shellQuoteArg(key))
}
