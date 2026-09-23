//go:build linux

package nanprovider

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"
)

type radioHealth struct {
	Peer radioPeer
	Up   bool
}

type radioLink struct {
	peer    radioPeer
	healthy bool
	since   time.Time
}

func (l *radioLink) health(up bool, now time.Time) {
	if l.healthy != up {
		l.since = now
	}
	l.healthy = up
}

func (l *radioLink) expired(now time.Time) bool {
	return !l.healthy && now.Sub(l.since) >= 35*time.Second
}

func (l *radioLink) matchesDisconnect(v map[string]string) bool {
	// A rejected replacement can reuse the old path's ID after a peer reboot.
	// Setup failure is not evidence that the established path was removed.
	if l.peer.NDI != "" && strings.TrimSuffix(v["failure"], ",") == "1" {
		return false
	}
	return l.peer.ID == "" || l.peer.ID == v["ndp_id"]
}

type ndpTuple struct{ peer, init, id string }

// Repeated successful NDP signaling without any authenticated data traffic can
// outlive per-NDP cleanup. Cycle only after a previously authenticated session
// loses every healthy neighbor, and several replacement NDPs also fail.
type radioRecovery struct {
	authenticated bool
	outage        time.Time
	replacements  int
	hasLowerPeer  bool
}

func (r *radioRecovery) observe(now time.Time, healthy bool) {
	if healthy {
		r.authenticated = true
		r.outage = time.Time{}
		r.replacements = 0
	} else if r.authenticated && r.outage.IsZero() {
		r.outage = now
	}
}

func (r *radioRecovery) connected() {
	if !r.outage.IsZero() {
		r.replacements++
	}
}

func (r *radioRecovery) expired(now time.Time) bool {
	delay := 90 * time.Second
	// Let an authenticated lower-ID peer recover first, retaining our NAN
	// cluster as its rendezvous point. Never learn this priority from SSI.
	if r.hasLowerPeer {
		delay = 270 * time.Second
	}
	return r.authenticated && !r.outage.IsZero() && r.replacements >= 3 &&
		now.Sub(r.outage) >= delay
}

// Supplicant uses NAN_NDP_RESPONSE for a counter-proposal confirmation too;
// specifying our initiator NDI selects ACTION_CONF and the subscription handle.
func counterResponse(local, subscription, ssi string, v map[string]string) (string, error) {
	if v["init_ndi"] != local {
		return "", fmt.Errorf("counter proposal is not for our initiator NDI")
	}
	if _, err := net.ParseMAC(v["peer_nmi"]); err != nil {
		return "", err
	}
	id, err := strconv.Atoi(v["ndp_id"])
	if err != nil || id < 1 || id > 255 {
		return "", fmt.Errorf("invalid counter NDP ID")
	}
	return fmt.Sprintf("NAN_NDP_RESPONSE accept handle=%s ndi=%s peer_nmi=%s ndp_id=%d init_ndi=%s ssi=%s", subscription, ndiName, v["peer_nmi"], id, local, ssi), nil
}

func fields(line string) map[string]string {
	out := map[string]string{}
	for _, f := range strings.Fields(line) {
		if k, v, ok := strings.Cut(f, "="); ok {
			out[k] = v
		}
	}
	return out
}

// Only paths on our owned NDI are eligible, even if another application uses NAN.
func ownedNDPs(peer, local, info string) []ndpTuple {
	var out []ndpTuple
	for _, line := range strings.Split(info, "\n") {
		v := fields(line)
		addr := v["resp_ndi"]
		if v["initiator"] == "1" {
			addr = v["init_ndi"]
		}
		id, err := strconv.Atoi(v["ndp_id"])
		if addr != local || err != nil || id < 1 || id > 255 {
			continue
		}
		if _, err = net.ParseMAC(v["init_ndi"]); err != nil {
			continue
		}
		out = append(out, ndpTuple{peer, v["init_ndi"], v["ndp_id"]})
	}
	return out
}

// drainRadio preserves the interface until explicit termination completes (or
// the bounded deadline expires). OK alone is not completion; a local event can
// also mean failed TX, so remote delivery must be checked in integration tests.
type radioCommander interface{ command(string) (string, error) }

func drainRadio(control radioCommander, events <-chan string, local, onlyPeer string, logger *zap.Logger) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	status, err := control.command("NAN_STATUS")
	if err != nil {
		logger.Warn("NAN drain status failed", zap.Error(err))
		return
	}
	for _, line := range strings.Split(status, "\n") {
		peer := fields(line)["peer"]
		if _, err := net.ParseMAC(peer); err != nil || (onlyPeer != "" && onlyPeer != peer) {
			continue
		}
		info, err := control.command("NAN_PEER_INFO " + peer + " ndps")
		if err != nil {
			continue
		}
		for _, ndp := range ownedNDPs(peer, local, info) {
			if ctx.Err() != nil {
				return
			}
			cmd := fmt.Sprintf("NAN_NDP_TERMINATE peer_nmi=%s init_ndi=%s ndp_id=%s", ndp.peer, ndp.init, ndp.id)
			logger.Info("NAN draining NDP", zap.String("peer", peer), zap.String("ndp_id", ndp.id))
			if _, err = control.command(cmd); err != nil {
				logger.Warn("NAN termination request failed", zap.Error(err))
				continue
			}
			completed := false
			for !completed {
				select {
				case <-ctx.Done():
					logger.Warn("NAN drain completion deadline", zap.String("peer", peer))
					return
				case raw, ok := <-events:
					if !ok {
						return
					}
					kind, v := parseEvent(raw)
					logger.Debug("NAN drain event", zap.String("event", raw))
					completed = kind == "NAN-NDP-DISCONNECTED" && v["peer"] == peer && v["ndp_id"] == ndp.id
				}
			}
			logger.Info("NAN NDP drain completed", zap.String("peer", peer), zap.String("ndp_id", ndp.id))
		}
	}
}
