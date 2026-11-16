package meshsession

import (
	"context"
	"crypto/x509"
	"errors"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/agent/udpmessage"
	"github.com/wendylabsinc/wendy/go/internal/shared/directstream"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

type DirectManagerConfig struct {
	// Share the node UDP ledger with ordinary clients/backends when supplied.
	UDPManager *udpmessage.Manager
	// PromotionSnapshot enables mandatory automatic promotion. It must return a
	// concurrency-safe snapshot of the current node generation; an empty snapshot
	// while the node is unavailable prevents dialing. It is also rechecked after TLS.
	PromotionSnapshot func() localmesh.NodeSnapshot
	// PromotionDialContext supplies connected TCP or UDP sockets from a userspace
	// mesh router. Nil uses the platform mesh sockets. TLS, peer pinning, route
	// revalidation and socket lifetime remain owned by this manager.
	PromotionDialContext func(context.Context, string, string) (net.Conn, error)
	// PromotionCheck is a nonblocking local policy check, called before dialing
	// and again after authentication before sending WDC1. Automatic runtime owners
	// must resolve a fresh installed route here; nil permits explicit manual dials.
	PromotionCheck func(meshid.Device, netip.AddrPort, uint32, DirectSessionStatus) error
	Credentials    *localmesh.Credentials
	Backend        DirectBackend
	// NativeBackend receives the original authenticated epoch identity. It must
	// enforce its normal service policy; app TCP/UDP continue through Backend.
	NativeBackend                         func(context.Context, DirectNativeIdentity, directstream.OpenRequest) (net.Conn, error)
	MaxPeers, FlowSlots, TXBytes, RXBytes int
	Window, Initial                       uint64
	RecoveryTimeout                       time.Duration
	// Both zero disables direct UDP; configured bytes reserve per-peer storage.
	DatagramBytes   int
	DatagramMaximum uint16
}
type managedDirectSession struct {
	promotionDial      bool // guarded by DirectManager.mu
	automaticPending   bool
	promotionRetry     time.Time
	session            *DirectSession
	peerCertificateDER []byte
	nativeAuth         *directNativeAuth
	selected           uint32
	mu                 sync.Mutex
	original, latest   *sessionLifetime
	deadline           time.Time
}

func (e *managedDirectSession) lifetime() (time.Time, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, l := range []*sessionLifetime{e.original, e.latest} {
		deadline, err := l.current()
		if err != nil {
			return time.Time{}, err
		}
		e.deadline = minTime(e.deadline, deadline)
	}
	return e.deadline, nil
}
func (e *managedDirectSession) replaceLifetime(l *sessionLifetime, deadline time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.latest = l
	e.deadline = minTime(e.deadline, deadline)
}

// DirectManager owns retained sessions independently of physical carrier
// lifetimes. Create a new manager whenever local credentials, trust or mesh
// policy changes, and join Close before discarding the old generation.
// Installing HandleCarrier on a Node is permitted only once the negotiated
// promotion paths are wired by the enclosing runtime.
type DirectManager struct {
	udpManager       *udpmessage.Manager
	ownsUDPManager   bool
	cfg              DirectManagerConfig
	ctx              context.Context
	cancel           context.CancelFunc
	mu               sync.Mutex
	peers            map[meshid.Device]*managedDirectSession
	admissions       map[meshid.Device]*directPeerAdmission
	closed           bool
	carrierBound     bool
	done             chan struct{}
	workers          sync.WaitGroup
	promotionMu      sync.Mutex
	promotionPending map[DirectPromotionRequest]*pendingPromotionFlow
	promotionChanged chan struct{}
}

func NewDirectManager(parent context.Context, cfg DirectManagerConfig) (*DirectManager, error) {
	if cfg.Credentials == nil || cfg.Backend == nil {
		return nil, errors.New("missing direct manager identity or backend")
	}
	if cfg.MaxPeers == 0 {
		cfg.MaxPeers = 16
	}
	if cfg.MaxPeers < 1 || cfg.FlowSlots < 0 || cfg.FlowSlots > 32767 {
		return nil, directstream.ErrBounds
	}
	if int(cfg.DatagramMaximum) > MaxUDPMessageSize || cfg.DatagramBytes < 0 || (cfg.DatagramBytes == 0) != (cfg.DatagramMaximum == 0) || cfg.DatagramBytes != 0 && cfg.DatagramBytes < 3*int(cfg.DatagramMaximum) {
		return nil, directstream.ErrBounds
	}
	ctx, cancel := context.WithCancel(parent)
	manager := cfg.UDPManager
	owns := manager == nil
	if owns {
		manager = udpmessage.NewManager()
	}
	m := &DirectManager{udpManager: manager, ownsUDPManager: owns, cfg: cfg, ctx: ctx, cancel: cancel, peers: make(map[meshid.Device]*managedDirectSession), done: make(chan struct{})}
	if cfg.PromotionSnapshot != nil {
		m.workers.Add(1)
		go m.runAutomaticPromotions()
	}
	return m, nil
}
func (m *DirectManager) Close() {
	m.mu.Lock()
	if m.closed {
		done := m.done
		m.mu.Unlock()
		<-done
		return
	}
	m.closed = true
	m.cancel()
	sessions := make([]*DirectSession, 0, len(m.peers))
	for _, e := range m.peers {
		sessions = append(sessions, e.session)
	}
	m.mu.Unlock()
	for _, s := range sessions {
		s.Close()
	}
	m.workers.Wait()
	if m.ownsUDPManager {
		m.udpManager.Close()
	}
	close(m.done)
}
func (m *DirectManager) acquire(a localmesh.DirectCarrierAdmission) (*DirectSession, error) {
	return m.acquireAdmission(a, true)
}

func (m *DirectManager) acquireAdmission(a localmesh.DirectCarrierAdmission, allowCreate bool, expected ...*DirectSession) (*DirectSession, error) {
	// Admission comes from Node or the authenticated upper dispatcher after
	// peer/instance validation. Recheck lifetime; resumption never renews it.
	if a.Self != m.cfg.Credentials.Device || !a.Peer.IsValid() || a.Self == a.Peer || !a.TLS.HandshakeComplete || len(a.TLS.PeerCertificates) == 0 || a.TLS.PeerCertificates[0] == nil || a.Records == nil || a.IdentityDigest == ([32]byte{}) {
		return nil, errors.New("invalid direct carrier admission")
	}
	role := byte(0)
	if meshid.Compare(a.Self, a.Peer) > 0 {
		role = 1
	}
	if a.Role != role {
		return nil, errors.New("direct coordinator role mismatch")
	}
	if directstream.SelectedTransport(a.Selected) == 0 {
		return nil, errors.New("unsupported direct promotion agreement")
	}
	lifetime, err := newSessionLifetime(m.cfg.Credentials, a.TLS)
	if err != nil {
		return nil, err
	}
	deadline, err := lifetime.current()
	if err != nil {
		return nil, err
	}
	// Node verifies the full principal/instance identity. Rechecking certificate
	// principal here also protects accidental cross-peer admission dispatch.
	peer, err := meshid.PeerFromCert(a.TLS.PeerCertificates[0])
	if err != nil || peer.Principal() != a.Peer.Principal() {
		return nil, errors.New("direct TLS peer mismatch")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || m.ctx.Err() != nil {
		return nil, net.ErrClosed
	}
	if len(expected) != 0 {
		e := m.peers[a.Peer]
		if e == nil || e.session != expected[0] {
			return nil, ErrNoDirectSession
		}
	}
	if e := m.peers[a.Peer]; e != nil {
		select {
		case <-e.session.done:
			delete(m.peers, a.Peer)
		default:
			if e.session.cfg.IdentityDigest != a.IdentityDigest || e.session.cfg.Role != a.Role || e.selected != a.Selected {
				if !allowCreate {
					return nil, errors.New("promotion does not match retained identity or transport")
				}
				// A changed authenticated key must invalidate old sockets, never attach
				// them to the new identity. Caller may retry after retirement completes.
				e.session.cancel(errors.New("direct authenticated identity changed"))
				return nil, errors.New("direct session identity changed; retiring old session")
			}
			e.replaceLifetime(lifetime, deadline)
			select {
			case e.session.wake <- struct{}{}:
			default:
			}
			return e.session, nil
		}
	}
	if !allowCreate {
		return nil, ErrNoDirectSession
	}
	// Reap completed entries synchronously too: a delayed watcher must not cause
	// false capacity exhaustion during rapid failed-admission/reconnect churn.
	for peer, e := range m.peers {
		select {
		case <-e.session.done:
			delete(m.peers, peer)
		default:
		}
	}
	// Other peers may already have agreed a parked-lane reservation. Preserve
	// their slots while this independently arriving carrier is admitted.
	occupied := len(m.peers)
	for peer, gate := range m.admissions {
		if peer != a.Peer && gate.reserved != nil {
			occupied++
		}
	}
	if occupied >= m.cfg.MaxPeers {
		return nil, directstream.ErrCapacity
	}
	auth, err := snapshotDirectNativeAuth(a.TLS)
	if err != nil {
		return nil, err
	}
	e := &managedDirectSession{peerCertificateDER: append([]byte(nil), a.TLS.PeerCertificates[0].Raw...), original: lifetime, latest: lifetime, deadline: deadline, selected: a.Selected, nativeAuth: auth}
	backend := m.cfg.Backend
	if m.cfg.NativeBackend != nil {
		admittedPeer := a.Peer // do not capture the old carrier/TLS admission
		backend = func(ctx context.Context, peer meshid.Device, request directstream.OpenRequest) (net.Conn, error) {
			if request.Service == directstream.ServiceWendyCom || request.Service == directstream.ServiceGRPCAdmin {
				if peer != admittedPeer || request.Port != 0 || request.DatagramSize != 0 {
					return nil, ErrDenied
				}
				identity := e.nativeIdentity(peer)
				if err := identity.check(); err != nil {
					return nil, err
				}
				return m.cfg.NativeBackend(ctx, identity, request)
			}
			return m.cfg.Backend(ctx, peer, request)
		}
	}
	s, err := NewDirectSession(m.ctx, DirectSessionConfig{Peer: a.Peer, IdentityDigest: a.IdentityDigest, Role: a.Role, Deadline: deadline, CheckLifetime: e.lifetime, Backend: backend, Slots: m.cfg.FlowSlots, TXBytes: m.cfg.TXBytes, RXBytes: m.cfg.RXBytes, Window: m.cfg.Window, Initial: m.cfg.Initial, RecoveryTimeout: m.cfg.RecoveryTimeout, DatagramBytes: m.cfg.DatagramBytes, DatagramMaximum: m.cfg.DatagramMaximum})
	if err != nil {
		return nil, err
	}
	e.session = s
	m.peers[a.Peer] = e
	m.workers.Add(1)
	go func() {
		defer m.workers.Done()
		<-s.done
		m.mu.Lock()
		if m.peers[a.Peer] == e {
			delete(m.peers, a.Peer)
		}
		m.mu.Unlock()
	}()
	return s, nil
}
func (m *DirectManager) HandleCarrier(ctx context.Context, a localmesh.DirectCarrierAdmission) error {
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(m.ctx, cancel)
	defer cancel()
	defer stop()

	lane := &directCarrierLane{DirectRecordCarrier: a.Records, alias: 1}
	if a.Deferred {
		lane.alias = 0
	}
	a.Records = lane
	var reserved func()
	defer func() {
		if reserved != nil {
			reserved()
		}
	}()
	if a.Deferred {
		ready := make(chan struct{})
		close(ready)
		var retired <-chan struct{} = ready
		m.mu.Lock()
		if old := m.peers[a.Peer]; old != nil {
			retired = old.session.done
		}
		m.mu.Unlock()
		var err error
		reserved, err = lane.proposeReuse(ctx, a.Role, retired, func() bool { return true }, func() (func(), bool) {
			if m.cfg.PromotionSnapshot != nil && m.cfg.PromotionSnapshot().HasCheaperLink(a.Peer, a.Cost) {
				return nil, false
			}
			return m.reserveReuse(a.Peer)
		})
		if err != nil {
			return err
		}
		if err = lane.advance(); err != nil {
			return err
		}
	}
	for {
		a.AttachmentAlias = lane.alias
		s, err := m.acquire(a)
		if err != nil {
			return err
		}
		if reserved != nil {
			err = attachReservedCarrier(ctx, s, a, reserved)
			reserved = nil
		} else {
			err = s.Attach(ctx, a)
		}
		if !errors.Is(err, localmesh.ErrDirectLaneRetired) {
			return err
		}
		if lane.alias == ^uint32(0) {
			return localmesh.ErrDirectLaneRetired
		}
		reserved, err = lane.proposeReuse(ctx, a.Role, s.done, func() bool {
			return errors.Is(context.Cause(s.ctx), errDirectEpochRetired)
		}, func() (func(), bool) {
			if m.cfg.PromotionSnapshot != nil && m.cfg.PromotionSnapshot().HasCheaperLink(a.Peer, a.Cost) {
				return nil, false
			}
			return m.reserveReuse(a.Peer)
		})
		if err != nil {
			return err
		}
		if err := lane.advance(); err != nil {
			return localmesh.ErrDirectLaneRetired
		}
	}
}

// HandlePromotion dispatches an authenticated upper control connection to an
// existing retained epoch. It never creates an epoch and never reopens backends.
// The transport dispatcher supplies locally verified peer/instance identity,
// agreement and TLS state, exactly as Node does for HandleCarrier. Unmatched
// candidates are rejected without revoking the existing BLE session.
//
// Caller owns admission.Records and closes/joins it after this method returns.
// ctx represents the upper connection lifetime, not its current mesh carrier.
func (m *DirectManager) HandlePromotion(ctx context.Context, a localmesh.DirectCarrierAdmission, open DirectPromotionOpener) error {
	if open == nil || !validPromotionTLS(a.TLS, a.Selected) {
		return errors.New("invalid upper promotion transport")
	}
	s, err := m.acquireAdmission(a, false)
	if err != nil {
		return err
	}
	return s.Promote(ctx, a, open)
}

// Dial returns ErrNoDirectSession only when no direct epoch owns the peer.
// Once a session exists, including during a carrier outage, errors must not
// trigger a parallel upper-layer dial or silently recreate an application flow.
// ErrDirectUseUpper is the explicit exception for a new, never-admitted request
// after promotion; the caller must use the selected normal upper transport.
func (m *DirectManager) Dial(ctx context.Context, peer meshid.Device, r directstream.OpenRequest) (net.Conn, error) {
	m.mu.Lock()
	if m.closed || m.ctx.Err() != nil {
		m.mu.Unlock()
		return nil, net.ErrClosed
	}
	e := m.peers[peer]
	m.mu.Unlock()
	if e == nil {
		return nil, ErrNoDirectSession
	}
	if r.Service == directstream.ServiceUDP && m.cfg.DatagramBytes == 0 {
		return nil, ErrDirectUseUpper
	}
	return e.session.Dial(ctx, r)
}

// DialNative opens an explicitly named native service on the retained epoch.
// The returned certificate belongs to that same authenticated epoch, not a new
// carrier that happened to replace it while OPEN was pending. Its lifetime is
// enforced by the session; callers must not add another TLS handshake.
func (m *DirectManager) DialNative(ctx context.Context, peer meshid.Device, service byte) (net.Conn, *x509.Certificate, error) {
	conn, identity, err := m.DialNativeIdentity(ctx, peer, service)
	if err != nil {
		return nil, nil, err
	}
	state, err := identity.TLSState()
	if err != nil {
		conn.Close()
		return nil, nil, err
	}
	return conn, state.PeerCertificates[0], nil
}

// DialNativeIdentity also exposes the sealed identity and logical lifetime for
// native transports whose authentication must survive physical carrier loss.
func (m *DirectManager) DialNativeIdentity(ctx context.Context, peer meshid.Device, service byte) (net.Conn, DirectNativeIdentity, error) {
	release, err := m.acquireUpper(ctx, peer)
	if err != nil {
		return nil, DirectNativeIdentity{}, err
	}
	conn, identity, err := m.dialNativeIdentity(ctx, peer, service)
	if err != nil {
		release()
		return nil, DirectNativeIdentity{}, err
	}
	return &directTrackedUpper{Conn: conn, release: release}, identity, nil
}
func (m *DirectManager) dialNativeIdentity(ctx context.Context, peer meshid.Device, service byte) (net.Conn, DirectNativeIdentity, error) {
	if service != directstream.ServiceWendyCom && service != directstream.ServiceGRPCAdmin {
		return nil, DirectNativeIdentity{}, ErrDenied
	}
	m.mu.Lock()
	if m.closed || m.ctx.Err() != nil {
		m.mu.Unlock()
		return nil, DirectNativeIdentity{}, net.ErrClosed
	}
	e := m.peers[peer]
	m.mu.Unlock()
	if e == nil {
		return nil, DirectNativeIdentity{}, ErrNoDirectSession
	}
	identity := e.nativeIdentity(peer)
	if err := identity.check(); err != nil {
		if errors.Is(context.Cause(e.session.ctx), errDirectEpochRetired) {
			return nil, DirectNativeIdentity{}, ErrNoDirectSession
		}
		return nil, DirectNativeIdentity{}, err
	}
	conn, err := e.session.Dial(ctx, directstream.OpenRequest{Service: service})
	if err != nil {
		return nil, DirectNativeIdentity{}, err
	}
	return conn, identity, nil
}

// Invalidate is a security/application-policy revocation, not a route loss.
func (m *DirectManager) Invalidate(peer meshid.Device) {
	m.mu.Lock()
	e := m.peers[peer]
	m.mu.Unlock()
	if e != nil {
		e.session.Close()
	}
}

// SetDirectManager installs the peer owner before client use. The client joins
// it on Close and revokes its sockets on Invalidate. Carrier registration is
// separate: the enclosing runtime must install all promotion paths first.
func (c *Client) SetDirectManager(m *DirectManager) error {
	if m == nil || m.cfg.Credentials != c.credentials {
		return errors.New("direct manager credentials differ from app client")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.direct != nil || len(c.peers) != 0 || len(c.tlsApps) != 0 || len(c.tlsDials) != 0 || len(c.udpSessions) != 0 {
		return errors.New("direct manager must be installed before client use")
	}
	if c.admission != nil && c.admission != m {
		return errors.New("app admission manager differs from direct owner")
	}
	c.direct = m
	c.admission = m
	return nil
}

// DialNative exposes the client's retained session owner to native consumers.
// ErrNoDirectSession and ErrDirectUseUpper allow an ordinary native connection;
// every other result belongs to this epoch and must not trigger a second OPEN.
func (c *Client) DialNative(ctx context.Context, peer meshid.Device, service byte) (net.Conn, *x509.Certificate, error) {
	c.mu.Lock()
	m, closed := c.direct, c.closed
	c.mu.Unlock()
	if closed {
		return nil, nil, net.ErrClosed
	}
	if m == nil {
		return nil, nil, ErrNoDirectSession
	}
	return m.DialNative(ctx, peer, service)
}

func (c *Client) dialDirect(ctx context.Context, peer meshid.Device, port uint16) (net.Conn, bool, error) {
	c.mu.Lock()
	m, closed := c.direct, c.closed
	c.mu.Unlock()
	if closed {
		return nil, true, net.ErrClosed
	}
	if m == nil {
		return nil, false, nil
	}
	conn, err := m.Dial(ctx, peer, directstream.OpenRequest{Service: directstream.ServiceTCP, Port: port})
	if errors.Is(err, ErrNoDirectSession) || errors.Is(err, ErrDirectUseUpper) {
		return nil, false, nil
	}
	return conn, true, err
}

// AcceptsDirectCarrier reports whether a new device carrier may advertise a
// direct application lane. An upper-owned epoch must keep its physical routing
// carriers available without attempting to attach them as old application lanes.
// The identity digest comes from this carrier's authenticated TLS state. A
// changed binding must reach registry admission to invalidate the old epoch.
// This is a hello hint; Attach still performs authoritative actor admission.
func (m *DirectManager) AcceptsDirectCarrier(peer meshid.Device, identity [32]byte) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || m.ctx.Err() != nil {
		return false
	}
	e := m.peers[peer]
	if e == nil {
		if m.applicationBusyLocked(peer) {
			return false
		}
		// A retained destination may need this neighbour as its only route.
		// Decline optional application framing when its admission would fail,
		// leaving the ordinary routing carrier available. Completed actors
		// do not consume capacity even if their cleanup watcher is delayed.
		live := 0
		for _, retained := range m.peers {
			select {
			case <-retained.session.done:
			default:
				live++
			}
		}
		for _, gate := range m.admissions {
			if gate.reserved != nil {
				live++
			}
		}
		return live < m.cfg.MaxPeers
	}
	state := e.session.Status()
	if state.Closed && m.applicationBusyLocked(peer) {
		return false
	}
	return state.Closed || !state.UpperOwned || e.session.cfg.IdentityDigest != identity
}
