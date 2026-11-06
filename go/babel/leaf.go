package babel

import (
	"errors"
	"net/netip"

	"github.com/wendylabsinc/WendyOS/go/babel/internal/wire"
)

// ValidateLeafPacket confines a non-transit peer to advertising or retracting
// its own exact host prefix. It uses the same decoder as Engine, including
// packet-local prefix compression and Router-ID/Next-Hop context. The caller
// must separately authenticate the peer and authorize each finite signed origin.
// Wildcard route requests only ask for information; wildcard retractions and
// requests to advance other origins' sequence numbers are refused.
func ValidateLeafPacket(packet []byte, peer netip.Addr, own netip.Prefix) error {
	if !peer.Is6() || !peer.IsLinkLocalUnicast() || !validPrefix(own) || !own.Addr().Is6() || own.Bits() != 128 {
		return ErrConfig
	}
	messages, err := wire.Decode(packet, peer)
	if err != nil {
		return err
	}
	for _, m := range messages {
		switch m.Type {
		case wire.Update:
			if m.Wildcard || m.Prefix != own || (m.Metric != Infinity && m.Address != peer) {
				return errors.New("Babel leaf may advertise or retract only its own host route")
			}
		case wire.Request, wire.SeqRequest:
			if !(m.Type == wire.Request && m.Wildcard) && m.Prefix != own {
				return errors.New("Babel leaf requested another origin's route")
			}
		}
	}
	return nil
}
