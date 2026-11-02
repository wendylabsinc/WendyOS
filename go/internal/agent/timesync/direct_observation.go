package timesync

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/asyncdiag"
	"go.uber.org/zap"
)

// DirectQueryPath is an observed socket path for one included, independently
// signed reply. Addresses are diagnostic metadata and never authorize time.
type DirectQueryPath struct {
	Server           string `json:"server"`
	LocalAddress     string `json:"local_address,omitempty"`
	RemoteAddress    string `json:"remote_address,omitempty"`
	SendBootNanos    int64  `json:"send_boottime_nanos"`
	ReceiveBootNanos int64  `json:"receive_boottime_nanos"`
}

// DirectSyncObservation records what happened around one direct quorum's clock
// write. A successful advance-only call alone does not prove that UTC is inside
// the signed interval: an already-ahead clock is deliberately never rewound.
type DirectSyncObservation struct {
	Source                    string
	Quorum                    int
	Confidence                string
	Paths                     []DirectQueryPath
	UTCBeforeApply            time.Time
	UTCAfterApply             time.Time
	BootBeforeApplyNanos      int64
	BootAfterApplyStartNanos  int64
	BootAfterApplyNanos       int64
	Target                    time.Time
	VerifiedEarliest          time.Time
	VerifiedLatest            time.Time
	VerifiedLowerBootNanos    int64
	VerifiedUpperBootNanos    int64
	ApplyAttempted            bool
	ApplyError                error
	ObservationError          error
	UTCWithinVerifiedInterval bool
}

// SetDirectSyncObserver installs an optional observer. It receives measurements,
// not a policy/confidence transition. It runs outside clockMu and Manager.mu.
func (m *Manager) SetDirectSyncObserver(observer func(DirectSyncObservation)) {
	m.mu.Lock()
	m.directObserver = observer
	m.mu.Unlock()
}

func (m *Manager) wallTime() time.Time {
	if m.now != nil {
		return m.now()
	}
	return time.Now()
}
func (m *Manager) bootTime() (int64, error) {
	if m.boot != nil {
		return m.boot()
	}
	return bootTimeNanos()
}

func directInterval(c Consensus, boot int64) (time.Time, time.Time, error) {
	if boot < 0 || c.UpperOffsetNanos < c.LowerOffsetNanos || c.UpperOffsetNanos > math.MaxInt64-boot {
		return time.Time{}, time.Time{}, fmt.Errorf("timesync: invalid direct boot-time interval")
	}
	return time.Unix(0, boot+c.LowerOffsetNanos).UTC(), time.Unix(0, boot+c.UpperOffsetNanos).UTC(), nil
}

func (m *Manager) applyDirect(c Consensus) DirectSyncObservation {
	o := DirectSyncObservation{Source: "direct", Quorum: c.Quorum, Confidence: c.Confidence}
	for _, ev := range c.Evidence {
		if ev.Included {
			o.Paths = append(o.Paths, DirectQueryPath{Server: ev.Server, LocalAddress: ev.LocalAddress, RemoteAddress: ev.RemoteAddress, SendBootNanos: ev.SendBootNanos, ReceiveBootNanos: ev.ReceiveBootNanos})
		}
	}
	m.clockMu.Lock()
	o.UTCBeforeApply = m.wallTime().UTC()
	o.BootBeforeApplyNanos, o.ObservationError = m.bootTime()
	if o.ObservationError == nil {
		var lower, upper time.Time
		lower, upper, o.ObservationError = directInterval(c, o.BootBeforeApplyNanos)
		if o.ObservationError == nil {
			o.Target = lower.Add(upper.Sub(lower) / 2)
			o.VerifiedEarliest, o.VerifiedLatest = lower, upper
			o.VerifiedLowerBootNanos, o.VerifiedUpperBootNanos = o.BootBeforeApplyNanos, o.BootBeforeApplyNanos
			if m.logger != nil {
				asyncdiag.Log(m.logger, zap.InfoLevel, "timesync: direct Roughtime quorum ready", directObservationFields(o)...)
			}
			o.ApplyAttempted = true
			o.ApplyError = m.applyClock(o.Target)
		}
	}
	// Bracket the UTC read. The intersection below holds for every possible
	// BOOTTIME instant of that read, rather than falsely widening the interval
	// by a slow sample taken only before or after it.
	start, startErr := m.bootTime()
	o.BootAfterApplyStartNanos = start
	o.UTCAfterApply = m.wallTime().UTC()
	end, endErr := m.bootTime()
	o.BootAfterApplyNanos = end
	afterErr := errors.Join(startErr, endErr)
	if afterErr == nil && (start < o.BootBeforeApplyNanos || end < start) {
		afterErr = fmt.Errorf("timesync: boot clock moved backward during apply")
	}
	if afterErr == nil {
		var lower, upper time.Time
		var lowerErr, upperErr error
		lower, _, lowerErr = directInterval(c, end)
		_, upper, upperErr = directInterval(c, start)
		afterErr = errors.Join(lowerErr, upperErr)
		if afterErr == nil && upper.Before(lower) {
			afterErr = fmt.Errorf("timesync: UTC sample exceeds verified interval width")
		}
		if afterErr == nil {
			o.VerifiedEarliest, o.VerifiedLatest = lower, upper
			o.VerifiedLowerBootNanos, o.VerifiedUpperBootNanos = end, start
		}
	}
	o.ObservationError = errors.Join(o.ObservationError, afterErr)
	o.UTCWithinVerifiedInterval = o.ObservationError == nil && !o.UTCAfterApply.Before(o.VerifiedEarliest) && !o.UTCAfterApply.After(o.VerifiedLatest)
	m.clockMu.Unlock()
	if m.logger != nil {
		fields := directObservationFields(o)
		outcome := "unconfirmed"
		if o.ApplyError != nil || o.ObservationError != nil {
			outcome = "failed"
		} else if o.UTCWithinVerifiedInterval {
			outcome = "no-step-requested"
			if o.Target.After(o.UTCBeforeApply) {
				// This reports the successful call, not an assumption that
				// settimeofday ran: another source may have caught UTC up.
				outcome = "apply-completed"
			}
		}
		fields = append(fields, zap.String("outcome", outcome), zap.Bool("utc_within_verified_interval", o.UTCWithinVerifiedInterval), zap.NamedError("apply_error", o.ApplyError), zap.NamedError("observation_error", o.ObservationError))
		if outcome == "failed" || outcome == "unconfirmed" {
			asyncdiag.Log(m.logger, zap.WarnLevel, "timesync: direct Roughtime apply complete", fields...)
		} else {
			asyncdiag.Log(m.logger, zap.InfoLevel, "timesync: direct Roughtime apply complete", fields...)
		}
	}
	m.mu.RLock()
	observer := m.directObserver
	m.mu.RUnlock()
	if observer != nil {
		observer(o)
	}
	return o
}

func directObservationFields(o DirectSyncObservation) []zap.Field {
	paths, _ := json.Marshal(o.Paths)
	fields := []zap.Field{zap.String("source", o.Source), zap.Int("quorum", o.Quorum), zap.String("confidence", o.Confidence), zap.Reflect("paths", json.RawMessage(paths)), zap.Time("utc_before_apply", o.UTCBeforeApply), zap.Int64("boottime_before_apply_nanos", o.BootBeforeApplyNanos), zap.Time("target", o.Target), zap.Time("verified_earliest", o.VerifiedEarliest), zap.Time("verified_latest", o.VerifiedLatest), zap.Int64("verified_lower_boottime_nanos", o.VerifiedLowerBootNanos), zap.Int64("verified_upper_boottime_nanos", o.VerifiedUpperBootNanos), zap.Bool("apply_attempted", o.ApplyAttempted)}
	if !o.UTCAfterApply.IsZero() {
		fields = append(fields, zap.Time("utc_after_apply", o.UTCAfterApply), zap.Int64("boottime_after_apply_start_nanos", o.BootAfterApplyStartNanos), zap.Int64("boottime_after_apply_nanos", o.BootAfterApplyNanos))
	}
	return fields
}
