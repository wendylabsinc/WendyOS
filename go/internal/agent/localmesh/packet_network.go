package localmesh

import (
	"io"

	"github.com/wendylabsinc/WendyOS/go/babel"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

// PacketNetwork installs authenticated host routes and exchanges IPv6 packets
// with each carrier. A desktop supplies a private userspace network; agents use
// their existing kernel interfaces and forwarding policy.
type PacketNetwork interface {
	ReconcileBase() error
	AddPeer(babel.LinkID, meshid.Device) (io.ReadWriteCloser, error)
	RemovePeer(babel.LinkID) error
	Apply([]babel.Route) error
	Stop() error
	Close() error
}
