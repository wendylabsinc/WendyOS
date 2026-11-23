package main

import (
	"context"
	"errors"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/cloudrequest"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func auxiliary(ctx context.Context, c grpc.ClientConnInterface, a *config.AuthConfig, o options) (any, error) {
	switch o.operation {
	case "unbound-create":
		// A declared compute asset is ordinary catalog data, not an enrolled or
		// impersonated device. It has never had an encryption key or connection.
		asset := new(pb.Asset)
		err := cloudrequest.Invoke(ctx, c, a, pb.AssetService_CreateAsset_FullMethodName, &pb.CreateAssetRequest{OrganizationId: o.tenant, Name: o.app + "-unbound", Details: o.app, IsComputeDevice: true, DeviceType: proto.String("raspberry-pi-5"), Architecture: proto.String("arm64")}, asset)
		if err != nil {
			return nil, err
		}
		return map[string]any{"id": asset.Id}, nil
	case "unbound-delete":
		asset, err := pb.NewAssetServiceClient(c).GetAsset(ctx, &pb.GetAssetRequest{Id: o.asset, OrganizationId: proto.String(o.tenant)})
		if status.Code(err) == codes.NotFound {
			return map[string]any{"deleted": true}, nil
		}
		if err != nil {
			return nil, err
		}
		if asset.OrganizationId != o.tenant || asset.Details != o.app || asset.GetPkiDeviceName() != "" {
			return nil, errors.New("only this test's never-enrolled declaration may be deleted")
		}
		err = cloudrequest.Invoke(ctx, c, a, pb.AssetService_DeleteAsset_FullMethodName, &pb.DeleteAssetRequest{Id: o.asset}, new(pb.DeleteAssetResponse))
		return map[string]any{"deleted": err == nil}, err
	case "partial-delete":
		rows, err := pages(func(token string) ([]*pb.PendingDeviceStatement, string, error) {
			r, e := pb.NewDeploymentServiceClient(c).ListPendingDeviceStatements(ctx, &pb.ListPendingDeviceStatementsRequest{AppId: o.app, PageToken: token, PageSize: 2})
			return r.GetStatements(), r.GetNextPageToken(), e
		})
		if err != nil {
			return nil, err
		}
		if len(rows) < 2 {
			return nil, errors.New("partial delete requires at least two holding devices")
		}
		device := "spiffe://wendy.sh/tenant/" + o.tenant + "/device/" + o.asset
		var row *pb.PendingDeviceStatement
		for _, r := range rows {
			if r.Device == device {
				row = r
			}
		}
		if row == nil {
			return nil, errors.New("selected owned device is not holding the app")
		}
		statement, err := cloudrequest.Sign(ctx, a, pb.DeploymentService_UnassignDevice_FullMethodName, &pb.UnassignDeviceRequest{OrganizationId: o.tenant, DeploymentId: row.DeploymentId, Device: device, Revision: 2147483647, ValidUntil: timestamppb.New(time.Now().Add(cloudrequest.MaxValidity))})
		if err != nil {
			return nil, err
		}
		err = cloudrequest.Invoke(ctx, c, a, pb.AppService_DeleteApp_FullMethodName, &pb.DeleteAppRequest{Id: o.app, OrganizationId: o.tenant, DeviceRemovals: []*pb.SignedRequest{statement}}, new(pb.DeleteAppResponse))
		// Return the actual outcome; Swift requires FailedPrecondition and observes
		// the stored partial deletion before resuming through the ordinary CLI.
		if status.Code(err) != codes.FailedPrecondition {
			return nil, err
		}
		return map[string]any{"code": status.Code(err).String(), "removedDevice": o.asset}, nil
	}
	return nil, errors.New("unknown auxiliary operation")
}
