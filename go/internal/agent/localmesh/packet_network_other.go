//go:build !linux

package localmesh

import (
	"errors"

	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

func newHostNetwork(meshid.Device) (PacketNetwork, error) {
	return nil, errors.New("host mesh routing requires Linux; desktop clients must supply a userspace network")
}
