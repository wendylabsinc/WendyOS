package localmesh

import (
	"context"
	"errors"
	"time"

	quic "github.com/quic-go/quic-go"
)

type LinkHello struct {
	Version          int `json:"version"`
	Org, Asset, Peer int32
	MTU              int `json:"mtu"`
	Datagram         int `json:"datagram"`
}

func QUICConfig() *quic.Config {
	return &quic.Config{EnableDatagrams: true, Allow0RTT: false, HandshakeIdleTimeout: 5 * time.Second, MaxIdleTimeout: 20 * time.Second, KeepAlivePeriod: 5 * time.Second,
		MaxIncomingStreams: 1, MaxIncomingUniStreams: -1, InitialStreamReceiveWindow: MaxControlMessage + 4, MaxStreamReceiveWindow: 2 * MaxControlMessage,
		InitialConnectionReceiveWindow: 2 * MaxControlMessage, MaxConnectionReceiveWindow: 2 * MaxControlMessage}
}

// OpenControl binds the fresh protocol to the mutually authenticated identity.
// The lower asset opens the sole bidirectional stream, retained for directory
// synchronization. It cannot interoperate accidentally with Ethernet/BATMAN.
func OpenControl(ctx context.Context, conn *quic.Conn, org, self, peer int32) (*quic.Stream, error) {
	if _, _, err := Addresses(org, self); err != nil {
		return nil, err
	}
	if _, _, err := Addresses(org, peer); err != nil {
		return nil, err
	}
	if self == peer {
		return nil, errors.New("self link")
	}
	state := conn.ConnectionState()
	if state.TLS.NegotiatedProtocol != LinkALPN || !state.SupportsDatagrams.Local || !state.SupportsDatagrams.Remote {
		return nil, errors.New("incompatible local-mesh transport")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var stream *quic.Stream
	var err error
	if self < peer {
		stream, err = conn.OpenStreamSync(ctx)
	} else {
		stream, err = conn.AcceptStream(ctx)
	}
	if err != nil {
		return nil, err
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
	if err = WriteControl(stream, ControlMessage{Kind: "hello", Hello: &LinkHello{Version: 1, Org: org, Asset: self, Peer: peer, MTU: TunnelMTU, Datagram: DatagramLimit}}); err != nil {
		return nil, err
	}
	m, err := ReadControl(stream)
	if err != nil {
		return nil, err
	}
	if m.Kind != "hello" || m.Hello == nil {
		return nil, errors.New("missing local-mesh hello")
	}
	h := m.Hello
	if h.Version != 1 || h.Org != org || h.Asset != peer || h.Peer != self || h.MTU != TunnelMTU || h.Datagram != DatagramLimit {
		return nil, errors.New("invalid local-mesh hello")
	}
	_ = stream.SetDeadline(time.Time{})
	ok = true
	return stream, nil
}
