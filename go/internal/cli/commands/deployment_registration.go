package commands

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	cloudpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type deploymentEnrollment struct {
	cloudHost      string
	organizationID string
}

type deploymentEnrollmentLookup func(context.Context) (*deploymentEnrollment, error)
type deploymentAppRegistrar func(context.Context, *config.AuthConfig, *deploymentEnrollment, []string) error

func registerCloudApps(ctx context.Context, conn *grpcclient.AgentConnection, appIDs []string, skip bool) error {
	if skip {
		return nil
	}
	if conn == nil || conn.Conn == nil {
		return fmt.Errorf("registering deployment with Cloud: device connection is unavailable; use --skip-cloud-registration only for offline deployments")
	}
	lookup := func(ctx context.Context) (*deploymentEnrollment, error) {
		response, err := agentpbv2.NewWendyProvisioningServiceClient(conn.Conn).IsProvisioned(ctx, &agentpbv2.IsProvisionedRequest{})
		if status.Code(err) == codes.Unimplemented {
			legacy, legacyErr := conn.ProvisioningService.IsProvisioned(ctx, &agentpb.IsProvisionedRequest{})
			if legacyErr != nil {
				return nil, legacyErr
			}
			if legacy.GetNotProvisioned() != nil {
				return nil, nil
			}
			if legacy.GetProvisioned() == nil {
				return nil, status.Error(codes.Internal, "agent returned an invalid provisioning response")
			}
			// Legacy Cloud does not expose the v2 app catalog contract. Keep the
			// existing deployment path unchanged; this registration is for Cloud v2.
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		if response.GetNotProvisioned() != nil {
			return nil, nil
		}
		provisioned := response.GetProvisioned()
		if provisioned == nil {
			return nil, status.Error(codes.Internal, "agent returned an invalid provisioning response")
		}
		if principal := provisioned.GetPrincipalUri(); principal != "" {
			identity, parseErr := certs.ParsePrincipal(principal)
			if parseErr != nil || identity.EntityType != certs.EntityAsset {
				return nil, status.Error(codes.FailedPrecondition, "agent reported an invalid device principal")
			}
			return &deploymentEnrollment{
				cloudHost:      provisioned.GetCloudHost(),
				organizationID: identity.TenantUUID,
			}, nil
		}
		return nil, nil
	}
	if err := registerDeviceApps(ctx, lookup, appIDs, config.Load, registerAppsWithCloud); err != nil {
		return fmt.Errorf("registering deployment with Cloud: %w; use --skip-cloud-registration only for offline deployments", err)
	}
	return nil
}

func registerDeviceApps(
	ctx context.Context,
	lookup deploymentEnrollmentLookup,
	appIDs []string,
	loadConfig func() (*config.Config, error),
	register deploymentAppRegistrar,
) error {
	device, err := lookup(ctx)
	if err != nil {
		return fmt.Errorf("reading device enrollment: %w", err)
	}
	if device == nil {
		return nil
	}
	cfg, err := loadConfig()
	if err != nil {
		return fmt.Errorf("loading Cloud credentials: %w", err)
	}
	auth, err := deploymentAuth(cfg, device)
	if err != nil {
		return err
	}
	return register(ctx, auth, device, uniqueAppIDs(appIDs))
}

func deploymentAuth(cfg *config.Config, device *deploymentEnrollment) (*config.AuthConfig, error) {
	if device.cloudHost == "" || device.organizationID == "" {
		return nil, fmt.Errorf("device enrollment is incomplete")
	}
	for i := range cfg.Auth {
		candidate := &cfg.Auth[i]
		if candidate.CloudGRPC != device.cloudHost {
			continue
		}
		for _, certificate := range candidate.Certificates {
			selected := *candidate
			selected.Certificates = []config.CertificateInfo{certificate}
			if selected.OrganizationKey() != device.organizationID {
				continue
			}
			if certificate.AssetID != 0 {
				continue
			}
			if principal := certificate.PrincipalURI; principal != "" {
				identity, err := certs.ParsePrincipal(principal)
				if err != nil || identity.EntityType == certs.EntityAsset {
					continue
				}
			}
			return &selected, nil
		}
	}
	return nil, fmt.Errorf("no Cloud operator credentials match organization %s on %s", device.organizationID, device.cloudHost)
}

func uniqueAppIDs(appIDs []string) []string {
	unique := make(map[string]struct{}, len(appIDs))
	for _, id := range appIDs {
		if id != "" {
			unique[id] = struct{}{}
		}
	}
	result := make([]string, 0, len(unique))
	for id := range unique {
		result = append(result, id)
	}
	sort.Strings(result)
	return result
}

func registerAppsWithCloud(ctx context.Context, auth *config.AuthConfig, device *deploymentEnrollment, appIDs []string) error {
	connection, err := dialCloudGRPC(auth)
	if err != nil {
		return err
	}
	defer connection.Close()
	cloudCtx, err := cloudContext(ctx, auth)
	if err != nil {
		return err
	}
	client := cloudpbv2.NewAppServiceClient(connection)
	for _, id := range appIDs {
		_, err := client.GetApp(cloudCtx, &cloudpbv2.GetAppRequest{Id: id, OrganizationId: device.organizationID})
		if err == nil {
			continue
		}
		if status.Code(err) != codes.NotFound {
			return err
		}
		name := deploymentAppName(id)
		if _, err := client.UpsertApp(cloudCtx, &cloudpbv2.UpsertAppRequest{Id: id, OrganizationId: device.organizationID, Name: &name}); err != nil {
			return err
		}
	}
	return nil
}

func deploymentAppName(id string) string {
	if name, ok := strings.CutPrefix(id, "campaign:"); ok && name != "" {
		return name
	}
	return id
}
