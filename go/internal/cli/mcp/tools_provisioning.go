package mcp

import (
	"context"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

func (s *mcpServer) registerProvisioningTools(srv *server.MCPServer) {
	statusOpts := []mcpgo.ToolOption{
		mcpgo.WithDescription("Check whether the connected device is provisioned with Wendy Cloud"),
	}
	statusOpts = append(statusOpts, readOnly()...)
	statusOpts = append(statusOpts, localOnly()...)
	srv.AddTool(mcpgo.NewTool("provisioning_status", statusOpts...), s.handleProvisioningStatus)

	startOpts := []mcpgo.ToolOption{
		mcpgo.WithDescription("Provision the device with Wendy Cloud and wait for completion (up to 2 minutes)"),
		mcpgo.WithString("enrollment_token",
			mcpgo.Required(),
			mcpgo.Description("Enrollment token obtained from Wendy Cloud"),
		),
		mcpgo.WithString("cloud_host",
			mcpgo.Required(),
			mcpgo.Description("Wendy Cloud hostname, e.g. cloud.wendy.dev"),
		),
		mcpgo.WithNumber("organization_id",
			mcpgo.Required(),
			mcpgo.Description("Organization ID from Wendy Cloud"),
		),
		mcpgo.WithNumber("asset_id",
			mcpgo.Description("Asset ID to assign to this device (optional)"),
		),
	}
	startOpts = append(startOpts, mutating()...)
	startOpts = append(startOpts, openWorld()...)
	startOpts = append(startOpts, idempotent()...)
	srv.AddTool(mcpgo.NewTool("provisioning_start", startOpts...), s.handleProvisioningStart)
}

func (s *mcpServer) handleProvisioningStatus(ctx context.Context, _ mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	conn := s.GetConn()
	if conn == nil {
		return errNotConnected(), nil
	}
	resp, err := conn.ProvisioningService.IsProvisioned(ctx, &agentpb.IsProvisionedRequest{})
	if err != nil {
		return errResult(codeFromGRPC(err), grpcErrString(err)), nil
	}
	var result map[string]any
	switch r := resp.GetResponse().(type) {
	case *agentpb.IsProvisionedResponse_Provisioned:
		result = map[string]any{
			"provisioned":     true,
			"cloud_host":      r.Provisioned.GetCloudHost(),
			"organization_id": r.Provisioned.GetOrganizationId(),
			"asset_id":        r.Provisioned.GetAssetId(),
		}
	case *agentpb.IsProvisionedResponse_NotProvisioned:
		result = map[string]any{
			"provisioned": false,
		}
	default:
		result = map[string]any{"provisioned": false}
	}
	return okResult(result), nil
}

func (s *mcpServer) handleProvisioningStart(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	conn := s.GetConn()
	if conn == nil {
		return errNotConnected(), nil
	}
	token := stringParam(req, "enrollment_token")
	cloudHost := stringParam(req, "cloud_host")
	orgID := intParam(req, "organization_id", 0)
	if token == "" {
		return errResult(errCodeInvalidArgument, "enrollment_token is required"), nil
	}
	if cloudHost == "" {
		return errResult(errCodeInvalidArgument, "cloud_host is required"), nil
	}
	if orgID == 0 {
		return errResult(errCodeInvalidArgument, "organization_id is required"), nil
	}
	tok := progressToken(req)
	_, err := conn.ProvisioningService.StartProvisioning(ctx, &agentpb.StartProvisioningRequest{
		EnrollmentToken: token,
		CloudHost:       cloudHost,
		OrganizationId:  int32(orgID),
		AssetId:         int32(intParam(req, "asset_id", 0)),
	})
	if err != nil {
		return errResult(codeFromGRPC(err), grpcErrString(err)), nil
	}

	// Poll until provisioned, caller cancelled, or 2-minute timeout.
	pollCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	var tick float64
	for {
		select {
		case <-pollCtx.Done():
			return errResult(errCodeTimeout, "provisioning timed out — check status with provisioning_status"), nil
		case <-ticker.C:
			// Indeterminate progress: total unknown, but the value must advance
			// monotonically per the MCP spec, so emit the tick count.
			tick++
			reportProgress(ctx, tok, tick, 0, "waiting for device to finish provisioning…")
			statusResp, err := conn.ProvisioningService.IsProvisioned(ctx, &agentpb.IsProvisionedRequest{})
			if err != nil {
				return errResult(codeFromGRPC(err), grpcErrString(err)), nil
			}
			if p, ok := statusResp.GetResponse().(*agentpb.IsProvisionedResponse_Provisioned); ok {
				result := map[string]any{
					"provisioned":     true,
					"cloud_host":      p.Provisioned.GetCloudHost(),
					"organization_id": p.Provisioned.GetOrganizationId(),
					"asset_id":        p.Provisioned.GetAssetId(),
				}
				return okResult(result), nil
			}
		}
	}
}
