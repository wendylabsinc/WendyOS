package localmesh

import (
	"crypto/rand"
	"net"
	"net/netip"
	"sort"
	"strings"

	"github.com/wendylabsinc/WendyOS/go/babel"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshoperator"
)

// Interface names this package owns. Declared here, rather than in the Linux
// adapter, so router-ID derivation can exclude its own interfaces: the dummy
// host link and every TUN carry a freshly randomised address on each boot, and
// deriving a router ID from one would make it change on every restart.
const (
	InterfaceName       = "wlmesh0"
	peerInterfacePrefix = "wlmp"
)

func ownedInterface(name string) bool {
	return name == InterfaceName || strings.HasPrefix(name, peerInterfacePrefix)
}

// admittedPrefix is only the structural boundary. Device announcements may
// authorize their exact derived /128, never an aggregate. The node separately
// requires a live signed origin/router-ID/sequence binding for every route.
func admittedPrefix(p netip.Prefix) bool {
	return (meshid.InMesh(p.Addr()) || meshoperator.InOperatorPool(p.Addr())) && p.Bits() == 128
}

// instanceRouterID derives this instance's Babel router ID with the profile's
// Stage-1 helpers: the permanent hardware address in the 0x0000||MAC48 form when
// a usable one exists, otherwise the tagged-random 0x0001||RANDOM48 form.
//
// The result is a routing identity, not a device identity: it is never derived
// from a device UUID, a hash or a manifest, and it is pinned for the identity's
// lifetime by PersistentState (see OpenStateStore), so a tagged-random fallback
// does not change on restart.
func instanceRouterID() (babel.RouterID, error) {
	if mac, ok := permanentMAC(); ok {
		if id, err := babel.RouterIDFromMAC(mac); err == nil {
			return id, nil
		}
	}
	var seed [6]byte
	if _, err := rand.Read(seed[:]); err != nil {
		return 0, err
	}
	return babel.RouterIDFromRandom(seed), nil
}

// permanentMAC selects a factory address by interface name, independent of
// carrier state. If the platform cannot establish permanence, use random state.
func permanentMAC() (net.HardwareAddr, bool) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, false
	}
	names := make([]string, 0, len(ifaces))
	for _, iface := range ifaces {
		if iface.Flags&net.FlagLoopback != 0 || ownedInterface(iface.Name) {
			continue
		}
		names = append(names, iface.Name)
	}
	sort.Strings(names)
	for _, name := range names {
		mac, ok := persistentMAC(name)
		if ok && usableMAC(mac) {
			return mac, true
		}
	}
	return nil, false
}

func usableMAC(mac net.HardwareAddr) bool {
	if len(mac) != 6 || mac[0]&1 != 0 {
		return false
	}
	zero := true
	for _, b := range mac {
		if b != 0 {
			zero = false
			break
		}
	}
	return !zero
}
