package timesync

import (
	"context"
	"net"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/roughtime"
	"go.uber.org/zap"
)

// Manager coordinates all time-sync sources and applies the best verified time.
// All sources call Apply; the clock is only ever advanced.
type Manager struct {
	logger         *zap.Logger
	configPath     string
	mu             sync.RWMutex
	clockMu        sync.Mutex
	latest         *Consensus
	directObserver func(DirectSyncObservation)

	// Injection points for tests of time-sync loops. Nil means use the real
	// network, clock write and retry timer, so a zero-value Manager still behaves.
	query       func(context.Context, []roughtime.Server) (Consensus, error)
	apply       func(time.Time)
	applyResult func(time.Time) error
	now         func() time.Time
	boot        func() (int64, error)
	sleep       func(time.Duration) <-chan time.Time

	multicastListen     func() (multicastPacketConn, error)
	multicastInterfaces func() ([]net.Interface, error)
	multicastInterval   time.Duration
}

func (m *Manager) queryConsensus(ctx context.Context, servers []roughtime.Server) (Consensus, error) {
	if m.query != nil {
		return m.query(ctx, servers)
	}
	return QueryConsensus(ctx, servers)
}

func (m *Manager) applyTime(t time.Time) {
	if m.apply != nil {
		m.apply(t)
		return
	}
	m.Apply(t)
}

func (m *Manager) after(d time.Duration) <-chan time.Time {
	if m.sleep != nil {
		return m.sleep(d)
	}
	return time.After(d)
}

func (m *Manager) RecordConsensus(c Consensus) {
	m.mu.Lock()
	defer m.mu.Unlock()
	copy := c
	m.latest = &copy
}
func (m *Manager) LatestConsensus() (Consensus, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.latest == nil {
		return Consensus{}, false
	}
	return *m.latest, true
}

// NewManager creates a Manager. logger may be nil. configPath is the agent
// config directory (e.g. /etc/wendy-agent).
func NewManager(logger *zap.Logger, configPath string) *Manager {
	return &Manager{logger: logger, configPath: configPath}
}

// ApplyFloor reads the config-partition floor and advances the clock if it
// is ahead of the current time. Called once at agent startup.
func (m *Manager) ApplyFloor() {
	floor, refused := readFloor(m.configPath)
	if !refused.IsZero() && m.logger != nil {
		m.logger.Warn("timesync: ignoring implausible clock floor",
			zap.Time("floor", refused),
			zap.Time("min", floorMin), zap.Time("max", floorMax))
	}
	if floor.IsZero() {
		return
	}
	if m.apply != nil {
		m.apply(floor)
		return
	}
	if err := AdvanceTo(floor, m.logger); err != nil && m.logger != nil {
		m.logger.Warn("timesync: floor apply failed", zap.Error(err))
	}
}

// Apply advances the clock to t if t is after the current time.
// Safe to call from any goroutine.
func (m *Manager) Apply(t time.Time) {
	if err := m.advance(t); err != nil && m.logger != nil {
		m.logger.Warn("timesync: apply failed", zap.Error(err))
	}
}

// Serialize the agent's independent verified clock sources so their
// advance-only checks cannot race one another into a backward step.
func (m *Manager) advance(t time.Time) error {
	m.clockMu.Lock()
	defer m.clockMu.Unlock()
	return m.applyClock(t)
}

// applyClock requires clockMu. All production clock writes still use the same
// advance-only operation; the other branches are isolated test injections.
func (m *Manager) applyClock(t time.Time) error {
	if m.applyResult != nil {
		return m.applyResult(t)
	}
	if m.apply != nil {
		m.apply(t)
		return nil
	}
	return AdvanceTo(t, m.logger)
}
