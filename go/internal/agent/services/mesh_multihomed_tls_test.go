package services

import (
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/agent/meshingress"
	"github.com/wendylabsinc/wendy/go/internal/agent/meshsession"
	"github.com/wendylabsinc/wendy/go/internal/shared/discovery"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
	"go.uber.org/zap"
)

type multihomedAdmission struct {
	*meshingress.Registry
	tcp string
	udp netip.AddrPort
}

func (a multihomedAdmission) DialAuthorized(port uint16, dial func(string) (net.Conn, error)) (net.Conn, error) {
	return a.Registry.DialAuthorized(port, func(string) (net.Conn, error) { return dial(a.tcp) })
}
func (a multihomedAdmission) WithAuthorizedUDPToken(port uint16, token uint64, send func(netip.AddrPort) error) error {
	return a.Registry.WithAuthorizedUDPToken(port, token, func(netip.AddrPort) error { return send(a.udp) })
}

func multihomedTLSFixture(t *testing.T) (*MeshDialer, meshid.Device, *meshsession.ApplicationListener) {
	t.Helper()
	root, issue := newRPCPolicyIssuer(t)
	peer := appTestMeshDevice(t)
	self, _ := meshid.ParseDevice(peer.Tenant(), "cccccccc-cccc-4ccc-8ccc-cccccccccccc")
	serverCert, serverKey := issue(2, "server", peer.Principal())
	sourceCert, sourceKey := issue(3, "source", self.Principal())
	credentials, err := localmesh.NewCredentials(serverCert, root, serverKey)
	if err != nil {
		t.Fatal(err)
	}
	app, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { app.Close() })
	go func() {
		for {
			conn, err := app.Accept()
			if err != nil {
				return
			}
			go func() { defer conn.Close(); io.Copy(conn, conn) }()
		}
	}()
	udp, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { udp.Close() })
	go func() {
		buffer := make([]byte, 8192)
		for {
			n, addr, err := udp.ReadFromUDP(buffer)
			if err != nil {
				return
			}
			udp.WriteToUDP(buffer[:n], addr)
		}
	}()
	registry := meshingress.NewRegistry()
	if err := registry.ClaimForApp("tcp", "test", 8080, "10.3.0.2", 8080); err != nil {
		t.Fatal(err)
	}
	if err := registry.ClaimUDPForApp("udp", "test", 8081, "10.3.0.2", 8081); err != nil {
		t.Fatal(err)
	}
	server, err := meshsession.NewServer(credentials, multihomedAdmission{registry, app.Addr().String(), udp.LocalAddr().(*net.UDPAddr).AddrPort()})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	listener := meshsession.NewApplicationListener()
	done := make(chan error, 1)
	go func() { done <- server.ServeTLS(ctx, listener) }()
	t.Cleanup(func() { cancel(); server.Close(); <-done })
	dialer := NewMeshDialer(zap.NewNop(), sourceCert, sourceKey, root, nil)
	return dialer, peer, listener
}

func multihomedTLSEvents(d *MeshDialer, peer meshid.Device, closed chan<- struct{}) {
	d.lanEvents = func(ctx context.Context, _ discovery.StreamOptions) <-chan discovery.LANEvent {
		out := make(chan discovery.LANEvent)
		go func() {
			defer close(out)
			if closed != nil {
				defer close(closed)
			}
			first := models.LANDevice{PrincipalURI: peer.Principal(), IPAddress: "2001:db8::1", IsMTLS: true, Port: 50052, AppTLSPort: 43023}
			select {
			case out <- discovery.LANEvent{Kind: discovery.LANFound, Device: first}:
			case <-ctx.Done():
				return
			}
			first.IPAddress = "192.0.2.10"
			first.Addresses = []string{"192.0.2.10", "2001:db8::1"}
			select {
			case out <- discovery.LANEvent{Kind: discovery.LANUpdated, Device: first}:
			case <-ctx.Done():
				return
			}
			<-ctx.Done()
		}()
		return out
	}
}

func TestMultihomedApplicationTLSRetriesOnlyUnavailableAddresses(t *testing.T) {
	for _, failure := range []string{"unreachable", "refused", "timeout"} {
		for _, datagrams := range []bool{false, true} {
			t.Run(failure+"-"+map[bool]string{false: "TCP", true: "UDP"}[datagrams], func(t *testing.T) {
				d, peer, listener := multihomedTLSFixture(t)
				closed := make(chan struct{})
				multihomedTLSEvents(d, peer, closed)
				var attempts atomic.Int32
				d.appSocketDial = func(ctx context.Context, _, address string) (net.Conn, error) {
					attempts.Add(1)
					if strings.HasPrefix(address, "[2001:db8:") {
						switch failure {
						case "timeout":
							<-ctx.Done()
							return nil, ctx.Err()
						case "refused":
							return nil, syscall.ECONNREFUSED
						default:
							return nil, syscall.ENETUNREACH
						}
					}
					return listener.Dial(ctx)
				}
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				setup, stop := context.WithCancel(ctx)
				if datagrams {
					flow, err := d.DialApplicationUDPWithoutMesh(setup, peer, 8081, meshsession.UDPOptions{})
					stop()
					if err != nil {
						t.Fatal(err)
					}
					defer flow.Close()
					if err := flow.Send([]byte("sibling")); err != nil {
						t.Fatal(err)
					}
					message, err := flow.Receive(ctx)
					if err != nil {
						t.Fatal(err)
					}
					defer message.Release()
					if string(message.Payload) != "sibling" {
						t.Fatalf("UDP echo=%q", message.Payload)
					}
				} else {
					flow, _, err := NewMeshAppDialer(d).DialDevice(setup, peer, 8080)
					stop()
					if err != nil {
						t.Fatal(err)
					}
					defer flow.Close()
					flow.SetDeadline(time.Now().Add(time.Second))
					if _, err := flow.Write([]byte("sibling")); err != nil {
						t.Fatal(err)
					}
					reply := make([]byte, 7)
					if _, err := io.ReadFull(flow, reply); err != nil || string(reply) != "sibling" {
						t.Fatalf("TCP echo=%q: %v", reply, err)
					}
				}
				if attempts.Load() != 2 {
					t.Fatalf("address attempts=%d", attempts.Load())
				}
				select {
				case <-closed:
				case <-time.After(time.Second):
					t.Fatal("successful sibling dial left discovery running")
				}
			})
		}
	}
}

func TestMultihomedApplicationTLSAuthenticationAndAdmissionAreTerminal(t *testing.T) {
	for _, datagrams := range []bool{false, true} {
		for _, wrongIdentity := range []bool{false, true} {
			t.Run(map[bool]string{false: "TCP", true: "UDP"}[datagrams]+map[bool]string{false: "-unpublished-port", true: "-wrong-principal"}[wrongIdentity], func(t *testing.T) {
				d, peer, listener := multihomedTLSFixture(t)
				if wrongIdentity {
					peer, _ = meshid.ParseDevice(peer.Tenant(), "dddddddd-dddd-4ddd-8ddd-dddddddddddd")
				}
				closed := make(chan struct{})
				multihomedTLSEvents(d, peer, closed)
				var attempts atomic.Int32
				d.appSocketDial = func(ctx context.Context, _, _ string) (net.Conn, error) { attempts.Add(1); return listener.Dial(ctx) }
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				var err error
				if datagrams {
					flow, failed := d.DialApplicationUDPWithoutMesh(ctx, peer, 8099, meshsession.UDPOptions{})
					err = failed
					if flow != nil {
						flow.Close()
					}
				} else {
					flow, _, failed := NewMeshAppDialer(d).DialDevice(ctx, peer, 8099)
					err = failed
					if flow != nil {
						flow.Close()
					}
				}
				if err == nil {
					t.Fatal("unauthorized app flow accepted")
				}
				if !wrongIdentity && !errors.Is(err, meshsession.ErrDenied) {
					t.Fatalf("port denial=%v", err)
				}
				if attempts.Load() != 1 {
					t.Fatalf("authorization failure retried %d addresses: %v", attempts.Load(), err)
				}
				select {
				case <-closed:
				case <-time.After(time.Second):
					t.Fatal("terminal admission left discovery running")
				}
			})
		}
	}
}

func TestMultihomedApplicationTLSUntrustedCertificateIsTerminal(t *testing.T) {
	for _, datagrams := range []bool{false, true} {
		t.Run(map[bool]string{false: "TCP", true: "UDP"}[datagrams], func(t *testing.T) {
			d, peer, _ := multihomedTLSFixture(t)
			_, _, untrusted := multihomedTLSFixture(t)
			stopped := make(chan struct{})
			multihomedTLSEvents(d, peer, stopped)
			var attempts atomic.Int32
			d.appSocketDial = func(ctx context.Context, _, _ string) (net.Conn, error) { attempts.Add(1); return untrusted.Dial(ctx) }
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			var err error
			if datagrams {
				_, err = d.DialApplicationUDPWithoutMesh(ctx, peer, 8081, meshsession.UDPOptions{})
			} else {
				_, _, err = NewMeshAppDialer(d).DialDevice(ctx, peer, 8080)
			}
			if err == nil || retryableLANAddressError(err) || attempts.Load() != 1 {
				t.Fatalf("untrusted peer err=%v attempts=%d", err, attempts.Load())
			}
			select {
			case <-stopped:
			case <-time.After(time.Second):
				t.Fatal("untrusted peer left discovery running")
			}
		})
	}
}

func TestMultihomedApplicationCancellationStopsDialAndDiscovery(t *testing.T) {
	for _, datagrams := range []bool{false, true} {
		t.Run(map[bool]string{false: "TCP", true: "UDP"}[datagrams], func(t *testing.T) {
			d, peer, _ := multihomedTLSFixture(t)
			stopped := make(chan struct{})
			multihomedTLSEvents(d, peer, stopped)
			started := make(chan struct{})
			var attempts atomic.Int32
			d.appSocketDial = func(ctx context.Context, _, _ string) (net.Conn, error) {
				attempts.Add(1)
				close(started)
				<-ctx.Done()
				return nil, ctx.Err()
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			result := make(chan error, 1)
			go func() {
				if datagrams {
					_, err := d.DialApplicationUDPWithoutMesh(ctx, peer, 8081, meshsession.UDPOptions{})
					result <- err
				} else {
					_, _, err := NewMeshAppDialer(d).DialDevice(ctx, peer, 8080)
					result <- err
				}
			}()
			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatal("LAN dial did not start")
			}
			cancel()
			select {
			case err := <-result:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("cancelled dial=%v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("cancelled LAN dial stayed blocked")
			}
			if attempts.Load() != 1 {
				t.Fatalf("cancelled dial tried %d addresses", attempts.Load())
			}
			select {
			case <-stopped:
			case <-time.After(time.Second):
				t.Fatal("cancelled LAN dial left discovery running")
			}
		})
	}
}
