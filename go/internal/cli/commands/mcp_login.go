package commands

import (
	"context"

	wendymcp "github.com/wendylabsinc/wendy/go/internal/cli/mcp"
)

// The dashboard sign-in is what auth_login runs.
var _ wendymcp.LoginSession = (*legacyLoginSession)(nil)

// mcpLoginStarter starts the default Wendy Cloud sign-in for the MCP auth_login
// tool. beginLegacyLogin writes nothing to stdout, which carries the protocol.
// It returns a nil interface (not a typed nil) on error.
func mcpLoginStarter(ctx context.Context) (wendymcp.LoginSession, error) {
	session, err := beginLegacyLogin(ctx, defaultCloudDashboard, defaultCloudGRPC)
	if err != nil {
		return nil, err
	}
	return session, nil
}
