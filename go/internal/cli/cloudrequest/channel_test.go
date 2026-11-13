package cloudrequest

import (
	"context"
	"google.golang.org/grpc"
	"testing"

	cloudpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
)

func TestMarkChannel(t *testing.T) {
	for _, env := range []string{channelEnv, mcpClientIDEnv, relaySessionIDEnv} {
		t.Setenv(env, "")
	}
	plain := &cloudpbv2.SignedRequest{}
	markChannel(plain)
	if plain.GetChannel() != cloudpbv2.RequestChannel_REQUEST_CHANNEL_UNSPECIFIED {
		t.Fatalf("plain CLI call marked %v", plain.GetChannel())
	}

	if err := SetChannel(cloudpbv2.RequestChannel_REQUEST_CHANNEL_MCP_LOCAL, "", ""); err != nil {
		t.Fatal(err)
	}
	local := &cloudpbv2.SignedRequest{}
	markChannel(local)
	if local.GetChannel() != cloudpbv2.RequestChannel_REQUEST_CHANNEL_MCP_LOCAL || local.GetMcpClientId() != "" {
		t.Fatalf("local MCP call marked %v", local)
	}

	if err := SetChannel(cloudpbv2.RequestChannel_REQUEST_CHANNEL_HOSTED_MCP, "https://chatgpt.com/oauth", "sess-1"); err != nil {
		t.Fatal(err)
	}
	hosted := &cloudpbv2.SignedRequest{}
	markChannel(hosted)
	if hosted.GetChannel() != cloudpbv2.RequestChannel_REQUEST_CHANNEL_HOSTED_MCP || hosted.GetMcpClientId() != "https://chatgpt.com/oauth" || hosted.GetRelaySessionId() != "sess-1" {
		t.Fatalf("hosted MCP call marked %v", hosted)
	}
}

func TestEmbeddedChannelDoesNotLeakToNativeCalls(t *testing.T) {
	t.Setenv(channelEnv, "")
	base := context.Background()
	ctx, cancel := context.WithCancel(WithLocalMCP(base))
	defer cancel()
	embedded := &cloudpbv2.SignedRequest{}
	markChannel(embedded)
	markContextChannel(ctx, embedded)
	if embedded.Channel != cloudpbv2.RequestChannel_REQUEST_CHANNEL_MCP_LOCAL {
		t.Fatalf("embedded tool request lacks MCP channel: %v", embedded.Channel)
	}
	native := &cloudpbv2.SignedRequest{}
	markChannel(native)
	markContextChannel(base, native)
	if native.Channel != cloudpbv2.RequestChannel_REQUEST_CHANNEL_UNSPECIFIED {
		t.Fatalf("embedded tool changed native request channel: %v", native.Channel)
	}
}

type channelCheckingCloud struct {
	*fakeCloud
	want cloudpbv2.RequestChannel
}

func (f *channelCheckingCloud) Invoke(ctx context.Context, method string, args, reply any, opts ...grpc.CallOption) error {
	if got := args.(*cloudpbv2.SignedRequest).Channel; got != f.want {
		f.t.Fatalf("%s channel = %v, want %v", method, got, f.want)
	}
	return f.fakeCloud.Invoke(ctx, method, args, reply, opts...)
}

func TestEmbeddedChannelReachesRegistrationAndSignedRequests(t *testing.T) {
	t.Setenv(channelEnv, "")
	auth, _, _ := testAuth(t)
	conn := &channelCheckingCloud{fakeCloud: &fakeCloud{t: t}, want: cloudpbv2.RequestChannel_REQUEST_CHANNEL_MCP_LOCAL}
	call := func(ctx context.Context) {
		t.Helper()
		err := Invoke(ctx, conn, auth, cloudpbv2.DeviceEnrollmentService_EnrollDevice_FullMethodName, &cloudpbv2.EnrollDeviceRequest{DeviceId: "dev-1"}, &cloudpbv2.EnrollDeviceResponse{})
		if err != nil {
			t.Fatal(err)
		}
	}
	call(WithLocalMCP(context.Background()))
	if len(conn.calls) < 2 {
		t.Fatal("expected both registration and enrollment")
	}
	conn.want = cloudpbv2.RequestChannel_REQUEST_CHANNEL_UNSPECIFIED
	call(context.Background())
}
