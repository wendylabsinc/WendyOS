package services

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/discovery"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
)

// Compile-time assertion that MeshDialer satisfies this package's PeerDialer
// (build delivery, push proxy). The VIP dialer is mesh.PeerDialer, implemented
// by MeshAppDialer over this dialer's self and local-mesh legs.
var _ PeerDialer = (*MeshDialer)(nil)

const (
	// LAN discovery gets a separate cold-resolve budget from connection setup.
	// The outcome cache keeps repeat administrative dials from re-paying it.
	lanBudget          = 1 * time.Second
	lanDiscoveryBudget = 3 * time.Second
	lanCacheTTL        = 60 * time.Second

	// meshDialTarget is the fixed gRPC target used for every LAN peer dial. The
	// peer's real address is supplied out-of-band by meshDialContextDialer, so a
	// resolved IP:port — including an IPv6 zone id such as "%wlan0" — is handed to
	// net.Dialer verbatim and never flows through gRPC's target URL parser, which
	// rejects the "%" zone separator as an invalid percent-escape (the failure that
	// silently forced every zoned-IPv6 LAN dial onto the cloud relay).
	meshDialTarget = "passthrough:///mesh-peer"
)

// DeliveryTarget binds a Cloud asset record to its authenticated device principal.
// Build callers perform endpoint mTLS pinned to Device after opening the transport.
type DeliveryTarget struct {
	Device    meshid.Device
	AssetUUID string
}

// meshDialContextDialer returns a gRPC dialer that connects to hostport
// verbatim via the standard library, which is the only component that
// understands IPv6 zone ids. hostport is the address MeshDialer resolved from
// mDNS (a net.JoinHostPort result). The gRPC-supplied target argument is
// ignored — meshDialTarget is a placeholder authority only.
func meshDialContextDialer(hostport string) func(context.Context, string) (net.Conn, error) {
	return func(ctx context.Context, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", hostport)
	}
}

// meshIdentity is this device's mesh principal plus its credential material,
// held as one value behind MeshDialer.mu so every dial reads a consistent
// snapshot and UpdateIdentity can swap it wholesale when the device is
// (re-)provisioned while the agent is running. device is read off certPEM: it
// is what a VIP dial aimed at this host is recognized as, what a local-mesh
// session authenticates with, and what the LAN leg compares against a
// discovered peer's advertised principal. A certificate carrying no device
// principal leaves it zero, which keeps the self-VIP and local-mesh legs
// closed instead of guessing an identity.
type meshIdentity struct {
	generation uint64 // retires in-flight app setup after an enrollment endpoint change
	device     meshid.Device
	certPEM    string
	keyPEM     string
	chainPEM   string
}

// MeshDialer is the agent's mesh dialer. Every leg is keyed by the target's
// device principal: this device's own apps, end-to-end QUIC app sessions over
// the routed local mesh, direct mTLS dials to LAN peers, and — for callers
// that name a Cloud asset — the Cloud relay's symbolic services.
type MeshDialer struct {
	logger  *zap.Logger
	metrics *MeshMetrics

	// Seams (overridden in tests).
	lanLookup     func(ctx context.Context, device meshid.Device) (hostport string, ok bool)
	dialLAN       func(ctx context.Context, hostport string, device meshid.Device, port uint16) (net.Conn, error)
	appLANDial    func(ctx context.Context, hostport string, device meshid.Device, port uint16) (net.Conn, error)
	cloudAppDial  func(ctx context.Context, device meshid.Device, port uint16) (net.Conn, error)
	appSocketDial func(context.Context, string, string) (net.Conn, error)
	lanEvents     func(context.Context, discovery.StreamOptions) <-chan discovery.LANEvent
	serviceDial   func(ctx context.Context, peer meshid.Device, port uint16) (net.Conn, error)
	localDial     func(ctx context.Context, peer meshid.Device, port uint16) (net.Conn, error)
	selfDial      func(ctx context.Context, port uint16) (net.Conn, error)
	now           func() time.Time

	mu       sync.Mutex
	ident    meshIdentity
	cache    map[meshid.Device]lanCacheEntry
	cloudURL string
	appFlows map[*ownedAppConnection]struct{}
}

// SetLocalMeshDialer supplies the end-to-end QUIC app session toward one
// device principal over the routed local mesh. A missing authenticated route
// is reported with meshsession.ErrNoRoute.
func (d *MeshDialer) SetLocalMeshDialer(dial func(context.Context, meshid.Device, uint16) (net.Conn, error)) {
	d.mu.Lock()
	d.localDial = dial
	d.mu.Unlock()
}

// SetSelfDialer supplies the same live ingress authorization used by peer
// sessions for this device's own VIP. A local app never needs a Babel route
// or a peer session to reach a sibling app on its own device.
func (d *MeshDialer) SetSelfDialer(dial func(context.Context, uint16) (net.Conn, error)) {
	d.mu.Lock()
	d.selfDial = dial
	d.mu.Unlock()
}

// SetLocalServiceDialer supplies routed TCP for administrative services. App
// sessions keep their separate ingress registry and never use this dial path.
func (d *MeshDialer) SetLocalServiceDialer(dial func(context.Context, meshid.Device, uint16) (net.Conn, error)) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.serviceDial = dial
}

// dialSelf answers a VIP dial aimed at this device's own principal. The flow
// never leaves the host, so it needs no Babel route and no peer session — only
// the live ingress authorization a peer app session would have been granted.
// own is false when the target is a different device.
func (d *MeshDialer) dialSelf(ctx context.Context, device meshid.Device, port uint16) (net.Conn, string, error, bool) {
	d.mu.Lock()
	self, dial := d.ident.device, d.selfDial
	d.mu.Unlock()
	if self == (meshid.Device{}) || device != self {
		return nil, "", nil, false
	}
	conn, mode, err := d.dialLocalApp(ctx, dial, port, func(result string, durMs float64) {
		d.metrics.RecordDial(device, "local-app", result, durMs)
	})
	return conn, mode, err, true
}

// dialLocalMesh opens the end-to-end QUIC app session toward a device
// principal over the routed local mesh. routed is false when this agent has no
// local mesh configured at all; a routed failure is reported with
// meshsession.ErrNoRoute when no authenticated route exists yet.
func (d *MeshDialer) dialLocalMesh(ctx context.Context, device meshid.Device, port uint16) (net.Conn, string, error, bool) {
	d.mu.Lock()
	dial := d.localDial
	d.mu.Unlock()
	if dial == nil {
		return nil, "", nil, false
	}
	started := d.now()
	conn, err := dial(ctx, device, port)
	result := "ok"
	if err != nil {
		result = "error"
	}
	d.metrics.RecordDial(device, "local-mesh", result, float64(d.now().Sub(started).Milliseconds()))
	if err != nil {
		return nil, "", err, true
	}
	return conn, "local-mesh", nil, true
}

// dialLocalApp opens one loopback connection to a local app port under the
// live ingress authorization, recording the outcome against whichever peer
// label the caller addressed the dial with.
func (d *MeshDialer) dialLocalApp(ctx context.Context, dial func(context.Context, uint16) (net.Conn, error), port uint16, record func(result string, durMs float64)) (net.Conn, string, error) {
	started := d.now()
	var conn net.Conn
	err := errors.New("wendy net app ingress is unavailable")
	if dial != nil {
		conn, err = dial(ctx, port)
	}
	result := "ok"
	if err != nil {
		result = "error"
	}
	record(result, float64(d.now().Sub(started).Milliseconds()))
	return conn, "local-app", err
}

type lanCacheEntry struct {
	hostport string // "" = not on LAN
	expires  time.Time
}

// NewMeshDialer builds a MeshDialer. certPEM/keyPEM/chainPEM are this device's
// credentials; the device principal dialed and pinned against is read off
// certPEM. metrics may be nil (e.g. in tests); RecordDial no-ops on a nil
// *MeshMetrics.
func NewMeshDialer(logger *zap.Logger, certPEM, keyPEM, chainPEM string, metrics *MeshMetrics) *MeshDialer {
	d := &MeshDialer{
		logger:  logger,
		metrics: metrics,
		ident: meshIdentity{
			device:   deviceFromCertPEM(certPEM),
			certPEM:  certPEM,
			keyPEM:   keyPEM,
			chainPEM: chainPEM,
		},
		now:   time.Now,
		cache: make(map[meshid.Device]lanCacheEntry),
	}
	d.lanLookup = d.discoverOnLAN
	d.dialLAN = d.meshDialLAN
	d.appLANDial = d.meshDialAppLAN
	d.cloudAppDial = d.meshDialAppCloud
	d.lanEvents = discovery.StreamLAN
	return d
}

// SetCloudEndpoint scopes app relays to the device's enrolled Cloud endpoint,
// rather than an endpoint supplied by a DNS query or discovery advertisement.
func (d *MeshDialer) SetCloudEndpoint(endpoint string) {
	d.mu.Lock()
	retired := d.replaceIdentityLocked(d.ident.certPEM, d.ident.keyPEM, d.ident.chainPEM, endpoint)
	d.mu.Unlock()
	closeRetiredApplications(retired)
}

// deviceFromCertPEM reads this device's mesh principal off its own leaf.
// Legacy asset, user and service certificates carry no device principal: the
// result is the zero Device, which every device-keyed leg treats as "not this
// device" rather than inventing an identity for it.
func deviceFromCertPEM(certPEM string) meshid.Device {
	if certPEM == "" {
		return meshid.Device{}
	}
	chain, err := certs.ParseCertsFromPEM([]byte(certPEM))
	if err != nil || len(chain) == 0 {
		return meshid.Device{}
	}
	device, err := meshid.DeviceFromCert(chain[0])
	if err != nil {
		return meshid.Device{}
	}
	return device
}

// UpdateIdentity swaps the dialer's identity snapshot; subsequent dials use
// the new values. App transports and in-flight app setup using the prior
// identity are retired before returning.
// Called from the OnProvisioned callback so a device enrolled while the agent
// is running (the normal BLE first-boot flow) gets a working mesh without a
// restart. The LAN outcome cache is cleared too: re-enrollment may move the
// device to a different tenant, and stale positive entries could point at
// peers the new identity can no longer authenticate against.
func (d *MeshDialer) UpdateIdentity(certPEM, keyPEM, chainPEM string) {
	d.mu.Lock()
	retired := d.replaceIdentityLocked(certPEM, keyPEM, chainPEM, d.cloudURL)
	d.mu.Unlock()
	closeRetiredApplications(retired)
}

// UpdateCloudIdentity replaces the enrolled endpoint and credentials atomically.
// App sessions cannot combine a new identity with the previous tenant endpoint.
func (d *MeshDialer) UpdateCloudIdentity(endpoint, certPEM, keyPEM, chainPEM string) {
	d.mu.Lock()
	retired := d.replaceIdentityLocked(certPEM, keyPEM, chainPEM, endpoint)
	d.mu.Unlock()
	closeRetiredApplications(retired)
}

func (d *MeshDialer) replaceIdentityLocked(certPEM, keyPEM, chainPEM, endpoint string) []*ownedAppConnection {
	changed := d.ident.certPEM != certPEM || d.ident.keyPEM != keyPEM || d.ident.chainPEM != chainPEM || d.cloudURL != endpoint
	generation := d.ident.generation
	if changed {
		generation++
	}
	d.ident = meshIdentity{
		generation: generation,
		device:     deviceFromCertPEM(certPEM),
		certPEM:    certPEM,
		keyPEM:     keyPEM,
		chainPEM:   chainPEM,
	}
	d.cloudURL = endpoint
	d.cache = make(map[meshid.Device]lanCacheEntry)
	var retired []*ownedAppConnection
	if changed {
		for flow := range d.appFlows {
			retired = append(retired, flow)
		}
		d.appFlows = nil
	}
	return retired
}

func closeRetiredApplications(retired []*ownedAppConnection) {
	for _, flow := range retired {
		_ = flow.Close()
	}
}

// identity returns a consistent snapshot of the dialer's identity. The lock is
// held only for the copy — never across network I/O.
func (d *MeshDialer) identity() meshIdentity {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.ident
}

// DialDevice opens a byte stream to port on the given mesh device. It is the
// device-keyed form of DialTarget for callers that name a principal rather
// than a Cloud asset (mesh.Proxy): with no asset record and no Cloud service,
// the relay leg stays closed and only the direct mesh legs can answer.
func (d *MeshDialer) DialDevice(ctx context.Context, device meshid.Device, port uint16) (net.Conn, string, error) {
	return d.DialTarget(ctx, DeliveryTarget{Device: device}, port)
}

// DialTarget opens raw TCP to an administrative service, over LAN or Babel.
// The caller must authenticate it with TLS pinned to target.Device. Cloud v2
// currently authorizes operator-originated relays only; a device credential
// cannot authorize a device-to-device relay fallback.
func (d *MeshDialer) DialTarget(ctx context.Context, target DeliveryTarget, port uint16) (net.Conn, string, error) {
	device := target.Device
	if !device.IsValid() || port == 0 {
		return nil, "", errors.New("invalid wendy net service target")
	}
	if target.AssetUUID != "" {
		if target.AssetUUID != device.UUID().String() {
			return nil, "", errors.New("Cloud target UUID does not match the device identity")
		}
	}
	if device == d.identity().device {
		conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(int(port))))
		if err != nil {
			return nil, "", err
		}
		return conn, "local-service", nil
	}
	if hostport, ok := d.lanAddr(ctx, device); ok {
		started := d.now()
		bounded, cancel := context.WithTimeout(ctx, lanBudget)
		conn, err := d.dialLAN(bounded, hostport, device, port)
		cancel()
		result := "ok"
		if err != nil {
			result = "error"
		}
		d.metrics.RecordDial(device, "lan-direct", result, float64(d.now().Sub(started).Milliseconds()))
		if err == nil {
			return conn, "lan-direct", nil
		}
	}
	return d.DialRoutedTarget(ctx, target, port)
}

// DialRoutedTarget retries an administrative connection over Babel alone.
// Delivery uses it after a LAN socket connects but fails principal-pinned TLS;
// the returned stream still requires exactly the same endpoint authentication.
func (d *MeshDialer) DialRoutedTarget(ctx context.Context, target DeliveryTarget, port uint16) (net.Conn, string, error) {
	device := target.Device
	if !device.IsValid() || port == 0 {
		return nil, "", errors.New("invalid wendy net service target")
	}
	if target.AssetUUID != "" && target.AssetUUID != device.UUID().String() {
		return nil, "", errors.New("Cloud target UUID does not match the device identity")
	}
	d.mu.Lock()
	dial := d.serviceDial
	d.mu.Unlock()
	if dial == nil {
		return nil, "", fmt.Errorf("wendy net: no LAN or Babel route to %s; Cloud does not authorize device-originated relays", device.Principal())
	}
	started := d.now()
	conn, err := dial(ctx, device, port)
	result := "ok"
	if err != nil {
		result = "error"
	}
	d.metrics.RecordDial(device, "local-mesh", result, float64(d.now().Sub(started).Milliseconds()))
	if err != nil {
		return nil, "", err
	}
	return conn, "local-mesh", nil
}

// lanAddr resolves device to a LAN hostport, consulting (and refreshing) the
// TTL cache so repeat dials don't re-pay the mDNS browse budget. Both positive
// and negative outcomes are cached.
func (d *MeshDialer) lanAddr(ctx context.Context, device meshid.Device) (string, bool) {
	d.mu.Lock()
	if e, ok := d.cache[device]; ok && d.now().Before(e.expires) {
		d.mu.Unlock()
		return e.hostport, e.hostport != ""
	}
	d.mu.Unlock()

	lctx, cancel := context.WithTimeout(ctx, lanDiscoveryBudget)
	hostport, ok := d.lanLookup(lctx, device)
	cancel()
	if !ok {
		hostport = ""
	}
	d.mu.Lock()
	d.cache[device] = lanCacheEntry{hostport: hostport, expires: d.now().Add(lanCacheTTL)}
	d.mu.Unlock()
	return hostport, hostport != ""
}

// discoverOnLAN browses _wendyos._udp for a provisioned device advertising
// the target device principal. The principal TXT record is the device's full
// SPIFFE identity; a sighting only counts when it parses to exactly the device
// being dialed.
func (d *MeshDialer) discoverOnLAN(ctx context.Context, device meshid.Device) (string, bool) {
	dev, found := d.findLANDevice(ctx, device, false)
	if !found {
		return "", false
	}
	return net.JoinHostPort(dev.IPAddress, strconv.Itoa(dev.Port)), true
}

// findLANDevice waits for the requested principal, rather than a batch browse
// settling after an unrelated (often this host's own) advertisement. Discovery
// supplies an address hint only; every app path must still prove the exact TLS
// principal and admit the live port.
func (d *MeshDialer) findLANDevice(ctx context.Context, peer meshid.Device, requireApplicationTLS bool) (models.LANDevice, bool) {
	ctx, cancel := context.WithTimeout(ctx, lanDiscoveryBudget)
	stream := d.lanEvents(ctx, discovery.StreamOptions{OnBackendError: func(err error) {
		d.logger.Debug("LAN peer discovery backend unavailable", zap.Error(err))
	}})
	defer func() {
		cancel()
		for range stream {
		}
	}()
	for event := range stream {
		if event.Kind != discovery.LANFound && event.Kind != discovery.LANUpdated {
			continue
		}
		dev := event.Device
		if dev.PrincipalURI != peer.Principal() || !dev.IsMTLS || dev.IPAddress == "" || (requireApplicationTLS && dev.AppTLSPort == 0) {
			continue
		}
		if strings.EqualFold(dev.IPAddress, "localhost") {
			continue
		}
		ip, err := netip.ParseAddr(dev.IPAddress)
		if (err != nil && requireApplicationTLS) || (err == nil && (ip.IsLoopback() || ip.IsUnspecified())) {
			continue
		}
		if !requireApplicationTLS && (dev.Port <= 0 || dev.Port > 65535) {
			continue
		}
		return dev, true
	}
	return models.LANDevice{}, false
}

// dialBoundContext returns a stream context that outlives the caller's ctx —
// the returned conn must survive past the dial budget — while still letting
// ctx bound the connect/stream-open phase: until established() is called,
// cancelling ctx also cancels sctx. Callers must call cancel to release the
// stream (typically via the conn's teardown) and established() once the
// stream is up and the open frame has been sent.
func dialBoundContext(ctx context.Context) (sctx context.Context, cancel context.CancelFunc, established func()) {
	sctx, cancel = context.WithCancel(context.Background())
	stop := context.AfterFunc(ctx, cancel)
	return sctx, cancel, func() { stop() }
}

// meshDialLAN opens the requested service directly. Sending this through
// MeshDial would apply the app-port allowlist to the agent and registry ports.
// Delivery authenticates the returned connection before sending image data.
func (d *MeshDialer) meshDialLAN(ctx context.Context, hostport string, device meshid.Device, port uint16) (net.Conn, error) {
	host, _, err := net.SplitHostPort(hostport)
	if err != nil {
		return nil, err
	}
	return (&net.Dialer{}).DialContext(ctx, "tcp", net.JoinHostPort(host, strconv.Itoa(int(port))))
}

// tunnelStream abstracts the stream framing behind one send/recv shape so
// streamNetConn can serve it.
type tunnelStream interface {
	send(payload []byte, halfClose bool) error
	recv() (payload []byte, halfClose bool, err error)
	closeSend() error
}

// streamConn exposes a tunnelStream directly as a net.Conn — no intermediate
// pipe, no relay goroutines — so the two stream directions stay fully
// independent: an inbound half_close ends only Reads (never truncating an
// in-flight upload), and CloseWrite ends only Writes while Reads keep
// draining the peer's response.
//
// Concurrency: Read may run concurrently with Write/CloseWrite/Close, but
// there must be at most one concurrent Read and one concurrent Write — the
// same single-reader/single-writer shape mesh.relayBytes uses — which maps
// exactly onto gRPC's one-concurrent-Recv/one-concurrent-Send stream rule.
//
// Close must always be called to release the underlying stream: teardown
// cancels the stream context and closes the gRPC connection, unblocking any
// Read/Write parked in recv/send, and runs exactly once.
type streamConn struct {
	s        tunnelStream
	teardown func()
	once     sync.Once

	// Read-side state; only the (single) reader touches these.
	readBuf []byte // leftover payload from a frame larger than the Read buffer
	readErr error  // sticky; io.EOF after a clean end or inbound half_close

	// writeClosed flips once on CloseWrite; Write and CloseWrite check it.
	writeClosed atomic.Bool
}

func (c *streamConn) Read(p []byte) (int, error) {
	for {
		if len(c.readBuf) > 0 {
			n := copy(p, c.readBuf)
			c.readBuf = c.readBuf[n:]
			return n, nil
		}
		if c.readErr != nil {
			return 0, c.readErr
		}
		payload, halfClose, err := c.s.recv()
		if err != nil {
			// gRPC returns io.EOF for a clean stream end; pass it (and any
			// other error) through sticky so subsequent Reads repeat it.
			c.readErr = err
			return 0, c.readErr
		}
		if halfClose {
			// Serve this frame's data first; EOF starts on the next Read.
			c.readErr = io.EOF
		}
		c.readBuf = payload
	}
}

func (c *streamConn) Write(p []byte) (int, error) {
	if c.writeClosed.Load() {
		return 0, net.ErrClosed
	}
	if err := c.s.send(p, false); err != nil {
		return 0, err
	}
	return len(p), nil
}

// CloseWrite half-closes the write direction: it sends a half_close frame and
// closes the gRPC send side. Reads are unaffected. Idempotent.
func (c *streamConn) CloseWrite() error {
	if c.writeClosed.Swap(true) {
		return nil
	}
	err := c.s.send(nil, true)
	if cerr := c.s.closeSend(); err == nil {
		err = cerr
	}
	return err
}

// Close releases the underlying stream (running teardown exactly once) and
// never blocks. Any concurrently parked Read/Write is unblocked with an error
// by the stream context cancellation inside teardown.
func (c *streamConn) Close() error {
	c.once.Do(c.teardown)
	return nil
}

// Deadlines are not supported: the conn is driven by relay loops that run
// until EOF/error, and Close unblocks parked calls. Accept and ignore them.
func (c *streamConn) SetDeadline(time.Time) error      { return nil }
func (c *streamConn) SetReadDeadline(time.Time) error  { return nil }
func (c *streamConn) SetWriteDeadline(time.Time) error { return nil }

func (c *streamConn) LocalAddr() net.Addr  { return streamConnAddr{} }
func (c *streamConn) RemoteAddr() net.Addr { return streamConnAddr{} }

type streamConnAddr struct{}

func (streamConnAddr) Network() string { return "wendy-mesh" }
func (streamConnAddr) String() string  { return "wendy-mesh" }

// streamNetConn exposes a tunnelStream as a net.Conn. teardown must release
// the stream (cancel its context + close the gRPC conn); it runs exactly
// once, on Close.
func streamNetConn(s tunnelStream, teardown func()) net.Conn {
	return &streamConn{s: s, teardown: teardown}
}

// IsSelf checks the authenticated full device principal.
func (d *MeshDialer) IsSelf(device meshid.Device) bool {
	self := d.identity().device
	return self != (meshid.Device{}) && self == device
}
