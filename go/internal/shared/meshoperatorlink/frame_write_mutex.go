package meshoperatorlink

import "sync"

// frameWriteMutex alternates waiting routed datagrams with ordinary writers.
// It cannot preempt a partial TLS frame. After one datagram frame, an already
// waiting ordinary writer gets a turn, so control/management cannot starve.
// Babel is selected from its reserved lane only after this lock is acquired.
// Links without datagram writers retain ordinary mutually exclusive writes.
type frameWriteMutex struct {
	mu              sync.Mutex
	changed         *sync.Cond
	held            bool
	lastRouting     bool
	waitingRouting  int
	waitingOrdinary int
}

func (m *frameWriteMutex) Lock()        { m.lock(false) }
func (m *frameWriteMutex) lockRouting() { m.lock(true) }
func (m *frameWriteMutex) lock(routing bool) {
	m.mu.Lock()
	if m.changed == nil {
		m.changed = sync.NewCond(&m.mu)
	}
	if routing {
		m.waitingRouting++
	} else {
		m.waitingOrdinary++
	}
	for m.held || routing && m.lastRouting && m.waitingOrdinary > 0 || !routing && !m.lastRouting && m.waitingRouting > 0 {
		m.changed.Wait()
	}
	if routing {
		m.waitingRouting--
	} else {
		m.waitingOrdinary--
	}
	m.held, m.lastRouting = true, routing
	m.mu.Unlock()
}
func (m *frameWriteMutex) Unlock() {
	m.mu.Lock()
	m.held = false
	m.changed.Broadcast()
	m.mu.Unlock()
}
