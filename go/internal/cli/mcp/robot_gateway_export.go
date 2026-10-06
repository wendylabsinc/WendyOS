package mcp

import (
	"context"
	"fmt"
	"slices"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
)

// InspectExport reads a tool descriptor for operator review. It does not call
// the app tool, update the gateway policy, or grant additional permissions.
func (g *RobotGateway) InspectExport(ctx context.Context, robotID, app, tool, name string) (GatewayExport, error) {
	if g.cfg.LocalSubject == "" {
		return GatewayExport{}, fmt.Errorf("export inspection requires local_subject")
	}
	ctx = context.WithValue(ctx, gatewayPrincipalKey{}, gatewayPrincipal{g.cfg.LocalSubject, robotGatewayScopes})
	r, err := g.authorize(ctx, robotID, RobotToolsScope)
	if err != nil {
		return GatewayExport{}, err
	}
	if !slices.Contains(r.Apps, app) || !gatewayIdentifier.MatchString(name) {
		return GatewayExport{}, fmt.Errorf("app must be permitted and export name must be a valid identifier")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	conn, err := g.connect(ctx, r.Device)
	if err != nil {
		return GatewayExport{}, fmt.Errorf("could not connect to robot: %w", err)
	}
	defer conn.Close()
	client, err := g.connectApp(ctx, ctx, conn, app)
	if err != nil {
		return GatewayExport{}, fmt.Errorf("could not connect to app MCP server: %w", err)
	}
	defer client.Close()
	var req mcpgo.ListToolsRequest
	for page := 0; page < 10; page++ {
		result, err := client.ListTools(ctx, req)
		if err != nil {
			return GatewayExport{}, err
		}
		for _, t := range result.Tools {
			if t.Name == tool {
				return GatewayExport{Name: name, App: app, Tool: t}, nil
			}
		}
		if result.NextCursor == "" {
			break
		}
		req.Params.Cursor = result.NextCursor
	}
	return GatewayExport{}, fmt.Errorf("app did not advertise the requested tool")
}
