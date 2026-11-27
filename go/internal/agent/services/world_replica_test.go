package services

import (
	"bytes"
	"context"
	"crypto/mldsa"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"net/netip"
	"net/url"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/wendylabsinc/WendyOS/go/babel"
	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/agent/mtls"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	"github.com/wendylabsinc/wendy/go/world/production/agentmodule"
	replicav1 "github.com/wendylabsinc/wendy/go/world/production/api/world/replica/v1"
	"github.com/wendylabsinc/wendy/go/world/production/replication"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
)

const worldReplicaTestTenant = "11111111-1111-4111-8111-111111111111"

func TestWorldReplicaRoundDiagnosticsRequireExactOptInAndExposeOnlyCounters(t *testing.T) {
	core, logs := observer.New(zap.DebugLevel)
	logger := zap.New(core)
	result := replication.Result{Pages: 1, Entries: 4, Fetched: 3, Admitted: 2, Unavailable: 1, Bytes: 4096, Deferred: true, UnknownOutcome: true}
	err := errors.New("private error must not be logged in the counter event")
	for _, disabled := range []string{"", "0", "true", "01", " 1"} {
		worldReplicaLogRound(logger, disabled, result, err, 37*time.Millisecond)
	}
	if logs.Len() != 0 {
		t.Fatal("replica diagnostics enabled without exact opt-in")
	}
	worldReplicaLogRound(logger, "1", result, err, 37*time.Millisecond)
	events := logs.All()
	want := map[string]any{"pages": int64(1), "entries": int64(4), "fetched": int64(3), "admitted": int64(2), "unavailable": int64(1), "bytes": int64(4096), "complete": false, "deferred": true, "unknown_outcome": true, "failed": true, "elapsed_ns": int64(37 * time.Millisecond)}
	if len(events) != 1 || events[0].Message != "World peer repair round" || !reflect.DeepEqual(events[0].ContextMap(), want) {
		t.Fatal("repair counter event changed", events)
	}
}

func TestWorldReplicaLiveCadenceWaitsAfterRoundAndCancels(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		const interval = 250 * time.Millisecond
		release := make(chan struct{})
		started := make(chan time.Time, 4)
		done := make(chan struct{})
		var calls atomic.Int32
		go func() {
			defer close(done)
			worldReplicaEvery(ctx, interval, func() {
				started <- time.Now()
				if calls.Add(1) == 1 {
					<-release
				}
			})
		}()
		start := time.Now()
		synctest.Wait()
		if first := <-started; !first.Equal(start) {
			t.Fatal("first repair round was delayed")
		}
		// A round lasting longer than the cadence must never overlap a new one.
		time.Sleep(4 * interval)
		if calls.Load() != 1 {
			t.Fatal("repair rounds overlapped")
		}
		finished := time.Now()
		close(release)
		synctest.Wait()
		time.Sleep(interval - time.Nanosecond)
		if calls.Load() != 1 {
			t.Fatal("pause was measured from round start instead of completion")
		}
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		if second := <-started; second.Sub(finished) != interval || calls.Load() != 2 {
			t.Fatal("live repair did not resume after its bounded pause")
		}
		cancel()
		synctest.Wait()
		select {
		case <-done:
		default:
			t.Fatal("cancellation did not stop the waiting scheduler")
		}
		time.Sleep(4 * interval)
		if calls.Load() != 2 {
			t.Fatal("repair continued after cancellation")
		}
	})
}

func worldReplicaTestDevice(t *testing.T, id string) meshid.Device {
	t.Helper()
	d, err := meshid.DeviceFromPrincipal("spiffe://wendy.sh/tenant/" + worldReplicaTestTenant + "/device/" + id)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestWorldReplicaPortRequiresSignedNativeEndpoint(t *testing.T) {
	self := worldReplicaTestDevice(t, "22222222-2222-4222-8222-222222222222")
	target := worldReplicaTestDevice(t, "33333333-3333-4333-8333-333333333333")
	base := func() localmesh.NodeSnapshot {
		return localmesh.NodeSnapshot{Self: self,
			ForwardingRoutes: []babel.Route{{Prefix: netip.PrefixFrom(target.MeshAddress(), 128), Link: 1}},
			Devices:          []localmesh.Manifest{{Device: target.UUID(), AgentTransport: localmesh.AgentMTLS, AgentPort: 5054, AdmittedUntil: time.Now().Add(time.Minute)}},
		}
	}
	if port, err := worldReplicaPort(base(), target); err != nil || port != 5054 {
		t.Fatal(port, err)
	}
	for name, change := range map[string]func(*localmesh.NodeSnapshot){
		"unknown self":       func(s *localmesh.NodeSnapshot) { s.Self = meshid.Device{} },
		"missing route":      func(s *localmesh.NodeSnapshot) { s.ForwardingRoutes = []babel.Route{} },
		"unreachable route":  func(s *localmesh.NodeSnapshot) { s.ForwardingRoutes[0].Unreachable = true },
		"withdrawn":          func(s *localmesh.NodeSnapshot) { s.Devices[0].Withdraw = true },
		"expired":            func(s *localmesh.NodeSnapshot) { s.Devices[0].AdmittedUntil = time.Now().Add(-time.Second) },
		"app TLS only":       func(s *localmesh.NodeSnapshot) { s.Devices[0].AgentTransport = localmesh.AppTLS },
		"no service":         func(s *localmesh.NodeSnapshot) { s.Devices[0].AgentPort = 0 },
		"ambiguous manifest": func(s *localmesh.NodeSnapshot) { s.Devices = append(s.Devices, s.Devices[0]) },
	} {
		t.Run(name, func(t *testing.T) {
			s := base()
			change(&s)
			if _, err := worldReplicaPort(s, target); err == nil {
				t.Fatal("unauthorized native port accepted")
			}
		})
	}
}

func TestWorldReplicaIncomingUsesOnlyCompletedTLSState(t *testing.T) {
	ctx := context.Background()
	called := 0
	auth := func(_ context.Context, chain [][]byte) (replication.Identity, error) {
		called++
		if len(chain) != 1 || !bytes.Equal(chain[0], []byte("original")) {
			t.Fatal("changed peer chain")
		}
		chain[0][0] = 'X'
		return replication.Identity{Device: "test", BindingSHA256: "test"}, nil
	}
	for _, info := range []credentials.TLSInfo{
		{},
		{State: tls.ConnectionState{Version: tls.VersionTLS12, HandshakeComplete: true}},
		{State: tls.ConnectionState{Version: tls.VersionTLS13, HandshakeComplete: true}},
	} {
		if _, err := worldReplicaIncoming(peer.NewContext(ctx, &peer.Peer{AuthInfo: info}), auth); err == nil {
			t.Fatal("incomplete TLS state accepted")
		}
	}
	if _, err := worldReplicaIncoming(ctx, auth); err == nil || called != 0 {
		t.Fatal("unauthenticated caller reached authority")
	}
	raw := []byte("original")
	info := credentials.TLSInfo{State: tls.ConnectionState{Version: tls.VersionTLS13, HandshakeComplete: true, PeerCertificates: []*x509.Certificate{{Raw: raw}}}}
	if _, err := worldReplicaIncoming(peer.NewContext(ctx, &peer.Peer{AuthInfo: info}), auth); err != nil || called != 1 || string(raw) != "original" {
		t.Fatal("lost or aliased peer certificate", err)
	}
}

func worldReplicaTestCredentials(t *testing.T) (agentmodule.IdentitySnapshot, agentmodule.IdentitySnapshot, string) {
	t.Helper()
	now := time.Now()
	rootKey, err := mldsa.GenerateKey(mldsa.MLDSA65())
	if err != nil {
		t.Fatal(err)
	}
	root := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "PRIVATE TEST ROOT"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), BasicConstraintsValid: true, IsCA: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
	rootDER, err := x509.CreateCertificate(rand.Reader, root, root, rootKey.Public(), rootKey)
	if err != nil {
		t.Fatal(err)
	}
	root, err = x509.ParseCertificate(rootDER)
	if err != nil {
		t.Fatal(err)
	}
	makeLeaf := func(serial int64, device string) agentmodule.IdentitySnapshot {
		key, err := mldsa.GenerateKey(mldsa.MLDSA65())
		if err != nil {
			t.Fatal(err)
		}
		u, err := url.Parse(device)
		if err != nil {
			t.Fatal(err)
		}
		leaf := &x509.Certificate{SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: "PRIVATE TEST DEVICE"}, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(10 * time.Minute), BasicConstraintsValid: true, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth}, URIs: []*url.URL{u}}
		der, err := x509.CreateCertificate(rand.Reader, leaf, root, key.Public(), rootKey)
		if err != nil {
			t.Fatal(err)
		}
		return agentmodule.IdentitySnapshot{Token: 1, ChainDER: [][]byte{der, rootDER}, Key: key, Invalidated: make(chan struct{})}
	}
	self := worldReplicaTestDevice(t, "22222222-2222-4222-8222-222222222222").Principal()
	target := worldReplicaTestDevice(t, "33333333-3333-4333-8333-333333333333").Principal()
	return makeLeaf(2, self), makeLeaf(3, target), target
}

func TestWorldReplicaTLSAuthenticatesNativeDeviceAndCurrentWorldPeer(t *testing.T) {
	self, remote, target := worldReplicaTestCredentials(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var current atomic.Bool
	current.Store(true)
	var called atomic.Int32
	auth := func(_ context.Context, chain [][]byte) (replication.Identity, error) {
		called.Add(1)
		// The native server may omit its root from the certificate flight.
		// The caller and World pin that root independently.
		if len(chain) == 0 || !bytes.Equal(chain[0], remote.ChainDER[0]) {
			return replication.Identity{}, replication.ErrDenied
		}
		return replication.Identity{Device: target, BindingSHA256: "commissioned-peer"}, nil
	}
	cfg, state, err := worldReplicaTLS(ctx, self, target, func(uint64) bool { return current.Load() }, auth)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ClientSessionCache != nil || cfg.MinVersion != tls.VersionTLS13 {
		t.Fatal("unexpected TLS policy")
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(remote.Key)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(keyDER)
	serverCfg, err := mtls.NewTLSConfig(string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: remote.ChainDER[0]})), string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: remote.ChainDER[1]})), string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})), zap.NewNop(), time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	serverDone := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			serverDone <- err
			return
		}
		defer conn.Close()
		serverDone <- tls.Server(conn, serverCfg).HandshakeContext(ctx)
	}()
	b, err := (&net.Dialer{}).DialContext(ctx, "tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	// Match credentials.NewTLS, which negotiates HTTP/2 for the real RPC path.
	cfg.NextProtos = []string{"h2"}
	client := tls.Client(b, cfg)
	if err := client.HandshakeContext(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
	state.mu.Lock()
	id := state.identity
	state.mu.Unlock()
	if id.Device != target || called.Load() == 0 {
		t.Fatal("native TLS skipped World commissioning")
	}
	current.Store(false)
	if cfg.VerifyConnection(client.ConnectionState()) == nil {
		t.Fatal("invalidated local identity kept authority")
	}
	current.Store(true)
	other, err := x509.ParseCertificate(self.ChainDER[0])
	if err != nil {
		t.Fatal(err)
	}
	if cfg.VerifyConnection(tls.ConnectionState{Version: tls.VersionTLS13, PeerCertificates: []*x509.Certificate{other}}) == nil {
		t.Fatal("wrong native device accepted")
	}
	if cfg.VerifyPeerCertificate(self.ChainDER, nil) == nil {
		t.Fatal("native principal pin accepted the caller as its target")
	}
}

func TestWorldReplicaTLSRechecksIdentityAfterAuthorization(t *testing.T) {
	self, remote, target := worldReplicaTestCredentials(t)
	current := true
	cfg, _, err := worldReplicaTLS(context.Background(), self, target, func(uint64) bool { return current }, func(_ context.Context, _ [][]byte) (replication.Identity, error) {
		current = false
		return replication.Identity{Device: target, BindingSHA256: "commissioned-peer"}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var certs []*x509.Certificate
	for _, der := range remote.ChainDER {
		cert, err := x509.ParseCertificate(der)
		if err != nil {
			t.Fatal(err)
		}
		certs = append(certs, cert)
	}
	if cfg.VerifyConnection(tls.ConnectionState{Version: tls.VersionTLS13, PeerCertificates: certs}) == nil {
		t.Fatal("identity changed during World authorization")
	}
}

type worldReplicaTestServer struct {
	replicav1.UnimplementedWorldReplicaServiceServer
	client []byte
}

func (s worldReplicaTestServer) Inventory(ctx context.Context, _ *replicav1.InventoryRequest) (*replicav1.InventoryReply, error) {
	_, err := worldReplicaIncoming(ctx, func(_ context.Context, chain [][]byte) (replication.Identity, error) {
		if !bytes.Equal(chain[0], s.client) {
			return replication.Identity{}, replication.ErrDenied
		}
		return replication.Identity{Device: "authenticated test client", BindingSHA256: "test"}, nil
	})
	if err != nil {
		return nil, err
	}
	return &replicav1.InventoryReply{Version: 1}, nil
}

func (s worldReplicaTestServer) FetchEvidence(ctx context.Context, q *replicav1.FetchEvidenceRequest) (*replicav1.FetchEvidenceReply, error) {
	if _, err := s.Inventory(ctx, nil); err != nil {
		return nil, err
	}
	return &replicav1.FetchEvidenceReply{Version: 1, Sha256: q.Sha256, Offset: q.Offset, Data: []byte("native media range")}, nil
}

func (s worldReplicaTestServer) PushLatest(ctx context.Context, q *replicav1.PushLatestRequest) (*replicav1.PushLatestReply, error) {
	if _, err := s.Inventory(ctx, nil); err != nil {
		return nil, err
	}
	if len(q.Items) != 1 || !bytes.Equal(q.Items[0].Envelope, []byte("unchanged latest original")) {
		return nil, replication.ErrDenied
	}
	return &replicav1.PushLatestReply{Version: 1, Admitted: 1, Complete: true}, nil
}

// Real native mTLS and gRPC run over a test-local stream. Routing is injected;
// this establishes connection lifetime, not a deployed mesh path.
func TestWorldReplicaConnectionOutlivesDialContext(t *testing.T) {
	for _, end := range []string{"host", "identity", "close"} {
		t.Run(end, func(t *testing.T) {
			self, remote, target := worldReplicaTestCredentials(t)
			keyDER, err := x509.MarshalPKCS8PrivateKey(remote.Key)
			if err != nil {
				t.Fatal(err)
			}
			defer clear(keyDER)
			serverTLS, err := mtls.NewTLSConfig(string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: remote.ChainDER[0]})), string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: remote.ChainDER[1]})), string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})), zap.NewNop(), time.Time{})
			if err != nil {
				t.Fatal(err)
			}
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			server := grpc.NewServer(grpc.Creds(credentials.NewTLS(serverTLS)))
			replicav1.RegisterWorldReplicaServiceServer(server, worldReplicaTestServer{client: self.ChainDER[0]})
			defer server.Stop()
			go server.Serve(listener)
			hostCtx, cancelHost := context.WithCancel(context.Background())
			defer cancelHost()
			invalidated := make(chan struct{})
			self.Invalidated = invalidated
			var current atomic.Bool
			current.Store(true)
			own := worldReplicaTestDevice(t, "22222222-2222-4222-8222-222222222222")
			want, err := meshid.DeviceFromPrincipal(target)
			if err != nil {
				t.Fatal(err)
			}
			host := &worldReplicaHost{ctx: hostCtx, credential: self, self: own, current: func(uint64) bool { return current.Load() },
				authenticate: func(_ context.Context, chain [][]byte) (replication.Identity, error) {
					if !bytes.Equal(chain[0], remote.ChainDER[0]) {
						return replication.Identity{}, replication.ErrDenied
					}
					return replication.Identity{Device: target, BindingSHA256: "commissioned-test-peer"}, nil
				},
				snapshot: func() (localmesh.NodeSnapshot, bool) {
					return localmesh.NodeSnapshot{Self: own, ForwardingRoutes: []babel.Route{{Prefix: netip.PrefixFrom(want.MeshAddress(), 128), Link: 1}}, Devices: []localmesh.Manifest{{Device: want.UUID(), AgentTransport: localmesh.AgentMTLS, AgentPort: 5054, AdmittedUntil: time.Now().Add(time.Minute)}}}, true
				},
				dial: func(ctx context.Context, got DeliveryTarget, port uint16) (net.Conn, string, error) {
					if got.Device != want || port != 5054 {
						return nil, "", replication.ErrDenied
					}
					conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", listener.Addr().String())
					return conn, "local-mesh", err
				},
			}
			dialCtx, cancelDial := context.WithTimeout(context.Background(), 5*time.Second)
			conn, err := host.connect(dialCtx, target)
			cancelDial() // replication.SyncPeer does this immediately on return.
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close.Close()
			rpcCtx, cancelRPC := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancelRPC()
			reply, err := conn.Client.Inventory(rpcCtx, &replicav1.InventoryRequest{Version: 1})
			if err != nil || reply.Version != 1 {
				t.Fatal("dial context canceled returned connection", err)
			}
			evidence, err := conn.Client.FetchEvidence(rpcCtx, &replicav1.FetchEvidenceRequest{Version: 1, Sha256: "original digest", Offset: 48 << 10, Length: 18})
			if err != nil || evidence.Sha256 != "original digest" || evidence.Offset != 48<<10 || string(evidence.Data) != "native media range" {
				t.Fatal("native authenticated connection lost evidence range", err)
			}
			latest, err := conn.Client.PushLatest(rpcCtx, &replicav1.PushLatestRequest{Version: 1, Items: []*replicav1.Item{{Envelope: []byte("unchanged latest original")}}})
			if err != nil || latest.Admitted != 1 || !latest.Complete {
				t.Fatal("native authenticated connection lost latest original", err)
			}
			switch end {
			case "host":
				cancelHost()
			case "identity":
				current.Store(false)
				close(invalidated)
			case "close":
				if err := conn.Close.Close(); err != nil {
					t.Fatal(err)
				}
			}
			deadline := time.Now().Add(time.Second)
			for {
				probeCtx, stop := context.WithTimeout(context.Background(), 100*time.Millisecond)
				_, err = conn.Client.Inventory(probeCtx, &replicav1.InventoryRequest{Version: 1})
				stop()
				if err != nil {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("ended host identity or owner kept transport open")
				}
				time.Sleep(time.Millisecond)
			}
			if err := conn.Close.Close(); err != nil {
				t.Fatal("connection close was not idempotent", err)
			}
		})
	}
}

type worldReplicaCloseCounter struct{ count *atomic.Int32 }

func (c worldReplicaCloseCounter) Close() error { c.count.Add(1); return nil }

func TestWorldReplicaLatestPeersProgressIndependentlyAndReuseChannels(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		wake, done, release := make(chan struct{}, 1), make(chan struct{}), make(chan struct{})
		var queueMu sync.Mutex
		queue := map[string][]replication.Item{
			"slow": {{Envelope: []byte("first")}},
			"fast": {{Envelope: []byte("first")}},
		}
		take := func(_ context.Context, device string) ([]replication.Item, error) {
			queueMu.Lock()
			defer queueMu.Unlock()
			items := queue[device]
			delete(queue, device)
			return items, nil
		}
		var connects, closes atomic.Int32
		checkReleased := func() {
			// Another peer may legitimately be taking its own batch here.
			// Wait for that short critical section. A caller retaining the
			// queue lock across network work instead deadlocks this synctest
			// bubble, so the test still detects the ownership violation.
			queueMu.Lock()
			queueMu.Unlock()
		}
		connect := func(_ context.Context, device string) (replication.Connection, error) {
			checkReleased()
			connects.Add(1)
			return replication.Connection{Identity: replication.Identity{Device: device}, Close: worldReplicaCloseCounter{&closes}}, nil
		}
		sent := make(chan string, 4)
		send := func(ctx context.Context, device string, connection replication.Connection, items []replication.Item) (replication.Result, error) {
			checkReleased()
			if connection.Identity.Device != device {
				t.Error("connection crossed commissioned peers")
			}
			value := string(items[0].Envelope)
			sent <- device + "/" + value
			if device == "slow" && value == "first" {
				select {
				case <-ctx.Done():
					return replication.Result{}, ctx.Err()
				case <-release:
				}
			}
			return replication.Result{Complete: true}, nil
		}
		go func() {
			defer close(done)
			worldReplicaForwardLatest(ctx, []string{"slow", "fast"}, wake, take, connect, send)
		}()
		synctest.Wait()
		if len(sent) != 2 || connects.Load() != 2 {
			t.Fatal("startup batch or independent peer was blocked")
		}
		seen := map[string]bool{<-sent: true, <-sent: true}
		if !seen["slow/first"] || !seen["fast/first"] {
			t.Fatal("wrong initial originals")
		}
		queueMu.Lock()
		queue["slow"] = []replication.Item{{Envelope: []byte("new")}}
		queueMu.Unlock()
		wake <- struct{}{}
		synctest.Wait()
		if len(sent) != 0 {
			t.Fatal("one peer's forwarding calls overlapped")
		}
		close(release)
		synctest.Wait()
		if len(sent) != 1 || <-sent != "slow/new" || connects.Load() != 2 {
			t.Fatal("new original did not reuse owned peer channel")
		}
		cancel()
		synctest.Wait()
		<-done
		if closes.Load() != 2 {
			t.Fatal("shutdown did not close each owned channel exactly once")
		}
	})
}

func TestWorldReplicaLatestUnknownIsConsumedAndOnlyNewBatchReconnects(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		wake, done := make(chan struct{}, 1), make(chan struct{})
		var mu sync.Mutex
		pending := []replication.Item{{Envelope: []byte("unknown")}}
		take := func(context.Context, string) ([]replication.Item, error) {
			mu.Lock()
			defer mu.Unlock()
			items := pending
			pending = nil
			return items, nil
		}
		var connects, closes atomic.Int32
		connect := func(context.Context, string) (replication.Connection, error) {
			connects.Add(1)
			return replication.Connection{Close: worldReplicaCloseCounter{&closes}}, nil
		}
		var sent []string
		send := func(_ context.Context, _ string, _ replication.Connection, items []replication.Item) (replication.Result, error) {
			sent = append(sent, string(items[0].Envelope))
			if len(sent) == 1 {
				return replication.Result{UnknownOutcome: true}, errors.New("acknowledgment lost")
			}
			return replication.Result{Complete: true}, nil
		}
		go func() {
			defer close(done)
			worldReplicaForwardLatest(ctx, []string{"peer"}, wake, take, connect, send)
		}()
		synctest.Wait()
		wake <- struct{}{}
		synctest.Wait()
		if len(sent) != 1 || closes.Load() != 1 || connects.Load() != 1 {
			t.Fatal("unknown batch was replayed or connection retained")
		}
		mu.Lock()
		pending = []replication.Item{{Envelope: []byte("fresh")}}
		mu.Unlock()
		wake <- struct{}{}
		synctest.Wait()
		if !reflect.DeepEqual(sent, []string{"unknown", "fresh"}) || connects.Load() != 2 {
			t.Fatal("fresh batch did not get a new channel")
		}
		close(wake)
		synctest.Wait()
		<-done
		if closes.Load() != 2 {
			t.Fatal("closed notification source leaked an owned channel")
		}
	})
}

func TestWorldReplicaLatestNetworkWaitIsBoundedAndShutdownJoins(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		wake, done := make(chan struct{}, 1), make(chan struct{})
		pending := true
		take := func(context.Context, string) ([]replication.Item, error) {
			if !pending {
				return nil, nil
			}
			pending = false
			return []replication.Item{{Envelope: []byte("one")}}, nil
		}
		var closes, sends atomic.Int32
		connect := func(context.Context, string) (replication.Connection, error) {
			return replication.Connection{Close: worldReplicaCloseCounter{&closes}}, nil
		}
		send := func(ctx context.Context, _ string, _ replication.Connection, _ []replication.Item) (replication.Result, error) {
			sends.Add(1)
			<-ctx.Done()
			return replication.Result{UnknownOutcome: true}, ctx.Err()
		}
		go func() {
			defer close(done)
			worldReplicaForwardLatest(ctx, []string{"peer"}, wake, take, connect, send)
		}()
		synctest.Wait()
		time.Sleep(2 * time.Second)
		synctest.Wait()
		if sends.Load() != 1 || closes.Load() != 1 {
			t.Fatal("latest-value RPC exceeded its fixed budget or was retried")
		}
		cancel()
		synctest.Wait()
		<-done
	})
}
