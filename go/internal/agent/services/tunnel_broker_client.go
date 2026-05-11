package services

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"math"
	"net"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/metadata"

	cloudpb "github.com/wendylabsinc/wendy/proto/gen/cloudpb"
)

const (
	brokerHeartbeatInterval = 30 * time.Second
	brokerMaxBackoff        = 90 * time.Second
	brokerKeepaliveTime     = 30 * time.Second
	brokerKeepaliveTimeout  = 10 * time.Second
)

// TunnelBrokerClient maintains a persistent RegisterPresence stream with the broker
// and dials local TCP connections in response to DialRequest messages.
type TunnelBrokerClient struct {
	logger   *zap.Logger
	url      string
	orgID    int32
	assetID  int32
	chainPEM string
}

// NewTunnelBrokerClient creates a new TunnelBrokerClient.
// chainPEM is the Wendy CA certificate chain used to verify the broker's TLS certificate.
func NewTunnelBrokerClient(logger *zap.Logger, url string, orgID, assetID int32, chainPEM string) *TunnelBrokerClient {
	return &TunnelBrokerClient{
		logger:   logger,
		url:      url,
		orgID:    orgID,
		assetID:  assetID,
		chainPEM: chainPEM,
	}
}

// Run connects to the broker and reconnects with exponential backoff on failure.
// Blocks until ctx is cancelled.
func (c *TunnelBrokerClient) Run(ctx context.Context) {
	attempt := 0
	for {
		if err := c.runOnce(ctx); err != nil {
			if ctx.Err() != nil {
				return
			}
			backoff := time.Duration(math.Min(
				float64(time.Second)*math.Pow(2, float64(attempt)),
				float64(brokerMaxBackoff),
			))
			c.logger.Warn("broker connection failed, reconnecting",
				zap.Error(err), zap.Duration("backoff", backoff))
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return
			}
			attempt++
		} else {
			attempt = 0
		}
	}
}

func (c *TunnelBrokerClient) runOnce(ctx context.Context) error {
	dialOpts, devMD, err := c.buildDialOpts()
	if err != nil {
		return err
	}
	conn, err := grpc.NewClient(c.url, dialOpts...)
	if err != nil {
		return err
	}
	defer conn.Close()

	client := cloudpb.NewTunnelBrokerServiceClient(conn)

	callCtx := ctx
	if devMD != nil {
		callCtx = metadata.NewOutgoingContext(ctx, devMD)
	}

	stream, err := client.RegisterPresence(callCtx)
	if err != nil {
		return err
	}

	c.logger.Info("registered presence with broker",
		zap.String("url", c.url), zap.Int32("asset_id", c.assetID))

	hbTicker := time.NewTicker(brokerHeartbeatInterval)
	defer hbTicker.Stop()

	recvCh := make(chan *cloudpb.DialRequest, 8)
	recvErr := make(chan error, 1)
	go func() {
		for {
			req, err := stream.Recv()
			if err != nil {
				select {
				case recvErr <- err:
				case <-ctx.Done():
				}
				return
			}
			select {
			case recvCh <- req:
			case <-ctx.Done():
				return
			}
		}
	}()

	for {
		select {
		case req := <-recvCh:
			go c.handleDialRequest(ctx, client, req, devMD)
		case err := <-recvErr:
			if err == io.EOF {
				return nil
			}
			return err
		case <-hbTicker.C:
			if err := stream.Send(&cloudpb.AgentHeartbeat{}); err != nil {
				return err
			}
		case <-ctx.Done():
			return nil
		}
	}
}

func (c *TunnelBrokerClient) buildDialOpts() ([]grpc.DialOption, metadata.MD, error) {
	// The broker uses tls.NoClientCert because Go's TLS library rejects ML-DSA
	// client certs (produced by pki-core) at the parsing stage regardless of
	// ClientAuth level. Identity is verified via the XFCC header at the application
	// layer instead.
	//
	// Broker cert CN is localhost and won't match the cloud host — skip hostname
	// verification but still validate the chain against the Wendy CA.
	caPool, err := x509.SystemCertPool()
	if err != nil {
		caPool = x509.NewCertPool()
	}
	if c.chainPEM != "" && !caPool.AppendCertsFromPEM([]byte(c.chainPEM)) {
		return nil, metadata.MD{}, fmt.Errorf("no valid CA certificates in chainPEM")
	}
	tlsCfg := &tls.Config{
		InsecureSkipVerify: true, //nolint:gosec
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return fmt.Errorf("broker presented no TLS certificate")
			}
			intermediates := x509.NewCertPool()
			for _, cert := range cs.PeerCertificates[1:] {
				intermediates.AddCert(cert)
			}
			_, err := cs.PeerCertificates[0].Verify(x509.VerifyOptions{
				Roots:         caPool,
				Intermediates: intermediates,
				KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
			})
			if err != nil {
				c.logger.Warn("broker TLS chain verification failed",
					zap.String("subject", cs.PeerCertificates[0].Subject.String()),
					zap.Error(err),
				)
			}
			return err
		},
	}
	certHeader := fmt.Sprintf("URI=urn:wendy:org:%d:asset:%d", c.orgID, c.assetID)
	md := metadata.Pairs(
		"x-wendy-client-cert", certHeader,
		"x-forwarded-client-cert", certHeader,
	)
	return []grpc.DialOption{
		grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg)),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                brokerKeepaliveTime,
			Timeout:             brokerKeepaliveTimeout,
			PermitWithoutStream: true,
		}),
	}, md, nil
}

func (c *TunnelBrokerClient) handleDialRequest(ctx context.Context, client cloudpb.TunnelBrokerServiceClient,
	req *cloudpb.DialRequest, devMD metadata.MD) {
	// Only allow loopback connections to prevent broker-directed SSRF.
	ip := net.ParseIP(req.Host)
	if req.Host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		c.logger.Error("broker dial request rejected: only loopback targets allowed",
			zap.String("host", req.Host))
		return
	}

	addr := net.JoinHostPort(req.Host, fmt.Sprint(req.Port))
	c.logger.Info("dialing local service for tunnel",
		zap.String("session_id", req.SessionId), zap.String("addr", addr))

	tcpConn, err := net.DialTimeout("tcp", addr, 10*time.Second)
	if err != nil {
		c.logger.Error("failed to dial local service", zap.String("addr", addr), zap.Error(err))
		return
	}
	defer tcpConn.Close()

	callCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	if devMD != nil {
		callCtx = metadata.NewOutgoingContext(callCtx, devMD)
	}

	agentStream, err := client.AgentTunnel(callCtx)
	if err != nil {
		c.logger.Error("failed to open AgentTunnel stream", zap.Error(err))
		return
	}

	if err := agentStream.Send(&cloudpb.TunnelData{SessionId: req.SessionId}); err != nil {
		c.logger.Error("failed to send join message", zap.Error(err))
		return
	}

	c.relay(callCtx, cancel, tcpConn, agentStream)
}

func (c *TunnelBrokerClient) relay(ctx context.Context, cancel context.CancelFunc,
	tcpConn net.Conn, stream cloudpb.TunnelBrokerService_AgentTunnelClient) {
	done := make(chan struct{}, 2)

	// gRPC -> TCP
	go func() {
		defer func() { done <- struct{}{} }()
		for {
			msg, err := stream.Recv()
			if err != nil {
				break
			}
			if len(msg.Payload) > 0 {
				if _, err := tcpConn.Write(msg.Payload); err != nil {
					break
				}
			}
			if msg.HalfClose {
				if tc, ok := tcpConn.(*net.TCPConn); ok {
					_ = tc.CloseWrite()
				}
				return
			}
		}
		tcpConn.Close()
	}()

	// TCP -> gRPC
	go func() {
		defer func() { done <- struct{}{} }()
		buf := make([]byte, 32*1024)
		for {
			n, readErr := tcpConn.Read(buf)
			if n > 0 {
				payload := make([]byte, n)
				copy(payload, buf[:n])
				if sendErr := stream.Send(&cloudpb.TunnelData{Payload: payload}); sendErr != nil {
					break
				}
			}
			if readErr != nil {
				if readErr == io.EOF {
					_ = stream.Send(&cloudpb.TunnelData{HalfClose: true})
				}
				break
			}
		}
		_ = stream.CloseSend()
	}()

	select {
	case <-done:
		cancel()
	case <-ctx.Done():
	}
	<-done
}
