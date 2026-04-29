package services

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	"github.com/wendylabsinc/wendy/internal/shared/certs"
	cloudpb "github.com/wendylabsinc/wendy/proto/gen/cloudpb"
)

// TunnelBrokerClient maintains a presence stream to the cloud tunnel broker
// and handles inbound dial requests by opening local TCP connections and relaying data.
type TunnelBrokerClient struct {
	logger      *zap.Logger
	url         string
	orgID       int32
	assetID     int32
	certPEM     string
	chainPEM    string
	keyPEM      string
	insecureDev bool
}

// NewTunnelBrokerClient creates a new tunnel broker client.
func NewTunnelBrokerClient(logger *zap.Logger, brokerURL string, orgID, assetID int32, certPEM, chainPEM, keyPEM string) *TunnelBrokerClient {
	return &TunnelBrokerClient{
		logger:      logger,
		url:         brokerURL,
		orgID:       orgID,
		assetID:     assetID,
		certPEM:     certPEM,
		chainPEM:    chainPEM,
		keyPEM:      keyPEM,
		insecureDev: os.Getenv("WENDY_BROKER_INSECURE_DEV") == "true",
	}
}

// Run starts the presence registration loop, reconnecting on error until ctx is cancelled.
func (c *TunnelBrokerClient) Run(ctx context.Context) {
	backoff := time.Second
	for {
		err := c.runOnce(ctx)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			c.logger.Warn("tunnel broker connection lost, reconnecting",
				zap.Error(err),
				zap.Duration("backoff", backoff),
			)
		}
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

func (c *TunnelBrokerClient) runOnce(ctx context.Context) error {
	conn, err := c.dialBroker()
	if err != nil {
		return fmt.Errorf("dial broker %s: %w", c.url, err)
	}
	defer conn.Close()

	client := cloudpb.NewTunnelBrokerServiceClient(conn)
	callCtx := c.withDevMeta(ctx)

	stream, err := client.RegisterPresence(callCtx)
	if err != nil {
		return fmt.Errorf("register presence: %w", err)
	}

	c.logger.Info("registered presence with tunnel broker", zap.String("url", c.url))

	if err := stream.Send(&cloudpb.AgentHeartbeat{}); err != nil {
		return fmt.Errorf("initial heartbeat: %w", err)
	}

	dialCh := make(chan *cloudpb.DialRequest, 8)
	errCh := make(chan error, 1)

	go func() {
		for {
			dr, err := stream.Recv()
			if err != nil {
				errCh <- err
				return
			}
			dialCh <- dr
		}
	}()

	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-errCh:
			return fmt.Errorf("presence stream recv: %w", err)
		case dr := <-dialCh:
			c.logger.Info("received dial request",
				zap.String("session", dr.SessionId),
				zap.String("host", dr.Host),
				zap.Uint32("port", dr.Port),
			)
			go c.handleDialRequest(ctx, client, dr)
		case <-ticker.C:
			if err := stream.Send(&cloudpb.AgentHeartbeat{}); err != nil {
				return fmt.Errorf("heartbeat send: %w", err)
			}
		}
	}
}

func (c *TunnelBrokerClient) handleDialRequest(ctx context.Context, client cloudpb.TunnelBrokerServiceClient, dr *cloudpb.DialRequest) {
	tcpAddr := fmt.Sprintf("%s:%d", dr.Host, dr.Port)
	tcpConn, err := net.DialTimeout("tcp", tcpAddr, 10*time.Second)
	if err != nil {
		c.logger.Error("failed to dial TCP target",
			zap.String("addr", tcpAddr),
			zap.String("session", dr.SessionId),
			zap.Error(err),
		)
		return
	}
	defer tcpConn.Close()

	stream, err := client.AgentTunnel(c.withDevMeta(ctx))
	if err != nil {
		c.logger.Error("failed to open AgentTunnel stream",
			zap.String("session", dr.SessionId),
			zap.Error(err),
		)
		return
	}

	// First message must carry the session_id to join the pending session.
	if err := stream.Send(&cloudpb.TunnelData{SessionId: dr.SessionId}); err != nil {
		c.logger.Error("failed to send session join message",
			zap.String("session", dr.SessionId),
			zap.Error(err),
		)
		return
	}

	c.relay(ctx, tcpConn, stream)
}

type agentTunnelStream interface {
	Send(*cloudpb.TunnelData) error
	Recv() (*cloudpb.TunnelData, error)
	CloseSend() error
}

func (c *TunnelBrokerClient) relay(ctx context.Context, tcpConn net.Conn, stream agentTunnelStream) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// TCP -> gRPC: read from local service, forward to CLI via broker.
	go func() {
		defer cancel()
		buf := make([]byte, 32*1024)
		for {
			n, err := tcpConn.Read(buf)
			if n > 0 {
				payload := make([]byte, n)
				copy(payload, buf[:n])
				if sendErr := stream.Send(&cloudpb.TunnelData{Payload: payload}); sendErr != nil {
					return
				}
			}
			if err != nil {
				if err != io.EOF {
					c.logger.Debug("TCP read ended", zap.Error(err))
				}
				_ = stream.Send(&cloudpb.TunnelData{HalfClose: true})
				_ = stream.CloseSend()
				return
			}
		}
	}()

	// gRPC -> TCP: receive from CLI via broker, write to local service.
	type halfCloser interface {
		CloseWrite() error
	}
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		msg, err := stream.Recv()
		if err != nil {
			if err != io.EOF {
				c.logger.Debug("gRPC stream recv ended", zap.Error(err))
			}
			if hc, ok := tcpConn.(halfCloser); ok {
				_ = hc.CloseWrite()
			}
			return
		}
		if msg.HalfClose {
			if hc, ok := tcpConn.(halfCloser); ok {
				_ = hc.CloseWrite()
			}
			return
		}
		if len(msg.Payload) > 0 {
			if _, writeErr := tcpConn.Write(msg.Payload); writeErr != nil {
				return
			}
		}
	}
}

func (c *TunnelBrokerClient) dialBroker() (*grpc.ClientConn, error) {
	if c.insecureDev {
		return grpc.NewClient(c.url, grpc.WithTransportCredentials(insecure.NewCredentials()))
	}
	tlsCfg, err := certs.LoadTLSConfig(c.certPEM, c.chainPEM, c.keyPEM, c.chainPEM)
	if err != nil {
		return nil, fmt.Errorf("loading mTLS config: %w", err)
	}
	return grpc.NewClient(c.url, grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg)))
}

// withDevMeta injects dev-mode identity headers when WENDY_BROKER_INSECURE_DEV is set.
func (c *TunnelBrokerClient) withDevMeta(ctx context.Context) context.Context {
	if !c.insecureDev {
		return ctx
	}
	return metadata.NewOutgoingContext(ctx, metadata.Pairs(
		"x-dev-org-id", fmt.Sprintf("%d", c.orgID),
		"x-dev-asset-id", fmt.Sprintf("%d", c.assetID),
	))
}
