//go:build linux

package sessionbroker

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/mldsa"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/interceptor"
	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/agent/mtls"
	"github.com/wendylabsinc/wendy/go/internal/agent/services"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/cli/netmesh/carriers"
	"github.com/wendylabsinc/wendy/go/internal/cli/operatormesh"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/devicepin"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshoperatorlink"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"go.uber.org/zap"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
)

type remoteKernelConfig struct {
	Identity                    localmesh.TCPIdentity
	Dir, Principal, Ready, Stop string
	Remote                      bool
}
type remoteKernelAgent struct {
	agentpb.UnimplementedWendyAgentServiceServer
	principal, device string
	remote            bool
	calls             atomic.Int32
}

func (s *remoteKernelAgent) GetAgentVersion(ctx context.Context, _ *agentpb.GetAgentVersionRequest) (*agentpb.GetAgentVersionResponse, error) {
	p, ok := peer.FromContext(ctx)
	if !ok {
		return nil, fmt.Errorf("missing peer")
	}
	info, ok := p.AuthInfo.(credentials.TLSInfo)
	if !ok || len(info.State.PeerCertificates) == 0 {
		return nil, fmt.Errorf("missing TLS identity")
	}
	principal, ok := certs.TenantPrincipalFromCert(info.State.PeerCertificates[0])
	if !ok || principal != s.principal {
		return nil, fmt.Errorf("operator identity changed")
	}
	if metadata.ValueFromIncomingContext(ctx, operatorTargetHeader) != nil {
		return nil, fmt.Errorf("local routing metadata escaped broker")
	}
	if s.remote {
		host, _, _ := net.SplitHostPort(p.Addr.String())
		if !strings.HasPrefix(host, "fd45:b891:5680:") {
			return nil, fmt.Errorf("source is not real operator5680: %s", host)
		}
		if info.State.NegotiatedProtocol != "h2" {
			return nil, fmt.Errorf("remote agent TLS ALPN %q", info.State.NegotiatedProtocol)
		}
	} else if info.State.NegotiatedProtocol != meshoperatorlink.RouterALPN {
		return nil, fmt.Errorf("adjacent RPC added TLS")
	}
	s.calls.Add(1)
	return &agentpb.GetAgentVersionResponse{Version: s.device, Hostname: strings.Repeat("remote64", 8192)}, nil
}
func TestOperatorRemoteKernelProcess(t *testing.T) {
	path := os.Getenv("WENDY_OPERATOR_REMOTE_CHILD")
	if path == "" {
		t.Skip("namespace child")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cfg remoteKernelConfig
	if err = json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 65*time.Second)
	defer cancel()
	var node *localmesh.Node
	var lock sync.Mutex
	var serverStop func()
	done := make(chan error, 1)
	go func() {
		done <- localmesh.RunConfiguredWithNode(ctx, cfg.Dir, cfg.Identity, func(current *localmesh.Node, _ *localmesh.TCPConfig) {
			if current == nil {
				if serverStop != nil {
					serverStop()
				}
				return
			}
			lock.Lock()
			node = current
			lock.Unlock()
			self := current.Credentials.Device
			server, e := mtls.NewServer(cfg.Identity.Certificate, cfg.Identity.Chain, cfg.Identity.Key, zap.NewNop(), time.Time{}, certs.Scope{TenantUUID: self.Tenant().String()}, interceptor.OrgModeStrict)
			if e != nil {
				t.Error(e)
				cancel()
				return
			}
			agentpb.RegisterWendyAgentServiceServer(server, &remoteKernelAgent{principal: cfg.Principal, device: self.UUID().String(), remote: cfg.Remote})
			admin := services.NewLocalMeshAdminService(zap.NewNop(), cfg.Dir, func() (meshid.Tenant, meshid.Device) { return self.Tenant(), self })
			admin.SetTopologySource(current.Snapshot)
			pb.RegisterWendyLocalMeshAdminServiceServer(server, admin)
			ingress := meshoperatorlink.NewAgentListener()
			current.SetOperatorAgent(ingress.Deliver)
			listener, e := net.Listen("tcp6", net.JoinHostPort(self.MeshAddress().String(), "50052"))
			if e != nil {
				t.Error(e)
				cancel()
				return
			}
			go server.Serve(listener)
			go server.Serve(ingress)
			serverStop = func() { server.Stop(); listener.Close(); ingress.Close() }
			if e = os.WriteFile(cfg.Ready, []byte("ready"), 0600); e != nil {
				t.Error(e)
				cancel()
			}
		})
	}()
	defer cancel()
	for {
		if _, err = os.Stat(cfg.Stop); err == nil {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("router exited: %v", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(50 * time.Millisecond):
		}
	}
	lock.Lock()
	snapshot := node.Snapshot()
	lock.Unlock()
	encoded, _ := json.Marshal(snapshot)
	_ = os.WriteFile(filepath.Join(cfg.Dir, "final-snapshot.json"), encoded, 0600)
	cancel()
	if err = <-done; err != nil {
		t.Fatal(err)
	}
}
func remoteKernelCredentials(t *testing.T) (config.CertificateInfo, [2]localmesh.TCPIdentity, [2]meshid.Device) {
	t.Helper()
	now := time.Now()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	root := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "remote operator positive root"}, IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour)}
	rootDER, err := x509.CreateCertificate(rand.Reader, root, root, caKey.Public(), caKey)
	if err != nil {
		t.Fatal(err)
	}
	root, err = x509.ParseCertificate(rootDER)
	if err != nil {
		t.Fatal(err)
	}
	encode := func(kind string, der []byte) string {
		return string(pem.EncodeToMemory(&pem.Block{Type: kind, Bytes: der}))
	}
	chain := encode("CERTIFICATE", rootDER)
	issue := func(serial int64, principal string, key crypto.Signer) (string, string) {
		uri, _ := url.Parse(principal)
		leaf := &x509.Certificate{SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: "positive peer"}, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour), URIs: []*url.URL{uri}, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}}
		der, e := x509.CreateCertificate(rand.Reader, leaf, root, key.Public(), caKey)
		if e != nil {
			t.Fatal(e)
		}
		pk, e := x509.MarshalPKCS8PrivateKey(key)
		if e != nil {
			t.Fatal(e)
		}
		return encode("CERTIFICATE", der), encode("PRIVATE KEY", pk)
	}
	tenant, _ := meshid.ParseTenant("11111111-1111-4111-8111-111111111111")
	var ids [2]meshid.Device
	var identities [2]localmesh.TCPIdentity
	for i, id := range []string{"22222222-2222-4222-8222-222222222222", "33333333-3333-4333-8333-333333333333"} {
		ids[i], _ = meshid.ParseDevice(tenant, id)
		key, e := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if e != nil {
			t.Fatal(e)
		}
		cert, pk := issue(int64(i+2), ids[i].Principal(), key)
		identities[i] = localmesh.TCPIdentity{Name: "operator-remote-kernel", AgentPort: 50052, Certificate: cert, Chain: chain, Key: pk}
	}
	principal := "spiffe://wendy.sh/tenant/" + tenant.String() + "/operator/auth0%7Cremote"
	key, err := mldsa.GenerateKey(mldsa.MLDSA65())
	if err != nil {
		t.Fatal(err)
	}
	cert, pk := issue(5, principal, key)
	return config.CertificateInfo{PemCertificate: cert, PemCertificateChain: chain, PemPrivateKey: pk, PrincipalURI: principal}, identities, ids
}
func TestOperatorRemoteBrokerTwoKernelRouters(t *testing.T) {
	operatorRemoteBrokerTwoKernelRouters(t, false)
}
func TestOperatorRemoteBrokerTwoKernelRoutersNormalContextRestart(t *testing.T) {
	operatorRemoteBrokerTwoKernelRouters(t, true)
}
func operatorRemoteBrokerTwoKernelRouters(t *testing.T, reconnect bool) {
	if os.Getenv("WENDY_LOCALMESH_ISOLATED_TEST") != "1" {
		t.Skip("disposable privileged container only")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 55*time.Second)
	defer cancel()
	run := func(name string, args ...string) {
		t.Helper()
		out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
		if err != nil {
			t.Fatalf("%s %v: %v %s", name, args, err, out)
		}
	}
	names := [2]string{fmt.Sprintf("wora%d", os.Getpid()), fmt.Sprintf("worb%d", os.Getpid())}
	for _, ns := range names {
		run("ip", "netns", "add", ns)
		defer exec.Command("ip", "netns", "del", ns).Run()
		run("ip", "-n", ns, "link", "set", "lo", "up")
		run("ip", "netns", "exec", ns, "sysctl", "-w", "net.ipv6.conf.all.forwarding=0")
	}
	run("ip", "link", "add", "worp", "type", "veth", "peer", "name", "wora")
	defer exec.Command("ip", "link", "del", "worp").Run()
	run("ip", "link", "set", "wora", "netns", names[0])
	run("ip", "addr", "add", "10.221.0.1/30", "dev", "worp")
	run("ip", "link", "set", "worp", "up")
	run("ip", "-n", names[0], "addr", "add", "10.221.0.2/30", "dev", "wora")
	run("ip", "-n", names[0], "link", "set", "wora", "up")
	run("ip", "link", "add", "worab", "type", "veth", "peer", "name", "worba")
	for i, iface := range []string{"worab", "worba"} {
		run("ip", "link", "set", iface, "netns", names[i])
		run("ip", "-n", names[i], "addr", "add", fmt.Sprintf("10.222.0.%d/30", i+1), "dev", iface)
		run("ip", "-n", names[i], "link", "set", iface, "up")
	}
	certificate, identities, devices := remoteKernelCredentials(t)
	directory := shortTempDir(t)
	var commands []*exec.Cmd
	var childDone []chan error
	var joined []bool
	var outputs []*bytes.Buffer
	var childCfg [2]remoteKernelConfig
	defer func() {
		for i, cfg := range childCfg {
			if i < len(commands) && !joined[i] {
				_ = os.WriteFile(cfg.Stop, []byte("stop"), 0600)
			}
		}
		for i, done := range childDone {
			if !joined[i] {
				select {
				case err := <-done:
					if err != nil {
						t.Logf("cleanup child%d: %v\n%s", i, err, outputs[i].String())
					}
				case <-time.After(5 * time.Second):
					cancel()
					<-done
					t.Logf("cleanup child%d timed out", i)
				}
			}
		}
		cancel()
	}()
	for i := range identities {
		dir := filepath.Join(directory, fmt.Sprint(i))
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		write := func(path string, value any) {
			t.Helper()
			data, err := json.Marshal(value)
			if err == nil {
				err = os.WriteFile(path, data, 0600)
			}
			if err != nil {
				t.Fatal(err)
			}
		}
		endpoint := ""
		if i == 0 {
			endpoint = "10.221.0.2:43024"
		}
		write(filepath.Join(dir, "local-mesh.json"), localmesh.TCPConfig{Listen: fmt.Sprintf("10.222.0.%d:43020", i+1), EndpointListen: endpoint, Peers: []localmesh.TCPPeer{{Device: devices[1-i].UUID().String(), Address: fmt.Sprintf("10.222.0.%d:43020", 2-i)}}})
		cfg := remoteKernelConfig{Identity: identities[i], Dir: dir, Principal: certificate.PrincipalURI, Ready: filepath.Join(dir, "ready"), Stop: filepath.Join(dir, "stop"), Remote: i == 1}
		childCfg[i] = cfg
		path := filepath.Join(dir, "child.json")
		write(path, cfg)
		cmd := exec.CommandContext(ctx, "ip", "netns", "exec", names[i], os.Args[0], "-test.run=^TestOperatorRemoteKernelProcess$", "-test.v")
		cmd.Env = append(os.Environ(), "WENDY_OPERATOR_REMOTE_CHILD="+path)
		out := new(bytes.Buffer)
		cmd.Stdout, cmd.Stderr = out, out
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		commands = append(commands, cmd)
		outputs = append(outputs, out)
		done := make(chan error, 1)
		childDone = append(childDone, done)
		joined = append(joined, false)
		go func() { done <- cmd.Wait() }()
	}
	for _, cfg := range childCfg {
		for {
			if _, err := os.Stat(cfg.Ready); err == nil {
				break
			}
			select {
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			case <-time.After(50 * time.Millisecond):
			}
		}
	}
	oldDir, oldLoad, oldLaunch, oldOpen := configDir, loadConfig, launchOperatorBroker, openOperatorSession
	defer func() {
		configDir, loadConfig, launchOperatorBroker, openOperatorSession = oldDir, oldLoad, oldLaunch, oldOpen
	}()
	configDir = func() (string, error) { return directory, nil }
	loadConfig = func() (*config.Config, error) {
		return &config.Config{CurrentContext: "operator", Auth: []config.AuthConfig{{Name: "operator", Certificates: []config.CertificateInfo{certificate}}}}, nil
	}
	sessionReady := make(chan *operatormesh.Session, 1)
	openOperatorSession = func(ctx context.Context, options operatormesh.Options) (*operatormesh.Session, error) {
		options.CarrierSources = []carriers.Source{}
		s, err := operatormesh.Open(ctx, options)
		if err == nil {
			sessionReady <- s
		}
		return s, err
	}
	brokerDone := make(chan error, 1)
	var launch sync.Once
	launchOperatorBroker = func(spec Spec) error {
		launch.Do(func() {
			go func() {
				data, _ := json.Marshal(spec)
				brokerDone <- Run(ctx, base64.RawURLEncoding.EncodeToString(data), time.Minute, nil)
			}()
		})
		return nil
	}
	adjacent, err := PrepareOperator(ctx, certificate, "10.221.0.2:43024", devices[0].UUID().String())
	if err != nil {
		t.Fatal(err)
	}
	defer adjacent.Close()
	var session *operatormesh.Session
	select {
	case session = <-sessionReady:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	var remoteErr error
	var remoteVersion *agentpb.GetAgentVersionResponse
	var remoteClient *grpcclient.AgentConnection
	for until := time.Now().Add(20 * time.Second); time.Now().Before(until); {
		remote, err := ConnectOperator(ctx, MeshSelector(devices[1]))
		remoteErr = err
		if err == nil {
			defer remote.Close()
			remoteVersion, err = remote.AgentService.GetAgentVersion(ctx, &agentpb.GetAgentVersionRequest{})
			remoteErr = err
			if err == nil {
				remoteClient = remote
				break
			}
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(200 * time.Millisecond):
		}
	}
	if remoteErr != nil {
		t.Fatalf("remote selector: %v; childA=%s childB=%s", remoteErr, outputs[0], outputs[1])
	}
	if remoteVersion.Version != devices[1].UUID().String() || len(remoteVersion.Hostname) != 64*1024 {
		t.Fatalf("remote 64KiB RPC response: %v", remoteVersion)
	}
	first := session.Info().Revision
	traffic := make(chan error, 1)
	go func() {
		for i := 0; i < 20; i++ {
			if _, err := remoteClient.AgentService.GetAgentVersion(ctx, &agentpb.GetAgentVersionRequest{}); err != nil {
				traffic <- err
				return
			}
			if _, err := adjacent.AgentService.GetAgentVersion(ctx, &agentpb.GetAgentVersionRequest{}); err != nil {
				traffic <- err
				return
			}
			select {
			case <-ctx.Done():
				traffic <- ctx.Err()
				return
			case <-time.After(150 * time.Millisecond):
			}
		}
		traffic <- nil
	}()
	if err := <-traffic; err != nil {
		t.Fatalf("remote packets/adjacent RPC during steady-state routing: %v", err)
	}
	pins, err := devicepin.Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	for _, device := range devices {
		if !pins.Has(operatorExpected(device).IdentityKey()) {
			t.Fatalf("missing selected-profile pin for %s", device.Principal())
		}
	}
	if session.Info().Revision != first {
		t.Fatal("steady-state RPC traffic churned the signed operator manifest")
	}
	if reconnect {
		before := session.Info()
		beforeNode := session.Node
		_ = session.Close() // Restart the full router context, preserving local IPC.
		select {
		case session = <-sessionReady:
		case <-ctx.Done():
			t.Fatal("operator router context did not restart", ctx.Err())
		}
		after := session.Info()
		if session.Node == beforeNode || after.Address != before.Address || after.Router != before.Router || after.Revision <= before.Revision {
			t.Fatal("router context restart changed installation identity or lost its counter", before, after)
		}
		var resumed error
		for until := time.Now().Add(15 * time.Second); time.Now().Before(until); {
			attempt, stop := context.WithTimeout(ctx, 2*time.Second)
			_, resumed = remoteClient.AgentService.GetAgentVersion(attempt, &agentpb.GetAgentVersionRequest{})
			stop()
			if resumed == nil {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		if resumed != nil {
			t.Fatal("same routed management client did not resume on replacement router context", resumed)
		}
	}
	remote, err := ConnectOperator(ctx, MeshSelector(devices[1]))
	if err != nil {
		t.Fatal(err)
	}
	defer remote.Close()
	if _, err = remote.AgentService.GetAgentVersion(ctx, &agentpb.GetAgentVersionRequest{}); err != nil {
		t.Fatal(err)
	}
	// Inspect the actual remote kernel return FIB, not a model of the route.
	out, err := exec.CommandContext(ctx, "ip", "-n", names[1], "-6", "route", "get", session.Info().Address).CombinedOutput()
	if err != nil || !bytes.Contains(out, []byte(fmt.Sprintf("table %d", localmesh.RouteTable))) {
		t.Fatalf("remote operator return FIB: %v %s", err, out)
	}
	t.Logf("remote FIB: %s; operator revision %d", out, session.Info().Revision)
	// Normal power-off removes reachability without invalidating the attachment.
	if err = os.WriteFile(childCfg[1].Stop, []byte("stop"), 0600); err != nil {
		t.Fatal(err)
	}
	offline := false
	for until := time.Now().Add(10 * time.Second); time.Now().Before(until); {
		probe, stop := context.WithTimeout(ctx, time.Second)
		_, err = remote.AgentService.GetAgentVersion(probe, &agentpb.GetAgentVersionRequest{})
		stop()
		if err != nil {
			offline = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !offline {
		t.Fatal("remote target remained available after stopping its router")
	}
	if _, err = adjacent.AgentService.GetAgentVersion(ctx, &agentpb.GetAgentVersionRequest{}); err != nil {
		t.Fatalf("remote outage retired adjacent context: %v", err)
	}
	if err = DisconnectOperator(ctx); err != nil {
		t.Fatal(err)
	}
	if err = <-brokerDone; err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(childCfg[0].Stop, []byte("stop"), 0600); err != nil {
		t.Fatal(err)
	}
	for i, done := range childDone {
		select {
		case err := <-done:
			joined[i] = true
			if err != nil {
				t.Fatalf("child%d failed: %v\n%s", i, err, outputs[i].String())
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
}
