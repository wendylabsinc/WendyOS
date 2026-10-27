package ipcam

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
)

// LinkManager watches ethernet links for a camera that cannot get an address, and
// serves DHCP on the ones it is allowed to. LinkGuard holds the policy; this type
// holds the plumbing.
type LinkManager struct {
	reg    *Registry
	guard  *LinkGuard
	logger *zap.Logger

	// Injection seams. Unit tests replace all of them, so the manager's own logic
	// is exercised without a socket or a real interface.
	listInterfaces func() ([]Interface, error)
	carrier        func(name string) bool
	addAddress     func(link, cidr string) error
	delAddress     func(link, cidr string) error
	watch          func(ctx context.Context, link string, onPacket func(*Packet)) error
	serve          func(ctx context.Context, link string, seg CameraSegment, pool *LeasePool, onLease func(net.HardwareAddr, net.IP, string)) error
	now            func() time.Time
	openPool       func(string, CameraSegment, []Camera) (*LeasePool, error)

	// Serialize scans with operator configuration so a revoked link cannot be
	// claimed by a scan that is still using the previous permission.
	scanMu         sync.Mutex
	mu             sync.Mutex
	claimed        map[string]CameraSegment
	claimIndices   map[string]int
	addressErrors  map[string]string
	serverErrors   map[string]string
	pools          map[string]*LeasePool
	running        map[string]*linkWatch
	stop           map[string]context.CancelFunc // cancels a claimed link's server
	downFor        map[string]int                // consecutive scans with no carrier
	watchingSince  map[string]time.Time          // when this process began watching a link
	allowedDHCP    map[string]bool               // explicit, root-owned camera DHCP opt-ins
	allowlistPath  string
	readAllowlist  func(string) (map[string]bool, error)
	writeAllowlist func(string, map[string]bool) error
	leaseMu        sync.Mutex // lease storage I/O never holds mu or blocks cancellation
	leasePools     map[string]*LeasePool
}

type linkWatch struct {
	index  int
	cancel context.CancelFunc
}

// NewLinkManager returns a manager writing discovered cameras into reg.
func NewLinkManager(reg *Registry, logger *zap.Logger) *LinkManager {
	m := &LinkManager{
		reg:            reg,
		guard:          NewLinkGuard(),
		logger:         logger,
		now:            time.Now,
		claimed:        make(map[string]CameraSegment),
		claimIndices:   make(map[string]int),
		addressErrors:  make(map[string]string),
		serverErrors:   make(map[string]string),
		leasePools:     make(map[string]*LeasePool),
		openPool:       openLeasePool,
		pools:          make(map[string]*LeasePool),
		running:        make(map[string]*linkWatch),
		stop:           make(map[string]context.CancelFunc),
		downFor:        make(map[string]int),
		watchingSince:  make(map[string]time.Time),
		allowedDHCP:    make(map[string]bool),
		readAllowlist:  readDHCPAllowlist,
		writeAllowlist: writeDHCPAllowlist,
	}
	m.carrier = sysfsCarrier
	m.listInterfaces = func() ([]Interface, error) {
		ifaces, err := net.Interfaces()
		if err != nil {
			return nil, err
		}
		return InterfacesFrom(ifaces, m.carrier), nil
	}
	m.addAddress = addLinkAddress
	m.delAddress = delLinkAddress
	m.watch = watchDHCP
	m.serve = serveDHCP
	return m
}

// State exposes a link's decision, for diagnostics.
func (m *LinkManager) State(link string) LinkState { return m.guard.State(link) }

// sysfsCarrier reads whether a link has a cable in it. net.Interface does not
// expose carrier, only the administrative up flag, and those differ: an
// unconfigured interface can be up with no cable.
func sysfsCarrier(name string) bool {
	data, err := os.ReadFile(fmt.Sprintf("/sys/class/net/%s/carrier", name))
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(data)) == "1"
}

// scanInterval is how often links are re-examined for carrier and address
// changes. Cables do not move often, and each scan is a handful of file reads.
const scanInterval = 15 * time.Second

// Run watches links until ctx is cancelled.
func (m *LinkManager) Run(ctx context.Context) {
	ticker := time.NewTicker(scanInterval)
	defer ticker.Stop()
	for {
		m.scanOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// scanOnce examines every link once: start watching newly eligible ones, claim
// those whose DISCOVER went unanswered, and forget those that lost carrier.
func (m *LinkManager) scanOnce(ctx context.Context) {
	m.scanMu.Lock()
	defer m.scanMu.Unlock()
	if err := m.refreshDHCPAllowlist(); err != nil {
		m.logger.Warn("camera DHCP disabled: invalid allowlist", zap.Error(err))
	}
	m.withdrawRevokedClaims()
	ifaces, err := m.listInterfaces()
	if err != nil {
		m.logger.Debug("listing interfaces failed", zap.Error(err))
		return
	}
	m.reconcileInterfaces(ifaces)
	for _, iface := range ifaces {
		if iface.AddressError != "" {
			// An unknown address on any port can hide an overlapping subnet.
			// Retain existing claims, but do not allocate or restore addresses
			// until the complete interface snapshot can be inspected again.
			m.logger.Debug("cannot inspect interface addresses", zap.String("link", iface.Name), zap.String("error", iface.AddressError))
			return
		}
	}

	for _, iface := range ifaces {
		// A claimed link carries our own address by design, so it necessarily
		// fails the eligibility test that got it claimed. Only a sustained loss of
		// carrier releases it; re-testing eligibility here would release the link
		// on the very next scan and then re-claim it on the next DISCOVER,
		// spawning a second server each time round.
		if seg, claimed := m.claimedSegment(iface.Name); claimed {
			// The watcher cancels serving immediately, but its queued cleanup
			// can be lost if it exits. Reconcile the recorded guard decision here,
			// after interface identity reconciliation and before restoring addresses.
			if m.guard.State(iface.Name) == LinkDisqualified {
				m.withdrawDisqualifiedClaim(iface.Name)
				continue
			}
			if m.carrierLost(iface) {
				m.release(iface.Name)
				continue
			}
			if iface.conflictsWithCameraSegment(seg) {
				m.logger.Warn("camera link has conflicting IP configuration; stopping DHCP",
					zap.String("link", iface.Name))
				m.guard.Disqualify(iface.Name)
				m.withdrawDisqualifiedClaim(iface.Name)
				continue
			}
			// Carrier alone is not proof the segment still works. Whatever else
			// manages this interface can strip the address we added, and nothing
			// tells us when it does: the DHCP watcher is a packet socket and the
			// server binds INADDR_ANY, so leases keep flowing on an addressless
			// link while every route to the camera is gone.
			m.reassertAddress(iface, seg)
			continue
		}
		retained, hasRetained := iface.configuredCameraSegment()
		eligible := iface
		if m.dhcpAllowed(iface.Name) && hasRetained {
			eligible.HasIPv4 = false
		}
		if !eligible.Eligible() {
			if m.carrierLost(iface) {
				// A link that has really lost carrier is no longer a camera
				// segment. Forget it so the next cable is judged afresh.
				m.release(iface.Name)
			}
			continue
		}
		m.noteCarrierUp(iface.Name)
		m.ensureWatching(ctx, iface)
		// DHCP traffic alone cannot distinguish a camera from an uplink client.
		// Keep passive observation, but never claim an interface without an
		// explicit root-owned opt-in.
		if !m.dhcpAllowed(iface.Name) {
			continue
		}
		if hasRetained {
			if m.observedLongEnough(iface.Name) && m.guard.RestoreClaim(iface.Name) {
				m.claim(ctx, iface, &retained, ifaces)
			}
			continue
		}
		if seg, ok := m.restorableSegment(iface.Name); ok {
			if m.guard.RestoreClaim(iface.Name) {
				m.logger.Info("restoring camera link claim from a previous run",
					zap.String("link", iface.Name), zap.String("address", seg.CIDR()))
				m.claim(ctx, iface, &seg, ifaces)
			}
			continue
		}
		if m.guard.ShouldClaim(iface.Name, m.now()) {
			m.claim(ctx, iface, nil, ifaces)
		}
	}
}

func (m *LinkManager) dhcpAllowed(link string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.allowedDHCP[link]
}

// reassertAddress puts our segment address back on a claimed link that has lost
// it. Losing it is silent and permanent otherwise: scanOnce judges a claimed
// link on carrier only, so nothing would ever notice or retry.
func (m *LinkManager) reassertAddress(iface Interface, seg CameraSegment) {
	if iface.HasAddress(seg.ServerIP) {
		m.mu.Lock()
		delete(m.addressErrors, iface.Name)
		m.mu.Unlock()
		return
	}
	// Warn, not Info: the address disappearing under us means something else is
	// managing this interface, and that is worth seeing in production logs.
	m.logger.Warn("camera link lost its address, restoring it",
		zap.String("link", iface.Name), zap.String("cidr", seg.CIDR()))
	err := m.addAddress(iface.Name, seg.CIDR())
	m.mu.Lock()
	if err != nil {
		m.addressErrors[iface.Name] = err.Error()
	} else {
		delete(m.addressErrors, iface.Name)
	}
	m.mu.Unlock()
	if err != nil {
		// Deliberately not disqualifying: the cause is usually transient, and
		// disqualifying would strand the segment until carrier is lost. The next
		// scan tries again.
		m.logger.Warn("restoring camera link address failed",
			zap.String("link", iface.Name), zap.String("cidr", seg.CIDR()), zap.Error(err))
	}
}

// restorableSegment returns the segment a link should reclaim after an agent
// restart, and whether there is one.
//
// LinkManager state is in-process, so a restart forgets every claim, while the
// registry remembers the camera and the address it holds. Without this the link
// waits for the camera to speak DHCP again, which a camera holding a lease will
// not do for up to LeaseWindow — twelve hours during which it is unreachable.
//
// The competing-server guard is still honoured: the link must have been watched
// for unansweredWindow first, so a foreign OFFER has had the same chance to
// disqualify it as it does on a first claim.
func (m *LinkManager) restorableSegment(link string) (CameraSegment, bool) {
	if m.reg == nil {
		return CameraSegment{}, false
	}
	if !m.observedLongEnough(link) {
		return CameraSegment{}, false
	}
	for _, cam := range m.reg.List() {
		if cam.Link != link || cam.Address == "" {
			continue
		}
		index, ok := SegmentIndexFor(net.ParseIP(cam.Address))
		if !ok {
			continue
		}
		return SegmentFor(index), true
	}
	return CameraSegment{}, false
}

func (m *LinkManager) observedLongEnough(link string) bool {
	m.mu.Lock()
	since, watching := m.watchingSince[link]
	m.mu.Unlock()
	return watching && m.now().Sub(since) >= unansweredWindow
}

// ensureWatching starts one passive DHCP watcher per link.
func (m *LinkManager) ensureWatching(ctx context.Context, iface Interface) {
	link := iface.Name
	m.mu.Lock()
	if m.running[link] != nil {
		m.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(ctx)
	watch := &linkWatch{index: iface.Index, cancel: cancel}
	type watchEvent struct {
		packet  *Packet
		serving bool
	}
	events := make(chan watchEvent, 32)
	m.running[link] = watch
	// The clock a restored claim waits on: it is how long we have been listening
	// for a competing DHCP server on this link in this process.
	m.watchingSince[link] = m.now()
	m.mu.Unlock()

	// Keep this at Info so production journald/stderr logs show whether the agent
	// is watching the camera's link. Debug still reaches the telemetry core and
	// wendy device logs, regardless of the production output level.
	m.logger.Info("watching camera link for DHCP", zap.String("link", link))

	// Registry and address I/O must never stop the reader from inspecting the
	// next packet. The bounded queue may drop advisory metadata under load, but
	// every packet still reaches the guard synchronously before enqueueing.
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case event := <-events:
				m.processWatchPacket(ctx, link, watch, event.packet, event.serving)
			}
		}
	}()

	go func() {
		defer cancel()
		defer func() {
			m.mu.Lock()
			if m.running[link] == watch {
				delete(m.running, link)
				delete(m.watchingSince, link)
			}
			m.mu.Unlock()
		}()
		err := m.watch(ctx, link, func(p *Packet) {
			// Detection and cancellation must not wait for scanMu: a scan or
			// configuration write can be blocked in unrelated filesystem I/O.
			// The short manager lock ties this observation to this watcher
			// generation, so a renamed/replaced port cannot affect its successor.
			m.mu.Lock()
			if ctx.Err() != nil || m.running[link] != watch {
				m.mu.Unlock()
				return
			}
			m.guard.Observe(link, p, m.now())
			if m.guard.State(link) == LinkDisqualified {
				if stop := m.stop[link]; stop != nil {
					stop()
				}
			}
			// Do not promote a packet overheard before the claim merely because
			// deferred registry work runs after the DHCP server starts.
			_, claimed := m.claimed[link]
			serving := claimed && m.stop[link] != nil && m.guard.State(link) == LinkServing
			m.mu.Unlock()
			select {
			case events <- watchEvent{packet: p, serving: serving}:
			default:
				// A queued event also performs pending disqualification cleanup.
			}
		})
		if err != nil && ctx.Err() == nil {
			// A watch that cannot start is why a camera never appears, so this is
			// not a Debug-level detail.
			m.logger.Warn("dhcp watch on camera link ended",
				zap.String("link", link), zap.Error(err))
		}
	}()
}

// processWatchPacket performs deferred work only for the current watcher.
// Serving was already cancelled by the reader if this link was disqualified.
func (m *LinkManager) processWatchPacket(ctx context.Context, link string, watch *linkWatch, p *Packet, observedServing bool) {
	m.scanMu.Lock()
	defer m.scanMu.Unlock()
	m.mu.Lock()
	current := ctx.Err() == nil && m.running[link] == watch
	m.mu.Unlock()
	if !current {
		return
	}
	if m.guard.State(link) == LinkDisqualified {
		m.withdrawDisqualifiedClaim(link)
	}
	m.logger.Info("dhcp seen on camera link",
		zap.String("link", link), zap.String("type", p.Type.String()),
		zap.String("mac", p.CHAddr.String()), zap.String("hostname", p.Hostname))
	if observedServing {
		m.noteClient(link, p)
	}
}

// noteClient records clients only on a claimed camera link with a DHCP server.
// A hostname in a DISCOVER/REQUEST on any other link says nothing about cameras.
func (m *LinkManager) noteClient(link string, p *Packet) {
	if p.Type != Discover && p.Type != Request {
		return
	}
	if len(p.CHAddr) == 0 || m.reg == nil {
		return
	}
	m.mu.Lock()
	_, claimed := m.claimed[link]
	serving := claimed && m.stop[link] != nil
	m.mu.Unlock()
	if !serving || m.guard.State(link) != LinkServing {
		return
	}
	if _, err := m.reg.Upsert(Camera{
		MAC:        p.CHAddr.String(),
		Model:      p.Hostname,
		Link:       link,
		DHCPServed: true,
	}); err != nil {
		m.logger.Debug("recording dhcp client failed",
			zap.String("mac", p.CHAddr.String()), zap.Error(err))
	}
}

// claim configures the link and starts serving leases on it. A non-nil preferred
// segment is used when it is free, which is how a restored claim keeps the
// subnet the camera already holds a lease on.
func (m *LinkManager) claim(ctx context.Context, iface Interface, preferred *CameraSegment, ifaces []Interface) {
	link := iface.Name
	// All configured IPv4 networks on other ports reserve overlapping camera
	// segments, including multi-address ports that cannot resume camera DHCP.
	reserved := make(map[string]bool)
	for _, other := range ifaces {
		if other.Name == link {
			continue
		}
		for index := range 256 {
			seg := SegmentFor(index)
			if other.overlapsCameraSegment(seg) {
				reserved[seg.ServerIP.String()] = true
			}
		}
	}
	m.mu.Lock()
	if _, exists := m.claimed[link]; exists {
		m.mu.Unlock()
		return
	}
	seg, ok := m.chooseSegmentLocked(preferred, reserved)
	if iface.HasIPv4 && (preferred == nil || !seg.ServerIP.Equal(preferred.ServerIP)) {
		ok = false // Never add a different segment beside a retained address.
	}
	if !ok {
		m.mu.Unlock()
		m.logger.Warn("no camera link subnet available", zap.String("link", link))
		m.guard.Disqualify(link)
		return
	}
	m.mu.Unlock()
	pool, err := m.leasePoolFor(seg)
	if err != nil {
		m.mu.Lock()
		m.serverErrors[link] = "Cannot restore camera DHCP leases: " + err.Error()
		m.mu.Unlock()
		m.logger.Warn("restoring camera DHCP leases failed", zap.String("link", link), zap.Error(err))
		m.guard.Disqualify(link)
		return
	}
	m.mu.Lock()
	if m.guard.State(link) == LinkDisqualified {
		m.mu.Unlock()
		return
	}
	delete(m.serverErrors, link)
	m.claimed[link] = seg
	m.claimIndices[link] = iface.Index
	m.pools[link] = pool
	m.mu.Unlock()

	var addressErr error
	if !iface.HasAddress(seg.ServerIP) {
		addressErr = m.addAddress(link, seg.CIDR())
	}
	if addressErr != nil {
		// Without an address we cannot answer, and retrying forever would be
		// noise. Disqualify so the decision is visible rather than silent.
		//
		// dropClaim rather than release: release resets the guard, which would
		// erase the disqualification we are about to record.
		m.logger.Warn("configuring camera link failed",
			zap.String("link", link), zap.String("cidr", seg.CIDR()), zap.Error(addressErr))
		m.dropClaim(link)
		m.guard.Disqualify(link)
		return
	}
	// A per-link context so releasing the link actually stops its server, rather
	// than leaving one running for every claim the link ever had.
	serveCtx, cancel := context.WithCancel(ctx)
	m.mu.Lock()
	// A foreign DHCP reply may have disqualified and withdrawn this link while
	// addAddress was in progress. Do not resurrect it after that decision.
	if currentPool := m.pools[link]; currentPool != pool {
		m.mu.Unlock()
		cancel()
		if err := m.delAddress(link, seg.CIDR()); err != nil {
			m.logger.Warn("removing disqualified camera link address failed",
				zap.String("link", link), zap.String("cidr", seg.CIDR()), zap.Error(err))
		}
		return
	}
	m.stop[link] = cancel
	m.mu.Unlock()
	if m.guard.State(link) == LinkDisqualified {
		m.withdrawDisqualifiedClaim(link)
		return
	}
	m.logger.Info("serving DHCP on camera link",
		zap.String("link", link),
		zap.String("address", seg.CIDR()),
		zap.String("pool", fmt.Sprintf("%s+%d", seg.PoolBase, seg.PoolSize)))

	go func() {
		err := m.serve(serveCtx, link, seg, pool, func(mac net.HardwareAddr, addr net.IP, hostname string) {
			m.recordLease(link, mac, addr, hostname)
		})
		if err != nil && serveCtx.Err() == nil {
			m.logger.Warn("dhcp server on camera link exited",
				zap.String("link", link), zap.Error(err))
			m.recoverFailedServer(serveCtx, link, seg, pool, err)
		}
	}()
}

// Keep one pool per persisted subnet even when its interface is disabled or
// released. Cameras still hold leases, and a cancelled server can be finishing
// a disk write while its replacement starts. Sharing the pool serializes them.
func (m *LinkManager) leasePoolFor(seg CameraSegment) (*LeasePool, error) {
	m.leaseMu.Lock()
	defer m.leaseMu.Unlock()
	m.mu.Lock()
	path := filepath.Join(filepath.Dir(m.allowlistPath), "camera-dhcp-leases-"+seg.ServerIP.String()+".json")
	m.mu.Unlock()
	if pool := m.leasePools[path]; pool != nil {
		return pool, nil
	}
	var legacy []Camera
	if m.reg != nil {
		legacy = m.reg.List()
	}
	pool, err := m.openPool(path, seg, legacy)
	if err != nil {
		return nil, err
	}
	m.leasePools[path] = pool
	return pool, nil
}

// chooseSegmentLocked returns the segment a claim should use: the preferred one
// when it is free, otherwise the lowest unused. Callers hold m.mu.
func (m *LinkManager) chooseSegmentLocked(preferred *CameraSegment, reserved map[string]bool) (CameraSegment, bool) {
	if preferred != nil && !m.segmentUsedLocked(*preferred) && !reserved[preferred.ServerIP.String()] {
		return *preferred, true
	}
	return m.nextSegmentLocked(reserved)
}

// segmentUsedLocked reports whether a segment is already claimed by some link.
// Callers hold m.mu.
func (m *LinkManager) segmentUsedLocked(seg CameraSegment) bool {
	for _, claimed := range m.claimed {
		if claimed.ServerIP.Equal(seg.ServerIP) {
			return true
		}
	}
	return false
}

// nextSegmentLocked returns the lowest unused camera subnet. Reusing holes is
// important: len(m.claimed) can name a subnet that is still active after a
// lower-numbered link has been released. Callers hold m.mu.
func (m *LinkManager) nextSegmentLocked(reserved map[string]bool) (CameraSegment, bool) {
	for index := range 256 {
		if candidate := SegmentFor(index); !m.segmentUsedLocked(candidate) && !reserved[candidate.ServerIP.String()] {
			return candidate, true
		}
	}
	return CameraSegment{}, false
}

// recordLease puts a leased camera into the registry, which is how a camera that
// never answers an ONVIF probe still becomes listable.
func (m *LinkManager) recordLease(link string, mac net.HardwareAddr, addr net.IP, hostname string) {
	if m.reg == nil {
		return
	}
	cam, err := m.reg.Upsert(Camera{
		MAC:        mac.String(),
		Address:    addr.String(),
		Model:      hostname,
		Link:       link,
		DHCPServed: true,
	})
	if err != nil {
		m.logger.Warn("registering leased camera failed",
			zap.String("mac", mac.String()), zap.Error(err))
		return
	}
	m.logger.Info("camera leased an address",
		zap.String("link", link),
		zap.String("mac", mac.String()),
		zap.String("address", addr.String()),
		zap.String("model", hostname),
		zap.Uint32("cameraId", cam.ID))
}

func (m *LinkManager) isClaimed(link string) bool {
	_, ok := m.claimedSegment(link)
	return ok
}

// claimedSegment returns the segment a link is claimed with, if it is claimed.
func (m *LinkManager) claimedSegment(link string) (CameraSegment, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	seg, ok := m.claimed[link]
	return seg, ok
}

// reconcileInterfaces stops sockets whose original name no longer identifies
// their kernel interface. Permissions stay attached to the configured name;
// they are never silently transferred to a renamed port. Call with scanMu held.
func (m *LinkManager) reconcileInterfaces(ifaces []Interface) bool {
	byName := make(map[string]Interface)
	byIndex := make(map[int]Interface)
	for _, iface := range ifaces {
		byName[iface.Name] = iface
		if iface.Index != 0 {
			byIndex[iface.Index] = iface
		}
	}
	unchanged := func(name string, index int) bool {
		iface, present := byName[name]
		return present && iface.Index == index
	}
	m.mu.Lock()
	stale := make(map[string]int)
	for name, index := range m.claimIndices {
		if !unchanged(name, index) {
			stale[name] = index
		}
	}
	for name, watch := range m.running {
		if !unchanged(name, watch.index) {
			watch.cancel()
			delete(m.running, name)
			delete(m.watchingSince, name)
			stale[name] = watch.index
		}
	}
	m.mu.Unlock()
	for name, index := range stale {
		seg, claimed := m.dropClaim(name)
		m.guard.Reset(name)
		m.noteCarrierUp(name)
		if current, present := byIndex[index]; claimed && present {
			if err := m.delAddress(current.Name, seg.CIDR()); err != nil {
				m.logger.Warn("removing renamed camera link address failed",
					zap.String("link", current.Name), zap.Error(err))
			}
		}
	}
	return len(stale) != 0
}

// dropClaim forgets a link's claim bookkeeping and stops its server, leaving the
// guard decision alone. It returns the segment that was claimed, if any.
func (m *LinkManager) dropClaim(link string) (CameraSegment, bool) {
	m.mu.Lock()
	seg, wasClaimed := m.claimed[link]
	cancel := m.stop[link]
	delete(m.claimed, link)
	delete(m.claimIndices, link)
	delete(m.addressErrors, link)
	delete(m.pools, link)
	delete(m.stop, link)
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return seg, wasClaimed
}

// withdrawDisqualifiedClaim stops serving a link where another DHCP server has
// appeared. The guard state is deliberately preserved so later client traffic
// cannot make the link claimable again without a carrier reset.
func (m *LinkManager) withdrawDisqualifiedClaim(link string) {
	seg, wasClaimed := m.dropClaim(link)
	if !wasClaimed {
		return
	}
	if err := m.delAddress(link, seg.CIDR()); err != nil {
		m.logger.Warn("removing disqualified camera link address failed",
			zap.String("link", link), zap.String("cidr", seg.CIDR()), zap.Error(err))
	}
	m.logger.Warn("stopped serving disqualified camera link", zap.String("link", link))
}

// withdrawRevokedClaims reconciles the claims we own rather than the current
// interface list. Revocation must still stop DHCP if enumeration fails or a
// link has disappeared or been renamed (a bound socket survives a rename).
func (m *LinkManager) withdrawRevokedClaims() {
	m.mu.Lock()
	var revoked []string
	for link := range m.claimed {
		if !m.allowedDHCP[link] {
			revoked = append(revoked, link)
		}
	}
	m.mu.Unlock()

	for _, link := range revoked {
		m.logger.Warn("camera DHCP interface is no longer allowed; stopping DHCP",
			zap.String("link", link))
		m.withdrawRevokedClaim(link)
	}
}

// withdrawRevokedClaim stops a server when its administrator removes the
// explicit opt-in. ResetServing lets a later opt-in go through the normal
// unanswered-request guard again, while preserving any competing-server
// disqualification that raced with removal.
func (m *LinkManager) withdrawRevokedClaim(link string) {
	m.mu.Lock()
	index := m.claimIndices[link]
	m.mu.Unlock()
	seg, wasClaimed := m.dropClaim(link)
	m.guard.ResetServing(link)
	if !wasClaimed {
		return
	}
	addressLink := link
	if index != 0 {
		ifaces, err := m.listInterfaces()
		if err != nil {
			m.logger.Warn("cannot locate revoked camera interface for address cleanup", zap.String("link", link), zap.Error(err))
			return
		}
		addressLink = ""
		for _, iface := range ifaces {
			if iface.Index == index {
				addressLink = iface.Name
				break
			}
		}
		if addressLink == "" {
			return // The original interface is gone; do not modify a replacement.
		}
	}
	if err := m.delAddress(addressLink, seg.CIDR()); err != nil {
		m.logger.Warn("removing revoked camera link address failed",
			zap.String("link", link), zap.String("cidr", seg.CIDR()), zap.Error(err))
	}
	m.logger.Info("stopped serving camera link after opt-in removal", zap.String("link", link))
}

// recoverFailedServer removes a failed claim and resets the guard so the next
// camera request can begin a fresh unanswered window. Recheck cancellation after
// acquiring scanMu: the pool can be reused by a newer claim while we wait.
func (m *LinkManager) recoverFailedServer(ctx context.Context, link string, seg CameraSegment, pool *LeasePool, failure error) {
	m.scanMu.Lock()
	defer m.scanMu.Unlock()
	if ctx.Err() != nil {
		return
	}
	m.mu.Lock()
	if m.pools[link] != pool {
		m.mu.Unlock()
		return
	}
	cancel := m.stop[link]
	delete(m.claimed, link)
	delete(m.claimIndices, link)
	delete(m.addressErrors, link)
	delete(m.pools, link)
	delete(m.stop, link)
	m.serverErrors[link] = "Camera DHCP server failed: " + failure.Error()
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if err := m.delAddress(link, seg.CIDR()); err != nil {
		m.logger.Warn("removing failed camera link address failed",
			zap.String("link", link), zap.String("cidr", seg.CIDR()), zap.Error(err))
		m.guard.Disqualify(link)
		return
	}
	// Do not erase a foreign DHCP reply that raced with the server failure.
	m.guard.ResetServing(link)
}

// release forgets a link entirely, guard decision included, because a new cable is
// a new decision. Use dropClaim when the decision must survive.
//
// The address we configured is removed as part of this. Leaving it behind would
// keep the link permanently ineligible, so a single carrier blip would strand the
// camera segment for good: the link would never be watched again and the camera
// could never renew.
func (m *LinkManager) release(link string) {
	seg, wasClaimed := m.dropClaim(link)
	if wasClaimed {
		if err := m.delAddress(link, seg.CIDR()); err != nil {
			m.logger.Warn("removing camera link address failed",
				zap.String("link", link), zap.String("cidr", seg.CIDR()), zap.Error(err))
		}
		m.logger.Info("camera link released", zap.String("link", link))
	}
	m.guard.Reset(link)
}

// carrierDownScans is how many consecutive scans a link must show no carrier
// before it is released. A camera rebooting drops carrier for a few seconds, and
// tearing the segment down for that would churn the address and the lease.
const carrierDownScans = 2

// carrierLost reports whether a link has been without carrier long enough to act
// on, and resets the counter once carrier returns.
func (m *LinkManager) carrierLost(iface Interface) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if iface.Carrier {
		delete(m.downFor, iface.Name)
		return false
	}
	m.downFor[iface.Name]++
	return m.downFor[iface.Name] >= carrierDownScans
}

// noteCarrierUp clears the carrier-down counter for a link that is up.
func (m *LinkManager) noteCarrierUp(link string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.downFor, link)
}

// Leases returns the current leases per link, for diagnostics.
func (m *LinkManager) Leases() map[string]map[string]net.IP {
	m.mu.Lock()
	pools := make(map[string]*LeasePool, len(m.pools))
	for link, pool := range m.pools {
		pools[link] = pool
	}
	m.mu.Unlock()
	out := make(map[string]map[string]net.IP, len(pools))
	for link, pool := range pools {
		out[link] = pool.Leases()
	}
	return out
}
