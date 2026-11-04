package services

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/agent/ipcam"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

func TestCameraDHCPServiceValidation(t *testing.T) {
	s := &VideoService{}
	if _, err := s.GetCameraDHCPStatus(context.Background(), &agentpb.GetCameraDHCPStatusRequest{}); status.Code(err) != codes.Unavailable {
		t.Fatalf("missing manager: %v", err)
	}
	s.links = ipcam.NewLinkManager(nil, zap.NewNop())
	if _, err := s.SetCameraDHCP(context.Background(), &agentpb.SetCameraDHCPRequest{InterfaceName: "eth0\neth1", Enabled: true}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("invalid name: %v", err)
	}
	if _, err := s.SetCameraDHCP(context.Background(), &agentpb.SetCameraDHCPRequest{InterfaceName: "eth0", Enabled: true}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("unconfigured manager: %v", err)
	}
}

// Exercise legacy startup migration, the registered RPCs, and real persistence,
// including disabling a missing port. No host network configuration is changed.
func TestCameraDHCPServiceMigratedRoundTrip(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root-owned configuration")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "camera-dhcp-interfaces")
	registry := ipcam.NewRegistry(filepath.Join(dir, "cameras.json"))
	for _, cam := range []ipcam.Camera{
		{MAC: "02:00:00:00:00:01", Link: "camera-test0", Address: "10.98.0.50"},
		{MAC: "02:00:00:00:00:02", Link: "camera-test1", Address: "10.98.1.50"},
	} {
		if _, err := registry.Upsert(cam); err != nil {
			t.Fatal(err)
		}
	}
	credentials := ipcam.NewCredentialStore(filepath.Join(dir, "camera-credentials.json"))
	for _, mac := range []string{"02:00:00:00:00:01", "02:00:00:00:00:02"} {
		if err := credentials.Set(mac, ipcam.Credential{Username: "admin"}); err != nil {
			t.Fatal(err)
		}
	}
	// An ordinary client mistakenly served before must not enable another port.
	if _, err := registry.Upsert(ipcam.Camera{MAC: "02:00:00:00:00:03", Link: "camera-test2", Address: "10.98.2.50", DHCPServed: true}); err != nil {
		t.Fatal(err)
	}
	s := &VideoService{links: ipcam.NewLinkManager(registry, zap.NewNop()), credentials: credentials}
	if err := s.LoadCameraDHCPAllowlist(path); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "camera-test0\ncamera-test1\n" {
		t.Fatalf("migration must enable only credential-matched ports: %q, %v", data, err)
	}
	listener := bufconn.Listen(1024 * 1024)
	server := grpc.NewServer()
	agentpb.RegisterWendyVideoServiceServer(server, s)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	conn, err := grpc.NewClient("passthrough:///camera-dhcp", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	client := agentpb.NewWendyVideoServiceClient(conn)
	before, err := client.GetCameraDHCPStatus(context.Background(), &agentpb.GetCameraDHCPStatusRequest{})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, iface := range before.GetInterfaces() {
		if iface.GetInterfaceName() == "camera-test0" {
			found = iface.GetEnabled() && iface.GetState() == "missing"
		}
	}
	if !found {
		t.Fatalf("configured missing port not reported: %v", before)
	}
	if _, err := client.SetCameraDHCP(context.Background(), &agentpb.SetCameraDHCPRequest{InterfaceName: "camera-test0", Enabled: false}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "camera-test1\n" {
		t.Fatalf("persisted configuration = %q, %v", data, err)
	}
	restarted := &VideoService{links: ipcam.NewLinkManager(registry, zap.NewNop()), credentials: credentials}
	if err := restarted.LoadCameraDHCPAllowlist(path); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(path)
	if err != nil || string(data) != "camera-test1\n" {
		t.Fatalf("restart overrode operator disable = %q, %v", data, err)
	}
	if _, err := client.SetCameraDHCP(context.Background(), &agentpb.SetCameraDHCPRequest{InterfaceName: "camera-test0", Enabled: true}); status.Code(err) != codes.NotFound {
		t.Fatalf("enable absent port: %v", err)
	}
}

func TestCameraDHCPStatusMapping(t *testing.T) {
	got := cameraDHCPStatusProto(ipcam.DHCPStatus{
		ConfigurationError: "configuration error", InterfaceError: "inspection error",
		Interfaces: []ipcam.DHCPInterfaceStatus{{Name: "eth1", Enabled: true, Serving: false, Present: true, Carrier: true, Up: true, IPv4s: []string{"192.168.1.2"}, State: "blocked", Detail: "uplink address"}},
	})
	if got.GetConfigurationError() != "configuration error" || got.GetInterfaceError() != "inspection error" || len(got.GetInterfaces()) != 1 {
		t.Fatalf("status: %v", got)
	}
	i := got.GetInterfaces()[0]
	if i.GetInterfaceName() != "eth1" || !i.GetEnabled() || i.GetServing() || !i.GetPresent() || !i.GetCarrier() || !i.GetUp() || i.GetState() != "blocked" || i.GetDetail() != "uplink address" || i.GetIpv4Addresses()[0] != "192.168.1.2" {
		t.Fatalf("interface: %v", i)
	}
}
