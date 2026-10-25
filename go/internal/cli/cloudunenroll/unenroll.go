// Package cloudunenroll adapts Cloud cleanup to shared WendyGo enrollment.
package cloudunenroll

import (
	"context"
	"fmt"

	"github.com/wendylabsinc/wendy-go/enrollment"
	"github.com/wendylabsinc/wendy/go/internal/cli/cloudrequest"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	cloudpb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
)

// Result retains acknowledged partial removal progress.
type Result = enrollment.Result

// DeviceID projects the asset's public certificate binding into Core.
func DeviceID(asset *cloudpb.Asset) string {
	if asset == nil {
		return enrollment.DeviceID(nil)
	}
	return enrollment.DeviceID(&enrollment.Asset{Id: asset.Id, PkiDeviceName: asset.PkiDeviceName})
}

// Remove delegates credential inventories, revocation order and binding checks.
func Remove(ctx context.Context, conn grpc.ClientConnInterface, auth *config.AuthConfig, asset *cloudpb.Asset) (Result, error) {
	if auth == nil || len(auth.Certificates) == 0 || asset == nil {
		return Result{}, fmt.Errorf("an operator session and cloud asset are required")
	}
	wire, err := proto.Marshal(asset)
	if err != nil {
		return Result{}, err
	}
	shared := new(enrollment.Asset)
	if err := proto.Unmarshal(wire, shared); err != nil {
		return Result{}, err
	}
	client := cloudrequest.EnrollmentClient(ctx, conn, auth)
	return client.Remove(ctx, cloudrequest.EnrollmentSession(auth), shared)
}
