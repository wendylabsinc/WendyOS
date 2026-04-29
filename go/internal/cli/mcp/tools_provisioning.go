package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/wendylabsinc/wendy/proto/gen/agentpb"
)

func (s *mcpServer) registerProvisioningTools(srv *server.MCPServer) {
	srv.AddTool(mcpgo.NewTool("provisioning_status",
		mcpgo.WithDescription("Check whether the connected device is provisioned with Wendy Cloud"),
	), s.handleProvisioningStatus)

	srv.AddTool(mcpgo.NewTool("provisioning_start",
		mcpgo.WithDescription("Provision the device with Wendy Cloud and wait for completion (up to 2 minutes)"),
		mcpgo.WithString("enrollment_token",
			mcpgo.Required(),
			mcpgo.Description("Enrollment token obtained from Wendy Cloud"),
		),
		mcpgo.WithString("cloud_host",
			mcpgo.Required(),
			mcpgo.Description("Wendy Cloud hostname, e.g. cloud.wendy.sh"),
		),
		mcpgo.WithNumber("organization_id",
			mcpgo.Required(),
			mcpgo.Description("Organization ID from Wendy Cloud"),
		),
		mcpgo.WithNumber("asset_id",
			mcpgo.Description("Asset ID to assign to this device (optional)"),
		),
	), s.handleProvisioningStart)
}

func (s *mcpServer) handleProvisioningStatus(ctx context.Context, _ mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	conn := s.GetConn()
	if conn == nil {
		return errNotConnected(), nil
	}
	resp, err := conn.ProvisioningService.IsProvisioned(ctx, &agentpb.IsProvisionedRequest{})
	if err != nil {
		return mcpgo.NewToolResultError(grpcErrString(err)), nil
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
	b, _ := json.MarshalIndent(result, "", "  ")
	return mcpgo.NewToolResultText(string(b)), nil
}

func (s *mcpServer) handleProvisioningStart(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	conn := s.GetConn()
	if conn == nil {
		return errNotConnected(), nil
	}
	token := stringParam(req, "enrollment_token")
	cloudHost := stringParam(req, "cloud_host")
	orgID := intParam(req, "organization_id", 0)
	if token == "" || cloudHost == "" || orgID == 0 {
		return mcpgo.NewToolResultError("enrollment_token, cloud_host, and organization_id are required"), nil
	}
	_, err := conn.ProvisioningService.StartProvisioning(ctx, &agentpb.StartProvisioningRequest{
		EnrollmentToken: token,
		CloudHost:       cloudHost,
		OrganizationId:  int32(orgID),
		AssetId:         int32(intParam(req, "asset_id", 0)),
	})
	if err != nil {
		return mcpgo.NewToolResultError(grpcErrString(err)), nil
	}

	// Poll until provisioned or 2-minute timeout.
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		time.Sleep(3 * time.Second)
		statusResp, err := conn.ProvisioningService.IsProvisioned(ctx, &agentpb.IsProvisionedRequest{})
		if err != nil {
			return mcpgo.NewToolResultError(grpcErrString(err)), nil
		}
		if p, ok := statusResp.GetResponse().(*agentpb.IsProvisionedResponse_Provisioned); ok {
			result := map[string]any{
				"provisioned":     true,
				"cloud_host":      p.Provisioned.GetCloudHost(),
				"organization_id": p.Provisioned.GetOrganizationId(),
				"asset_id":        p.Provisioned.GetAssetId(),
			}
			b, _ := json.MarshalIndent(result, "", "  ")
			return mcpgo.NewToolResultText(string(b)), nil
		}
	}
	return mcpgo.NewToolResultError(fmt.Sprintf("provisioning timed out after 2 minutes — check status with provisioning_status")), nil
}
