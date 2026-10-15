package services

import (
	"context"
	"net"
	"path/filepath"
	"sync"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/agent/robotarm"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"
)

func armClient(t *testing.T, svc *RobotArmService) pb.WendyRobotArmServiceClient {
	t.Helper()
	lis := bufconn.Listen(1024 * 1024)
	server := grpc.NewServer()
	pb.RegisterWendyRobotArmServiceServer(server, svc)
	go func() { _ = server.Serve(lis) }()
	t.Cleanup(server.Stop)
	conn, err := grpc.NewClient("passthrough:///target-agent", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return pb.NewWendyRobotArmServiceClient(conn)
}
func TestRobotArmTargetInventoryAndBindingLifecycle(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "target-agent", "bindings.json")
	d := &pb.RobotArm{VendorId: "1a86", ProductId: "55d3", SerialNumber: "TARGET", UsbInterface: "1", UsbLocation: "1-2", DevicePath: "/target/dev/ttyACM0"}
	robotarm.Identify(d)
	live := []*pb.RobotArm{d}
	var mu sync.RWMutex
	setLive := func(devices []*pb.RobotArm) { mu.Lock(); defer mu.Unlock(); live = devices }
	// The fixture lives on the target service. There is no CLI-side discovery or
	// serial path in the request; client and target may use different OSes.
	svc := NewRobotArmService(robotarm.NewStore(path), func(context.Context) ([]*pb.RobotArm, error) {
		mu.RLock()
		defer mu.RUnlock()
		return append([]*pb.RobotArm(nil), live...), nil
	})
	client := armClient(t, svc)
	listed, err := client.ListRobotArms(ctx, &pb.ListRobotArmsRequest{})
	if err != nil || len(listed.Devices) != 1 || listed.Devices[0].DevicePath != "/target/dev/ttyACM0" {
		t.Fatalf("target inventory: %v %v", listed, err)
	}
	bound, err := client.BindRobotArm(ctx, &pb.BindRobotArmRequest{Id: d.Id, Model: "so101", Name: "Follower"})
	if err != nil {
		t.Fatal(err)
	}
	if !bound.Device.Configured || bound.Device.Verified || bound.Device.Model != "so101" {
		t.Fatalf("invalid bind: %v", bound)
	}
	// Change only the endpoint node; saved serial identity stays associated.
	renamed := proto.Clone(d).(*pb.RobotArm)
	renamed.DevicePath = "/target/dev/ttyACM9"
	setLive([]*pb.RobotArm{renamed})
	listed, err = client.ListRobotArms(ctx, &pb.ListRobotArmsRequest{})
	if err != nil || listed.Devices[0].Name != "Follower" || listed.Devices[0].DevicePath != renamed.DevicePath {
		t.Fatalf("replug: %v %v", listed, err)
	}
	setLive(nil)
	listed, err = client.ListRobotArms(ctx, &pb.ListRobotArmsRequest{})
	if err != nil || len(listed.Devices) != 1 || listed.Devices[0].Connected {
		t.Fatalf("disconnect: %v %v", listed, err)
	}
	if _, err = client.BindRobotArm(ctx, &pb.BindRobotArmRequest{Id: d.Id, Model: "so101"}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("disconnected bind: %v", err)
	}
	// A restarted service sees the target's record and can forget it offline.
	restarted := NewRobotArmService(robotarm.NewStore(path), func(context.Context) ([]*pb.RobotArm, error) { return nil, nil })
	after, err := restarted.ListRobotArms(ctx, &pb.ListRobotArmsRequest{})
	if err != nil || len(after.Devices) != 1 {
		t.Fatal("target state did not survive restart")
	}
	if _, err = client.ForgetRobotArm(ctx, &pb.ForgetRobotArmRequest{Id: d.Id}); err != nil {
		t.Fatal(err)
	}
	after, err = restarted.ListRobotArms(ctx, &pb.ListRobotArmsRequest{})
	if err != nil || len(after.Devices) != 0 {
		t.Fatal("offline forget failed")
	}
}
func TestRobotArmAmbiguityWeakIdentityAndValidation(t *testing.T) {
	ctx := context.Background()
	d := &pb.RobotArm{VendorId: "abcd", ProductId: "1234", UsbLocation: "1-2", DevicePath: "/target/unknown"}
	robotarm.Identify(d)
	live := []*pb.RobotArm{d}
	s := NewRobotArmService(robotarm.NewStore(filepath.Join(t.TempDir(), "bindings.json")), func(context.Context) ([]*pb.RobotArm, error) { return live, nil })
	bind := &pb.BindRobotArmRequest{Id: d.Id, Model: "so101"}
	if _, err := s.BindRobotArm(ctx, bind); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("weak identity accepted implicitly: %v", err)
	}
	bind.AllowLocationBinding = true
	response, err := s.BindRobotArm(ctx, bind)
	if err != nil || response.Device.Candidate || !response.Device.Configured || response.Device.Verified {
		t.Fatalf("explicit unknown model declaration: %v %v", response, err)
	}
	duplicate := proto.Clone(d).(*pb.RobotArm)
	duplicate.DevicePath = "/target/duplicate"
	live = append(live, duplicate)
	if _, err = s.BindRobotArm(ctx, bind); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("duplicate accepted: %v", err)
	}
	for _, bad := range []*pb.BindRobotArmRequest{{Id: d.Id, Model: "other"}, {Model: "so101"}, {Id: d.Id, Model: "so101", Name: string(make([]byte, 129))}} {
		if _, err = s.BindRobotArm(ctx, bad); status.Code(err) != codes.InvalidArgument {
			t.Fatalf("validation: %v", err)
		}
	}
	live = nil
	if _, err = s.BindRobotArm(ctx, &pb.BindRobotArmRequest{Id: "missing", Model: "so101"}); status.Code(err) != codes.NotFound {
		t.Fatalf("missing accepted: %v", err)
	}
	if _, err = s.ForgetRobotArm(ctx, &pb.ForgetRobotArmRequest{}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("empty forget: %v", err)
	}
}

func TestRobotArmUnsupportedAndUnavailableInventory(t *testing.T) {
	for _, tc := range []struct {
		err  error
		code codes.Code
	}{{robotarm.ErrUnsupported, codes.Unimplemented}, {context.DeadlineExceeded, codes.Unavailable}} {
		svc := NewRobotArmService(robotarm.NewStore(filepath.Join(t.TempDir(), "bindings.json")), func(context.Context) ([]*pb.RobotArm, error) { return nil, tc.err })
		if _, err := svc.ListRobotArms(context.Background(), &pb.ListRobotArmsRequest{}); status.Code(err) != tc.code {
			t.Fatalf("inventory error: %v", err)
		}
		// Forget is configuration-only and remains possible when discovery is down.
		if _, err := svc.ForgetRobotArm(context.Background(), &pb.ForgetRobotArmRequest{Id: "offline"}); err != nil {
			t.Fatalf("offline forget depended on inventory: %v", err)
		}
	}
}
