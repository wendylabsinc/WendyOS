package localmesh

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	quic "github.com/quic-go/quic-go"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

// LinkHello names both ends by device UUID. The TLS handshake already pinned the
// full principal, so the hello only has to prove the peer read the pinned
// identity correctly - there is no numeric identity to announce.
type LinkHello struct {
	Version  int       `json:"version" cbor:"v"`
	Role     string    `json:"role,omitempty" cbor:"r,omitempty"`
	Device   uuid.UUID `json:"device" cbor:"d"`
	Peer     uuid.UUID `json:"peer" cbor:"p"`
	MTU      int       `json:"mtu" cbor:"m"`
	Datagram int       `json:"datagram" cbor:"g"`
	// IPDatagram is router-QUIC-only. Stream peers must leave it zero.
	IPDatagram int `json:"ipDatagram,omitempty" cbor:"i,omitempty"`
}

// MaxQUICCryptoStreamSize bounds TLS data at each encryption level while
// allowing enrolled Cloud v2 certificate chains without weakening verification.
const MaxQUICCryptoStreamSize = 64 << 10

func QUICConfig() *quic.Config {
	return &quic.Config{InitialPacketSize: 1200, MaxPacketSize: carrierPacketLimit, MaxCryptoStreamSize: MaxQUICCryptoStreamSize, EnableDatagrams: true, Allow0RTT: false, HandshakeIdleTimeout: 5 * time.Second, MaxIdleTimeout: 20 * time.Second, KeepAlivePeriod: 5 * time.Second,
		MaxIncomingStreams: 1, MaxIncomingUniStreams: -1, InitialStreamReceiveWindow: MaxControlMessage + 4, MaxStreamReceiveWindow: 2 * MaxControlMessage,
		InitialConnectionReceiveWindow: 2 * MaxControlMessage, MaxConnectionReceiveWindow: 2 * MaxControlMessage}
}

// OpenControl binds the fresh protocol to the mutually authenticated identity.
// The device with the lower UUID opens the sole bidirectional stream, retained
// for directory synchronization; the authenticated hello also bounds each
// direction's IP receive capability. It cannot interoperate accidentally with
// older router protocols or stream carriers.
func OpenControl(ctx context.Context, conn *quic.Conn, self, peer meshid.Device) (*quic.Stream, error) {
	stream, _, err := openRouterControl(ctx, conn, self, peer, WholeIPDatagramLimit)
	return stream, err
}

// openRouterControl returns the peer's bounded receive capability only after
// the authenticated hello succeeds. The local receive limit is independently
// advertised; it never comes from discovery or from the current outbound PMTU.
func openRouterControl(ctx context.Context, conn *quic.Conn, self, peer meshid.Device, receiveIP int) (*quic.Stream, int, error) {
	if !validIPDatagramLimit(receiveIP) {
		return nil, 0, errors.New("invalid local router IP capability")
	}
	if self.Principal() == peer.Principal() {
		return nil, 0, errors.New("self link")
	}
	if self.Tenant() != peer.Tenant() {
		return nil, 0, errors.New("wendy net peer is outside the tenant")
	}
	state := conn.ConnectionState()
	if state.TLS.NegotiatedProtocol != LinkALPN || !state.SupportsDatagrams.Local || !state.SupportsDatagrams.Remote {
		return nil, 0, errors.New("incompatible wendy net transport")
	}
	capacity, err := conn.CurrentMaxDatagramPayloadSize()
	if err != nil {
		return nil, 0, err
	}
	if capacity < DatagramLimit {
		return nil, 0, errors.New("router transport cannot carry baseline datagrams")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var stream *quic.Stream
	if meshid.Compare(self, peer) < 0 {
		stream, err = conn.OpenStreamSync(ctx)
	} else {
		stream, err = conn.AcceptStream(ctx)
	}
	if err != nil {
		return nil, 0, err
	}
	ok := false
	defer func() {
		if !ok {
			stream.CancelRead(1)
			stream.CancelWrite(1)
		}
	}()
	deadline, _ := ctx.Deadline()
	_ = stream.SetDeadline(deadline)
	if err = WriteControl(stream, ControlMessage{Kind: "hello", Hello: &LinkHello{Version: LinkProtocolVersion, Device: self.UUID(), Peer: peer.UUID(), MTU: TunnelMTU, Datagram: DatagramLimit, IPDatagram: receiveIP}}); err != nil {
		return nil, 0, err
	}
	m, err := ReadControl(stream)
	if err != nil {
		return nil, 0, err
	}
	if m.Kind != "hello" || m.Hello == nil {
		return nil, 0, errors.New("missing wendy net hello")
	}
	h := m.Hello
	if !validRouterHello(h, self, peer) {
		return nil, 0, errors.New("invalid wendy net hello")
	}
	_ = stream.SetDeadline(time.Time{})
	ok = true
	return stream, h.IPDatagram, nil
}

func validIPDatagramLimit(limit int) bool {
	return limit == DatagramLimit || limit == WholeIPDatagramLimit
}

func validRouterHello(h *LinkHello, self, peer meshid.Device) bool {
	return h != nil && h.Version == LinkProtocolVersion && h.Device == peer.UUID() && h.Peer == self.UUID() && h.MTU == TunnelMTU && h.Datagram == DatagramLimit && validIPDatagramLimit(h.IPDatagram)
}
