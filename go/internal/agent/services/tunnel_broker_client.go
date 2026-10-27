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
	"sync/atomic"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/legacycertproof"
	cloudpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
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

// brokerRunOnceFn, brokerNowFn and brokerAfterFn are seams for tests.
var (
	brokerRunOnceFn = (*TunnelBrokerClient).runOnce
	brokerNowFn     = time.Now
	brokerAfterFn   = time.After
)

type TunnelBrokerClient struct {
	logger   *zap.Logger
	url      string
	certPEM  string
	keyPEM   string
	chainPEM string
	mtlsPort int

	// sessions counts live tunnel sessions; they ride the presence connection.
	sessions atomic.Int32
}

// NewTunnelBrokerClient serves the Cloud relay's presence leg for this device.
// The identity the broker authenticates is read off the provisioned leaf
// certificate at each connection — the SPIFFE device principal pki-core
// stamps on it — so enrollment while the agent runs is picked up on the next
// reconnect without rebuilding the client.
func NewTunnelBrokerClient(logger *zap.Logger, url, certPEM, keyPEM, chainPEM string, mtlsPort int) *TunnelBrokerClient {
	return &TunnelBrokerClient{
		logger:   logger,
		url:      url,
		certPEM:  certPEM,
		keyPEM:   keyPEM,
		chainPEM: chainPEM,
		mtlsPort: mtlsPort,
	}
}

// brokerIdentityFromCert reads the principal URI the broker must authenticate
// this agent as: the SPIFFE device principal on a v2 leaf, or the legacy URN
// on an old-chain certificate. Empty means no provisioned identity — presence
// cannot authenticate, so runOnce refuses to dial rather than connecting
// anonymously.
func brokerIdentityFromCert(logger *zap.Logger, certPEM string) string {
	if certPEM == "" {
		return ""
	}
	leaves, err := certs.ParseCertsFromPEM([]byte(certPEM))
	if err != nil || len(leaves) == 0 {
		logger.Warn("broker presence: device certificate could not be read", zap.Error(err))
		return ""
	}
	identity, found, err := certs.IdentityFromCert(leaves[0])
	if err != nil || !found {
		return ""
	}
	return identity.IdentityKey()
}

func (c *TunnelBrokerClient) Run(ctx context.Context) {
	attempt := 0
	for {
		started := brokerNowFn()
		if err := brokerRunOnceFn(c, ctx); err != nil {
			if ctx.Err() != nil {
				return
			}
			moved := errors.Is(err, errBrokerPathMoved)
			// A path move or a session that outlived the longest backoff is not a
			// failure, but still pause: the broker must drop the old stream first.
			if moved || brokerNowFn().Sub(started) >= brokerMaxBackoff {
				attempt = 0
			}
			backoff := time.Duration(math.Min(
				float64(time.Second)*math.Pow(2, float64(attempt)),
				float64(brokerMaxBackoff),
			))
			if !moved {
				c.logger.Warn("broker connection failed, reconnecting",
					zap.Error(err), zap.Duration("backoff", backoff))
			}
			select {
			case <-brokerAfterFn(backoff):
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
				c.url, cloudpbv2.TunnelBrokerService_RegisterPresence_FullMethodName, retErr)
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

	client := cloudpbv2.NewTunnelBrokerServiceClient(conn)

	devMD, err := requestMetadata(cloudpbv2.TunnelBrokerService_RegisterPresence_FullMethodName)
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
	pathTicker := time.NewTicker(brokerPathCheckInterval)
	defer pathTicker.Stop()
	var pathWatch brokerPathWatch
	var lastHeartbeat time.Time

	recvCh := make(chan *cloudpbv2.DialRequest, 8)
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
			c.sessions.Add(1)
			go func() {
				defer c.sessions.Add(-1)
				c.handleDialRequest(ctx, client, req, requestMetadata)
			}()
		case err := <-recvErr:
			if err == io.EOF {
				return nil
			}
			return err
		case <-hbTicker.C:
			if err := stream.Send(&cloudpbv2.AgentHeartbeat{}); err != nil {
				return err
			}
			lastHeartbeat = time.Now()
		case <-pathTicker.C:
			p, _ := peer.FromContext(stream.Context())
			preferred, moved := brokerPathMoved(p)
			busy := func() bool {
				return brokerBusy(c.sessions.Load(), len(recvCh), time.Since(lastHeartbeat))
			}
			if !pathWatch.shouldRedial(moved, busy()) {
				continue
			}
			if !brokerPathReachableFn(ctx, c.url, dialOpts) {
				if ctx.Err() != nil {
					return nil
				}
				c.logger.Info("preferred network path does not reach the broker; keeping the current one",
					zap.Stringer("current", p.LocalAddr), zap.Stringer("preferred", preferred))
				pathWatch.unreachable()
				continue
			}
			// The probe took time; a session may have arrived meanwhile.
			if busy() {
				continue
			}
			c.logger.Info("moving the broker connection to the preferred network path",
				zap.Stringer("from", p.LocalAddr), zap.Stringer("to", preferred))
			return errBrokerPathMoved
		case <-ctx.Done():
			return nil
		}
	}
}

func (c *TunnelBrokerClient) buildDialOpts() ([]grpc.DialOption, brokerRequestMetadata, error) {
	identityURI := brokerIdentityFromCert(c.logger, c.certPEM)
	if identityURI == "" {
		return nil, nil, errors.New("no provisioned device identity; broker presence requires an enrolled certificate to authenticate with")
	}
	return brokerIdentityDialOpts(c.logger, identityURI, c.certPEM, c.keyPEM, c.chainPEM)
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

	// Present the device's leaf certificate (P-256 or ML-DSA-65) so the broker can authenticate
	// this connection via mTLS once it starts requesting client certs. Loading
	// only the leaf (not the ML-DSA CA chain) keeps Go's TLS stack from tripping
	// over ML-DSA parse failures.
	if certPEM != "" && keyPEM != "" {
		if clientCert, err := tls.X509KeyPair([]byte(certPEM), []byte(keyPEM)); err != nil {
			logger.Warn("failed to load device client certificate for broker mTLS; presenting none",
				zap.String("event", "broker_mtls_client_cert_load_failed"),
				zap.String("auth_downgrade", "mtls_client_cert->none"),
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

// brokerIdentityDialOpts returns gRPC options and certificate-bound identity
// metadata for a broker caller authenticating as a principal URI — the SPIFFE
// device principal on a v2 leaf, the legacy URN on an old-chain certificate.
func brokerIdentityDialOpts(logger *zap.Logger, identityURI, certPEM, keyPEM, chainPEM string) ([]grpc.DialOption, brokerRequestMetadata, error) {
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
				zap.String("auth_downgrade", "signed_proof->unsigned_headers"),
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

func (c *TunnelBrokerClient) handleDialRequest(ctx context.Context, client cloudpbv2.TunnelBrokerServiceClient,
	req *cloudpbv2.DialRequest, requestMetadata brokerRequestMetadata) {
	// Only allow loopback connections to prevent broker-directed SSRF.
	ip := net.ParseIP(req.Host)
	if req.Host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		c.logger.Error("broker dial request rejected: only loopback targets allowed",
			zap.String("host", req.Host))
		return
	}

	if req.GetProtocol() == cloudpbv2.TunnelProtocol_TUNNEL_PROTOCOL_DATAGRAM {
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

	devMD, err := requestMetadata(cloudpbv2.TunnelBrokerService_AgentTunnel_FullMethodName)
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

	if err := agentStream.Send(&cloudpbv2.TunnelData{SessionId: req.SessionId}); err != nil {
		c.logger.Error("failed to send join message", zap.Error(err))
		return
	}

	c.relay(callCtx, cancel, tcpConn, agentStream)
}

// handleDatagramDial claims the session and serves a multiplexed datagram
// relay (UDP flows + ICMP echo). Nothing is dialed upfront; UDP sockets are
// created per flow on first sight, restricted to loopback like TCP dials.
func (c *TunnelBrokerClient) handleDatagramDial(ctx context.Context, client cloudpbv2.TunnelBrokerServiceClient,
	req *cloudpbv2.DialRequest, requestMetadata brokerRequestMetadata) {
	callCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	devMD, err := requestMetadata(cloudpbv2.TunnelBrokerService_AgentTunnel_FullMethodName)
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
	if err := agentStream.Send(&cloudpbv2.TunnelData{SessionId: req.SessionId}); err != nil {
		c.logger.Error("failed to send join message", zap.Error(err))
		return
	}
	c.logger.Info("serving datagram session", zap.String("session_id", req.SessionId))
	newDatagramRelay(c.logger, agentStream, datagramFlowIdleTimeout).run(callCtx)
}

type agentTunnelStream interface {
	Send(*cloudpbv2.TunnelData) error
	Recv() (*cloudpbv2.TunnelData, error)
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
				if sendErr := stream.Send(&cloudpbv2.TunnelData{Payload: payload}); sendErr != nil {
					abort()
					break
				}
			}
			if readErr != nil {
				if errors.Is(readErr, io.EOF) {
					if sendErr := stream.Send(&cloudpbv2.TunnelData{HalfClose: true}); sendErr != nil {
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
