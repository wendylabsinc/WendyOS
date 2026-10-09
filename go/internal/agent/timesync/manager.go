package timesync

import (
	"context"
	"fmt"
	"math"
	"net"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/roughtime"
	"go.uber.org/zap"
)

// Manager coordinates all time-sync sources and applies the best verified time.
// All sources call Apply; the clock is only ever advanced.
type Manager struct {
	logger     *zap.Logger
	configPath string
	mu         sync.RWMutex
	latest     *Consensus
	floorMu    sync.Mutex

	// Injection points for tests of time-sync loops. Nil means use the real
	// network, clock write and retry timer, so a zero-value Manager still behaves.
	query func(context.Context, []roughtime.Server) (Consensus, error)
	apply func(time.Time)
	sleep func(time.Duration) <-chan time.Time

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

// persistConsensusFloor uses the lower end of the authenticated interval, not
// its midpoint or the current wall clock. A serialized read/replace preserves
// the maximum floor even when time sources complete concurrently.
func (m *Manager) persistConsensusFloor(c Consensus, boot int64) error {
	if !disciplinesClock(c) || c.Quorum < 2 || c.UpperOffsetNanos < c.LowerOffsetNanos || boot < 0 {
		return fmt.Errorf("timesync: insufficient consensus for a durable floor")
	}
	if c.LowerOffsetNanos > math.MaxInt64-boot {
		return fmt.Errorf("timesync: verified floor overflow")
	}
	m.floorMu.Lock()
	defer m.floorMu.Unlock()
	return advanceVerifiedFloor(m.configPath, time.Unix(0, boot+c.LowerOffsetNanos))
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

// TimeWindow is the interval supported by a fresh authenticated consensus.
// Time-gated operations must be valid throughout the interval, not just at a
// midpoint selected from it. The bounds are captured at query completion.
type TimeWindow struct {
	Earliest time.Time
	Latest   time.Time
}

// FreshTime obtains new signed evidence rather than treating LatestConsensus
// or the boot floor as proof of current time. Unknown time or failed durable
// floor advancement returns an error; callers must retain their current state.
func (m *Manager) FreshTime(ctx context.Context) (TimeWindow, error) {
	queryCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	c, err := m.queryConsensus(queryCtx, Servers)
	if err != nil {
		return TimeWindow{}, fmt.Errorf("timesync: fresh authenticated query failed: %w", err)
	}
	if err := queryCtx.Err(); err != nil {
		return TimeWindow{}, err
	}
	boot, err := bootTimeNanos()
	if err != nil {
		return TimeWindow{}, err
	}
	if boot < 0 || c.UpperOffsetNanos > math.MaxInt64-boot {
		return TimeWindow{}, fmt.Errorf("timesync: authenticated upper bound overflow")
	}
	upper := time.Unix(0, boot+c.UpperOffsetNanos)
	if upper.Before(floorMin) || upper.After(floorMax) {
		return TimeWindow{}, fmt.Errorf("timesync: implausible authenticated upper bound")
	}
	if err := m.persistConsensusFloor(c, boot); err != nil {
		return TimeWindow{}, err
	}
	// A verified response cannot authorize an operation below the durable
	// anti-rollback floor, including a newer update from a concurrent source.
	m.floorMu.Lock()
	floor, refused := readFloor(m.configPath)
	m.floorMu.Unlock()
	if floor.IsZero() || !refused.IsZero() || upper.Before(floor) {
		return TimeWindow{}, fmt.Errorf("timesync: authenticated interval precedes durable floor")
	}
	lower := time.Unix(0, boot+c.LowerOffsetNanos)
	if lower.Before(floor) {
		lower = floor
	}
	if err := queryCtx.Err(); err != nil {
		return TimeWindow{}, err
	}
	m.RecordConsensus(c)
	return TimeWindow{Earliest: lower, Latest: upper}, nil
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
	if err := AdvanceTo(floor, m.logger); err != nil && m.logger != nil {
		m.logger.Warn("timesync: floor apply failed", zap.Error(err))
	}
}

// Apply advances the clock to t if t is after the current time.
// Safe to call from any goroutine.
func (m *Manager) Apply(t time.Time) {
	if err := AdvanceTo(t, m.logger); err != nil && m.logger != nil {
		m.logger.Warn("timesync: apply failed", zap.Error(err))
	}
}
