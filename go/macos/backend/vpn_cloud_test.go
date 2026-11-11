package backend

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/url"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/wendylabsinc/wendy/go/internal/agent/meshingress"
	"github.com/wendylabsinc/wendy/go/internal/agent/services"
	"github.com/wendylabsinc/wendy/go/internal/cli/commands"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/macos/vpn"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

type cloudVPNAgent struct {
	agentpb.UnimplementedWendyAgentServiceServer
}

func (cloudVPNAgent) GetAgentVersion(context.Context, *agentpb.GetAgentVersionRequest) (*agentpb.GetAgentVersionResponse, error) {
	return &agentpb.GetAgentVersionResponse{}, nil
}

type cloudVPNAdmission struct{ address string }

func (a cloudVPNAdmission) DialAuthorized(port uint16, dial func(string) (net.Conn, error)) (net.Conn, error) {
	if port != 18080 {
		return nil, meshingress.ErrPortDenied
	}
	return dial(a.address)
}
func cloudVPNCertificate(t *testing.T, parent *x509.Certificate, parentKey *ecdsa.PrivateKey, principal string, ca bool) (*x509.Certificate, *ecdsa.PrivateKey, tls.Certificate) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: "VPN fixture"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}, BasicConstraintsValid: true, IsCA: ca}
	if ca {
		template.KeyUsage |= x509.KeyUsageCertSign
	}
	if principal != "" {
		uri, err := url.Parse(principal)
		if err != nil {
			t.Fatal(err)
		}
		template.URIs = []*url.URL{uri}
	}
	if parent == nil {
		parent = template
		parentKey = key
	}
	der, err := x509.CreateCertificate(rand.Reader, template, parent, &key.PublicKey, parentKey)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return cert, key, tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// Compose Cloud-only DNS and the native relay with real operator/device mTLS,
// MeshDial and destination app admission. No mesh route or raw app relay exists.
func TestCloudPrivateNamesReachAuthorizedApplication(t *testing.T) {
	const tenant = "11111111-1111-4111-8111-111111111111"
	const asset = "22222222-2222-4222-8222-222222222222"
	const device = "33333333-3333-4333-8333-333333333333"
	principal := "spiffe://wendy.sh/tenant/" + tenant + "/device/" + device
	selector := "cloud://api.wendy.dev/tenant/" + tenant + "/asset/" + asset
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	routes, err := prepareVPNRoutes(ctx, []vpn.Route{{Selector: selector, Name: "untrusted", Principal: "untrusted"}}, func(_ context.Context, selected []string) (map[string]commands.DesktopCloudRouteIdentity, error) {
		if len(selected) != 1 || selected[0] != selector {
			t.Fatal(selected)
		}
		return map[string]commands.DesktopCloudRouteIdentity{selector: {Name: "jetson", Principal: principal}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	echo, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer echo.Close()
	go func() {
		for {
			c, err := echo.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); _, _ = io.Copy(c, c) }()
		}
	}()
	root, key, _ := cloudVPNCertificate(t, nil, nil, "", true)
	_, _, serverCert := cloudVPNCertificate(t, root, key, principal, false)
	_, _, operatorCert := cloudVPNCertificate(t, root, key, "spiffe://wendy.sh/tenant/"+tenant+"/operator/alice", false)
	pool := x509.NewCertPool()
	pool.AddCert(root)
	server := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{Certificates: []tls.Certificate{serverCert}, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: pool, MinVersion: tls.VersionTLS13})))
	app := services.NewMeshService(zap.NewNop(), t.TempDir(), certs.Scope{TenantUUID: tenant})
	app.SetIngressAuthorizer(cloudVPNAdmission{echo.Addr().String()})
	agentpbv2.RegisterWendyMeshServiceServer(server, app)
	agentpb.RegisterWendyAgentServiceServer(server, cloudVPNAgent{})
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go server.Serve(listener)
	defer server.Stop()
	connect := func(ctx context.Context, selected string) (*grpcclient.AgentConnection, error) {
		if selected != selector {
			t.Error("changed Cloud selector", selected)
		}
		observer := new(grpcclient.VerifiedServerIdentity)
		identity := certs.WendyIdentity{EntityType: certs.EntityAsset, EntityID: device, TenantUUID: tenant, Principal: principal}
		verify, err := certs.BuildServerVerifyConnection(certs.ServerVerifyOpts{ChainPEM: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: root.Raw})), ExpectedIdentity: &identity, OnVerifiedServerIdentity: observer.Observe})
		if err != nil {
			return nil, err
		}
		// The production identity verifier validates chain and the full SPIFFE identity.
		cc, err := grpc.NewClient(listener.Addr().String(), grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{Certificates: []tls.Certificate{operatorCert}, InsecureSkipVerify: true, VerifyConnection: verify, MinVersion: tls.VersionTLS13})))
		if err != nil {
			return nil, err
		}
		agent := grpcclient.NewFromConnWithVerifiedIdentity(cc, observer)
		agent.IsMTLS = true
		agent.RegistryDialer = func(context.Context, int) (net.Conn, error) {
			t.Error("raw Cloud app relay used")
			return nil, meshingress.ErrPortDenied
		}
		return agent, nil
	}
	dial := func(ctx context.Context, selected string, port uint16) (net.Conn, error) {
		return desktopDialWithConnect(ctx, selected, port, routes[0].Principal, connect)
	}
	relay, cfg, err := vpn.NewRelay(routes, dial, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer relay.Close()
	for _, name := range []string{"jetson.wendy.internal", "device-" + device + ".wendy.internal"} {
		q := new(dns.Msg)
		q.SetQuestion(dns.Fqdn(name), dns.TypeA)
		wire, _ := q.Pack()
		answer, err := vpn.AnswerDNS(wire, cfg.Routes)
		if err != nil {
			t.Fatal(err)
		}
		reply := new(dns.Msg)
		if err = reply.Unpack(answer); err != nil || len(reply.Answer) != 1 {
			t.Fatal(reply, err)
		}
		conn, err := vpn.DialRelay(ctx, cfg, reply.Answer[0].(*dns.A).A.String(), 18080)
		if err != nil {
			t.Fatal(name, err)
		}
		payload := "Cloud private alias data"
		if _, err = io.WriteString(conn, payload); err != nil {
			t.Fatal(err)
		}
		got := make([]byte, len(payload))
		if _, err = io.ReadFull(conn, got); err != nil || string(got) != payload {
			t.Fatal(string(got), err)
		}
		conn.Close()
	}
	if c, err := vpn.DialRelay(ctx, cfg, cfg.Routes[0].Address, 18081); err == nil {
		c.Close()
		t.Fatal("unpublished port admitted")
	}
	if c, err := desktopDialWithConnect(ctx, selector, 18080, "spiffe://wendy.sh/tenant/"+tenant+"/device/"+asset, connect); err == nil {
		c.Close()
		t.Fatal("changed device identity admitted")
	}
}

func TestLegacyCloudRouteRetainsEstablishedPortTransport(t *testing.T) {
	selector := "cloud://api.wendy.dev/org/7/asset/42"
	connect := func(context.Context, string) (*grpcclient.AgentConnection, error) {
		return &grpcclient.AgentConnection{RegistryDialer: func(_ context.Context, port int) (net.Conn, error) {
			if port != 18080 {
				t.Fatal(port)
			}
			client, server := net.Pipe()
			server.Close()
			return client, nil
		}}, nil
	}
	conn, err := desktopDialWithConnect(context.Background(), selector, 18080, "", connect)
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
}
