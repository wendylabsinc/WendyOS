package commands

import (
	"fmt"
	"strings"

	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
)

// missingNetworkWarnings receives the exact create configs in startup order.
// A shared-namespace secondary uses the primary's network, even when its own
// configuration declares different networking. Explicit none is intentional.
func missingNetworkWarnings(configs []*appconfig.AppConfig) []string {
	var warnings []string
	for i, cfg := range configs {
		if cfg == nil {
			continue
		}
		effective := cfg
		if i > 0 && appconfig.IsSharedNamespaceIsolation(cfg.Isolation) {
			effective = configs[0]
		}
		if effective == nil {
			continue
		}
		declared := false
		for _, entitlement := range effective.Entitlements {
			if entitlement.Type == appconfig.EntitlementNetwork {
				declared = true
				break
			}
		}
		// Isolated service groups receive a CNI bridge even without a network
		// entitlement. Report only services that actually get loopback alone.
		if declared || (effective.Isolation == "isolated" && effective.ServiceName != "") {
			continue
		}
		name := cfg.ContainerName()
		inherited := ""
		if effective != cfg {
			inherited = fmt.Sprintf(" (inherits primary service %q's network namespace)", effective.ServiceName)
		}
		warnings = append(warnings, fmt.Sprintf("service %q has no network entitlement and will receive only loopback%s. Declare {\"type\":\"network\",\"mode\":\"host\"} for LAN access, \"bridge\" for outbound DNS/downloads without LAN port publishing, or \"none\" for intentional offline operation.", name, inherited))
	}
	return warnings
}

func printMissingNetworkWarnings(configs ...*appconfig.AppConfig) {
	for _, warning := range missingNetworkWarnings(configs) {
		cliLogln("Warning: %s", warning)
	}
}

func isContainerPlatform(platform string) bool {
	return strings.HasPrefix(platform, "linux") || strings.HasPrefix(platform, "wendyos")
}
