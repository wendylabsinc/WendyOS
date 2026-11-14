package backend

import (
	"context"
	"errors"
	"net"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/commands"
	"github.com/wendylabsinc/wendy/go/macos/vpn"
)

// PrepareVPNRoutes replaces caller-provided Cloud metadata with the selected
// organization's authenticated inventory. The same refresh runs in the mobile
// extension when it restores saved routing intent.
func PrepareVPNRoutes(ctx context.Context, input []vpn.Route) ([]vpn.Route, error) {
	return prepareVPNRoutes(ctx, input, commands.DesktopCloudRouteIdentities)
}

func prepareVPNRoutes(ctx context.Context, input []vpn.Route, lookup func(context.Context, []string) (map[string]commands.DesktopCloudRouteIdentity, error)) ([]vpn.Route, error) {
	routes := append([]vpn.Route(nil), input...)
	var selectors []string
	hasMesh := false
	for i := range routes {
		if strings.HasPrefix(routes[i].Selector, "cloud://") {
			routes[i].Principal = ""
			selectors = append(selectors, routes[i].Selector)
		} else if strings.HasPrefix(routes[i].Selector, "mesh:") {
			hasMesh = true
		}
	}
	if len(selectors) == 0 {
		return routes, nil
	}
	// A mixed configuration must still work offline. Bound inventory refresh
	// separately; retain route selectors/addresses so retrying does not recycle
	// VIPs. Missing principals withhold private aliases and Cloud forwarding.
	lookupCtx := ctx
	if hasMesh {
		var cancel context.CancelFunc
		lookupCtx, cancel = context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
	}
	identities, err := lookup(lookupCtx, selectors)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil {
		if hasMesh {
			return routes, nil
		}
		return nil, err
	}
	for i := range routes {
		if identity, ok := identities[routes[i].Selector]; ok {
			routes[i].Principal = identity.Principal
			routes[i].Name = identity.Name
		}
	}
	return routes, nil
}

func desktopRouteDial(routes []vpn.Route) vpn.DialFunc {
	principals := make(map[string]string, len(routes))
	for _, route := range routes {
		principals[route.Selector] = route.Principal
	}
	return func(ctx context.Context, selector string, port uint16) (net.Conn, error) {
		if strings.HasPrefix(selector, "cloud://") && strings.Contains(selector, "/tenant/") && principals[selector] == "" {
			return nil, errors.New("Cloud route identity is unavailable; reconnect the VPN when Cloud inventory is reachable")
		}
		return desktopDialExpected(ctx, selector, port, principals[selector])
	}
}
