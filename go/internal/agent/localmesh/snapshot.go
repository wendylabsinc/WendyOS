package localmesh

import "github.com/wendylabsinc/WendyOS/babel"

// NodeSnapshot reports authenticated device presence and installed routes.
// It is internal to internet-sharing policy, not an app service directory.
type NodeSnapshot struct {
	Peers   int
	Links   []PeerLink
	Devices []Manifest
	Routes  []babel.Route
}

// PeerLink identifies an authenticated direct carrier and its Babel cost.
// Multiple carriers to one asset remain distinct routing links.
type PeerLink struct {
	Asset int32
	Cost  uint16
	// SendQueue is populated for a BLE byte-stream link when Snapshot is read.
	SendQueue *IPQueueStats
	queue     ipQueueSnapshotter
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

func (s NodeSnapshot) HasCheaperLink(asset int32, cost uint16) bool {
	for _, link := range s.Links {
		if link.Asset == asset && link.Cost < cost {
			return true
		}
	}
	return false
}
