package services

import (
	"context"
	"errors"
	"github.com/wendylabsinc/wendy/go/internal/shared/discovery"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
	"io"
	"net"
	"strconv"
	"syscall"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/agent/meshsession"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

func TestMeshAppDialerNeverUsesLegacyOrCloudFallback(t *testing.T) {
	d, probes := testDialer()
	peer := appTestMeshDevice(t)
	d.SetLocalMeshDialer(func(context.Context, meshid.Device, uint16) (net.Conn, error) {
		return nil, meshsession.ErrDenied
	})
	app := NewMeshAppDialer(d)
	if _, _, err := app.DialDevice(context.Background(), peer, 8080); !errors.Is(err, meshsession.ErrDenied) {
		t.Fatalf("denial = %v", err)
	}
	if probes.lookups != 0 || probes.lanDials != 0 || probes.brokerDials != 0 {
		t.Fatalf("app dial used legacy transport: %+v", probes)
	}
	d.SetLocalMeshDialer(func(context.Context, meshid.Device, uint16) (net.Conn, error) {
		conn, _ := net.Pipe()
		return conn, nil
	})
	conn, mode, err := app.DialDevice(context.Background(), peer, 8080)
	if err != nil || mode != "local-mesh" {
		t.Fatalf("mode=%q err=%v", mode, err)
	}
	_ = conn.Close()
	if probes.lookups != 0 || probes.lanDials != 0 || probes.brokerDials != 0 {
		t.Fatalf("app dial used legacy transport: %+v", probes)
	}
}

func TestMeshAppDialerRetriesTransientKernelRouteFailure(t *testing.T) {
	d, probes := testDialer()
	peer := appTestMeshDevice(t)
	attempts := 0
	d.SetLocalMeshDialer(func(context.Context, meshid.Device, uint16) (net.Conn, error) {
		attempts++
		if attempts == 1 {
			return nil, &net.OpError{Op: "write", Net: "udp", Err: syscall.EHOSTUNREACH}
		}
		conn, other := net.Pipe()
		_ = other.Close()
		return conn, nil
	})
	conn, mode, err := NewMeshAppDialer(d).DialDevice(context.Background(), peer, 8080)
	if err != nil || mode != "local-mesh" || attempts != 2 {
		t.Fatalf("mode=%q err=%v attempts=%d", mode, err, attempts)
	}
	_ = conn.Close()
	if probes.lookups != 0 || probes.lanDials != 0 || probes.brokerDials != 0 {
		t.Fatalf("app dial used legacy transport: %+v", probes)
	}
}

// appTestMeshDevice is the fixed device principal used by the mesh app-path
// tests.
func appTestMeshDevice(t *testing.T) meshid.Device {
	t.Helper()
	device, err := meshid.DeviceFromPrincipal("spiffe://wendy.sh/tenant/11111111-1111-4111-8111-111111111111/device/22222222-2222-4222-8222-222222222222")
	if err != nil {
		t.Fatal(err)
	}
	return device
}

// The proxy keeps one dialer from unenrolled startup through enrollment and
// first configuration. Runtime route appearance/removal must affect that same
// instance without reconstruction or a cloud/legacy fallback.
func TestMeshAppDialerFollowsLateRuntimeWithoutRestart(t *testing.T) {
	d, probes := testDialer()
	d.UpdateIdentity("", "", "")
	peer := appTestMeshDevice(t)
	available := false
	d.SetLocalMeshDialer(func(context.Context, meshid.Device, uint16) (net.Conn, error) {
		if !available {
			return nil, meshsession.ErrNoRoute
		}
		conn, other := net.Pipe()
		_ = other.Close()
		return conn, nil
	})
	app := NewMeshAppDialer(d)
	assertUnavailable := func() {
		t.Helper()
		if _, _, err := app.DialDevice(context.Background(), peer, 8080); !errors.Is(err, meshsession.ErrNoRoute) {
			t.Fatalf("absent runtime: %v", err)
		}
	}
	assertUnavailable()
	d.UpdateIdentity("CERT", "KEY", "CHAIN")
	assertUnavailable()
	available = true
	conn, mode, err := app.DialDevice(context.Background(), peer, 8080)
	if err != nil || mode != "local-mesh" {
		t.Fatalf("late route: %q %v", mode, err)
	}
	_ = conn.Close()
	available = false
	assertUnavailable()
	if probes.lookups == 0 || probes.lanDials != 0 || probes.brokerDials != 0 {
		t.Fatalf("missing route must check LAN without administrative fallback: %+v", probes)
	}
}

func TestMeshAppDialerUsesAuthorizedLANWhenMeshHasNoRoute(t *testing.T) {
	d, probes := testDialer()
	peer := appTestMeshDevice(t)
	probes.lanAddr = "192.0.2.10:50052"
	appLANTestEvents(d, probes, appTestMeshDevice(t))
	d.SetLocalMeshDialer(func(context.Context, meshid.Device, uint16) (net.Conn, error) {
		return nil, meshsession.ErrNoRoute
	})
	var calls int
	d.appLANDial = func(_ context.Context, address string, device meshid.Device, port uint16) (net.Conn, error) {
		calls++
		if address != probes.lanAddr || device != peer || port != 8080 {
			t.Fatalf("LAN app target = %s %s %d", address, device, port)
		}
		local, remote := net.Pipe()
		_ = remote.Close()
		return local, nil
	}
	for range 2 {
		conn, mode, err := NewMeshAppDialer(d).DialDevice(context.Background(), peer, 8080)
		if err != nil || mode != "lan-direct" {
			t.Fatalf("LAN app mode=%q err=%v", mode, err)
		}
		_ = conn.Close()
	}
	if calls != 2 || probes.lookups != 2 || probes.lanDials != 0 || probes.brokerDials != 0 {
		t.Fatalf("LAN application dials=%d probes=%+v", calls, probes)
	}
}

func TestMeshAppDialerLANDenialNeverUsesAdministrativeService(t *testing.T) {
	d, probes := testDialer()
	probes.lanAddr = "192.0.2.10:50052"
	appLANTestEvents(d, probes, appTestMeshDevice(t))
	d.appLANDial = func(context.Context, string, meshid.Device, uint16) (net.Conn, error) {
		return nil, meshsession.ErrDenied
	}
	_, _, err := NewMeshAppDialer(d).DialDevice(context.Background(), appTestMeshDevice(t), 8080)
	if !errors.Is(err, meshsession.ErrDenied) || probes.lanDials != 0 || probes.brokerDials != 0 {
		t.Fatalf("LAN denial=%v probes=%+v", err, probes)
	}
}

func TestMeshAppDialerUsesOnlyExplicitAppCloudCapability(t *testing.T) {
	d, probes := testDialer()
	d.SetCloudEndpoint("api.cloud.test:443")
	peer := appTestMeshDevice(t)
	called := 0
	d.cloudAppDial = func(_ context.Context, device meshid.Device, port uint16) (net.Conn, error) {
		called++
		if device != peer || port != 8080 {
			t.Fatalf("Cloud app target=%s:%d", device, port)
		}
		local, remote := net.Pipe()
		remote.Close()
		return local, nil
	}
	flow, mode, err := NewMeshAppDialer(d).DialDevice(context.Background(), peer, 8080)
	if err != nil || mode != "cloud-app" || called != 1 || probes.brokerDials != 0 {
		t.Fatalf("Cloud app mode=%q err=%v calls=%d probes=%+v", mode, err, called, probes)
	}
	flow.Close()
	d.SetLocalMeshDialer(func(context.Context, meshid.Device, uint16) (net.Conn, error) { return nil, meshsession.ErrDenied })
	if _, _, err := NewMeshAppDialer(d).DialDevice(context.Background(), peer, 8080); !errors.Is(err, meshsession.ErrDenied) || called != 1 {
		t.Fatal("mesh denial triggered Cloud fallback")
	}
}

func TestAppTransportsRetireWhenEnrolledIdentityChanges(t *testing.T) {
	d, _ := testDialer()
	identity := d.identity()
	local, remote := net.Pipe()
	defer remote.Close()
	flow, err := d.ownApplicationConnection(local, func() error { return nil }, identity)
	if err != nil {
		t.Fatal(err)
	}
	d.UpdateIdentity("replacement certificate", "replacement key", "replacement issuer")
	if _, err := flow.Write([]byte("old tenant")); !errors.Is(err, net.ErrClosed) && !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("retired app transport write=%v", err)
	}
	local, remote = net.Pipe()
	defer remote.Close()
	if flow, err := d.ownApplicationConnection(local, func() error { return nil }, identity); err == nil {
		flow.Close()
		t.Fatal("in-flight setup admitted a replaced identity")
	}
}

func TestAppTransportsRetireWhenEnrolledCloudChanges(t *testing.T) {
	d, _ := testDialer()
	d.SetCloudEndpoint("https://old.example")
	identity := d.identity()
	local, remote := net.Pipe()
	defer remote.Close()
	flow, err := d.ownApplicationConnection(local, func() error { return nil }, identity)
	if err != nil {
		t.Fatal(err)
	}
	d.UpdateCloudIdentity("https://new.example", identity.certPEM, identity.keyPEM, identity.chainPEM)
	if _, err := flow.Write([]byte("old cloud")); !errors.Is(err, net.ErrClosed) && !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("retired cloud transport write=%v", err)
	}
	local, remote = net.Pipe()
	defer remote.Close()
	if flow, err := d.ownApplicationConnection(local, func() error { return nil }, identity); err == nil {
		flow.Close()
		t.Fatal("in-flight setup admitted a replaced Cloud endpoint")
	}
}

func appLANTestEvents(d *MeshDialer, probes *dialerProbes, peer meshid.Device) {
	d.lanEvents = func(context.Context, discovery.StreamOptions) <-chan discovery.LANEvent {
		probes.lookups++
		out := make(chan discovery.LANEvent, 1)
		if probes.lanAddr != "" {
			host, port, _ := net.SplitHostPort(probes.lanAddr)
			number, _ := strconv.Atoi(port)
			out <- discovery.LANEvent{Kind: discovery.LANFound, Device: models.LANDevice{IPAddress: host, Port: number, PrincipalURI: peer.Principal(), IsMTLS: true}}
		}
		close(out)
		return out
	}
}
