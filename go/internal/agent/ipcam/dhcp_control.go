package ipcam

import (
	"errors"
	"fmt"
	"maps"
	"sort"
)

var (
	ErrInvalidDHCPInterface = errors.New("invalid camera DHCP interface")
	ErrDHCPInterfaceMissing = errors.New("camera DHCP interface not found")
	ErrDHCPInterfaceBlocked = errors.New("interface cannot serve camera DHCP")
)

// DHCPStatus separates saved permission from the observed server state.
type DHCPStatus struct {
	Interfaces         []DHCPInterfaceStatus
	ConfigurationError string
	InterfaceError     string
}

type DHCPInterfaceStatus struct {
	Name    string
	Enabled bool
	Serving bool
	Present bool
	Carrier bool
	Up      bool
	IPv4s   []string
	State   string
	Detail  string
}

func (m *LinkManager) DHCPStatus() DHCPStatus {
	m.scanMu.Lock()
	defer m.scanMu.Unlock()
	return m.dhcpStatusLocked()
}

func (m *LinkManager) dhcpStatusLocked() DHCPStatus {
	var result DHCPStatus
	if err := m.refreshDHCPAllowlist(); err != nil {
		result.ConfigurationError = err.Error()
	}
	m.withdrawRevokedClaims()
	ifaces, err := m.listInterfaces()
	if err != nil {
		result.InterfaceError = err.Error()
	} else if m.reconcileInterfaces(ifaces) {
		// Reconciliation may remove the old segment from a renamed port.
		ifaces, err = m.listInterfaces()
		if err != nil {
			result.InterfaceError = err.Error()
		}
	}
	m.mu.Lock()
	allowed := maps.Clone(m.allowedDHCP)
	claimed := maps.Clone(m.claimed)
	addressErrors := maps.Clone(m.addressErrors)
	serverErrors := maps.Clone(m.serverErrors)
	if m.allowlistPath == "" {
		result.ConfigurationError = "camera DHCP configuration is unavailable on this agent"
	}
	m.mu.Unlock()

	byName := make(map[string]Interface)
	names := make(map[string]bool)
	for _, iface := range ifaces {
		byName[iface.Name] = iface
		if cameraDHCPPort(iface) {
			names[iface.Name] = true
		}
	}
	for name := range allowed {
		names[name] = true
	}
	for name := range claimed {
		names[name] = true
	}
	for name := range names {
		iface, present := byName[name]
		seg, serving := claimed[name]
		_, retained := iface.configuredCameraSegment()
		s := DHCPInterfaceStatus{Name: name, Enabled: allowed[name], Serving: serving,
			Present: present, Carrier: iface.Carrier, Up: iface.Up}
		for _, ip := range iface.IPv4s {
			s.IPv4s = append(s.IPv4s, ip.String())
		}
		switch {
		case err != nil:
			s.State, s.Detail = "unknown", "Interface status is unavailable"
		case !s.Enabled:
			s.State, s.Detail = "disabled", "Camera DHCP is disabled"
		case !present:
			s.State, s.Detail = "missing", "Configured interface is not present"
		case serving && m.guard.State(name) == LinkDisqualified:
			s.Serving = false
			s.State, s.Detail = "blocked", "Camera DHCP stopped after disqualification; address cleanup is pending"
		case serving && (!iface.Up || !iface.Carrier):
			s.State, s.Detail = "degraded", "Camera link is not active"
		case serving && iface.conflictsWithCameraSegment(seg):
			s.State, s.Detail = "degraded", "Conflicting IPv4 configuration detected; camera DHCP will stop on the next scan"
		case serving && !iface.HasAddress(seg.ServerIP):
			s.State, s.Detail = "degraded", "Camera address "+seg.CIDR()+" is missing; restoration will be retried"
			if failure := addressErrors[name]; failure != "" {
				s.Detail += ": " + failure
			}
		case serving:
			s.State, s.Detail = "serving", "Serving camera addresses on "+seg.CIDR()
		case !cameraDHCPPort(iface):
			s.State, s.Detail = "blocked", "Only wired interfaces can serve camera DHCP"
		case iface.HasIPv4 && !retained:
			s.State, s.Detail = "blocked", "Interface has conflicting IPv4 configuration"
		case !iface.Up || !iface.Carrier:
			s.State, s.Detail = "waiting", "Waiting for a connected, active Ethernet link"
		case serverErrors[name] != "":
			s.State, s.Detail = "blocked", serverErrors[name]
		case m.guard.State(name) == LinkDisqualified:
			s.State, s.Detail = "blocked", "Competing DHCP server, uplink, or link configuration failure detected; reconnect the dedicated camera link to retry"
		case retained:
			s.State, s.Detail = "waiting", "Checking for competing DHCP servers before resuming the camera segment"
		default:
			s.State, s.Detail = "waiting", "Waiting for unanswered camera DHCP requests"
		}
		result.Interfaces = append(result.Interfaces, s)
	}
	sort.Slice(result.Interfaces, func(i, j int) bool { return result.Interfaces[i].Name < result.Interfaces[j].Name })
	return result
}

func cameraDHCPPort(iface Interface) bool {
	return !iface.Loopback && !iface.PointToPo && !isWireless(iface.Name)
}

// SetDHCPEnabled changes one port without overwriting other ports. A disable
// cancels serving before returning, even if interface enumeration fails. An
// enable is permission only: the normal scan and competing-server guards still
// decide when the server may start, using the manager's long-lived context.
func (m *LinkManager) SetDHCPEnabled(link string, enabled bool) (DHCPStatus, error) {
	m.scanMu.Lock()
	defer m.scanMu.Unlock()
	if !validDHCPInterfaceName(link) {
		return DHCPStatus{}, ErrInvalidDHCPInterface
	}
	if err := m.refreshDHCPAllowlist(); err != nil {
		m.withdrawRevokedClaims()
		return DHCPStatus{}, fmt.Errorf("reading camera DHCP configuration: %w", err)
	}
	m.withdrawRevokedClaims()
	m.mu.Lock()
	path := m.allowlistPath
	allowed := maps.Clone(m.allowedDHCP)
	m.mu.Unlock()
	if path == "" {
		return DHCPStatus{}, fmt.Errorf("camera DHCP configuration is unavailable on this agent")
	}
	if enabled {
		ifaces, err := m.listInterfaces()
		if err != nil {
			return DHCPStatus{}, fmt.Errorf("checking camera interface: %w", err)
		}
		m.reconcileInterfaces(ifaces)
		found := false
		for _, iface := range ifaces {
			if iface.Name != link {
				continue
			}
			found = true
			seg, claimed := m.claimedSegment(link)
			_, retained := iface.configuredCameraSegment()
			if !cameraDHCPPort(iface) || (!claimed && iface.HasIPv4 && !retained) || (claimed && iface.conflictsWithCameraSegment(seg)) {
				return DHCPStatus{}, fmt.Errorf("%w: %s must be a dedicated wired camera port without an uplink address", ErrDHCPInterfaceBlocked, link)
			}
		}
		if !found {
			return DHCPStatus{}, fmt.Errorf("%w: %s", ErrDHCPInterfaceMissing, link)
		}
		allowed[link] = true
	} else {
		delete(allowed, link)
	}
	if err := m.writeAllowlist(path, allowed); err != nil {
		// Rename may have succeeded before directory sync failed. Reconcile
		// what is visible so an already committed disable stops serving even
		// though we cannot acknowledge durable success to the operator.
		_ = m.refreshDHCPAllowlist()
		m.withdrawRevokedClaims()
		return DHCPStatus{}, fmt.Errorf("saving camera DHCP configuration: %w", err)
	}
	m.mu.Lock()
	m.allowedDHCP = allowed
	m.mu.Unlock()
	m.withdrawRevokedClaims()
	return m.dhcpStatusLocked(), nil
}
