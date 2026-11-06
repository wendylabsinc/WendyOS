package meshsession

import (
	"github.com/wendylabsinc/wendy/go/internal/agent/meshingress"
	"net"
)

// Unit tests map an admitted private endpoint to their loopback listener;
// real CNI endpoint routing is verified in the isolated Linux namespace tests.
type loopbackRegistry struct{ *meshingress.Registry }

func (r *loopbackRegistry) DialAuthorized(port uint16, dial func(string) (net.Conn, error)) (net.Conn, error) {
	return r.Registry.DialAuthorized(port, func(address string) (net.Conn, error) {
		_, p, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		return dial(net.JoinHostPort("127.0.0.1", p))
	})
}
