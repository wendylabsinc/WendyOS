package containerd

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"

	specs "github.com/opencontainers/runtime-spec/specs-go"
	"go.uber.org/zap"

	"github.com/wendylabsinc/wendy/go/internal/agent/hostnetwork"
	"github.com/wendylabsinc/wendy/go/internal/agent/mesh"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
)

// verifiedMeshResultIP accepts only one IPv4 address in this app's allocated
// subnet. CNI CHECK subsequently proves the corresponding host-local lease,
// bridge attachment, and namespace state before this IP is used for ingress.
func verifiedMeshResultIP(result, subnet string) (string, error) {
	var parsed cniResult
	if err := json.Unmarshal([]byte(result), &parsed); err != nil || len(parsed.IPs) != 1 {
		return "", fmt.Errorf("mesh CNI proof has invalid IP list")
	}
	ip, resultNet, err := net.ParseCIDR(parsed.IPs[0].Address)
	if err != nil || ip.To4() == nil {
		return "", fmt.Errorf("mesh CNI proof has invalid IPv4 address")
	}
	_, assignedNet, err := net.ParseCIDR(subnet)
	if err != nil || !assignedNet.Contains(ip) || !networkMasksEqual(resultNet.Mask, assignedNet.Mask) {
		return "", fmt.Errorf("mesh CNI proof IP is outside assigned subnet")
	}
	return ip.String(), nil
}

func networkMasksEqual(a, b net.IPMask) bool {
	return a.String() == b.String()
}

// meshCheckResultForTask maps the original CNI ADD namespace path to the
// surviving task's procfs namespace path. The bridge CHECK plugin compares
// these strings before performing its live bridge, veth, address, route, and
// host-local IPAM checks. Mesh tasks do not retain the original bind mount.
// Refuse any stored result whose original eth0 sandbox was not our canonical
// CNI path; only that one expected-state field may be translated.
func meshCheckResultForTask(result, originalPath, taskPath string) (string, error) {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal([]byte(result), &doc); err != nil {
		return "", err
	}
	var interfaces []map[string]json.RawMessage
	if err := json.Unmarshal(doc["interfaces"], &interfaces); err != nil {
		return "", err
	}
	var matched int
	for _, iface := range interfaces {
		var name, sandbox string
		if err := json.Unmarshal(iface["name"], &name); err != nil || name != "eth0" {
			continue
		}
		if err := json.Unmarshal(iface["sandbox"], &sandbox); err != nil || sandbox != originalPath {
			return "", fmt.Errorf("mesh CNI proof has unexpected eth0 sandbox")
		}
		matched++
		iface["sandbox"], _ = json.Marshal(taskPath)
	}
	if matched != 1 {
		return "", fmt.Errorf("mesh CNI proof must have exactly one eth0 interface")
	}
	updated, err := json.Marshal(interfaces)
	if err != nil {
		return "", err
	}
	doc["interfaces"] = updated
	updated, err = json.Marshal(doc)
	if err != nil {
		return "", err
	}
	return string(updated), nil
}

// meshResolvConfDir holds per-app resolv.conf files pointing meshed
// containers at the mesh DNS server on their bridge gateway.
const meshResolvConfDir = "/run/wendy/mesh"

// meshDNSService is the seam through which the container lifecycle manages
// per-gateway mesh DNS listeners. Satisfied by *mesh.DNSServer in production
// (injected via SetMeshDNS) and by a recording fake in tests, so the
// Ensure/Release pairing invariant is unit-testable without binding UDP
// listeners or running iptables.
type meshDNSService interface {
	EnsureListener(gatewayIP string) error
	ReleaseListener(gatewayIP string)
}

// meshMDNSService owns a per-app multicast bridge. It is injected by the
// local-mesh runtime; containerd supplies trusted CNI and port-map state.
type meshMDNSService interface {
	StartMeshApp(containerName, appID, ip, bridge, netnsPath, gateway string, ports []appconfig.PortMapping) error
	StopMeshApp(containerName string)
}

// Narrow network seams let ingress authorization tests exercise the real
// lifecycle without requiring root privileges or mutating host iptables.
var (
	addMeshIngressPortForward       = hostnetwork.AddIngressPortForward
	removeMeshIngressPortForward    = hostnetwork.RemoveIngressPortForward
	addMeshUDPIngressPortForward    = hostnetwork.AddIngressUDPPortForward
	removeMeshUDPIngressPortForward = hostnetwork.RemoveIngressUDPPortForward
	flushOrphanMeshPort             = hostnetwork.FlushOrphanMeshPort
)

// Compile-time check that the real DNS server satisfies the seam.
var _ meshDNSService = (*mesh.DNSServer)(nil)

// ensureMeshDNS acquires one DNS-listener reference for containerName's
// gateway and records the acquisition in c.meshDNSHeld, so releaseMeshDNS
// only ever balances refcounts this exact container actually took. Without
// the held map, a container whose EnsureListener failed would still release
// on teardown and decrement a refcount owned by a sibling service sharing
// the same gateway, prematurely killing the sibling's listener.
//
// Best-effort: a failure only logs a warning (device-N hostnames won't
// resolve; VIP literals still work) and takes no refcount.
func (c *Client) ensureMeshDNS(containerName, gateway string) bool {
	if c.meshDNS == nil {
		return false
	}
	// Idempotent per container: the monitor's restartSingle calls
	// StartContainer directly with no intervening stopOne/teardownMeshEgress,
	// so applyMeshEgress (and therefore this function) can run a second time
	// for a container whose listener reference is already held. Without this
	// guard EnsureListener would be called again (refs++) while
	// meshDNSHeld[containerName] was already true, and the single paired
	// release in teardownMeshEgress would only ever bring the refcount back
	// down by one — permanently leaking a reference (and, on the real DNS
	// server, the listener) on every restart that skips teardown.
	c.meshMu.Lock()
	alreadyHeld := c.meshDNSHeld[containerName]
	c.meshMu.Unlock()
	if alreadyHeld {
		return true
	}
	if err := c.meshDNS.EnsureListener(gateway); err != nil {
		c.logger.Warn("mesh: DNS listener unavailable; device-N hostnames will not resolve",
			zap.String("container", containerName), zap.String("gateway", gateway), zap.Error(err))
		return false
	}
	c.meshMu.Lock()
	if c.meshDNSHeld == nil {
		c.meshDNSHeld = make(map[string]bool)
	}
	c.meshDNSHeld[containerName] = true
	c.meshMu.Unlock()
	return true
}

// releaseMeshDNS drops the DNS-listener reference containerName holds, if
// any. It is idempotent: the held-map entry is consumed on the first call,
// so a container torn down twice (stopOne followed by deleteOne) releases
// exactly once, and a container whose EnsureListener never succeeded
// releases nothing. Guarded by meshMu (not c.mu) because deleteOne runs
// with c.mu already held by DeleteContainer, while stopOne runs without it.
func (c *Client) releaseMeshDNS(containerName, appID string) {
	if c.meshDNS == nil {
		return
	}
	c.meshMu.Lock()
	held := c.meshDNSHeld[containerName]
	delete(c.meshDNSHeld, containerName)
	c.meshMu.Unlock()
	if !held {
		return
	}
	// Recomputing the gateway is safe: meshGateway is idempotent and returns
	// the same value ensureMeshDNS used to acquire the listener.
	gw, err := meshGateway(appID)
	if err != nil {
		c.logger.Warn("mesh: could not derive gateway to release DNS listener (non-fatal, listener may leak until agent restart)",
			zap.String("container", containerName), zap.String("app_id", appID), zap.Error(err))
		return
	}
	c.meshDNS.ReleaseListener(gw)
}

// findMeshEntitlement returns the network entitlement with mode "mesh" from
// entitlements, if one is present. Apps without the mesh entitlement (no
// network entitlement, or network mode host/host-admin/none) get ok == false,
// which callers must treat as a complete no-op — mesh wiring must never run
// for a container that did not request it.
func findMeshEntitlement(entitlements []appconfig.Entitlement) (ent appconfig.Entitlement, ok bool) {
	for _, e := range entitlements {
		if e.Type == appconfig.EntitlementNetwork && e.Mode == "mesh" {
			return e, true
		}
	}
	return appconfig.Entitlement{}, false
}

// normalizeCIDR parses s as a CIDR and returns the canonical form (network
// address + prefix length) via (*net.IPNet).String(). This guards against a
// serviceCIDR with host bits set (e.g. "10.99.0.5/16") being passed as-is to
// `ip route replace` or iptables, which would silently narrow the intended
// match to a single mis-aligned network (C3a-review Minor #1).
func normalizeCIDR(s string) (string, error) {
	_, ipNet, err := net.ParseCIDR(s)
	if err != nil {
		return "", fmt.Errorf("parsing CIDR %q: %w", s, err)
	}
	return ipNet.String(), nil
}

// meshGateway derives the mesh gateway address for appID: the first host
// address (".1") of the /28 subnet the CNI bridge plugin already allocated
// for this app. It calls allocateSubnet rather than deriving the subnet
// independently so it always agrees with the subnet the bridge actually
// configured as isGateway:true (see buildBridgeCNIConfig) — allocateSubnet
// is idempotent and returns the existing registry entry for an appID that
// already has one, so this never allocates a second, different subnet.
func meshGateway(appID string) (string, error) {
	subnet, err := allocateSubnet(appID)
	if err != nil {
		return "", fmt.Errorf("resolving mesh gateway subnet: %w", err)
	}
	_, ipNet, err := net.ParseCIDR(subnet)
	if err != nil {
		return "", fmt.Errorf("parsing allocated subnet %q: %w", subnet, err)
	}
	gateway := make(net.IP, len(ipNet.IP))
	copy(gateway, ipNet.IP)
	gateway[len(gateway)-1] |= 1
	return gateway.String(), nil
}

// meshEgressParams bundles the values needed to wire (or tear down) mesh
// egress for one container: the mesh gateway derived from the app's CNI
// subnet, and the serviceCIDR normalized once so every downstream call
// (SetMeshRoute, AddMeshRule, RemoveMeshRule) sees an identical string.
type meshEgressParams struct {
	gateway string
	cidr    string
	ports   []appconfig.PortMapping
}

// writeMeshResolvConfIn writes the resolv.conf for one app under baseDir and
// returns its path. Split from writeMeshResolvConf for testability: tests
// pass a temp directory instead of the real meshResolvConfDir, which lives
// under /run and is not writable in a non-root test sandbox.
//
// The write is atomic (temp file + os.Rename, mirroring writeHostsFile):
// every sibling-service create rewrites the same appID-keyed file while a
// running sibling may have it bind-mounted at /etc/resolv.conf, so a plain
// truncating write could expose a zero-byte resolv.conf mid-write and break
// the sibling's DNS until the next write completed (NIST-SI-10).
func writeMeshResolvConfIn(baseDir, appID string) (string, error) {
	gw, err := meshGateway(appID)
	if err != nil {
		return "", err
	}
	dir := filepath.Join(baseDir, appID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(dir, "resolv.conf")
	content := fmt.Sprintf("nameserver %s\noptions ndots:1\n", gw)

	tmp, err := os.CreateTemp(dir, ".resolv-*.tmp")
	if err != nil {
		return "", fmt.Errorf("creating temp resolv.conf: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.WriteString(content); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return "", fmt.Errorf("writing temp resolv.conf: %w", err)
	}
	// Chmod via the open fd before Close: CreateTemp creates 0600, but the
	// file must be world-readable for arbitrary container users, and the
	// fd-based chmod leaves no TOCTOU window before the rename (NIST-SI-10).
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return "", fmt.Errorf("chmod temp resolv.conf: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return "", fmt.Errorf("closing temp resolv.conf: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return "", fmt.Errorf("renaming resolv.conf into place: %w", err)
	}
	return path, nil
}

// writeMeshResolvConf writes appID's resolv.conf under the real
// meshResolvConfDir. Called from the container create path to produce the
// file bind-mounted into meshed containers at /etc/resolv.conf.
func writeMeshResolvConf(appID string) (string, error) {
	return writeMeshResolvConfIn(meshResolvConfDir, appID)
}

// meshResolvMountSource returns the Source of the create-time mesh resolv.conf
// bind mount in mounts — a mount whose Destination is /etc/resolv.conf and
// whose Source lives under baseDir (meshResolvConfDir in production) — if one
// is present. It is the exact, leak-free signal that a container was created
// with mesh DNS wiring: InjectResolvMount adds precisely this mount, and only
// for isolated meshed apps (CreateContainerWithProgress gates it on
// findMeshEntitlement && Isolation=="isolated"). The baseDir prefix match is
// path-segment bounded (baseDir + separator) so a sibling directory sharing
// baseDir's string prefix cannot spoof a match, and a resolv.conf mount from
// the image or elsewhere (Source outside baseDir) is correctly ignored.
func meshResolvMountSource(mounts []specs.Mount, baseDir string) (source string, ok bool) {
	prefix := baseDir + string(os.PathSeparator)
	for _, m := range mounts {
		if m.Destination == "/etc/resolv.conf" && strings.HasPrefix(m.Source, prefix) {
			return m.Source, true
		}
	}
	return "", false
}

// recreateMeshResolvConfIn rewrites the mesh resolv.conf bind-mount source for
// a container whose persisted OCI spec still carries the create-time mesh
// mount (see meshResolvMountSource), returning ok == false as a complete no-op
// otherwise. The appID is derived from the mount Source itself
// (baseDir/<appID>/resolv.conf), so the file recreated is exactly the path the
// spec's mount points at. Pure/containerd-free (baseDir + mounts injected) so
// this is unit-testable without touching /run.
//
// This is the reboot-resilience fix for meshed containers (C-final-review Fix
// 1): /etc/resolv.conf is bind-mounted from this path, baked into the OCI spec
// once at CreateContainerWithProgress time. containerd persists the container
// definition (and its spec's mount list) across a reboot, but /run is tmpfs,
// so the file written at create time is gone. ReconcileBootContainers restarts
// surviving containers via StartContainer directly — never CreateContainer —
// and the runtime processes the spec's bind mounts as part of
// container.NewTask, so a missing source there fails task creation outright: a
// meshed container would never start again after a reboot without recreating
// the file first.
//
// The gate is the spec mount, NOT the mesh entitlement (round-2 leak fix): a
// mesh-entitled-but-NON-isolated app has the entitlement but no
// /etc/resolv.conf mesh mount and no bridge, so entitlement gating would have
// run meshGateway → allocateSubnet on every StartContainer and leaked a
// permanent, never-released subnet-registry entry for an app that never gets a
// bridge. Keying on the mount fires only for containers actually created with
// the mount — leak-free and matching create-time exactly.
func recreateMeshResolvConfIn(baseDir string, mounts []specs.Mount) (ok bool, err error) {
	source, found := meshResolvMountSource(mounts, baseDir)
	if !found {
		return false, nil
	}
	// source == baseDir/<appID>/resolv.conf, so the parent dir's base is appID.
	appID := filepath.Base(filepath.Dir(source))
	if _, err := writeMeshResolvConfIn(baseDir, appID); err != nil {
		return true, err
	}
	return true, nil
}

// recreateMeshResolvConfForStart is StartContainer's reboot-resilience hook:
// called with the container's persisted OCI spec mounts before
// container.NewTask, it recreates the mesh resolv.conf bind-mount source so it
// exists by the time the runtime processes the mount. Gating on the spec mount
// (not c.getIsolation(appID), which reads an in-memory cache empty after every
// agent restart — the very reboot this exists to survive — and not the mesh
// entitlement, which over-fires for non-isolated apps and leaks subnet
// registry entries) keys the work on exactly the containers created with the
// mount. Best-effort like the rest of mesh wiring: a failure only logs a
// warning (the container may fail to start, or fall back to the image's
// resolv.conf) and never blocks a non-mesh container's start.
func (c *Client) recreateMeshResolvConfForStart(mounts []specs.Mount) {
	if _, err := recreateMeshResolvConfIn(meshResolvConfDir, mounts); err != nil {
		c.logger.Warn("mesh: could not recreate resolv.conf before container start", zap.Error(err))
	}
}

// resolveMeshEgress checks entitlements for a network/mesh entry and, if
// found, computes the gateway + normalized CIDR needed to wire mesh egress.
// It returns ok == false as a complete no-op signal for apps without the
// mesh entitlement — callers must not touch iptables/routes in that case.
func resolveMeshEgress(entitlements []appconfig.Entitlement, appID string) (params meshEgressParams, ok bool, err error) {
	ent, found := findMeshEntitlement(entitlements)
	if !found {
		return meshEgressParams{}, false, nil
	}
	cidr, err := normalizeCIDR(ent.ServiceCIDR)
	if err != nil {
		return meshEgressParams{}, true, fmt.Errorf("mesh entitlement has invalid serviceCIDR: %w", err)
	}
	gateway, err := meshGateway(appID)
	if err != nil {
		return meshEgressParams{}, true, fmt.Errorf("deriving mesh gateway: %w", err)
	}
	return meshEgressParams{gateway: gateway, cidr: cidr, ports: ent.Ports}, true, nil
}

// applyMeshEgress wires mesh egress for a just-started container: a route
// inside its netns toward the mesh service CIDR via the app's bridge
// gateway, and a host iptables rule scoping egress to exactly that CIDR for
// exactly this container's IP. It is a complete no-op — no route, no rule,
// no error — for any app without a network entitlement in mode "mesh"
// (SOC2-CC6: least privilege, opt-in only).
//
// This is fail-closed: if either the route or the rule cannot be installed,
// an error is returned and any partially-applied state is best-effort rolled
// back (the rule, if the route succeeded but the rule failed) so a meshed
// container never runs believing it has egress it does not actually have.
// The caller MUST fail container start on a non-nil error.
//
// containerName is the containerd container ID ({appID}_{serviceName}); it
// keys the DNS-listener held map so teardown releases exactly the refcounts
// this container took (see ensureMeshDNS/releaseMeshDNS).
func (c *Client) applyMeshEgress(entitlements []appconfig.Entitlement, containerName, appID, netnsPath, projectionNetnsPath, ip string) error {
	params, ok, err := resolveMeshEgress(entitlements, appID)
	if err != nil {
		return fmt.Errorf("mesh egress: %w", err)
	}
	if !ok {
		return nil
	}

	if err := hostnetwork.SetMeshRoute(netnsPath, params.cidr, params.gateway); err != nil {
		return fmt.Errorf("mesh egress: setting route for app %q: %w", appID, err)
	}
	if err := hostnetwork.SetMeshMDNSRoute(netnsPath, "eth0"); err != nil {
		return fmt.Errorf("mesh egress: setting mDNS route for app %q: %w", appID, err)
	}

	if err := hostnetwork.AddMeshRule(ip, params.cidr); err != nil {
		// The route lives in the container's netns and needs no explicit
		// cleanup here — it disappears automatically when the netns is torn
		// down as part of the failed start. Only the host-side iptables rule
		// could leak, and AddMeshRule failed to install it in the first
		// place, so there is nothing to remove. This branch exists so a
		// future change to what AddMeshRule partially applies on error does
		// not silently skip cleanup.
		if rmErr := hostnetwork.RemoveMeshRule(ip, params.cidr); rmErr != nil {
			c.logger.Warn("mesh egress: best-effort rule cleanup after failed AddMeshRule also failed",
				zap.String("app_id", appID), zap.String("ip", ip), zap.Error(rmErr))
		}
		return fmt.Errorf("mesh egress: adding iptables rule for app %q: %w", appID, err)
	}

	if err := hostnetwork.AddMeshRedirect(ip, params.cidr, mesh.ProxyPort); err != nil {
		// Roll back what we installed; the start must fail closed. The
		// DNS listener has not been touched yet at this point (EnsureListener
		// runs after this check succeeds), so there is nothing to release here.
		if rmErr := hostnetwork.RemoveMeshRule(ip, params.cidr); rmErr != nil {
			c.logger.Warn("mesh egress: rollback of ACCEPT rule after failed REDIRECT rule also failed",
				zap.String("app_id", appID), zap.String("ip", ip), zap.Error(rmErr))
		}
		return fmt.Errorf("mesh egress: adding REDIRECT rule for app %q: %w", appID, err)
	}
	if err := hostnetwork.AddMeshUDPIntercept(ip, params.cidr, bridgeName(appID), mesh.UDPProxyPort); err != nil {
		_ = hostnetwork.RemoveMeshRedirect(ip, params.cidr, mesh.ProxyPort)
		_ = hostnetwork.RemoveMeshRule(ip, params.cidr)
		return fmt.Errorf("mesh egress: adding UDP TPROXY rule for app %q: %w", appID, err)
	}

	// DNS is best-effort: without it, device-N.cloud.wendy.dev hostnames fail
	// to resolve but VIP literals still work over the REDIRECT/route wired
	// above, so a DNS listener failure must not fail container start.
	// ensureMeshDNS is the last fallible step in this function — nothing
	// below can fail once it returns, so there is no rollback path in this
	// function that needs to release it; the paired release lives in
	// teardownMeshEgress (invoked from stopOne and deleteOne) and only fires
	// for containers whose acquisition actually succeeded (held map).
	c.ensureMeshDNS(containerName, params.gateway)

	// CNI's bind mount is released after app startup. The mDNS bridge remains
	// alive for the task and must use that task's live namespace path when it
	// later installs a newly discovered physical-LAN service route.
	if err := c.applyMeshIngressPorts(containerName, appID, projectionNetnsPath, ip, params.ports); err != nil {
		return err
	}
	if err := c.claimMeshSource(containerName, appID, ip, params.cidr); err != nil {
		return fmt.Errorf("mesh egress: claiming app source: %w", err)
	}

	c.logger.Info("mesh egress applied",
		zap.String("app_id", appID), zap.String("ip", ip), zap.String("service_cidr", params.cidr))
	return nil
}

func (c *Client) claimMeshSource(containerName, appID, ip, cidr string) error {
	bridge, err := net.InterfaceByName(bridgeName(appID))
	if err != nil {
		return fmt.Errorf("locating app bridge: %w", err)
	}
	return c.meshIngress.ClaimSource(containerName, appID, ip, cidr, bridge.Index)
}

// applyMeshIngressPorts publishes only declared host ports for an isolated
// mesh task whose CNI setup has completed. It also runs when StartContainer
// reuses a validated network sandbox, because that path skips applyMeshEgress.
// A failed individual iptables forward remains unavailable to remote peers;
// a conflicting host port fails the second task's start before iptables can
// replace the first task's rule.
func (c *Client) applyMeshIngressPorts(containerName, appID, netnsPath, ip string, ports []appconfig.PortMapping) error {
	if ip == "" {
		return fmt.Errorf("mesh ingress: container %q has no CNI IP", containerName)
	}
	if c.meshIngress == nil {
		return fmt.Errorf("mesh ingress: authorization registry is unavailable")
	}
	c.meshIngressMu.Lock()
	defer c.meshIngressMu.Unlock()
	if len(ports) == 0 {
		return c.startMeshMDNS(containerName, appID, netnsPath, ip, nil)
	}

	seen := make(map[string]struct{}, len(ports))
	for _, pm := range ports {
		if pm.Host == 0 || pm.Container == 0 {
			return fmt.Errorf("mesh ingress: container %q declares a zero port", containerName)
		}
		protocol := pm.Protocol
		if protocol == "" {
			protocol = "tcp"
		}
		if protocol != "tcp" && protocol != "udp" {
			return fmt.Errorf("mesh ingress: invalid protocol %q", protocol)
		}
		key := fmt.Sprintf("%s/%d", protocol, pm.Host)
		if _, duplicate := seen[key]; duplicate {
			return fmt.Errorf("mesh ingress: container %q declares host port %d more than once", containerName, pm.Host)
		}
		seen[key] = struct{}{}
		var err error
		if protocol == "udp" {
			err = c.meshIngress.CheckUDPAvailable(containerName, pm.Host)
		} else {
			err = c.meshIngress.CheckAvailable(containerName, pm.Host)
		}
		if err != nil {
			return err
		}
	}
	if err := hostnetwork.EnableRouteLocalnet(bridgeName(appID)); err != nil {
		c.logger.Warn("mesh ingress: could not enable route_localnet on bridge; ingress replies may fail",
			zap.String("app_id", appID), zap.Error(err))
	}
	forwarded := make([]appconfig.PortMapping, 0, len(ports))
	for _, pm := range ports {
		add, remove := addMeshIngressPortForward, removeMeshIngressPortForward
		if pm.Protocol == "udp" {
			add, remove = addMeshUDPIngressPortForward, removeMeshUDPIngressPortForward
			if !c.meshIngress.Allowed(pm.Host) {
				if err := flushOrphanMeshPort(pm.Host, "tcp"); err != nil {
					return err
				}
			}
		} else if !c.meshIngress.AllowedUDP(pm.Host) {
			if err := flushOrphanMeshPort(pm.Host, "udp"); err != nil {
				return err
			}
		}
		if err := add(pm.Host, ip, pm.Container); err != nil {
			c.logger.Warn("mesh ingress: could not install port forward; remote peers cannot open this port",
				zap.String("app_id", appID), zap.String("ip", ip),
				zap.Uint16("host_port", pm.Host), zap.Uint16("container_port", pm.Container), zap.Error(err))
			continue
		}
		var err error
		if pm.Protocol == "udp" {
			err = c.meshIngress.ClaimUDPForApp(containerName, appID, pm.Host)
		} else {
			err = c.meshIngress.ClaimForApp(containerName, appID, pm.Host)
		}
		if err != nil {
			_ = remove(pm.Host, ip, pm.Container)
			return fmt.Errorf("mesh ingress: claiming host port %d: %w", pm.Host, err)
		}
		forwarded = append(forwarded, pm)
		c.logger.Info("mesh ingress: port forward authorized",
			zap.String("app_id", appID), zap.String("ip", ip),
			zap.Uint16("host_port", pm.Host), zap.Uint16("container_port", pm.Container))
	}
	// The collector must see only live forwards. Otherwise an app announcing
	// an unforwarded declared port causes authorization to fail and tears down
	// the bridge, including unrelated services that did forward successfully.
	return c.startMeshMDNS(containerName, appID, netnsPath, ip, forwarded)
}

func (c *Client) startMeshMDNS(containerName, appID, netnsPath, ip string, ports []appconfig.PortMapping) error {
	if c.meshMDNS == nil {
		return nil
	}
	address := net.ParseIP(ip).To4()
	if address == nil {
		return errors.New("mesh app has no IPv4 address")
	}
	gatewayIP := append(net.IP(nil), address...)
	gatewayIP[3] = gatewayIP[3]&0xf0 | 1 // CNI bridge uses a /28 and its first address.
	gateway := gatewayIP.String()
	if err := c.meshMDNS.StartMeshApp(containerName, appID, ip, bridgeName(appID), netnsPath, gateway, ports); err != nil {
		// Direct VIP traffic remains usable if an app multicast bridge cannot
		// start. The catalog never publishes this app without its bridge.
		c.logger.Warn("mesh app mDNS bridge unavailable", zap.String("app_id", appID), zap.Error(err))
	}
	return nil
}

// beginMeshIngressRun revokes any claim from the previous task before its
// replacement starts. The returned generation ties task-exit revocation to
// this run, so a delayed exit event cannot revoke a newer replacement.
func (c *Client) beginMeshIngressRun(containerName string) uint64 {
	c.meshIngressMu.Lock()
	defer c.meshIngressMu.Unlock()
	if c.meshMDNS != nil {
		c.meshMDNS.StopMeshApp(containerName)
	}
	c.meshIngress.Release(containerName)
	c.meshIngressNext++
	if c.meshIngressRuns == nil {
		c.meshIngressRuns = make(map[string]uint64)
	}
	c.meshIngressRuns[containerName] = c.meshIngressNext
	return c.meshIngressNext
}

func (c *Client) releaseMeshIngressRun(containerName string, generation uint64) {
	c.meshIngressMu.Lock()
	defer c.meshIngressMu.Unlock()
	if generation == 0 || c.meshIngressRuns[containerName] != generation {
		return
	}
	if c.meshMDNS != nil {
		c.meshMDNS.StopMeshApp(containerName)
	}
	c.meshIngress.Release(containerName)
	delete(c.meshIngressRuns, containerName)
}

func (c *Client) releaseMeshIngress(containerName string) {
	c.meshIngressMu.Lock()
	defer c.meshIngressMu.Unlock()
	if c.meshMDNS != nil {
		c.meshMDNS.StopMeshApp(containerName)
	}
	c.meshIngress.Release(containerName)
	delete(c.meshIngressRuns, containerName)
}

// teardownMeshEgress removes the host iptables rules (ACCEPT + REDIRECT)
// installed by applyMeshEgress and releases the container's DNS-listener
// reference. It is a complete no-op for apps without the mesh entitlement.
// The iptables removals are skipped if ip is empty (the container's IP could
// not be recovered — see stopOne for how it is normally recovered from
// c.serviceIPs), but the DNS release still runs: it is keyed by
// containerName via the held map, not by IP, so a lost IP must not strand a
// listener refcount. The netns route needs no explicit cleanup: it is
// destroyed automatically when the network namespace is torn down with the
// container.
//
// Idempotent: rule removals tolerate already-absent rules, and the DNS
// release consumes the held-map entry on first call, so running stopOne and
// then deleteOne for the same container releases exactly once.
//
// Errors are logged but not returned — mirroring CNIDel's best-effort
// contract, so a host-side iptables failure never blocks a container stop.
func (c *Client) teardownMeshEgress(entitlements []appconfig.Entitlement, containerName, appID, ip string) {
	c.releaseMeshIngress(containerName)
	ent, found := findMeshEntitlement(entitlements)
	if !found {
		return
	}
	_ = os.Remove(networkSandboxResultPath(containerName))
	if ip != "" {
		cidr, err := normalizeCIDR(ent.ServiceCIDR)
		if err != nil {
			c.logger.Warn("mesh egress teardown: invalid serviceCIDR in entitlement, skipping rule removal",
				zap.String("app_id", appID), zap.Error(err))
		} else {
			if err := hostnetwork.RemoveMeshRule(ip, cidr); err != nil {
				c.logger.Warn("mesh egress teardown: RemoveMeshRule failed (non-fatal)",
					zap.String("app_id", appID), zap.String("ip", ip), zap.Error(err))
			}
			if err := hostnetwork.RemoveMeshRedirect(ip, cidr, mesh.ProxyPort); err != nil {
				c.logger.Warn("mesh egress teardown: RemoveMeshRedirect failed (non-fatal)",
					zap.String("app_id", appID), zap.String("ip", ip), zap.Error(err))
			}
			if err := hostnetwork.RemoveMeshUDPIntercept(ip, cidr, bridgeName(appID), mesh.UDPProxyPort); err != nil {
				c.logger.Warn("mesh egress teardown: RemoveMeshUDPIntercept failed (non-fatal)", zap.String("app_id", appID), zap.String("ip", ip), zap.Error(err))
			}
		}
		for _, pm := range ent.Ports {
			remove := hostnetwork.RemoveIngressPortForward
			if pm.Protocol == "udp" {
				remove = hostnetwork.RemoveIngressUDPPortForward
			}
			if err := remove(pm.Host, ip, pm.Container); err != nil {
				c.logger.Warn("mesh egress teardown: RemoveIngressPortForward failed (non-fatal)",
					zap.String("app_id", appID), zap.String("ip", ip),
					zap.Uint16("host_port", pm.Host), zap.Uint16("container_port", pm.Container), zap.Error(err))
			}
		}
	}
	// Pairs with ensureMeshDNS in applyMeshEgress: releases exactly the
	// refcount this container acquired, or nothing if it never acquired one
	// (held-map guard — see releaseMeshDNS for the sibling-imbalance and
	// double-teardown rationale).
	c.releaseMeshDNS(containerName, appID)
}
