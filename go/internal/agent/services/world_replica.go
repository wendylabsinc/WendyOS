package services

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"net"
	"os"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/agent/meshsession"
	"github.com/wendylabsinc/wendy/go/internal/agent/mtls"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	"github.com/wendylabsinc/wendy/go/world/production/agentmodule"
	replicav1 "github.com/wendylabsinc/wendy/go/world/production/api/world/replica/v1"
	"github.com/wendylabsinc/wendy/go/world/production/replication"
	"github.com/wendylabsinc/wendy/go/world/production/wendycert"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
)

// ReplicaService is registered only by the host's authenticated server. It is
// present before mesh startup but refuses requests until StartReplication.
func (m *AppWorldSocketManager) ReplicaService() replicav1.WorldReplicaServiceServer {
	if m == nil || m.module == nil {
		return nil
	}
	return m.module.ReplicaService()
}

// StartReplication adds bounded peer repair to an explicitly commissioned
// module. The host supplies its native routed dialer and authenticated mesh
// snapshot. Neither discovery addresses nor an app-supplied port are accepted.
func (m *AppWorldSocketManager) StartReplication(ctx context.Context, provisioning *ProvisioningService, dialer *MeshDialer, snapshot func() (localmesh.NodeSnapshot, bool)) error {
	if m == nil || m.module == nil || len(m.module.ReplicaDevices()) == 0 {
		return nil
	}
	if ctx == nil || ctx.Err() != nil || provisioning == nil || dialer == nil || snapshot == nil {
		return replication.ErrDenied
	}
	m.replicaMu.Lock()
	defer m.replicaMu.Unlock()
	if m.replicaClosed || m.replicaDone != nil {
		return replication.ErrDenied
	}
	identity := worldIdentity{provisioning}
	credential, err := identity.Snapshot(ctx)
	if err != nil {
		return err
	}
	if len(credential.ChainDER) == 0 {
		return replication.ErrDenied
	}
	leaf, err := x509.ParseCertificate(credential.ChainDER[0])
	if err != nil {
		return replication.ErrDenied
	}
	self, err := meshid.DeviceFromCert(leaf)
	if err != nil {
		return replication.ErrDenied
	}
	runCtx, cancel := context.WithCancel(ctx)
	host := &worldReplicaHost{ctx: runCtx, authenticate: m.module.AuthenticateReplicaPeer, current: identity.Current, credential: credential, self: self, snapshot: snapshot, dial: dialer.DialRoutedTarget}
	if err = m.module.StartReplication(runCtx, agentmodule.ReplicaTransport{
		IncomingPeer: func(callCtx context.Context) (replication.Identity, error) {
			return worldReplicaIncoming(callCtx, m.module.AuthenticateReplicaPeer)
		},
		Dial: host.connect,
	}); err != nil {
		cancel()
		return err
	}
	m.replicaCancel, m.replicaDone = cancel, make(chan struct{})
	done := m.replicaDone
	timing := os.Getenv("WENDY_WORLD_RPC_TIMING")
	go func() {
		defer close(done)
		var workers sync.WaitGroup
		workers.Add(3)
		go func() {
			defer workers.Done()
			worldReplicaForwardLatest(runCtx, m.module.ReplicaDevices(), m.module.ReplicaLatestWake(), m.module.TakeReplicaLatest, host.connect, func(ctx context.Context, device string, connection replication.Connection, items []replication.Item) (replication.Result, error) {
				result, err := m.module.SendReplicaLatest(ctx, device, connection, items)
				if result.UnknownOutcome {
					provisioning.logger.Debug("World latest forwarding outcome unknown", zap.Int("entries", result.Entries), zap.Int("bytes", result.Bytes))
				}
				return result, err
			})
		}()
		go func() {
			defer workers.Done()
			worldReplicaEvery(runCtx, 10*time.Second, func() {
				if err := m.module.RefreshReplication(runCtx); err != nil && runCtx.Err() == nil {
					provisioning.logger.Warn("World source authority refresh refused", zap.Error(err))
				}
			})
		}()
		go func() {
			defer workers.Done()
			worldReplicaEvery(runCtx, m.module.ReplicaSyncInterval(), func() {
				for _, device := range m.module.ReplicaDevices() {
					if runCtx.Err() != nil {
						return
					}
					// SyncPeer bounds each round and request. Rounds do not
					// overlap; lease refresh continues during network waits.
					var started time.Time
					if timing == "1" {
						started = time.Now()
					}
					result, err := m.module.SyncPeer(runCtx, device)
					if timing == "1" {
						worldReplicaLogRound(provisioning.logger, timing, result, err, time.Since(started))
					}
					if err != nil && runCtx.Err() == nil {
						provisioning.logger.Debug("World peer repair incomplete", zap.String("device", device), zap.Error(err))
					}
				}
			})
		}()
		select {
		case <-runCtx.Done():
		case <-credential.Invalidated:
			cancel()
		}
		workers.Wait()
	}()
	return nil
}

// Diagnostics use existing round counters and local elapsed time only. The
// normal logger's private Debug history owns delivery; no proof, peer identity,
// error text, authority timestamp or extra state read is recorded here.
func worldReplicaLogRound(logger *zap.Logger, enabled string, result replication.Result, err error, elapsed time.Duration) {
	if enabled != "1" || logger == nil {
		return
	}
	logger.Debug("World peer repair round", zap.Int("pages", result.Pages), zap.Int("entries", result.Entries),
		zap.Int("fetched", result.Fetched), zap.Int("admitted", result.Admitted),
		zap.Int("unavailable", result.Unavailable), zap.Int("bytes", result.Bytes),
		zap.Bool("complete", result.Complete), zap.Bool("deferred", result.Deferred),
		zap.Bool("unknown_outcome", result.UnknownOutcome), zap.Bool("failed", err != nil),
		zap.Int64("elapsed_ns", int64(elapsed)))
}

func worldReplicaEvery(ctx context.Context, interval time.Duration, fn func()) {
	for ctx.Err() == nil {
		fn()
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

// Forwarding owns a channel per commissioned peer. A slow peer cannot block
// another peer, state repair or source refresh. Take releases all local fences
// before returning owned originals; no local callback spans a network wait.
// A taken batch is consumed even on failure. Repair remains the independent
// fallback; this worker never requeues or replays an unknown send.
func worldReplicaForwardLatest(ctx context.Context, devices []string, wake <-chan struct{}, take func(context.Context, string) ([]replication.Item, error), connect func(context.Context, string) (replication.Connection, error), send func(context.Context, string, replication.Connection, []replication.Item) (replication.Result, error)) {
	// Match the manager's existing commissioned peer bound. Every worker is
	// canceled and joined below, including a call waiting on the network.
	if ctx == nil || len(devices) > 16 {
		return
	}
	ctx, cancel := context.WithCancel(ctx)
	var workers sync.WaitGroup
	defer func() { cancel(); workers.Wait() }()
	peerWake := make([]chan struct{}, len(devices))
	for i, device := range devices {
		pending := make(chan struct{}, 1)
		peerWake[i] = pending
		workers.Add(1)
		go func() {
			defer workers.Done()
			var connection replication.Connection
			closeConnection := func() {
				if connection.Close != nil {
					_ = connection.Close.Close()
				}
				connection = replication.Connection{}
			}
			defer closeConnection()
			for {
				select {
				case <-ctx.Done():
					return
				case <-pending:
				}
				for ctx.Err() == nil {
					localCtx, stop := context.WithTimeout(ctx, 2*time.Second)
					items, err := take(localCtx, device)
					stop()
					if err != nil || len(items) == 0 {
						break
					}
					if ctx.Err() != nil {
						return
					}
					if connection.Close == nil {
						dialCtx, stop := context.WithTimeout(ctx, 2*time.Second)
						connection, err = connect(dialCtx, device)
						stop()
						if err != nil {
							closeConnection()
							break
						}
					}
					if ctx.Err() != nil {
						return
					}
					rpcCtx, stop := context.WithTimeout(ctx, 2*time.Second)
					result, err := send(rpcCtx, device, connection, items)
					stop()
					if err != nil || result.UnknownOutcome {
						closeConnection()
						break
					}
				}
			}
		}()
	}
	// Publications may precede worker startup. Drain them once without waiting
	// for another publication, then use coalesced notifications only.
	notify := func() {
		for _, pending := range peerWake {
			select {
			case pending <- struct{}{}:
			default:
			}
		}
	}
	notify()
	for {
		select {
		case <-ctx.Done():
			return
		case _, ok := <-wake:
			if !ok {
				return
			}
			notify()
		}
	}
}

func (m *AppWorldSocketManager) stopReplication() {
	m.replicaMu.Lock()
	m.replicaClosed = true
	cancel, done := m.replicaCancel, m.replicaDone
	m.replicaMu.Unlock()
	if cancel != nil {
		cancel()
		<-done
	}
}

type worldReplicaAuthenticate func(context.Context, [][]byte) (replication.Identity, error)

func worldReplicaChain(certs []*x509.Certificate) ([][]byte, error) {
	if len(certs) == 0 || len(certs) > wendycert.MaxCertificates {
		return nil, replication.ErrDenied
	}
	out := make([][]byte, len(certs))
	total := 0
	for i, cert := range certs {
		if cert == nil || len(cert.Raw) == 0 || len(cert.Raw) > wendycert.MaxCertificateBytes-total {
			return nil, replication.ErrDenied
		}
		total += len(cert.Raw)
		out[i] = bytes.Clone(cert.Raw)
	}
	return out, nil
}

func worldReplicaIncoming(ctx context.Context, authenticate worldReplicaAuthenticate) (replication.Identity, error) {
	p, ok := peer.FromContext(ctx)
	if !ok || authenticate == nil || ctx.Err() != nil {
		return replication.Identity{}, replication.ErrDenied
	}
	info, ok := p.AuthInfo.(credentials.TLSInfo)
	if !ok || !info.State.HandshakeComplete || info.State.Version < tls.VersionTLS13 {
		return replication.Identity{}, replication.ErrDenied
	}
	chain, err := worldReplicaChain(info.State.PeerCertificates)
	if err != nil {
		return replication.Identity{}, err
	}
	// VerifiedChains can be empty for the host's ML-DSA custom verifier. The
	// module independently checks the original chain, exact peer, policy and
	// current authenticated time for this request.
	return authenticate(ctx, chain)
}

func worldReplicaPort(snapshot localmesh.NodeSnapshot, target meshid.Device) (uint16, error) {
	if !target.IsValid() || !snapshot.Self.IsValid() || snapshot.Self == target || snapshot.Self.Tenant() != target.Tenant() {
		return 0, replication.ErrDenied
	}
	var port uint16
	for _, device := range snapshot.Devices {
		if device.Device != target.UUID() {
			continue
		}
		if port != 0 || device.AgentPort == 0 || device.AgentTransport != localmesh.AgentMTLS {
			return 0, replication.ErrDenied
		}
		port = device.AgentPort
	}
	if port == 0 {
		return 0, replication.ErrDenied
	}
	if _, native, err := meshsession.ResolveAgentMTLS(snapshot, target, port); err != nil || !native {
		return 0, replication.ErrDenied
	}
	return port, nil
}

type worldReplicaHost struct {
	ctx          context.Context
	authenticate worldReplicaAuthenticate
	current      func(uint64) bool
	credential   agentmodule.IdentitySnapshot
	self         meshid.Device
	snapshot     func() (localmesh.NodeSnapshot, bool)
	dial         func(context.Context, DeliveryTarget, uint16) (net.Conn, string, error)
}

type worldReplicaPeer struct {
	mu       sync.Mutex
	identity replication.Identity
}

// State repair and on-demand evidence share this native authenticated dialer.
// Host credentials remain in-process. The existing native mTLS verifier pins
// the full SPIFFE device principal; World additionally checks its private roots,
// peer fingerprint, deny policy and authenticated physical interval. No session
// cache is shared with other native clients.
func worldReplicaTLS(ctx context.Context, credential agentmodule.IdentitySnapshot, target string, current func(uint64) bool, authenticate worldReplicaAuthenticate) (*tls.Config, *worldReplicaPeer, error) {
	if credential.Key == nil || len(credential.ChainDER) == 0 || credential.Invalidated == nil || current == nil || authenticate == nil || !current(credential.Token) {
		return nil, nil, replication.ErrDenied
	}
	want, err := meshid.DeviceFromPrincipal(target)
	if err != nil || want.Principal() != target {
		return nil, nil, replication.ErrDenied
	}
	state := &worldReplicaPeer{}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: credential.ChainDER[0]})
	var chainPEM []byte
	for _, der := range credential.ChainDER[1:] {
		chainPEM = append(chainPEM, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})...)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(credential.Key)
	if err != nil {
		return nil, nil, replication.ErrDenied
	}
	defer clear(keyDER)
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	defer clear(keyPEM)
	cfg, err := mtls.NewClientTLSConfigExpectingDevice(string(certPEM), string(chainPEM), string(keyPEM), zap.NewNop(), want)
	if err != nil || cfg.VerifyConnection == nil || cfg.VerifyPeerCertificate == nil {
		return nil, nil, replication.ErrDenied
	}
	cfg.MinVersion = tls.VersionTLS13
	cfg.ClientSessionCache = nil
	nativeVerify := cfg.VerifyConnection
	cfg.VerifyConnection = func(cs tls.ConnectionState) error {
		if ctx.Err() != nil || !current(credential.Token) || cs.Version < tls.VersionTLS13 {
			return replication.ErrDenied
		}
		if err := nativeVerify(cs); err != nil {
			return err
		}
		chain, err := worldReplicaChain(cs.PeerCertificates)
		if err != nil {
			return err
		}
		authCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		id, err := authenticate(authCtx, chain)
		if err != nil || id.Device != target || id.BindingSHA256 == "" || !current(credential.Token) || authCtx.Err() != nil {
			return replication.ErrDenied
		}
		state.mu.Lock()
		defer state.mu.Unlock()
		if state.identity.Device != "" && state.identity != id {
			return replication.ErrDenied
		}
		state.identity = id
		return nil
	}
	return cfg, state, nil
}

type worldReplicaConnection struct {
	*grpc.ClientConn
	cancel context.CancelFunc
	once   sync.Once
	err    error
}

func (c *worldReplicaConnection) Close() error {
	c.once.Do(func() {
		c.cancel()
		c.err = c.ClientConn.Close()
	})
	return c.err
}

func (h *worldReplicaHost) connect(ctx context.Context, principal string) (replication.Connection, error) {
	device, err := meshid.DeviceFromPrincipal(principal)
	if err != nil || h.ctx == nil || ctx == nil || !h.current(h.credential.Token) {
		return replication.Connection{}, replication.ErrDenied
	}
	// The replica service cancels its dial-only context immediately after
	// establishment. A returned connection belongs to this host lifetime and
	// its caller's Close, while each later RPC has its own bounded context.
	callCtx, cancel := context.WithCancel(h.ctx)
	dialCtx, cancelDial := context.WithCancel(ctx)
	stopDial := context.AfterFunc(callCtx, cancelDial)
	defer stopDial()
	defer cancelDial()
	failed := true
	defer func() {
		if failed {
			cancel()
		}
	}()
	go func() {
		select {
		case <-h.credential.Invalidated:
			cancel()
		case <-callCtx.Done():
		}
	}()
	tlsConfig, authenticated, err := worldReplicaTLS(callCtx, h.credential, principal, h.current, h.authenticate)
	if err != nil {
		return replication.Connection{}, err
	}
	conn, err := grpc.DialContext(dialCtx, "passthrough:///world-replica", grpc.WithBlock(), grpc.WithTransportCredentials(credentials.NewTLS(tlsConfig)), grpc.WithContextDialer(func(dialCtx context.Context, _ string) (net.Conn, error) {
		view, ok := h.snapshot()
		if !ok || view.Self != h.self || !h.current(h.credential.Token) {
			return nil, replication.ErrDenied
		}
		port, err := worldReplicaPort(view, device)
		if err != nil {
			return nil, err
		}
		raw, transport, err := h.dial(dialCtx, DeliveryTarget{Device: device}, port)
		if err != nil {
			return nil, err
		}
		if raw == nil {
			return nil, replication.ErrDenied
		}
		// Recheck signed endpoint admission after the route dial waited.
		latest, ok := h.snapshot()
		latestPort, checkErr := worldReplicaPort(latest, device)
		if !ok || latest.Self != h.self || transport != "local-mesh" || checkErr != nil || latestPort != port || !h.current(h.credential.Token) {
			raw.Close()
			return nil, replication.ErrDenied
		}
		return raw, nil
	}), grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(replication.MaxPageBytes), grpc.MaxCallSendMsgSize(replication.MaxPageBytes)))
	if err != nil {
		return replication.Connection{}, err
	}
	owned := &worldReplicaConnection{ClientConn: conn, cancel: cancel}
	context.AfterFunc(callCtx, func() { owned.Close() })
	authenticated.mu.Lock()
	id := authenticated.identity
	authenticated.mu.Unlock()
	if id.Device != principal || callCtx.Err() != nil || !h.current(h.credential.Token) {
		owned.Close()
		return replication.Connection{}, replication.ErrDenied
	}
	failed = false
	return replication.Connection{Identity: id, Client: replicav1.NewWorldReplicaServiceClient(conn), Close: owned}, nil
}
