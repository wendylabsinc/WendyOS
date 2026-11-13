package appconfig

import (
	"fmt"
	"hash/fnv"
	"regexp"
	"strconv"
	"strings"
)

// ROS2AnnotationKey is the containerd label / OCI annotation under which a
// container's resolved ROS 2 configuration is published, e.g.
// "distro=humble,domain_id=42". The agent uses it to discover ROS 2
// containers and configure the CLI sidecar (WDY-884, WDY-1332).
const ROS2AnnotationKey = EntitlementAnnotationKeyPrefix + "ros2"

// ROS2DefaultDistro is the ROS 2 distribution assumed when wendy.json does
// not specify one.
const ROS2DefaultDistro = "humble"

// ROS2DefaultRMW is the RMW implementation injected when wendy.json does not
// specify one.
const ROS2DefaultRMW = "rmw_cyclonedds_cpp"

// ROS 2 scopes say who can see an app's topics. Each includes the one before:
//   - app: only this app. The agent assigns the app its own domain.
//   - device: every app on the device (loopback discovery).
//   - peripherals: the device plus hardware attached to it, i.e. loopback and
//     the device's wired interfaces other than its uplink (CycloneDDS only).
//   - network: anything on the device's networks (subnet discovery).
const (
	ROS2ScopeApp         = "app"
	ROS2ScopeDevice      = "device"
	ROS2ScopePeripherals = "peripherals"
	ROS2ScopeNetwork     = "network"
)

// ROS2Scopes lists the valid scope values, narrowest first.
var ROS2Scopes = []string{ROS2ScopeApp, ROS2ScopeDevice, ROS2ScopePeripherals, ROS2ScopeNetwork}

// Deprecated discoveryScope values. "app" has always meant loopback discovery,
// shared by apps that pick the same domain, so it maps to scope "device", not
// to the stricter scope "app"; "host" means subnet discovery, i.e. "network".
const (
	ROS2DiscoveryScopeApp  = "app"
	ROS2DiscoveryScopeHost = "host"
)

// ROS2DomainIDMin and ROS2DomainIDMax bound valid ROS_DOMAIN_ID values to the
// full ROS 2 range (0–232). RMW implementations map the domain ID to UDP ports;
// 232 is the maximum the DDS port-mapping scheme supports
// (SOC2-CC6, NIST-SI-10).
const (
	ROS2DomainIDMin = 0
	ROS2DomainIDMax = 232
)

// ros2RMWAliases maps wendy.json rmw values to full RMW implementation
// identifiers. Both the short and full forms are accepted; validating against
// a fixed set prevents injection of arbitrary strings into the container
// environment (SOC2-CC6, ISO27001-A.8, NIST-SI-10).
var ros2RMWAliases = map[string]string{
	"cyclonedds":         "rmw_cyclonedds_cpp",
	"fastrtps":           "rmw_fastrtps_cpp",
	"fastdds":            "rmw_fastrtps_cpp",
	"connextdds":         "rmw_connextdds",
	"gurumdds":           "rmw_gurumdds_cpp",
	"rmw_cyclonedds_cpp": "rmw_cyclonedds_cpp",
	"rmw_fastrtps_cpp":   "rmw_fastrtps_cpp",
	"rmw_connextdds":     "rmw_connextdds",
	"rmw_gurumdds_cpp":   "rmw_gurumdds_cpp",
}

// ROS2DistroPattern is the shape a ROS 2 distribution name must have. It gets
// interpolated into an image reference (docker.io/library/ros:<distro>) and into
// a shell command inside the sidecar (source /opt/ros/<distro>/setup.bash), so it
// is validated at config-parse time as well as at use time (SOC2-CC6,
// ISO27001-A.8, NIST-SI-10). The agent-side gate in containerd/ros2.go enforces
// the same shape.
var ROS2DistroPattern = regexp.MustCompile(`^[a-z][a-z0-9]*$`)

// ros2KnownDistros are the distributions this codebase has been exercised
// against. A well-formed but unrecognized distro is allowed through — pinning the
// list would block a new ROS 2 release until Wendy shipped support — but it is
// worth surfacing as a warning at the CLI, hence the exported helper.
var ros2KnownDistros = map[string]bool{
	"foxy": true, "galactic": true, "humble": true, "iron": true,
	"jazzy": true, "kilted": true, "rolling": true,
}

// IsKnownROS2Distro reports whether s is a ROS 2 distribution this codebase
// recognizes. A false result is not fatal (see ros2KnownDistros).
func IsKnownROS2Distro(s string) bool { return ros2KnownDistros[strings.ToLower(s)] }

// validateROS2Config rejects an out-of-range domainId, an unsupported rmw, or a
// malformed distro so a typo fails fast at config-parse / `wendy run` time
// instead of silently launching a container with no ROS_DOMAIN_ID/
// RMW_IMPLEMENTATION isolation (WDY-1701). prefix labels the source, e.g.
// "frameworks.ros2" or `services["talker"].frameworks.ros2`.
func validateROS2Config(prefix string, r *ROS2Config, entitlements []Entitlement) error {
	if r == nil {
		return nil
	}
	if r.DomainID != nil && (*r.DomainID < ROS2DomainIDMin || *r.DomainID > ROS2DomainIDMax) {
		return fmt.Errorf("%s.domainId must be between %d and %d, got %d", prefix, ROS2DomainIDMin, ROS2DomainIDMax, *r.DomainID)
	}
	if r.RMW != "" && ros2RMWAliases[strings.ToLower(r.RMW)] == "" {
		return fmt.Errorf("%s.rmw %q is not a supported RMW implementation", prefix, r.RMW)
	}
	// Distro was previously unvalidated here, so a typo only surfaced much later
	// as "invalid ROS 2 distro %q in container label" from `wendy device ros2 …`,
	// long after `wendy run` had reported success.
	if r.Distro != "" && !ROS2DistroPattern.MatchString(strings.ToLower(r.Distro)) {
		return fmt.Errorf("%s.distro %q is not a valid ROS 2 distribution name "+
			"(lowercase letters and digits, starting with a letter — e.g. %q)",
			prefix, r.Distro, ROS2DefaultDistro)
	}
	if r.DiscoveryScope != "" && r.ResolvedDiscoveryScope() == "" {
		return fmt.Errorf("%s.discoveryScope %q must be %q or %q", prefix, r.DiscoveryScope, ROS2DiscoveryScopeApp, ROS2DiscoveryScopeHost)
	}
	if r.Scope == "" {
		return nil
	}
	scope := r.ResolvedScope()
	if scope == "" {
		return fmt.Errorf("%s.scope %q must be one of %s", prefix, r.Scope, strings.Join(ROS2Scopes, ", "))
	}
	// scope wins on agents that know it; older agents only read discoveryScope.
	// Both may be set so one manifest serves both, but only when the older
	// agent's reading can never be wider than what scope asks for.
	if r.DiscoveryScope != "" && strings.ToLower(r.DiscoveryScope) != ros2LegacyFallback(scope) {
		return fmt.Errorf("%s: discoveryScope %q conflicts with scope %q; agents that ignore scope would use it, so with scope %q set discoveryScope to %q or remove it",
			prefix, r.DiscoveryScope, r.Scope, scope, ros2LegacyFallback(scope))
	}
	if scope == ROS2ScopeApp && r.DomainID != nil {
		return fmt.Errorf("%s.scope %q gives the app its own domain; remove domainId, or use scope %q to share a domain with other apps on the device",
			prefix, ROS2ScopeApp, ROS2ScopeDevice)
	}
	if scope == ROS2ScopePeripherals && r.ResolvedRMW() != ROS2DefaultRMW {
		return fmt.Errorf("%s.scope %q currently requires rmw %q", prefix, ROS2ScopePeripherals, ROS2DefaultRMW)
	}
	if (scope == ROS2ScopeDevice || scope == ROS2ScopePeripherals) && !sharesHostNetwork(entitlements) {
		return fmt.Errorf("%s.scope %q needs the host network to reach other apps and hardware; use {\"type\": \"network\", \"mode\": \"host\"}",
			prefix, scope)
	}
	return nil
}

// ros2LegacyFallback is the discoveryScope an agent that predates scope may
// safely read next to the given scope. It is never wider: scope peripherals
// falls back to loopback discovery, like device.
func ros2LegacyFallback(scope string) string {
	if scope == ROS2ScopeNetwork {
		return ROS2DiscoveryScopeHost
	}
	return ROS2DiscoveryScopeApp
}

// sharesHostNetwork reports whether the entitlements leave the app on the
// device's host network: no network entitlement (the current default) or mode
// host / host-admin.
func sharesHostNetwork(entitlements []Entitlement) bool {
	for _, ent := range entitlements {
		if ent.Type == EntitlementNetwork && ent.Mode != "" && ent.Mode != "host" && ent.Mode != "host-admin" {
			return false
		}
	}
	return true
}

// IsValidRMWImplementation reports whether s is a full RMW implementation
// identifier this codebase recognizes (the values ros2RMWAliases maps to).
// Callers validate an RMW string read back from a container environment or
// label before injecting it into another container's environment, so a
// tampered or malformed value can never reach the sidecar's env
// (SOC2-CC6, ISO27001-A.8, NIST-SI-10).
func IsValidRMWImplementation(s string) bool {
	switch s {
	case "rmw_cyclonedds_cpp", "rmw_fastrtps_cpp", "rmw_connextdds", "rmw_gurumdds_cpp":
		return true
	default:
		return false
	}
}

// ROS2AutoDomainID derives a stable ROS_DOMAIN_ID from the appId so the
// domain does not change between restarts (WDY-884). The result is always in
// [ROS2DomainIDMin, ROS2DomainIDMax].
func ROS2AutoDomainID(appID string) int {
	h := fnv.New32a()
	_, _ = h.Write([]byte(appID))
	return int(h.Sum32() % uint32(ROS2DomainIDMax-ROS2DomainIDMin+1))
}

// ResolvedDomainID returns the effective ROS_DOMAIN_ID: the explicit
// domainId when set (except with scope "app", which always gets its own),
// otherwise a stable hash of appID. It returns -1 when an
// explicit domainId is outside the valid 0–232 range.
func (r *ROS2Config) ResolvedDomainID(appID string) int {
	if r.DomainID == nil || r.ResolvedScope() == ROS2ScopeApp {
		return ROS2AutoDomainID(appID)
	}
	id := *r.DomainID
	if id < ROS2DomainIDMin || id > ROS2DomainIDMax {
		return -1
	}
	return id
}

// ResolvedRMW returns the full RMW implementation identifier for the config,
// defaulting to CycloneDDS. It returns "" for unknown rmw values so callers
// can drop the value rather than inject an unvalidated string.
func (r *ROS2Config) ResolvedRMW() string {
	if r.RMW == "" {
		return ROS2DefaultRMW
	}
	return ros2RMWAliases[strings.ToLower(r.RMW)]
}

// ResolvedDistro returns the ROS 2 distribution for the config, defaulting
// to ROS2DefaultDistro.
func (r *ROS2Config) ResolvedDistro() string {
	if r.Distro == "" {
		return ROS2DefaultDistro
	}
	return strings.ToLower(r.Distro)
}

// ResolvedScope returns the effective scope: scope when set (it takes
// precedence over discoveryScope), otherwise the deprecated discoveryScope translated to the scope that behaves the same,
// otherwise "device". It returns "" for unknown values so callers never widen
// discovery because of a typo.
func (r *ROS2Config) ResolvedScope() string {
	if r.Scope != "" {
		scope := strings.ToLower(r.Scope)
		for _, valid := range ROS2Scopes {
			if scope == valid {
				return scope
			}
		}
		return ""
	}
	return ros2DiscoveryScopeReplacement(r.DiscoveryScope)
}

// ros2DiscoveryScopeReplacement maps a deprecated discoveryScope value to the
// scope that behaves the same; "" (absent) maps to the default "device".
func ros2DiscoveryScopeReplacement(discoveryScope string) string {
	switch strings.ToLower(discoveryScope) {
	case "", ROS2DiscoveryScopeApp:
		return ROS2ScopeDevice
	case ROS2DiscoveryScopeHost:
		return ROS2ScopeNetwork
	default:
		return ""
	}
}

// ResolvedDiscoveryScope returns the configured deprecated discovery scope,
// defaulting to "app". It returns "" for unknown values.
//
// Deprecated: use ResolvedScope.
func (r *ROS2Config) ResolvedDiscoveryScope() string {
	if r.DiscoveryScope == "" {
		return ROS2DiscoveryScopeApp
	}
	switch strings.ToLower(r.DiscoveryScope) {
	case ROS2DiscoveryScopeApp:
		return ROS2DiscoveryScopeApp
	case ROS2DiscoveryScopeHost:
		return ROS2DiscoveryScopeHost
	default:
		return ""
	}
}

// ResolveROS2ConfigForService returns the effective ROS 2 config for the
// named service: the service-level frameworks.ros2 when present, otherwise
// the group-level frameworks.ros2. serviceName may be empty for
// single-container apps. Returns nil when ROS 2 is not configured.
func (a *AppConfig) ResolveROS2ConfigForService(serviceName string) *ROS2Config {
	if serviceName != "" {
		if svc, ok := a.Services[serviceName]; ok && svc != nil && svc.Frameworks != nil && svc.Frameworks.ROS2 != nil {
			return svc.Frameworks.ROS2
		}
	}
	return a.GetROS2Config()
}

// ROS2AnnotationValue encodes the resolved ROS 2 configuration for the
// sh.wendy/entitlement.ros2 container label, e.g. "distro=humble,domain_id=42".
// It returns "" when the resolved domain ID is invalid.
func ROS2AnnotationValue(r *ROS2Config, appID string) string {
	domainID := r.ResolvedDomainID(appID)
	if domainID < 0 {
		return ""
	}
	return "distro=" + r.ResolvedDistro() + ",domain_id=" + strconv.Itoa(domainID)
}

// ParseROS2Annotation decodes a sh.wendy/entitlement.ros2 label value
// produced by ROS2AnnotationValue. ok is false when the value is missing
// required fields or malformed.
func ParseROS2Annotation(value string) (distro string, domainID int, ok bool) {
	domainID = -1
	for _, part := range strings.Split(value, ",") {
		key, val, found := strings.Cut(part, "=")
		if !found {
			continue
		}
		switch key {
		case "distro":
			distro = val
		case "domain_id":
			if n, err := strconv.Atoi(val); err == nil {
				domainID = n
			}
		}
	}
	if distro == "" || domainID < ROS2DomainIDMin || domainID > ROS2DomainIDMax {
		return "", 0, false
	}
	return distro, domainID, true
}
