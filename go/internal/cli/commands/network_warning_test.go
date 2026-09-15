package commands

import (
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
)

func TestMissingNetworkWarnings(t *testing.T) {
	for _, mode := range []string{"host", "bridge", "none", ""} {
		cfg := &appconfig.AppConfig{AppID: "app", Entitlements: []appconfig.Entitlement{{Type: "network", Mode: mode}}}
		if got := missingNetworkWarnings([]*appconfig.AppConfig{cfg}); len(got) != 0 {
			t.Errorf("explicit %q: %v", mode, got)
		}
	}
	cfg := &appconfig.AppConfig{AppID: "app", ServiceName: "api"}
	warnings := missingNetworkWarnings([]*appconfig.AppConfig{cfg})
	if len(warnings) != 1 {
		t.Fatalf("warnings = %v", warnings)
	}
	for _, text := range []string{"app_api", "loopback", "host", "bridge", "none", "LAN"} {
		if !strings.Contains(warnings[0], text) {
			t.Errorf("missing %q: %s", text, warnings[0])
		}
	}
	cfg.Isolation = "isolated"
	if got := missingNetworkWarnings([]*appconfig.AppConfig{cfg}); len(got) != 0 {
		t.Fatalf("isolated CNI group: %v", got)
	}
}

func TestMissingNetworkWarningsSharedPrimary(t *testing.T) {
	app := &appconfig.AppConfig{AppID: "app", Isolation: "shared-network"}
	primary := multiServiceCreateConfig(app, "primary", &appconfig.ServiceConfig{})
	secondary := multiServiceCreateConfig(app, "secondary", &appconfig.ServiceConfig{Entitlements: []appconfig.Entitlement{{Type: "network", Mode: "host"}}})
	warnings := missingNetworkWarnings([]*appconfig.AppConfig{primary, secondary})
	if len(warnings) != 2 || !strings.Contains(warnings[1], "inherits primary") {
		t.Fatalf("warnings = %v", warnings)
	}
	app.Entitlements = []appconfig.Entitlement{{Type: "network", Mode: "none"}}
	primary = multiServiceCreateConfig(app, "primary", &appconfig.ServiceConfig{})
	if got := missingNetworkWarnings([]*appconfig.AppConfig{primary, secondary}); len(got) != 0 {
		t.Fatalf("explicit primary none: %v", got)
	}
}
