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
	Self       meshid.Device
	CapturedAt time.Time
	Peers      int
	Links      []PeerLink
	Devices    []Manifest
	Routes     []babel.Route
	// ForwardingRoutes contains the selected routes installed in the kernel.
	ForwardingRoutes    []babel.Route
	RouteAuthorizations map[RouteOrigin]bool
	RouteProofs         map[RouteOrigin]RouteProof // Exact current/compact-handoff receiver lease metadata.
}

// PeerLink identifies an authenticated direct carrier and its Babel cost.
// Multiple carriers to one device remain distinct routing links.
type PeerLink struct {
	ID        babel.LinkID
	Device    meshid.Device
	Cost      uint16
	Transport string
	// ForwardingReady is local runtime evidence, never a discovery claim. The
	// authenticated neighbour is Babel-reachable and its current authorized /128
	// is selected and installed through this exact carrier. Only such a link may
	// make a useful slower carrier redundant.
	ForwardingReady bool
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
	case "ethernet", "infrastructure-wifi", "tcp", "nan", "ble", "quic", "tls-stream":
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
		if link.Device == device && link.Cost < cost && link.ForwardingReady {
			return true
		}
	}
	return false
}

// markForwardingReady joins actor-owned routing and forwarding state. Attached
// TLS presence alone survives a silent blackhole; a cached or uninstalled route
// likewise cannot justify retiring the carrier that still forwards traffic.
func markForwardingReady(links []PeerLink, routing babel.Snapshot, installed []babel.Route, origins map[RouteOrigin]bool) {
	reachable := make(map[babel.LinkID]bool, len(routing.Neighbours))
	for _, neighbour := range routing.Neighbours {
		reachable[neighbour.Link] = neighbour.Reachable
	}
	peers := make(map[babel.LinkID]int, len(links))
	for i := range links {
		links[i].ForwardingReady = false
		peers[links[i].ID] = i
	}
	// Retain only direct peer routes here, rather than copying the whole mesh
	// FIB into another map on each routing refresh.
	selected := make(map[babel.LinkID]babel.Route, len(links))
	for _, route := range routing.Routes {
		i, attached := peers[route.Link]
		if !attached || !reachable[route.Link] || route.Local || route.Unreachable || route.Metric == babel.Infinity {
			continue
		}
		peer := links[i].Device
		if !peer.IsValid() || route.Prefix.Bits() != 128 || route.Prefix.Addr() != peer.MeshAddress() {
			continue
		}
		if origins[RouteOrigin{Prefix: route.Prefix, RouterID: route.RouterID, Seqno: route.Seqno}] {
			selected[route.Link] = route
		}
	}
	for _, route := range installed {
		if current, ok := selected[route.Link]; ok && current == route {
			links[peers[route.Link]].ForwardingReady = true
		}
	}
}
