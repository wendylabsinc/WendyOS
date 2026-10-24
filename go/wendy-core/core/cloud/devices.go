package cloud

import (
	"context"
	"fmt"
	"io"

	pb "github.com/wendylabsinc/WendyOS/go/wendy-core/core/cloud/internal/cloudpb"
	"github.com/wendylabsinc/WendyOS/go/wendy-core/platform"
	"google.golang.org/protobuf/proto"
)

const devicePageSize = 200
const maxDevices = 10_000

// DeviceLimitError refuses a truncated inventory rather than returning success.
// The limit and diagnostic retain the existing CLI/MCP behavior.
type DeviceLimitError struct{}

func (*DeviceLimitError) Error() string {
	return fmt.Sprintf("cloud returned more than %d devices", maxDevices)
}

// ListDevices returns the complete bounded Cloud v2 compute-device inventory.
// The filter and online-only flag are applied to every request. The host supplies
// authentication on each call. On failure no partial inventory is returned.
//
// Cloud currently exposes only offset/limit; those parameters remain private to
// this adapter. Concurrent inserts/removals may skip or repeat a device. Stable
// pagination requires a Cloud API change; this function makes no snapshot claim.
func (c *Client) ListDevices(ctx context.Context, tenant, filter string, onlineOnly bool) ([]*Asset, error) {
	if c.Transport == nil {
		return nil, fmt.Errorf("host Cloud transport is required")
	}
	assets := make([]*Asset, 0)
	for offset := int32(0); ; {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		request := &pb.ListAssetsRequest{OrganizationId: tenant, IsComputeDevice: proto.Bool(true), Offset: proto.Int32(offset), Limit: proto.Int32(devicePageSize)}
		if filter != "" {
			request.Filter = proto.String(filter)
		}
		if onlineOnly {
			request.OnlineOnly = proto.Bool(true)
		}
		wire, err := proto.Marshal(request)
		if err != nil {
			return nil, err
		}
		stream, err := c.Transport.ServerStream(ctx, platform.RPCRequest{Method: "/wendycloud.v2.AssetService/ListAssets", Message: wire})
		if err != nil {
			return nil, fmt.Errorf("listing devices: %w", err)
		}
		page, total, err := receiveDevices(stream, &assets)
		if err != nil {
			return nil, err
		}
		offset += page
		if page == 0 || offset >= total {
			return assets, nil
		}
	}
}

func receiveDevices(stream platform.ServerStream, assets *[]*Asset) (page, total int32, err error) {
	defer stream.Close()
	for {
		data, err := stream.Recv()
		if err == io.EOF {
			return page, total, nil
		}
		if err != nil {
			return 0, 0, fmt.Errorf("listing devices: %w", err)
		}
		var response pb.ListAssetsResponse
		if err := proto.Unmarshal(data, &response); err != nil {
			return 0, 0, fmt.Errorf("listing devices: %w", err)
		}
		if len(*assets) >= maxDevices {
			return 0, 0, &DeviceLimitError{}
		}
		*assets = append(*assets, response.GetAsset())
		page++
		total = response.GetTotal()
	}
}
