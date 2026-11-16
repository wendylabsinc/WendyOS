package meshsession

import (
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	"net"
)

func (s *inboundPromotedStream) openDatagrams(owner net.Conn, peer meshid.Device, maximum uint16) (net.Conn, error) {
	factory, ok := s.PromotedStream.(directDatagramFactory)
	if !ok {
		return nil, ErrUDPUnsupported
	}
	return factory.openDatagrams(owner, peer, maximum)
}
func (s *promotedQUICStream) openDatagrams(owner net.Conn, peer meshid.Device, maximum uint16) (net.Conn, error) {
	if s.mux == nil || s.udpManager == nil || maximum == 0 || !udpDatagramsSupported(s.conn) {
		return nil, ErrUDPUnsupported
	}
	endpoint, err := s.udpManager.Open(peer, func(send func() error) error { return send() })
	if err != nil {
		return nil, err
	}
	flow, err := s.mux.register(s.Stream, endpoint, false, nil)
	if err != nil {
		endpoint.Close()
		return nil, err
	}
	if err = endpoint.SetTransport(maxMuxUDPChunk, false, multiplexUDPAdmission(s.conn, flow.id)); err != nil {
		flow.close()
		return nil, err
	}
	association := newDirectQUICDatagramConn(owner, flow, maximum)
	go flow.watchControl()
	go flow.watchLifetime(s.conn.Context(), nil, false)
	return association, nil
}
