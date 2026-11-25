package mesh

import (
	"context"
	"net/netip"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/meshsession"
	"github.com/wendylabsinc/wendy/go/internal/agent/udpmessage"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

const UDPProxyPort = 50059
const udpFlowLimit = 256
const udpFlowIdle = 30 * time.Second

// Cover the TLS certificate flights and app-port setup on routed BLE paths.
// Established flows retain their separate, shorter idle timeout.
const udpDialTimeout = meshsession.TLSAppSessionSetupTimeout

// UDPFlow sends and receives complete unreliable UDP messages, including empty.
// Every received Message must be released after final local delivery or discard.
type UDPFlow interface {
	Send([]byte) error
	Receive(context.Context) (*udpmessage.Message, error)
	Close() error
}
type UDPDialRequest struct {
	Peer meshid.Device
	Port uint16
	// Endpoint already owns queued app messages. The dialer must attach this
	// endpoint through UDPOptions, rather than create a second message queue.
	Endpoint *udpmessage.Endpoint
	Source   meshsession.UDPSourceClaim
	Done     <-chan struct{}
}
type UDPDialer interface {
	DialUDP(context.Context, UDPDialRequest) (UDPFlow, error)
}
type UDPDialFunc func(context.Context, UDPDialRequest) (UDPFlow, error)

func (f UDPDialFunc) DialUDP(ctx context.Context, r UDPDialRequest) (UDPFlow, error) {
	return f(ctx, r)
}

type UDPSourcePolicy interface {
	SourceToken(netip.Addr, netip.Addr, int) uint64
	SourceDone(netip.Addr, netip.Addr, int, uint64) <-chan struct{}
	WithSourceToken(netip.Addr, netip.Addr, int, uint64, func() error) error
}
