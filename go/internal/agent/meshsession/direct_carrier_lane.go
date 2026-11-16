package meshsession

import (
	"context"
	"encoding/binary"
	"math"

	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/shared/directstream"
)

// directCarrierLane owns the receive handoff between successive logical epochs
// on one TLS incarnation. The session reader must be joined before the reuse proposal. Only
// the carrier handler proposes and advances; the active session reads alone.
// This adapter does not decide whether retirement or reuse is permitted.
type directCarrierLane struct {
	localmesh.DirectRecordCarrier
	alias   uint32
	pending []byte
}

func (c *directCarrierLane) ReceiveDirectRecord(ctx context.Context) ([]byte, error) {
	for {
		var wire []byte
		var err error
		if c.pending != nil {
			wire, c.pending = c.pending, nil
		} else {
			wire, err = c.DirectRecordCarrier.ReceiveDirectRecord(ctx)
		}
		if err != nil {
			return nil, err
		}
		if len(wire) < 5 || len(wire) > directstream.MaxFlowFrame || binary.BigEndian.Uint32(wire[1:5]) == 0 {
			return nil, directstream.ErrBounds
		}
		if binary.BigEndian.Uint32(wire[1:5]) < c.alias {
			continue
		}
		return wire, nil
	}
}

func (c *directCarrierLane) advance() error {
	if c.alias == math.MaxUint32 {
		return directstream.ErrBounds
	}
	c.alias++
	return nil
}
