//go:build linux

package bleprovider

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"
	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	"github.com/wendylabsinc/wendy/go/internal/shared/tsarelay"
	"go.uber.org/zap"
)

type runtime struct {
	policy               *localmesh.ConnectionPolicy
	candidates           localmesh.CandidateQueue
	nextDial             time.Time
	dialTimeouts         map[string]int
	cfg                  Config
	uuid                 string
	owner                string
	hciIndex             int
	local                l2addr
	ownerLookup          func(context.Context, *dbus.Conn) (string, error)
	bus                  *dbus.Conn
	freshness            *advertisementFreshness
	mu                   sync.Mutex
	radioRetry           map[string]time.Time
	known                map[string]meshid.Device
	scanPaused           bool
	lastScanToggle       time.Time
	links                sync.WaitGroup
	dialSlot             chan struct{}
	dialCoC              func(context.Context, string, string, uint16) (net.Conn, error)
	serverTLS            *tls.Config
	advertisementRefresh chan struct{}
	advertisement        Advertisement
	timeSlots            chan struct{}
	tlsGate              tlsHandshakeAdmission
	discovery            discoveryRecovery
	reportErrors         <-chan error
	reportErr            error
	scanErr              error
	discoveryErr         error // owned by the scan loop; cached object reads cannot clear it
	restartScan          func(context.Context, *dbus.Conn, dbus.ObjectPath) error
}

type cheaperLinkChecker interface {
	HasCheaperLink(device meshid.Device, cost uint16) bool
}

func (r *runtime) hasCheaperLink(device meshid.Device) bool {
	if r.cfg.Selection != nil {
		return !r.cfg.Selection.AllowRadio(device, localmesh.RadioBLE)
	}
	checker, ok := r.cfg.Node.(cheaperLinkChecker)
	return ok && checker.HasCheaperLink(device, LinkCost)
}

// noteDialTimeout warns after three consecutive dial-context timeouts.
// It reports the observed failures; retry and cancellation policy is unchanged.
func (r *runtime) noteDialTimeout(asset string, timedOut bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.dialTimeouts == nil {
		r.dialTimeouts = make(map[string]int)
	}
	if !timedOut {
		delete(r.dialTimeouts, asset)
		return
	}
	// Radio addresses are untrusted and can rotate between advertisements.
	// Bound diagnostic history just like the actual retry history; dropping
	// a diagnostic entry must never affect connection admission or backoff.
	if _, exists := r.dialTimeouts[asset]; !exists && len(r.dialTimeouts) >= maxPeerRetryHistory {
		return
	}
	if r.dialTimeouts[asset] >= 3 {
		return
	}
	r.dialTimeouts[asset]++
	if r.dialTimeouts[asset] == 3 {
		r.cfg.Logger.Warn("BLE peer has repeated dial timeouts", zap.String("radio", asset), zap.Int("consecutive_timeouts", r.dialTimeouts[asset]))
	}
}

func (r *runtime) clearDialTimeouts(asset string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.dialTimeouts, asset)
}

// cheaperLinkReason reports the veto branch behind a cheaper-link decision so
// a torn-down link names the exact clause that fired.
func (r *runtime) cheaperLinkReason(asset meshid.Device) string {
	if r.cfg.Selection != nil {
		_, reason := r.cfg.Selection.AllowRadioReason(asset, localmesh.RadioBLE)
		return reason
	}
	return "legacy-checker"
}

// admitDial reports whether an outbound dial may start now, and if so moves
// the device-wide pacing marker forward. Now is a parameter for tests.
func (r *runtime) admitDial(now time.Time) bool {
	if r.cfg.Pacer != nil {
		return r.cfg.Pacer.Admit(now)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if now.Before(r.nextDial) {
		return false
	}
	r.nextDial = now.Add(globalDialPace)
	return true
}

// cheaperLinkReverify is the delay between a veto sample and its
// confirmation. A single transient false evaluation (duplicate sighting,
// fresh-hint churn, mid-formation snapshot skew) must not kill a new or
// established link; a genuinely superseded link is still shed ~4s after the
// tick that first noticed it.
const cheaperLinkReverify = 3 * time.Second

// confirmCheaperLink re-verifies a veto after a short delay and reports
// whether it persisted for the full delay. Any abort (context done, stop
// channel) reports false, and the caller must tear down without attaching:
// an aborted wait is not a cleared veto. stop may be nil (dial/accept paths
// have no stop channel); a nil channel simply never fires in the select.
func (r *runtime) confirmCheaperLink(ctx context.Context, stop <-chan struct{}, asset meshid.Device) bool {
	r.cfg.Logger.Debug("BLE cheaper-link decision pending re-verify", zap.Any("peer", asset), zap.String("reason", r.cheaperLinkReason(asset)))
	timer := time.NewTimer(cheaperLinkReverify)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-stop:
		return false
	case <-timer.C:
	}
	if !r.hasCheaperLink(asset) {
		return false
	}
	r.cfg.Logger.Debug("BLE closing CoC on cheaper-link decision", zap.Any("peer", asset), zap.String("reason", r.cheaperLinkReason(asset)))
	return true
}

func (r *runtime) watchCheaperLink(ctx context.Context, device meshid.Device, conn net.Conn) func() {
	stop := make(chan struct{})
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-stop:
				return
			case <-ticker.C:
				if r.hasCheaperLink(device) && r.confirmCheaperLink(ctx, stop, device) {
					r.cfg.Logger.Debug("BLE closing CoC on cheaper-link decision", zap.Stringer("peer", device.UUID()))
					_ = conn.Close()
					return
				}
			}
		}
	}()
	return func() { close(stop) }
}

const maxPeerRetryHistory = 128

func (r *runtime) reserveCandidate(peer candidate) *localmesh.ConnectionLease {
	r.mu.Lock()
	if r.known == nil {
		r.known = make(map[string]meshid.Device)
	}
	r.expireRadioRetries(time.Now())
	known := r.known[peer.key()]
	if known != (meshid.Device{}) && deviceHint(known.UUID()) != peer.hint {
		delete(r.known, peer.key())
		known = meshid.Device{}
	}
	cooling := time.Now().Before(r.radioRetry[peer.key()])
	r.mu.Unlock()
	if cooling || (known != (meshid.Device{}) && r.hasCheaperLink(known)) {
		return nil
	}
	return r.connectionPolicy().Reserve(peer.key(), known, true)
}

func (r *runtime) failedCandidate(peer candidate) {
	r.candidateQueue().Failed(peer.key(), time.Now())
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.radioRetry == nil {
		r.radioRetry = make(map[string]time.Time)
	}
	r.expireRadioRetries(time.Now())
	if len(r.radioRetry) < maxPeerRetryHistory {
		r.radioRetry[peer.key()] = time.Now().Add(5 * time.Second)
	}
}

// Caller holds mu. Radio-address churn is untrusted and has the same bounded
// retry history as full-principal attempts.
func (r *runtime) expireRadioRetries(now time.Time) {
	for address, until := range r.radioRetry {
		if !now.Before(until) {
			delete(r.radioRetry, address)
		}
	}
}

func (r *runtime) knownCandidate(peer candidate) (meshid.Device, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	device, ok := r.known[peer.key()]
	return device, ok && deviceHint(device.UUID()) == peer.hint
}

func (r *runtime) candidateQueue() *localmesh.CandidateQueue {
	if r.cfg.Candidates != nil {
		return r.cfg.Candidates
	}
	return &r.candidates
}

func (r *runtime) connectionPolicy() *localmesh.ConnectionPolicy {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.policy == nil {
		if r.cfg.Policy != nil {
			r.policy = r.cfg.Policy
			return r.policy
		}
		self := meshid.Device{}
		if r.cfg.Credentials != nil {
			self = r.cfg.Credentials.Device
		}
		r.policy = localmesh.NewConnectionPolicy(self, false, localmesh.ConnectionLimits{Outgoing: localmesh.BLEMaxOutgoing, Attempts: localmesh.BLEMaxOutgoingAttempts, Total: localmesh.BLEMaxConnections})
	}
	return r.policy
}

// Run advertises, scans, listens for CoC channels, and initiates channels to
// eligible devices without UUID ordering. It blocks until ctx ends. BlueZ data is
// only a discovery hint: every link requires a verified same-tenant device
// certificate and complete UUID before AttachStream is called.
func Run(ctx context.Context, cfg Config) error {
	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	if err := cfg.defaults(); err != nil {
		return err
	}
	uuid := ServiceUUID(cfg.Credentials.Device.Tenant())
	if uuid == "" {
		return errors.New("BLE requires a tenant UUID")
	}
	adv, err := NewAdvertisement(cfg.Credentials.Device.UUID(), cfg.NetworkName, cfg.PSM)
	if err != nil {
		return err
	}
	if cfg.Policy != nil && !cfg.Policy.IncomingAvailable() {
		adv.Capabilities = CapabilityTimeRelay
	}
	payload, _ := adv.MarshalBinary()
	bus, closeTransport, err := connectProviderBus(runCtx)
	if err != nil {
		return fmt.Errorf("BlueZ system bus: %w", err)
	}
	stopBusGuard := guardProviderBusShutdown(runCtx, 3*time.Second, closeTransport)
	defer func() { _ = closeTransport(); _ = bus.Close(); stopBusGuard() }()
	owner, err := bluezOwner(runCtx, bus)
	if err != nil {
		return err
	}
	adapter, err := selectAdapter(runCtx, bus, cfg.AdapterPath)
	if err != nil {
		return err
	}
	local, err := adapterL2Address(runCtx, bus, adapter, cfg.PSM)
	if err != nil {
		return err
	}
	listener, err := listenL2CAP(local)
	if err != nil {
		return err
	}
	defer listener.Close()
	timeLocal := local
	timeLocal.PSM = tsarelay.BLETimePSM
	timeListener, err := listenL2CAP(timeLocal)
	if err != nil {
		return fmt.Errorf("BLE timestamp HTTPS listener: %w", err)
	}
	defer timeListener.Close()
	hciIndex, parseErr := adapterHCIIndex(adapter)
	if parseErr != nil {
		return parseErr
	}
	// Subscribe before StartDiscovery. A setup error leaves binary reception
	// usable and is retried by the report loop, with degraded health reported.
	reportSource, reportErr := openManagementReports()
	if reportSource != nil {
		defer reportSource.Close()
	}
	reportErrors := make(chan error, 8)
	freshness := newAdvertisementFreshness(adapter, uuid, cfg.NetworkName, cfg.Credentials.Device)
	stopWatching, err := watchAdvertisements(runCtx, bus, owner, freshness)
	if err != nil {
		return err
	}
	defer func() { cancelRun(); stopWatching() }()
	if err = setupBlueZ(runCtx, bus, adapter, uuid, payload); err != nil {
		return err
	}
	defer func() { cancelRun(); cleanupBlueZ(bus, adapter, cfg.Logger) }()
	// A daemon restart during setup loses the registration before the scan
	// begins. The outer carrier supervisor retries with a fresh bus.
	currentOwner, err := bluezOwner(runCtx, bus)
	if err != nil {
		return err
	}
	if err := verifyBlueZOwner(owner, currentOwner); err != nil {
		return err
	}
	r := &runtime{cfg: cfg, uuid: uuid, owner: owner, hciIndex: hciIndex, local: local, ownerLookup: bluezOwner, bus: bus, freshness: freshness, reportErrors: reportErrors, reportErr: reportErr, dialSlot: make(chan struct{}, 1), tlsGate: newTLSHandshakeAdmission()}
	r.advertisement = adv
	r.timeSlots = make(chan struct{}, 2)
	r.serverTLS = r.makeServerTLS()
	r.advertisementRefresh = make(chan struct{}, 1)
	var loops sync.WaitGroup
	loopErrors := make(chan error, 5)
	// Publish setup completion before the loops can report a discovery failure.
	r.publishDiscoveryHealth(runCtx)
	loops.Add(5)
	go func() {
		defer loops.Done()
		loopErrors <- runManagementReports(runCtx, hciIndex, freshness, reportSource, openManagementReports, reportErrors)
	}()
	go func() { defer loops.Done(); loopErrors <- r.acceptLoop(runCtx, listener) }()
	go func() { defer loops.Done(); loopErrors <- r.acceptTimeLoop(runCtx, timeListener) }()
	go func() { defer loops.Done(); loopErrors <- r.scanLoop(runCtx, bus, adapter) }()
	go func() {
		defer loops.Done()
		loopErrors <- runAdvertisementRefresh(runCtx, r.advertisementRefresh, advertisementRefreshInterval, func(ctx context.Context) error {
			r.mu.Lock()
			current := r.advertisement
			r.mu.Unlock()
			latest, err := current.MarshalBinary()
			if err != nil {
				return err
			}
			return refreshOwnedAdvertisement(ctx, bus, closeTransport, owner, uuid, latest)
		})
	}()
	var loopErr error
	select {
	case <-ctx.Done():
	case loopErr = <-loopErrors:
		if loopErr == nil && ctx.Err() == nil {
			loopErr = errors.New("BLE provider loop exited unexpectedly")
		}
	}
	cancelRun()
	_ = listener.Close()
	_ = timeListener.Close()
	loops.Wait()
	r.links.Wait()
	return loopErr
}

// The fixed-destination time relay has a separate small admission bound and
// never consumes a router-link slot or changes the shared adapter's state.
func (r *runtime) acceptTimeLoop(ctx context.Context, listener *l2Listener) error {
	sem := r.timeSlots
	if sem == nil {
		return errors.New("BLE timestamp relay admission unavailable")
	}
	for ctx.Err() == nil {
		conn, err := listener.Accept(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("BLE timestamp HTTPS accept: %w", err)
		}
		select {
		case sem <- struct{}{}:
			r.updateAdvertisementAdmission()
			r.links.Add(1)
			go func() {
				defer r.links.Done()
				defer func() { <-sem; r.updateAdvertisementAdmission() }()
				if err := tsarelay.TunnelConnection(ctx, conn); err != nil && ctx.Err() == nil {
					r.cfg.Logger.Debug("BLE timestamp HTTPS relay ended", zap.Error(err))
				}
			}()
		default:
			_ = conn.Close()
		}
	}
	return nil
}

func (r *runtime) acceptLoop(ctx context.Context, listener *l2Listener) error {
	// Bound unauthenticated workers too; policy reservations also include
	// established and pending outgoing connections.
	sem := make(chan struct{}, localmesh.BLEMaxConnections)
	for ctx.Err() == nil {
		conn, err := listener.Accept(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("BLE L2CAP accept: %w", err)
		}
		select {
		case sem <- struct{}{}:
			r.updateAdvertisementAdmission()
			r.links.Add(1)
			go func() {
				defer r.links.Done()
				defer func() { <-sem; r.updateAdvertisementAdmission() }()
				r.acceptLink(ctx, conn)
			}()
		default:
			_ = conn.Close()
		}
	}
	return nil
}

func (r *runtime) makeServerTLS() *tls.Config {
	config := &tls.Config{
		MinVersion: tls.VersionTLS13, MaxVersion: tls.VersionTLS13,
		// A hybrid ML-KEM key share expands the first ClientHello beyond six
		// 240-byte CoC SDUs on tested controllers. A single lost fragment stalls
		// the whole TLS flight. BLE uses classical X25519 key exchange while the
		// enrolled mTLS certificate and signature verification remain unchanged.
		CurvePreferences: []tls.CurveID{tls.X25519},
		// Present only the leaf to a client that named our issuer set (see
		// tls_issuers_linux.go). Without SNI, crypto/tls consults
		// GetCertificate only when Certificates is empty.
		GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
			return serverCertificate(r.cfg.Credentials, hello), nil
		},
		ClientAuth: tls.RequireAnyClientCert, NextProtos: []string{ALPN},
		VerifyConnection: func(state tls.ConnectionState) error {
			chain := make([][]byte, 0, len(state.PeerCertificates))
			for _, cert := range state.PeerCertificates {
				chain = append(chain, cert.Raw)
			}
			id, err := r.cfg.Credentials.Verify(chain, time.Now())
			if err != nil {
				return err
			}
			if id.Device.Tenant() != r.cfg.Credentials.Device.Tenant() {
				return errors.New("BLE inbound peer is outside the tenant")
			}
			if id.Device == r.cfg.Credentials.Device {
				return errors.New("BLE self connection")
			}
			return nil
		},
	}
	r.cfg.Credentials.ServerTicketStore(ALPN, "ble-tls").Configure(config)
	return config
}

func (r *runtime) acceptLink(ctx context.Context, raw net.Conn) {
	defer raw.Close()
	key := fmt.Sprintf("incoming:%p", raw)
	var incomingCandidate *candidate
	if radio, ok := raw.RemoteAddr().(l2addr); ok {
		kind := map[uint8]string{btAddrLEPublic: "public", btAddrLERandom: "random"}[radio.Type]
		if _, _, err := parseAddress(radio.Address, kind, radio.PSM); err == nil {
			c := candidate{address: radio.Address, addressType: kind}
			incomingCandidate = &c
			key = c.key()
		}
	}
	lease := r.connectionPolicy().Reserve(key, meshid.Device{}, false)
	if lease == nil {
		return
	}
	defer lease.Release()
	ctx = localmesh.WithConnectionLease(ctx, lease)
	stopWatch := watchLinkContext(ctx, raw)
	defer stopWatch()
	measured, meter := meterHandshake(lease.TrackIncoming(raw), r.cfg.Logger)
	secure := tls.Server(measured, r.serverTLS)
	meter.start()
	wait, elapsed, err := r.tlsGate.run(ctx, meshTLSAdmissionWait, meshTLSHandshakeTimeout, secure.HandshakeContext)
	measurement := meter.finish()
	state := secure.ConnectionState()
	var peer meshid.Device
	if err == nil && state.NegotiatedProtocol == ALPN {
		chain := make([][]byte, 0, len(state.PeerCertificates))
		for _, cert := range state.PeerCertificates {
			chain = append(chain, cert.Raw)
		}
		if id, verifyErr := r.cfg.Credentials.Verify(chain, time.Now()); verifyErr == nil && id.Device.Tenant() == r.cfg.Credentials.Device.Tenant() && id.Device != r.cfg.Credentials.Device {
			peer = id.Device
		}
	}
	measurement.log(r.cfg.Logger, "inbound", peer, state, err == nil && state.NegotiatedProtocol == ALPN && peer != (meshid.Device{}))
	if err != nil || state.NegotiatedProtocol != ALPN || peer == (meshid.Device{}) {
		r.cfg.Logger.Warn("BLE inbound TLS rejected", zap.Duration("admission_wait", wait), zap.Duration("handshake_time", elapsed), zap.Error(err))
		_ = secure.Close()
		return
	}
	lease.Identify(peer)
	if incomingCandidate != nil {
		lease.ObserveReady(func() { r.candidateQueue().Succeeded(key) })
		defer func() {
			if ctx.Err() == nil && !lease.Retired() && !r.hasCheaperLink(peer) {
				r.failedCandidate(*incomingCandidate)
			}
		}()
	}
	if r.cfg.Selection != nil {
		r.cfg.Selection.Connected(peer, localmesh.RadioBLE)
	}
	r.cfg.Logger.Info("BLE inbound TLS established", zap.Stringer("peer", peer.UUID()), zap.Duration("admission_wait", wait), zap.Duration("handshake_time", elapsed), zap.Bool("resumed", state.DidResume), zap.Int("peer_certificates", len(state.PeerCertificates)))
	if r.hasCheaperLink(peer) && (r.confirmCheaperLink(ctx, nil, peer) || ctx.Err() != nil) {
		r.cfg.Logger.Info("BLE closing inbound CoC on cheaper-link decision", zap.Stringer("peer", peer.UUID()))
		_ = secure.Close()
		return
	}
	defer secure.Close()
	r.rememberInboundRadio(raw, peer)
	r.requestAdvertisementRefresh()
	stopCheaperWatch := r.watchCheaperLink(ctx, peer, secure)
	defer stopCheaperWatch()
	attached := time.Now()
	err = r.cfg.Node.AttachStream(localmesh.WithCarrier(ctx, "ble"), peer, secure, LinkCost)
	r.cfg.Logger.Info("BLE link ended", zap.Stringer("peer", peer.UUID()),
		zap.String("direction", "inbound"), zap.Duration("lifetime", time.Since(attached)),
		zap.Bool("cancelled", ctx.Err() != nil), zap.Error(err))
}

// Discovery hints can match for many UUIDv7 devices. Remember an authenticated
// inbound channel's actual radio too, so its next advertisement does not start
// another TLS flight while this principal is already active. This uses the
// accepted socket address, never an advertiser's claim about its identity.
func (r *runtime) rememberInboundRadio(raw net.Conn, peer meshid.Device) {
	radio, ok := raw.RemoteAddr().(l2addr)
	if !ok {
		return
	}
	kind := map[uint8]string{btAddrLEPublic: "public", btAddrLERandom: "random"}[radio.Type]
	if _, address, err := parseAddress(radio.Address, kind, radio.PSM); err != nil || address == ([6]byte{}) {
		return
	}
	key := (candidate{address: radio.Address, addressType: kind}).key()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.known == nil {
		r.known = make(map[string]meshid.Device)
	}
	if _, exists := r.known[key]; exists || len(r.known) < maxPeerRetryHistory {
		r.known[key] = peer
	}
}

func (r *runtime) scanLoop(ctx context.Context, bus *dbus.Conn, adapter dbus.ObjectPath) error {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	consecutiveErrors := 0
	for {
		r.updateAdvertisementAdmission()
		r.governDiscovery(ctx, bus, adapter)
		if err := r.scanOnce(ctx, bus, adapter); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if errors.Is(err, errBlueZOwnerChanged) || errors.Is(err, errBLEAdapterUnavailable) {
				return err
			}
			consecutiveErrors++
			r.cfg.Logger.Warn("BLE discovery unavailable", zap.Error(err), zap.Int("consecutive_errors", consecutiveErrors))
			if consecutiveErrors >= 3 {
				return fmt.Errorf("BlueZ discovery failed three times: %w", err)
			}
		} else {
			consecutiveErrors = 0
		}
		select {
		case <-ctx.Done():
			return nil
		case err := <-r.reportErrors:
			r.reportErr = err
			r.publishDiscoveryHealth(ctx)
		case <-ticker.C:
		}
	}
}

// Existing scan-loop cadence observes reservations, not only authenticated
// peers. A coalesced request updates this provider's advertisement instance;
// time relay stays independently advertised while native admission is full.
func (r *runtime) updateAdvertisementAdmission() {
	caps := byte(0)
	if r.timeSlots != nil && len(r.timeSlots) < cap(r.timeSlots) {
		caps |= CapabilityTimeRelay
	}
	if r.connectionPolicy().IncomingAvailable() {
		caps |= CapabilityNativeAccepting
	}
	r.mu.Lock()
	changed := r.advertisement.Capabilities != caps
	r.advertisement.Capabilities = caps
	r.mu.Unlock()
	if changed {
		r.requestAdvertisementRefresh()
	}
}

// scanToggleDwell is the minimum time between discovery pause/resume
// transitions. Toggling reconfigures the controller's scan state; without a
// dwell, link flaps would churn it every few seconds.
const scanToggleDwell = 10 * time.Second

// governDiscovery pauses discovery when the outgoing target or total capacity
// is occupied, and resumes it when a slot opens. Paused devices keep
// advertising (inbound joins still work) but stop spending ~half the radio
// on active scanning. Returns whether scanning is currently enabled.
func (r *runtime) governDiscovery(ctx context.Context, bus *dbus.Conn, adapter dbus.ObjectPath) bool {
	target := localmesh.BLEMaxOutgoing
	paused := r.scanPaused
	if now := time.Now(); now.Sub(r.lastScanToggle) >= scanToggleDwell {
		if counts := r.connectionPolicy().Counts(); (counts.Outgoing >= target || counts.Total >= localmesh.BLEMaxConnections) != paused {
			wantPaused := counts.Outgoing >= target || counts.Total >= localmesh.BLEMaxConnections
			if wantPaused {
				if err := stopBlueZDiscovery(ctx, bus, adapter); err != nil {
					r.reportDiscoveryHealth(ctx, fmt.Errorf("pausing BLE discovery: %w", err))
					r.cfg.Logger.Debug("BLE discovery pause unavailable", zap.Error(err))
					return !paused
				}
			} else if err := startBlueZDiscovery(ctx, bus, adapter); err != nil {
				r.reportDiscoveryHealth(ctx, fmt.Errorf("resuming BLE discovery: %w", err))
				r.cfg.Logger.Debug("BLE discovery resume unavailable", zap.Error(err))
				return !paused
			}
			r.reportDiscoveryHealth(ctx, nil)
			if r.freshness != nil {
				r.freshness.clearReports()
			}
			paused = wantPaused
			r.scanPaused = paused
			r.lastScanToggle = now
			r.cfg.Logger.Info("BLE discovery pause toggled", zap.Bool("paused", paused), zap.Int("peers", r.blePeerCount()), zap.Int("target", target))
		}
	}
	return !paused
}

// blePeerCount reports distinct established BLE neighbours for diagnostics.
func (r *runtime) blePeerCount() int {
	seen := make(map[meshid.Device]struct{})
	if node, ok := r.cfg.Node.(interface{ Snapshot() localmesh.NodeSnapshot }); ok {
		for _, link := range node.Snapshot().Links {
			if link.Cost == LinkCost {
				seen[link.Device] = struct{}{}
			}
		}
	}
	return len(seen)
}

func (r *runtime) scanOnce(ctx context.Context, bus *dbus.Conn, adapter dbus.ObjectPath) error {
	query, cancel := context.WithTimeout(ctx, 3*time.Second)
	lookup := r.ownerLookup
	if lookup == nil {
		lookup = bluezOwner
	}
	owner, err := lookup(query, bus)
	if err != nil {
		cancel()
		return err
	}
	if err := verifyBlueZOwner(r.owner, owner); err != nil {
		cancel()
		return err
	}
	objects, err := getManaged(query, bus)
	cancel()
	if err != nil {
		return err
	}
	if err := requirePoweredAdapter(objects, adapter); err != nil {
		return err
	}
	// Pausing radio discovery must not pause adapter/daemon health monitoring.
	if r.scanPaused {
		return nil
	}
	now := time.Now()
	peers := r.discoveredCandidates(objects, now)
	r.startCandidates(ctx, peers, now)

	r.maybeRestartDiscovery(ctx, bus, adapter, now)
	return nil
}

// startCandidates never penalises a candidate skipped by capacity or pacing.
func (r *runtime) startCandidates(ctx context.Context, peers []candidate, now time.Time) {
	byKey := make(map[string]candidate, len(peers))
	keys := make([]string, 0, len(peers))
	for _, peer := range peers {
		byKey[peer.key()] = peer
		keys = append(keys, peer.key())
	}
	for _, key := range r.candidateQueue().Order(keys, now) {
		peer := byKey[key]
		if r.freshness != nil && !r.freshness.nativeReady(peer, now) {
			continue
		}
		lease := r.reserveCandidate(peer)
		if lease == nil {
			continue
		}
		if !r.admitDial(now) {
			lease.Release()
			break
		}
		lease.ObserveReady(func() { r.candidateQueue().Succeeded(key) })
		r.links.Add(1)
		go func() {
			defer r.links.Done()
			defer lease.Release()
			skipped := r.dialLink(localmesh.WithConnectionLease(ctx, lease), peer)
			known, _ := r.knownCandidate(peer)
			if !skipped && ctx.Err() == nil && !lease.Retired() && (known == (meshid.Device{}) || !r.hasCheaperLink(known)) {
				r.failedCandidate(peer)
			}
		}()
	}

}

func (r *runtime) discoveredCandidates(objects managedObjects, at time.Time) []candidate {
	// Device1.ServiceData is retained by BlueZ after an advertiser stops.
	// Only live validated advertising observations may refresh radio selection
	// or initiate a new CoC dial.
	peers, newlySeen := r.freshness.candidates(objects, at)
	if len(newlySeen) > 0 {
		r.discovery.observed(at)
	}
	if r.cfg.Selection != nil {
		for _, path := range newlySeen {
			for _, peer := range peers {
				if peer.path == path {
					if device, ok := r.knownCandidate(peer); ok {
						r.cfg.Selection.Seen(device, localmesh.RadioBLE)
					}
				}
			}
		}
	}
	return peers
}

func (r *runtime) missingPeerSlots() bool {
	counts := r.connectionPolicy().Counts()
	return counts.Outgoing < localmesh.BLEMaxOutgoing && counts.Total < localmesh.BLEMaxConnections
}

func (r *runtime) maybeRestartDiscovery(ctx context.Context, bus *dbus.Conn, adapter dbus.ObjectPath, now time.Time) {
	// Never fight the governor: a paused device stays paused until a peer
	// slot opens.
	if r.scanPaused {
		return
	}
	if !r.discovery.restartDue(now, r.missingPeerSlots()) {
		return
	}
	restart := r.restartScan
	if restart == nil {
		restart = restartBlueZDiscovery
	}
	restartCtx, cancel := context.WithTimeout(ctx, 6*time.Second)
	err := restart(restartCtx, bus, adapter)
	cancel()
	if err != nil {
		r.reportDiscoveryHealth(ctx, fmt.Errorf("restarting BLE discovery: %w", err))
		r.cfg.Logger.Warn("BLE discovery restart failed", zap.Duration("wendy_net_advert_silence", now.Sub(r.discovery.lastMeshSignal)), zap.Error(err))
		return
	}
	r.reportDiscoveryHealth(ctx, nil)
	if r.freshness != nil {
		r.freshness.clearReports()
	}
	r.cfg.Logger.Info("BLE discovery restarted after wendy net advertisement silence", zap.Duration("wendy_net_advert_silence", now.Sub(r.discovery.lastMeshSignal)))
}

func (r *runtime) dialLink(ctx context.Context, peer candidate) (skipped bool) {
	if known, ok := r.knownCandidate(peer); ok && r.hasCheaperLink(known) {
		r.cfg.Logger.Info("BLE closing outbound CoC on cheaper-link decision", zap.String("radio", peer.address))
		return
	}
	// Reserve the complete outgoing setup before opening a CoC. Its remote
	// inbound handshake must never wait for a local slot held by this same
	// connection. One outgoing setup and one inbound TLS flight can progress
	// independently, including simultaneous dials with equal UUID hints.
	queued := time.Now()
	select {
	case r.dialSlot <- struct{}{}:
	case <-ctx.Done():
		return
	}
	slotWait := time.Since(queued)
	slotHeld := true
	defer func() {
		if slotHeld {
			<-r.dialSlot
		}
	}()
	if ctx.Err() != nil {
		return
	}
	if r.freshness != nil && !r.freshness.nativeReady(peer, time.Now()) {
		return true
	}
	// Multiple BlueZ advertisements share the controller's advertising time.
	// A BLE CoC connect can wait for the peer's next connectable advertisement
	// while the local adapter scans and advertises at the same time.
	dialCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	dial := r.dialCoC
	if dial == nil {
		dial = func(ctx context.Context, address, kind string, psm uint16) (net.Conn, error) {
			return dialL2CAP(ctx, r.local, address, kind, psm)
		}
	}
	raw, err := dial(dialCtx, peer.address, peer.addressType, peer.psm)
	cancel()
	if err != nil {
		// dialL2CAP closes its failed socket. A peer-level management abort
		// would also disconnect unrelated profiles sharing an established ACL,
		// including when only this new CoC's setup has timed out. Leave shared
		// ACL lifetime and pending-connection teardown to the kernel.
		r.noteDialTimeout(peer.key(), errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil)
		r.cfg.Logger.Debug("BLE CoC dial failed", zap.String("radio", peer.address), zap.Error(err))
		return
	}
	r.clearDialTimeouts(peer.key())
	// Own only this CoC. Device1.Disconnect also terminates unrelated GATT,
	// audio and other profiles sharing the peer's underlying ACL.
	defer raw.Close()
	// Capture the ACL identity before the context watcher can close/reuse the
	// CoC descriptor. The early update is limited to this Wendy CoC's ACL.
	aclHandle, handleErr := meshACLHandle(raw)
	stopWatch := watchLinkContext(ctx, raw)
	defer stopWatch()
	var secure *tls.Conn
	defer func() {
		if secure != nil {
			_ = secure.Close()
		}
	}()
	// Complete the initial controller procedure before TLS firstflight. An
	// accepted HCI update cannot be cancelled by cancelling its caller; any
	// uncertain result closes this attempt without sending TLS bytes.
	tuneStarted := time.Now()
	interval, untuned, tuneErr := tuneMeshBeforeTLS(ctx, func(updateCtx context.Context) (time.Duration, error) {
		if handleErr != nil {
			return 0, &leUpdateUnavailableError{err: handleErr}
		}
		return requestMeshConnectionInterval(updateCtx, r.hciIndex, aclHandle)
	})
	r.cfg.Logger.Info("BLE initial tune barrier completed", zap.String("radio", peer.address),
		zap.Duration("wait", time.Since(tuneStarted)), zap.Duration("interval", interval),
		zap.Bool("untuned_fallback", untuned), zap.Error(tuneErr))
	if tuneErr != nil && !untuned {
		return
	}
	cfg, authenticated, err := r.clientTLS(peer)
	if err != nil {
		return
	}
	// This is a per-connection config, so this BLE-only choice cannot alter
	// the QUIC curve preferences used over NAN, LAN, or TCP links.
	cfg.CurvePreferences = []tls.CurveID{tls.X25519}
	measured, meter := meterHandshake(raw, r.cfg.Logger)
	secure = tls.Client(measured, cfg)
	meter.start()
	elapsed, err := r.handshakeOutbound(ctx, secure)
	wait := slotWait
	measurement := meter.finish()
	measurement.log(r.cfg.Logger, "outbound", *authenticated, secure.ConnectionState(), err == nil && secure.ConnectionState().NegotiatedProtocol == ALPN)
	if err != nil || secure.ConnectionState().NegotiatedProtocol != ALPN {
		r.mu.Lock()
		delete(r.known, peer.key()) // Address reuse or reenrollment requires fresh discovery authentication.
		r.mu.Unlock()
		r.cfg.Logger.Warn("BLE outbound TLS rejected", zap.String("radio", peer.address), zap.Duration("admission_wait", wait), zap.Duration("handshake_time", elapsed), zap.Error(err))
		return
	}
	device := *authenticated
	r.mu.Lock()
	if r.known == nil {
		r.known = make(map[string]meshid.Device)
	}
	// At capacity, omit the reconnect optimization. A new address still gets
	// a full verified handshake; it never inherits another address's pin.
	if _, exists := r.known[peer.key()]; exists || len(r.known) < maxPeerRetryHistory {
		r.known[peer.key()] = device
	}
	r.mu.Unlock()
	if r.cfg.Selection != nil {
		r.cfg.Selection.Connected(device, localmesh.RadioBLE)
	}
	r.cfg.Logger.Info("BLE outbound TLS established", zap.String("radio", peer.address), zap.Duration("admission_wait", wait), zap.Duration("handshake_time", elapsed), zap.Bool("resumed", secure.ConnectionState().DidResume), zap.Int("peer_certificates", len(secure.ConnectionState().PeerCertificates)))
	if r.hasCheaperLink(device) && (r.confirmCheaperLink(ctx, nil, device) || ctx.Err() != nil) {
		r.cfg.Logger.Debug("BLE closing outbound CoC on cheaper-link decision", zap.String("radio", peer.address))
		return
	}
	stopCheaperWatch := r.watchCheaperLink(ctx, device, secure)
	defer stopCheaperWatch()
	// Reassert the steady-state interval after authentication. Controller
	// scheduling and peer updates can change the negotiated interval, even
	// when the initial and steady-state preferences are the same. This is
	// best-effort and never fatal to an authenticated link.
	if handleErr == nil {
		relaxCtx, relaxCancel := context.WithTimeout(ctx, meshInitialTuneBudget)
		relaxed, _, relaxErr := tuneMeshBeforeTLS(relaxCtx, func(updateCtx context.Context) (time.Duration, error) {
			return requestSteadyConnectionInterval(updateCtx, r.hciIndex, aclHandle)
		})
		relaxCancel()
		if relaxErr != nil {
			r.cfg.Logger.Debug("BLE steady-state interval relax unavailable", zap.String("peer", device.Principal()), zap.Error(relaxErr))
		} else {
			r.cfg.Logger.Info("BLE steady-state interval relaxed", zap.String("peer", device.Principal()), zap.Duration("interval", relaxed))
		}
	}
	<-r.dialSlot
	slotHeld = false
	attached := time.Now()
	err = r.cfg.Node.AttachStream(localmesh.WithCarrier(ctx, "ble"), device, secure, LinkCost)
	r.cfg.Logger.Info("BLE link ended", zap.String("radio", peer.address), zap.Stringer("peer", device.UUID()),
		zap.String("direction", "outbound"), zap.Duration("lifetime", time.Since(attached)),
		zap.Bool("cancelled", ctx.Err() != nil), zap.Error(err))
	return
}

func (r *runtime) handshakeOutbound(ctx context.Context, secure *tls.Conn) (time.Duration, error) {
	// dialSlot already serializes outgoing setup, starting before CoC creation.
	// Sharing inbound admission here deadlocks crossed discovery: each device
	// can hold an outgoing slot while its peer waits for that slot to accept.
	handshakeCtx, cancel := context.WithTimeout(ctx, meshTLSOutboundTimeout)
	defer cancel()
	started := time.Now()
	err := secure.HandshakeContext(handshakeCtx)
	return time.Since(started), err
}

// Bound initial tuning before TLS firstflight so controller procedures cannot
// consume the peer's entire handshake budget. No post-authentication update
// races the stream-hello deadline at this boundary.
const meshInitialTuneBudget = 4 * time.Second

func tuneMeshBeforeTLS(ctx context.Context, tune func(context.Context) (time.Duration, error)) (time.Duration, bool, error) {
	updateCtx, cancel := context.WithTimeout(ctx, meshInitialTuneBudget)
	defer cancel()
	interval, err := tune(updateCtx)
	if ctx.Err() != nil {
		return 0, false, ctx.Err()
	}
	var unavailable *leUpdateUnavailableError
	var rejected *leUpdateRejectedError
	var completed *leUpdateCompletedError
	return interval, errors.As(err, &unavailable) || errors.As(err, &rejected) || errors.As(err, &completed), err
}

func watchLinkContext(ctx context.Context, conn net.Conn) func() {
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-done:
		}
	}()
	return func() { close(done) }
}

// First contact verifies the tenant, complete UUID and advertised prefix.
// Reconnects additionally pin the previously authenticated full principal.
func (r *runtime) clientTLS(peer candidate) (*tls.Config, *meshid.Device, error) {
	var authenticated meshid.Device
	firstContact := &firstContactSessionCache{}
	config := &tls.Config{
		MinVersion: tls.VersionTLS13, MaxVersion: tls.VersionTLS13,
		Certificates: []tls.Certificate{r.cfg.Credentials.Certificate},
		// SECURITY: VerifyConnection below enforces current PKI trust, tenant
		// and device identity on full and resumed handshakes. DNS-name
		// verification cannot authenticate a device discovered by radio hint.
		InsecureSkipVerify: true,
		NextProtos:         []string{ALPN},
		ClientSessionCache: firstContact,
	}
	if known, ok := r.knownCandidate(peer); ok {
		firstContact = nil
		var err error
		config, err = r.cfg.Credentials.PeerTLSWithTickets(known, ALPN, "ble-tls")
		if err != nil {
			return nil, nil, err
		}
	}
	var server []*x509.Certificate
	previous := config.VerifyConnection
	config.VerifyConnection = func(state tls.ConnectionState) error {
		if previous != nil {
			if err := previous(state); err != nil {
				return err
			}
		}
		raw := make([][]byte, len(state.PeerCertificates))
		for i, cert := range state.PeerCertificates {
			raw[i] = cert.Raw
		}
		identity, err := r.cfg.Credentials.Verify(raw, time.Now())
		if err != nil {
			return err
		}
		device := identity.Device
		if device.Tenant() != r.cfg.Credentials.Device.Tenant() || device == r.cfg.Credentials.Device {
			return errors.New("BLE outbound peer must be a different device in this tenant")
		}
		if deviceHint(device.UUID()) != peer.hint {
			return errors.New("BLE certificate does not match discovery hint")
		}
		if firstContact != nil && state.NegotiatedProtocol == ALPN {
			// The verified principal, not the radio address or truncated hint,
			// selects the cache scope and canonical key for post-handshake tickets.
			// Cache setup failure only loses this optimization; TLS remains fully
			// authenticated and the next connection can perform a full handshake.
			if pinned, err := r.cfg.Credentials.PeerTLSWithTickets(device, ALPN, "ble-tls"); err == nil {
				firstContact.bind(pinned)
			}
		}
		authenticated = device
		server = state.PeerCertificates
		return nil
	}
	if digest := r.cfg.Credentials.IssuerDigest(); digest != "" {
		config.NextProtos = []string{ALPN, issuerHintPrefix + digest}
		// crypto/tls runs VerifyConnection on the server's certificate
		// before it calls GetClientCertificate.
		config.GetClientCertificate = func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			return clientCertificate(r.cfg.Credentials, server), nil
		}
	}
	return config, &authenticated, nil
}
