package udpmessage

import (
	"errors"
	"time"
)

func (m *Manager) pickLocked(now time.Time) (*Endpoint, *outgoing) {
	if len(m.order) == 0 {
		return nil, nil
	}
	for range len(m.order) {
		m.next %= len(m.order)
		p := m.order[m.next]
		m.next++
		for range len(p.flows) {
			p.next %= len(p.flows)
			e := p.flows[p.next]
			p.next++
			e.expireLocked(now)
			if e.closed.Load() || e.try == nil || len(e.queue) == 0 {
				continue
			}
			for i, q := range e.queue {
				if !now.Before(q.retry) {
					// One chunk per message visit lets a later small message overtake a
					// large one. Peer/flow rotation is independent of their queue lengths.
					copy(e.queue[i:], e.queue[i+1:])
					e.queue[len(e.queue)-1] = q
					return e, q
				}
			}
		}
	}
	return nil, nil
}
func (m *Manager) removeLocked(e *Endpoint, q *outgoing) {
	for i, item := range e.queue {
		if item == q {
			copy(e.queue[i:], e.queue[i+1:])
			e.queue[len(e.queue)-1] = nil
			e.queue = e.queue[:len(e.queue)-1]
			break
		}
	}
	m.releaseLocked(q.reservation)
}
func (m *Manager) step() bool {
	m.mu.Lock()
	e, q := m.pickLocked(m.now())
	m.mu.Unlock()
	if e == nil {
		return false
	}
	e.sendMu.Lock()
	if e.closed.Load() {
		e.sendMu.Unlock()
		return true
	}
	m.mu.Lock()
	local, maximum, send, guard := e.local, e.maximum, e.try, e.guard
	if q.stride == 0 {
		q.stride = maximum - HeaderSize
	}
	stride, index := q.stride, q.index
	m.mu.Unlock()
	payload := q.payload
	if !local {
		payload = encode(q.id, q.payload, stride, index)
	}
	// No app queue/budget mutex is held through authorization or transport.
	admitted := false
	err := guard(func() error {
		admitted = true
		if e.closed.Load() {
			return ErrClosed
		}
		m.mu.Lock()
		expired := q.reservation.released || !m.now().Before(q.born.Add(Lifetime))
		m.mu.Unlock()
		if expired {
			return errExpired
		}
		return send(payload)
	})
	e.sendMu.Unlock()
	m.mu.Lock()
	if q.reservation.released {
		m.mu.Unlock()
		return true
	}
	now := m.now()
	closeFlow := false
	switch {
	case err == nil:
		m.stats.TransportAdmissions++
		if local {
			m.stats.LocalDeliveryAdmissions++
		}
		q.index++
		q.backoff = 0
		q.retry = time.Time{}
		if local || q.index >= max(1, (len(q.payload)+q.stride-1)/q.stride) {
			m.removeLocked(e, q)
		}
	case errors.Is(err, ErrQueueFull):
		m.stats.TransportQueueRefusals++
		if q.backoff == 0 {
			q.backoff = 5 * time.Millisecond
		} else {
			q.backoff = min(100*time.Millisecond, 2*q.backoff)
		}
		q.retry = now.Add(q.backoff)
	default:
		var tooLarge *ChunkTooLarge
		if errors.As(err, &tooLarge) && !local && tooLarge.Maximum >= MinChunkSize && tooLarge.Maximum < q.stride+HeaderSize {
			e.maximum = min(maximum, tooLarge.Maximum)
			if m.stats.SendChunkSeenMin == 0 || e.maximum < m.stats.SendChunkSeenMin {
				m.stats.SendChunkSeenMin = e.maximum
			}
			if q.index == 0 {
				q.stride = e.maximum - HeaderSize
				break
			}
		}
		switch {
		case !admitted:
			m.stats.GuardRejects++
		case errors.As(err, &tooLarge):
			m.stats.SizeDrops++
		case errors.Is(err, errExpired):
			m.stats.ExpiredMessages++
		default:
			m.stats.TransportFailures++
		}
		m.removeLocked(e, q)
		// A partially admitted message is never restarted. Only future
		// messages use the reduced path capacity. Other terminal failures
		// cancel the flow; an expired/oversize message remains a UDP loss.
		closeFlow = !errors.As(err, &tooLarge) && !errors.Is(err, errExpired)

	}
	m.mu.Unlock()
	if closeFlow {
		e.Close()
	}
	return true
}
func (m *Manager) run() {
	defer close(m.done)
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-m.stop:
			return
		case <-m.wake:
		case <-tick.C:
		}
		for range 64 {
			if !m.step() {
				break
			}
		}
		m.mu.Lock()
		now := m.now()
		for _, p := range m.order {
			for _, e := range p.flows {
				e.expireLocked(now)
			}
		}
		m.mu.Unlock()
	}
}
