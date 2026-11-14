package backend

import (
	"context"
	"errors"
	"net"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/wendylabsinc/wendy/go/internal/agent/meshsharing"
	"github.com/wendylabsinc/wendy/go/internal/cli/sessionbroker"
	"github.com/wendylabsinc/wendy/go/macos/vpn"
)

func TestAutomaticVPNTruncatedDirectoryWithholdsFriendlyAliases(t *testing.T) {
	const tenant = "11111111-1111-4111-8111-111111111111"
	const first = "22222222-2222-4222-8222-222222222222"
	const second = "33333333-3333-4333-8333-333333333333"
	device := func(id, name string) sessionbroker.OperatorTargetInfo {
		return sessionbroker.OperatorTargetInfo{Selector: "mesh://" + tenant + "/" + id,
			Principal: "spiffe://wendy.sh/tenant/" + tenant + "/device/" + id, Name: name}
	}
	a, controller, err := newAutomaticVPN()
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	var snapshotMu sync.Mutex
	var snapshot sessionbroker.OperatorDirectory
	a.discover = func(context.Context) (sessionbroker.OperatorDirectory, error) {
		snapshotMu.Lock()
		defer snapshotMu.Unlock()
		return snapshot, nil
	}
	answer := func(name string, routes []vpn.Route) *dns.Msg {
		t.Helper()
		query := new(dns.Msg)
		query.SetQuestion(dns.Fqdn(name), dns.TypeA)
		wire, err := query.Pack()
		if err != nil {
			t.Fatal(err)
		}
		wire, err = vpn.AnswerDNS(wire, routes)
		if err != nil {
			t.Fatal(err)
		}
		reply := new(dns.Msg)
		if err := reply.Unpack(wire); err != nil {
			t.Fatal(err)
		}
		return reply
	}
	var firstAddress string
	for _, phase := range []struct {
		name      string
		truncated bool
		devices   []sessionbroker.OperatorTargetInfo
		friendly  bool
	}{
		{"initial partial", true, []sessionbroker.OperatorTargetInfo{device(first, "jetson")}, false},
		{"complete unique", false, []sessionbroker.OperatorTargetInfo{device(first, "jetson")}, true},
		{"partial after complete", true, []sessionbroker.OperatorTargetInfo{device(first, "jetson")}, false},
		{"complete with omitted duplicate", false, []sessionbroker.OperatorTargetInfo{device(first, "jetson"), device(second, "JETSON")}, false},
		{"complete after rename", false, []sessionbroker.OperatorTargetInfo{device(first, "jetson"), device(second, "camera")}, true},
	} {
		t.Run(phase.name, func(t *testing.T) {
			snapshotMu.Lock()
			snapshot = sessionbroker.OperatorDirectory{State: "ready", Truncated: phase.truncated, Devices: phase.devices}
			snapshotMu.Unlock()
			cfg, err := vpn.Refresh(context.Background(), controller, true)
			if err != nil {
				t.Fatal(err)
			}
			reply := answer("jetson.wendy.internal", cfg.Routes)
			if phase.friendly {
				if reply.Rcode != dns.RcodeSuccess || len(reply.Answer) != 1 {
					t.Fatalf("complete unique name did not recover: %s", reply)
				}
			} else if reply.Rcode != dns.RcodeNameError || len(reply.Answer) != 0 {
				t.Fatalf("incomplete or conflicting name became DNS authority: %s", reply)
			}
			for _, device := range phase.devices {
				id := first
				if device.Selector != "mesh://"+tenant+"/"+first {
					id = second
				}
				reply := answer("device-"+id+".wendy.internal", cfg.Routes)
				if reply.Rcode != dns.RcodeSuccess || len(reply.Answer) != 1 {
					t.Fatalf("verified UUID route lost: %s", reply)
				}
				if id == first {
					address := reply.Answer[0].(*dns.A).A.String()
					if firstAddress != "" && address != firstAddress {
						t.Fatal("snapshot completeness changed the stable UUID address")
					}
					firstAddress = address
				}
			}
		})
	}
}

func TestAutomaticVPNRoutesAllDevicesAndRecoversInternet(t *testing.T) {
	a, controller, err := newAutomaticVPN()
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	devices := []sessionbroker.OperatorTargetInfo{
		{Selector: "mesh:far", Name: "Far", Metric: 100, InternetSharing: true},
		{Selector: "mesh:near", Name: "Near", Metric: 10, InternetSharing: true},
		{Selector: "mesh:device", Name: "Device"},
	}
	a.discover = func(context.Context) (sessionbroker.OperatorDirectory, error) {
		return sessionbroker.OperatorDirectory{State: "ready", Devices: devices}, nil
	}
	var attempted []string
	a.open = func(_ context.Context, selector string) (*vpn.Internet, error) {
		attempted = append(attempted, selector)
		left, right := net.Pipe()
		t.Cleanup(func() { left.Close(); right.Close() })
		return &vpn.Internet{Stream: left, Config: meshsharing.InternetConfig{Address4: "10.88.1.1", MTU: 1280}, DNS: func(context.Context) (net.Conn, error) { return nil, errors.New("unused") }}, nil
	}
	refresh := func(online bool) vpn.Config {
		t.Helper()
		cfg, err := vpn.Refresh(context.Background(), controller, online)
		if err != nil {
			t.Fatal(err)
		}
		return cfg
	}
	normal := refresh(true)
	if len(normal.Routes) != 3 || normal.Internet != nil || len(attempted) != 0 {
		t.Fatalf("normal connection changed: %+v %v", normal, attempted)
	}
	backup := refresh(false)
	if backup.Internet == nil || !reflect.DeepEqual(attempted, []string{"mesh:near"}) {
		t.Fatalf("lowest route cost not selected: %v", attempted)
	}
	stable := refresh(false)
	if stable.Endpoint != backup.Endpoint {
		t.Fatal("working donor unnecessarily replaced")
	}
	a.internet.Close()
	alternative := refresh(false)
	if alternative.Internet == nil || a.gateway != "mesh:far" {
		t.Fatal("failed donor did not move to next candidate")
	}
	recovered := refresh(true)
	if recovered.Internet != nil || len(recovered.Routes) != 3 {
		t.Fatal("normal recovery lost device routes or kept default route")
	}
	devices = append(devices, sessionbroker.OperatorTargetInfo{Selector: "mesh:new", Name: "New"})
	added := refresh(true)
	if len(added.Routes) != 4 {
		t.Fatal("new device was not added automatically")
	}
	// Failed discovery must not remove the existing mesh device routes.
	a.discover = func(context.Context) (sessionbroker.OperatorDirectory, error) {
		return sessionbroker.OperatorDirectory{}, errors.New("temporary")
	}
	if refresh(true).Endpoint != added.Endpoint {
		t.Fatal("transient discovery interrupted device routes")
	}
}

func TestAutomaticVPNWithoutUplinkKeepsDeviceRoutes(t *testing.T) {
	a, _, err := newAutomaticVPN()
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	a.discover = func(context.Context) (sessionbroker.OperatorDirectory, error) {
		return sessionbroker.OperatorDirectory{State: "ready", Devices: []sessionbroker.OperatorTargetInfo{{Selector: "mesh:device"}}}, nil
	}
	a.open = func(context.Context, string) (*vpn.Internet, error) {
		t.Fatal("unadvertised uplink attempted")
		return nil, nil
	}
	cfg, err := a.refresh(context.Background(), false)
	if err != nil || len(cfg.Routes) != 1 || cfg.Internet != nil {
		t.Fatalf("device routing requires internet: %+v %v", cfg, err)
	}
}

func TestAutomaticVPNAllowsSlowBluetoothLease(t *testing.T) {
	a, controller, err := newAutomaticVPN()
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	a.discover = func(context.Context) (sessionbroker.OperatorDirectory, error) {
		return sessionbroker.OperatorDirectory{State: "ready", Devices: []sessionbroker.OperatorTargetInfo{{Selector: "mesh:bluetooth", InternetSharing: true}}}, nil
	}
	a.open = func(ctx context.Context, _ string) (*vpn.Internet, error) {
		timer := time.NewTimer(4200 * time.Millisecond)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timer.C:
		}
		left, right := net.Pipe()
		t.Cleanup(func() { left.Close(); right.Close() })
		return &vpn.Internet{Stream: left, Config: meshsharing.InternetConfig{Address4: "10.88.1.1", MTU: 1280}, DNS: func(context.Context) (net.Conn, error) { return nil, errors.New("unused") }}, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cfg, err := vpn.Refresh(ctx, controller, false)
	if err != nil || cfg.Internet == nil {
		t.Fatalf("slow Bluetooth handshake was discarded: %+v %v", cfg, err)
	}
}
