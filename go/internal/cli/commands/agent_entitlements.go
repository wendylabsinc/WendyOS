package commands

import (
	"slices"
	"strconv"
	"strings"

	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// checkAgentEntitlements refuses a project whose wendy.json uses an
// entitlement type the device's agent does not list. An agent ignores a type
// it does not know and starts the app without that access, so the app would
// fail later in its own way (WDY-4049). cfgs are the app's configs: one, or
// one per service. Agents that predate the list report none and are not
// checked.
func checkAgentEntitlements(resp *agentpb.GetAgentVersionResponse, cfgs ...*appconfig.AppConfig) error {
	known := resp.GetEntitlementTypes()
	if len(known) == 0 {
		return nil
	}
	var unknown []string
	for _, cfg := range cfgs {
		for _, e := range cfg.Entitlements {
			if !slices.Contains(known, e.Type) && !slices.Contains(unknown, e.Type) {
				unknown = append(unknown, e.Type)
			}
		}
	}
	if len(unknown) == 0 {
		return nil
	}
	slices.Sort(unknown)
	quoted := make([]string, len(unknown))
	for i, t := range unknown {
		quoted[i] = strconv.Quote(t)
	}
	noun := "entitlement"
	if len(unknown) > 1 {
		noun = "entitlements"
	}
	return withNextSteps(commandErrorf(errProjectTargetMismatch,
		"the device's agent does not support the %s %s in wendy.json (agent version %s); it would start the app without that access",
		strings.Join(quoted, ", "), noun, resp.GetVersion()),
		"Update the device: wendy device update",
		"Or remove the entitlement from wendy.json if the app does not need it.")
}
