package backend

import (
	"context"
	"errors"
	"net"
	"reflect"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/meshsharing"
	"github.com/wendylabsinc/wendy/go/internal/cli/sessionbroker"
	"github.com/wendylabsinc/wendy/go/macos/vpn"
)

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
