package browserauth

import (
	"context"
	"crypto"
	"crypto/tls"
	"fmt"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	cloudpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
	"net"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/clouddefaults"
	"github.com/wendylabsinc/wendy/go/internal/cli/cloudrequest"
	"github.com/wendylabsinc/wendy/go/internal/shared/cloudrelay"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// OpenService opens only a Cloud catalog symbol, never an arbitrary address.
// The relay verifies the signed grant and the device's authenticated relay join.
func (m *MachineSession) OpenService(ctx context.Context, tenant, subject, device, service string) (net.Conn, error) {
	if tenant != m.tenant || subject != m.subject || !canonicalUUID(device) {
		return nil, fmt.Errorf("machine identity or device mismatch")
	}
	auth, err := m.Credentials(ctx)
	if err != nil {
		return nil, err
	}
	settings := m.session.settings()
	dialer := &tls.Dialer{Config: &tls.Config{MinVersion: tls.VersionTLS13, NextProtos: []string{"h2"}}}
	dial := func(c context.Context, address string) (net.Conn, error) {
		return dialer.DialContext(c, "tcp", address)
	}
	options := []grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials()), clouddefaults.TunnelDialer(func(c context.Context) (net.Conn, error) { return dial(c, settings.CloudGRPC) })}
	options = append(options, cloudrequest.DPoPDialOptions(auth, func(context.Context) (string, crypto.Signer, error) { return auth.APIKey, m.session.key, nil })...)
	cloud, err := grpc.NewClient("passthrough:///"+settings.CloudGRPC, options...)
	if err != nil {
		return nil, err
	}
	signer, err := cloudrelay.PrincipalSigner(auth.Certificates[0].PemCertificate, []byte(auth.DPoPPrivateKey))
	if err != nil {
		cloud.Close()
		return nil, err
	}
	verifier := &cloudrelay.Verifier{Issuer: settings.RelayIssuer, RelayDial: func(endpoint string) (*grpc.ClientConn, error) {
		target, err := cloudrelay.BrowserBrokerTarget(endpoint)
		if err != nil {
			return nil, err
		}
		return grpc.NewClient("passthrough:///"+target, grpc.WithTransportCredentials(insecure.NewCredentials()), clouddefaults.TunnelDialer(func(c context.Context) (net.Conn, error) { return dial(c, target) }))
	}}
	socket, err := cloudrelay.OpenTCP(ctx, ctx, cloud, verifier, device, service, signer)
	if err != nil {
		cloud.Close()
		return nil, err
	}
	if service == "wendy-registry" || service == "wendy-registry-darwin" {
		// Enrolled registries speak mTLS. Terminate it here using the machine
		// identity, so CLI image tooling sees HTTP inside its HTTPS WebSocket.
		record, err := cloudpbv2.NewAssetServiceClient(cloud).GetAsset(ctx, &cloudpbv2.GetAssetRequest{Id: device})
		if err != nil {
			socket.Close()
			cloud.Close()
			return nil, err
		}
		expected, err := cloudDeviceIdentity(record, device, tenant)
		if err != nil {
			socket.Close()
			cloud.Close()
			return nil, err
		}
		certificate := auth.Certificates[0]
		pair, err := certs.TLSKeyPair(certificate.PemCertificate, certificate.PemCertificateChain, auth.DPoPPrivateKey)
		if err != nil {
			socket.Close()
			cloud.Close()
			return nil, err
		}
		verify, err := certs.BuildServerVerifyConnection(certs.ServerVerifyOpts{ChainPEM: certificate.PemCertificateChain, ExpectedIdentity: expected})
		if err != nil {
			socket.Close()
			cloud.Close()
			return nil, err
		}
		secured := tls.Client(socket, &tls.Config{
			MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{pair}, NextProtos: []string{"http/1.1"},
			// Wendy SPIFFE identity replaces DNS-name verification; verify checks
			// the chain, validity and exact enrolled device identity.
			InsecureSkipVerify: true, VerifyConnection: verify,
			GetClientCertificate: func(request *tls.CertificateRequestInfo) (*tls.Certificate, error) {
				supported := *request
				supported.AcceptableCAs = nil
				if err := supported.SupportsCertificate(&pair); err != nil {
					return nil, err
				}
				return &pair, nil
			},
		})
		handshake, stop := context.WithTimeout(ctx, 15*time.Second)
		err = secured.HandshakeContext(handshake)
		stop()
		if err != nil {
			secured.Close()
			cloud.Close()
			return nil, err
		}
		socket = secured
	}
	return &machineTunnel{Conn: socket, cloud: cloud}, nil
}

type machineTunnel struct {
	net.Conn
	cloud *grpc.ClientConn
}

func (t *machineTunnel) Close() error { err := t.Conn.Close(); t.cloud.Close(); return err }
