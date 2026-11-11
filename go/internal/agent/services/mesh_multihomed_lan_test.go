package services

import (
	"context"
	"io"
	"net"
	"net/netip"
	"syscall"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/agent/meshsession"
	"github.com/wendylabsinc/wendy/go/internal/shared/discovery"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
)

type reviewAppAdmission struct{ address string }

func (a reviewAppAdmission) DialAuthorized(port uint16, dial func(string) (net.Conn, error)) (net.Conn, error) {
	if port != 8080 {
		return nil, meshsession.ErrDenied
	}
	return dial(a.address)
}

func TestMultihomedLANTriesReachableSibling(t *testing.T) {
	d, _ := testDialer()
	peer := appTestMeshDevice(t)
	root, issue := newRPCPolicyIssuer(t)
	self, err := meshid.ParseDevice(peer.Tenant(), "cccccccc-cccc-4ccc-8ccc-cccccccccccc")
	if err != nil {
		t.Fatal(err)
	}
	serverCert, serverKey := issue(2, "server", peer.Principal())
	clientCert, clientKey := issue(3, "client", self.Principal())
	serverCredentials, err := localmesh.NewCredentials(serverCert, root, serverKey)
	if err != nil {
		t.Fatal(err)
	}
	clientCredentials, err := localmesh.NewCredentials(clientCert, root, clientKey)
	if err != nil {
		t.Fatal(err)
	}
	app, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	go func() {
		for {
			conn, err := app.Accept()
			if err != nil {
				return
			}
			go func() { defer conn.Close(); io.Copy(conn, conn) }()
		}
	}()
	server, err := meshsession.NewServer(serverCredentials, reviewAppAdmission{app.Addr().String()})
	if err != nil {
		t.Fatal(err)
	}
	listener := meshsession.NewApplicationListener()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*5)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- server.ServeTLS(ctx, listener) }()
	defer func() { cancel(); server.Close(); <-done }()
	d.lanLookup = d.discoverOnLAN
	d.lanEvents = func(ctx context.Context, _ discovery.StreamOptions) <-chan discovery.LANEvent {
		out := make(chan discovery.LANEvent, 2)
		out <- discovery.LANEvent{Kind: discovery.LANFound, Device: models.LANDevice{PrincipalURI: peer.Principal(), IPAddress: "2001:db8::1", Port: 50052, IsMTLS: true}}
		out <- discovery.LANEvent{Kind: discovery.LANUpdated, Device: models.LANDevice{PrincipalURI: peer.Principal(), IPAddress: "192.0.2.10", Addresses: []string{"192.0.2.10", "2001:db8::1"}, Port: 50052, IsMTLS: true}}
		go func() { <-ctx.Done(); close(out) }()
		return out
	}
	var attempted []string
	d.appLANDial = func(ctx context.Context, address string, device meshid.Device, port uint16) (net.Conn, error) {
		if device != peer {
			t.Fatalf("unexpected target principal: %v", device)
		}
		attempted = append(attempted, address)
		if address == "[2001:db8::1]:50052" {
			return nil, syscall.ENETUNREACH
		}
		return meshsession.DialTLSWithDialer(ctx, clientCredentials, device, netip.MustParseAddrPort(address), port, func(ctx context.Context, _, _ string) (net.Conn, error) { return listener.Dial(ctx) })
	}
	control, err := d.appLANDial(ctx, "192.0.2.10:50052", peer, 8080)
	if err != nil {
		t.Fatalf("reachable sibling control failed exact TLS identity/app admission: %v", err)
	}
	control.SetDeadline(time.Now().Add(time.Second))
	if _, err := control.Write([]byte("echo")); err != nil {
		t.Fatal(err)
	}
	reply := make([]byte, 4)
	if _, err := io.ReadFull(control, reply); err != nil || string(reply) != "echo" {
		t.Fatalf("reachable control echo=%q error=%v", reply, err)
	}
	control.Close()
	attempted = nil
	conn, _, err := NewMeshAppDialer(d).DialDevice(ctx, peer, 8080)
	if conn != nil {
		conn.Close()
	}
	if err != nil {
		t.Fatalf("reachable IPv4 sibling was advertised but not attempted: attempts=%v, error=%v", attempted, err)
	}
}
