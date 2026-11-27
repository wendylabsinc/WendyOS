package appservice

import (
	"context"
	"errors"
	"net"

	"github.com/wendylabsinc/wendy/go/world/production/rpctrace"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
)

type serviceAuthInfo struct {
	service *service
	conn    *trackedConn
	peer    Peer
}

// peerPin refers to the process that originally connected, not whichever
// process happens to own its numerical PID when a runtime mapping is read.
type peerPin interface {
	Alive() error
	Close() error
}

func (serviceAuthInfo) AuthType() string { return "wendy-world-service-unix" }

type localCredentials struct{ service *service }

// ClientCredentials permits only a Unix connection. It makes no assertion
// about the caller's identity: the server reads kernel credentials and checks
// its exact commissioned container service independently.
func ClientCredentials() credentials.TransportCredentials { return &localCredentials{} }
func (c *localCredentials) ClientHandshake(ctx context.Context, _ string, conn net.Conn) (net.Conn, credentials.AuthInfo, error) {
	if ctx.Err() != nil {
		return nil, nil, ctx.Err()
	}
	if _, ok := conn.(*net.UnixConn); !ok || c.service != nil {
		return nil, nil, ErrPeer
	}
	return conn, serviceAuthInfo{}, nil
}
func (c *localCredentials) ServerHandshake(conn net.Conn) (net.Conn, credentials.AuthInfo, error) {
	t, ok := conn.(*trackedConn)
	if !ok || c.service == nil || t.service != c.service {
		return nil, nil, ErrPeer
	}
	ctx, cancel := context.WithTimeout(c.service.ctx, c.service.manager.cfg.Limits.RequestTimeout)
	defer cancel()
	p, err := c.service.checkPeer(ctx, t)
	if err != nil {
		return nil, nil, err
	}
	if err = c.service.checkAuthority(ctx); err != nil {
		return nil, nil, err
	}
	return t, serviceAuthInfo{service: c.service, conn: t, peer: p}, nil
}
func (c *localCredentials) Info() credentials.ProtocolInfo {
	return credentials.ProtocolInfo{SecurityProtocol: "wendy-world-service-unix", SecurityVersion: "1"}
}
func (c *localCredentials) Clone() credentials.TransportCredentials {
	return &localCredentials{service: c.service}
}
func (*localCredentials) OverrideServerName(string) error {
	return errors.New("Unix service credentials do not accept server names")
}

func (s *service) checkPeer(ctx context.Context, conn *trackedConn) (Peer, error) {
	defer rpctrace.Start(ctx, rpctrace.StagePeer)()
	if err := ctx.Err(); err != nil {
		s.refuse(ctx, rpctrace.PointPeerBeforeScope, contextRefusal(err), conn)
		return Peer{}, ErrPeer
	}
	if s.closed.Load() {
		s.refuse(ctx, rpctrace.PointPeerBeforeScope, rpctrace.ReasonServiceClosed, conn)
		return Peer{}, ErrPeer
	}
	if conn == nil || conn.service != s || conn.pin == nil {
		s.refuse(ctx, rpctrace.PointPeerBeforeScope, rpctrace.ReasonInvalidBinding, conn)
		return Peer{}, ErrPeer
	}
	if s.peerAlive(ctx, rpctrace.PointPeerBeforeScope, conn) != nil {
		return Peer{}, ErrPeer
	}
	p, err := s.manager.peerCredentials(conn.UnixConn)
	if err != nil || p.PID <= 0 {
		s.refuse(ctx, rpctrace.PointPeerBeforeScope, rpctrace.ReasonCredentialLookup, conn)
		return Peer{}, ErrPeer
	}
	if err = s.manager.cfg.PeerCheck(ctx, p, s.scope); err != nil {
		s.refuse(ctx, rpctrace.PointPeerBeforeScope, rpctrace.ReasonScopedPeerRefused, conn)
		return Peer{}, ErrPeer
	}
	if err := ctx.Err(); err != nil {
		s.refuse(ctx, rpctrace.PointPeerAfterScope, contextRefusal(err), conn)
		return Peer{}, ErrPeer
	}
	if s.closed.Load() {
		s.refuse(ctx, rpctrace.PointPeerAfterScope, rpctrace.ReasonServiceClosed, conn)
		return Peer{}, ErrPeer
	}
	if s.peerAlive(ctx, rpctrace.PointPeerAfterScope, conn) != nil {
		return Peer{}, ErrPeer
	}
	return p, nil
}
func (s *service) checkAuthority(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		s.refuse(ctx, rpctrace.PointAuthority, contextRefusal(err), nil)
		return err
	}
	if s.closed.Load() {
		s.refuse(ctx, rpctrace.PointAuthority, rpctrace.ReasonServiceClosed, nil)
		return ErrClosed
	}
	select {
	case <-s.publisher.Done():
		s.refuse(ctx, rpctrace.PointAuthority, rpctrace.ReasonPublisherDone, nil)
		go s.stop()
		return ErrClosed
	default:
	}
	validating := rpctrace.Start(ctx, rpctrace.StagePublisher)
	err := s.publisher.Validate(ctx)
	validating()
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			s.refuse(ctx, rpctrace.PointAuthority, contextRefusal(contextErr), nil)
			return ctx.Err()
		}
		s.refuse(ctx, rpctrace.PointAuthority, rpctrace.ReasonPublisherRefused, nil)
		go s.stop()
		return err
	}
	if s.closed.Load() {
		s.refuse(ctx, rpctrace.PointAuthority, rpctrace.ReasonServiceClosed, nil)
		return ErrClosed
	}
	return nil
}
func (s *service) authorize(ctx context.Context) error {
	defer rpctrace.Start(ctx, rpctrace.StageAuthorize)()
	p, ok := peer.FromContext(ctx)
	if !ok {
		s.refuse(ctx, rpctrace.PointAuthorize, rpctrace.ReasonInvalidBinding, nil)
		return ErrPeer
	}
	a, ok := p.AuthInfo.(serviceAuthInfo)
	if !ok || a.service != s || a.conn == nil {
		s.refuse(ctx, rpctrace.PointAuthorize, rpctrace.ReasonInvalidBinding, a.conn)
		return ErrPeer
	}
	actual, err := s.checkPeer(ctx, a.conn)
	if err != nil {
		return ErrPeer
	}
	if actual != a.peer {
		s.refuse(ctx, rpctrace.PointAuthorize, rpctrace.ReasonPeerChanged, a.conn)
		return ErrPeer
	}
	return s.checkAuthority(ctx)
}

// responseAlive is the last gate after response finalization. It performs no
// host peer mapping or clock I/O; the original pidfd must still be live even if
// another process inherited its connection while a previous check was waiting.
func (s *service) responseAlive(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		s.refuse(ctx, rpctrace.PointResponse, contextRefusal(err), nil)
		return ErrClosed
	}
	if s.closed.Load() {
		s.refuse(ctx, rpctrace.PointResponse, rpctrace.ReasonServiceClosed, nil)
		return ErrClosed
	}
	p, ok := peer.FromContext(ctx)
	if !ok {
		s.refuse(ctx, rpctrace.PointResponse, rpctrace.ReasonInvalidBinding, nil)
		return ErrPeer
	}
	a, ok := p.AuthInfo.(serviceAuthInfo)
	if !ok || a.service != s || a.conn == nil || a.conn.pin == nil {
		s.refuse(ctx, rpctrace.PointResponse, rpctrace.ReasonInvalidBinding, a.conn)
		return ErrPeer
	}
	if s.peerAlive(ctx, rpctrace.PointResponse, a.conn) != nil {
		return ErrPeer
	}
	select {
	case <-s.publisher.Done():
		s.refuse(ctx, rpctrace.PointResponse, rpctrace.ReasonPublisherDone, a.conn)
		return ErrClosed
	default:
	}
	return nil
}
