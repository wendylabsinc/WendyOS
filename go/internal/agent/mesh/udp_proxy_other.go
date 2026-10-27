//go:build !linux

package mesh

import (
	"errors"
	"net"

	"github.com/wendylabsinc/wendy/go/internal/agent/udpmessage"
)

type UDPProxy struct{}

func NewUDPProxy(UDPDialer, UDPSourcePolicy, *VIPTable, ...*udpmessage.Manager) (*UDPProxy, error) {
	return nil, errors.New("Wendy Net UDP proxy requires Linux")
}
func (*UDPProxy) Start(string) error { return errors.New("Wendy Net UDP proxy requires Linux") }
func (*UDPProxy) Close() error       { return nil }
func (*UDPProxy) Addr() net.Addr     { return nil }
