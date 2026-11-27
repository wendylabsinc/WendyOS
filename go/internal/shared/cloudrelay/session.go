package cloudrelay

import (
	"context"
	"crypto/ecdsa"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	pb "github.com/wendylabsinc/wendy/go/proto/gen/relaypb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

type Session struct {
	ctx    context.Context
	stream pb.TunnelBrokerV2Service_JoinSessionClient
	cancel context.CancelFunc
	conn   *grpc.ClientConn
	mu     sync.Mutex
	closed bool
}

func join(parent context.Context, broker *pb.BrokerInstance, artifact string, c claims, key *ecdsa.PrivateKey, role pb.JoinRole, dial func(string) (*grpc.ClientConn, error)) (result *Session, err error) {
	ctx, cancel := context.WithDeadline(parent, time.Unix(c.RelayExp, 0))
	ctx, cancelAdmission := context.WithCancelCause(ctx)
	ctx = metadata.NewOutgoingContext(ctx, metadata.MD{})
	conn, err := dial(broker.Endpoint)
	if err != nil {
		cancel()
		return nil, err
	}
	s := &Session{ctx: ctx, cancel: cancel, conn: conn}
	// Admission must finish before the grant expires; the relay has a later deadline.
	started := time.Now()
	timerDone := make(chan struct{})
	timer := time.AfterFunc(time.Until(time.Unix(c.Exp, 0)), func() {
		cancelAdmission(errAdmissionTimeout)
		close(timerDone)
	})
	defer func() {
		if !timer.Stop() {
			<-timerDone
		}
		if err == nil {
			err = ctx.Err()
		}
		if err != nil {
			if role == pb.JoinRole_JOIN_ROLE_CALLER {
				err = admissionError(parent, context.Cause(ctx), started, err)
			}
			s.Close()
			result = nil
		}
	}()
	s.stream, err = pb.NewTunnelBrokerV2ServiceClient(conn).JoinSession(ctx)
	if err != nil {
		return nil, err
	}
	if err = s.stream.Send(&pb.JoinSessionRequest{Message: &pb.JoinSessionRequest_Open{Open: &pb.JoinOpen{SessionGrantJws: artifact, Role: role}}}); err != nil {
		return nil, err
	}
	msg, err := s.stream.Recv()
	if err != nil {
		return nil, fmt.Errorf("joining Cloud relay: %w", err)
	}
	label := "caller"
	if role == pb.JoinRole_JOIN_ROLE_AGENT {
		label = "agent"
	}
	sig, err := proof(key, "join", c.Aud, c.Session, c.JTI, artifact, label, msg.GetChallenge())
	if err != nil {
		return nil, err
	}
	if err = s.stream.Send(&pb.JoinSessionRequest{Message: &pb.JoinSessionRequest_Proof{Proof: &pb.JoinProof{ChallengeId: msg.GetChallenge().ChallengeId, Signature: sig}}}); err != nil {
		return nil, err
	}
	msg, err = s.stream.Recv()
	if err != nil {
		return nil, err
	}
	a := msg.GetAccepted()
	if a == nil || a.Role != role || a.RelayExpiresAt == nil || a.RelayExpiresAt.CheckValid() != nil || a.RelayExpiresAt.AsTime().Unix() != c.RelayExp {
		return nil, fmt.Errorf("invalid relay join acceptance")
	}
	return s, nil
}
func (s *Session) Close() error { s.cancel(); return s.conn.Close() }
func (s *Session) Send(payload []byte, halfClose bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return io.ErrClosedPipe
	}
	for len(payload) > 0 {
		n := min(len(payload), 65536)
		if err := s.stream.Send(&pb.JoinSessionRequest{Message: &pb.JoinSessionRequest_Frame{Frame: &pb.TunnelFrame{Frame: &pb.TunnelFrame_Data{Data: payload[:n]}}}}); err != nil {
			return err
		}
		payload = payload[n:]
	}
	if halfClose {
		s.closed = true
		return s.stream.Send(&pb.JoinSessionRequest{Message: &pb.JoinSessionRequest_Frame{Frame: &pb.TunnelFrame{Frame: &pb.TunnelFrame_HalfClose{HalfClose: true}}}})
	}
	return nil
}
func (s *Session) Recv() ([]byte, bool, error) {
	m, err := s.stream.Recv()
	if err != nil {
		return nil, false, err
	}
	f := m.GetFrame()
	if f == nil {
		return nil, false, fmt.Errorf("expected relay frame")
	}
	switch x := f.Frame.(type) {
	case *pb.TunnelFrame_Data:
		if len(x.Data) > 0 && len(x.Data) <= 65536 {
			return x.Data, false, nil
		}
	case *pb.TunnelFrame_HalfClose:
		if x.HalfClose {
			return nil, true, nil
		}
	}
	return nil, false, fmt.Errorf("invalid TCP relay frame")
}

// Conn adapts the admitted byte stream to net.Conn. CloseWrite sends a relay
// half-close while keeping the response path open. A full close or the device
// ending its response closes the owned gRPC connection.
func (s *Session) Conn() net.Conn {
	reader, toLocal := net.Pipe()
	writer, fromLocal := net.Pipe()
	var once sync.Once
	closeSession := func() {
		once.Do(func() {
			_ = toLocal.Close()
			_ = fromLocal.Close()
			_ = s.Close()
		})
	}
	stop := context.AfterFunc(s.ctx, closeSession)
	go func() {
		defer closeSession()
		defer stop()
		for {
			b, half, err := s.Recv()
			if err != nil || half {
				return
			}
			if _, err = toLocal.Write(b); err != nil {
				return
			}
		}
	}()
	go func() {
		buf := make([]byte, 65536)
		for {
			n, err := fromLocal.Read(buf)
			if err == io.EOF {
				if err := s.Send(nil, true); err != nil {
					closeSession()
				}
				return
			}
			if err != nil || s.Send(buf[:n], false) != nil {
				closeSession()
				return
			}
		}
	}()
	return &tcpSessionConn{Conn: reader, writer: writer, closeSession: closeSession}
}

// Separate pipes allow one direction to reach EOF without closing the other.
type tcpSessionConn struct {
	net.Conn
	writer       net.Conn
	closeSession func()
}

func (c *tcpSessionConn) Write(p []byte) (int, error) { return c.writer.Write(p) }
func (c *tcpSessionConn) CloseWrite() error           { return c.writer.Close() }
func (c *tcpSessionConn) SetWriteDeadline(t time.Time) error {
	return c.writer.SetWriteDeadline(t)
}
func (c *tcpSessionConn) SetDeadline(t time.Time) error {
	readErr := c.SetReadDeadline(t)
	writeErr := c.SetWriteDeadline(t)
	if readErr != nil {
		return readErr
	}
	return writeErr
}
func (c *tcpSessionConn) Close() error {
	c.closeSession()
	_ = c.writer.Close()
	return c.Conn.Close()
}
