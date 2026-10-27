package localmesh

import (
	"context"
	"github.com/wendylabsinc/WendyOS/go/babel"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	"time"
)

// NodeSnapshot reports authenticated device presence and installed routes.
// It feeds internal policy and a bounded public diagnostic projection. It is
// not an app service directory.
type NodeSnapshot struct {
	Self                meshid.Device
	CapturedAt          time.Time
	Peers               int
	Links               []PeerLink
	Devices             []Manifest
	Routes              []babel.Route
	RouteAuthorizations map[RouteOrigin]bool
}

// PeerLink identifies an authenticated direct carrier and its Babel cost.
// Multiple carriers to one device remain distinct routing links.
type PeerLink struct {
	ID        babel.LinkID
	Device    meshid.Device
	Cost      uint16
	Transport string
	// CarrierIP reports router QUIC enqueue/capability diagnostics.
	CarrierIP *CarrierIPStats
	carrier   *carrierIPSender
	// Reassembly is a consistent, current hop-local receive snapshot.
	Reassembly *IPAssemblyStats
	assembler  *IPAssembler
	// SendQueue is populated for a framed byte-stream link when Snapshot is read.
	SendQueue *IPQueueStats
	queue     ipQueueSnapshotter
}

type carrierContextKey struct{}

// WithCarrier labels a carrier's established connection for diagnostics. It
// carries no authentication or routing authority and never changes link cost.
func WithCarrier(ctx context.Context, carrier string) context.Context {
	return context.WithValue(ctx, carrierContextKey{}, carrier)
}

func carrierName(ctx context.Context, fallback string) string {
	carrier, _ := ctx.Value(carrierContextKey{}).(string)
	switch carrier {
	case "ethernet", "infrastructure-wifi", "tcp", "nan", "ble", "quic", "tls-stream", "leaf":
		return carrier
	default:
		return fallback
	}
}

// IPQueueStats reports BLE routed-packet admission, drops, and live occupancy.
// Counts do not include the packet currently being written or kernel buffers.
type IPQueueStats struct {
	Bytes, Packets                        int
	ByteLimit                             int
	Enqueued, Dequeued                    uint64
	DroppedCapacity, DroppedExpired       uint64
	LastDequeuedWaitMS, MaxDequeuedWaitMS uint64
	AgeLimitMS                            uint64
	WriteRateBytesPerSec                  int
}

type ipQueueSnapshotter interface{ snapshot() IPQueueStats }

func (s NodeSnapshot) HasCheaperLink(device meshid.Device, cost uint16) bool {
	for _, link := range s.Links {
		if link.Device == device && link.Cost < cost {
			return true
		}
	}
	return false
}
