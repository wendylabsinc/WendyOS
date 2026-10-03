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
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/legacycertproof"
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

func (c *TunnelBrokerClient) runOnce(ctx context.Context) (retErr error) {
	defer func() {
		if status.Code(retErr) == codes.Unimplemented {
			retErr = fmt.Errorf("cloud endpoint %s does not implement %s; agent and cloud presence protocols are incompatible: %w",
				c.url, cloudpb.TunnelBrokerService_RegisterPresence_FullMethodName, retErr)
		}
	}()
	dialOpts, requestMetadata, err := c.buildDialOpts()
	if err != nil {
		return err
	}
	conn, err := grpc.NewClient(c.url, dialOpts...)
	if err != nil {
		return err
	}
	defer conn.Close()

	client := cloudpb.NewTunnelBrokerServiceClient(conn)

	devMD, err := requestMetadata(cloudpb.TunnelBrokerService_RegisterPresence_FullMethodName)
	if err != nil {
		return err
	}
	callCtx := metadata.NewOutgoingContext(ctx, devMD)
	stream, err := client.RegisterPresence(callCtx)
	if err != nil {
		return err
	}

	// Opening a gRPC stream does not prove the server accepted the RPC. The
	// first Recv may still return Unimplemented or an authentication refusal.
	c.logger.Debug("opened broker presence stream; awaiting server response",
		zap.String("url", c.url))

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
			go c.handleDialRequest(ctx, client, req, requestMetadata)
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

func (c *TunnelBrokerClient) buildDialOpts() ([]grpc.DialOption, brokerRequestMetadata, error) {
	return brokerDialOpts(c.logger, c.orgID, c.assetID, c.certPEM, c.keyPEM, c.chainPEM)
}

// brokerTLSConfig validates the broker certificate chain against system and
// provisioned Wendy roots. gRPC supplies the endpoint name for DNS/IP SAN
// verification. The device leaf is presented for direct endpoint mTLS.
//
// Loading the TLS client cert remains non-fatal because Cloud Run terminates
// TLS before the broker. Request authentication separately proves possession
// of this key; retaining the TLS presentation supports direct broker endpoints.
// Extracted so the cert-load fallback is unit-testable without a live dial.
func brokerTLSConfig(logger *zap.Logger, certPEM, keyPEM, chainPEM string) (*tls.Config, error) {
	caPool, err := x509.SystemCertPool()
	if err != nil {
		caPool = x509.NewCertPool()
	}
	if chainPEM != "" && certs.AppendChainToPool(caPool, chainPEM) == 0 {
		return nil, fmt.Errorf("no valid CA certificates in chainPEM")
	}
	// gRPC sets ServerName from the configured endpoint (including IP
	// literals). Standard verification checks both the trusted chain and its
	// DNS/IP SAN; trusting a public or Wendy CA alone is not broker identity.
	tlsCfg := &tls.Config{
		MinVersion: tls.VersionTLS12,
		RootCAs:    caPool,
	}

	// Present the device's ECDSA leaf certificate so the broker can authenticate
	// this connection via mTLS once it starts requesting client certs. Loading
	// only the leaf (not the ML-DSA CA chain) keeps Go's TLS stack from tripping
	// over ML-DSA parse failures.
	if certPEM != "" && keyPEM != "" {
		if clientCert, err := tls.X509KeyPair([]byte(certPEM), []byte(keyPEM)); err != nil {
			logger.Warn("failed to load device client certificate for broker mTLS; presenting none",
				zap.String("event", "broker_mtls_client_cert_load_failed"),
				zap.Bool("client_cert_presented", false),
				zap.Error(err))
		} else {
			tlsCfg.Certificates = []tls.Certificate{clientCert}
		}
	}
	return tlsCfg, nil
}

// brokerRequestMetadata signs identity metadata for an agent-originated RPC.
type brokerRequestMetadata func(fullMethod string) (metadata.MD, error)

// brokerDialOpts returns gRPC options and identity metadata shared by the
// presence client (serving side) and the mesh dialer (dialing side).
func brokerDialOpts(logger *zap.Logger, orgID, assetID int32, certPEM, keyPEM, chainPEM string) ([]grpc.DialOption, brokerRequestMetadata, error) {
	// Cloud Run cannot forward the TLS client certificate to the broker, so each
	// RPC carries a fresh method-bound signature from the enrolled certificate
	// key. Legacy XFCC headers remain during the additive rollout only. Direct
	// broker endpoints also receive the certificate at the TLS layer.
	//
	// Standard TLS binds the broker certificate to the configured endpoint.
	// Local development broker certificates carry localhost/LAN-IP SANs.
	tlsCfg, err := brokerTLSConfig(logger, certPEM, keyPEM, chainPEM)
	if err != nil {
		return nil, nil, err
	}

	identityURI := fmt.Sprintf("urn:wendy:org:%d:asset:%d", orgID, assetID)
	certHeader := "URI=" + identityURI
	legacyMD := metadata.Pairs(
		"x-wendy-client-cert", certHeader,
		"x-forwarded-client-cert", certHeader,
	)
	var proofSigner *legacycertproof.Signer
	if certPEM != "" && keyPEM != "" {
		proofSigner, err = legacycertproof.New(identityURI, certPEM, keyPEM)
		if err != nil {
			logger.Warn("failed to initialize certificate-bound broker authentication; legacy headers still apply",
				zap.String("event", "broker_certificate_proof_unavailable"),
				zap.Error(err))
		}
	}
	requestMetadata := func(fullMethod string) (metadata.MD, error) {
		md := legacyMD.Copy()
		if proofSigner == nil {
			return md, nil
		}
		proofMD, err := proofSigner.Metadata(fullMethod)
		if err != nil {
			return nil, fmt.Errorf("create certificate-bound broker authentication: %w", err)
		}
		return metadata.Join(md, proofMD), nil
	}
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
	}, requestMetadata, nil
}

func (c *TunnelBrokerClient) handleDialRequest(ctx context.Context, client cloudpb.TunnelBrokerServiceClient,
	req *cloudpb.DialRequest, requestMetadata brokerRequestMetadata) {
	// Only allow loopback connections to prevent broker-directed SSRF.
	ip := net.ParseIP(req.Host)
	if req.Host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		c.logger.Error("broker dial request rejected: only loopback targets allowed",
			zap.String("host", req.Host))
		return
	}

	if req.GetProtocol() == cloudpb.TunnelProtocol_TUNNEL_PROTOCOL_DATAGRAM {
		c.handleDatagramDial(ctx, client, req, requestMetadata)
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

	devMD, err := requestMetadata(cloudpb.TunnelBrokerService_AgentTunnel_FullMethodName)
	if err != nil {
		c.logger.Error("failed to authenticate AgentTunnel stream", zap.Error(err))
		return
	}
	callCtx = metadata.NewOutgoingContext(callCtx, devMD)
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
	req *cloudpb.DialRequest, requestMetadata brokerRequestMetadata) {
	callCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	devMD, err := requestMetadata(cloudpb.TunnelBrokerService_AgentTunnel_FullMethodName)
	if err != nil {
		c.logger.Error("failed to authenticate datagram AgentTunnel stream", zap.Error(err))
		return
	}
	callCtx = metadata.NewOutgoingContext(callCtx, devMD)
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
