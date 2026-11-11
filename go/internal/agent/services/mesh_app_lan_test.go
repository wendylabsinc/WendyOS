package services

import (
	"context"
	"github.com/wendylabsinc/wendy/go/internal/shared/discovery"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/interceptor"
	"github.com/wendylabsinc/wendy/go/internal/agent/meshsession"
	"github.com/wendylabsinc/wendy/go/internal/agent/mtls"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// The actual agent mTLS listener proves that a same-tenant device can reach a
// published app without a Babel route, while UUID and port admission stay live.
func TestMeshAppLANUsesPinnedMTLSAndRunningAppAdmission(t *testing.T) {
	root, issue := newRPCPolicyIssuer(t)
	tenant, err := meshid.ParseTenant(meshTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	self, err := meshid.ParseDevice(tenant, "cccccccc-cccc-4ccc-8ccc-cccccccccccc")
	if err != nil {
		t.Fatal(err)
	}
	peer, err := meshid.DeviceFromPrincipal(meshTestDevice)
	if err != nil {
		t.Fatal(err)
	}
	serverCert, serverKey := issue(2, "server", peer.Principal())
	clientCert, clientKey := issue(3, "client", self.Principal())
	server, err := mtls.NewServer(serverCert, root, serverKey, zap.NewNop(), time.Time{}, certs.Scope{TenantUUID: meshTestTenant}, interceptor.OrgModeStrict)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(server.Stop)
	app, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.Close() })
	var appConnections atomic.Int32
	go func() {
		for {
			conn, err := app.Accept()
			if err != nil {
				return
			}
			appConnections.Add(1)
			go func() {
				defer conn.Close()
				data, err := io.ReadAll(conn)
				if err == nil {
					_, _ = conn.Write(data)
				}
			}()
		}
	}()
	svc, _ := newMeshServiceForTest(t)
	svc.dialLocal = func(_ string, timeout time.Duration) (net.Conn, error) {
		return net.DialTimeout("tcp", app.Addr().String(), timeout)
	}
	pb.RegisterWendyMeshServiceServer(server, svc)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() { _ = server.Serve(listener) }()
	dialer := NewMeshDialer(zap.NewNop(), clientCert, clientKey, root, nil)
	wrong, _ := meshid.ParseDevice(tenant, "dddddddd-dddd-4ddd-8ddd-dddddddddddd")
	dialer.lanEvents = func(context.Context, discovery.StreamOptions) <-chan discovery.LANEvent {
		out := make(chan discovery.LANEvent, 2)
		for _, device := range []meshid.Device{peer, wrong} {
			out <- discovery.LANEvent{Kind: discovery.LANFound, Device: models.LANDevice{PrincipalURI: device.Principal(), IPAddress: "2001:db8::1", Addresses: []string{"192.0.2.20", "192.0.2.21"}, Port: listener.Addr().(*net.TCPAddr).Port, IsMTLS: true}}
		}
		close(out)
		return out
	}
	var socketAttempts atomic.Int32
	dialer.appSocketDial = func(ctx context.Context, network, address string) (net.Conn, error) {
		socketAttempts.Add(1)
		if strings.HasPrefix(address, "[2001:db8:") {
			return nil, syscall.ENETUNREACH
		}
		return (&net.Dialer{}).DialContext(ctx, network, listener.Addr().String())
	}
	dialer.lanLookup = func(context.Context, meshid.Device) (string, bool) { return listener.Addr().String(), true }
	dialer.SetLocalMeshDialer(func(context.Context, meshid.Device, uint16) (net.Conn, error) { return nil, meshsession.ErrNoRoute })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	setup, stopSetup := context.WithCancel(ctx)
	flow, mode, err := NewMeshAppDialer(dialer).DialDevice(setup, peer, 8080)
	stopSetup()
	if err != nil || mode != "lan-direct" {
		t.Fatalf("LAN app mode=%q err=%v", mode, err)
	}
	defer flow.Close()
	_ = flow.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.WriteString(flow, "LAN without mesh"); err != nil {
		t.Fatal(err)
	}
	if err := flow.(interface{ CloseWrite() error }).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(flow)
	if err != nil || string(got) != "LAN without mesh" {
		t.Fatalf("LAN echo=%q err=%v", got, err)
	}
	_ = flow.Close()
	if _, _, err := NewMeshAppDialer(dialer).DialDevice(ctx, peer, 8081); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("unpublished port=%v", err)
	}
	_, _, err = NewMeshAppDialer(dialer).DialDevice(ctx, wrong, 8080)
	if err == nil || !strings.Contains(err.Error(), "device") {
		t.Fatalf("wrong advertised identity=%v", err)
	}
	if count := socketAttempts.Load(); count != 6 {
		t.Fatalf("LAN address attempts=%d, want unreachable+reachable once for each success/port denial/identity denial", count)
	}
	if count := appConnections.Load(); count != 1 {
		t.Fatalf("unauthorized application connections=%d", count)
	}
}
