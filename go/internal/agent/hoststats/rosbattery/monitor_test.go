package rosbattery

import (
	"context"
	"errors"
	"github.com/wendylabsinc/wendy/go/internal/rtps"
	"net"
	"slices"
	"testing"
	"time"
)

func TestEligibleInterfaces(t *testing.T) {
	const up = net.FlagUp | net.FlagMulticast

	tests := []struct {
		name   string
		ifaces []candidateIface
		want   []string
	}{
		{
			name: "wired interface is eligible",
			ifaces: []candidateIface{
				{Name: "eth0", Flags: up, HasIPv4: true},
			},
			want: []string{"eth0"},
		},
		{
			// The motivating case: a Jetson whose only routable interface is
			// WiFi must yield nothing, so the monitor never announces SPDP onto
			// somebody's office network.
			name: "wireless-only host has no candidates",
			ifaces: []candidateIface{
				{Name: "wlan0", Flags: up, HasIPv4: true},
			},
			want: nil,
		},
		{
			name: "wireless is dropped alongside wired",
			ifaces: []candidateIface{
				{Name: "wlan0", Flags: up, HasIPv4: true},
				{Name: "eth0", Flags: up, HasIPv4: true},
				{Name: "wlp2s0", Flags: up, HasIPv4: true},
				{Name: "wifi0", Flags: up, HasIPv4: true},
			},
			want: []string{"eth0"},
		},
		{
			name: "virtual interfaces are dropped",
			ifaces: []candidateIface{
				{Name: "docker0", Flags: up, HasIPv4: true},
				{Name: "veth1a2b", Flags: up, HasIPv4: true},
				{Name: "br-abc123", Flags: up, HasIPv4: true},
				{Name: "enp3s0", Flags: up, HasIPv4: true},
			},
			want: []string{"enp3s0"},
		},
		{
			name: "down, loopback, non-multicast and IPv6-only are dropped",
			ifaces: []candidateIface{
				{Name: "eth0", Flags: net.FlagMulticast, HasIPv4: true},
				{Name: "lo", Flags: up | net.FlagLoopback, HasIPv4: true},
				{Name: "eth1", Flags: net.FlagUp, HasIPv4: true},
				{Name: "eth2", Flags: up, HasIPv4: false},
				{Name: "eth3", Flags: up, HasIPv4: true},
			},
			want: []string{"eth3"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := eligibleInterfaces(tc.ifaces)
			if !slices.Equal(got, tc.want) {
				t.Errorf("eligibleInterfaces() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestIsWireless(t *testing.T) {
	wireless := []string{"wlan0", "wlp2s0", "wl0", "wifi0", "ath0", "ra0", "WLAN0"}
	for _, name := range wireless {
		if !isWireless(name) {
			t.Errorf("isWireless(%q) = false, want true", name)
		}
	}
	// "eno1" and "enp3s0" both start with "en", which must not be confused with
	// the "ra" and "wl" wireless prefixes.
	wired := []string{"eth0", "eno1", "enp3s0", "end0", "usb0", "rndis0"}
	for _, name := range wired {
		if isWireless(name) {
			t.Errorf("isWireless(%q) = true, want false", name)
		}
	}
}

func TestMoveToFront(t *testing.T) {
	tests := []struct {
		name  string
		names []string
		want  string
		out   []string
	}{
		{"present", []string{"eth0", "eth1", "eth2"}, "eth1", []string{"eth1", "eth0", "eth2"}},
		{"already first", []string{"eth0", "eth1"}, "eth0", []string{"eth0", "eth1"}},
		{"absent leaves order", []string{"eth0", "eth1"}, "eth9", []string{"eth0", "eth1"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := moveToFront(tc.names, tc.want); !slices.Equal(got, tc.out) {
				t.Errorf("moveToFront(%v, %q) = %v, want %v", tc.names, tc.want, got, tc.out)
			}
		})
	}
}

func TestExplicitWirelessBatteryInterfaceOverridesAutomaticPolicy(t *testing.T) {
	pool := rtps.NewPool()
	defer pool.Close()
	m := NewMonitor(Config{Enabled: true, Interfaces: []string{"wlan0"}, DomainID: 7}, NewCache(time.Now), pool, nil)
	var targets []rtps.Config
	m.acquire = func(_ context.Context, cfg rtps.Config) (*rtps.Lease, error) {
		targets = append(targets, cfg)
		return nil, errors.New("no publisher")
	}
	m.scanAndSubscribe(context.Background())
	if len(targets) != 1 || targets[0].Interface != "wlan0" || targets[0].DomainID != 7 || targets[0].NetworkNamespacePID != 0 {
		t.Fatalf("targets = %+v", targets)
	}
}
