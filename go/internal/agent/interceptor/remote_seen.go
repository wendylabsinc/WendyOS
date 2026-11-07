package interceptor

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"

	"github.com/wendylabsinc/wendy/go/internal/shared/cloudrelay"
	"google.golang.org/grpc"
	"google.golang.org/grpc/peer"
)

// RemoteVia names the path a call came in on: "lan" for a network client,
// "relay" for the cloud relay (its sessions come from RelaySourceIP), and ""
// for the local socket and other on-device callers such as wendy-net sessions.
func RemoteVia(ctx context.Context) string {
	p, ok := peer.FromContext(ctx)
	if !ok {
		return ""
	}
	tcp, ok := p.Addr.(*net.TCPAddr)
	if !ok {
		return ""
	}
	switch {
	case tcp.IP.Equal(cloudrelay.RelaySourceIP):
		return "relay"
	case tcp.IP.IsLoopback():
		return ""
	}
	return "lan"
}

// UnaryRemoteSeen tells the agent supervisor that a remote client reached this
// agent, which is what approves a new agent on trial.
func UnaryRemoteSeen(dir string) grpc.UnaryServerInterceptor {
	m := &remoteMarker{dir: dir}
	return func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		m.mark(ctx)
		return handler(ctx, req)
	}
}

// StreamRemoteSeen is UnaryRemoteSeen for streaming calls.
func StreamRemoteSeen(dir string) grpc.StreamServerInterceptor {
	m := &remoteMarker{dir: dir}
	return func(srv any, ss grpc.ServerStream, _ *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		m.mark(ss.Context())
		return handler(srv, ss)
	}
}

type remoteMarker struct {
	dir  string
	done sync.Map
}

// mark writes once per path; the pid keeps a mark from an earlier agent from counting.
func (m *remoteMarker) mark(ctx context.Context) {
	via := RemoteVia(ctx)
	if via == "" {
		return
	}
	if _, seen := m.done.LoadOrStore(via, true); seen {
		return
	}
	if MarkRemoteSeen(m.dir, via) != nil {
		m.done.Delete(via)
	}
}

// MarkRemoteSeen tells the agent supervisor this agent was reached over via.
func MarkRemoteSeen(dir, via string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "remote-seen."+via), []byte(strconv.Itoa(os.Getpid())), 0o644)
}
