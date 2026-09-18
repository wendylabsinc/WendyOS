package timesync

import (
	"context"
	"time"

	"go.uber.org/zap"
)

// backoffSchedule is the delay sequence after a failed Roughtime query.
var backoffSchedule = []time.Duration{
	5 * time.Second,
	30 * time.Second,
	5 * time.Minute,
	30 * time.Minute,
}

// RunDirect queries the baked-in Roughtime servers in a loop, backing off on
// failure and re-querying every 6 hours after a successful sync.
// Blocks until ctx is cancelled. Call as a goroutine.
func (m *Manager) RunDirect(ctx context.Context) {
	attempt := 0
	for {
		qCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		result, err := QueryConsensus(qCtx, Servers)
		cancel()
		if err != nil {
			if m.logger != nil {
				m.logger.Warn("timesync: direct Roughtime query failed",
					zap.Error(err), zap.Int("attempt", attempt))
			}
			delay := backoffSchedule[min(attempt, len(backoffSchedule)-1)]
			attempt++
			select {
			case <-ctx.Done():
				return
			case <-time.After(delay):
			}
			continue
		}

		attempt = 0
		if m.logger != nil {
			m.logger.Info("timesync: synced via Roughtime",
				zap.Int("quorum", result.Quorum),
				zap.String("confidence", result.Confidence),
				zap.Duration("uncertainty", time.Duration((result.UpperOffsetNanos-result.LowerOffsetNanos)/2)))
		}
		m.RecordConsensus(result)
		// Clock discipline is deliberately separate from observation. Only a
		// verified quorum may advance an obviously stale wall clock.
		if result.Confidence == "verified" {
			boot, _ := bootTimeNanos()
			mid := result.LowerOffsetNanos + (result.UpperOffsetNanos-result.LowerOffsetNanos)/2
			m.Apply(time.Unix(0, boot+mid))
		}

		// Re-query every 6 hours.
		select {
		case <-ctx.Done():
			return
		case <-time.After(6 * time.Hour):
		}
	}
}
