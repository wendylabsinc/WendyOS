//go:build linux

package nanprovider

import (
	"context"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
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
	n, err := c.conn.Read(buf)
	if err != nil {
		return "", err
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

func helperOutput(ctx context.Context, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
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
	Asset    int32
	NMI, NDI string
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

func shouldInitiate(self, asset int32, nmi string, active map[string]*radioLink, attempts map[string]time.Time, now time.Time) bool {
	if asset <= self || active[nmi] != nil || len(active) >= 3 {
		return false
	}
	if last, ok := attempts[nmi]; ok && now.Sub(last) < 15*time.Second {
		return false
	}
	return true
}

func shouldAccept(self, asset int32, nmi string, active map[string]*radioLink) bool {
	return asset > 0 && asset < self && (active[nmi] != nil || len(active) < 3)
}

func shouldConnect(nmi string, asset int32, active map[string]*radioLink) bool {
	if asset <= 0 || !validMAC(nmi) {
		return false
	}
	if existing := active[nmi]; existing != nil {
		return existing.peer.Asset == asset
	}
	return len(active) < 3
}

func validMAC(raw string) bool {
	mac, err := net.ParseMAC(raw)
	return err == nil && len(mac) == 6
}

func connectedPeer(v map[string]string, local string, known map[string]int32, active map[string]*radioLink) (radioPeer, bool) {
	nmi := v["peer"]
	asset := known[nmi]
	if v["local_ndi"] != local || !shouldConnect(nmi, asset, active) || !validNDPID(v["ndp_id"]) || !validMAC(v["peer_ndi"]) {
		return radioPeer{}, false
	}
	return radioPeer{Asset: asset, NMI: nmi, NDI: v["peer_ndi"], ID: v["ndp_id"]}, true
}

func disconnectedPeer(v map[string]string, local string, active map[string]*radioLink) (radioPeer, bool) {
	if v["local_ndi"] != "" && v["local_ndi"] != local {
		return radioPeer{}, false
	}
	link := active[v["peer"]]
	if link == nil || !link.matchesDisconnect(v) {
		return radioPeer{}, false
	}
	return radioPeer{Asset: link.peer.Asset, NMI: v["peer"], ID: link.peer.ID}, true
}

// runRadio runs all NDP responses on a persistent local control socket so it
// meets the firmware deadline. Discovery claims remain untrusted until QUIC TLS.
func runRadio(ctx context.Context, ident Identity, connected chan<- radioPeer, health <-chan radioHealth, logger *zap.Logger) error {
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
	ssi := hex.EncodeToString([]byte(fmt.Sprintf("wendy-nan:1:%d:%d", ident.Org, ident.Asset)))
	pub, err := control.command("NAN_PUBLISH service_name=wendy.mesh.v1 sync=1 data_path=1 ttl=0 ssi=" + ssi)
	if err != nil {
		return err
	}
	if !validHandle(pub) {
		return fmt.Errorf("invalid NAN publish handle %q", pub)
	}
	defer control.command("NAN_CANCEL_PUBLISH publish_id=" + pub)
	sub, err := control.command("NAN_SUBSCRIBE service_name=wendy.mesh.v1 sync=1 ttl=0 ssi=" + ssi)
	if err != nil {
		return err
	}
	if !validHandle(sub) {
		return fmt.Errorf("invalid NAN subscribe handle %q", sub)
	}
	defer control.command("NAN_CANCEL_SUBSCRIBE subscribe_id=" + sub)
	known := map[string]int32{}
	attempts := map[string]time.Time{}
	active := map[string]*radioLink{}
	lastProbe := time.Now()
	recovery := radioRecovery{}
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
			if l := active[h.Peer.NMI]; l != nil && l.peer == h.Peer {
				l.health(h.Up, time.Now())
				if h.Up && h.Peer.Asset < ident.Asset {
					recovery.hasLowerPeer = true
				}
			}
			continue
		case <-ticker.C:
			anyHealthy := false
			for _, link := range active {
				anyHealthy = anyHealthy || link.healthy
			}
			recovery.observe(time.Now(), anyHealthy)
			if recovery.expired(time.Now()) {
				return fmt.Errorf("NAN data recovery: repeated replacement NDPs without authenticated traffic; cycling owned NAN session after staggered deadline")
			}
			if time.Since(lastProbe) >= 5*time.Second {
				if pong, err := control.command("PING"); err != nil || pong != "PONG" {
					return fmt.Errorf("NAN control connection lost: reply=%q err=%v", pong, err)
				}
				lastProbe = time.Now()
			}
			for nmi, link := range active {
				if !link.expired(time.Now()) {
					continue
				}
				logger.Warn("NAN stale link recovery", zap.String("peer", nmi), zap.Int32("asset", link.peer.Asset))
				info, err := control.command("NAN_PEER_INFO " + nmi + " ndps")
				if err == nil {
					for _, ndp := range ownedNDPs(nmi, local, info) {
						_, err := control.command(fmt.Sprintf("NAN_NDP_TERMINATE peer_nmi=%s init_ndi=%s ndp_id=%s", ndp.peer, ndp.init, ndp.id))
						if err != nil {
							logger.Warn("NAN stale termination failed", zap.Error(err))
						}
					}
				}
				emit(radioPeer{Asset: link.peer.Asset, NMI: nmi, ID: link.peer.ID})
				delete(active, nmi)
				attempts[nmi] = time.Now()
			}
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
		case "NAN-DISCOVERY-RESULT", "NAN-REPLIED":
			if kind == "NAN-DISCOVERY-RESULT" && v["subscribe_id"] != sub {
				continue
			}
			asset, err := decodePresence(v["ssi"], ident.Org)
			if err != nil || asset == ident.Asset {
				continue
			}
			nmi := v["address"]
			if !validMAC(nmi) {
				continue
			}
			known[nmi] = asset
			if kind != "NAN-DISCOVERY-RESULT" || !validHandle(v["publish_id"]) || !shouldInitiate(ident.Asset, asset, nmi, active, attempts, time.Now()) {
				continue
			}
			attempts[nmi] = time.Now()
			if _, err = control.command(fmt.Sprintf("NAN_NDP_REQUEST handle=%s ndi=%s peer_nmi=%s peer_id=%s ssi=%s", sub, ndiName, nmi, v["publish_id"], ssi)); err == nil {
				active[nmi] = &radioLink{peer: radioPeer{Asset: asset, NMI: nmi}, since: time.Now()}
			}
		case "NAN-NDP-REQUEST":
			nmi := v["peer_nmi"]
			if v["publish_inst_id"] != pub {
				continue
			}
			asset, err := decodePresence(v["ssi"], ident.Org)
			if err != nil || !shouldAccept(ident.Asset, asset, nmi, active) {
				continue
			}
			if !validMAC(nmi) {
				continue
			}
			if !validMAC(v["init_ndi"]) {
				continue
			}
			if !validNDPID(v["ndp_id"]) {
				continue
			}
			// This provider owns one NDP per peer. A new request while an old
			// NDP exists usually means that the peer restarted. Do not replace
			// its established record with a pending boolean: that would lose
			// the old NDP's health deadline and leave its NDL/schedule behind.
			if old := active[nmi]; old != nil && old.peer.NDI != "" {
				_, rejectErr := control.command(fmt.Sprintf("NAN_NDP_RESPONSE reject peer_nmi=%s ndp_id=%s init_ndi=%s", nmi, v["ndp_id"], v["init_ndi"]))
				logger.Warn("NAN replacement waits for old NDP cleanup", zap.String("peer", nmi), zap.Error(rejectErr))
				old.health(false, time.Now())
				emit(radioPeer{Asset: old.peer.Asset, NMI: nmi, ID: old.peer.ID})
				continue
			}
			known[nmi] = asset
			if _, err = control.command(fmt.Sprintf("NAN_NDP_RESPONSE accept handle=%s ndi=%s peer_nmi=%s ndp_id=%s init_ndi=%s ssi=%s", pub, ndiName, nmi, v["ndp_id"], v["init_ndi"], ssi)); err == nil {
				active[nmi] = &radioLink{peer: radioPeer{Asset: asset, NMI: nmi}, since: time.Now()}
			}
		case "NAN-NDP-COUNTER-REQUEST":
			nmi := v["peer_nmi"]
			asset, err := decodePresence(v["ssi"], ident.Org)
			link := active[nmi]
			if err != nil || asset <= ident.Asset || link == nil || link.peer.Asset != asset || link.healthy || link.peer.NDI != "" {
				continue
			}
			cmd, err := counterResponse(local, sub, ssi, v)
			if err != nil {
				continue
			}
			if _, err = control.command(cmd); err != nil {
				logger.Warn("NAN counter proposal confirmation failed", zap.Error(err))
			} else {
				link.peer.ID = v["ndp_id"]
				logger.Info("NAN counter proposal confirmed", zap.String("peer", nmi), zap.String("ndp_id", v["ndp_id"]))
			}
		case "NAN-NDP-CONNECTED":
			p, ok := connectedPeer(v, local, known, active)
			if !ok {
				continue
			}
			if existing := active[p.NMI]; existing != nil && existing.peer == p {
				continue
			}
			active[p.NMI] = &radioLink{peer: p, since: time.Now()}
			recovery.connected()
			emit(p)
		case "NAN-NDP-DISCONNECTED":
			if p, ok := disconnectedPeer(v, local, active); ok {
				emit(p)
				delete(active, p.NMI)
			}
		}
	}
}
