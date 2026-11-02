//go:build linux

package localmesh

import (
	"io"

	"github.com/wendylabsinc/WendyOS/go/babel"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

type hostNetwork struct{ *Kernel }

func newHostNetwork(self meshid.Device) (PacketNetwork, error) {
	k, e := NewKernel(self)
	if e != nil {
		return nil, e
	}
	return &hostNetwork{k}, nil
}
func (k *hostNetwork) AddPeer(id babel.LinkID, peer meshid.Device) (io.ReadWriteCloser, error) {
	return k.Kernel.AddPeer(id, peer)
}
