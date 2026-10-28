package browserauth

import (
	"context"
	"crypto"
	"crypto/tls"
	"encoding/asn1"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/clouddefaults"
	"github.com/wendylabsinc/wendy/go/internal/cli/cloudrequest"
	"github.com/wendylabsinc/wendy/go/internal/cli/liteenroll"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/cloudrelay"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/literelay"
	"github.com/wendylabsinc/wendy/go/internal/shared/litetopology"
	cloud "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

type liteReporter struct {
	Conn   net.Conn
	Peer   certs.WendyIdentity
	Read   litetopology.ReadPage
	cancel context.CancelFunc
}

func (c *liteReporter) Close() error {
	if c.cancel != nil {
		c.cancel()
	}
	return c.Conn.Close()
}
func liteAsset(asset *cloud.Asset) bool {
	return strings.EqualFold(asset.GetOsType(), "wendy-lite") || strings.EqualFold(asset.GetDeviceType(), "wendy-lite") || strings.EqualFold(asset.GetDeviceType(), "micro-wendy")
}

// The compatible operator leaf is ephemeral and remains inside this session.
// Token rotation is saved even if the later certificate request fails.
func (s *Session) liteOperatorLocked(ctx context.Context) (config.CertificateInfo, error) {
	cached := s.liteCertificate
	leaves, err := certs.ParseCertsFromPEM([]byte(cached.PemCertificate))
	if err == nil && len(leaves) == 1 && cached.PrincipalURI == s.profile.Principal && time.Until(leaves[0].NotAfter) > time.Minute {
		return cached, nil
	}
	identity, err := s.exchange(ctx, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {s.tokens.Refresh}, "resource": {s.settings().IdentityResource}})
	if err != nil {
		return config.CertificateInfo{}, err
	}
	if identity.Refresh != "" {
		s.tokens.Refresh = identity.Refresh
	}
	s.tokens.Access = ""
	s.profile.Expires = ""
	if err := s.saveLocked(); err != nil {
		return config.CertificateInfo{}, err
	}
	claims, issueErr := s.verifyAccess(ctx, identity.Access, s.settings().IdentityResource)
	var certificate config.CertificateInfo
	if issueErr == nil && (claims["sub"] != s.profile.Subject || claims["tenant_uuid"] != s.profile.Tenant) {
		issueErr = fmt.Errorf("Lite identity token changed session principal")
	}
	if issueErr == nil {
		var private string
		private, issueErr = certs.GenerateKeyPair()
		if issueErr == nil {
			certificate, issueErr = requestPKIIdentityCertificate(ctx, s.Client, s.settings().IdentityEndpoint, private, s.key, identity.Access, s.profile.Tenant, s.profile.Subject)
		}
	}
	// Restore the Cloud resource before exposing another connection snapshot.
	rotateErr := s.rotateCloudToken(ctx)
	if issueErr != nil {
		return config.CertificateInfo{}, issueErr
	}
	if rotateErr != nil {
		return config.CertificateInfo{}, rotateErr
	}
	leaves, err = certs.ParseCertsFromPEM([]byte(certificate.PemCertificate))
	if err != nil || len(leaves) != 1 {
		return config.CertificateInfo{}, fmt.Errorf("invalid Lite operator certificate")
	}
	marked := false
	oid := asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 65441, 1, 9}
	for _, usage := range leaves[0].UnknownExtKeyUsage {
		marked = marked || usage.Equal(oid)
	}
	if !marked {
		return config.CertificateInfo{}, fmt.Errorf("PKI did not issue the Lite operator profile")
	}
	if _, err := cloudrelay.LitePrincipalSigner(certificate.PemCertificate, []byte(certificate.PemPrivateKey)); err != nil {
		return config.CertificateInfo{}, err
	}
	s.liteCertificate = certificate
	return certificate, nil
}
func (s *Session) connectLiteReporter(ctx context.Context, cc *grpc.ClientConn, asset string, expected *certs.WendyIdentity, dial func(context.Context, string) (net.Conn, error), client *http.Client) (*liteReporter, error) {
	if err := s.lockReporterIdentity(ctx); err != nil {
		return nil, err
	}
	identity, err := s.liteOperatorLocked(ctx)
	active, access, key, private, settings := s.certificate, s.tokens.Access, s.key, s.privatePEM, s.settings()
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	certificate, err := certs.TLSKeyPair(identity.PemCertificate, identity.PemCertificateChain, identity.PemPrivateKey)
	if err != nil {
		return nil, err
	}
	sign, err := cloudrelay.LitePrincipalSigner(identity.PemCertificate, []byte(identity.PemPrivateKey))
	if err != nil {
		return nil, err
	}
	options := literelay.RelayOptions{OperatorIdentity: identity, DeviceTrustChainPEM: active.PemCertificateChain, SignRequest: sign,
		DeviceTrustFallback: func(ctx context.Context) (string, error) {
			return liteenroll.DiscoverLiteDeviceCAChainWithClient(ctx, client, settings.IdentityEndpoint, expected.TenantUUID)
		}}
	options.Redial = func(ctx context.Context, endpoint string) (*grpc.ClientConn, context.Context, error) {
		target, err := cloudrelay.BrowserBrokerTarget(endpoint)
		if err != nil {
			return nil, ctx, err
		}
		auth := &config.AuthConfig{DPoPPrivateKey: private, Certificates: []config.CertificateInfo{active}}
		opts := []grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials()), clouddefaults.TunnelDialer(func(ctx context.Context) (net.Conn, error) { return dial(ctx, target) })}
		opts = append(opts, cloudrequest.DPoPDialOptions(auth, func(context.Context) (string, crypto.Signer, error) { return access, key, nil })...)
		conn, err := grpc.NewClient("passthrough:///"+target, opts...)
		return conn, ctx, err
	}
	cfg, err := literelay.DeviceTLSConfig(ctx, certificate, options.DeviceTrustChainPEM, expected.EntityID, options.DeviceTrustFallback)
	if err != nil {
		return nil, err
	}
	raw, err := literelay.Open(ctx, cc, asset, options)
	if err != nil {
		return nil, err
	}
	failed := true
	defer func() {
		if failed {
			raw.Close()
		}
	}()
	handshake, cancelHandshake := context.WithTimeout(ctx, browserLiteHandshakeBudget)
	defer cancelHandshake()
	conn := tls.Client(raw, cfg)
	if err := conn.HandshakeContext(handshake); err != nil {
		return nil, err
	}
	if deadline, ok := handshake.Deadline(); ok {
		if err := conn.SetDeadline(deadline); err != nil {
			return nil, err
		}
	}
	if _, err := conn.Write(raw.Grant()); err != nil {
		return nil, err
	}
	var ack [4]byte
	if _, err := io.ReadFull(conn, ack[:]); err != nil {
		return nil, err
	}
	if string(ack[:]) != "WLA2" {
		return nil, fmt.Errorf("device refused Lite session grant")
	}
	if err := raw.Established(); err != nil {
		return nil, err
	}
	// Close the pipe first; TLS close_notify must never wait behind a broker.
	read, err := litetopology.OpenStream(handshake, conn)
	if err != nil {
		return nil, err
	}
	failed = false
	return &liteReporter{Read: read, Conn: &liteTLSConn{Conn: conn, raw: raw}, Peer: *expected}, nil
}

type liteTLSConn struct {
	*tls.Conn
	raw net.Conn
}

func (c *liteTLSConn) Close() error { return c.raw.Close() }
