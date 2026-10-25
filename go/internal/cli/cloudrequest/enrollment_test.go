package cloudrequest

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"testing"

	wendy "github.com/wendylabsinc/wendy-go"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

type enrollmentConn struct {
	t       *testing.T
	calls   int
	failure error
}

func (c *enrollmentConn) Invoke(ctx context.Context, method string, args, reply any, options ...grpc.CallOption) error {
	c.calls++
	request, ok := args.(*pb.GetAssetRequest)
	if !ok || request.GetId() != "asset-1" {
		c.t.Fatalf("typed request lost: %T", args)
	}
	md, _ := metadata.FromOutgoingContext(ctx)
	if !reflect.DeepEqual(md.Get("authorization"), []string{"Bearer host-token"}) || !reflect.DeepEqual(md.Get("repeated-bin"), []string{"\x00\xff", "second"}) {
		c.t.Fatal("host authentication or repeated binary metadata changed")
	}
	for _, option := range options {
		switch o := option.(type) {
		case grpc.HeaderCallOption:
			*o.HeaderAddr = metadata.Pairs("response-header", "value")
		case grpc.TrailerCallOption:
			*o.TrailerAddr = metadata.Pairs("wendy-colliding-field", "mesh_suffix", "proof-bin", "\x00\xff")
		}
	}
	if c.failure != nil {
		return c.failure
	}
	asset, ok := reply.(*pb.Asset)
	if !ok {
		c.t.Fatalf("typed reply lost: %T", reply)
	}
	asset.Id = request.Id
	asset.ProtoReflect().SetUnknown(request.ProtoReflect().GetUnknown())
	return nil
}
func (*enrollmentConn) NewStream(context.Context, *grpc.StreamDesc, string, ...grpc.CallOption) (grpc.ClientStream, error) {
	panic("unexpected stream")
}
func TestEnrollmentTransportRetainsTypedInterceptorsMetadataAndStatus(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "failure"}[fail], func(t *testing.T) {
			conn := &enrollmentConn{t: t}
			if fail {
				s, err := status.New(codes.AlreadyExists, "occupied").WithDetails(&errdetails.ErrorInfo{Reason: "device_id_collision"})
				if err != nil {
					t.Fatal(err)
				}
				conn.failure = s.Err()
			}
			unknown := protowire.AppendTag(nil, 19000, protowire.BytesType)
			unknown = protowire.AppendString(unknown, "preserve-unknown")
			request := &pb.GetAssetRequest{Id: "asset-1"}
			request.ProtoReflect().SetUnknown(unknown)
			wire, err := proto.Marshal(request)
			if err != nil {
				t.Fatal(err)
			}
			ctx := metadata.NewOutgoingContext(context.Background(), metadata.Pairs("authorization", "Bearer host-token"))
			response, err := EnrollmentTransport(conn).Unary(ctx, wendy.RPCRequest{Method: pb.AssetService_GetAsset_FullMethodName, Message: wire, Metadata: wendy.Metadata{{Name: "repeated-bin", Value: []byte{0, 255}}, {Name: "repeated-bin", Value: []byte("second")}}})
			if grpcMetadata(response.TrailingMetadata).Get("proof-bin")[0] != "\x00\xff" {
				t.Fatal("binary trailer lost")
			}
			if fail {
				var sdk *wendy.RPCStatusError
				if err == nil || err.Error() != conn.failure.Error() || !errors.Is(err, conn.failure) || !errors.As(err, &sdk) || sdk.Code != wendy.RPCAlreadyExists || status.Code(err) != codes.AlreadyExists || len(status.Convert(err).Details()) != 1 {
					t.Fatalf("status/cause/diagnostic lost: %v", err)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				var asset pb.Asset
				if err := proto.Unmarshal(response.Message, &asset); err != nil {
					t.Fatal(err)
				}
				if asset.Id != "asset-1" || !bytes.Equal(asset.ProtoReflect().GetUnknown(), unknown) {
					t.Fatal("reply or unknown fields lost")
				}
			}
		})
	}
}
func TestEnrollmentTransportStopsBeforeRPCOnCancellation(t *testing.T) {
	conn := &enrollmentConn{t: t}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := EnrollmentTransport(conn).Unary(ctx, wendy.RPCRequest{Method: pb.AssetService_GetAsset_FullMethodName})
	if conn.calls != 0 || !errors.Is(err, context.Canceled) {
		t.Fatalf("calls=%d err=%v", conn.calls, err)
	}
}
