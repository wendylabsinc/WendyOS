package meshsession

import (
	"context"
	"net/netip"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

const directPromotionPoll = 250 * time.Millisecond
const directPromotionRetry = time.Second

type automaticPromotionDial func(context.Context, meshid.Device, netip.AddrPort, *managedDirectSession) error

func (m *DirectManager) runAutomaticPromotions() {
	defer m.workers.Done()
	ticker := time.NewTicker(directPromotionPoll)
	defer ticker.Stop()
	for {
		m.pollAutomaticPromotions(time.Now(), m.dialPromotionTarget)
		select {
		case <-m.ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (m *DirectManager) pollAutomaticPromotions(now time.Time, dial automaticPromotionDial) {
	// Never call the routing owner while holding the manager lock.
	snapshot := m.cfg.PromotionSnapshot()
	m.mu.Lock()
	if m.closed || m.ctx.Err() != nil {
		m.mu.Unlock()
		return
	}
	peers := make(map[meshid.Device]*managedDirectSession, len(m.peers))
	for peer, target := range m.peers {
		peers[peer] = target
	}
	m.mu.Unlock()
	for peer, target := range peers {
		status := target.session.Status()
		if !status.Established || !status.Busy || status.Closed || status.Promoting || status.Promoted {
			continue
		}
		address, err := ResolvePromotion(snapshot, peer, status.DirectAvailable, status.BLECost, target.selected, now)
		if err != nil {
			continue
		}
		m.mu.Lock()
		if m.closed || m.ctx.Err() != nil || m.peers[peer] != target || target.promotionDial || target.automaticPending || now.Before(target.promotionRetry) {
			m.mu.Unlock()
			continue
		}
		target.automaticPending = true
		m.workers.Add(1)
		m.mu.Unlock()
		go func() {
			defer m.workers.Done()
			defer func() {
				m.mu.Lock()
				target.automaticPending = false
				target.promotionRetry = time.Now().Add(directPromotionRetry)
				m.mu.Unlock()
			}()
			// The captured epoch is checked atomically before claiming the dial. A
			// replacement epoch must not inherit a queued attempt or its endpoint.
			_ = dial(m.ctx, peer, address, target)
		}()
	}
}
