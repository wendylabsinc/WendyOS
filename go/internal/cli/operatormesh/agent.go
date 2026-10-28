package operatormesh

import (
	"context"
	"io"
	"net"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshoperatorlink"
	"google.golang.org/grpc"
)

// AgentConnection reuses the actual adjacent router's pinned operator TLS
// attachment. Run must already be draining its packets and controls. The
// session retains one normal HTTP/2 client, reusable by concurrent RPC calls;
// closing that client releases it, and closing the session closes it as well.
// This does not dial a different device through an adjacent router's identity.
func (s *Session) AgentConnection(ctx context.Context) (*grpcclient.AgentConnection, error) {
	s.runMu.Lock()
	started, done := s.started, s.runDone
	s.runMu.Unlock()
	if !started {
		return nil, io.ErrClosedPipe
	}
	for {
		if peer, ok := s.AdjacentPeer(); ok {
			return s.AgentConnectionForPeer(ctx, peer)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-done:
			return nil, io.ErrClosedPipe
		case <-s.linkChanged:
		}
	}

}

func (s *Session) AgentConnectionForPeer(ctx context.Context, peer meshid.Device) (*grpcclient.AgentConnection, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.runMu.Lock()
	defer s.runMu.Unlock()
	if s.closed || !s.started {
		return nil, io.ErrClosedPipe
	}
	if !s.hasDirectPeer(peer) {
		return nil, io.ErrClosedPipe
	}
	s.agentMu.Lock()
	defer s.agentMu.Unlock()
	if found := s.agentClients[peer]; found != nil {
		return found, nil
	}
	conn, err := grpc.NewClient("passthrough:///operator-agent", grpc.WithIdleTimeout(0), grpc.WithTransportCredentials(meshoperatorlink.AgentTransportCredentials(nil)), grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return s.openAgentForPeer(ctx, peer) }))
	if err != nil {
		return nil, err
	}
	result := grpcclient.NewFromConn(conn)
	result.Host = peer.Principal()
	result.IsMTLS = true
	info := s.certificate
	result.CertInfo = &info
	result.AddCloser(agentClientRelease(func() {
		s.agentMu.Lock()
		if s.agentClients[peer] == result {
			delete(s.agentClients, peer)
		}
		s.agentMu.Unlock()
	}))
	s.agentClients[peer] = result
	return result, nil
}

type agentClientRelease func()

func (f agentClientRelease) Close() error { f(); return nil }

func (s *Session) openAgent(ctx context.Context, _ string) (net.Conn, error) {
	peer, ok := s.AdjacentPeer()
	if !ok {
		return nil, io.ErrClosedPipe
	}
	return s.openAgentForPeer(ctx, peer)
}
