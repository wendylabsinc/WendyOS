package commands

import (
	"context"
	"fmt"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"golang.org/x/sync/errgroup"
)

// Metadata requests share the authenticated connection but have no ordering
// dependency. Enrollment still precedes the organization-name lookup inside
// deviceOrganization; both can overlap the version and update-status requests.
func fetchDeviceInfo(ctx context.Context, conn *grpcclient.AgentConnection) (*agentpb.GetAgentVersionResponse, *agentpb.GetOSUpdateStatusResponse, *deviceOrganizationInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, nil, err
	}
	group, queryCtx := errgroup.WithContext(ctx)
	var version *agentpb.GetAgentVersionResponse
	var update *agentpb.GetOSUpdateStatusResponse
	var organization *deviceOrganizationInfo
	group.Go(func() error {
		var cached bool
		version, cached = conn.CachedAgentVersion()
		if !cached {
			var err error
			version, err = conn.AgentService.GetAgentVersion(queryCtx, &agentpb.GetAgentVersionRequest{})
			if err != nil {
				return fmt.Errorf("getting agent version: %w", err)
			}
		}
		return nil
	})
	group.Go(func() error {
		// Only read the persisted outcome. A live updater scan belongs to
		// the explicit os update-status command. Older agents may omit it.
		statusCtx, cancel := context.WithTimeout(queryCtx, deviceInfoOSUpdateTimeout)
		defer cancel()
		if resp, err := conn.AgentService.GetOSUpdateStatus(statusCtx, &agentpb.GetOSUpdateStatusRequest{}); err == nil {
			update = resp
		}
		return nil
	})
	group.Go(func() error {
		organization = deviceOrganization(queryCtx, conn)
		return nil
	})
	if err := group.Wait(); err != nil {
		return nil, nil, nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, nil, err
	}
	return version, update, organization, nil
}
