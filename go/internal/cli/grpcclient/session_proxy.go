package grpcclient

import (
	"context"
	"net"
	"sync/atomic"

	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/useripc"
	"google.golang.org/grpc"
)

// ConnectSessionProxy connects to a user-owned local session broker. The
// broker is the authenticated endpoint: it established and retains the mTLS
// connection to the exact device identity supplied here. Keeping that verified
// identity on AgentConnection makes the ordinary device-pin check apply to a
// broker hit exactly as it does to a fresh network connection.
func ConnectSessionProxy(ctx context.Context, socketPath, host, addr string, certInfo *config.CertificateInfo, identity certs.WendyIdentity, options ...grpc.DialOption) (*AgentConnection, error) {
	// The open channel is what tells the broker this invocation still
	// exists: its connection count keeps the broker alive through long
	// local quiet periods (an image build between device RPCs). gRPC's
	// default 30-minute client idle timeout would tear the transport down
	// mid-command, letting the broker expire and unlink the socket the
	// post-build RPC then re-dials — failing an invocation that direct
	// dialing would have completed. Zero disables idleness.
	dialer := func(ctx context.Context, _ string) (net.Conn, error) { return useripc.Dial(ctx, socketPath) }
	conn, err := newLocalStreamClient(dialer, "wendy-session-broker", append([]grpc.DialOption{grpc.WithIdleTimeout(0)}, options...)...)
	if err != nil {
		return nil, err
	}

	ac := newAgentConnection(conn)
	ac.Host = host
	ac.Addr = addr
	ac.IsMTLS = true
	ac.IsSessionProxy = true
	ac.CertInfo = certInfo
	ac.observedServerIdentity = newAtomicIdentity(identity)
	// The org was proven by the broker's original TLS verification, exactly
	// like the OnServerIdentity sink proves it on a direct connect; carrying
	// it here keeps org-consuming paths (e.g. the device-cache write-back)
	// behaving identically on broker hits.
	ac.observedServerOrg = new(atomic.Int32)
	ac.observedServerOrg.Store(identity.OrgID)
	return ac, nil
}

func newAtomicIdentity(identity certs.WendyIdentity) *atomic.Pointer[certs.WendyIdentity] {
	dst := new(atomic.Pointer[certs.WendyIdentity])
	stored := identity
	dst.Store(&stored)
	return dst
}
