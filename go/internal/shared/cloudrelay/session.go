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
	stream pb.TunnelBrokerV2Service_JoinSessionClient
	cancel context.CancelFunc
	conn   *grpc.ClientConn
	mu     sync.Mutex
	closed bool
}

func join(ctx context.Context, broker *pb.BrokerInstance, artifact string, c claims, key *ecdsa.PrivateKey, role pb.JoinRole, dial func(string) (*grpc.ClientConn, error)) (*Session, error) {
	ctx, cancel := context.WithDeadline(ctx, time.Unix(c.RelayExp, 0))
	ctx = metadata.NewOutgoingContext(ctx, metadata.MD{})
	conn, err := dial(broker.Endpoint)
	if err != nil {
		cancel()
		return nil, err
	}
	s := &Session{cancel: cancel, conn: conn}
	ok := false
	defer func() {
		if !ok {
			s.Close()
		}
	}()
	// Admission must finish before the grant expires; the relay has a later deadline.
	timer := time.AfterFunc(time.Until(time.Unix(c.Exp, 0)), cancel)
	defer timer.Stop()
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
	ok = true
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

// Conn adapts the admitted byte stream to net.Conn. Closing either direction
// tears down the owned gRPC connection, canceling blocked send/receive calls.
func (s *Session) Conn() net.Conn {
	local, remote := net.Pipe()
	go func() {
		defer remote.Close()
		defer s.Close()
		for {
			b, half, err := s.Recv()
			if err != nil || half {
				return
			}
			if _, err = remote.Write(b); err != nil {
				return
			}
		}
	}()
	go func() {
		defer remote.Close()
		defer s.Close()
		buf := make([]byte, 65536)
		for {
			n, err := remote.Read(buf)
			if n > 0 {
				if e := s.Send(buf[:n], false); e != nil {
					return
				}
			}
			if err != nil {
				if err == io.EOF {
					_ = s.Send(nil, true)
				}
				return
			}
		}
	}()
	return local
}
