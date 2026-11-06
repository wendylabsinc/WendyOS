package containerd

import (
	"reflect"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
)

func TestFindBridgeEntitlement(t *testing.T) {
	tests := []struct {
		name         string
		entitlements []appconfig.Entitlement
		wantFound    bool
		wantEnt      appconfig.Entitlement
	}{
		{
			name:         "no entitlements",
			entitlements: nil,
			wantFound:    false,
		},
		{
			name: "bridge mode present",
			entitlements: []appconfig.Entitlement{
				{Type: appconfig.EntitlementNetwork, Mode: "bridge"},
			},
			wantFound: true,
			wantEnt:   appconfig.Entitlement{Type: appconfig.EntitlementNetwork, Mode: "bridge"},
		},
		{
			name: "host mode is not bridge",
			entitlements: []appconfig.Entitlement{
				{Type: appconfig.EntitlementNetwork, Mode: "host"},
			},
			wantFound: false,
		},
		{
			name: "none mode is not bridge",
			entitlements: []appconfig.Entitlement{
				{Type: appconfig.EntitlementNetwork, Mode: "none"},
			},
			wantFound: false,
		},
		{
			name: "mesh mode is not bridge",
			entitlements: []appconfig.Entitlement{
				{Type: appconfig.EntitlementNetwork, Mode: "mesh", ServiceCIDR: "10.99.0.0/16"},
			},
			wantFound: false,
		},
		{
			name: "unrelated entitlements only",
			entitlements: []appconfig.Entitlement{
				{Type: appconfig.EntitlementBluetooth},
				{Type: appconfig.EntitlementGPU},
			},
			wantFound: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ent, ok := findBridgeEntitlement(tt.entitlements)
			if ok != tt.wantFound {
				t.Fatalf("findBridgeEntitlement() ok = %v, want %v", ok, tt.wantFound)
			}
			if ok && !reflect.DeepEqual(ent, tt.wantEnt) {
				t.Errorf("findBridgeEntitlement() ent = %+v, want %+v", ent, tt.wantEnt)
			}
		})
	}
}

// TestNeedsCNIBridgeWiring verifies the single predicate that gates CNI
// ADD/DEL: multi-service isolated app services and single-service bridge-mode
// apps are selected; host, none, and non-isolated multi-service apps are not
// (specs/2026-07-05-network-bridge-default-design.md, "Testing").
func TestNeedsCNIBridgeWiring(t *testing.T) {
	bridgeEnt := []appconfig.Entitlement{{Type: appconfig.EntitlementNetwork, Mode: "bridge"}}
	hostEnt := []appconfig.Entitlement{{Type: appconfig.EntitlementNetwork, Mode: "host"}}
	noneEnt := []appconfig.Entitlement{{Type: appconfig.EntitlementNetwork, Mode: "none"}}
	meshEnt := []appconfig.Entitlement{{Type: appconfig.EntitlementNetwork, Mode: "mesh", ServiceCIDR: "10.99.0.0/16"}}

	tests := []struct {
		name         string
		isolation    string
		serviceName  string
		entitlements []appconfig.Entitlement
		want         bool
	}{
		{
			name:        "multi-service isolated app service",
			isolation:   "isolated",
			serviceName: "api",
			want:        true,
		},
		{
			name:         "multi-service isolated app service with mesh entitlement",
			isolation:    "isolated",
			serviceName:  "api",
			entitlements: meshEnt,
			want:         true,
		},
		{
			name:         "single-service bridge-mode app",
			isolation:    "",
			serviceName:  "",
			entitlements: bridgeEnt,
			want:         true,
		},
		{
			name:         "single-service host-mode app excluded",
			isolation:    "",
			serviceName:  "",
			entitlements: hostEnt,
			want:         false,
		},
		{
			name:         "single-service none-mode app excluded",
			isolation:    "",
			serviceName:  "",
			entitlements: noneEnt,
			want:         false,
		},
		{
			name:        "multi-service shared-net (non-isolated) app excluded",
			isolation:   "shared-net",
			serviceName: "api",
			want:        false,
		},
		{
			name:        "isolated app with no serviceName (single-container isolated) excluded",
			isolation:   "isolated",
			serviceName: "",
			want:        false,
		},
		{
			name:         "single-service isolated mesh app (WDY-1853)",
			isolation:    "isolated",
			serviceName:  "",
			entitlements: meshEnt,
			want:         true,
		},
		{
			name:         "mesh without isolation excluded",
			isolation:    "",
			serviceName:  "",
			entitlements: meshEnt,
			want:         false,
		},
		{
			name:         "no isolation, no serviceName, no entitlements excluded",
			isolation:    "",
			serviceName:  "",
			entitlements: nil,
			want:         false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := needsCNIBridgeWiring(tt.isolation, tt.serviceName, tt.entitlements)
			if got != tt.want {
				t.Errorf("needsCNIBridgeWiring(%q, %q, %+v) = %v, want %v",
					tt.isolation, tt.serviceName, tt.entitlements, got, tt.want)
			}
		})
	}
}

func TestNeedsGatewayDNS(t *testing.T) {
	bridgeEnt := []appconfig.Entitlement{{Type: appconfig.EntitlementNetwork, Mode: "bridge"}}
	meshEnt := []appconfig.Entitlement{{Type: appconfig.EntitlementNetwork, Mode: "mesh", ServiceCIDR: "10.99.0.0/16"}}
	hostEnt := []appconfig.Entitlement{{Type: appconfig.EntitlementNetwork, Mode: "host"}}

	tests := []struct {
		name         string
		isolation    string
		entitlements []appconfig.Entitlement
		want         bool
	}{
		{
			name:         "bridge mode always gets gateway DNS",
			isolation:    "",
			entitlements: bridgeEnt,
			want:         true,
		},
		{
			name:         "mesh entitlement with isolated app gets gateway DNS",
			isolation:    "isolated",
			entitlements: meshEnt,
			want:         true,
		},
		{
			name:         "mesh entitlement without isolated app does not get gateway DNS",
			isolation:    "",
			entitlements: meshEnt,
			want:         false,
		},
		{
			name:         "host mode does not get gateway DNS",
			isolation:    "isolated",
			entitlements: hostEnt,
			want:         false,
		},
		{
			name:         "no entitlements does not get gateway DNS",
			isolation:    "isolated",
			entitlements: nil,
			want:         false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := needsGatewayDNS(tt.isolation, tt.entitlements)
			if got != tt.want {
				t.Errorf("needsGatewayDNS(%q, %+v) = %v, want %v", tt.isolation, tt.entitlements, got, tt.want)
			}
		})
	}
}

// TestHasImplicitHostNetworkMode covers the deprecation-warning predicate:
// it fires only for a network entitlement with an omitted/empty mode, not for
// any explicit mode, and not for apps without a network entitlement at all.
func TestHasImplicitHostNetworkMode(t *testing.T) {
	tests := []struct {
		name         string
		entitlements []appconfig.Entitlement
		want         bool
	}{
		{
			name:         "omitted mode fires the warning",
			entitlements: []appconfig.Entitlement{{Type: appconfig.EntitlementNetwork, Mode: ""}},
			want:         true,
		},
		{
			name:         "explicit host mode does not fire",
			entitlements: []appconfig.Entitlement{{Type: appconfig.EntitlementNetwork, Mode: "host"}},
			want:         false,
		},
		{
			name:         "explicit host-admin mode does not fire",
			entitlements: []appconfig.Entitlement{{Type: appconfig.EntitlementNetwork, Mode: "host-admin"}},
			want:         false,
		},
		{
			name:         "explicit bridge mode does not fire",
			entitlements: []appconfig.Entitlement{{Type: appconfig.EntitlementNetwork, Mode: "bridge"}},
			want:         false,
		},
		{
			name:         "explicit mesh mode does not fire",
			entitlements: []appconfig.Entitlement{{Type: appconfig.EntitlementNetwork, Mode: "mesh", ServiceCIDR: "10.99.0.0/16"}},
			want:         false,
		},
		{
			name:         "explicit none mode does not fire",
			entitlements: []appconfig.Entitlement{{Type: appconfig.EntitlementNetwork, Mode: "none"}},
			want:         false,
		},
		{
			name:         "no network entitlement at all does not fire",
			entitlements: []appconfig.Entitlement{{Type: appconfig.EntitlementGPU}},
			want:         false,
		},
		{
			name:         "no entitlements does not fire",
			entitlements: nil,
			want:         false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := hasImplicitHostNetworkMode(tt.entitlements)
			if got != tt.want {
				t.Errorf("hasImplicitHostNetworkMode(%+v) = %v, want %v", tt.entitlements, got, tt.want)
			}
		})
	}
}

func TestInternetDefaultRouteEntitlement(t *testing.T) {
	for _, tc := range []struct {
		modes []string
		want  bool
	}{
		{[]string{"bridge"}, true}, {[]string{"mesh"}, false},
		{[]string{"bridge", "mesh"}, false}, {[]string{"host"}, false},
		{[]string{"none"}, false}, {[]string{}, false},
	} {
		var ents []appconfig.Entitlement
		for _, mode := range tc.modes {
			ents = append(ents, appconfig.Entitlement{Type: appconfig.EntitlementNetwork, Mode: mode})
		}
		if got := needsInternetDefaultRoute(ents); got != tc.want {
			t.Errorf("modes %v: Internet route=%v, want %v", tc.modes, got, tc.want)
		}
	}
}
