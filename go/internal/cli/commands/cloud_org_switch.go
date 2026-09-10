package commands

import (
	"context"
	"errors"
	"fmt"

	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	cloudpb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb"
)

type accessibleCloudOrg struct {
	org    *cloudpb.Organization
	source *config.AuthConfig
}

// loadCloudOrgConfig is a seam for the post-login reload in tests.
var loadCloudOrgConfig = config.Load

// accessibleCloudOrganizations aggregates every organization visible through
// the locally stored sessions. The source session is retained so an org that
// has no local certificate yet can launch login against the same cloud
// environment that reported the membership.
func accessibleCloudOrganizations(ctx context.Context, cfg *config.Config) ([]accessibleCloudOrg, error) {
	if cfg == nil || len(cfg.Auth) == 0 {
		return nil, config.ErrNotLoggedIn
	}

	seen := make(map[int32]bool)
	var (
		result  []accessibleCloudOrg
		lastErr error
	)
	for i := range cfg.Auth {
		auth := &cfg.Auth[i]
		if len(auth.Certificates) == 0 {
			continue
		}
		orgs, err := listOrgsFromCloud(ctx, auth)
		if err != nil {
			lastErr = err
			continue
		}
		for _, org := range orgs {
			if org == nil || seen[org.GetId()] {
				continue
			}
			seen[org.GetId()] = true
			result = append(result, accessibleCloudOrg{org: org, source: auth})
		}
	}
	if len(result) == 0 && lastErr != nil {
		return nil, fmt.Errorf("listing organizations: %w", lastErr)
	}
	return result, nil
}

func cloudAuthForOrg(cfg *config.Config, orgID int32) *config.AuthConfig {
	if cfg == nil {
		return nil
	}
	for i := range cfg.Auth {
		auth := &cfg.Auth[i]
		if cloudAuthOrgID(auth) == orgID {
			return auth
		}
	}
	return nil
}

// switchCloudOrganization shows the full membership roster, including orgs
// for which this machine does not hold credentials. Selecting an authenticated
// org switches immediately. Selecting an unauthenticated org starts login and
// requires that the chosen org's certificate be present afterwards.
func switchCloudOrganization(ctx context.Context, cfg *config.Config) (*config.AuthConfig, *config.Config, error) {
	if auth := devicePickerInitialAuth(cfg); auth != nil && len(auth.Certificates) > 0 && auth.Certificates[0].TenantUUID() != "" {
		return switchCloudOrganizationV2(ctx, cfg, auth)
	}
	accessible, err := accessibleCloudOrganizations(ctx, cfg)
	if err != nil {
		return nil, cfg, err
	}
	if len(accessible) == 0 {
		return nil, cfg, fmt.Errorf("your account belongs to no organizations; contact your administrator")
	}

	orgs := make([]*cloudpb.Organization, 0, len(accessible))
	byID := make(map[int32]accessibleCloudOrg, len(accessible))
	for _, entry := range accessible {
		orgs = append(orgs, entry.org)
		byID[entry.org.GetId()] = entry
	}

	orgID, orgName, err := pickOrgInteractiveFn(orgs, cfg, false)
	if err != nil {
		return nil, cfg, err
	}
	if auth := cloudAuthForOrg(cfg, orgID); auth != nil {
		fresh, loadErr := loadCloudOrgConfig()
		if loadErr == nil {
			cfg = fresh // captures a default changed with 'd' inside the picker
			if freshAuth := cloudAuthForOrg(cfg, orgID); freshAuth != nil {
				return freshAuth, cfg, nil
			}
		} else {
			return auth, cfg, nil
		}
	}

	entry, ok := byID[orgID]
	if !ok || entry.source == nil {
		return nil, cfg, fmt.Errorf("selected organization %d is no longer available", orgID)
	}
	fmt.Println(tui.InfoMessage(fmt.Sprintf("No credentials are stored for %s (org %d). Complete login and select that organization in the browser.", orgName, orgID)))
	dashboard, grpcEndpoint := loginTargetsForAuth(entry.source)
	if err := performLoginFn(ctx, dashboard, grpcEndpoint); err != nil {
		return nil, cfg, err
	}

	fresh, err := loadCloudOrgConfig()
	if err != nil {
		return nil, cfg, fmt.Errorf("loading config after login: %w", err)
	}
	auth := cloudAuthForOrg(fresh, orgID)
	if auth == nil {
		return nil, fresh, errors.New("login completed, but credentials for the selected organization were not stored; press 'o' and select it again, then choose the same organization in the browser")
	}
	return auth, fresh, nil
}
