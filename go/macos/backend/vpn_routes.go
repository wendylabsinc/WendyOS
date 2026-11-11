package backend

import (
	"context"
	"net"
	"strings"

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
	for i := range routes {
		if strings.HasPrefix(routes[i].Selector, "cloud://") {
			routes[i].Principal = ""
			selectors = append(selectors, routes[i].Selector)
		}
	}
	if len(selectors) == 0 {
		return routes, nil
	}
	identities, err := lookup(ctx, selectors)
	if err != nil {
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
		return desktopDialExpected(ctx, selector, port, principals[selector])
	}
}
