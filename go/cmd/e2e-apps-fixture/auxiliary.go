package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
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

type declaredAssetReceipt struct {
	ID   string `json:"id"`
	App  string `json:"app"`
	Name string `json:"name"`
}

func auxiliary(ctx context.Context, c grpc.ClientConnInterface, a *config.AuthConfig, o options) (any, error) {
	switch o.operation {
	case "unbound-create":
		// A declared compute asset is ordinary catalog data, not an enrolled or
		// impersonated device. It has never had an encryption key or connection.
		asset := new(pb.Asset)
		name := strings.ReplaceAll(o.app, ".", "-") + "-unbound"
		err := cloudrequest.Invoke(ctx, c, a, pb.AssetService_CreateAsset_FullMethodName, &pb.CreateAssetRequest{OrganizationId: o.tenant, Name: name, Details: o.app, IsComputeDevice: true, DeviceType: proto.String("raspberry-pi-5"), Architecture: proto.String("arm64")}, asset)
		if err != nil {
			return nil, atFixtureStage("unbound_create", err)
		}
		// A public create receipt, never identity/key material. Cleanup must be
		// bound to the actual returned ID as well as its unique app/name marker.
		receipt, err := json.Marshal(declaredAssetReceipt{ID: asset.Id, App: o.app, Name: name})
		if err == nil {
			err = os.WriteFile("unbound-asset.json", receipt, 0600)
		}
		if err != nil {
			return nil, atFixtureStage("unbound_receipt", err)
		}
		return map[string]any{"id": asset.Id}, nil
	case "unbound-delete", "unbound-state":
		data, err := os.ReadFile("unbound-asset.json")
		var receipt declaredAssetReceipt
		if err != nil || json.Unmarshal(data, &receipt) != nil || receipt.ID != o.asset || receipt.App != o.app || receipt.Name != strings.ReplaceAll(o.app, ".", "-")+"-unbound" {
			return nil, atFixtureStage("unbound_receipt", errors.New("exact public create receipt required"))
		}
		asset, err := pb.NewAssetServiceClient(c).GetAsset(ctx, &pb.GetAssetRequest{Id: o.asset})
		if status.Code(err) == codes.NotFound {
			if o.operation == "unbound-state" {
				return map[string]any{"exists": false}, nil
			}
			return map[string]any{"deleted": true}, nil
		}
		if err != nil {
			return nil, atFixtureStage("unbound_get_asset", err)
		}
		expectedName := strings.ReplaceAll(o.app, ".", "-") + "-unbound"
		if asset.OrganizationId != o.tenant || asset.Details != o.app || asset.Name != expectedName || !asset.IsComputeDevice {
			return nil, atFixtureStage("unbound_ownership", errors.New("only this test's never-enrolled declaration may be deleted"))
		}
		if o.operation == "unbound-state" {
			return map[string]any{"exists": true, "hasDeviceName": asset.GetPkiDeviceName() != ""}, nil
		}
		// Dev can reserve the asset UUID as a device name at catalog creation.
		// That is not a credential or enrollment. Retain the exact CAS binding,
		// and refuse cleanup if a heartbeat appeared or its identity changed.
		binding := asset.GetPkiDeviceName()
		if asset.LastHeartbeat != nil || binding != "" && binding != asset.Id {
			return nil, atFixtureStage("unbound_not_enrolled", errors.New("owned declaration became active or its reserved identity changed"))
		}
		request := &pb.DeleteAssetRequest{Id: o.asset}
		if binding != "" {
			request.ExpectedDeviceId = proto.String(binding)
		}
		err = cloudrequest.Invoke(ctx, c, a, pb.AssetService_DeleteAsset_FullMethodName, request, new(pb.DeleteAssetResponse))
		if err != nil {
			return nil, atFixtureStage("unbound_delete", err)
		}
		return map[string]any{"deleted": true}, nil
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
