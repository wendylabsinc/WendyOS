package meshsession

import (
	"net"
	"sync"

	"github.com/quic-go/quic-go"
	"github.com/wendylabsinc/wendy/go/internal/agent/udpmessage"
)

// A direct epoch can own both namespaces' full slot allowance. Reserve a
// separate control stream in addition to those flows and ordinary app capacity.
// This is enabled only when the retained manager is installed on the listener.
func (s *Server) promotionStreamCapacity() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.direct == nil {
		return 0
	}
	slots := s.direct.cfg.FlowSlots
	if slots == 0 {
		slots = 16
	}
	return 2*slots + 1
}

// promotedQUICStream never owns the shared QUIC connection. Full cancellation
// interrupts blocked writes; an already successful send half-close preserves
// queued data/FIN until QUIC acknowledges it instead of resetting that suffix.
type promotedQUICStream struct {
	mux        *udpMultiplexer
	udpManager *udpmessage.Manager
	*quic.Stream
	conn               *quic.Conn
	mu                 sync.Mutex
	closed, sendClosed bool
}

func (s *promotedQUICStream) LocalAddr() net.Addr  { return s.conn.LocalAddr() }
func (s *promotedQUICStream) RemoteAddr() net.Addr { return s.conn.RemoteAddr() }
func (s *promotedQUICStream) CloseWrite() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return net.ErrClosed
	}
	if s.sendClosed {
		return nil
	}
	if err := s.Stream.Close(); err != nil {
		return err
	}
	s.sendClosed = true
	return nil
}
func (s *promotedQUICStream) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	s.Stream.CancelRead(0)
	if !s.sendClosed {
		s.Stream.CancelWrite(0)
	}
	return nil
}
