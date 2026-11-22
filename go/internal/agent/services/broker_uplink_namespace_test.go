//go:build regression_broker_uplink

package services

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	cloudpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

// The broker is a protocol boundary, not a replacement for the Agent path:
// Run, runOnce, route lookup, TLS probe, timers, session accounting and relay
// all run unchanged. veth interfaces model uplinks; no radio/sysfs/Avahi claim.
const uplinkPrincipal = "urn:wendy:org:7:asset:42"

type uplinkCredentials struct{ CA, ServerCert, ServerKey, DeviceCert, DeviceKey string }

func makeUplinkCredentials(t *testing.T, address string) uplinkCredentials {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "private uplink fixture"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, ca, ca, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	ca, err = x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	result := uplinkCredentials{CA: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))}
	issue := func(serial int64, client bool) (string, string) {
		k, e := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if e != nil {
			t.Fatal(e)
		}
		c := &x509.Certificate{SerialNumber: big.NewInt(serial), NotBefore: ca.NotBefore, NotAfter: ca.NotAfter, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, IPAddresses: []net.IP{net.ParseIP(address)}}
		if client {
			u, _ := url.Parse(uplinkPrincipal)
			c.URIs = []*url.URL{u}
			c.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
		}
		d, e := x509.CreateCertificate(rand.Reader, c, ca, &k.PublicKey, key)
		if e != nil {
			t.Fatal(e)
		}
		kd, e := x509.MarshalECPrivateKey(k)
		if e != nil {
			t.Fatal(e)
		}
		return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: d})), string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kd}))
	}
	result.ServerCert, result.ServerKey = issue(2, false)
	result.DeviceCert, result.DeviceKey = issue(3, true)
	return result
}

type uplinkSnapshot struct {
	Registrations, PresenceActive, TunnelsActive int
	PresenceSource, TunnelSource                 string
	Heartbeats                                   int
}
type uplinkExchange struct {
	data   []byte
	result chan []byte
}
type uplinkTunnel struct {
	id   string
	work chan uplinkExchange
	stop chan struct{}
	once sync.Once
}
type uplinkBroker struct {
	cloudpbv2.UnimplementedTunnelBrokerServiceServer
	mu       sync.Mutex
	snapshot uplinkSnapshot
	requests chan *cloudpbv2.DialRequest
	tunnel   *uplinkTunnel
}

func (b *uplinkBroker) authenticate(ctx context.Context) (string, error) {
	p, ok := peer.FromContext(ctx)
	if !ok {
		return "", fmt.Errorf("missing actual TCP peer")
	}
	info, ok := p.AuthInfo.(credentials.TLSInfo)
	if !ok || len(info.State.VerifiedChains) == 0 {
		return "", fmt.Errorf("missing verified mTLS chain")
	}
	cert := info.State.PeerCertificates[0]
	if len(cert.URIs) != 1 || cert.URIs[0].String() != uplinkPrincipal {
		return "", fmt.Errorf("wrong enrolled principal")
	}
	md, _ := metadata.FromIncomingContext(ctx)
	if got := md.Get("x-wendy-certificate-uri"); len(got) != 1 || got[0] != uplinkPrincipal {
		return "", fmt.Errorf("missing method-bound identity metadata")
	}
	if got := md.Get("x-wendy-certificate-signature"); len(got) != 1 || got[0] == "" {
		return "", fmt.Errorf("missing certificate proof")
	}
	return p.Addr.(*net.TCPAddr).IP.String(), nil
}
func (b *uplinkBroker) RegisterPresence(s grpc.BidiStreamingServer[cloudpbv2.AgentHeartbeat, cloudpbv2.DialRequest]) error {
	source, err := b.authenticate(s.Context())
	if err != nil {
		return status.Error(codes.Unauthenticated, err.Error())
	}
	requests := make(chan *cloudpbv2.DialRequest, 1)
	b.mu.Lock()
	b.snapshot.Registrations++
	b.snapshot.PresenceActive++
	b.snapshot.PresenceSource = source
	b.requests = requests
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		b.snapshot.PresenceActive--
		if b.requests == requests {
			b.requests = nil
		}
		b.mu.Unlock()
	}()
	if err := s.SendHeader(metadata.Pairs("fixture", "uplink")); err != nil {
		return err
	}
	recv := make(chan error, 1)
	go func() {
		for {
			_, e := s.Recv()
			if e != nil {
				recv <- e
				return
			}
			b.mu.Lock()
			b.snapshot.Heartbeats++
			b.mu.Unlock()
		}
	}()
	for {
		select {
		case r := <-requests:
			if err := s.Send(r); err != nil {
				return err
			}
		case e := <-recv:
			return e
		case <-s.Context().Done():
			return s.Context().Err()
		}
	}
}
func (b *uplinkBroker) AgentTunnel(s grpc.BidiStreamingServer[cloudpbv2.TunnelData, cloudpbv2.TunnelData]) error {
	source, err := b.authenticate(s.Context())
	if err != nil {
		return status.Error(codes.Unauthenticated, err.Error())
	}
	join, err := s.Recv()
	if err != nil {
		return err
	}
	b.mu.Lock()
	tunnel := b.tunnel
	if tunnel == nil || join.SessionId != tunnel.id {
		b.mu.Unlock()
		return status.Error(codes.InvalidArgument, "unexpected session")
	}
	b.snapshot.TunnelsActive++
	b.snapshot.TunnelSource = source
	b.mu.Unlock()
	defer func() { b.mu.Lock(); b.snapshot.TunnelsActive--; b.mu.Unlock() }()
	for {
		select {
		case exchange := <-tunnel.work:
			if err := s.Send(&cloudpbv2.TunnelData{Payload: exchange.data}); err != nil {
				return err
			}
			var received []byte
			for len(received) < len(exchange.data) {
				m, e := s.Recv()
				if e != nil {
					return e
				}
				received = append(received, m.Payload...)
			}
			exchange.result <- received
		case <-tunnel.stop:
			return status.Error(codes.Canceled, "fixture session released")
		case <-s.Context().Done():
			return s.Context().Err()
		}
	}
}
func (b *uplinkBroker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/state":
		b.mu.Lock()
		s := b.snapshot
		b.mu.Unlock()
		_ = json.NewEncoder(w).Encode(s)
	case "/open":
		var req struct {
			Port uint32
			ID   string
		}
		if json.NewDecoder(r.Body).Decode(&req) != nil {
			http.Error(w, "bad open", 400)
			return
		}
		b.mu.Lock()
		requests := b.requests
		if requests == nil || b.snapshot.TunnelsActive != 0 {
			b.mu.Unlock()
			http.Error(w, "no idle presence", 409)
			return
		}
		b.tunnel = &uplinkTunnel{id: req.ID, work: make(chan uplinkExchange), stop: make(chan struct{})}
		b.mu.Unlock()
		select {
		case requests <- &cloudpbv2.DialRequest{SessionId: req.ID, Host: "127.0.0.1", Port: req.Port}:
		case <-r.Context().Done():
			return
		}
	case "/exchange":
		data, err := io.ReadAll(io.LimitReader(r.Body, 65537))
		if err != nil || len(data) > 65536 {
			http.Error(w, "bad payload", 400)
			return
		}
		b.mu.Lock()
		tunnel := b.tunnel
		b.mu.Unlock()
		if tunnel == nil {
			http.Error(w, "no tunnel", 409)
			return
		}
		ex := uplinkExchange{data: data, result: make(chan []byte, 1)}
		select {
		case tunnel.work <- ex:
		case <-r.Context().Done():
			return
		}
		select {
		case result := <-ex.result:
			_, _ = w.Write(result)
		case <-r.Context().Done():
			return
		}
	case "/close":
		b.mu.Lock()
		tunnel := b.tunnel
		b.mu.Unlock()
		if tunnel != nil {
			tunnel.once.Do(func() { close(tunnel.stop) })
		}
	default:
		http.NotFound(w, r)
	}
}
func serveUplinkBroker(t *testing.T, keys uplinkCredentials, brokerAddress, adminAddress string) (string, string) {
	t.Helper()
	pair, err := tls.X509KeyPair([]byte(keys.ServerCert), []byte(keys.ServerKey))
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM([]byte(keys.CA)) {
		t.Fatal("invalid fixture CA")
	}
	lis, err := net.Listen("tcp", brokerAddress)
	if err != nil {
		t.Fatal(err)
	}
	admin, err := net.Listen("tcp", adminAddress)
	if err != nil {
		lis.Close()
		t.Fatal(err)
	}
	server := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{pair}, ClientCAs: roots, ClientAuth: tls.RequireAndVerifyClientCert})))
	broker := &uplinkBroker{}
	cloudpbv2.RegisterTunnelBrokerServiceServer(server, broker)
	httpServer := &http.Server{Handler: broker, ReadHeaderTimeout: 2 * time.Second, ReadTimeout: 3 * time.Second, WriteTimeout: 4 * time.Second}
	done := make(chan struct{}, 2)
	go func() { _ = server.Serve(lis); done <- struct{}{} }()
	go func() { _ = httpServer.Serve(admin); done <- struct{}{} }()
	t.Cleanup(func() {
		server.Stop()
		_ = httpServer.Close()
		for range 2 {
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Error("broker server did not drain")
			}
		}
	})
	return lis.Addr().String(), "http://" + admin.Addr().String()
}
func TestBrokerUplinkNamespacePeer(t *testing.T) {
	path := os.Getenv("WENDY_UPLINK_KEYS")
	if os.Getenv("WENDY_UPLINK_PEER") != "1" || path == "" {
		t.Skip("private broker subprocess only")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var keys uplinkCredentials
	if err = json.Unmarshal(data, &keys); err != nil {
		t.Fatal(err)
	}
	serveUplinkBroker(t, keys, "203.0.113.1:18473", "10.81.1.2:18474")
	select {}
}

type uplinkAdmin struct {
	client *http.Client
	url    string
}

func newUplinkAdmin(address string) *uplinkAdmin {
	return &uplinkAdmin{client: &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{Proxy: nil}, CheckRedirect: func(*http.Request, []*http.Request) error { return fmt.Errorf("no redirects") }}, url: address}
}
func (a *uplinkAdmin) call(path string, data []byte) ([]byte, error) {
	resp, err := a.client.Post(a.url+path, "application/octet-stream", bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	result, err := io.ReadAll(io.LimitReader(resp.Body, 65537))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("admin %s: %d: %s", path, resp.StatusCode, result)
	}
	return result, nil
}
func (a *uplinkAdmin) state() (uplinkSnapshot, error) {
	data, err := a.call("/state", nil)
	var state uplinkSnapshot
	if err == nil {
		err = json.Unmarshal(data, &state)
	}
	return state, err
}
func awaitUplink(t *testing.T, d time.Duration, reason string, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(30 * time.Millisecond)
	}
	t.Fatal(reason)
}
func (a *uplinkAdmin) exchange(t *testing.T, marker string) {
	t.Helper()
	payload := bytes.Repeat([]byte(marker+"\x00"), 1024)
	got, err := a.call("/exchange", payload)
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("%s: established tunnel lost exact bytes: %v (%d/%d)", marker, err, len(got), len(payload))
	}
}
func (a *uplinkAdmin) open(t *testing.T, port int, id, source string) {
	t.Helper()
	data, _ := json.Marshal(map[string]any{"Port": port, "ID": id})
	if _, err := a.call("/open", data); err != nil {
		t.Fatal(err)
	}
	awaitUplink(t, 4*time.Second, "new tunnel did not use expected source "+source, func() bool { s, e := a.state(); return e == nil && s.TunnelsActive == 1 && s.TunnelSource == source })
}
func startUplinkAgent(t *testing.T, keys uplinkCredentials, address string) (*TunnelBrokerClient, int, context.CancelFunc, <-chan struct{}) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	connections := map[net.Conn]bool{}
	var wg sync.WaitGroup
	acceptDone := make(chan struct{})
	wg.Add(1)
	go func() {
		defer close(acceptDone)
		defer wg.Done()
		for {
			conn, e := listener.Accept()
			if e != nil {
				return
			}
			mu.Lock()
			connections[conn] = true
			mu.Unlock()
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer conn.Close()
				defer func() { mu.Lock(); delete(connections, conn); mu.Unlock() }()
				_, _ = io.Copy(conn, conn)
			}()
		}
	}()
	t.Cleanup(func() {
		listener.Close()
		// An accepted connection must be registered before cleanup snapshots the map.
		select {
		case <-acceptDone:
		case <-time.After(3 * time.Second):
			t.Error("local relay accept loop did not drain")
			return
		}
		mu.Lock()
		for c := range connections {
			c.Close()
		}
		mu.Unlock()
		done := make(chan struct{})
		go func() { wg.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("local relay target did not drain")
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	agent := NewTunnelBrokerClient(zap.NewExample(), address, keys.DeviceCert, keys.DeviceKey, keys.CA, 0)
	done := make(chan struct{})
	go func() { defer close(done); agent.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(4 * time.Second):
			t.Error("Agent Run did not drain")
		}
		awaitUplink(t, 3*time.Second, "agent sessions did not drain", func() bool { return agent.sessions.Load() == 0 })
	})
	return agent, listener.Addr().(*net.TCPAddr).Port, cancel, done
}

// Portable validation of actual TLS/presence/relay boundaries. It makes no
// route-migration claim and cannot substitute for the required namespace gate.
func TestBrokerUplinkProtocolControl(t *testing.T) {
	keys := makeUplinkCredentials(t, "127.0.0.1")
	address, admin := serveUplinkBroker(t, keys, "127.0.0.1:0", "127.0.0.1:0")
	a := newUplinkAdmin(admin)
	defer a.client.CloseIdleConnections()
	agent, port, cancel, done := startUplinkAgent(t, keys, address)
	awaitUplink(t, 4*time.Second, "TLS presence absent", func() bool {
		s, e := a.state()
		return e == nil && s.PresenceActive == 1 && s.PresenceSource == "127.0.0.1"
	})
	a.open(t, port, "portable", "127.0.0.1")
	a.exchange(t, "portable-runtime")
	_, err := a.call("/close", nil)
	if err != nil {
		t.Fatal(err)
	}
	awaitUplink(t, 3*time.Second, "session release absent", func() bool { return agent.sessions.Load() == 0 })
	cancel()
	<-done
	awaitUplink(t, 3*time.Second, "presence did not drain", func() bool { s, e := a.state(); return e == nil && s.PresenceActive == 0 && s.TunnelsActive == 0 })
}
func TestBrokerMovesToPreferredUplinkAfterSessionDrains(t *testing.T) {
	if os.Getenv("WENDY_UPLINK_NAMESPACE") != "1" {
		t.Skip("requires disposable hosted network namespace")
	}
	if runtime.GOOS != "linux" || os.Geteuid() != 0 {
		t.Fatal("invalid namespace environment")
	}
	keys := makeUplinkCredentials(t, "203.0.113.1")
	dir := t.TempDir()
	data, _ := json.Marshal(keys)
	keyPath := filepath.Join(dir, "keys.json")
	if err := os.WriteFile(keyPath, data, 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancelPeer := context.WithCancel(context.Background())
	defer cancelPeer()
	peerCmd := exec.CommandContext(ctx, "ip", "netns", "exec", "broker", os.Args[0], "-test.run=^TestBrokerUplinkNamespacePeer$", "-test.v", "-test.timeout=100s")
	peerCmd.Env = append(os.Environ(), "WENDY_UPLINK_PEER=1", "WENDY_UPLINK_KEYS="+keyPath)
	peerCmd.Stdout = os.Stdout
	peerCmd.Stderr = os.Stderr
	if err := peerCmd.Start(); err != nil {
		t.Fatal(err)
	}
	peerDone := make(chan error, 1)
	go func() { peerDone <- peerCmd.Wait() }()
	t.Cleanup(func() {
		cancelPeer()
		select {
		case <-peerDone:
		case <-time.After(4 * time.Second):
			t.Error("owned broker subprocess did not drain")
		}
	})
	a := newUplinkAdmin("http://10.81.1.2:18474")
	defer a.client.CloseIdleConnections()
	awaitUplink(t, 4*time.Second, "broker control endpoint unavailable", func() bool { _, e := a.state(); return e == nil })
	agent, port, cancel, done := startUplinkAgent(t, keys, "203.0.113.1:18473")
	awaitUplink(t, 4*time.Second, "initial Wi-Fi-model presence missing", func() bool {
		s, e := a.state()
		return e == nil && s.Registrations == 1 && s.PresenceActive == 1 && s.PresenceSource == "10.81.1.1"
	})
	a.open(t, port, "held-wifi-session", "10.81.1.1")
	a.exchange(t, "before-uplink-change")
	command := exec.Command("ip", "route", "replace", "203.0.113.1/32", "via", "10.81.2.2", "dev", "wired0", "src", "10.81.2.1", "metric", "5")
	if out, e := command.CombinedOutput(); e != nil {
		t.Fatalf("change isolated route: %v: %s", e, out)
	}
	source, e := preferredSource(&net.TCPAddr{IP: net.ParseIP("203.0.113.1"), Port: 18473})
	if e != nil || source.String() != "10.81.2.1" {
		t.Fatalf("kernel preferred source did not change: %v %v", source, e)
	}
	// Preserve the established TCP session beyond BOTH production path checks.
	// Its source remains Wi-Fi-model even though the kernel now routes via cable.
	until := time.Now().Add(2*brokerPathCheckInterval + 2*time.Second)
	for time.Now().Before(until) {
		a.exchange(t, "held-across-route-change")
		s, e := a.state()
		if e != nil || s.Registrations != 1 || s.PresenceActive != 1 || s.PresenceSource != "10.81.1.1" || agent.sessions.Load() != 1 {
			t.Fatalf("busy uplink migration interrupted established session: %+v %v", s, e)
		}
		time.Sleep(200 * time.Millisecond)
	}
	if _, e := a.call("/close", nil); e != nil {
		t.Fatal(e)
	}
	awaitUplink(t, 4*time.Second, "held session did not drain", func() bool { s, e := a.state(); return e == nil && s.TunnelsActive == 0 && agent.sessions.Load() == 0 })
	awaitUplink(t, 35*time.Second, "idle broker did not migrate to kernel-preferred wired source", func() bool {
		s, e := a.state()
		return e == nil && s.Registrations == 2 && s.PresenceActive == 1 && s.PresenceSource == "10.81.2.1"
	})
	a.open(t, port, "new-wired-session", "10.81.2.1")
	a.exchange(t, "after-wired-migration")
	if _, e := a.call("/close", nil); e != nil {
		t.Fatal(e)
	}
	awaitUplink(t, 4*time.Second, "wired session did not drain", func() bool { return agent.sessions.Load() == 0 })
	cancel()
	select {
	case <-done:
	case <-time.After(4 * time.Second):
		t.Fatal("Agent Run did not stop")
	}
	awaitUplink(t, 4*time.Second, "broker streams survived agent shutdown", func() bool { s, e := a.state(); return e == nil && s.PresenceActive == 0 && s.TunnelsActive == 0 })
	t.Log("held exact-byte TLS tunnel survived preferred uplink change; idle presence and new tunnel moved to wired source")
}
