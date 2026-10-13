package containerd

import (
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
)

// findBridgeEntitlement returns the network entitlement with mode "bridge"
// from entitlements, if one is present. ok == false means no bridge-mode
// wiring should happen for this container — callers must treat that as a
// complete no-op, mirroring findMeshEntitlement.
func findBridgeEntitlement(entitlements []appconfig.Entitlement) (ent appconfig.Entitlement, ok bool) {
	for _, e := range entitlements {
		if e.Type == appconfig.EntitlementNetwork && e.Mode == "bridge" {
			return e, true
		}
	}
	return appconfig.Entitlement{}, false
}

// needsCNIBridgeWiring is the single predicate that decides whether a
// container's networking requires attachment to the per-app CNI bridge (ADD
// on start, DEL on stop): a service within a multi-service isolated app group
// (isolation == "isolated" && serviceName != ""), which relies on the bridge
// for its CNI-assigned IP and cross-service /etc/hosts resolution; a
// single-service isolated app with a "mesh" entitlement, whose mesh
// reachability (ingress DNAT + egress route + gateway DNS) all hangs off the
// per-app bridge/netns; or a single-service app whose network entitlement mode
// is "bridge" (isolated namespace + NAT egress, no /etc/hosts).
//
// Both StartContainer (CNI ADD) and stopOne/deleteOne (CNI DEL) call this
// exact function so the two sides of the wiring can never drift apart (see
// specs/2026-07-05-network-bridge-default-design.md, "factor the shared
// block"). Host, none, mesh-without-isolation, and apps with none of these
// conditions all return false. This stays aligned with needsGatewayDNS, which
// already recognises the single-service isolated mesh case (WDY-1853).
func needsCNIBridgeWiring(isolation, serviceName string, entitlements []appconfig.Entitlement) bool {
	if isolation == "isolated" && serviceName != "" {
		return true
	}
	// A single-service isolated app with a "mesh" entitlement (top-level
	// entitlements, serviceName == "") still needs the bridge. Without this it
	// deploys and runs but is silently unreachable over the mesh — no netns, no
	// ingress DNAT (WDY-1853). Gated on isolation because mesh requires the
	// container's own network namespace.
	if isolation == "isolated" {
		if _, ok := findMeshEntitlement(entitlements); ok {
			return true
		}
	}
	_, ok := findBridgeEntitlement(entitlements)
	return ok
}

// needsGatewayDNS reports whether a container should get the per-app mesh-DNS
// resolv.conf + listener at its CNI bridge gateway: the existing case of a
// multi-service isolated app service with a "mesh" network entitlement, and
// now also a single-service "bridge"-mode app. Both need a resolver reachable
// from inside their isolated namespace; the host's own /etc/resolv.conf is
// often not reachable there (e.g. a systemd-resolved stub bound to loopback),
// so both reuse the mesh DNS server's per-gateway forwarding listener instead
// of a host bind-mount (see writeMeshResolvConf, ensureMeshDNS).
func needsGatewayDNS(isolation string, entitlements []appconfig.Entitlement) bool {
	if _, ok := findMeshEntitlement(entitlements); ok && isolation == "isolated" {
		return true
	}
	_, ok := findBridgeEntitlement(entitlements)
	return ok
}

// hasImplicitHostNetworkMode reports whether entitlements contains a network
// entitlement with an omitted/empty mode — the implicit-host default flagged
// for deprecation by specs/2026-07-05-network-bridge-default-design.md.
// Explicit modes ("host", "host-admin", "bridge", "mesh", "none") and apps
// with no network entitlement at all return false: the deprecation warning is
// about the *omission*, not about host networking itself.
func hasImplicitHostNetworkMode(entitlements []appconfig.Entitlement) bool {
	for _, e := range entitlements {
		if e.Type == appconfig.EntitlementNetwork && e.Mode == "" {
			return true
		}
	}
	return false
}

// needsInternetDefaultRoute grants Internet routing only to explicit bridge
// apps. Mesh and service-only isolation keep their scoped routes.
func needsInternetDefaultRoute(entitlements []appconfig.Entitlement) bool {
	_, bridge := findBridgeEntitlement(entitlements)
	_, mesh := findMeshEntitlement(entitlements)
	return bridge && !mesh
}
