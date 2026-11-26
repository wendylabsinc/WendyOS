package services

import (
	"context"
	"crypto/x509"
	"path/filepath"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/interceptor"
	"github.com/wendylabsinc/wendy/go/internal/agent/mtls"
	"github.com/wendylabsinc/wendy/go/internal/agent/trustedkeys"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
)

type authModesLogPeer struct {
	agentpb.UnimplementedWendyTelemetryServiceServer
}

func (authModesLogPeer) StreamLogs(_ *agentpb.StreamLogsRequest, s grpc.ServerStreamingServer[agentpb.StreamLogsResponse]) error {
	if err := s.Send(&agentpb.StreamLogsResponse{IsHistory: true}); err != nil {
		return err
	}
	<-s.Context().Done()
	return s.Context().Err()
}

// A09 isolated transport proof. The fixture honors the restart request by
// replacing its servers. Actual Agent process/supervisor restart is a separate
// live assertion, not established by stopping a test gRPC server.
func TestAuthModesA09RemovedStream(t *testing.T) {
	r := newClosedRig(t)
	op := r.trust(t, trustedkeys.RoleOperator)
	opDER, err := x509.MarshalPKIXPublicKey(op.Public())
	if err != nil {
		t.Fatal(err)
	}
	restart := make(chan struct{}, 1)
	pb.RegisterWendyAccessServiceServer(r.srv, NewAccessService(zap.NewNop(), r.keys, func() bool { return false }, func() { restart <- struct{}{} }))
	agentpb.RegisterWendyTelemetryServiceServer(r.srv, authModesLogPeer{})
	r.start()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, err := agentpb.NewWendyTelemetryServiceClient(r.conn(t, op)).StreamLogs(ctx, &agentpb.StreamLogsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	first, err := stream.Recv()
	if err != nil || !first.GetIsHistory() {
		t.Fatalf("stream never entered its inert handler: %v", err)
	}
	_, err = pb.NewWendyAccessServiceClient(r.conn(t, r.owner)).RemoveTrustedKey(ctx, &pb.RemoveTrustedKeyRequest{Fingerprint: trustedkeys.Fingerprint(opDER)})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-restart:
	case <-ctx.Done():
		t.Fatal("removal did not request restart")
	}
	start := time.Now()
	r.srv.Stop()
	if _, err = stream.Recv(); err == nil || status.Code(err) == codes.DeadlineExceeded || ctx.Err() != nil {
		t.Fatalf("stream did not end due to fixture restart: %v", err)
	}
	t.Logf("removed stream ended after fixture stop in %s", time.Since(start))
	r.keys, err = trustedkeys.Open(filepath.Join(r.dir, trustedkeys.FileName))
	if err != nil {
		t.Fatal(err)
	}
	cert, key, err := mtls.ClosedServerIdentity(r.dir, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := mtls.NewTrustedKeyTLSConfig(cert, key, r.keys, trustedkeys.RoleViewer)
	if err != nil {
		t.Fatal(err)
	}
	a := interceptor.ClosedAuthorizer{Logger: zap.NewNop(), Listener: interceptor.ClosedKeyListener, Keys: r.keys}
	r.srv, r.lis = serve(t, grpc.Creds(credentials.NewTLS(cfg)), grpc.UnaryInterceptor(a.Unary()), grpc.StreamInterceptor(a.Stream()))
	pb.RegisterWendyAccessServiceServer(r.srv, NewAccessService(zap.NewNop(), r.keys, func() bool { return false }, func() {}))
	r.start()
	deniedCtx, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	if _, err = pb.NewWendyAccessServiceClient(r.conn(t, op)).GetAccess(deniedCtx, &pb.GetAccessRequest{}); err == nil {
		t.Fatal("removed key reconnected")
	}
	got, err := pb.NewWendyAccessServiceClient(r.conn(t, r.owner)).GetAccess(ctx, &pb.GetAccessRequest{})
	if err != nil || got.GetCallerRole() != pb.TrustedKeyRole_TRUSTED_KEY_ROLE_OWNER {
		t.Fatalf("remaining owner: %v %v", got, err)
	}
}
