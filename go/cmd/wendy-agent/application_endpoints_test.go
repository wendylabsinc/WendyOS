package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"net/netip"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/agent/meshingress"
	"github.com/wendylabsinc/wendy/go/internal/agent/meshsession"
	"github.com/wendylabsinc/wendy/go/internal/agent/udpmessage"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"go.uber.org/zap"
)

type graceApplicationAdmission struct{ address string }

func (a graceApplicationAdmission) DialAuthorized(port uint16, dial func(string) (net.Conn, error)) (net.Conn, error) {
	if port != 8080 {
		return nil, meshsession.ErrDenied
	}
	return dial(a.address)
}

type lifecycleApplicationAdmission struct {
	graceApplicationAdmission
	*meshingress.Registry
}

func (a lifecycleApplicationAdmission) DialAuthorized(port uint16, dial func(string) (net.Conn, error)) (net.Conn, error) {
	return a.graceApplicationAdmission.DialAuthorized(port, dial)
}

// Retain real registry incarnation/admission checks while delivering the test
// app's registered private endpoint to its loopback UDP echo fixture.
func (a lifecycleApplicationAdmission) WithAuthorizedUDPToken(port uint16, token uint64, write func(netip.AddrPort) error) error {
	return a.Registry.WithAuthorizedUDPToken(port, token, func(address netip.AddrPort) error {
		return write(netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), address.Port()))
	})
}

// App identity retirement precedes the provisioning RPC's response grace: its
// parent control context can remain alive without admitting old app traffic.
func TestApplicationRetirementClosesTrafficDuringControlGrace(t *testing.T) {
	source, target := applicationGraceCredentials(t)
	app, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	go func() {
		conn, err := app.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = io.Copy(conn, conn)
	}()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	manager := udpmessage.NewManager()
	defer manager.Close()
	var workers sync.WaitGroup
	launch := func(run func()) { workers.Add(1); go func() { defer workers.Done(); run() }() }
	listener, retire := startApplicationEndpoints(ctx, target, graceApplicationAdmission{app.Addr().String()}, manager, false, zap.NewNop(), launch)
	defer retire()
	address := netip.AddrPortFrom(target.Device.MeshAddress(), meshsession.TLSPort)
	setup, stop := context.WithCancel(ctx)
	conn, err := meshsession.DialTLSWithDialer(setup, source, target.Device, address, 8080, func(ctx context.Context, _, _ string) (net.Conn, error) { return listener.Dial(ctx) })
	stop()
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(time.Second))
	if _, err := conn.Write([]byte("ready")); err != nil {
		t.Fatal(err)
	}
	reply := make([]byte, 5)
	if _, err := io.ReadFull(conn, reply); err != nil || string(reply) != "ready" {
		t.Fatalf("app echo=%q: %v", reply, err)
	}
	// No caller is reading now. Retirement must abort the raw transport rather
	// than wait for a graceful TLS close_notify to cross the in-memory pipe.
	retired := make(chan struct{})
	go func() { retire(); close(retired) }()
	select {
	case <-retired:
	case <-time.After(time.Second):
		t.Fatal("application retirement blocked on an unread peer")
	}
	if ctx.Err() != nil {
		t.Fatal("retirement canceled the control-plane response grace")
	}
	done := make(chan struct{})
	go func() { workers.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("retired endpoint workers did not drain without a peer read")
	}
	if _, err := conn.Read(reply); err == nil {
		t.Fatal("old app transport survived retirement")
	} else if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
		t.Fatal("retirement left old app transport blocked")
	}
	if raw, err := listener.Dial(ctx); err == nil {
		raw.Close()
		t.Fatal("retired application callback still admitted transports")
	}
}

func TestApplicationEndpointsSurviveRoutedBindFailure(t *testing.T) {
	for _, protocol := range []string{"tcp", "udp"} {
		t.Run(protocol, func(t *testing.T) {
			source, target := applicationGraceCredentials(t)
			app, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer app.Close()
			go func() {
				for {
					conn, err := app.Accept()
					if err != nil {
						return
					}
					go func() { defer conn.Close(); _, _ = io.Copy(conn, conn) }()
				}
			}()
			udp, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
			if err != nil {
				t.Fatal(err)
			}
			defer udp.Close()
			go func() {
				buffer := make([]byte, 1024)
				for {
					n, peer, err := udp.ReadFromUDP(buffer)
					if err != nil {
						return
					}
					_, _ = udp.WriteToUDP(buffer[:n], peer)
				}
			}()
			registry := meshingress.NewRegistry()
			if err := registry.ClaimUDPForApp("fixture", "app", 8081, "10.3.0.2", uint16(udp.LocalAddr().(*net.UDPAddr).Port)); err != nil {
				t.Fatal(err)
			}
			admission := lifecycleApplicationAdmission{graceApplicationAdmission{app.Addr().String()}, registry}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			manager := udpmessage.NewManager()
			defer manager.Close()
			var workers sync.WaitGroup
			launch := func(run func()) { workers.Add(1); go func() { defer workers.Done(); run() }() }
			advertisements := make(chan uint16, 8)
			var lanAddress netip.AddrPort
			cloud, retire := startApplicationEndpointsWithLAN(ctx, target, admission, manager, true, zap.NewNop(), launch,
				func() (net.Listener, error) {
					listener, err := net.Listen("tcp", "127.0.0.1:0")
					if err == nil {
						lanAddress = listener.Addr().(*net.TCPAddr).AddrPort()
					}
					return listener, err
				}, func(port uint16) { advertisements <- port })
			defer retire()
			for {
				select {
				case port := <-advertisements:
					if port == meshsession.ApplicationLANPort {
						goto ready
					}
				case <-time.After(time.Second):
					t.Fatal("LAN endpoint was not advertised after readiness")
				}
			}
		ready:
			tlsAddress, quicAddress := "127.0.0.1:0", "127.0.0.1:0"
			if protocol == "tcp" {
				occupied, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				defer occupied.Close()
				tlsAddress = occupied.Addr().String()
			} else {
				occupied, err := net.ListenPacket("udp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				defer occupied.Close()
				quicAddress = occupied.LocalAddr().String()
			}
			server, err := meshsession.NewServer(target, admission, manager)
			if err != nil {
				t.Fatal(err)
			}
			routed, ok := startRoutedApplicationSessions(ctx, server, tlsAddress, quicAddress, zap.NewNop(), launch)
			if ok || routed.Err() == nil || ctx.Err() != nil {
				t.Fatalf("routed ready=%v routed error=%v identity error=%v", ok, routed.Err(), ctx.Err())
			}
			for name, dial := range map[string]func(context.Context, string, string) (net.Conn, error){
				"LAN":   (&net.Dialer{}).DialContext,
				"Cloud": func(ctx context.Context, _, _ string) (net.Conn, error) { return cloud.Dial(ctx) },
			} {
				conn, err := meshsession.DialTLSWithDialer(ctx, source, target.Device, lanAddress, 8080, dial)
				if err != nil {
					t.Fatalf("%s stopped after routed bind conflict: %v", name, err)
				}
				_ = conn.SetDeadline(time.Now().Add(time.Second))
				_, err = conn.Write([]byte("alive"))
				reply := make([]byte, 5)
				if err == nil {
					_, err = io.ReadFull(conn, reply)
				}
				conn.Close()
				if err != nil || string(reply) != "alive" {
					t.Fatalf("%s app echo=%q error=%v", name, reply, err)
				}
				if denied, err := meshsession.DialTLSWithDialer(ctx, source, target.Device, lanAddress, 22, dial); !errors.Is(err, meshsession.ErrDenied) || denied != nil {
					t.Fatalf("%s admitted unpublished TCP port: %v", name, err)
				}
				datagrams, err := meshsession.DialUDPTLSWithDialer(ctx, source, target.Device, lanAddress, 8081, dial)
				if err != nil {
					t.Fatalf("%s UDP stopped after routed conflict: %v", name, err)
				}
				if err := datagrams.Send([]byte("datagram")); err != nil {
					t.Fatal(err)
				}
				receive, stop := context.WithTimeout(ctx, time.Second)
				message, err := datagrams.Receive(receive)
				stop()
				datagrams.Close()
				if err != nil {
					t.Fatalf("%s UDP echo: %v", name, err)
				}
				match := string(message.Payload) == "datagram"
				message.Release()
				if !match {
					t.Fatalf("%s changed UDP payload", name)
				}
				if denied, err := meshsession.DialUDPTLSWithDialer(ctx, source, target.Device, lanAddress, 22, dial); err == nil || denied != nil {
					t.Fatalf("%s admitted unpublished UDP port: %v", name, err)
				}
			}
			select {
			case port := <-advertisements:
				t.Fatalf("routed failure changed healthy LAN advertisement to %d", port)
			default:
			}
			cancel() // Actual generation retirement still closes every endpoint.
			done := make(chan struct{})
			go func() { workers.Wait(); close(done) }()
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("generation shutdown did not drain endpoints")
			}
			select {
			case port := <-advertisements:
				if port != 0 {
					t.Fatalf("retirement advertised port %d", port)
				}
			default:
				t.Fatal("generation shutdown did not withdraw LAN advertisement")
			}
			if conn, err := cloud.Dial(context.Background()); err == nil {
				conn.Close()
				t.Fatal("retired Cloud endpoint still accepts transports")
			}
		})
	}
}

func applicationGraceCredentials(t *testing.T) (*localmesh.Credentials, *localmesh.Credentials) {
	t.Helper()
	rootKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	root := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
	der, err := x509.CreateCertificate(rand.Reader, root, root, &rootKey.PublicKey, rootKey)
	if err != nil {
		t.Fatal(err)
	}
	root, err = x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	chain := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	issue := func(serial int64, id string) *localmesh.Credentials {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		principal, err := url.Parse(certs.DeviceSPIFFEURI("11111111-1111-4111-8111-111111111111", id))
		if err != nil {
			t.Fatal(err)
		}
		leaf := &x509.Certificate{SerialNumber: big.NewInt(serial), NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour), BasicConstraintsValid: true, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth}, URIs: []*url.URL{principal}}
		der, err := x509.CreateCertificate(rand.Reader, leaf, root, &key.PublicKey, rootKey)
		if err != nil {
			t.Fatal(err)
		}
		private, err := x509.MarshalECPrivateKey(key)
		if err != nil {
			t.Fatal(err)
		}
		credential, err := localmesh.NewCredentials(string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), chain, string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: private})))
		if err != nil {
			t.Fatal(err)
		}
		return credential
	}
	return issue(2, "22222222-2222-4222-8222-222222222222"), issue(3, "33333333-3333-4333-8333-333333333333")
}
