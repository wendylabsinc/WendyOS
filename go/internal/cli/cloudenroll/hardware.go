package cloudenroll

import (
	"context"

	"github.com/wendylabsinc/wendy-go/enrollment"
	"github.com/wendylabsinc/wendy/go/internal/cli/cloudrequest"
	cloudpb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
)

// ReadHardware adapts Core's optional hardware report to the CLI wire type.
func ReadHardware(ctx context.Context, conn grpc.ClientConnInterface) (*cloudpb.DeviceHardware, error) {
	hardware, err := enrollment.ReadHardware(ctx, cloudrequest.EnrollmentTransport(conn))
	if err != nil {
		return nil, err
	}
	wire, err := proto.Marshal(hardware)
	if err != nil {
		return nil, err
	}
	out := new(cloudpb.DeviceHardware)
	if err := proto.Unmarshal(wire, out); err != nil {
		return nil, err
	}
	return out, nil
}
