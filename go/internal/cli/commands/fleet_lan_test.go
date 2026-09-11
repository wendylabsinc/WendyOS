package commands

import (
	"encoding/json"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
)

func dev(hostname, display, ip string) models.LANDevice {
	return models.LANDevice{Hostname: hostname, DisplayName: display, IPAddress: ip, IsWendyDevice: true}
}

func TestDeviceShortName(t *testing.T) {
	cases := []struct{ host, display, want string }{
		{"wendyos-camera-01.local", "Camera 01", "camera-01"},
		{"wendyos-camera-01.local.", "", "camera-01"},
		{"WENDYOS-Thor.local", "", "thor"},
		{"", "Camera 02", "camera-02"},
	}
	for _, c := range cases {
		if got := deviceShortName(dev(c.host, c.display, "")); got != c.want {
			t.Errorf("deviceShortName(%q,%q) = %q, want %q", c.host, c.display, got, c.want)
		}
	}
}

func TestMatchesGroupPattern(t *testing.T) {
	d := dev("wendyos-camera-01.local", "Camera 01", "")
	cases := []struct {
		pattern string
		want    bool
	}{
		{"", true},
		{"*", true},
		{"all", true},
		{"camera", true},     // token prefix "camera-"
		{"camera-*", true},   // glob
		{"camera-01", true},  // exact
		{"camera-0?", true},  // glob
		{"cameras", false},   // no "cameras-" prefix, not exact
		{"cam", false},       // not a "<token>-" boundary
		{"thor", false},      // different device
		{"camera-02", false}, // different unit
	}
	for _, c := range cases {
		if got := matchesGroupPattern(d, c.pattern); got != c.want {
			t.Errorf("matchesGroupPattern(camera-01, %q) = %v, want %v", c.pattern, got, c.want)
		}
	}
}

func TestPeerHostPrefersIP(t *testing.T) {
	if got := peerHost(dev("wendyos-camera-01.local", "", "10.0.0.4")); got != "10.0.0.4" {
		t.Errorf("peerHost with IP = %q, want 10.0.0.4", got)
	}
	if got := peerHost(dev("wendyos-camera-01.local.", "", "")); got != "wendyos-camera-01.local" {
		t.Errorf("peerHost without IP = %q, want wendyos-camera-01.local", got)
	}
}

func TestComputePeers(t *testing.T) {
	comp := &appconfig.ComponentConfig{
		Tags:   []string{"camera-*"},
		Expose: &appconfig.ComponentExpose{Port: 8000, Path: "/stream"},
	}
	peers := computePeers(comp, []meshPeer{
		{Name: "camera-01", AssetID: 42, Host: "10.0.0.4"},
		{Name: "camera-02", AssetID: 0, Host: "wendyos-camera-02.local"},
	})
	if len(peers) != 2 {
		t.Fatalf("computePeers returned %d peers, want 2", len(peers))
	}
	// Known asset id -> mesh name (asset id wins over the LAN host).
	if peers[0].URL != "http://device-42.mesh.wendy.internal:8000" {
		t.Errorf("peer[0].URL = %q", peers[0].URL)
	}
	// Unknown asset id -> direct-LAN host fallback.
	if peers[1].URL != "http://wendyos-camera-02.local:8000" {
		t.Errorf("peer[1].URL = %q", peers[1].URL)
	}
	if peers[0].Name != "camera-01" || peers[0].Group != "camera-*" || peers[0].Status != "ready" {
		t.Errorf("peer[0] = %+v", peers[0])
	}
}

func TestDiscoveryEnv(t *testing.T) {
	manifest := &appconfig.FleetManifest{
		AppID: "sh.wendy.fleet",
		Components: map[string]*appconfig.ComponentConfig{
			"camera": {
				Path:   "camera",
				Tags:   []string{"camera-*"},
				Expose: &appconfig.ComponentExpose{Port: 8000, Path: "/stream"},
			},
			"dashboard": {
				Path:      "dashboard",
				Tags:      []string{"central"},
				Discovers: []appconfig.DiscoverRef{{Component: "camera", As: "WENDY_FLEET_PEERS"}},
			},
		},
	}
	matched := map[string][]meshPeer{
		"camera": {{Name: "camera-01", AssetID: 42}},
	}
	env, err := discoveryEnv(manifest.Components["dashboard"], manifest, matched)
	if err != nil {
		t.Fatalf("discoveryEnv error: %v", err)
	}
	if len(env) != 1 {
		t.Fatalf("discoveryEnv returned %d entries, want 1", len(env))
	}
	const prefix = "WENDY_FLEET_PEERS="
	if len(env[0]) <= len(prefix) || env[0][:len(prefix)] != prefix {
		t.Fatalf("env entry %q does not start with %q", env[0], prefix)
	}
	var peers []fleetPeer
	if err := json.Unmarshal([]byte(env[0][len(prefix):]), &peers); err != nil {
		t.Fatalf("env value is not valid JSON: %v", err)
	}
	if len(peers) != 1 || peers[0].URL != "http://device-42.mesh.wendy.internal:8000" {
		t.Errorf("peers = %+v", peers)
	}
}

func TestDiscoveryEnvErrors(t *testing.T) {
	manifest := &appconfig.FleetManifest{
		Components: map[string]*appconfig.ComponentConfig{
			"noexpose": {Path: "x", Tags: []string{"g"}},
		},
	}
	// Unknown referenced component.
	consumer := &appconfig.ComponentConfig{Discovers: []appconfig.DiscoverRef{{Component: "missing", As: "X"}}}
	if _, err := discoveryEnv(consumer, manifest, nil); err == nil {
		t.Error("expected error for unknown discovered component")
	}
	// Referenced component without an expose endpoint.
	consumer = &appconfig.ComponentConfig{Discovers: []appconfig.DiscoverRef{{Component: "noexpose", As: "X"}}}
	if _, err := discoveryEnv(consumer, manifest, nil); err == nil {
		t.Error("expected error for discovered component without expose")
	}
}

func TestShellQuoteEnv(t *testing.T) {
	if got := shellQuoteEnv(`FOO=[{"a":1}]`); got != `FOO='[{"a":1}]'` {
		t.Errorf("shellQuoteEnv = %q", got)
	}
	if got := shellQuoteEnv("noequals"); got != "noequals" {
		t.Errorf("shellQuoteEnv(noequals) = %q", got)
	}
}

func TestValidateGroupPattern(t *testing.T) {
	for _, ok := range []string{"camera", "camera-*", "camera-0?", "grp_1", "a.b-c"} {
		if err := validateGroupPattern(ok); err != nil {
			t.Errorf("validateGroupPattern(%q) unexpected error: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "-leading", "has space", "weird$"} {
		if err := validateGroupPattern(bad); err == nil {
			t.Errorf("validateGroupPattern(%q) expected error", bad)
		}
	}
}
