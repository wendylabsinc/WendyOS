package lite

import (
	"time"

	wendypb "github.com/wendylabsinc/WendyOS/go/wendy-core/core/lite/pb"
)

// wcomLink transports whole WendyComMessages; implementations own framing.
// The adapter marshals bytes through the host-owned framing and connection.
type wcomLink interface {
	// linkHandshake performs any transport-level handshake needed before
	// WendyCom messages can flow. Called once, before the protocol handshake.
	linkHandshake() error
	// send marshals and transmits one message; safe for concurrent use.
	send(*wendypb.WendyComMessage) error
	// recv blocks for the next message; timeout <= 0 means no deadline.
	// Called only by the client's single reader (handshake, then readLoop).
	recv(timeout time.Duration) (*wendypb.WendyComMessage, error)
	// preferredChunkSize is the transport's preferred app-push chunk size. It
	// is a throughput preference, not a limit callers must respect.
	preferredChunkSize() int
	close() error
}
