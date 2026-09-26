package browserauth

import (
	"context"
	"crypto"
	"fmt"
	"net"
	"net/http"
	"strings"

	"github.com/wendylabsinc/wendy/go/internal/cli/clouddefaults"
	"github.com/wendylabsinc/wendy/go/internal/cli/cloudrequest"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/cloudrelay"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	cloudpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	grpcmetadata "google.golang.org/grpc/metadata"
)

// ConnectDevice keeps tunnel authorization, grant verification and device mTLS
// in the caller. dial carries public Cloud/broker HTTP/2 over verified TLS.
func (s *Session) ConnectDevice(ctx context.Context, asset string, dial func(context.Context, string) (net.Conn, error), httpClient *http.Client) (*grpcclient.AgentConnection, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !canonicalUUID(asset) {
		return nil, fmt.Errorf("Invalid cloud device ID")
	}
	if s.tokens.Access == "" {
		return nil, fmt.Errorf("Sign in with Wendy first")
	}
	if err := s.refreshCloudToken(ctx); err != nil {
		return nil, err
	}
	cert, access, key := s.certificate, s.tokens.Access, s.key
	auth := &config.AuthConfig{CloudGRPC: "api.dev.wendy.sh:443", OAuthIssuer: s.meta.Issuer, DPoPPrivateKey: s.privatePEM, Certificates: []config.CertificateInfo{cert}}
	opts := []grpc.DialOption{clouddefaults.TunnelDialer(func(c context.Context) (net.Conn, error) { return dial(c, "api.dev.wendy.sh:443") }), grpc.WithTransportCredentials(insecure.NewCredentials())}
	opts = append(opts, cloudrequest.DPoPDialOptions(auth, func(context.Context) (string, crypto.Signer, error) { return access, key, nil })...)
	cloud, err := grpc.NewClient("passthrough:///api.dev.wendy.sh:443", opts...)
	if err != nil {
		return nil, err
	}
	signer, err := cloudrelay.PrincipalSigner(cert.PemCertificate, []byte(s.privatePEM))
	if err != nil {
		cloud.Close()
		return nil, err
	}
	verifier := &cloudrelay.Verifier{Issuer: "https://api.dev.wendy.sh", HTTP: httpClient, RelayDial: func(endpoint string) (*grpc.ClientConn, error) {
		target, err := cloudrelay.BrowserBrokerTarget(endpoint)
		if err != nil {
			return nil, err
		}
		return grpc.NewClient("passthrough:///"+target, grpc.WithTransportCredentials(insecure.NewCredentials()), clouddefaults.TunnelDialer(func(c context.Context) (net.Conn, error) { return dial(c, target) }))
	}}
	authCtx := grpcmetadata.NewOutgoingContext(ctx, grpcmetadata.Pairs("x-wendy-client-cert", "URI="+cert.PrincipalURI, "x-forwarded-client-cert", "URI="+cert.PrincipalURI))
	record, err := cloudpbv2.NewAssetServiceClient(cloud).GetAsset(authCtx, &cloudpbv2.GetAssetRequest{Id: asset})
	if err != nil {
		cloud.Close()
		return nil, fmt.Errorf("Looking up device certificate identity: %w", err)
	}
	expected, err := cloudDeviceIdentity(record, asset, s.profile.Tenant)
	if err != nil {
		cloud.Close()
		return nil, err
	}

	conn, err := grpcclient.ConnectWithTLSExpecting(ctx, "passthrough:///cloud-device", &cert, nil, expected, clouddefaults.TunnelDialer(func(c context.Context) (net.Conn, error) {
		authCtx := grpcmetadata.NewOutgoingContext(c, grpcmetadata.Pairs("x-wendy-client-cert", "URI="+cert.PrincipalURI, "x-forwarded-client-cert", "URI="+cert.PrincipalURI))
		return cloudrelay.OpenTCP(c, authCtx, cloud, verifier, asset, "wendy-agent", signer)
	}))
	if err != nil {
		cloud.Close()
		return nil, err
	}
	conn.ExtraClosers = append(conn.ExtraClosers, cloud)
	return conn, nil
}

// Cloud asset IDs identify inventory rows. Only the enrollment binding names
// the certificate principal. Never learn this binding from the peer itself.
func cloudDeviceIdentity(asset *cloudpbv2.Asset, id, tenant string) (*certs.WendyIdentity, error) {
	if asset.GetId() != id || asset.GetOrganizationId() != tenant {
		return nil, fmt.Errorf("Cloud returned a device belonging to a different asset or tenant")
	}
	name := asset.GetPkiDeviceName()
	if name == "" {
		return nil, fmt.Errorf("Cloud has no certificate identity recorded for this device")
	}
	for _, segment := range strings.Split(name, "/") {
		if len(segment) == 0 || len(segment) > 64 || segment == "." || segment == ".." {
			return nil, fmt.Errorf("Cloud returned an invalid device certificate identity")
		}
		for _, c := range segment {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-') {
				return nil, fmt.Errorf("Cloud returned an invalid device certificate identity")
			}
		}
	}
	identity, err := certs.ParsePrincipal("spiffe://wendy.sh/tenant/" + tenant + "/device/" + name)
	if err != nil {
		return nil, err
	}
	return &identity, nil
}
