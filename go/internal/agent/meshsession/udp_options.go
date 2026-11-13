package meshsession

import (
	"context"
	"net/netip"
	"sync"

	"github.com/wendylabsinc/wendy/go/internal/agent/udpmessage"
)

const MaxUDPMessageSize = udpmessage.MaxMessageSize
const MaxUDPTransportChunk = udpmessage.MaxChunkSize

// UDPSourceClaim is immutable proof input, not a grant. The registry checks it
// again at every queued fragment admission and every final local delivery.
type UDPSourceClaim struct {
	IP, VIP netip.Addr
	IfIndex int
	Token   uint64
}
type UDPOptions struct {
	Manager  *udpmessage.Manager
	Endpoint *udpmessage.Endpoint
	Guard    udpmessage.Guard
	Done     <-chan struct{}
	Source   *UDPSourceClaim
}

func udpOptions(options []UDPOptions) UDPOptions {
	if len(options) == 0 {
		return UDPOptions{}
	}
	return options[0]
}

// Only one scanner consumes an endpoint's chunks/socket at a time. Waiting for
// another receiver must still honor cancellation and endpoint retirement.
type udpReceiveLock struct {
	once sync.Once
	gate chan struct{}
}

func (l *udpReceiveLock) lock(ctx context.Context, retired <-chan struct{}) error {
	l.once.Do(func() { l.gate = make(chan struct{}, 1) })
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case l.gate <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-retired:
		return udpmessage.ErrClosed
	}
}
func (l *udpReceiveLock) unlock() { <-l.gate }
