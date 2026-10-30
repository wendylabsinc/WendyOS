//go:build !windows

package sessionbroker

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/mldsa"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/interceptor"
	"github.com/wendylabsinc/wendy/go/internal/agent/mtls"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/cli/netmesh/carriers"
	"github.com/wendylabsinc/wendy/go/internal/cli/operatormesh"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/discovery"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshalpn"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshoperator"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshoperatorlink"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
)

// The router's packet path is idle during this RPC/lease test, like an idle
// kernel TUN. The broker owns the one real portable userspace endpoint.
type operatorBrokerRouterPackets struct{}

func (operatorBrokerRouterPackets) ReadPacket(ctx context.Context) ([]byte, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}
func (operatorBrokerRouterPackets) Inject([]byte) error { return nil }

type operatorBrokerAgent struct {
	agentpb.UnimplementedWendyAgentServiceServer
	principal string
	calls     atomic.Int32
}

func (s *operatorBrokerAgent) GetAgentVersion(ctx context.Context, _ *agentpb.GetAgentVersionRequest) (*agentpb.GetAgentVersionResponse, error) {
	p, ok := peer.FromContext(ctx)
	if !ok {
		return nil, fmt.Errorf("missing original TLS peer")
	}
	info, ok := p.AuthInfo.(credentials.TLSInfo)
	if !ok || info.State.NegotiatedProtocol != meshoperatorlink.RouterALPN || len(info.State.PeerCertificates) == 0 {
		return nil, fmt.Errorf("RPC did not reuse attachment TLS")
	}
	principal, ok := certs.TenantPrincipalFromCert(info.State.PeerCertificates[0])
	if !ok || principal != s.principal {
		return nil, fmt.Errorf("operator role was not preserved")
	}
	s.calls.Add(1)
	return &agentpb.GetAgentVersionResponse{Version: "retained-operator", Hostname: "router.fixture"}, nil
}
func TestOperatorBrokerNormalConcurrentInvocationsRenewAndIdle(t *testing.T) {
	for _, algorithm := range []string{"P256", "MLDSA65", "P256-process"} {
		t.Run(algorithm, func(t *testing.T) { operatorBrokerNormal(t, algorithm, false) })
	}
}
func TestOperatorBrokerNormalLongOpaquePrincipal(t *testing.T) {
	for _, mode := range []string{"P256-long", "P256-long-escaped"} {
		t.Run(mode, func(t *testing.T) { operatorBrokerNormal(t, mode, false) })
	}
}
func TestOperatorBrokerEnableAutomaticKeepsExistingOwner(t *testing.T) {
	operatorBrokerNormal(t, "P256-enable", false)
}
func TestOperatorBrokerNormalAutomaticLANAttachment(t *testing.T) {
	operatorBrokerNormal(t, "P256-auto", false)
}
func TestOperatorBrokerTransitSurvivesLastCommandLease(t *testing.T) {
	operatorBrokerNormal(t, "P256-transit", false)
}

func TestOperatorBrokerNormalReconnectRetainsIPCAndEndpoint(t *testing.T) {
	operatorBrokerNormal(t, "P256-reconnect", false)
}
func TestOperatorBrokerNormalAutomaticAddressHandover(t *testing.T) {
	operatorBrokerNormal(t, "P256-roam", false)
}
func TestOperatorBrokerNormalColdNearbyPreparation(t *testing.T) {
	operatorBrokerNormal(t, "P256-nearby", false)
}
func TestOperatorBrokerNormalColdDiscoveryKeepsFirstRecipeAcrossPaths(t *testing.T) {
	operatorBrokerNormal(t, "P256-nearby-paths", false)
}
func TestEmbeddedOperatorBrokerDisconnectJoins(t *testing.T) {
	operatorBrokerNormal(t, "P256-embedded", true)
}

func TestOperatorBrokerNormalExplicitDisconnectJoins(t *testing.T) {
	operatorBrokerNormal(t, "P256", true)
}
func TestOperatorBrokerNormalCredentialRenewalRejoins(t *testing.T) {
	operatorBrokerNormal(t, "P256-renew", false)
}
func operatorBrokerNormal(t *testing.T, algorithm string, disconnect bool) {
	var extraSources []carriers.Source
	previousOpen := openOperatorSession
	t.Cleanup(func() { openOperatorSession = previousOpen })
	openOperatorSession = func(ctx context.Context, options operatormesh.Options) (*operatormesh.Session, error) {
		options.CarrierSources = append([]carriers.Source{}, extraSources...)
		return operatormesh.Open(ctx, options)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	now := time.Now()
	rootKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "operator broker positive root"}, IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour)}
	rootDER, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, rootKey.Public(), rootKey)
	if err != nil {
		t.Fatal(err)
	}
	root, err := x509.ParseCertificate(rootDER)
	if err != nil {
		t.Fatal(err)
	}
	tenant, _ := meshid.ParseTenant("11111111-1111-4111-8111-111111111111")
	router, _ := meshid.ParseDevice(tenant, "22222222-2222-4222-8222-222222222222")
	principal := "spiffe://wendy.sh/tenant/" + tenant.String() + "/operator/alice%7Cbroker"
	if strings.HasPrefix(algorithm, "P256-long") {
		prefix := "spiffe://wendy.sh/tenant/" + tenant.String() + "/operator/"
		subjectByte := "s"
		if strings.HasSuffix(algorithm, "escaped") {
			subjectByte = "&"
		}
		principal = prefix + strings.Repeat(subjectByte, meshoperator.MaxPrincipalBytes-len(prefix))
	}
	issuer, issuerKey := root, rootKey
	issuerChain := [][]byte{rootDER}
	if algorithm == "P256-auto" {
		// Current native listeners can omit shared issuers during TLS. Router
		// admission still verifies the wire chain, so automatic attachments
		// must retain the intermediate rather than negotiate its omission.
		issuerKey, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		intermediate := *tmpl
		intermediate.SerialNumber = big.NewInt(20)
		intermediate.Subject = pkix.Name{CommonName: "operator broker intermediate"}
		der, e := x509.CreateCertificate(rand.Reader, &intermediate, root, issuerKey.Public(), rootKey)
		if e != nil {
			t.Fatal(e)
		}
		issuer, err = x509.ParseCertificate(der)
		if err != nil {
			t.Fatal(err)
		}
		issuerChain = [][]byte{der, rootDER}
	}
	issue := func(serial int64, principal string, key crypto.Signer, usage x509.ExtKeyUsage) (tls.Certificate, string, string) {
		uri, _ := url.Parse(principal)
		leaf := &x509.Certificate{SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: "operator broker normal leaf"}, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour), URIs: []*url.URL{uri}, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{usage}}
		if strings.Contains(principal, "/device/") {
			leaf.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth}
		}
		der, err := x509.CreateCertificate(rand.Reader, leaf, issuer, key.Public(), issuerKey)
		if err != nil {
			t.Fatal(err)
		}
		pk, err := x509.MarshalPKCS8PrivateKey(key)
		if err != nil {
			t.Fatal(err)
		}
		return tls.Certificate{Certificate: append([][]byte{der}, issuerChain...), PrivateKey: key}, string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pk}))
	}
	var operatorKey crypto.Signer
	if algorithm == "MLDSA65" {
		operatorKey, err = mldsa.GenerateKey(mldsa.MLDSA65())
	} else {
		operatorKey, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	}
	if err != nil {
		t.Fatal(err)
	}
	_, operatorPEM, operatorPrivate := issue(2, principal, operatorKey, x509.ExtKeyUsageClientAuth)
	routerKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	presentation, routerPEM, routerPrivate := issue(3, router.Principal(), routerKey, x509.ExtKeyUsageServerAuth)
	rootPEM := ""
	for _, der := range issuerChain {
		rootPEM += string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	}
	certificate := config.CertificateInfo{PemCertificate: operatorPEM, PemCertificateChain: rootPEM, PemPrivateKey: operatorPrivate, PrincipalURI: principal}
	directory := shortTempDir(t)
	if algorithm == "P256-process" {
		t.Setenv("WENDY_CONFIG_DIR", directory)
	}
	oldDir, oldLoad, oldLaunch := configDir, loadConfig, launchOperatorBroker
	t.Cleanup(func() { configDir, loadConfig, launchOperatorBroker = oldDir, oldLoad, oldLaunch })
	configDir = func() (string, error) { return directory, nil }
	cfg := &config.Config{CurrentContext: "operator", Auth: []config.AuthConfig{{Name: "operator", CloudGRPC: "fixture.invalid:443", Certificates: []config.CertificateInfo{certificate}}}}
	if algorithm == "P256-process" {
		if err := config.Save(cfg); err != nil {
			t.Fatal(err)
		}
	}
	loadConfig = func() (*config.Config, error) {
		copy := *cfg
		copy.Auth = append([]config.AuthConfig(nil), cfg.Auth...)
		return &copy, nil
	}
	ingress := meshoperatorlink.NewAgentListener()
	defer ingress.Close()
	server, err := mtls.NewServer(routerPEM, rootPEM, routerPrivate, zap.NewNop(), time.Time{}, certs.Scope{TenantUUID: tenant.String()}, interceptor.OrgModeStrict)
	if err != nil {
		t.Fatal(err)
	}
	service := &operatorBrokerAgent{principal: principal}
	agentpb.RegisterWendyAgentServiceServer(server, service)
	serverDone := make(chan struct{})
	go func() { defer close(serverDone); _ = server.Serve(ingress) }()
	defer func() { server.Stop(); <-serverDone }()
	socket, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer socket.Close()
	listeners := []net.Listener{socket}
	var useAlternate atomic.Bool
	var browses atomic.Int32
	if algorithm == "P256-roam" || algorithm == "P256-nearby-paths" {
		alternate, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer alternate.Close()
		listeners = append(listeners, alternate)
	}
	var transit *brokerTransitFixture
	if algorithm == "P256-transit" {
		transit = newBrokerTransitFixture(t, ctx, tenant, rootPEM, issue)
		extraSources = []carriers.Source{transit.source}
	}
	var handshakes, admitted, withdrawn atomic.Int32
	carrierSockets := make(chan net.Conn, 8)
	proofs := make(chan meshid.Device, 8)
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS13, MaxVersion: tls.VersionTLS13, Certificates: []tls.Certificate{presentation}, ClientAuth: tls.RequireAnyClientCert, NextProtos: []string{meshoperatorlink.RouterALPN}, GetConfigForClient: func(*tls.ClientHelloInfo) (*tls.Config, error) { handshakes.Add(1); return nil, nil }}
	if algorithm == "P256-auto" {
		meshalpn.NewCertificates(presentation, []*x509.Certificate{root}).ConfigureServer(tlsConfig)
	}
	var routes sync.WaitGroup
	acceptDone := make(chan struct{})
	serveListener := func(listener net.Listener) {
		for {
			raw, err := listener.Accept()
			if err != nil {
				return
			}
			routes.Add(1)
			go func() {
				defer routes.Done()
				secure := tls.Server(raw, tlsConfig)
				if secure.HandshakeContext(ctx) != nil {
					raw.Close()
					return
				}
				carrierSockets <- raw
				link, err := meshoperatorlink.AcceptRouter(ctx, secure, tenant, []*x509.Certificate{root}, ingress.Deliver)
				if err != nil {
					raw.Close()
					return
				}
				admitted.Add(1)
				proofs <- link.Peer()
				defer func() { _ = link.Close(); link.Wait(); withdrawn.Add(1) }()
				select {
				case <-ctx.Done():
				case <-link.Done():
				}
			}()
		}
	}
	go func() {
		defer close(acceptDone)
		var accepting sync.WaitGroup
		for _, listener := range listeners {
			accepting.Add(1)
			go func() { defer accepting.Done(); serveListener(listener) }()
		}
		accepting.Wait()
	}()
	t.Cleanup(func() {
		cancel()
		for _, listener := range listeners {
			listener.Close()
		}
		<-acceptDone
		routes.Wait()
	})
	// Run the exact production entrypoint, also in a real detached subprocess.
	// Independent callers retain separate local IPC channels and parent EOF leases.
	var starts atomic.Int32
	var helpers sync.WaitGroup
	var leaseMu sync.Mutex
	var leases []*os.File
	if algorithm == "P256-embedded" {
		ResumeEmbedded()
		t.Cleanup(func() { SuspendEmbedded(); ResumeEmbedded() })
	}
	launchOperatorBroker = func(spec Spec) error {
		starts.Add(1)
		if algorithm == "P256-embedded" {
			return startEmbedded(spec)
		}
		b, _ := json.Marshal(spec)
		r, w, err := os.Pipe()
		if err != nil {
			return err
		}
		leaseMu.Lock()
		leases = append(leases, w)
		leaseMu.Unlock()
		if algorithm == "P256-process" {
			exe, err := os.Executable()
			if err != nil {
				return err
			}
			cmd := exec.CommandContext(ctx, exe, "-test.run=^TestOperatorBrokerChildProcess$")
			cmd.Env = append(os.Environ(), "WENDY_OPERATOR_BROKER_CHILD="+base64.RawURLEncoding.EncodeToString(b))
			cmd.ExtraFiles = []*os.File{r}
			cmd.SysProcAttr = detachedProcessAttributes()
			if err = cmd.Start(); err != nil {
				r.Close()
				w.Close()
				return err
			}
			r.Close()
			helpers.Add(1)
			go func() {
				defer helpers.Done()
				if err := cmd.Wait(); err != nil && ctx.Err() == nil {
					t.Errorf("operator helper process: %v", err)
				}
			}()
			return nil
		}
		helpers.Add(1)
		go func() {
			defer helpers.Done()
			_ = Run(ctx, base64.RawURLEncoding.EncodeToString(b), time.Second, r)
		}()
		return nil
	}
	t.Cleanup(func() {
		cancel()
		leaseMu.Lock()
		for _, w := range leases {
			w.Close()
		}
		leaseMu.Unlock()
		helpers.Wait()
	})
	if algorithm == "P256-enable" || algorithm == "P256-auto" || algorithm == "P256-roam" || algorithm == "P256-nearby" || algorithm == "P256-nearby-paths" {
		previousBrowse := browseOperatorLAN
		t.Cleanup(func() { browseOperatorLAN = previousBrowse })
		browseOperatorLAN = func(browse context.Context, service string) (<-chan discovery.MDNSService, error) {
			count := browses.Add(1)
			selected := listeners[0]
			if algorithm == "P256-nearby-paths" && count%2 == 0 {
				selected = listeners[1]
			}
			if useAlternate.Load() {
				selected = listeners[1]
			}
			host, port, _ := net.SplitHostPort(selected.Addr().String())
			portNumber, _ := strconv.Atoi(port)
			ch := make(chan discovery.MDNSService, 1)
			ch <- discovery.MDNSService{IPAddress: host, Port: portNumber, TXTRecords: map[string]string{"platform": "WendyOS", "version": "1", "protocol": "tls-v1", "role": "router", "principal": router.Principal()}}
			close(ch)
			return ch, nil
		}
	}
	if view, err := ReadOperatorTopology(ctx); view != nil || !errors.Is(err, ErrUnavailable) || starts.Load() != 0 {
		t.Fatal("read-only topology started an operator context", view, err, starts.Load())
	}
	clients := make(chan *grpcclient.AgentConnection, 3)
	failures := make(chan error, 3)
	for range 3 {
		go func() {
			var client *grpcclient.AgentConnection
			var err error
			if algorithm == "P256-nearby" || algorithm == "P256-nearby-paths" {
				client, err = prepareOperatorDirectory(ctx)
			} else if algorithm == "P256-enable" || algorithm == "P256-auto" || algorithm == "P256-roam" {
				client, err = ConnectOperator(ctx, MeshSelector(router))
			} else {
				client, err = PrepareOperator(ctx, certificate, socket.Addr().String(), router.UUID().String())
			}
			if err != nil {
				failures <- err
			} else {
				clients <- client
			}
		}()
	}
	var connected []*grpcclient.AgentConnection
	for range 3 {
		select {
		case err := <-failures:
			t.Fatal(err)
		case client := <-clients:
			connected = append(connected, client)
		case <-ctx.Done():
			t.Fatalf("initial proxy wait: %v TLS=%d admissions=%d RPCs=%d launches=%d", ctx.Err(), handshakes.Load(), admitted.Load(), service.calls.Load(), starts.Load())
		}
	}
	for _, c := range connected {
		expectedHost := MeshSelector(router)
		if algorithm == "P256-nearby" || algorithm == "P256-nearby-paths" {
			expectedHost = principal
		}
		if c.Host != expectedHost || !c.IsSessionProxy {
			t.Fatal("normal selector did not select typed proxy")
		}
		c.Close()
	}
	// The original launching process and even its fixture parent pipe stay
	// alive; only open user IPC connections may hold this operator context.
	warm, err := ConnectOperator(ctx, MeshSelector(router))
	if err != nil {
		t.Fatal(err)
	}
	view, err := ReadOperatorTopology(ctx)
	if err != nil || view.SelfPrincipal != principal || view.SelfDeviceId == router.UUID().String() || !strings.HasPrefix(view.SelfMeshAddress, "fd45:b891:5680:") {
		t.Fatalf("local full-router topology: %v %v", view, err)
	}
	// Quiet local work plus the full node's timers must keep a live user channel, and
	// must not itself reset idle time after the final user channel closes.
	time.Sleep(1200 * time.Millisecond)
	if _, err = warm.AgentService.GetAgentVersion(ctx, &agentpb.GetAgentVersionRequest{}); err != nil {
		t.Fatal(err)
	}
	if algorithm == "P256-enable" {
		enabled, e := EnableOperatorAutomatic(ctx, certificate)
		if e != nil {
			t.Fatal(e)
		}
		enabled.Close()
	}
	if handshakes.Load() != 1 || admitted.Load() != 1 || starts.Load() != 1 {
		t.Fatalf("warm context was not retained: TLS=%d admit=%d launches=%d", handshakes.Load(), admitted.Load(), starts.Load())
	}
	if algorithm == "P256-nearby-paths" && browses.Load() < 1 {
		t.Fatal("cold callers did not discover their retained recipe", browses.Load())
	}
	pin, err := os.ReadFile(filepath.Join(directory, "known_devices.json"))
	if err != nil || len(pin) == 0 {
		t.Fatalf("profile SPKI pin not retained: %v", err)
	}
	expectedWithdrawals := int32(1)
	if algorithm == "P256-reconnect" || algorithm == "P256-roam" {
		original := <-proofs
		firstSocket := <-carrierSockets
		if algorithm == "P256-roam" {
			useAlternate.Store(true)
			socket.Close() // The old address no longer accepts connections.
		}
		firstSocket.Close() // Ordinary peer restart / carrier loss.
		// The local context probe must not depend on the original peer still
		// answering an agent RPC; it remains usable during recovery itself.
		statusUntil := time.Now().Add(800 * time.Millisecond)
		for {
			request, reply := rawMessage{}, rawMessage{}
			if err := warm.Conn.Invoke(ctx, operatorContextMethod, &request, &reply, grpc.ForceCodec(rawCodec{})); err != nil {
				t.Fatal("local context probe died with its first hop", err)
			}
			var context operatorContextStatus
			if json.Unmarshal(reply, &context) == nil && context.State == "reconnecting" {
				break
			}
			if time.Now().After(statusUntil) {
				t.Fatal("context did not expose reconnecting state")
			}
			time.Sleep(time.Millisecond)
		}
		control, err := connectedOperatorControl(ctx)
		if err != nil {
			t.Fatal("could not reuse local broker during carrier outage", err)
		}
		control.Close()
		var replacement meshid.Device
		select {
		case replacement = <-proofs:
		case <-time.After(8 * time.Second):
			t.Fatal("retained context did not reconnect")
		}
		if replacement != original {
			t.Fatal("reconnect changed the operator installation identity", original, replacement)
		}
		// Reuse this same still-open local client, rather than resolving a new
		// broker. Its next call resolves the new attachment generation.
		until := time.Now().Add(5 * time.Second)
		for {
			probe, stop := context.WithTimeout(ctx, time.Second)
			_, err = warm.AgentService.GetAgentVersion(probe, &agentpb.GetAgentVersionRequest{})
			stop()
			if err == nil {
				break
			}
			if time.Now().After(until) {
				t.Fatal("same IPC client did not recover", err)
			}
			time.Sleep(20 * time.Millisecond)
		}
		if starts.Load() != 1 || handshakes.Load() != 2 || admitted.Load() != 2 {
			t.Fatal("reconnect replaced broker instead of retaining IPC", starts.Load(), handshakes.Load(), admitted.Load())
		}
		expectedWithdrawals = 2
	}
	if algorithm == "P256-renew" {
		// Normal issuance renews the same canonical operator without changing the
		// installation's endpoint identity or silently retaining an old credential.
		_, renewedPEM, renewedPrivate := issue(4, principal, operatorKey, x509.ExtKeyUsageClientAuth)
		fresh := certificate
		fresh.PemCertificate = renewedPEM
		fresh.PemPrivateKey = renewedPrivate
		copyConfig := *cfg
		copyConfig.Auth = append([]config.AuthConfig(nil), cfg.Auth...)
		copyConfig.Auth[0].Certificates = []config.CertificateInfo{fresh}
		cfg = &copyConfig
		next, err := ConnectOperator(ctx, MeshSelector(router))
		if err != nil {
			t.Fatal(err)
		}
		if _, err = next.AgentService.GetAgentVersion(ctx, &agentpb.GetAgentVersionRequest{}); err != nil {
			t.Fatal(err)
		}
		warm.Close()
		warm = next
		expectedWithdrawals = 2
		if handshakes.Load() != 2 || admitted.Load() != 2 || starts.Load() != 2 || withdrawn.Load() != 1 {
			t.Fatal("credential renewal did not drain then rejoin exact context", handshakes.Load(), admitted.Load(), starts.Load(), withdrawn.Load())
		}
	}
	if disconnect {
		if err := DisconnectOperator(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if transit != nil {
		transit.roundTrip(t)
	}
	warm.Close()
	if !disconnect && !strings.Contains(algorithm, "auto") && !strings.Contains(algorithm, "roam") && !strings.Contains(algorithm, "nearby") {
		time.Sleep(1200 * time.Millisecond)
		// Both remote routers still use this actual broker Node as their only
		// path, despite every command IPC channel having been closed.
		if transit != nil {
			transit.roundTrip(t)
		}
		retained, err := connectedOperatorControl(ctx)
		if err != nil {
			t.Fatal("enabled full router expired with no local command lease", err)
		}
		retained.Close()
		if err := DisconnectOperator(ctx); err != nil {
			t.Fatal("explicit full-router disconnect", err)
		}
	}
	if transit != nil {
		transit.detached(t)
	}
	complete := make(chan struct{})
	go func() { helpers.Wait(); close(complete) }()
	select {
	case <-complete:
	case <-time.After(5 * time.Second):
		t.Fatal("full-node work retained an idle broker")
	}
	until := time.After(5 * time.Second)
	for withdrawn.Load() != expectedWithdrawals {
		select {
		case <-until:
			t.Fatalf("server route withdrawal count=%d want%d", withdrawn.Load(), expectedWithdrawals)
		case <-time.After(10 * time.Millisecond):
		}
	}

}

// Executed only by the ordinary positive subprocess fixture. Real recipe and
// synthetic credentials are read from its isolated private test profile.
func TestOperatorBrokerChildProcess(t *testing.T) {
	openOperatorSession = func(ctx context.Context, options operatormesh.Options) (*operatormesh.Session, error) {
		options.CarrierSources = []carriers.Source{}
		return operatormesh.Open(ctx, options)
	}

	encoded := os.Getenv("WENDY_OPERATOR_BROKER_CHILD")
	if encoded == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := Run(ctx, encoded, time.Second, os.NewFile(3, "parent-lease")); err != nil {
		t.Fatal(err)
	}
}
