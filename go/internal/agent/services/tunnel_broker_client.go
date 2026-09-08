package services

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
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

	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	cloudpb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb"
)

const (
	brokerHeartbeatInterval = 30 * time.Second
	brokerMaxBackoff        = 90 * time.Second
	brokerKeepaliveTime     = 30 * time.Second
	brokerKeepaliveTimeout  = 10 * time.Second

	// defaultMTLSPort is the well-known mTLS port the CLI always requests via the
	// broker. When the agent is running on a non-default port, incoming tunnel
	// requests for this port are remapped to the actual local mTLS port.
	defaultMTLSPort = 50052
)

type TunnelBrokerClient struct {
	logger   *zap.Logger
	url      string
	orgID    int32
	assetID  int32
	certPEM  string
	keyPEM   string
	chainPEM string
	mtlsPort int
}

func NewTunnelBrokerClient(logger *zap.Logger, url string, orgID, assetID int32, certPEM, keyPEM, chainPEM string, mtlsPort int) *TunnelBrokerClient {
	return &TunnelBrokerClient{
		logger:   logger,
		url:      url,
		orgID:    orgID,
		assetID:  assetID,
		certPEM:  certPEM,
		keyPEM:   keyPEM,
		chainPEM: chainPEM,
		mtlsPort: mtlsPort,
	}
}

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
	return brokerDialOpts(c.logger, c.orgID, c.assetID, c.certPEM, c.keyPEM, c.chainPEM)
}

// brokerDialOpts returns gRPC dial options and identity metadata for any
// brokerTLSConfig builds the TLS config for a broker connection: it validates
// the broker's chain against the Wendy CA (hostname verification is skipped —
// the broker cert CN is localhost, not the cloud host) and presents the
// device's ECDSA leaf for mTLS.
//
// Loading the client cert is non-fatal: today's broker runs NoClientCert and
// authenticates on the XFCC header, so a failed load still yields a working
// certless connection. The failure is logged with a stable, greppable
// event key and an explicit client_cert_presented=false field so the rollout
// to a cert-requiring broker (phase 2) — where a load failure WILL break
// authentication — can be alerted on from logs. Extracted from brokerDialOpts
// so the cert-load / fallback behavior is unit-testable without a live dial.
func brokerTLSConfig(logger *zap.Logger, certPEM, keyPEM, chainPEM string) (*tls.Config, error) {
	caPool, err := x509.SystemCertPool()
	if err != nil {
		caPool = x509.NewCertPool()
	}
	if chainPEM != "" && certs.AppendChainToPool(caPool, chainPEM) == 0 {
		return nil, fmt.Errorf("no valid CA certificates in chainPEM")
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
				logger.Warn("broker TLS chain verification failed",
					zap.String("subject", cs.PeerCertificates[0].Subject.String()),
					zap.Error(err),
				)
			}
			return err
		},
	}

	// Present the device's ECDSA leaf certificate so the broker can authenticate
	// this connection via mTLS once it starts requesting client certs. Loading
	// only the leaf (not the ML-DSA CA chain) keeps Go's TLS stack from tripping
	// over ML-DSA parse failures.
	if certPEM != "" && keyPEM != "" {
		if clientCert, err := tls.X509KeyPair([]byte(certPEM), []byte(keyPEM)); err != nil {
			logger.Warn("failed to load device client certificate for broker mTLS; presenting none (XFCC header identity still applies)",
				zap.String("event", "broker_mtls_client_cert_load_failed"),
				zap.Bool("client_cert_presented", false),
				zap.Error(err))
		} else {
			tlsCfg.Certificates = []tls.Certificate{clientCert}
		}
	}
	return tlsCfg, nil
}

// agent-originated connection to the tunnel broker. Shared by the presence
// client (serving side) and the mesh dialer (dialing side).
func brokerDialOpts(logger *zap.Logger, orgID, assetID int32, certPEM, keyPEM, chainPEM string) ([]grpc.DialOption, metadata.MD, error) {
	// Identity is asserted two ways during the mTLS rollout:
	//
	//   1. The device's client certificate (mTLS). We present the ECDSA leaf +
	//      key below so the broker can authenticate us cryptographically once it
	//      starts requesting client certs (rollout phase 2). Only the leaf is
	//      presented, never the ML-DSA CA chain — Go's TLS stack rejects ML-DSA
	//      certs at parse time, but the leaf itself is ECDSA and parses fine.
	//   2. The XFCC header (below). Today the broker still runs NoClientCert and
	//      authenticates on the header, so presenting the cert is a no-op on the
	//      wire (the server never sends a CertificateRequest). Presenting it now
	//      makes the deployed fleet ready for the broker to require client certs
	//      without a flag-day cutover.
	//
	// Broker cert CN is localhost and won't match the cloud host — skip hostname
	// verification but still validate the chain against the Wendy CA.
	tlsCfg, err := brokerTLSConfig(logger, certPEM, keyPEM, chainPEM)
	if err != nil {
		return nil, metadata.MD{}, err
	}

	certHeader := fmt.Sprintf("URI=urn:wendy:org:%d:asset:%d", orgID, assetID)
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
		grpc.WithInitialWindowSize(8 * 1024 * 1024),
		grpc.WithInitialConnWindowSize(16 * 1024 * 1024),
		grpc.WithReadBufferSize(256 * 1024),
		grpc.WithWriteBufferSize(256 * 1024),
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

	if req.GetProtocol() == cloudpb.TunnelProtocol_TUNNEL_PROTOCOL_DATAGRAM {
		c.handleDatagramDial(ctx, client, req, devMD)
		return
	}

	port := int(req.Port)
	if c.mtlsPort != 0 && port == defaultMTLSPort && c.mtlsPort != defaultMTLSPort {
		port = c.mtlsPort
	}
	addr := net.JoinHostPort(req.Host, fmt.Sprint(port))
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

// handleDatagramDial claims the session and serves a multiplexed datagram
// relay (UDP flows + ICMP echo). Nothing is dialed upfront; UDP sockets are
// created per flow on first sight, restricted to loopback like TCP dials.
func (c *TunnelBrokerClient) handleDatagramDial(ctx context.Context, client cloudpb.TunnelBrokerServiceClient,
	req *cloudpb.DialRequest, devMD metadata.MD) {
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
	c.logger.Info("serving datagram session", zap.String("session_id", req.SessionId))
	newDatagramRelay(c.logger, agentStream, datagramFlowIdleTimeout).run(callCtx)
}

type agentTunnelStream interface {
	Send(*cloudpb.TunnelData) error
	Recv() (*cloudpb.TunnelData, error)
	CloseSend() error
}

func (c *TunnelBrokerClient) relay(ctx context.Context, cancel context.CancelFunc,
	tcpConn net.Conn, stream agentTunnelStream) {
	done := make(chan struct{}, 2)

	// A hard failure in either copy direction invalidates the whole byte
	// stream. Closing the TCP side is essential: the opposite goroutine may be
	// parked in Read forever, which used to leave this method waiting for its
	// second completion after a registry reset. Cancelling the gRPC side does
	// the symmetric job for a goroutine parked in Recv or Send.
	abort := func() {
		cancel()
		_ = tcpConn.Close()
	}
	closeWrite := func() {
		if tc, ok := tcpConn.(*net.TCPConn); ok {
			_ = tc.CloseWrite()
		}
	}

	// gRPC -> TCP
	go func() {
		defer func() { done <- struct{}{} }()
		for {
			msg, err := stream.Recv()
			if err != nil {
				if errors.Is(err, io.EOF) {
					closeWrite()
				} else {
					abort()
				}
				break
			}
			if len(msg.Payload) > 0 {
				if _, err := tcpConn.Write(msg.Payload); err != nil {
					abort()
					break
				}
			}
			if msg.HalfClose {
				// Half-close: only close the write side so the TCP->gRPC
				// goroutine can still read and forward the backend response.
				closeWrite()
				break
			}
		}
	}()

	// TCP -> gRPC
	go func() {
		defer func() { done <- struct{}{} }()
		buf := make([]byte, 256*1024)
		for {
			n, readErr := tcpConn.Read(buf)
			if n > 0 {
				payload := make([]byte, n)
				copy(payload, buf[:n])
				if sendErr := stream.Send(&cloudpb.TunnelData{Payload: payload}); sendErr != nil {
					abort()
					break
				}
			}
			if readErr != nil {
				if errors.Is(readErr, io.EOF) {
					if sendErr := stream.Send(&cloudpb.TunnelData{HalfClose: true}); sendErr != nil {
						abort()
					}
				} else {
					abort()
				}
				break
			}
		}
		_ = stream.CloseSend()
	}()

	// A clean half-close in either direction must not tear down the other
	// direction before its final frames have reached the broker or backend.
	for completed := 0; completed < 2; completed++ {
		select {
		case <-done:
		case <-ctx.Done():
			abort()
			<-done
			return
		}
	}
}
