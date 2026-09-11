package vm

import (
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"net"
	"strings"
)

// DefaultAgentPort is the agent's port, fixed in the image's avahi service
// record. Only the host side of a forward is configurable.
const DefaultAgentPort = 50051

// AgentPortSpan is how many consecutive ports one VM occupies. A provisioned
// agent stops serving plaintext and moves one port up, so forwarding only
// DefaultAgentPort makes a VM unreachable the moment it is provisioned.
const AgentPortSpan = 2

// ErrSocketVMNetNotFound reports that the socket_vmnet helper is not available.
var ErrSocketVMNetNotFound = errors.New("socket_vmnet helper not found")

// ErrPortInUse reports that the host port for the agent forward is taken.
var ErrPortInUse = errors.New("host port already in use")

// CheckHostPort reports whether the agent forwards can bind the whole span
// starting at port. Checked up front because QEMU's own failure names neither
// the port nor a remedy.
func CheckHostPort(port int) error {
	// Range first: a bind failure on an impossible port would otherwise be
	// reported as "already in use", sending the user to hunt for a process
	// that was never there. The span has to fit too, not just the base.
	if port < 1 || port+AgentPortSpan-1 > 65535 {
		return fmt.Errorf("port %d is out of range: use 1-%d, leaving room for the %d ports a VM occupies",
			port, 65535-AgentPortSpan+1, AgentPortSpan)
	}
	for i := range AgentPortSpan {
		l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port+i))
		if err != nil {
			return fmt.Errorf("%w: %d is taken (%v); pass --port to pick another",
				ErrPortInUse, port+i, err)
		}
		if err := l.Close(); err != nil {
			return err
		}
	}
	return nil
}

// PickHostPort returns the first free port span at or after preferred, trying
// tries spans. The span is the unit: a VM needs its plaintext and mTLS ports
// adjacent, so spans start AgentPortSpan apart rather than one.
func PickHostPort(preferred, tries int) (int, error) {
	if tries <= 0 {
		return 0, fmt.Errorf("no ports to try")
	}
	if preferred == 0 {
		preferred = DefaultAgentPort
	}
	var firstErr error
	for i := range tries {
		port := preferred + i*AgentPortSpan
		if err := CheckHostPort(port); err == nil {
			return port, nil
		} else if firstErr == nil {
			firstErr = err
		}
	}
	return 0, firstErr
}

// NetMode selects how the guest reaches the network. Explicit rather than
// inferred because it decides whether `wendy discover` can find the VM.
type NetMode string

const (
	// NetUser forwards the agent port over SLIRP. Needs no privileges, and
	// the VM is reachable only by the forwarded address: nothing on the
	// network can route to the guest. Its mDNS announcement does escape,
	// though -- SLIRP passes guest multicast out through a host socket, one
	// way -- so the host and its LAN hear a service whose address (10.0.2.15)
	// they cannot dial. The CLI recognises and hides that sighting.
	NetUser NetMode = "user"

	// NetShared puts the guest on a vmnet segment the host also has an
	// interface on, so mDNS reaches it and discovery works. Requires the
	// socket_vmnet helper.
	NetShared NetMode = "shared"
)

// ParseNetMode validates a user-supplied mode. Callers check it up front: the
// deeper rejection in NetConfig.args happens after the run lock is taken and
// the previous console log has already been rotated away.
func ParseNetMode(s string) (NetMode, error) {
	switch NetMode(s) {
	case NetUser:
		return NetUser, nil
	case NetShared:
		return NetShared, nil
	default:
		return "", fmt.Errorf("unknown network mode %q: use %q or %q", s, NetUser, NetShared)
	}
}

// NetConfig is a fully resolved network choice.
type NetConfig struct {
	Mode NetMode

	// AgentPort is the host port forwarded to the guest's agent in NetUser
	// mode. Ignored in NetShared mode, which needs no forward.
	AgentPort int

	// RegistryPort forwards the guest's container registry. Zero means the host
	// port was already taken, so the forward is skipped rather than failing the
	// whole start -- a developer running their own registry on 5000 must still
	// be able to boot a VM.
	RegistryPort int

	// SocketPath is the socket_vmnet endpoint in NetShared mode.
	SocketPath string

	// MAC determines the DHCP lease, and so the guest's address and mDNS
	// hostname. Required; use MACFor.
	MAC string
}

// SupportsDiscovery reports whether a guest on this network can be found by
// `wendy discover`.
func (n NetConfig) SupportsDiscovery() bool { return n.Mode == NetShared }

// MACFor preserves the legacy name-derived identity for existing VMs without
// a stored MAC. New VMs use a random, persisted address instead: the same name
// is common on different hosts sharing one LAN.
func MACFor(name string) string {
	sum := sha256.Sum256([]byte("wendy-vm:" + name))
	return fmt.Sprintf("52:%02x:%02x:%02x:%02x:%02x", sum[0], sum[1], sum[2], sum[3], sum[4])
}

func newMAC() (string, error) {
	mac := make(net.HardwareAddr, 6)
	if _, err := rand.Read(mac); err != nil {
		return "", err
	}
	mac[0] = (mac[0] & 0xfc) | 0x02 // locally administered, unicast
	return mac.String(), nil
}

// MACAddress never changes an existing VM's network identity on upgrade.
func (s *Store) MACAddress(name string) (string, error) {
	meta, ok := s.ReadMeta(name)
	if !ok {
		return "", fmt.Errorf("cannot read VM %q metadata to resolve its MAC address", name)
	}
	if meta.MAC == "" {
		return MACFor(name), nil
	}
	mac, err := net.ParseMAC(meta.MAC)
	if err != nil || len(mac) != 6 || mac[0]&3 != 2 {
		return "", fmt.Errorf("VM %q has an invalid locally-administered unicast MAC in metadata", name)
	}
	return mac.String(), nil
}

// Args returns the QEMU netdev and device arguments for this configuration.
func (n NetConfig) Args() ([]string, error) {
	// QEMU silently defaults the MAC, which would give every VM on the host the
	// same address.
	if n.MAC == "" {
		return nil, errors.New("no MAC address: use MACFor(name)")
	}
	device := "virtio-net-pci,netdev=net0,mac=" + n.MAC

	switch n.Mode {
	case NetUser:
		port := n.AgentPort
		if port == 0 {
			port = DefaultAgentPort
		}
		// Bind loopback explicitly: with no host address QEMU forwards on the
		// wildcard, exposing the agent, which is unauthenticated until the
		// device is provisioned. Both ports of the span are forwarded so the
		// VM stays reachable after provisioning moves the agent to mTLS.
		fwd := make([]string, 0, AgentPortSpan)
		for i := range AgentPortSpan {
			fwd = append(fwd, fmt.Sprintf("hostfwd=tcp:127.0.0.1:%d-:%d", port+i, DefaultAgentPort+i))
		}
		if n.RegistryPort != 0 {
			fwd = append(fwd, fmt.Sprintf("hostfwd=tcp:127.0.0.1:%d-:%d", n.RegistryPort, DeviceRegistryPort))
		}
		return []string{
			"-netdev", "user,id=net0," + strings.Join(fwd, ","),
			"-device", device,
		}, nil

	case NetShared:
		if n.SocketPath == "" {
			return nil, errors.New("shared networking needs a socket_vmnet path")
		}
		// The helper owns the privileged vmnet handle, so QEMU itself stays
		// unprivileged; -netdev vmnet-shared would need root.
		return []string{
			"-netdev", fmt.Sprintf("stream,id=net0,addr.type=unix,addr.path=%s", escapeQEMUValue(n.SocketPath)),
			"-device", device,
		}, nil

	default:
		return nil, fmt.Errorf("unknown network mode %q: use %q or %q", n.Mode, NetUser, NetShared)
	}
}

// SocketVMNetCandidates lists the helper's socket paths, most likely first.
func SocketVMNetCandidates(brewPrefix string) []string {
	var out []string
	if brewPrefix != "" {
		out = append(out, brewPrefix+"/var/run/socket_vmnet")
	}
	return append(out,
		"/opt/homebrew/var/run/socket_vmnet",
		"/usr/local/var/run/socket_vmnet",
		"/var/run/socket_vmnet",
	)
}

// ErrSharedNetUnsupported reports that shared networking cannot work on this
// host at all.
// DeviceRegistryPort is where a Linux device serves its container registry --
// commands/helpers.go registryPort(). `wendy run` falls back to a registry push
// when chunked deploy is unavailable, and it addresses the device by host and
// this port, so the guest's 5000 has to be reachable at the host's 5000 or the
// fallback cannot connect at all.
const DeviceRegistryPort = 5000

var ErrSharedNetUnsupported = errors.New("shared networking needs macOS")

// FindSocketVMNet returns the first socket_vmnet endpoint present on the host.
//
// vmnet is an Apple framework, so anywhere else the answer is not "install the
// helper" but "this mode does not exist here" -- the error used to recommend
// Homebrew commands on machines with no Homebrew.
func FindSocketVMNet(goos, brewPrefix string) (string, error) {
	if goos != "darwin" {
		return "", fmt.Errorf("%w: socket_vmnet builds on Apple's vmnet framework. "+
			"Use --net user and reach the VM by address", ErrSharedNetUnsupported)
	}
	candidates := SocketVMNetCandidates(brewPrefix)
	for _, path := range candidates {
		if _, err := vmStat(path); err == nil {
			return path, nil
		}
	}
	return "", fmt.Errorf("%w: install and start it with 'brew install socket_vmnet' "+
		"and 'sudo brew services start socket_vmnet', or use --net user "+
		"(no setup, but `wendy discover` cannot see the VM). Looked in: %s",
		ErrSocketVMNetNotFound, strings.Join(candidates, ", "))
}
