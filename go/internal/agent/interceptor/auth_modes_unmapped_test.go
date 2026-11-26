package interceptor

import (
	"context"
	"net"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/trustedkeys"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/emptypb"
)

// A31: register real handlers and call them over gRPC. An unregistered method
// returning UNIMPLEMENTED would not prove default denial. Caller identity is
// supplied by an inert interceptor; this test does not claim TLS coverage.
func TestAuthModesA31RegisteredUnmappedCalls(t *testing.T) {
	keys, err := trustedkeys.Open(filepath.Join(t.TempDir(), trustedkeys.FileName))
	if err != nil {
		t.Fatal(err)
	}
	owner, operator, viewer := spkiOf(t), spkiOf(t), spkiOf(t)
	o, err := keys.Close(owner, "owner", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []struct {
		spki []byte
		role trustedkeys.Role
	}{{operator, trustedkeys.RoleOperator}, {viewer, trustedkeys.RoleViewer}} {
		if _, err = keys.Add(k.spki, k.role, "", o.Fingerprint(), time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []struct {
		name     string
		listener ClosedListener
		ctx      context.Context
		keyless  bool
	}{
		{"owner", ClosedKeyListener, keyCtx(owner), false}, {"operator", ClosedKeyListener, keyCtx(operator), false},
		{"viewer", ClosedKeyListener, keyCtx(viewer), false}, {"admin", ClosedAdminSocket, context.Background(), false},
		{"keyless-off", ClosedKeylessListener, context.Background(), false}, {"keyless-on", ClosedKeylessListener, context.Background(), true},
	} {
		t.Run(c.name, func(t *testing.T) {
			if err := keys.SetKeylessApps(c.keyless, o.Fingerprint()); err != nil {
				t.Fatal(err)
			}
			var entries atomic.Int32
			a := ClosedAuthorizer{Logger: zap.NewNop(), Listener: c.listener, Keys: keys}
			lis := bufconn.Listen(64 * 1024)
			t.Cleanup(func() { lis.Close() })
			srv := grpc.NewServer(grpc.UnaryInterceptor(func(ctx context.Context, req any, info *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
				return a.Unary()(c.ctx, req, info, h)
			}), grpc.StreamInterceptor(func(s any, stream grpc.ServerStream, info *grpc.StreamServerInfo, h grpc.StreamHandler) error {
				return a.Stream()(s, acceptanceStream{ServerStream: stream, ctx: c.ctx}, info, h)
			}))
			srv.RegisterService(&grpc.ServiceDesc{ServiceName: "fixture.Unmapped", HandlerType: (*interface{})(nil), Methods: []grpc.MethodDesc{{MethodName: "Unary", Handler: func(s any, ctx context.Context, dec func(any) error, i grpc.UnaryServerInterceptor) (any, error) {
				req := new(emptypb.Empty)
				if err := dec(req); err != nil {
					return nil, err
				}
				h := func(context.Context, any) (any, error) { entries.Add(1); return &emptypb.Empty{}, nil }
				return i(ctx, req, &grpc.UnaryServerInfo{Server: s, FullMethod: "/fixture.Unmapped/Unary"}, h)
			}}}, Streams: []grpc.StreamDesc{{StreamName: "Stream", ServerStreams: true, Handler: func(s any, stream grpc.ServerStream) error { entries.Add(1); return stream.SendMsg(&emptypb.Empty{}) }}}}, struct{}{})
			t.Cleanup(srv.Stop)
			go srv.Serve(lis)
			conn, err := grpc.NewClient("passthrough:///fixture", grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }), grpc.WithTransportCredentials(insecure.NewCredentials()))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { conn.Close() })
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			err = conn.Invoke(ctx, "/fixture.Unmapped/Unary", &emptypb.Empty{}, &emptypb.Empty{})
			if status.Code(err) != codes.PermissionDenied {
				t.Fatalf("unary: %v", err)
			}
			stream, err := conn.NewStream(ctx, &grpc.StreamDesc{ServerStreams: true}, "/fixture.Unmapped/Stream")
			if err != nil {
				t.Fatal(err)
			}
			_ = stream.SendMsg(&emptypb.Empty{})
			_ = stream.CloseSend()
			err = stream.RecvMsg(&emptypb.Empty{})
			if status.Code(err) != codes.PermissionDenied {
				t.Fatalf("stream: %v", err)
			}
			if entries.Load() != 0 {
				t.Fatalf("unmapped handlers entered %d times", entries.Load())
			}
			t.Log("registered unary and streaming calls: PERMISSION_DENIED, zero handler entries")
		})
	}
}
