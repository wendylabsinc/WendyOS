package ipcam

import (
	"context"
	"fmt"
	"net"
	"os"
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

	mu            sync.Mutex
	claimed       map[string]CameraSegment
	pools         map[string]*LeasePool
	running       map[string]bool
	stop          map[string]context.CancelFunc // cancels a claimed link's server
	downFor       map[string]int                // consecutive scans with no carrier
	watchingSince map[string]time.Time          // when this process began watching a link
	allowedDHCP   map[string]bool               // explicit, root-owned camera DHCP opt-ins
	allowlistPath string
	readAllowlist func(string) (map[string]bool, error)
}

// NewLinkManager returns a manager writing discovered cameras into reg.
func NewLinkManager(reg *Registry, logger *zap.Logger) *LinkManager {
	m := &LinkManager{
		reg:           reg,
		guard:         NewLinkGuard(),
		logger:        logger,
		now:           time.Now,
		claimed:       make(map[string]CameraSegment),
		pools:         make(map[string]*LeasePool),
		running:       make(map[string]bool),
		stop:          make(map[string]context.CancelFunc),
		downFor:       make(map[string]int),
		watchingSince: make(map[string]time.Time),
		allowedDHCP:   make(map[string]bool),
		readAllowlist: readDHCPAllowlist,
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
	if err := m.refreshDHCPAllowlist(); err != nil {
		m.logger.Warn("camera DHCP disabled: invalid allowlist", zap.Error(err))
	}
	ifaces, err := m.listInterfaces()
	if err != nil {
		m.logger.Debug("listing interfaces failed", zap.Error(err))
		return
	}

	for _, iface := range ifaces {
		// A claimed link carries our own address by design, so it necessarily
		// fails the eligibility test that got it claimed. Only a sustained loss of
		// carrier releases it; re-testing eligibility here would release the link
		// on the very next scan and then re-claim it on the next DISCOVER,
		// spawning a second server each time round.
		if seg, claimed := m.claimedSegment(iface.Name); claimed {
			if !m.dhcpAllowed(iface.Name) {
				m.logger.Warn("camera DHCP interface is no longer allowed; stopping DHCP",
					zap.String("link", iface.Name))
				m.withdrawRevokedClaim(iface.Name)
				continue
			}
			if m.carrierLost(iface) {
				m.release(iface.Name)
				continue
			}
			if iface.HasOtherIPv4(seg.ServerIP) {
				m.logger.Warn("camera link became an uplink; stopping DHCP",
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
		if !iface.Eligible() {
			if m.carrierLost(iface) {
				// A link that has really lost carrier is no longer a camera
				// segment. Forget it so the next cable is judged afresh.
				m.release(iface.Name)
			}
			continue
		}
		m.noteCarrierUp(iface.Name)
		m.ensureWatching(ctx, iface.Name)
		// DHCP traffic alone cannot distinguish a camera from an uplink client.
		// Keep passive observation, but never claim an interface without an
		// explicit root-owned opt-in.
		if !m.dhcpAllowed(iface.Name) {
			continue
		}
		if seg, ok := m.restorableSegment(iface.Name); ok {
			if m.guard.RestoreClaim(iface.Name) {
				m.logger.Info("restoring camera link claim from a previous run",
					zap.String("link", iface.Name), zap.String("address", seg.CIDR()))
				m.claim(ctx, iface.Name, &seg)
			}
			continue
		}
		if m.guard.ShouldClaim(iface.Name, m.now()) {
			m.claim(ctx, iface.Name, nil)
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
		return
	}
	// Warn, not Info: the address disappearing under us means something else is
	// managing this interface, and that is worth seeing in production logs.
	m.logger.Warn("camera link lost its address, restoring it",
		zap.String("link", iface.Name), zap.String("cidr", seg.CIDR()))
	if err := m.addAddress(iface.Name, seg.CIDR()); err != nil {
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
	m.mu.Lock()
	since, watching := m.watchingSince[link]
	m.mu.Unlock()
	if !watching || m.now().Sub(since) < unansweredWindow {
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

// ensureWatching starts one passive DHCP watcher per link.
func (m *LinkManager) ensureWatching(ctx context.Context, link string) {
	m.mu.Lock()
	if m.running[link] {
		m.mu.Unlock()
		return
	}
	m.running[link] = true
	// The clock a restored claim waits on: it is how long we have been listening
	// for a competing DHCP server on this link in this process.
	m.watchingSince[link] = m.now()
	m.mu.Unlock()

	// Info, not Debug: when a camera does not appear, the first question is
	// whether the agent is watching its link at all, and production logging drops
	// Debug.
	m.logger.Info("watching camera link for DHCP", zap.String("link", link))

	go func() {
		defer func() {
			m.mu.Lock()
			delete(m.running, link)
			delete(m.watchingSince, link)
			m.mu.Unlock()
		}()
		err := m.watch(ctx, link, func(p *Packet) {
			m.logger.Info("dhcp seen on camera link",
				zap.String("link", link),
				zap.String("type", p.Type.String()),
				zap.String("mac", p.CHAddr.String()),
				zap.String("hostname", p.Hostname))
			m.guard.Observe(link, p, m.now())
			// The guard can discover a competing server after we have already
			// claimed the link. Stop answering immediately and remove our address,
			// while leaving the disqualification sticky until carrier is lost.
			if m.guard.State(link) == LinkDisqualified {
				m.withdrawDisqualifiedClaim(link)
			}
			// A DISCOVER names the camera before it has any address, and its
			// hostname is often the model. That is worth recording even on a link
			// we will never serve.
			m.noteClient(link, p)
		})
		if err != nil && ctx.Err() == nil {
			// A watch that cannot start is why a camera never appears, so this is
			// not a Debug-level detail.
			m.logger.Warn("dhcp watch on camera link ended",
				zap.String("link", link), zap.Error(err))
		}
	}()
}

// noteClient records a camera seen asking for an address, so `camera list` can
// show it as offline-but-known rather than nothing at all.
func (m *LinkManager) noteClient(link string, p *Packet) {
	if p.Type != Discover && p.Type != Request {
		return
	}
	if len(p.CHAddr) == 0 || m.reg == nil {
		return
	}
	if _, err := m.reg.Upsert(Camera{
		MAC:   p.CHAddr.String(),
		Model: p.Hostname,
		Link:  link,
	}); err != nil {
		m.logger.Debug("recording dhcp client failed",
			zap.String("mac", p.CHAddr.String()), zap.Error(err))
	}
}

// claim configures the link and starts serving leases on it. A non-nil preferred
// segment is used when it is free, which is how a restored claim keeps the
// subnet the camera already holds a lease on.
func (m *LinkManager) claim(ctx context.Context, link string, preferred *CameraSegment) {
	m.mu.Lock()
	if _, exists := m.claimed[link]; exists {
		m.mu.Unlock()
		return
	}
	seg, ok := m.chooseSegmentLocked(preferred)
	if !ok {
		m.mu.Unlock()
		m.logger.Warn("no camera link subnet available", zap.String("link", link))
		m.guard.Disqualify(link)
		return
	}
	pool := NewLeasePool(seg.PoolBase, seg.PoolSize)
	m.claimed[link] = seg
	m.pools[link] = pool
	m.mu.Unlock()

	if err := m.addAddress(link, seg.CIDR()); err != nil {
		// Without an address we cannot answer, and retrying forever would be
		// noise. Disqualify so the decision is visible rather than silent.
		//
		// dropClaim rather than release: release resets the guard, which would
		// erase the disqualification we are about to record.
		m.logger.Warn("configuring camera link failed",
			zap.String("link", link), zap.String("cidr", seg.CIDR()), zap.Error(err))
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
	m.guard.RegisterServerID(seg.ServerIP)
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
			m.recoverFailedServer(link, seg, pool)
		}
	}()
}

// chooseSegmentLocked returns the segment a claim should use: the preferred one
// when it is free, otherwise the lowest unused. Callers hold m.mu.
func (m *LinkManager) chooseSegmentLocked(preferred *CameraSegment) (CameraSegment, bool) {
	if preferred != nil && !m.segmentUsedLocked(*preferred) {
		return *preferred, true
	}
	return m.nextSegmentLocked()
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
func (m *LinkManager) nextSegmentLocked() (CameraSegment, bool) {
	for index := range 256 {
		if candidate := SegmentFor(index); !m.segmentUsedLocked(candidate) {
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
		MAC:     mac.String(),
		Address: addr.String(),
		Model:   hostname,
		Link:    link,
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

// dropClaim forgets a link's claim bookkeeping and stops its server, leaving the
// guard decision alone. It returns the segment that was claimed, if any.
func (m *LinkManager) dropClaim(link string) (CameraSegment, bool) {
	m.mu.Lock()
	seg, wasClaimed := m.claimed[link]
	cancel := m.stop[link]
	delete(m.claimed, link)
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

// withdrawRevokedClaim stops a server when its administrator removes the
// explicit opt-in. ResetServing lets a later opt-in go through the normal
// unanswered-request guard again, while preserving any competing-server
// disqualification that raced with removal.
func (m *LinkManager) withdrawRevokedClaim(link string) {
	seg, wasClaimed := m.dropClaim(link)
	m.guard.ResetServing(link)
	if !wasClaimed {
		return
	}
	if err := m.delAddress(link, seg.CIDR()); err != nil {
		m.logger.Warn("removing revoked camera link address failed",
			zap.String("link", link), zap.String("cidr", seg.CIDR()), zap.Error(err))
	}
	m.logger.Info("stopped serving camera link after opt-in removal", zap.String("link", link))
}

// recoverFailedServer removes a failed claim and resets the guard so the next
// camera request can begin a fresh unanswered window. The pool identity prevents
// a stale server goroutine from tearing down a newer claim for the same link.
func (m *LinkManager) recoverFailedServer(link string, seg CameraSegment, pool *LeasePool) {
	m.mu.Lock()
	if m.pools[link] != pool {
		m.mu.Unlock()
		return
	}
	cancel := m.stop[link]
	delete(m.claimed, link)
	delete(m.pools, link)
	delete(m.stop, link)
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
	defer m.mu.Unlock()
	out := make(map[string]map[string]net.IP, len(m.pools))
	for link, pool := range m.pools {
		out[link] = pool.Leases()
	}
	return out
}
