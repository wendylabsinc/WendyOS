package operatormesh

import (
	"context"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshoperatorlink"
	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
)

func retainedAgentTestClient(t *testing.T) *grpcclient.AgentConnection {
	t.Helper()
	conn, err := grpc.NewClient("passthrough:///unused-retained-test", grpc.WithTransportCredentials(meshoperatorlink.AgentTransportCredentials(nil)))
	if err != nil {
		t.Fatal(err)
	}
	client := grpcclient.NewFromConn(conn)
	t.Cleanup(func() { client.Close() })
	return client
}
func waitAgentWorkers(t *testing.T, s *Session) {
	t.Helper()
	done := make(chan struct{})
	go func() { s.agentWorkers.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("agent watcher did not join")
	}
}

func TestRetainedAgentSurvivesNoCarrierAndRetiresOnLogicalDeath(t *testing.T) {
	var peer meshid.Device
	client := retainedAgentTestClient(t)
	s := &Session{started: true, agentClients: map[meshid.Device]*grpcclient.AgentConnection{peer: client}}
	done := make(chan struct{})
	if err := s.watchAgent(peer, client, done); err != nil {
		t.Fatal(err)
	}
	got, err := s.AgentConnectionForPeer(context.Background(), peer)
	if err != nil || got != client {
		t.Fatalf("lost live client with no carrier: %v", err)
	}
	close(done)
	waitAgentWorkers(t, s)
	if client.Conn.GetState() != connectivity.Shutdown {
		t.Fatal("logical death left grpc reconnect loop")
	}
	s.agentMu.Lock()
	defer s.agentMu.Unlock()
	if len(s.agentClients) != 0 || len(s.agentOwners) != 0 {
		t.Fatal("retired client retained")
	}
}

func TestRetainedAgentReplacementCannotBeClosedByOldEpoch(t *testing.T) {
	var peer meshid.Device
	old, next := retainedAgentTestClient(t), retainedAgentTestClient(t)
	s := &Session{agentClients: map[meshid.Device]*grpcclient.AgentConnection{peer: old}}
	first, second := make(chan struct{}), make(chan struct{})
	if err := s.watchAgent(peer, old, first); err != nil {
		t.Fatal(err)
	}
	s.agentMu.Lock()
	s.agentClients[peer] = next
	s.agentMu.Unlock()
	if err := s.watchAgent(peer, next, second); err != nil {
		t.Fatal(err)
	}
	close(first)
	s.agentMu.Lock()
	s.clearAgentOwnerLocked(peer, next)
	s.agentMu.Unlock()
	waitAgentWorkers(t, s)
	if next.Conn.GetState() == connectivity.Shutdown {
		t.Fatal("old epoch closed replacement")
	}
	s.agentMu.Lock()
	s.agentStopping = true
	s.agentMu.Unlock()
	if err := s.watchAgent(peer, next, second); err == nil {
		t.Fatal("registered watcher during shutdown")
	}
}
