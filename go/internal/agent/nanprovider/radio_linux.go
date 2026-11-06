//go:build linux

package nanprovider

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/agent/nancontrol"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshdiscovery"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	"go.uber.org/zap"
	"golang.org/x/sys/unix"
)

const ndiName = "wnanndi0"

type radioControl struct {
	conn *net.UnixConn
	dir  string
	mu   sync.Mutex
}

func openRadio() (*radioControl, error) {
	dir, err := os.MkdirTemp("/run", "wendy-nan-ctrl-")
	if err != nil {
		return nil, err
	}
	conn, err := net.DialUnix("unixgram", &net.UnixAddr{Name: filepath.Join(dir, "socket"), Net: "unixgram"}, &net.UnixAddr{Name: "/run/wpa_supplicant/nan0", Net: "unixgram"})
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	return &radioControl{conn: conn, dir: dir}, nil
}
func (c *radioControl) close() { _ = c.conn.Close(); _ = os.RemoveAll(c.dir) }
func (c *radioControl) command(cmd string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	_ = c.conn.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := c.conn.Write([]byte(cmd)); err != nil {
		return "", err
	}
	buf := make([]byte, 8192)
	n, _, flags, _, err := c.conn.ReadMsgUnix(buf, nil)
	if err != nil {
		return "", err
	}
	if flags&unix.MSG_TRUNC != 0 {
		return "", fmt.Errorf("supplicant reply exceeds %d bytes", len(buf))
	}
	reply := strings.TrimSpace(string(buf[:n]))
	if strings.HasPrefix(reply, "FAIL") || reply == "UNKNOWN COMMAND" {
		return reply, fmt.Errorf("supplicant %s: %s", strings.Fields(cmd)[0], reply)
	}
	return reply, nil
}
func runHelper(ctx context.Context, args ...string) error {
	_, err := helperOutput(ctx, args...)
	return err
}

const nanHelperTimeout = 20 * time.Second

func helperOutput(ctx context.Context, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, nanHelperTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/usr/sbin/wendyos-nan", args...)
	cmd.Env = append(os.Environ(), "WENDYOS_NAN_NDI_IFACE="+ndiName)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("wendyos-nan %v: %w: %s", args, err, out)
	}
	return string(out), nil
}

type radioPeer struct {
	Outgoing bool
	Lease    *localmesh.ConnectionLease
	Device   meshid.Device
	NMI      string
	NDI      string
	ID       string
}

func validNDPID(raw string) bool {
	id, err := strconv.Atoi(raw)
	return err == nil && id >= 1 && id <= 255
}

func validHandle(raw string) bool {
	id, err := strconv.ParseUint(raw, 10, 32)
	return err == nil && id > 0
}

const (
	radioAttemptCooldown = 15 * time.Second
	maxRadioAttempts     = 128
)

func expireRadioAttempts(attempts map[string]time.Time, now time.Time) {
	for nmi, last := range attempts {
		if now.Sub(last) >= radioAttemptCooldown {
			delete(attempts, nmi)
		}
	}
}

func rememberRadioAttempt(attempts map[string]time.Time, nmi string, now time.Time) {
	expireRadioAttempts(attempts, now)
	// At capacity, shouldInitiate already prevents new requests. Do not retain
	// unlimited cooldown entries even if incoming peers disconnect rapidly.
	if _, exists := attempts[nmi]; exists || len(attempts) < maxRadioAttempts {
		attempts[nmi] = now
	}
}

// shouldInitiate checks discovery identity, underlay and retry safety. UUID
// ordering never decides eligibility; ConnectionPolicy reserves setup capacity.
func shouldInitiate(self, peer meshid.Device, nmi string, active map[string]*radioLink, attempts map[string]time.Time, now time.Time) bool {
	expireRadioAttempts(attempts, now)
	if self == peer || sameUnderlay(self, peer) ||
		active[nmi] != nil || !shouldConnect(nmi, peer, active) || len(attempts) >= maxRadioAttempts {
		return false
	}
	if last, ok := attempts[nmi]; ok && now.Sub(last) < radioAttemptCooldown {
		return false
	}
	return true
}

func shouldAccept(self, peer meshid.Device, nmi string, active map[string]*radioLink) bool {
	return peer != self && peer.Tenant() == self.Tenant() && !sameUnderlay(self, peer) && shouldConnect(nmi, peer, active)
}

// sameUnderlay reports two distinct devices whose last UUID bytes collide,
// which would give them the same NAN underlay address.
func sameUnderlay(a, b meshid.Device) bool {
	return a.UUID()[14] == b.UUID()[14] && a.UUID()[15] == b.UUID()[15]
}

func shouldConnect(nmi string, peer meshid.Device, active map[string]*radioLink) bool {
	if !validMAC(nmi) {
		return false
	}
	if _, err := underlayAddr(peer); err != nil {
		return false
	}
	existingPeer := false
	for _, existing := range active {
		if existing.peer.NMI == nmi {
			existingPeer = true
			if existing.peer.Device != peer {
				return false
			}
			continue
		}
		// One NDI has one neighbour entry per address. A second NMI claiming
		// that address/device must not replace an incumbent before TLS.
		if sameUnderlay(existing.peer.Device, peer) {
			return false
		}
	}
	return existingPeer || len(active) < localmesh.NANMaxConnections
}

// NMI and NDI targets are individual six-byte addresses. They are passed
// verbatim to supplicant's colon-delimited hwaddr_aton control API; zero is
// its missing-parameter sentinel, not a usable target. Hex case is immaterial.
func validMAC(raw string) bool {
	mac, err := net.ParseMAC(raw)
	if err != nil || len(mac) != 6 || !strings.EqualFold(raw, mac.String()) || mac[0]&1 != 0 {
		return false
	}
	for _, octet := range mac {
		if octet != 0 {
			return true
		}
	}
	return false
}

func connectedPeer(v map[string]string, local string, active map[string]*radioLink) (radioPeer, bool) {
	nmi := v["peer"]
	// CONNECTED must complete a request/response we admitted on our own NDI.
	// Discovery SSI alone cannot create a worker or overwrite its identity.
	_, link := radioEventLink(nmi, v["ndp_id"], active)
	if link == nil {
		return radioPeer{}, false
	}
	if (link.peer.ID != "" && link.peer.ID != v["ndp_id"]) ||
		(link.peer.NDI != "" && link.peer.NDI != v["peer_ndi"]) {
		return radioPeer{}, false
	}
	peer := link.peer.Device
	if v["local_ndi"] != local || !shouldConnect(nmi, peer, active) || !validNDPID(v["ndp_id"]) || !validMAC(v["peer_ndi"]) {
		return radioPeer{}, false
	}
	return radioPeer{Device: peer, NMI: nmi, NDI: v["peer_ndi"], ID: v["ndp_id"], Outgoing: link.peer.Outgoing, Lease: link.peer.Lease}, true
}

func disconnectedPeer(v map[string]string, local string, active map[string]*radioLink) (radioPeer, bool) {
	if v["local_ndi"] != "" && v["local_ndi"] != local {
		return radioPeer{}, false
	}
	_, link := radioEventLink(v["peer"], v["ndp_id"], active)
	if link == nil || !link.matchesDisconnect(v) {
		return radioPeer{}, false
	}
	p := link.peer
	p.NDI = ""
	return p, true
}

// Match the exact NDP incarnation. An ambiguous firmware event must never
// tear down a different path to the same peer.
func radioEventLink(nmi, id string, active map[string]*radioLink) (string, *radioLink) {
	var key string
	var found *radioLink
	for k, l := range active {
		if l.peer.NMI != nmi || l.peer.ID != id {
			continue
		}
		if found != nil {
			return "", nil
		}
		key, found = k, l
	}
	if found != nil {
		return key, found
	}
	for k, l := range active {
		if l.peer.NMI == nmi && l.peer.ID == "" {
			if found != nil {
				return "", nil
			}
			key, found = k, l
		}
	}
	return key, found
}

func radioPeerKey(p radioPeer, active map[string]*radioLink) string {
	for k, l := range active {
		if l.peer.NMI == p.NMI && l.peer.ID == p.ID && l.peer.Outgoing == p.Outgoing && l.peer.Lease == p.Lease {
			return k
		}
	}
	return ""
}

// runRadio runs all NDP responses on a persistent local control socket so it
// meets the firmware deadline. Discovery claims remain untrusted until QUIC TLS.
func runRadio(ctx context.Context, ident meshid.Device, connected chan<- radioPeer, health <-chan radioHealth, selection *localmesh.PeerSelection, sessionRecovery *nanRecovery, logger *zap.Logger, status func(localmesh.CarrierStatus)) error {
	control, err := openRadio()
	if err != nil {
		return err
	}
	defer control.close()
	events, err := openRadio()
	if err != nil {
		return err
	}
	defer events.close()
	if _, err = events.command("ATTACH"); err != nil {
		return err
	}
	_ = events.conn.SetDeadline(time.Time{})
	rawEvents := make(chan string, 128)
	readerDone := make(chan struct{})
	defer close(readerDone)
	go func() {
		defer close(rawEvents)
		buf := make([]byte, 16384)
		for {
			n, err := events.conn.Read(buf)
			if err != nil {
				return
			}
			select {
			case rawEvents <- string(buf[:n]):
			case <-readerDone:
				return
			}
		}
	}()
	iface, err := net.InterfaceByName(ndiName)
	if err != nil {
		return err
	}
	local := iface.HardwareAddr.String()
	defer drainRadio(control, rawEvents, local, "", logger)
	mesh := meshdiscovery.DefaultHint(ident.Tenant())
	ssi, err := encodePresence(presence{Device: ident, Mesh: &mesh})
	if err != nil {
		return err
	}
	// Permission changes update the socket receipt atomically with this snapshot.
	unlockManagement, err := nancontrol.Lock(ctx)
	if err != nil {
		return err
	}
	discovery, err := func() (*discoveryLease, error) {
		defer unlockManagement()
		socket, err := nanControlSocketIdentity()
		if err != nil {
			return nil, err
		}
		return startDiscovery(control, discoveryOwnerPath, ident, socket, ssi)
	}()
	if err != nil {
		return err
	}
	defer func() {
		if err := discovery.release(); err != nil {
			logger.Warn("NAN discovery cleanup failed", zap.Error(err))
		}
	}()
	pub, sub := discovery.publish, discovery.sub
	if status != nil {
		status(localmesh.CarrierStatus{Ready: true})
	}
	attempts := map[string]time.Time{}
	active := map[string]*radioLink{}
	policy := localmesh.NewConnectionPolicy(ident, true, localmesh.ConnectionLimits{Outgoing: localmesh.NANMaxOutgoing, Attempts: localmesh.NANMaxOutgoingAttempts, Total: localmesh.NANMaxConnections})
	defer func() {
		for _, l := range active {
			l.peer.Lease.Release()
		}
	}()
	queue := &localmesh.CandidateQueue{}
	type discoveredPeer struct {
		device meshid.Device
		handle string
		seen   time.Time
	}
	candidates := make(map[string]discoveredPeer)
	tryCandidates := func() {
		var keys []string
		for nmi, c := range candidates {
			if time.Since(c.seen) > 60*time.Second {
				delete(candidates, nmi)
				continue
			}
			keys = append(keys, nmi)
		}
		for _, nmi := range queue.Order(keys, time.Now()) {
			c := candidates[nmi]
			if !shouldInitiate(ident, c.device, nmi, active, attempts, time.Now()) || (selection != nil && !selection.AllowRadio(c.device, localmesh.RadioNAN)) {
				continue
			}
			lease := policy.Reserve(nmi, c.device, true)
			if lease == nil {
				continue
			}
			rememberRadioAttempt(attempts, nmi, time.Now())
			if _, err := control.command(fmt.Sprintf("NAN_NDP_REQUEST handle=%s ndi=%s peer_nmi=%s peer_id=%s ssi=%s", sub, ndiName, nmi, c.handle, ssi)); err == nil {
				active[nmi] = &radioLink{peer: radioPeer{Device: c.device, NMI: nmi, Outgoing: true, Lease: lease}, since: time.Now()}
			} else {
				lease.Release()
				queue.Failed(nmi, time.Now())
			}
		}
	}
	lastProbe := time.Now()
	recovery := radioRecovery{}
	coldRecovery := coldNDPRecovery{}
	setupRecovery := ndpSetupRecovery{}
	warnedWarmRecovery := false
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	emit := func(p radioPeer) {
		select {
		case connected <- p:
		case <-ctx.Done():
		}
	}
	for {
		var raw string
		select {
		case <-ctx.Done():
			return ctx.Err()
		case h := <-health:
			if l := active[radioPeerKey(h.Peer, active)]; l != nil {
				if l.draining {
					continue
				}
				l.health(h.Up, time.Now())
				if h.Up {
					queue.Succeeded(h.Peer.NMI)
				} else {
					l.since = time.Now().Add(-35 * time.Second)
					queue.Failed(h.Peer.NMI, time.Now())
				}
				if h.Up && selection != nil {
					selection.Connected(h.Peer.Device, localmesh.RadioNAN)
				}
				if h.Up && meshid.Compare(h.Peer.Device, ident) < 0 {
					recovery.hasLowerPeer = true
				}
			}
			continue
		case <-ticker.C:
			expireRadioAttempts(attempts, time.Now())
			for _, link := range active {
				nmi := link.peer.NMI
				if !link.draining && (selection == nil || selection.AllowRadio(link.peer.Device, localmesh.RadioNAN)) {
					continue
				}
				if !link.draining {
					link.draining = true
					link.since = time.Now()
					// Stop the exact authenticated worker before removing the
					// underlay; do not lose NDP ownership until disconnect.
					removed := link.peer
					removed.NDI = ""
					emit(removed)
				}
				if time.Since(link.since) >= 35*time.Second {
					return fmt.Errorf("NAN NDP for %s did not disconnect while draining", nmi)
				}
				if time.Since(link.stopAttempt) < 5*time.Second {
					continue
				}
				link.stopAttempt = time.Now()
				info, err := control.command("NAN_PEER_INFO " + nmi + " ndps")
				if err != nil {
					logger.Warn("NAN selected-peer lookup failed", zap.String("peer", nmi), zap.Error(err))
					continue
				}
				for _, ndp := range ownedNDPs(nmi, local, info) {
					if link.peer.ID != "" && ndp.id != link.peer.ID {
						continue
					}
					if (ndp.init == local) != link.peer.Outgoing {
						continue
					}
					if _, err := control.command(fmt.Sprintf("NAN_NDP_TERMINATE peer_nmi=%s init_ndi=%s ndp_id=%s", ndp.peer, ndp.init, ndp.id)); err != nil {
						logger.Warn("NAN selected-peer termination failed", zap.String("peer", nmi), zap.Error(err))
					}
				}
			}
			anyHealthy := false
			for _, link := range active {
				anyHealthy = anyHealthy || link.healthy
			}
			now := time.Now()
			recovery.observe(now, anyHealthy)
			if sessionRecovery.rearmAfterStableAuthentication(now, active) {
				logger.Info("NAN private-interface recovery rearmed after stable authenticated connectivity")
			}
			if setupRecovery.ready(now, anyHealthy, recovery.authenticated) {
				allowed := sessionRecovery.canReset(false)
				logger.Warn("NAN initial data-path setup repeatedly failed", zap.Int("failed_owned_attempts", setupRecovery.failures), zap.Bool("private_reset_allowed", allowed))
				if allowed {
					return fmt.Errorf("NAN initial NDP setup failed: %w", errSoftNANRecovery)
				}
			}
			if recovery.expired(now) {
				if sessionRecovery.canReset(false) {
					return fmt.Errorf("NAN data recovery: repeated replacement NDPs without authenticated traffic: %w", errSoftNANRecovery)
				}
				if !warnedWarmRecovery {
					logger.Warn("NAN remains degraded after bounded soft recovery; preserving supplicant and Wi-Fi")
					warnedWarmRecovery = true
				}
			}
			if traffic, err := ndiTrafficSnapshot(); err == nil {
				coldRecovery.observe(now, traffic)
			}
			if coldRecovery.ready(now, anyHealthy) {
				logger.Warn("NAN connected repeatedly without an authenticated data path", zap.Int("paths_since_last_receive", coldRecovery.paths), zap.Bool("received_traffic", coldRecovery.hadRX), zap.Bool("soft_reset_allowed", sessionRecovery.canReset(!coldRecovery.hadRX)))
				if sessionRecovery.canReset(!coldRecovery.hadRX) {
					if !coldRecovery.hadRX {
						return fmt.Errorf("%w: %w", errSoftNANRecovery, errNoRXNANRecovery)
					}
					return errSoftNANRecovery
				}
			}
			if time.Since(lastProbe) >= 5*time.Second {
				if pong, err := control.command("PING"); err != nil || pong != "PONG" {
					return fmt.Errorf("NAN control connection lost: reply=%q err=%v", pong, err)
				}
				lastProbe = time.Now()
			}
			for _, link := range active {
				if link.draining || !link.expired(time.Now()) {
					continue
				}
				link.draining = true
				link.since = time.Now()
				removed := link.peer
				removed.NDI = ""
				emit(removed)
				queue.Failed(link.peer.NMI, time.Now())
				rememberRadioAttempt(attempts, link.peer.NMI, time.Now())
				if selection != nil {
					selection.Failed(link.peer.Device, localmesh.RadioNAN)
				}
			}
			tryCandidates()
			continue
		case event, ok := <-rawEvents:
			if !ok {
				return fmt.Errorf("NAN event monitor closed")
			}
			raw = event
		}
		kind, v := parseEvent(raw)
		if strings.HasPrefix(kind, "NAN-NDP-") {
			logger.Info("NAN lifecycle event", zap.String("event", strings.TrimSpace(raw)))
		}
		switch kind {
		case "NAN-DISCOVERY-RESULT":
			if v["subscribe_id"] != sub {
				continue
			}
			peer, err := decodePresence(ident.Tenant(), mesh, v["ssi"])
			if err != nil || peer == ident {
				continue
			}
			if sameUnderlay(ident, peer) {
				logger.Warn("NAN underlay collision with peer", zap.String("peer", v["address"]), zap.String("device", peer.UUID().String()))
				continue
			}
			nmi := v["address"]
			if !validMAC(nmi) {
				continue
			}
			if selection != nil {
				selection.Seen(peer, localmesh.RadioNAN)
			}
			if !validHandle(v["publish_id"]) {
				continue
			}
			if _, exists := candidates[nmi]; exists || len(candidates) < maxRadioAttempts {
				candidates[nmi] = discoveredPeer{peer, v["publish_id"], time.Now()}
			}
		case "NAN-NDP-REQUEST":
			nmi := v["peer_nmi"]
			if v["publish_inst_id"] != pub {
				continue
			}
			peer, err := decodePresence(ident.Tenant(), mesh, v["ssi"])
			if err != nil || !validMAC(nmi) || !shouldAccept(ident, peer, nmi, active) {
				continue
			}
			if sameUnderlay(ident, peer) {
				logger.Warn("NAN underlay collision with peer", zap.String("peer", nmi), zap.String("device", peer.UUID().String()))
				continue
			}
			if !validMAC(v["init_ndi"]) {
				continue
			}
			if !validNDPID(v["ndp_id"]) {
				continue
			}
			if selection != nil {
				selection.Seen(peer, localmesh.RadioNAN)
				if !selection.AllowRadio(peer, localmesh.RadioNAN) {
					_, _ = control.command(fmt.Sprintf("NAN_NDP_RESPONSE reject peer_nmi=%s ndp_id=%s init_ndi=%s", nmi, v["ndp_id"], v["init_ndi"]))
					continue
				}
			}
			key := nmi + "/incoming/" + v["ndp_id"]
			if active[key] != nil {
				continue
			}
			lease := policy.Reserve(key, peer, false)
			if lease == nil {
				_, _ = control.command(fmt.Sprintf("NAN_NDP_RESPONSE reject peer_nmi=%s ndp_id=%s init_ndi=%s", nmi, v["ndp_id"], v["init_ndi"]))
				continue
			}
			if _, err = control.command(fmt.Sprintf("NAN_NDP_RESPONSE accept handle=%s ndi=%s peer_nmi=%s ndp_id=%s init_ndi=%s ssi=%s", pub, ndiName, nmi, v["ndp_id"], v["init_ndi"], ssi)); err == nil {
				active[key] = &radioLink{peer: radioPeer{Device: peer, NMI: nmi, ID: v["ndp_id"], Lease: lease}, since: time.Now()}
			} else {
				lease.Release()
			}

		case "NAN-NDP-COUNTER-REQUEST":
			nmi := v["peer_nmi"]
			peer, err := decodePresence(ident.Tenant(), mesh, v["ssi"])
			link := active[nmi]
			if err != nil || link == nil || !link.peer.Outgoing || link.peer.Device != peer || link.healthy || link.peer.NDI != "" {
				continue
			}
			cmd, err := counterResponse(local, sub, ssi, v)
			if err != nil {
				continue
			}
			if _, err = control.command(cmd); err != nil {
				logger.Warn("NAN counter proposal confirmation failed", zap.Error(err))
			} else {
				link.peer.Lease.Progress()
				link.peer.ID = v["ndp_id"]
				logger.Info("NAN counter proposal confirmed", zap.String("peer", nmi), zap.String("ndp_id", v["ndp_id"]))
			}
		case "NAN-NDP-CONNECTED":
			p, ok := connectedPeer(v, local, active)
			if !ok {
				continue
			}
			key, link := radioEventLink(p.NMI, p.ID, active)
			if link == nil {
				continue
			}
			if selection != nil && !selection.AllowRadio(p.Device, localmesh.RadioNAN) {
				if old := active[key]; old != nil && old.peer.NDI != "" {
					removed := old.peer
					removed.NDI = ""
					emit(removed)
				}
				active[key] = &radioLink{peer: p, since: time.Now(), draining: true}
				continue
			}
			if existing := active[key]; existing != nil && existing.peer == p {
				continue
			}
			p.Lease.Progress()
			active[key] = &radioLink{peer: p, since: time.Now()}
			recovery.connected()
			if traffic, err := ndiTrafficSnapshot(); err == nil {
				coldRecovery.connected(time.Now(), traffic)
			}
			emit(p)
		case "NAN-NDP-DISCONNECTED":
			if p, ok := disconnectedPeer(v, local, active); ok {
				key := radioPeerKey(p, active)
				if key == "" {
					key, _ = radioEventLink(p.NMI, p.ID, active)
				}
				if l := active[key]; l != nil {
					recovery.setupFailed(l, v)
					setupRecovery.setupFailed(l, v, time.Now())
					l.peer.Lease.Release()
				}
				emit(p)
				delete(active, key)
			}
		}
	}
}
