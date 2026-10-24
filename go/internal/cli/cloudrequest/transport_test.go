package cloudrequest

import (
	"context"
	"errors"
	"fmt"
	"io"

	wendy "github.com/wendylabsinc/wendy-go"
	"github.com/wendylabsinc/wendy-go/cloud"
	"google.golang.org/protobuf/proto"
	"strings"
	"testing"

	pb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

type collisionCloud struct{ *fakeCloud }

func (f collisionCloud) Invoke(ctx context.Context, method string, args, reply any, options ...grpc.CallOption) error {
	if method == pb.OperatorSessionService_RegisterOperatorLeaf_FullMethodName {
		return f.fakeCloud.Invoke(ctx, method, args, reply, options...)
	}
	for _, option := range options {
		if trailer, ok := option.(grpc.TrailerCallOption); ok {
			*trailer.TrailerAddr = metadata.Pairs(CollidingFieldMetadataKey, "name", "binary-bin", string([]byte{0, 255}))
		}
	}
	return status.Error(codes.AlreadyExists, "device name already used")
}

func TestSDKAdapterPreservesCollisionTrailerAndGRPCDiagnostic(t *testing.T) {
	auth, _, _ := testAuth(t)
	conn := collisionCloud{&fakeCloud{t: t}}
	var trailer metadata.MD
	err := InvokeWithTrailer(context.Background(), conn, auth, pb.DeviceEnrollmentService_EnrollDevice_FullMethodName, &pb.EnrollDeviceRequest{DeviceId: "dev-1"}, &pb.EnrollDeviceResponse{}, &trailer)
	if status.Code(err) != codes.AlreadyExists || err.Error() != "rpc error: code = AlreadyExists desc = device name already used" {
		t.Fatal("diagnostic changed", err)
	}
	if trailer.Get(CollidingFieldMetadataKey)[0] != "name" || trailer.Get("binary-bin")[0] != string([]byte{0, 255}) {
		t.Fatal("trailers changed", trailer)
	}
}

func TestSDKAdapterPreservesFinalRetryDiagnostic(t *testing.T) {
	auth, _, _ := testAuth(t)
	denied := status.Error(codes.PermissionDenied, "permission refused")
	conn := &fakeCloud{t: t, refuse: []error{denied, denied}}
	err := Invoke(context.Background(), conn, auth, pb.DeviceEnrollmentService_EnrollDevice_FullMethodName, &pb.EnrollDeviceRequest{DeviceId: "dev-1"}, &pb.EnrollDeviceResponse{})
	if status.Code(err) != codes.PermissionDenied || !strings.HasSuffix(err.Error(), "with the operator certificate attached: rpc error: code = PermissionDenied desc = permission refused") {
		t.Fatal("retry diagnostic changed", err)
	}
}

type inventoryTransport struct {
	wendy.Transport
	open func(context.Context, wendy.RPCRequest) (wendy.ServerStream, error)
}

func (t inventoryTransport) ServerStream(ctx context.Context, request wendy.RPCRequest) (wendy.ServerStream, error) {
	return t.open(ctx, request)
}

type inventoryPage struct {
	wendy.ServerStream
	message []byte
}

func (s *inventoryPage) Recv() ([]byte, error) {
	if s.message == nil {
		return nil, io.EOF
	}
	data := s.message
	s.message = nil
	return data, nil
}
func (*inventoryPage) Close() error { return nil }

func TestSDKClientPreservesAuthenticationErrorWithoutListingPrefix(t *testing.T) {
	for _, failAt := range []int{1, 2} {
		t.Run(fmt.Sprint("page=", failAt), func(t *testing.T) {
			cause := errors.New("invalid_grant")
			refreshErr := fmt.Errorf("refreshing OAuth session: %w", cause)
			calls, opened := 0, 0
			transport := inventoryTransport{open: func(context.Context, wendy.RPCRequest) (wendy.ServerStream, error) {
				opened++
				message, err := proto.Marshal(&pb.ListAssetsResponse{Asset: &pb.Asset{Id: "device-1"}, Total: 2})
				if err != nil {
					t.Fatal(err)
				}
				return &inventoryPage{message: message}, nil
			}}
			c := &client{Client: &cloud.Client{Transport: authenticatedTransport{Transport: transport, authenticate: func(ctx context.Context) (context.Context, error) {
				calls++
				if calls == failAt {
					return nil, refreshErr
				}
				return ctx, nil
			}}}}
			assets, err := c.ListDevices(context.Background(), testTenant, "", false)
			if err != refreshErr || err.Error() != "refreshing OAuth session: invalid_grant" || !errors.Is(err, cause) {
				t.Fatalf("authentication diagnostic changed: %v", err)
			}
			if assets != nil || opened != failAt-1 {
				t.Fatalf("partial inventory or unexpected RPC: %v, %d", assets, opened)
			}
			// Organization lookup already returned the host error without RPC context.
			calls = failAt - 1
			_, err = c.GetOrganization(context.Background(), testTenant)
			if err == nil || err.Error() != refreshErr.Error() || !errors.Is(err, cause) {
				t.Fatalf("organization diagnostic changed: %v", err)
			}
		})
	}
}

func TestSDKClientKeepsExistingRPCErrorContext(t *testing.T) {
	cause := status.Error(codes.PermissionDenied, "membership required")
	transport := inventoryTransport{open: func(context.Context, wendy.RPCRequest) (wendy.ServerStream, error) {
		return nil, &wendy.RPCStatusError{Code: wendy.RPCPermissionDenied, Message: "membership required", Cause: cause}
	}}
	c := &client{Client: &cloud.Client{Transport: authenticatedTransport{Transport: transport}}}
	_, err := c.ListDevices(context.Background(), testTenant, "", false)
	const want = "listing devices: rpc error: code = PermissionDenied desc = membership required"
	if err == nil || err.Error() != want || status.Code(err) != codes.PermissionDenied {
		t.Fatalf("RPC diagnostic changed: %v", err)
	}
}
