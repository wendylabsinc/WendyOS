package commands

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/cli/sessionbroker"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/discoverycache"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	"github.com/wendylabsinc/wendy/go/internal/testutil/deliverypki"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

type meshLANCloser func() error

func (f meshLANCloser) Close() error { return f() }

type meshLANFixture struct {
	device        meshid.Device
	selector      string
	auth          config.AuthConfig
	session       TunnelSession
	ctx           context.Context
	meshes        atomic.Int32
	closed        atomic.Int32
	appCalls      atomic.Int32
	registryCalls atomic.Int32
}

func newMeshLANFixture(t *testing.T) *meshLANFixture {
	t.Helper()
	f := &meshLANFixture{}
	f.device, _ = meshid.DeviceFromPrincipal(deliverypki.TargetPrincipal)
	f.selector = sessionbroker.MeshSelector(f.device)
	f.auth = config.AuthConfig{Name: "selected", CloudGRPC: "cloud.example", Certificates: []config.CertificateInfo{meshTestCert(deliverypki.Tenant, "operator")}}
	t.Setenv("WENDY_CONFIG_DIR", t.TempDir())
	t.Setenv("WENDY_SECRET_STORE", "file")
	t.Setenv("WENDY_TLS_SESSION_STORE", "off")
	f.save(t)
	oldConnect, oldCandidates, oldDial := connectOperatorMeshSelector, desktopMeshLocalCandidates, desktopLocalDial
	t.Cleanup(func() {
		connectOperatorMeshSelector, desktopMeshLocalCandidates, desktopLocalDial = oldConnect, oldCandidates, oldDial
	})
	connectOperatorMeshSelector = func(ctx context.Context, selector string) (*grpcclient.AgentConnection, error) {
		if selector != f.selector {
			t.Errorf("changed selected identity: %s", selector)
		}
		f.meshes.Add(1)
		certificate := f.auth.Certificates[0]
		return &grpcclient.AgentConnection{Host: selector, CertInfo: &certificate, OperatorMeshTarget: f.device,
			ExtraClosers: []io.Closer{meshLANCloser(func() error { f.closed.Add(1); return nil })},
			AppDialer: func(context.Context, uint16) (net.Conn, error) {
				f.appCalls.Add(1)
				return nil, errors.New("app route")
			},
			RegistryDialer: func(context.Context, int) (net.Conn, error) {
				f.registryCalls.Add(1)
				return nil, errors.New("registry route")
			}}, nil
	}
	desktopMeshLocalCandidates = func(meshid.Device) []string { return []string{"192.0.2.1:50052"} }
	desktopLocalDial = func(context.Context, string, config.CertificateInfo, certs.WendyIdentity) (*grpcclient.AgentConnection, error) {
		return nil, errors.New("unreachable")
	}
	f.session.SetActive(true)
	f.ctx = f.session.Context(t.Context())
	t.Cleanup(func() { f.session.SetActive(false) })
	return f
}

func (f *meshLANFixture) save(t *testing.T) {
	t.Helper()
	if err := config.Save(&config.Config{CurrentContext: f.auth.Name, Auth: []config.AuthConfig{f.auth}}); err != nil {
		t.Fatal(err)
	}
}

func TestDesktopMeshLANUpgradeOwnsRoutesAndReconnect(t *testing.T) {
	f := newMeshLANFixture(t)
	var localClosed atomic.Int32
	desktopLocalDial = func(_ context.Context, _ string, certificate config.CertificateInfo, expected certs.WendyIdentity) (*grpcclient.AgentConnection, error) {
		if certificate != f.auth.Certificates[0] || expected.Principal != deliverypki.TargetPrincipal {
			t.Error("LAN authentication lost credential or device binding")
		}
		return &grpcclient.AgentConnection{Host: "192.0.2.1", IsMTLS: true, ExtraClosers: []io.Closer{meshLANCloser(func() error { localClosed.Add(1); return nil })}}, nil
	}
	one, err := connectDesktopMeshAgent(f.ctx, f.selector)
	if err != nil {
		t.Fatal(err)
	}
	if one.Host != "192.0.2.1" || one.OperatorMeshTarget != f.device {
		t.Fatal("LAN shortcut lost identity")
	}
	_, _ = one.AppDialer(f.ctx, 8080)
	_, _ = one.RegistryDialer(f.ctx, 5000)
	if f.appCalls.Load() != 1 || f.registryCalls.Load() != 1 {
		t.Fatal("auxiliary mesh routes replaced")
	}
	two, err := connectDesktopMeshAgent(f.ctx, f.selector)
	if err != nil {
		t.Fatal(err)
	}
	if f.meshes.Load() != 1 {
		t.Fatal("native connection was not reused")
	}
	one.Close()
	if f.closed.Load() != 0 || localClosed.Load() != 0 {
		t.Fatal("one handle closed another consumer's connection")
	}
	next, err := two.Reconnect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if f.meshes.Load() != 2 || next.Host != "192.0.2.1" {
		t.Fatal("reconnect did not reacquire the same target and upgrade")
	}
	two.Close()
	next.Close()
	f.session.SetActive(false)
	deadline := time.Now().Add(time.Second)
	for (f.closed.Load() != 2 || localClosed.Load() != 2) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if f.closed.Load() != 2 || localClosed.Load() != 2 {
		t.Fatalf("leaked transports: mesh=%d LAN=%d", f.closed.Load(), localClosed.Load())
	}
	if stale, err := next.Reconnect(context.Background()); stale != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("retired generation reconnected: %v", err)
	}
}

func TestDesktopMeshLANFallbackAndScope(t *testing.T) {
	for _, mode := range []string{"unreachable", "no hints", "CLI", "VPN", "wrong credential", "wrong target", "account changed", "certificate changed", "generation changed"} {
		t.Run(mode, func(t *testing.T) {
			f := newMeshLANFixture(t)
			ctx := f.ctx
			var attempts atomic.Int32
			base := connectOperatorMeshSelector
			connectOperatorMeshSelector = func(ctx context.Context, s string) (*grpcclient.AgentConnection, error) {
				c, e := base(ctx, s)
				if mode == "wrong credential" {
					copy := *c.CertInfo
					copy.PrincipalURI = "spiffe://wendy.sh/tenant/" + deliverypki.Tenant + "/operator/other"
					c.CertInfo = &copy
				}
				if mode == "wrong target" {
					c.OperatorMeshTarget = meshid.Device{}
				}
				return c, e
			}
			if mode == "no hints" {
				desktopMeshLocalCandidates = func(meshid.Device) []string { return nil }
			}
			if mode == "CLI" {
				ctx = t.Context()
			}
			if mode == "VPN" {
				ctx = context.WithValue(ctx, cloudTunnelOnlyKey{}, true)
			}
			desktopLocalDial = func(context.Context, string, config.CertificateInfo, certs.WendyIdentity) (*grpcclient.AgentConnection, error) {
				attempts.Add(1)
				switch mode {
				case "account changed":
					f.auth.Name = "other"
					f.save(t)
				case "certificate changed":
					f.auth.Certificates[0].PemCertificate = "renewed"
					f.save(t)
				case "generation changed":
					f.session.SetActive(false)
				}
				return nil, errors.New("unreachable")
			}
			c, err := connectDesktopMeshAgent(ctx, f.selector)
			refused := mode == "wrong credential" || mode == "wrong target" || mode == "account changed" || mode == "certificate changed" || mode == "generation changed"
			if refused {
				if err == nil || c != nil {
					t.Fatal("changed scope accepted")
				}
			} else {
				if err != nil || c.Host != f.selector {
					t.Fatalf("mesh fallback lost: %v", err)
				}
				c.Close()
			}
			if (mode == "CLI" || mode == "VPN" || mode == "no hints" || mode == "wrong credential" || mode == "wrong target") && attempts.Load() != 0 {
				t.Fatal("unexpected LAN dial")
			}
		})
	}
}

func TestDesktopMeshLANClosesCancelledWinner(t *testing.T) {
	f := newMeshLANFixture(t)
	started := make(chan struct{})
	release := make(chan struct{})
	closed := make(chan struct{})
	desktopLocalDial = func(context.Context, string, config.CertificateInfo, certs.WendyIdentity) (*grpcclient.AgentConnection, error) {
		close(started)
		<-release
		return &grpcclient.AgentConnection{ExtraClosers: []io.Closer{meshLANCloser(func() error { close(closed); return nil })}}, nil
	}
	done := make(chan error, 1)
	go func() {
		c, e := connectDesktopMeshAgent(f.ctx, f.selector)
		if c != nil {
			c.Close()
		}
		done <- e
	}()
	<-started
	f.session.SetActive(false)
	close(release)
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled acquisition: %v", err)
	}
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("late LAN winner leaked")
	}
}

func TestDesktopMeshLANStaleHintHasBoundedFallback(t *testing.T) {
	f := newMeshLANFixture(t)
	desktopLocalDial = func(ctx context.Context, _ string, _ config.CertificateInfo, _ certs.WendyIdentity) (*grpcclient.AgentConnection, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	start := time.Now()
	c, err := connectDesktopMeshAgent(f.ctx, f.selector)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if c.Host != f.selector || time.Since(start) > cloudLANBudget+time.Second {
		t.Fatal("stale LAN hint blocked authenticated mesh fallback")
	}
	f.auth.Name = "other-profile"
	f.save(t)
	if changed, err := c.Reconnect(context.Background()); changed != nil || err == nil {
		t.Fatal("reconnect crossed the original account scope")
	}
}

func TestDesktopMeshLANAuthenticatesExactDeviceAndPins(t *testing.T) {
	for _, mode := range []string{"matching", "different device", "saved pin mismatch"} {
		t.Run(mode, func(t *testing.T) {
			f := newMeshLANFixture(t)
			pki := deliverypki.New(t)
			f.auth.Certificates[0] = config.CertificateInfo{PemCertificate: pki.Operator.PEM, PemCertificateChain: pki.Chain, PemPrivateKey: pki.Operator.Key, PrincipalURI: deliverypki.OperatorPrincipal}
			f.save(t)
			credential := pki.Target
			if mode == "different device" {
				credential = pki.Builder
			}
			pair, err := certs.TLSKeyPair(credential.PEM, pki.Chain, credential.Key)
			if err != nil {
				t.Fatal(err)
			}
			roots := x509.NewCertPool()
			roots.AppendCertsFromPEM([]byte(pki.Chain))
			server := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{pair}, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: roots})))
			agentpb.RegisterWendyAgentServiceServer(server, versionOnlyAgent{os: "wendyos"})
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			go server.Serve(listener)
			t.Cleanup(func() { server.Stop(); listener.Close() })
			if mode == "saved pin mismatch" {
				if err := openPinStore().CheckAndUpdate(pki.Mint(t, deliverypki.TargetPrincipal).Leaf, "previous key"); err != nil {
					t.Fatal(err)
				}
			}
			desktopMeshLocalCandidates = func(meshid.Device) []string { return []string{listener.Addr().String()} }
			desktopLocalDial = dialDesktopLocal
			c, err := connectDesktopMeshAgent(f.ctx, f.selector)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			if mode == "matching" {
				if c.Host == f.selector || !c.IsMTLS {
					t.Fatal("matching LAN identity not used")
				}
			} else if c.Host != f.selector {
				t.Fatal("unverified LAN identity displaced authenticated mesh")
			}
		})
	}
}

func TestCachedMeshLANHintsAreBoundedAndIdentityMatched(t *testing.T) {
	f := newMeshLANFixture(t)
	cache, err := discoverycache.Load()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	cache.Upsert(discoverycache.Entry{ID: f.device.UUID().String(), IP: "192.168.1.20", Port: 50052, MTLS: true}, now)
	cache.Upsert(discoverycache.Entry{ID: "other", DisplayName: f.device.UUID().String(), IP: "192.168.1.21", Port: 50052, MTLS: true}, now)
	cache.Upsert(discoverycache.Entry{ID: f.device.Principal(), IP: "192.168.1.22", Port: 50052, MTLS: false}, now)
	if err := cache.Flush(now); err != nil {
		t.Fatal(err)
	}
	got := cachedMeshLocalCandidates(f.device)
	if len(got) != 2 || got[0] != "192.168.1.20:50052" || got[1] != "192.168.1.20:50053" {
		t.Fatalf("unsafe hints: %v", got)
	}
}
