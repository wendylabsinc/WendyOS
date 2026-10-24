package cloud

import (
	"bytes"
	"context"
	"errors"
	"testing"

	pb "github.com/wendylabsinc/WendyOS/go/wendy-core/core/cloud/internal/cloudpb"
	"github.com/wendylabsinc/WendyOS/go/wendy-core/platform"
	"github.com/wendylabsinc/WendyOS/go/wendy-core/platform/platformtest"
	"google.golang.org/protobuf/proto"
)

func TestExactAssetSelectorAndUnknownFieldsSurvive(t *testing.T) {
	wanted := &Asset{Id: assetID, OrganizationId: tenant, Name: "device", IsComputeDevice: true, DisplayName: proto.String("My device")}
	wanted.ProtoReflect().SetUnknown([]byte{0xc0, 0x0c, 0x01})
	transport := platformtest.Transport{UnaryFunc: func(ctx context.Context, r platform.RPCRequest) (platform.RPCResponse, error) {
		if r.Method != "/wendycloud.v2.AssetService/GetAsset" {
			t.Fatal(r.Method)
		}
		var request pb.GetAssetRequest
		if err := proto.Unmarshal(r.Message, &request); err != nil {
			t.Fatal(err)
		}
		if request.Id != assetID || request.GetOrganizationId() != tenant || request.GetDeviceId() != assetID {
			t.Fatal(&request)
		}
		return platform.RPCResponse{Message: wire(t, wanted)}, nil
	}}
	got, err := (&Client{Transport: transport}).GetAsset(context.Background(), &GetAssetRequest{Id: assetID, OrganizationId: proto.String(tenant), DeviceId: proto.String(assetID)})
	if err != nil || !proto.Equal(got, wanted) || !bytes.Equal(got.ProtoReflect().GetUnknown(), wanted.ProtoReflect().GetUnknown()) {
		t.Fatalf("asset changed %v %v", got, err)
	}
}

func TestGetAssetStatusDetailsAndOrganizationLookup(t *testing.T) {
	failure := &platform.RPCStatusError{Code: platform.RPCNotFound, Message: "deleted", TrailingMetadata: platform.Metadata{{Name: "grpc-status-details-bin", Value: []byte("typed deletion evidence")}}}
	transport := platformtest.Transport{UnaryFunc: func(ctx context.Context, r platform.RPCRequest) (platform.RPCResponse, error) {
		return platform.RPCResponse{}, failure
	}}
	_, err := (&Client{Transport: transport}).GetAsset(context.Background(), &GetAssetRequest{Id: assetID})
	var status *platform.RPCStatusError
	if !errors.As(err, &status) || status != failure {
		t.Fatal("typed status lost", err)
	}
	transport.UnaryFunc = func(ctx context.Context, r platform.RPCRequest) (platform.RPCResponse, error) {
		var request pb.GetOrganizationRequest
		if r.Method != "/wendycloud.v2.OrganizationService/GetOrganization" {
			t.Fatal(r.Method)
		}
		if err := proto.Unmarshal(r.Message, &request); err != nil || request.Id != tenant {
			t.Fatal("tenant selector changed", err)
		}
		return platform.RPCResponse{Message: wire(t, &pb.Organization{Id: tenant, Name: "org"})}, nil
	}
	got, err := (&Client{Transport: transport}).GetOrganization(context.Background(), tenant)
	if err != nil || got.Id != tenant || got.Name != "org" {
		t.Fatalf("organization %v %v", got, err)
	}
}

func TestLookupCancellationAndMalformedResponse(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := (&Client{Transport: platformtest.Transport{}}).GetOrganization(ctx, tenant)
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	transport := platformtest.Transport{UnaryFunc: func(context.Context, platform.RPCRequest) (platform.RPCResponse, error) {
		return platform.RPCResponse{Message: []byte{0xff}}, nil
	}}
	if _, err := (&Client{Transport: transport}).GetOrganization(context.Background(), tenant); err == nil {
		t.Fatal("invalid protobuf accepted")
	}
}
