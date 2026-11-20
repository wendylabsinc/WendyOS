package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	pbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// Opt-in real-device check against a panel already deployed on WendyOS. It only
// reads device information and applications, and runs a read-only shell probe.
func TestDeployedPanel(t *testing.T) {
	endpoint := os.Getenv("WENDY_TEST_PANEL_URL")
	if endpoint == "" {
		t.Skip("set WENDY_TEST_PANEL_URL to test a deployed panel")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	payload, _ := json.Marshal(map[string]string{"password": os.Getenv("WENDY_TEST_PANEL_PASSWORD")})
	req, _ := http.NewRequestWithContext(ctx, "POST", endpoint+"/api/session", strings.NewReader(string(payload)))
	req.Header.Set("Origin", endpoint)
	response, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 204 {
		t.Fatalf("login: %s", response.Status)
	}
	headers := http.Header{"Origin": {endpoint}}
	for _, cookie := range response.Cookies() {
		headers.Add("Cookie", cookie.String())
	}
	conn, err := grpc.NewClient("passthrough:///device", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(dialCtx context.Context, _ string) (net.Conn, error) {
		ws, response, err := websocket.Dial(dialCtx, endpoint+"/api/agent", &websocket.DialOptions{HTTPHeader: headers})
		if err != nil {
			if response != nil {
				t.Logf("websocket HTTP status: %s", response.Status)
			}
			return nil, err
		}
		return websocket.NetConn(ctx, ws, websocket.MessageBinary), nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	version, err := pb.NewWendyAgentServiceClient(conn).GetAgentVersion(ctx, &pb.GetAgentVersionRequest{})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("Connected to Wendy %s on %s", version.GetVersion(), version.GetOs())
	info, err := pbv2.NewWendyDeviceInfoServiceClient(conn).GetDeviceInfo(ctx, &pbv2.GetDeviceInfoRequest{})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("Device: %s %s, %d network interfaces", info.GetOs(), info.GetOsVersion(), len(version.GetNetworkInterfaces()))
	shell, err := pb.NewWendyShellServiceClient(conn).HostShell(ctx)
	if err != nil {
		t.Fatal(err)
	}
	err = shell.Send(&pb.HostShellRequest{RequestType: &pb.HostShellRequest_Start_{Start: &pb.HostShellRequest_Start{Command: []string{"sh", "-c", "printf panel-shell-ok; sleep 0.2"}, TermSize: &pb.WindowSize{Rows: 24, Cols: 80}}}})
	if err != nil {
		t.Fatal(err)
	}
	var output strings.Builder
	for {
		message, err := shell.Recv()
		if err != nil {
			t.Fatalf("shell ended before probe completed: %v", err)
		}
		output.Write(message.GetStdoutData())
		if _, exited := message.ResponseType.(*pb.HostShellResponse_ExitCode); exited {
			if message.GetExitCode() != 0 || !strings.Contains(output.String(), "panel-shell-ok") {
				t.Fatalf("shell probe failed: %q", output.String())
			}
			break
		}
	}
	_ = shell.CloseSend()
	stream, err := pb.NewWendyContainerServiceClient(conn).ListContainers(ctx, &pb.ListContainersRequest{})
	if err != nil {
		t.Fatal(err)
	}
	item, err := stream.Recv()
	if err != nil {
		t.Fatal(err)
	}
	if item.Container == nil {
		t.Fatal("no container record")
	}
}
