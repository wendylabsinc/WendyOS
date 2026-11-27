package agentmodule

import "github.com/wendylabsinc/wendy/go/world/production/trustedtime"

// physicalNow serializes observations by this module, including the final
// response checks. A later read cannot reopen a deadline by regressing from a
// clock interval already used for authority. Restart requires a fresh module;
// durable journal/evidence floors separately protect retained state.
func (m *Manager) physicalNow() (trustedtime.Interval, error) {
	m.clockMu.Lock()
	defer m.clockMu.Unlock()
	if m.policyFailed.Load() {
		return trustedtime.Interval{}, ErrDenied
	}
	w, e := m.deps.Clock.Now()
	if e != nil || w.Validate() != nil || m.clockSeen && (w.BootID != m.clockLast.BootID || w.Monotonic < m.clockLast.Monotonic || w.Earliest.Before(m.clockLast.Earliest) || w.Latest.Before(m.clockLast.Latest)) {
		m.policyFailed.Store(true)
		return trustedtime.Interval{}, ErrDenied
	}
	m.clockSeen = true
	m.clockLast = w
	return w, nil
}
