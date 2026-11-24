package services

import (
	"context"
	"crypto"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/interceptor"
	"github.com/wendylabsinc/wendy/go/internal/agent/mtls"
	"github.com/wendylabsinc/wendy/go/internal/agent/trustedkeys"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/test/bufconn"
)

// closedRig is a Closed device's trusted-key listener as main wires it: the
// real TLS config and the role table, with an owner key already trusted.
type closedRig struct {
	dir   string
	keys  *trustedkeys.Store
	srv   *grpc.Server
	lis   *bufconn.Listener
	owner crypto.Signer
}

func newClosedRig(t *testing.T) *closedRig {
	t.Helper()
	dir := t.TempDir()
	keys, err := trustedkeys.Open(filepath.Join(dir, trustedkeys.FileName))
	if err != nil {
		t.Fatal(err)
	}
	owner, ownerSPKI := newKey(t)
	if _, err := keys.Close(ownerSPKI, "owner", time.Now()); err != nil {
		t.Fatal(err)
	}
	certPEM, keyPEM, err := mtls.ClosedServerIdentity(dir, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	tlsCfg, err := mtls.NewTrustedKeyTLSConfig(certPEM, keyPEM, keys, trustedkeys.RoleViewer)
	if err != nil {
		t.Fatal(err)
	}
	authz := &interceptor.ClosedAuthorizer{Logger: zap.NewNop(), Listener: interceptor.ClosedKeyListener, Keys: keys}
	srv, lis := serve(t, grpc.Creds(credentials.NewTLS(tlsCfg)),
		grpc.ChainUnaryInterceptor(authz.Unary()), grpc.ChainStreamInterceptor(authz.Stream()))
	return &closedRig{dir: dir, keys: keys, srv: srv, lis: lis, owner: owner}
}

func (r *closedRig) start() { go r.srv.Serve(r.lis) }

// trust adds a new key with role and returns it.
func (r *closedRig) trust(t *testing.T, role trustedkeys.Role) crypto.Signer {
	t.Helper()
	key, spki := newKey(t)
	ownerFP := r.keys.List()[0].Fingerprint()
	if _, err := r.keys.Add(spki, role, "", ownerFP, time.Now()); err != nil {
		t.Fatal(err)
	}
	return key
}

func (r *closedRig) conn(t *testing.T, key crypto.Signer) *grpc.ClientConn {
	t.Helper()
	conn, err := grpc.NewClient("passthrough:///device",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return r.lis.Dial() }),
		grpc.WithTransportCredentials(keyCreds(t, key)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

// The delivery calls and the builder switch check Wendy certificates in their
// handlers; a trusted key the role table admitted must pass them.
func TestClosedTrustedKeyPassesHandlerIdentityChecks(t *testing.T) {
	ctx := context.Background()
	r := newClosedRig(t)
	agentpb.RegisterWendyContainerServiceServer(r.srv, NewContainerService(zap.NewNop(), &mockContainerdClient{}))
	agentpbv2.RegisterWendyBuildServiceServer(r.srv, NewBuildService(zap.NewNop(), BuildServiceOptions{ConfigPath: r.dir}))
	r.start()

	op := agentpb.NewWendyContainerServiceClient(r.conn(t, r.trust(t, trustedkeys.RoleOperator)))
	if _, err := op.QueryChunks(ctx, &agentpb.QueryChunksRequest{}); err != nil {
		t.Fatalf("operator QueryChunks: %v", err)
	}
	if _, err := op.QueryLayers(ctx, &agentpb.QueryLayersRequest{}); err != nil {
		t.Fatalf("operator QueryLayers: %v", err)
	}
	owner := agentpbv2.NewWendyBuildServiceClient(r.conn(t, r.owner))
	if _, err := owner.SetBuildHostEnabled(ctx, &agentpbv2.SetBuildHostEnabledRequest{Enabled: true}); err != nil {
		t.Fatalf("owner SetBuildHostEnabled: %v", err)
	}
	viewer := agentpb.NewWendyContainerServiceClient(r.conn(t, r.trust(t, trustedkeys.RoleViewer)))
	_, err := viewer.QueryChunks(ctx, &agentpb.QueryChunksRequest{})
	wantCode(t, "viewer QueryChunks", err, codes.PermissionDenied)
}
