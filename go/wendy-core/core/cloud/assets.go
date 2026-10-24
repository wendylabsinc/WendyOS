package cloud

import (
	"context"
	"fmt"

	pb "github.com/wendylabsinc/WendyOS/go/wendy-core/core/cloud/internal/cloudpb"
	"github.com/wendylabsinc/WendyOS/go/wendy-core/platform"
	"google.golang.org/protobuf/proto"
)

// Asset is the Cloud v2 asset contract generated from the monorepo schema.
// It preserves UUIDs, device metadata and unknown fields across host boundaries.
type Asset = pb.Asset

// Organization is the Cloud v2 organization contract.
type Organization = pb.Organization

// GetAssetRequest preserves exact ID and tenant/device selectors, including
// their presence. An untyped NOT_FOUND is never evidence of device deletion.
type GetAssetRequest = pb.GetAssetRequest

// DeleteAssetRequest carries Cloud's compare-and-delete and reset-proof guards.
type DeleteAssetRequest = pb.DeleteAssetRequest

// DeleteAssetResponse preserves the Cloud reset authorization after deletion.
type DeleteAssetResponse = pb.DeleteAssetResponse

// Client sends Cloud API calls through a host-supplied, authenticated transport.
// A host may use a DPoP interceptor or AuthenticationMetadata to authenticate.
// Client never opens a connection or falls back to a different tenant/API.
type Client struct {
	Transport platform.Transport
}

// GetAsset retrieves the exact requested asset and preserves RPC status details.
func (c *Client) GetAsset(ctx context.Context, request *GetAssetRequest) (*Asset, error) {
	reply := new(Asset)
	if err := c.unary(ctx, "/wendycloud.v2.AssetService/GetAsset", request, reply); err != nil {
		return nil, err
	}
	return reply, nil
}

// GetOrganization retrieves one organization by its immutable tenant UUID.
func (c *Client) GetOrganization(ctx context.Context, tenant string) (*Organization, error) {
	reply := new(Organization)
	if err := c.unary(ctx, "/wendycloud.v2.OrganizationService/GetOrganization", &pb.GetOrganizationRequest{Id: tenant}, reply); err != nil {
		return nil, err
	}
	return reply, nil
}

// DeleteAsset sends an operator-signed exact-body deletion. PKI-bound requests
// include same-key principal revocation authority and Cloud owns orchestration.
func (c *Client) DeleteAsset(ctx context.Context, signer *Signer, request *DeleteAssetRequest, options InvokeOptions) (*DeleteAssetResponse, platform.RPCResponse, error) {
	reply := new(DeleteAssetResponse)
	response, err := signer.Invoke(ctx, c.Transport, "/wendycloud.v2.AssetService/DeleteAsset", request, options)
	if err != nil {
		return nil, response, err
	}
	if err := proto.Unmarshal(response.Message, reply); err != nil {
		return nil, response, err
	}
	return reply, response, nil
}

func (c *Client) unary(ctx context.Context, method string, request, reply proto.Message) error {
	if c.Transport == nil {
		return fmt.Errorf("host Cloud transport is required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	wire, err := proto.Marshal(request)
	if err != nil {
		return err
	}
	response, err := c.Transport.Unary(ctx, platform.RPCRequest{Method: method, Message: wire})
	if err != nil {
		return err
	}
	return proto.Unmarshal(response.Message, reply)
}
