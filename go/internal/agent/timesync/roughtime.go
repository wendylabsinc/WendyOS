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

// resyncInterval is the wait after a query that disciplined the clock.
const resyncInterval = 6 * time.Hour

// Confidence levels a Consensus can carry, in descending strength.
const (
	// ConfidenceVerified is a quorum of at least three servers whose intervals
	// intersect.
	ConfidenceVerified = "verified"
	// ConfidenceDegraded is two independent signed responses that agree.
	ConfidenceDegraded = "degraded"
	// ConfidenceUnbounded is one response or none: no interval was agreed.
	ConfidenceUnbounded = "unbounded"
)

// disciplinesClock reports whether a consensus is strong enough to move the
// wall clock.
//
// WHY DEGRADED COUNTS. Two independent Roughtime servers, each response signed
// by a long-term key baked into the image and each interval intersecting the
// other, is strictly stronger evidence than the policy this loop replaced,
// which applied whatever the first server said. It is also stronger than the
// multicast path, which still applies a single relayed response. Requiring
// three left the realistic failure — a device that can reach some Roughtime
// servers but not others, behind a firewall or on a partitioned network — with
// a clock that never advanced at all, and a stale clock fails every
// certificate check on the device. One response remains insufficient: a single
// server can lie on its own, and nothing else contradicts it.
func disciplinesClock(c Consensus) bool {
	return c.Confidence == ConfidenceVerified || c.Confidence == ConfidenceDegraded
}

// RunDirect queries the baked-in Roughtime servers in a loop and disciplines the
// clock from any result that reaches at least degraded confidence. A weaker
// result is retried on the backoff schedule rather than waiting out the full
// re-sync interval, because a device that reached only one server has not
// synced and will not be helped by six hours of silence.
// Blocks until ctx is cancelled. Call as a goroutine.
func (m *Manager) RunDirect(ctx context.Context) {
	attempt := 0
	for {
		qCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		result, err := m.queryConsensus(qCtx, Servers)
		cancel()
		if err != nil {
			if m.logger != nil {
				m.logger.Warn("timesync: direct Roughtime query failed",
					zap.Error(err), zap.Int("attempt", attempt))
			}
			if !m.waitBackoff(ctx, attempt) {
				return
			}
			attempt++
			continue
		}

		m.RecordConsensus(result)
		if !disciplinesClock(result) {
			// Observed but not applied. Retrying soon is the point: this is a
			// device that reached too few servers, and the servers it could
			// not reach may be reachable in thirty seconds.
			if m.logger != nil {
				m.logger.Warn("timesync: Roughtime consensus too weak to set the clock; retrying",
					zap.Int("quorum", result.Quorum),
					zap.String("confidence", result.Confidence),
					zap.Int("attempt", attempt))
			}
			if !m.waitBackoff(ctx, attempt) {
				return
			}
			attempt++
			continue
		}

		attempt = 0
		if m.logger != nil {
			m.logger.Info("timesync: synced via Roughtime",
				zap.Int("quorum", result.Quorum),
				zap.String("confidence", result.Confidence),
				zap.Duration("uncertainty", time.Duration((result.UpperOffsetNanos-result.LowerOffsetNanos)/2)))
		}
		boot, _ := bootTimeNanos()
		mid := result.LowerOffsetNanos + (result.UpperOffsetNanos-result.LowerOffsetNanos)/2
		m.applyTime(time.Unix(0, boot+mid))

		if !m.wait(ctx, resyncInterval) {
			return
		}
	}
}

// waitBackoff sleeps the delay for attempt, reporting false when ctx ended.
func (m *Manager) waitBackoff(ctx context.Context, attempt int) bool {
	return m.wait(ctx, backoffSchedule[min(attempt, len(backoffSchedule)-1)])
}

// wait sleeps d, reporting false when ctx ended first.
func (m *Manager) wait(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-m.after(d):
		return true
	}
}
