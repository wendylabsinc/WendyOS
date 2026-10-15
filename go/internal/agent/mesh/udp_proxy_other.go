//go:build !linux

package mesh

import (
	"context"
	"errors"
	"net"

	"github.com/wendylabsinc/wendy/go/internal/agent/meshingress"
)

const UDPProxyPort = 50059

type UDPFlow interface {
	Send([]byte) error
	Receive(context.Context) ([]byte, error)
	Close() error
}

type UDPDialFunc func(context.Context, int32, uint16) (UDPFlow, error)
type UDPProxy struct{}

func NewUDPProxy(UDPDialFunc, *meshingress.Registry) (*UDPProxy, error) {
	return nil, errors.New("mesh UDP VIP proxy requires Linux")
}
func (*UDPProxy) Start(string) error { return errors.New("mesh UDP VIP proxy requires Linux") }
func (*UDPProxy) Close() error       { return nil }
func (*UDPProxy) Addr() net.Addr     { return nil }
