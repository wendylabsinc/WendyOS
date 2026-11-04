//go:build linux

package nanprovider

import (
	"fmt"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"go.uber.org/zap"
)

// handleRadioHealth accepts only the exact owned NDP incarnation. Capacity
// reclamation and verified setup deferral are intentional completions, while an
// ordinary failed session retains the existing failure/cooldown behavior.
func handleRadioHealth(h radioHealth, active map[string]*radioLink, queue *localmesh.CandidateQueue, attempts map[string]time.Time, now time.Time, emit func(radioPeer)) *radioLink {
	l := active[radioPeerKey(h.Peer, active)]
	if l == nil || l.draining {
		return nil
	}
	l.health(h.Up, now)
	if h.Up {
		queue.Succeeded(h.Peer.NMI)
	} else if l.peer.Lease.Retired() {
		queue.Succeeded(l.peer.NMI)
		delete(attempts, l.peer.NMI)
		l.beginDrain(now, emit)
	} else {
		l.since = now.Add(-35 * time.Second)
		queue.Failed(h.Peer.NMI, now)
	}
	return l
}

func (l *radioLink) beginDrain(now time.Time, emit func(radioPeer)) {
	if l.draining {
		return
	}
	l.draining, l.since = true, now
	removed := l.peer
	removed.NDI = ""
	emit(removed)
}

// drainTick requests termination of only this owned NDP. Command success is
// not disconnect completion; its lease continues consuming capacity until the
// matched lifecycle event is processed.
func (l *radioLink) drainTick(now time.Time, control radioCommander, local string, logger *zap.Logger) error {
	if now.Sub(l.since) >= 35*time.Second {
		return fmt.Errorf("Wi-Fi Aware data connection to %s did not close during shutdown", l.peer.NMI)
	}
	if now.Sub(l.stopAttempt) < 5*time.Second {
		return nil
	}
	l.stopAttempt = now
	info, err := control.command("NAN_PEER_INFO " + l.peer.NMI + " ndps")
	if err != nil {
		logger.Warn("NAN selected-peer lookup failed", zap.String("peer", l.peer.NMI), zap.Error(err))
		return nil
	}
	for _, ndp := range ownedNDPs(l.peer.NMI, local, info) {
		if l.peer.ID != "" && ndp.id != l.peer.ID {
			continue
		}
		if (ndp.init == local) != l.peer.Outgoing {
			continue
		}
		if _, err := control.command(fmt.Sprintf("NAN_NDP_TERMINATE peer_nmi=%s init_ndi=%s ndp_id=%s", ndp.peer, ndp.init, ndp.id)); err != nil {
			logger.Warn("NAN selected-peer termination failed", zap.String("peer", l.peer.NMI), zap.Error(err))
		}
	}
	return nil
}

func expireRadioLink(l *radioLink, now time.Time, queue *localmesh.CandidateQueue, attempts map[string]time.Time, selection *localmesh.PeerSelection, emit func(radioPeer)) {
	if l.draining || !l.expired(now) {
		return
	}
	l.beginDrain(now, emit)
	queue.Failed(l.peer.NMI, now)
	rememberRadioAttempt(attempts, l.peer.NMI, now)
	if selection != nil {
		selection.Failed(l.peer.Device, localmesh.RadioNAN)
	}
}

func finishRadioDisconnect(v map[string]string, local string, active map[string]*radioLink, recovery *radioRecovery, setupRecovery *ndpSetupRecovery, now time.Time, emit func(radioPeer)) {
	if p, ok := disconnectedPeer(v, local, active); ok {
		key := radioPeerKey(p, active)
		if key == "" {
			key, _ = radioEventLink(p.NMI, p.ID, active)
		}
		if l := active[key]; l != nil {
			if !l.peer.Lease.Retired() {
				recovery.setupFailed(l, v)
				setupRecovery.setupFailed(l, v, now)
			}
			l.peer.Lease.Release()
		}
		emit(p)
		delete(active, key)
	}
}
