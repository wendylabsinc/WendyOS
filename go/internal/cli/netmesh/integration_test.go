package netmesh

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/mldsa"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/netip"
	"net/url"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/wendylabsinc/WendyOS/go/babel"
	"github.com/wendylabsinc/wendy/go/internal/agent/interceptor"
	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/agent/meshsession"
	"github.com/wendylabsinc/wendy/go/internal/agent/mtls"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshoperator"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshoperatorlink"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	grpccredentials "google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv6"
)

type fixtureAuthority struct {
	certificate *x509.Certificate
	key         crypto.Signer
}

func fixtureCredentials(t *testing.T, roots ...*fixtureAuthority) []*localmesh.Credentials {
	t.Helper()
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	now := time.Now()
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Full mesh normal primary CA"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, ca, ca, caKey.Public(), caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, _ = x509.ParseCertificate(der)
	if len(roots) > 0 {
		roots[0].certificate = ca
		roots[0].key = caKey
	}
	chain := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	principals := []string{"device/22222222-2222-4222-8222-222222222222", "operator/alice", "device/33333333-3333-4333-8333-333333333333"}
	var credentials []*localmesh.Credentials
	for i, p := range principals {
		var key crypto.Signer
		if i == 1 {
			key, err = mldsa.GenerateKey(mldsa.MLDSA65())
		} else {
			key, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		}
		if err != nil {
			t.Fatal(err)
		}
		uri, _ := url.Parse("spiffe://wendy.sh/tenant/11111111-1111-4111-8111-111111111111/" + p)
		leaf := &x509.Certificate{SerialNumber: big.NewInt(int64(i + 2)), Subject: pkix.Name{CommonName: "Full mesh normal peer"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), URIs: []*url.URL{uri}, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth}}
		der, err := x509.CreateCertificate(rand.Reader, leaf, ca, key.Public(), caKey)
		if err != nil {
			t.Fatal(err)
		}
		keyDER, err := x509.MarshalPKCS8PrivateKey(key)
		if err != nil {
			t.Fatal(err)
		}
		certPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
		keyPEM := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}))
		var credential *localmesh.Credentials
		if i == 1 {
			credential, err = localmesh.NewOperatorCredentials(certPEM, chain, keyPEM, uuid.MustParse("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"))
		} else {
			credential, err = localmesh.NewCredentials(certPEM, chain, keyPEM)
		}
		if err != nil {
			t.Fatal(err)
		}
		credentials = append(credentials, credential)
	}
	return credentials
}
func TestFullOperatorRouterMultihopTCPFailover(t *testing.T) {
	var authority fixtureAuthority
	credentials := fixtureCredentials(t, &authority)
	root := authority.certificate
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	var wg sync.WaitGroup
	defer func() { cancel(); wg.Wait() }()
	nodes := make([]*localmesh.Node, 3)
	networks := make([]*Network, 3)
	for i, c := range credentials {
		dir := t.TempDir()
		store, state, err := localmesh.OpenDeviceStateStore(filepath.Join(dir, "routing-state.json"), c.Device, babel.RouterID(0x1000000000001+i))
		if err != nil {
			t.Fatal(err)
		}
		if err = store.Save(state); err != nil {
			t.Fatal(err)
		}
		networks[i], err = NewNetwork(c.Device)
		if err != nil {
			t.Fatal(err)
		}
		port := uint16(50052)
		if i == 1 {
			port = 0
		}
		nodes[i], err = localmesh.NewNodeWithNetwork(ctx, dir, c, "robot", port, networks[i])
		if err != nil {
			t.Fatal(err)
		}
		nodes[i].SetOperatorAgent(func(_ context.Context, conn net.Conn) error { _, err := io.Copy(conn, conn); return err })
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := nodes[i].Run(); err != nil && ctx.Err() == nil {
				t.Errorf("node: %v", err)
			}
		}(i)
	}
	var managementLinks []*meshoperatorlink.RouterLink
	primary, err := meshoperator.VerifyPrimary(credentials[1].Certificate.Certificate, []*x509.Certificate{root}, credentials[1].Device.Tenant(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	link := func(a, b int, cost uint16) context.CancelFunc {
		if credentials[b].Device.IsOperator() {
			a, b = b, a
		}

		linkCtx, stop := context.WithCancel(ctx)
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		serverConfig, err := credentials[b].PeerTLS(credentials[a].Device)
		if err != nil {
			t.Fatal(err)
		}
		serverConfig.NextProtos = []string{meshoperatorlink.RouterALPN}
		clientConfig, err := credentials[a].PeerTLS(credentials[b].Device)
		if err != nil {
			t.Fatal(err)
		}
		clientConfig.NextProtos = []string{meshoperatorlink.RouterALPN}
		ready := make(chan error, 2)
		opened := make(chan *meshoperatorlink.RouterLink, 1)
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer listener.Close()
			raw, err := listener.Accept()
			if err != nil {
				ready <- err
				return
			}
			secure := tls.Server(raw, serverConfig)
			defer secure.Close()
			closeOnCancel := context.AfterFunc(linkCtx, func() { secure.Close() })
			defer closeOnCancel()
			err = secure.HandshakeContext(linkCtx)
			ready <- err
			if err == nil {
				_ = nodes[b].AttachOperatorRouterStream(linkCtx, secure, cost)
			}
		}()
		raw, err := net.Dial("tcp", listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		secure := tls.Client(raw, clientConfig)
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer secure.Close()
			closeOnCancel := context.AfterFunc(linkCtx, func() { secure.Close() })
			defer closeOnCancel()
			err := secure.HandshakeContext(linkCtx)
			ready <- err
			if err == nil {
				cfg := meshoperatorlink.ClientConfig{Identity: primary, Signer: credentials[a].Signer, Instance: meshoperator.InstanceID(credentials[a].Device.UUID()), Router: credentials[b].Device, Roots: []*x509.Certificate{root}}
				router, openErr := meshoperatorlink.RouterClient(linkCtx, secure, cfg)
				if openErr != nil {
					t.Errorf("operator router: %v", openErr)
					return
				}
				opened <- router
				defer func() { _ = router.Close(); router.Wait() }()
				_ = nodes[a].AttachOperatorRouterLink(linkCtx, router, cost)
			}
		}()
		for range 2 {
			if err := <-ready; err != nil {
				t.Fatal(err)
			}
		}
		select {
		case router := <-opened:
			managementLinks = append(managementLinks, router)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		return stop
	}
	stopFast := link(0, 1, 64)
	link(0, 1, 4096)
	link(1, 2, 512)
	waitFor := func(check func() bool) {
		t.Helper()
		ticker := time.NewTicker(30 * time.Millisecond)
		defer ticker.Stop()
		for !check() {
			select {
			case <-ctx.Done():
				t.Fatalf("mesh did not converge: A=%+v B=%+v C=%+v", nodes[0].Snapshot(), nodes[1].Snapshot(), nodes[2].Snapshot())
			case <-ticker.C:
			}
		}
	}
	waitFor(func() bool {
		snapshot := nodes[0].Snapshot()
		for _, r := range snapshot.Routes {
			if r.Prefix.Addr() == credentials[2].Device.MeshAddress() && !r.Unreachable && !r.Local {
				return true
			}
		}
		return false
	})
	listener, err := gonet.ListenTCP(networks[2].stack, tcpip.FullAddress{NIC: 1, Addr: tcpip.AddrFrom16(credentials[2].Device.MeshAddress().As16()), Port: 9000}, ipv6.ProtocolNumber)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	wg.Add(1)
	go func() {
		defer wg.Done()
		stop := context.AfterFunc(ctx, func() { listener.Close() })
		defer stop()
		connection, err := listener.Accept()
		if err != nil {
			return
		}
		defer connection.Close()
		closeOnCancel := context.AfterFunc(ctx, func() { connection.Close() })
		defer closeOnCancel()
		io.Copy(connection, connection)
	}()
	connection, err := networks[0].DialContext(ctx, net.JoinHostPort(credentials[2].Device.MeshAddress().String(), "9000"))
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	roundtrip := func() {
		t.Helper()
		connection.SetDeadline(time.Now().Add(10 * time.Second))
		payload := bytes.Repeat([]byte("operator mesh failover"), 100)
		if _, err := connection.Write(payload); err != nil {
			t.Fatal(err)
		}
		answer := make([]byte, len(payload))
		if _, err := io.ReadFull(connection, answer); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(payload, answer) {
			t.Fatal("corrupted data")
		}
	}
	roundtrip()
	testOperatorTargetAppTLS(t, ctx, networks[1], networks[2], credentials[1], credentials[2], root.Raw)
	testOperatorTargetAgentTLS(t, ctx, networks[1], networks[2], credentials[1], credentials[2], root.Raw)
	management, err := managementLinks[0].OpenAgent(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_ = management.SetDeadline(time.Now().Add(5 * time.Second))
	greeting := []byte("management shares the full-router carrier TLS")
	if _, err = management.Write(greeting); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(greeting))
	if _, err = io.ReadFull(management, got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(greeting, got) {
		t.Fatal("same-TLS management payload changed")
	}
	_ = management.Close()
	stopFast()
	waitFor(func() bool {
		snapshot := nodes[0].Snapshot()
		for _, r := range snapshot.Routes {
			if r.Prefix.Addr() != credentials[2].Device.MeshAddress() || r.Unreachable {
				continue
			}
			for _, l := range snapshot.Links {
				if l.ID == r.Link && l.Cost == 4096 {
					return true
				}
			}
		}
		return false
	})
	roundtrip()
	// All nodes independently validate the operator's own signed origin;
	// the intermediate operator's Babel engine selects both transit directions.
	for i, node := range nodes {
		found := false
		for _, m := range node.Snapshot().Devices {
			if m.Owner != nil && m.Owner.Principal == credentials[1].Device.Principal() && m.Route != nil && m.Route.Prefix.Addr() == credentials[1].Device.MeshAddress() {
				if m.Device != uuid.Nil || m.AgentPort != 0 || m.AgentTransport != "" || m.Platform != "" || len(m.AppEndpoints) != 0 || len(m.Services) != 0 || len(m.Caps) != 0 {
					t.Fatalf("operator origin advertised device services on node %d: %+v", i, m)
				}
				found = true
			}
		}
		if !found {
			t.Fatalf("node %d lacks verified operator manifest", i)
		}
	}
}

// This fixture exercises the production end-to-end TLS/WAS1 implementation,
// rather than treating successful carrier admission or a raw TCP echo as proof.
func testOperatorTargetAppTLS(t *testing.T, ctx context.Context, source, target *Network, operator, device *localmesh.Credentials, root []byte) {
	t.Helper()
	key, err := x509.MarshalPKCS8PrivateKey(device.Signer)
	if err != nil {
		t.Fatal(err)
	}
	issuerPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: root})
	isolated, err := localmesh.NewCredentials(string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: device.Certificate.Certificate[0]})), string(issuerPEM), string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key})))
	if err != nil {
		t.Fatal(err)
	}
	var verified atomic.Bool
	verify := isolated.Verify
	isolated.Verify = func(chain [][]byte, now time.Time) (localmesh.Identity, error) {
		identity, err := verify(chain, now)
		if err == nil && identity.Device.IsOperator() && identity.Device.Principal() == operator.Device.Principal() && len(chain) > 0 && bytes.Equal(chain[0], operator.Certificate.Certificate[0]) {
			verified.Store(true)
		}
		return identity, err
	}
	app, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	go func() {
		for {
			c, e := app.Accept()
			if e != nil {
				return
			}
			go func() { defer c.Close(); _, _ = io.Copy(c, c) }()
		}
	}()
	port := uint16(app.Addr().(*net.TCPAddr).Port)
	auth := &normalAppAuthorizer{port: port, address: app.Addr().String()}
	server, err := meshsession.NewServer(isolated, auth)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	listener, err := target.ListenTCP(meshsession.TLSPort)
	if err != nil {
		t.Fatal(err)
	}
	observed := &observedAppListener{Listener: listener, source: make(chan string, 1)}
	done := make(chan error, 1)
	run, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() { done <- server.ServeTLS(run, observed) }()
	defer func() {
		cancel()
		server.Close()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	flow, err := meshsession.DialTLSWithDialer(ctx, operator, device.Device, netip.AddrPortFrom(target.self, meshsession.TLSPort), port, source.Dial)
	if err != nil {
		t.Fatal("operator end-to-end app TLS/WAS1:", err)
	}
	defer flow.Close()
	flow.SetDeadline(time.Now().Add(5 * time.Second))
	payload := []byte("real ML-DSA primary operator, 5680 source, destination TLS and WAS1 port admission")
	if _, err = flow.Write(payload); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(payload))
	if _, err = io.ReadFull(flow, got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(payload, got) || !verified.Load() || !auth.called.Load() {
		t.Fatal("missing actual operator verification or app authorization")
	}
	select {
	case address := <-observed.source:
		ip, _, err := net.SplitHostPort(address)
		if err != nil || ip != operator.Device.MeshAddress().String() {
			t.Fatal("wrong app source address", address, err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

type normalAppAuthorizer struct {
	port    uint16
	address string
	called  atomic.Bool
}

func (a *normalAppAuthorizer) DialAuthorized(port uint16, dial func(string) (net.Conn, error)) (net.Conn, error) {
	if port != a.port {
		return nil, meshsession.ErrDenied
	}
	a.called.Store(true)
	return dial(a.address)
}

type observedAppListener struct {
	net.Listener
	source chan string
}

func (l *observedAppListener) Accept() (net.Conn, error) {
	c, e := l.Listener.Accept()
	if e == nil {
		select {
		case l.source <- c.RemoteAddr().String():
		default:
		}
	}
	return c, e
}

func testOperatorTargetAgentTLS(t *testing.T, ctx context.Context, source, target *Network, operator, device *localmesh.Credentials, root []byte) {
	t.Helper()
	encode := func(c *localmesh.Credentials) *config.CertificateInfo {
		der, err := x509.MarshalPKCS8PrivateKey(c.Signer)
		if err != nil {
			t.Fatal(err)
		}
		return &config.CertificateInfo{PemCertificate: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.Certificate.Certificate[0]})), PemCertificateChain: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: root})), PemPrivateKey: string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})), PrincipalURI: c.Device.Principal()}
	}
	serving := encode(device)
	server, err := mtls.NewServer(serving.PemCertificate, serving.PemCertificateChain, serving.PemPrivateKey, zap.NewNop(), time.Time{}, certs.Scope{TenantUUID: device.Device.Tenant().String()}, interceptor.OrgModeStrict)
	if err != nil {
		t.Fatal(err)
	}
	service := &normalOperatorAgent{principal: operator.Device.Principal(), address: operator.Device.MeshAddress().String()}
	agentpb.RegisterWendyAgentServiceServer(server, service)
	listener, err := target.ListenTCP(50052)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	defer func() { server.Stop(); <-done }()
	want, err := certs.ParseDevicePrincipal(device.Device.Principal())
	if err != nil {
		t.Fatal(err)
	}
	connection, err := grpcclient.ConnectWithTLSExpecting(ctx, net.JoinHostPort(target.self.String(), "50052"), encode(operator), nil, &want, grpc.WithContextDialer(func(ctx context.Context, address string) (net.Conn, error) { return source.DialContext(ctx, address) }))
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	reply, err := connection.AgentService.GetAgentVersion(ctx, &agentpb.GetAgentVersionRequest{})
	if err != nil {
		t.Fatal("operator end-to-end agent RPC:", err)
	}
	if reply.Version != "full-operator-router" || !service.called.Load() {
		t.Fatal("actual agent RPC did not preserve operator")
	}
}

type normalOperatorAgent struct {
	agentpb.UnimplementedWendyAgentServiceServer
	principal, address string
	called             atomic.Bool
}

func (s *normalOperatorAgent) GetAgentVersion(ctx context.Context, _ *agentpb.GetAgentVersionRequest) (*agentpb.GetAgentVersionResponse, error) {
	p, ok := peer.FromContext(ctx)
	if !ok {
		return nil, fmt.Errorf("missing real RPC peer")
	}
	info, ok := p.AuthInfo.(grpccredentials.TLSInfo)
	if !ok || len(info.State.PeerCertificates) == 0 {
		return nil, fmt.Errorf("missing actual target TLS")
	}
	principal, ok := certs.TenantPrincipalFromCert(info.State.PeerCertificates[0])
	if !ok || principal != s.principal {
		return nil, fmt.Errorf("wrong primary operator principal")
	}
	address, _, err := net.SplitHostPort(p.Addr.String())
	if err != nil || address != s.address {
		return nil, fmt.Errorf("wrong operator source: %v", p.Addr)
	}
	s.called.Store(true)
	return &agentpb.GetAgentVersionResponse{Version: "full-operator-router"}, nil
}
