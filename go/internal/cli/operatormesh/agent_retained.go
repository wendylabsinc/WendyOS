package operatormesh

import (
	"context"
	"errors"
	"io"
	"net"

	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/agent/meshsession"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/directstream"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshoperatorlink"
)

type retainedAgentOwner struct {
	client *grpcclient.AgentConnection
	stop   chan struct{}
}

// agentMu protects ownership. A replacement dial stops the previous watcher;
// old epochs can never retire a replacement client or leave orphan watchers.
func (s *Session) clearAgentOwnerLocked(peer meshid.Device, client *grpcclient.AgentConnection) {
	if owner := s.agentOwners[peer]; owner != nil && owner.client == client {
		delete(s.agentOwners, peer)
		close(owner.stop)
	}
}

func (s *Session) retainAgent(peer meshid.Device, client *grpcclient.AgentConnection, identity meshsession.DirectNativeIdentity) error {
	if _, err := identity.TLSState(); err != nil {
		return err
	}
	return s.watchAgent(peer, client, identity.Done())
}

func (s *Session) watchAgent(peer meshid.Device, client *grpcclient.AgentConnection, done <-chan struct{}) error {
	if done == nil {
		return io.ErrClosedPipe
	}
	s.agentMu.Lock()
	defer s.agentMu.Unlock()
	if s.agentStopping || s.agentClients[peer] != client {
		return io.ErrClosedPipe
	}
	if previous := s.agentOwners[peer]; previous != nil {
		s.clearAgentOwnerLocked(peer, previous.client)
	}
	if s.agentOwners == nil {
		s.agentOwners = make(map[meshid.Device]*retainedAgentOwner)
	}
	owner := &retainedAgentOwner{client: client, stop: make(chan struct{})}
	s.agentOwners[peer] = owner
	s.agentWorkers.Add(1)
	go func() {
		defer s.agentWorkers.Done()
		select {
		case <-done:
		case <-owner.stop:
			return
		}
		s.agentMu.Lock()
		retire := s.agentOwners[peer] == owner && s.agentClients[peer] == client
		if retire {
			delete(s.agentClients, peer)
			s.clearAgentOwnerLocked(peer, client)
		}
		s.agentMu.Unlock()
		if retire {
			client.Close()
		}
	}()
	return nil
}

func (s *Session) openManagedAgent(ctx context.Context, peer meshid.Device, client *grpcclient.AgentConnection) (net.Conn, error) {
	if s.direct != nil {
		raw, identity, err := s.direct.DialNativeIdentity(ctx, peer, directstream.ServiceGRPCAdmin)
		if err == nil {
			secured, err := meshoperatorlink.RetainedAgentClient(raw, identity)
			if err != nil {
				raw.Close()
				return nil, err
			}
			if err = s.retainAgent(peer, client, identity); err != nil {
				secured.Close()
				return nil, err
			}
			return secured, nil
		}
		if errors.Is(err, meshsession.ErrDirectUseUpper) {
			return s.openUpperAgent(ctx, peer, client)
		}
		if !errors.Is(err, meshsession.ErrNoDirectSession) && !errors.Is(err, meshsession.ErrDirectUseUpper) {
			return nil, err
		}
	}
	if s.upper != nil && !s.hasDirectPeer(peer) {
		return s.openUpperAgent(ctx, peer, client)
	}
	raw, err := s.openAgentForPeer(ctx, peer)
	if err != nil {
		return nil, err
	}
	// Serialize physical ownership publication against last-carrier teardown.
	s.linkMu.RLock()
	s.agentMu.Lock()
	alive := !s.agentStopping && s.agentClients[peer] == client && s.hasDirectPeerLocked(peer)
	if alive {
		s.clearAgentOwnerLocked(peer, client)
	}
	s.agentMu.Unlock()
	s.linkMu.RUnlock()
	if !alive {
		raw.Close()
		return nil, io.ErrClosedPipe
	}
	return raw, nil
}

// Both the native port and QUIC endpoint must come from the current signed
// directory. No arbitrary app port or transport is inferred after a failure.
func (s *Session) agentUpperTarget(peer meshid.Device) (meshsession.AppEndpoint, uint16, error) {
	return resolveAgentUpperTarget(s.Snapshot(), peer)
}

func resolveAgentUpperTarget(snapshot localmesh.NodeSnapshot, peer meshid.Device) (meshsession.AppEndpoint, uint16, error) {
	for _, manifest := range snapshot.Devices {
		if manifest.Device != peer.UUID() || manifest.AgentPort == 0 {
			continue
		}
		_, found, err := meshsession.ResolveAgentMTLS(snapshot, peer, manifest.AgentPort)
		if err != nil {
			return meshsession.AppEndpoint{}, 0, err
		}
		if !found {
			continue
		}
		target, err := meshsession.ResolveApp(snapshot, peer)
		if err != nil {
			return meshsession.AppEndpoint{}, 0, err
		}
		if target.Transport != localmesh.AppQUIC {
			return meshsession.AppEndpoint{}, 0, meshsession.ErrUnsupportedTransport
		}
		return target, manifest.AgentPort, nil
	}
	return meshsession.AppEndpoint{}, 0, meshsession.ErrNoRoute
}

func (s *Session) openUpperAgent(ctx context.Context, peer meshid.Device, client *grpcclient.AgentConnection) (net.Conn, error) {
	if s.upper == nil {
		return nil, meshsession.ErrNoDirectSession
	}
	target, port, err := s.agentUpperTarget(peer)
	if err != nil {
		return nil, err
	}
	raw, err := s.upper.DialWithRetryCheck(ctx, peer, target.Address, port, func() error {
		current, currentPort, err := s.agentUpperTarget(peer)
		if err != nil {
			return err
		}
		if current != target || currentPort != port {
			return meshsession.ErrNoRoute
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.runMu.Lock()
	done := s.runDone
	s.runMu.Unlock()
	if err = s.watchAgent(peer, client, done); err != nil {
		raw.Close()
		return nil, err
	}
	// This is an ordinary QUIC app stream. gRPC performs its own pinned TLS
	// handshake using the configured fallback; no carrier identity is borrowed.
	return raw, nil
}
