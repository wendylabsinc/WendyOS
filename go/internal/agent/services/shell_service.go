//go:build unix

package services

import (
	"context"
	"io"
	"sync"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// HostShellSpawner runs an interactive host PTY. It is injected so the service
// can be tested without spawning a real shell (see hostexec.Spawner).
type HostShellSpawner interface {
	Run(ctx context.Context, command []string, stdin io.Reader, stdout io.Writer, resize <-chan [2]uint32) (int, error)
}

// ShellService serves WendyShellService: an interactive shell on the device host.
type ShellService struct {
	agentpb.UnimplementedWendyShellServiceServer
	logger  *zap.Logger
	spawner HostShellSpawner
}

// NewShellService constructs the host shell service.
func NewShellService(logger *zap.Logger, spawner HostShellSpawner) *ShellService {
	return &ShellService{logger: logger, spawner: spawner}
}

// HostShell opens an interactive host PTY. The first client message must be
// Start; later messages carry stdin or window resizes. Output streams back as
// stdout frames, followed by a final exit_code.
func (s *ShellService) HostShell(stream grpc.BidiStreamingServer[agentpb.HostShellRequest, agentpb.HostShellResponse]) error {
	first, err := stream.Recv()
	if err == io.EOF {
		return status.Error(codes.InvalidArgument, "missing first shell message")
	}
	if err != nil {
		return err
	}
	start := first.GetStart()
	if start == nil {
		return status.Error(codes.InvalidArgument, "first message must be Start")
	}
	command := start.GetCommand()

	ctx := stream.Context()

	// Audit: a host shell over mTLS is a root shell on the device. Log every
	// session and its outcome so there is a forensic trail. An empty command
	// means the resolved login shell. clientAuditFields records the caller's
	// identity (org/entity/serial/remote addr) per the data-minimisation policy
	// in interceptor/mtls.go — never the certificate Subject/CN.
	s.logger.Info("host shell started",
		append([]zap.Field{zap.Strings("command", command)}, clientAuditFields(ctx)...)...)
	startedAt := time.Now()
	stdinR, stdinW := io.Pipe()
	defer stdinR.Close()

	resize := make(chan [2]uint32, 8)
	if ts := start.GetTermSize(); ts != nil {
		resize <- [2]uint32{ts.GetRows(), ts.GetCols()}
	}

	// Forward stdin + resize frames until the client closes the stream.
	go func() {
		defer stdinW.Close()
		defer close(resize)
		for {
			msg, recvErr := stream.Recv()
			if recvErr != nil {
				return
			}
			switch {
			case len(msg.GetStdinData()) > 0:
				if _, werr := stdinW.Write(msg.GetStdinData()); werr != nil {
					return
				}
			case msg.GetResize() != nil:
				select {
				case resize <- [2]uint32{msg.GetResize().GetRows(), msg.GetResize().GetCols()}:
				default: // drop resizes if the consumer is momentarily behind
				}
			}
		}
	}()

	// gRPC forbids concurrent SendMsg; serialize the stdout writer and the final
	// exit_code frame through one lock.
	sender := &shellSender{stream: stream}
	stdout := &shellWriter{sender: sender}

	code, err := s.spawner.Run(ctx, command, stdinR, stdout, resize)
	if err != nil {
		s.logger.Warn("host shell failed",
			zap.Duration("duration", time.Since(startedAt)), zap.Error(err))
		return status.Errorf(codes.Internal, "host shell failed: %v", err)
	}
	s.logger.Info("host shell completed",
		zap.Int("exit_code", code), zap.Duration("duration", time.Since(startedAt)))
	return sender.send(&agentpb.HostShellResponse{
		ResponseType: &agentpb.HostShellResponse_ExitCode{ExitCode: int32(code)},
	})
}

// clientAuditFields extracts best-effort, non-PII caller identity for the audit
// log. Per the data-minimisation policy in interceptor/mtls.go it records the
// org id, entity type/id, cert serial, and remote address — never the subject/CN.
// It returns whatever is available: over the plaintext server or the admin unix
// socket there is no client certificate, so only the remote address (if any) is
// logged.
func clientAuditFields(ctx context.Context) []zap.Field {
	p, ok := peer.FromContext(ctx)
	if !ok {
		return nil
	}
	fields := []zap.Field{zap.String("remote", p.Addr.String())}
	tlsInfo, ok := p.AuthInfo.(credentials.TLSInfo)
	if !ok || len(tlsInfo.State.PeerCertificates) == 0 {
		return fields
	}
	cert := tlsInfo.State.PeerCertificates[0]
	fields = append(fields, zap.String("cert_serial", cert.SerialNumber.String()))
	if ident, found, err := certs.IdentityFromCert(cert); err == nil && found {
		fields = append(fields,
			zap.String("scope", ident.Scope().String()),
			zap.String("entity_type", ident.EntityType),
			zap.String("entity_id", ident.EntityID),
		)
	}
	return fields
}

// shellSender serializes Send on the host shell bidi stream.
type shellSender struct {
	stream grpc.BidiStreamingServer[agentpb.HostShellRequest, agentpb.HostShellResponse]
	mu     sync.Mutex
}

func (s *shellSender) send(resp *agentpb.HostShellResponse) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stream.Send(resp)
}

// shellWriter adapts the PTY output stream to io.Writer, forwarding it as
// stdout_data frames.
type shellWriter struct{ sender *shellSender }

func (w *shellWriter) Write(p []byte) (int, error) {
	// Copy: the PTY may reuse its read buffer once Write returns.
	buf := append([]byte(nil), p...)
	if err := w.sender.send(&agentpb.HostShellResponse{
		ResponseType: &agentpb.HostShellResponse_StdoutData{StdoutData: buf},
	}); err != nil {
		return 0, err
	}
	return len(p), nil
}
