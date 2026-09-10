package liteclient

import (
	"time"

	wendypb "github.com/wendylabsinc/wendy/go/proto/gen/litepb"
)

// Per-transport app-push chunk sizes. WendyCom acknowledges every AppPushData,
// so throughput is chunk/RTT and a larger chunk is a straight win wherever the
// transport can carry it. On the TLS transports that is all this is: a tuning
// choice, bounded only by maxTLSRecordSize in link_direct.go.
//
// Serial is the exception, and there the number is a limit rather than a
// preference. The device's USB Serial/JTAG input buffer has no backpressure, so
// a larger chunk overruns it and loses bytes instead of being throttled. Until
// the firmware gains flow control, treat this as a ceiling, not a knob.
const (
	chunkSize          = 4096
	chunkSizeForSerial = 768
)

// wcomLink transports whole WendyComMessages; implementations own framing.
// directLink frames messages itself over TCP-TLS or serial; tunnelLink
// exchanges bare message bodies through a cloud gRPC tunnel, where the
// broker owns framing and channels.
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
