package commands

import (
	"context"
	"errors"
	"net"
	"reflect"
	"testing"
	"time"

	wendy "github.com/wendylabsinc/wendy-go"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

func TestDeviceTransportPreservesStatusMetadataAndProbeCache(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer(grpc.UnaryInterceptor(func(ctx context.Context, request any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		md, _ := metadata.FromIncomingContext(ctx)
		if !reflect.DeepEqual(md.Get("test"), []string{"one", "two"}) {
			t.Errorf("request metadata: %v", md.Get("test"))
		}
		grpc.SetHeader(ctx, metadata.Pairs("initial-bin", string([]byte{0, 255})))
		grpc.SetTrailer(ctx, metadata.Pairs("trailing", "retained"))
		if len(md.Get("fail")) > 0 {
			return nil, status.Error(codes.PermissionDenied, "fixture denied")
		}
		return handler(ctx, request)
	}))
	agentpb.RegisterWendyAgentServiceServer(server, versionOnlyAgent{})
	go server.Serve(listener)
	t.Cleanup(server.Stop)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for _, fail := range []bool{false, true} {
		conn, err := grpcclient.Connect(ctx, listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		request := wendy.RPCRequest{Method: agentpb.WendyAgentService_GetAgentVersion_FullMethodName, Metadata: wendy.Metadata{{Name: "test", Value: []byte("one")}, {Name: "test", Value: []byte("two")}}}
		if fail {
			request.Metadata = append(request.Metadata, wendy.MetadataEntry{Name: "fail", Value: []byte("yes")})
		}
		response, err := (&deviceConnection{AgentConnection: conn}).Unary(ctx, request)
		found := false
		for _, entry := range response.InitialMetadata {
			if entry.Name == "initial-bin" && reflect.DeepEqual(entry.Value, []byte{0, 255}) {
				found = true
			}
		}
		if !found {
			t.Fatalf("binary header lost: %v", response.InitialMetadata)
		}

		if !reflect.DeepEqual(response.TrailingMetadata, wendy.Metadata{{Name: "trailing", Value: []byte("retained")}}) {
			t.Fatalf("trailer lost: %v", response.TrailingMetadata)
		}
		cached, ok := conn.CachedAgentVersion()
		if fail {
			var rpc *wendy.RPCStatusError
			if !errors.As(err, &rpc) || rpc.Code != wendy.RPCPermissionDenied || status.Code(err) != codes.PermissionDenied || err.Error() != "rpc error: code = PermissionDenied desc = fixture denied" || ok {
				t.Fatalf("error=%v cached=%v", err, ok)
			}
			if !reflect.DeepEqual(rpc.InitialMetadata, response.InitialMetadata) || !reflect.DeepEqual(rpc.TrailingMetadata, response.TrailingMetadata) {
				t.Fatal("status metadata lost")
			}
		} else {
			message := new(agentpb.GetAgentVersionResponse)
			if err != nil || !ok || proto.Unmarshal(response.Message, message) != nil || !proto.Equal(message, cached) {
				t.Fatalf("response/cache mismatch: %v cached=%v", err, ok)
			}
		}
	}
}
