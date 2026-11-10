package commands

import (
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/interceptor"
	"github.com/wendylabsinc/wendy/go/internal/agent/mtls"
	"github.com/wendylabsinc/wendy/go/internal/cli/clouddefaults"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshalpn"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"go.uber.org/zap"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
)

// Issuer-path rejection deliberately enables a process-wide fallback cooldown.
// Strict optimization checks use a fresh CLI process, so unrelated rejection
// tests cannot disable their negotiation or resumption before they start.
func isolatedCloudTLSProcess(t *testing.T) bool {
	t.Helper()
	const marker = "WENDY_TEST_CLOUD_TLS_PROCESS"
	if os.Getenv(marker) == t.Name() {
		return true
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^"+regexp.QuoteMeta(t.Name())+"$", "-test.count=1")
	cmd.Env = append(os.Environ(), marker+"="+t.Name())
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("isolated TLS regression: %v\n%s", err, output)
	}
	return false
}

type cloudHandshakeServer struct {
	agentpb.UnimplementedWendyAgentServiceServer
	states chan tls.ConnectionState
}

func (s *cloudHandshakeServer) GetAgentVersion(ctx context.Context, _ *agentpb.GetAgentVersionRequest) (*agentpb.GetAgentVersionResponse, error) {
	p, _ := peer.FromContext(ctx)
	s.states <- p.AuthInfo.(credentials.TLSInfo).State
	return &agentpb.GetAgentVersionResponse{Version: "test"}, nil
}

// Include a real intermediate so the first handshake proves omission and local
// issuer completion, rather than merely negotiating an unused ALPN hint.
func cloudIntermediatePKI(t *testing.T) (config.CertificateInfo, string, string, string, certs.WendyIdentity) {
	t.Helper()
	issue := func(serial int64, name, principal string, ca bool, parent *x509.Certificate, signer *ecdsa.PrivateKey) (*x509.Certificate, string) {
		t.Helper()
		keyPEM, key := newP256KeyPEM(t)
		template := &x509.Certificate{SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: name}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), IsCA: ca, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageDigitalSignature}
		if ca {
			template.KeyUsage |= x509.KeyUsageCertSign
		} else {
			template.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth}
			uri, err := url.Parse(principal)
			if err != nil {
				t.Fatal(err)
			}
			template.URIs = []*url.URL{uri}
		}
		if parent == nil {
			parent, signer = template, key
		}
		der, err := x509.CreateCertificate(rand.Reader, template, parent, key.Public(), signer)
		if err != nil {
			t.Fatal(err)
		}
		certificate, err := x509.ParseCertificate(der)
		if err != nil {
			t.Fatal(err)
		}
		return certificate, keyPEM
	}
	encode := func(c *x509.Certificate) string {
		return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.Raw}))
	}
	root, rootPEM := issue(1, "root", "", true, nil, nil)
	parseKey := func(text string) *ecdsa.PrivateKey {
		block, _ := pem.Decode([]byte(text))
		key, err := x509.ParseECPrivateKey(block.Bytes)
		if err != nil {
			t.Fatal(err)
		}
		return key
	}
	intermediate, intermediatePEM := issue(2, "intermediate", "", true, root, parseKey(rootPEM))
	operator, operatorPEM := issue(3, "operator", "spiffe://wendy.sh/tenant/"+liteTestTenant+"/operator/cloud-inner", false, intermediate, parseKey(intermediatePEM))
	principal := certs.DeviceSPIFFEURI(liteTestTenant, liteTestAssetID)
	serverIssuer, serverIssuerPEM := issue(4, "server issuer", "", true, root, parseKey(rootPEM))
	device, devicePEM := issue(5, "device", principal, false, serverIssuer, parseKey(serverIssuerPEM))
	identity, err := certs.ParsePrincipal(principal)
	if err != nil {
		t.Fatal(err)
	}
	chain := encode(intermediate) + encode(root)
	return config.CertificateInfo{PrincipalURI: "spiffe://wendy.sh/tenant/" + liteTestTenant + "/operator/cloud-inner", PemCertificate: encode(operator), PemPrivateKey: operatorPEM, PemCertificateChain: chain}, encode(device), devicePEM, encode(serverIssuer) + chain, identity
}

func TestCloudAgentTransportKnownIssuersAndResumption(t *testing.T) {
	if !isolatedCloudTLSProcess(t) {
		return
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("WENDY_TLS_SESSION_STORE", "file")
	operator, device, key, chain, expected := cloudIntermediatePKI(t)
	server, err := mtls.NewServer(device, chain, key, zap.NewNop(), time.Time{}, certs.Scope{TenantUUID: liteTestTenant}, interceptor.OrgModeOff)
	if err != nil {
		t.Fatal(err)
	}
	service := &cloudHandshakeServer{states: make(chan tls.ConnectionState, 32)}
	agentpb.RegisterWendyAgentServiceServer(server, service)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go server.Serve(listener)
	t.Cleanup(server.Stop)
	var dials atomic.Int32
	call := func() tls.ConnectionState {
		t.Helper()
		conn, err := connectCloudAgentTransport(context.Background(), operator, expected, clouddefaults.TunnelDialer(func(ctx context.Context) (net.Conn, error) {
			dials.Add(1)
			return (&net.Dialer{}).DialContext(ctx, "tcp", listener.Addr().String())
		}))
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if _, err := conn.AgentService.GetAgentVersion(ctx, &agentpb.GetAgentVersionRequest{}); err != nil {
			t.Fatal(err)
		}
		if identity, ok := conn.ObservedServerIdentity(); !ok || !identity.SameEntity(expected) {
			t.Fatal("lost verified device identity")
		}
		return <-service.states
	}
	first := call()
	if first.DidResume {
		t.Fatal("first connection resumed")
	}
	app, issuers := meshalpn.Decode(first.NegotiatedProtocol)
	if app != "h2" || len(issuers) != 2 || len(first.PeerCertificates) != 1 {
		t.Fatalf("first handshake protocol=%q issuers=%d wire certificates=%d", app, len(issuers), len(first.PeerCertificates))
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		if call().DidResume {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("repeat Cloud connection never resumed")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if dials.Load() < 2 {
		t.Fatal("custom tunnel dialer was not used for every connection")
	}
}
