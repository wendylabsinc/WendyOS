//go:build linux

package bleprovider

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"
	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"go.uber.org/zap"
)

type runtime struct {
	cfg                  Config
	uuid                 string
	owner                string
	hciIndex             int
	ownerLookup          func(context.Context, *dbus.Conn) (string, error)
	bus                  *dbus.Conn
	freshness            *advertisementFreshness
	mu                   sync.Mutex
	active               map[int32]struct{}
	nextAttempt          map[int32]time.Time
	links                sync.WaitGroup
	dialSlot             chan struct{}
	serverTLS            *tls.Config
	advertisementRefresh chan struct{}
}

type cheaperLinkChecker interface {
	HasCheaperLink(asset int32, cost uint16) bool
}

func (r *runtime) hasCheaperLink(asset int32) bool {
	checker, ok := r.cfg.Node.(cheaperLinkChecker)
	return ok && checker.HasCheaperLink(asset, LinkCost)
}

func (r *runtime) watchCheaperLink(ctx context.Context, asset int32, conn net.Conn) func() {
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
				if r.hasCheaperLink(asset) {
					_ = conn.Close()
					return
				}
			}
		}
	}()
	return func() { close(stop) }
}

func (r *runtime) claim(asset int32) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.active[asset]; exists || len(r.active) >= r.cfg.TargetPeers {
		return false
	}
	r.active[asset] = struct{}{}
	return true
}
func (r *runtime) release(asset int32) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.active, asset)
	r.nextAttempt[asset] = time.Now().Add(5 * time.Second)
}
func (r *runtime) shouldDial(asset int32) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, active := r.active[asset]
	return !active && len(r.active) < r.cfg.TargetPeers && !time.Now().Before(r.nextAttempt[asset])
}

// Run advertises, scans, listens for CoC channels, and initiates channels to
// lower-numbered peers concurrently. It blocks until ctx ends. BlueZ data is
// only a discovery hint: every link requires a Wendy asset certificate pinned
// to the expected organization and peer asset before AttachStream is called.
func Run(ctx context.Context, cfg Config) error {
	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	if err := cfg.defaults(); err != nil {
		return err
	}
	uuid, err := ServiceUUID(cfg.Credentials.Org)
	if err != nil {
		return err
	}
	adv, err := NewAdvertisement(cfg.Credentials.Asset, cfg.MeshName, cfg.PSM)
	if err != nil {
		return err
	}
	payload, _ := adv.MarshalBinary()
	listener, err := listenL2CAP(cfg.PSM)
	if err != nil {
		return err
	}
	defer listener.Close()
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
	freshness := newAdvertisementFreshness(adapter, uuid, cfg.MeshName, cfg.Credentials.Asset)
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
	hciIndex, parseErr := adapterHCIIndex(adapter)
	if parseErr != nil {
		cfg.Logger.Debug("BLE connection interval tuning unavailable", zap.Error(parseErr))
	}
	r := &runtime{cfg: cfg, uuid: uuid, owner: owner, hciIndex: hciIndex, ownerLookup: bluezOwner, bus: bus, freshness: freshness, active: make(map[int32]struct{}), nextAttempt: make(map[int32]time.Time), dialSlot: make(chan struct{}, 1)}
	r.serverTLS = r.makeServerTLS()
	r.advertisementRefresh = make(chan struct{}, 1)
	var loops sync.WaitGroup
	loopErrors := make(chan error, 3)
	loops.Add(3)
	go func() { defer loops.Done(); loopErrors <- r.acceptLoop(runCtx, listener) }()
	go func() { defer loops.Done(); loopErrors <- r.scanLoop(runCtx, bus, adapter) }()
	go func() {
		defer loops.Done()
		loopErrors <- runAdvertisementRefresh(runCtx, r.advertisementRefresh, advertisementRefreshInterval, func(ctx context.Context) error {
			return refreshOwnedAdvertisement(ctx, bus, closeTransport, owner, uuid, payload)
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
	loops.Wait()
	r.links.Wait()
	return loopErr
}

func (r *runtime) acceptLoop(ctx context.Context, listener *l2Listener) error {
	sem := make(chan struct{}, 8)
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
			r.links.Add(1)
			go func() { defer r.links.Done(); defer func() { <-sem }(); r.acceptLink(ctx, conn) }()
		default:
			_ = conn.Close()
		}
	}
	return nil
}

func (r *runtime) makeServerTLS() *tls.Config {
	config := &tls.Config{
		MinVersion: tls.VersionTLS13, MaxVersion: tls.VersionTLS13,
		Certificates: []tls.Certificate{r.cfg.Credentials.Certificate},
		ClientAuth:   tls.RequireAnyClientCert, NextProtos: []string{ALPN},
		VerifyConnection: func(state tls.ConnectionState) error {
			chain := make([][]byte, 0, len(state.PeerCertificates))
			for _, cert := range state.PeerCertificates {
				chain = append(chain, cert.Raw)
			}
			id, err := r.cfg.Credentials.Verify(chain, time.Now())
			if err != nil {
				return err
			}
			if id.Org != r.cfg.Credentials.Org || id.Asset <= r.cfg.Credentials.Asset {
				return errors.New("BLE inbound peer must be a higher asset in this organization")
			}
			return nil
		},
	}
	localmesh.NewTicketStore().Configure(config)
	return config
}

func (r *runtime) acceptLink(ctx context.Context, raw net.Conn) {
	defer raw.Close()
	stopWatch := watchLinkContext(ctx, raw)
	defer stopWatch()
	measured, meter := meterHandshake(raw, r.cfg.Logger)
	secure := tls.Server(measured, r.serverTLS)
	handshake, cancel := context.WithTimeout(ctx, 10*time.Second)
	meter.start()
	err := secure.HandshakeContext(handshake)
	measurement := meter.finish()
	cancel()
	state := secure.ConnectionState()
	var peer int32
	if err == nil && state.NegotiatedProtocol == ALPN {
		chain := make([][]byte, 0, len(state.PeerCertificates))
		for _, cert := range state.PeerCertificates {
			chain = append(chain, cert.Raw)
		}
		if id, verifyErr := r.cfg.Credentials.Verify(chain, time.Now()); verifyErr == nil && id.Org == r.cfg.Credentials.Org && id.Asset > r.cfg.Credentials.Asset {
			peer = id.Asset
		}
	}
	measurement.log(r.cfg.Logger, "inbound", peer, state, err == nil && state.NegotiatedProtocol == ALPN && peer != 0)
	if err != nil || state.NegotiatedProtocol != ALPN || peer == 0 {
		r.cfg.Logger.Debug("BLE inbound TLS rejected", zap.Error(err))
		_ = secure.Close()
		return
	}
	if r.hasCheaperLink(peer) {
		_ = secure.Close()
		return
	}
	if !r.claim(peer) {
		_ = secure.Close()
		return
	}
	defer r.release(peer)
	defer secure.Close()
	r.requestAdvertisementRefresh()
	stopCheaperWatch := r.watchCheaperLink(ctx, peer, secure)
	defer stopCheaperWatch()
	if err = r.cfg.Node.AttachStream(ctx, peer, secure, LinkCost); err != nil && ctx.Err() == nil {
		r.cfg.Logger.Debug("BLE link ended", zap.Int32("peer", peer), zap.Error(err))
	}
}

func (r *runtime) scanLoop(ctx context.Context, bus *dbus.Conn, adapter dbus.ObjectPath) error {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	consecutiveErrors := 0
	for {
		if err := r.scanOnce(ctx, bus, adapter); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if errors.Is(err, errBlueZOwnerChanged) {
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
		case <-ticker.C:
		}
	}
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
	peers := r.discoveredCandidates(objects, time.Now())
	for _, peer := range peers {
		if r.hasCheaperLink(peer.asset) {
			continue
		}
		if !r.shouldDial(peer.asset) {
			continue
		}
		if !r.claim(peer.asset) {
			continue
		}
		r.links.Add(1)
		go func(peer candidate) { defer r.links.Done(); defer r.release(peer.asset); r.dialLink(ctx, peer) }(peer)
	}
	return nil
}

func (r *runtime) discoveredCandidates(objects managedObjects, at time.Time) []candidate {
	// Device1.ServiceData is retained by BlueZ after an advertiser stops.
	// Only live ServiceData discovery signals may refresh radio selection or
	// initiate a new CoC dial.
	peers, _ := r.freshness.candidates(objects, at)
	return peers
}

func (r *runtime) dialLink(ctx context.Context, peer candidate) {
	if r.hasCheaperLink(peer.asset) {
		return
	}
	// Serializing outgoing LE creation avoids accumulating simultaneous pending
	// ACLs while the adapter scans and advertises for other mesh peers.
	select {
	case r.dialSlot <- struct{}{}:
	case <-ctx.Done():
		return
	}
	slotHeld := true
	defer func() {
		if slotHeld {
			<-r.dialSlot
		}
	}()
	if r.hasCheaperLink(peer.asset) || ctx.Err() != nil {
		return
	}
	// Multiple BlueZ advertisements share the controller's advertising time.
	// A BLE CoC connect can wait for the peer's next connectable advertisement
	// while the local adapter scans and advertises at the same time.
	dialCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	raw, err := dialL2CAP(dialCtx, peer.address, peer.addressType, peer.psm)
	cancel()
	if err != nil {
		// A timed-out socket connect can leave a controller attempt pending.
		// Clear only this Wendy peer before another outgoing dial starts.
		r.disconnectOwnedPeer(peer)
		r.cfg.Logger.Debug("BLE CoC dial failed", zap.Int32("peer", peer.asset), zap.Error(err))
		return
	}
	<-r.dialSlot
	slotHeld = false
	// Closing the CoC socket alone can leave its LE ACL held by BlueZ. The
	// higher-asset dialer owns this link and releases that specific peer after
	// every failed attempt, route suppression, or provider shutdown.
	defer func() {
		if raw != nil {
			_ = raw.Close()
		}
		r.disconnectOwnedPeer(peer)
	}()
	// Capture the ACL identity before the context watcher can close/reuse the
	// CoC descriptor. The early update is limited to this Wendy CoC's ACL.
	aclHandle, handleErr := meshACLHandle(raw)
	stopWatch := watchLinkContext(ctx, raw)
	defer stopWatch()
	var authenticated func()
	var finishTune func()
	var stopTune func()
	var secure *tls.Conn
	defer func() {
		if stopTune != nil {
			stopTune()
		}
		if secure != nil {
			_ = secure.Close()
		}
	}()
	if r.hciIndex >= 0 && handleErr == nil {
		// Update only this Wendy ACL as soon as the CoC exposes its handle.
		// TLS proceeds concurrently, so a slow controller cannot delay peer
		// authentication. A rejected early request gets one authenticated retry.
		authenticated, finishTune, stopTune = startMeshIntervalTune(ctx, func(updateCtx context.Context) (time.Duration, error) {
			return requestMeshConnectionInterval(updateCtx, r.hciIndex, aclHandle)
		}, func(attempt int, interval time.Duration, updateErr error) {
			if updateErr != nil {
				r.cfg.Logger.Debug("BLE mesh connection interval unchanged", zap.Int32("peer", peer.asset), zap.Int("attempt", attempt), zap.Error(updateErr))
			} else {
				r.cfg.Logger.Info("BLE mesh connection interval updated", zap.Int32("peer", peer.asset), zap.Int("attempt", attempt), zap.Duration("interval", interval),
					zap.Duration("supervision_timeout", time.Duration(meshTimeoutUnits)*10*time.Millisecond),
					zap.Duration("requested_max_event_length", time.Duration(meshMaxEventLengthUnits)*625*time.Microsecond))
			}
		})
	} else if handleErr != nil {
		r.cfg.Logger.Debug("BLE mesh connection interval unavailable", zap.Int32("peer", peer.asset), zap.Error(handleErr))
	}
	cfg, err := r.cfg.Credentials.PeerTLSWithTickets(peer.asset, ALPN, "ble-tls")
	if err != nil {
		return
	}
	measured, meter := meterHandshake(raw, r.cfg.Logger)
	secure = tls.Client(measured, cfg)
	handshake, cancel := context.WithTimeout(ctx, 10*time.Second)
	meter.start()
	err = secure.HandshakeContext(handshake)
	measurement := meter.finish()
	cancel()
	measurement.log(r.cfg.Logger, "outbound", peer.asset, secure.ConnectionState(), err == nil && secure.ConnectionState().NegotiatedProtocol == ALPN)
	if err != nil || secure.ConnectionState().NegotiatedProtocol != ALPN {
		r.cfg.Logger.Debug("BLE outbound TLS rejected", zap.Int32("peer", peer.asset), zap.Error(err))
		return
	}
	if r.hasCheaperLink(peer.asset) {
		return
	}
	if authenticated != nil {
		authenticated()
		// AttachStream may close the CoC itself on a read/write failure. Join
		// both controller attempts before it can release and reuse this ACL.
		finishTune()
	}
	stopCheaperWatch := r.watchCheaperLink(ctx, peer.asset, secure)
	defer stopCheaperWatch()
	err = r.cfg.Node.AttachStream(ctx, peer.asset, secure, LinkCost)
	if err != nil && ctx.Err() == nil {
		r.cfg.Logger.Debug("BLE link ended", zap.Int32("peer", peer.asset), zap.Error(err))
	}
}

func (r *runtime) disconnectOwnedPeer(peer candidate) {
	if r.bus == nil || !peer.path.IsValid() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := r.bus.Object(bluezName, peer.path).CallWithContext(ctx, deviceInterface+".Disconnect", 0).Err; err != nil {
		r.cfg.Logger.Debug("BLE owned ACL disconnect unavailable", zap.Int32("peer", peer.asset), zap.Error(err))
	}
}

// Start tuning before TLS, then retry only if the early controller request
// failed and the peer authenticated. The caller joins before closing its CoC
// socket so the ACL handle cannot be reused by another Wendy connection.
// The inbound peer starts its five-second stream hello deadline as soon as TLS
// completes, so controller work must yield well before that deadline.
const meshTunePostTLSBudget = 2 * time.Second

func startMeshIntervalTune(ctx context.Context, tune func(context.Context) (time.Duration, error), report func(int, time.Duration, error)) (func(), func(), func()) {
	ctx, cancel := context.WithCancel(ctx)
	authenticated := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for attempt := 1; attempt <= 2; attempt++ {
			if attempt == 2 {
				select {
				case <-authenticated:
				case <-ctx.Done():
					return
				}
			}
			updateCtx, stopUpdate := context.WithTimeout(ctx, 4*time.Second)
			interval, err := tune(updateCtx)
			stopUpdate()
			if ctx.Err() != nil {
				return
			}
			report(attempt, interval, err)
			if err == nil {
				return
			}
		}
	}()
	return func() { close(authenticated) }, func() {
		timer := time.NewTimer(meshTunePostTLSBudget)
		defer timer.Stop()
		select {
		case <-done:
		case <-timer.C:
			cancel()
			<-done
		}
	}, func() { cancel(); <-done }
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
