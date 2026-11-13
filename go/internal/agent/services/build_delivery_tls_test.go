package services

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/mtls"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

type deliveryTLSFixture struct {
	client        *tls.Config
	target, wrong *tls.Config
}

func newDeliveryTLSFixture(t *testing.T) deliveryTLSFixture {
	t.Helper()
	ca, caKey, caPEM := brokerTestCA(t)
	mint := func(device meshid.Device) (tls.Certificate, string, string) {
		t.Helper()
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		uri, _ := url.Parse(device.Principal())
		template := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), URIs: []*url.URL{uri}, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth}}
		der, err := x509.CreateCertificate(rand.Reader, template, ca, &key.PublicKey, caKey)
		if err != nil {
			t.Fatal(err)
		}
		kd, err := x509.MarshalECPrivateKey(key)
		if err != nil {
			t.Fatal(err)
		}
		return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kd}))
	}
	self := mustTestDevice("3f2a7c9d-8b1e-4f6a-9c0d-1e2b3c4d5e71")
	_, leaf, key := mint(self)
	client, err := mtls.NewClientTLSConfigExpectingDevice(leaf, caPEM, key, zap.NewNop(), testMeshDevice)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	server := func(device meshid.Device) *tls.Config {
		cert, _, _ := mint(device)
		return &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{cert}, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: roots,
			VerifyConnection: func(state tls.ConnectionState) error {
				if len(state.PeerCertificates) == 0 {
					return errors.New("missing build host certificate")
				}
				got, err := meshid.DeviceFromCert(state.PeerCertificates[0])
				if err != nil || got != self {
					return errors.New("build host credential changed during retry")
				}
				return nil
			}}
	}
	return deliveryTLSFixture{client: client, target: server(testMeshDevice), wrong: server(mustTestDevice("3f2a7c9d-8b1e-4f6a-9c0d-1e2b3c4d5e72"))}
}

func deliveryTestRoutes(t *testing.T, lan, routed string) (*MeshDialer, *atomic.Int32) {
	t.Helper()
	d := NewMeshDialer(zap.NewNop(), "", "", "", nil)
	calls := new(atomic.Int32)
	d.lanLookup = func(context.Context, meshid.Device) (string, bool) { return lan, true }
	d.dialLAN = func(ctx context.Context, _ string, _ meshid.Device, _ uint16) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", lan)
	}
	d.serviceDial = func(ctx context.Context, peer meshid.Device, _ uint16) (net.Conn, error) {
		if peer != testMeshDevice {
			return nil, errors.New("retry changed target principal")
		}
		calls.Add(1)
		return (&net.Dialer{}).DialContext(ctx, "tcp", routed)
	}
	return d, calls
}

type deliveryTLSProbe struct {
	agentpb.UnimplementedWendyContainerServiceServer
	calls atomic.Int32
}

func (p *deliveryTLSProbe) QueryChunks(context.Context, *agentpb.QueryChunksRequest) (*agentpb.QueryChunksResponse, error) {
	p.calls.Add(1)
	return &agentpb.QueryChunksResponse{}, nil
}

func TestBuildDeliveryGRPCFallsBackAfterLANPinFailure(t *testing.T) {
	fixture := newDeliveryTLSFixture(t)
	serve := func(config *tls.Config) (string, *deliveryTLSProbe) {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		server := grpc.NewServer(grpc.Creds(credentials.NewTLS(config)))
		probe := new(deliveryTLSProbe)
		agentpb.RegisterWendyContainerServiceServer(server, probe)
		go server.Serve(listener)
		t.Cleanup(server.Stop)
		return listener.Addr().String(), probe
	}
	lan, impostor := serve(fixture.wrong)
	routed, target := serve(fixture.target)
	peers, calls := deliveryTestRoutes(t, lan, routed)
	dial := meshTargetDialer(peers, func(string) (DeliveryTarget, error) { return dialerTestTarget, nil }, func(meshid.Device) (*tls.Config, error) { return fixture.client, nil }, 50052)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := dial(ctx, &agentpbv2.PushTarget{AssetUuid: testAssetUUID})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := agentpb.NewWendyContainerServiceClient(conn).QueryChunks(ctx, &agentpb.QueryChunksRequest{}); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 || target.calls.Load() != 1 || impostor.calls.Load() != 0 {
		t.Fatalf("routed=%d target RPCs=%d impostor RPCs=%d", calls.Load(), target.calls.Load(), impostor.calls.Load())
	}
}

func TestBuildDeliveryRegistryFallbackKeepsPrincipalPin(t *testing.T) {
	fixture := newDeliveryTLSFixture(t)
	for _, wrongBabel := range []bool{false, true} {
		t.Run(fmt.Sprint("wrong-Babel=", wrongBabel), func(t *testing.T) {
			serve := func(config *tls.Config) (*httptest.Server, *atomic.Int32) {
				calls := new(atomic.Int32)
				server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { calls.Add(1); io.WriteString(w, "registry") }))
				server.TLS = config.Clone()
				server.StartTLS()
				t.Cleanup(server.Close)
				return server, calls
			}
			lan, lanCalls := serve(fixture.wrong)
			config := fixture.target
			if wrongBabel {
				config = fixture.wrong
			}
			routed, routedCalls := serve(config)
			peers, attempts := deliveryTestRoutes(t, lan.Listener.Addr().String(), routed.Listener.Addr().String())
			proxy, err := newPushProxy(peers, dialerTestTarget, 5000, fixture.client)
			if err != nil {
				t.Fatal(err)
			}
			defer proxy.stop()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			conn, err := proxy.dial(ctx)
			if wrongBabel {
				if err == nil {
					conn.Close()
					t.Fatal("wrong Babel principal accepted")
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				defer conn.Close()
				conn.SetDeadline(time.Now().Add(time.Second))
				if _, err := io.WriteString(conn, "GET /v2/ HTTP/1.1\r\nHost: registry\r\nConnection: close\r\n\r\n"); err != nil {
					t.Fatal(err)
				}
				response, err := http.ReadResponse(bufio.NewReader(conn), nil)
				if err != nil {
					t.Fatal(err)
				}
				defer response.Body.Close()
				if response.StatusCode != http.StatusOK {
					t.Fatal(response.Status)
				}
			}
			expected := int32(1)
			if wrongBabel {
				expected = 0
			}
			if attempts.Load() != 1 || lanCalls.Load() != 0 || routedCalls.Load() != expected {
				t.Fatalf("attempts=%d LAN requests=%d Babel requests=%d", attempts.Load(), lanCalls.Load(), routedCalls.Load())
			}
		})
	}
}

func TestBuildDeliveryCancelledAuthenticationDoesNotRetry(t *testing.T) {
	peers, calls := deliveryTestRoutes(t, "unused", "unused")
	raw, other := net.Pipe()
	defer other.Close()
	ctx, cancel := context.WithCancel(context.Background())
	_, err := authenticateDelivery(ctx, peers, dialerTestTarget, 50052, raw, "lan-direct", func(context.Context, net.Conn) (net.Conn, error) {
		cancel()
		return nil, context.Canceled
	})
	if !errors.Is(err, context.Canceled) || calls.Load() != 0 {
		t.Fatalf("error=%v retries=%d", err, calls.Load())
	}
}

func TestBuildDeliveryCredentialsRefuseUnauthenticatedSocket(t *testing.T) {
	raw, other := net.Pipe()
	defer raw.Close()
	defer other.Close()
	adapter := (&deliveryCredentials{TransportCredentials: credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS13})}).Clone()
	if _, _, err := adapter.ClientHandshake(context.Background(), "build-target", raw); err == nil {
		t.Fatal("delivery credentials accepted a socket that skipped TLS authentication")
	}
}

func TestBuildDeliveryRejectsMissingTLSConfiguration(t *testing.T) {
	peers, probes := testDialer()
	dial := meshTargetDialer(peers, func(string) (DeliveryTarget, error) { return dialerTestTarget, nil }, func(meshid.Device) (*tls.Config, error) { return nil, nil }, 50052)
	if conn, err := dial(context.Background(), &agentpbv2.PushTarget{AssetUuid: testAssetUUID}); err == nil {
		conn.Close()
		t.Fatal("gRPC delivery accepted missing TLS configuration")
	}
	proxy, err := newPushProxy(peers, dialerTestTarget, 5000, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer proxy.stop()
	if conn, err := proxy.dial(context.Background()); err == nil {
		conn.Close()
		t.Fatal("registry delivery accepted missing TLS configuration")
	}
	if probes.lookups != 0 || probes.lanDials != 0 || probes.brokerDials != 0 {
		t.Fatal("missing TLS credentials triggered a network operation")
	}
}
