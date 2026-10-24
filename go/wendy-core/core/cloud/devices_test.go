package cloud

import (
	"context"
	"errors"
	"fmt"
	"io"
	"testing"

	pb "github.com/wendylabsinc/WendyOS/go/wendy-core/core/cloud/internal/cloudpb"
	"github.com/wendylabsinc/WendyOS/go/wendy-core/platform"
	"github.com/wendylabsinc/WendyOS/go/wendy-core/platform/platformtest"
	"google.golang.org/protobuf/proto"
)

func TestDeviceListRetainsFilterAndUUIDsAcrossPages(t *testing.T) {
	var offsets []int32
	closed := 0
	transport := platformtest.Transport{ServerStreamFunc: func(ctx context.Context, r platform.RPCRequest) (platform.ServerStream, error) {
		if r.Method != "/wendycloud.v2.AssetService/ListAssets" {
			t.Fatal(r.Method)
		}
		var request pb.ListAssetsRequest
		if err := proto.Unmarshal(r.Message, &request); err != nil {
			t.Fatal(err)
		}
		if request.OrganizationId != tenant || !request.GetIsComputeDevice() || !request.GetOnlineOnly() || request.GetFilter() != "robot" || request.GetLimit() != 200 || request.Offset == nil {
			t.Fatal("query changed", &request)
		}
		offsets = append(offsets, request.GetOffset())
		emitted := false
		return platformtest.ServerStream{CloseFunc: func() error { closed++; return nil }, RecvFunc: func() ([]byte, error) {
			if emitted {
				return nil, io.EOF
			}
			emitted = true
			return wire(t, &pb.ListAssetsResponse{Asset: &pb.Asset{Id: assetID, Name: "robot", OrganizationId: tenant}, Total: 2}), nil
		}}, nil
	}}
	assets, err := (&Client{Transport: transport}).ListDevices(context.Background(), tenant, "robot", true)
	if err != nil || len(assets) != 2 || assets[0].Id != assetID || len(offsets) != 2 || offsets[0] != 0 || offsets[1] != 1 || closed != 2 {
		t.Fatalf("assets %v offsets %v closed %d err %v", assets, offsets, closed, err)
	}
}

func TestDeviceListReturnsNoPartialResultsAndClosesStreams(t *testing.T) {
	failure := &platform.RPCStatusError{Code: platform.RPCUnavailable, Message: "offline"}
	closed := false
	received := 0
	transport := platformtest.Transport{ServerStreamFunc: func(ctx context.Context, r platform.RPCRequest) (platform.ServerStream, error) {
		return platformtest.ServerStream{CloseFunc: func() error { closed = true; return nil }, RecvFunc: func() ([]byte, error) {
			received++
			if received == 1 {
				return wire(t, &pb.ListAssetsResponse{Asset: &pb.Asset{Id: assetID}, Total: 2}), nil
			}
			return nil, failure
		}}, nil
	}}
	assets, err := (&Client{Transport: transport}).ListDevices(context.Background(), tenant, "", false)
	if assets != nil || !errors.Is(err, failure) || !closed {
		t.Fatalf("partial result %v closed %t err %v", assets, closed, err)
	}
}

func TestDeviceListBoundAndEmptyPage(t *testing.T) {
	for _, count := range []int{0, maxDevices + 1} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			received := 0
			closed := false
			transport := platformtest.Transport{ServerStreamFunc: func(ctx context.Context, r platform.RPCRequest) (platform.ServerStream, error) {
				return platformtest.ServerStream{CloseFunc: func() error { closed = true; return nil }, RecvFunc: func() ([]byte, error) {
					if received >= count {
						return nil, io.EOF
					}
					received++
					return wire(t, &pb.ListAssetsResponse{Asset: &pb.Asset{Id: assetID}, Total: int32(count)}), nil
				}}, nil
			}}
			assets, err := (&Client{Transport: transport}).ListDevices(context.Background(), tenant, "", false)
			if !closed {
				t.Fatal("stream leaked")
			}
			if count == 0 {
				if err != nil || assets == nil || len(assets) != 0 {
					t.Fatalf("empty list %v %v", assets, err)
				}
			} else {
				var limit *DeviceLimitError
				if assets != nil || !errors.As(err, &limit) {
					t.Fatalf("truncated list %v %v", assets, err)
				}
			}
		})
	}
}
