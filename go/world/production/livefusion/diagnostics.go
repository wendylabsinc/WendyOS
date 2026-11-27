package livefusion

import (
	"context"
	"time"

	"github.com/wendylabsinc/wendy/go/world/production/appclock"
	"github.com/wendylabsinc/wendy/go/world/production/fusioncapture"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
	"github.com/wendylabsinc/wendy/go/world/production/trustedtime"
)

// Diagnostics describes this process's work; its elapsed times grant no
// authority. Clock endpoints are copies of the intervals actually checked.
// At most sixteen attempts and eight physical checkpoints per attempt exist.
type Diagnostics struct {
	ElapsedNS   int64                   `json:"elapsed_ns"`
	Clock       ClockDiagnostics        `json:"clock"`
	Query       PhaseDiagnostics        `json:"query"`
	Verify      VerificationDiagnostics `json:"verify"`
	Attempts    []*AttemptDiagnostics   `json:"attempts"`
	Publication *PublicationDiagnostics `json:"publication,omitempty"`
}

// PublicationDiagnostics describes the single mutation attempt. Neither an OK
// RPC code nor an acknowledgment flag establishes a verified admission. Exact
// bounded acknowledgment proof bytes remain in Result.SignedRecordJSON.
type PublicationDiagnostics struct {
	Attempts         int    `json:"attempts"`
	ElapsedNS        int64  `json:"elapsed_ns"`
	RPCElapsedNS     int64  `json:"rpc_elapsed_ns"`
	PostAckElapsedNS int64  `json:"post_ack_elapsed_ns"`
	GRPCCode         string `json:"grpc_code,omitempty"`
	AckPresent       bool   `json:"ack_present"`
	AckAccepted      bool   `json:"ack_accepted"`
	ProofBytes       int    `json:"proof_bytes"`
	FailureStage     string `json:"failure_stage,omitempty"`
	FailureCode      string `json:"failure_code,omitempty"`
}

// Count is the number of records submitted, including a refused batch. ElapsedNS
// is inclusive batch time, counted once per batch, never multiplied by records.
type VerificationDiagnostics struct {
	PhaseDiagnostics
	Batches int `json:"batches"`
}
type PhaseDiagnostics struct {
	Count     int   `json:"count"`
	ElapsedNS int64 `json:"elapsed_ns"`
}
type ClockInterval struct {
	EarliestUnixNS int64 `json:"earliest_unix_ns"`
	LatestUnixNS   int64 `json:"latest_unix_ns"`
	SampleUnixNS   int64 `json:"sample_unix_ns,omitempty"`
	RoundTripNS    int64 `json:"round_trip_ns"`
}
type ClockDiagnostics struct {
	PhaseDiagnostics
	CombinedCount int            `json:"combined_count"`
	PhysicalCount int            `json:"physical_count"`
	First         *ClockInterval `json:"first,omitempty"`
	Last          *ClockInterval `json:"last,omitempty"`
	Failure       string         `json:"failure,omitempty"`
}
type AttemptDiagnostics struct {
	Attempt             int                       `json:"attempt"`
	ElapsedNS           int64                     `json:"elapsed_ns"`
	CandidateCount      int                       `json:"candidate_count"`
	MinCaptureUnixNS    int64                     `json:"min_capture_unix_ns,omitempty"`
	MaxCaptureUnixNS    int64                     `json:"max_capture_unix_ns,omitempty"`
	CaptureSpanNS       int64                     `json:"capture_span_ns,omitempty"`
	Clock               ClockDiagnostics          `json:"clock"`
	Query               PhaseDiagnostics          `json:"query"`
	QueryFailure        string                    `json:"query_failure,omitempty"`
	QueryRetryScheduled bool                      `json:"query_retry_scheduled,omitempty"`
	EvaluationOutcome   string                    `json:"evaluation_outcome,omitempty"`
	RetryReason         string                    `json:"retry_reason,omitempty"`
	MatchingElapsedNS   int64                     `json:"matching_elapsed_ns,omitempty"`
	Selection           *SelectionDiagnostics     `json:"selection,omitempty"`
	Verify              VerificationDiagnostics   `json:"verify"`
	Physical            fusioncapture.Diagnostics `json:"physical"`
	started             time.Time
}

type SelectionDiagnostics struct {
	Buffered         bool  `json:"buffered"`
	MinCaptureUnixNS int64 `json:"min_capture_unix_ns"`
	MaxCaptureUnixNS int64 `json:"max_capture_unix_ns"`
	CaptureSpanNS    int64 `json:"capture_span_ns"`
}

func (a *AttemptDiagnostics) recordSelection(current, selected []inputProof) {
	latest := make(map[string]bool, len(current))
	for _, p := range current {
		latest[p.reference.EnvelopeSHA256] = true
	}
	ranges := &AttemptDiagnostics{}
	ranges.captureRange(selected)
	a.Selection = &SelectionDiagnostics{MinCaptureUnixNS: ranges.MinCaptureUnixNS, MaxCaptureUnixNS: ranges.MaxCaptureUnixNS, CaptureSpanNS: ranges.CaptureSpanNS}
	for _, p := range selected {
		if !latest[p.reference.EnvelopeSHA256] {
			a.Selection.Buffered = true
		}
	}
}

type diagnosticClock struct {
	base    *appclock.Provider
	stats   *Diagnostics
	current *AttemptDiagnostics
}

func (c *diagnosticClock) Observe(ctx context.Context) (appclock.Observation, error) {
	start := time.Now()
	v, err := c.base.Observe(ctx)
	elapsed := time.Since(start)
	c.stats.Clock.record(v, err, elapsed)
	if c.current != nil {
		c.current.Clock.record(v, err, elapsed)
	}
	return v, err
}
func (d *ClockDiagnostics) record(o appclock.Observation, err error, elapsed time.Duration) {
	d.CombinedCount++
	d.Count++
	d.ElapsedNS += int64(elapsed)
	if err != nil {
		d.Failure = failureCode(err)
		return
	}
	v := ClockInterval{EarliestUnixNS: o.Physical.Earliest.UnixNano(), LatestUnixNS: o.Physical.Latest.UnixNano(), SampleUnixNS: o.SampleTime.UnixNano(), RoundTripNS: int64(o.RoundTrip)}
	if d.First == nil {
		first := v
		d.First = &first
	}
	d.Last = &v
}
func (d *ClockDiagnostics) recordPhysical(o appclock.PhysicalObservation, err error, elapsed time.Duration) {
	d.Count++
	d.PhysicalCount++
	d.ElapsedNS += int64(elapsed)
	if err != nil {
		d.Failure = failureCode(err)
		return
	}
	v := ClockInterval{EarliestUnixNS: o.Physical.Earliest.UnixNano(), LatestUnixNS: o.Physical.Latest.UnixNano(), RoundTripNS: int64(o.RoundTrip)}
	if d.First == nil {
		first := v
		d.First = &first
	}
	d.Last = &v
}
func (c *diagnosticClock) Now() (trustedtime.Interval, error) {
	start := time.Now()
	v, e := c.base.PhysicalObserve(context.Background())
	elapsed := time.Since(start)
	c.stats.Clock.recordPhysical(v, e, elapsed)
	if c.current != nil {
		c.current.Clock.recordPhysical(v, e, elapsed)
	}
	return v.Physical, e
}
func (c *diagnosticClock) SampleNow() (time.Time, error) {
	v, e := c.Observe(context.Background())
	if e != nil {
		return time.Time{}, e
	}
	if v.SampleClockDomain == "utc" {
		return v.Physical.Latest, nil
	}
	return v.SampleTime, nil
}
func (c *diagnosticClock) Close() error { return c.base.Close() }
func (s *session) recordQuery(start time.Time) {
	elapsed := int64(time.Since(start))
	s.clock.stats.Query.Count++
	s.clock.stats.Query.ElapsedNS += elapsed
	if a := s.clock.current; a != nil {
		a.Query.Count++
		a.Query.ElapsedNS += elapsed
	}
}
func (s *session) verifyBatch(ctx context.Context, signed []serviceorigin.SignedRecord) ([]serviceorigin.VerifiedRecord, error) {
	start := time.Now()
	defer func() {
		elapsed := int64(time.Since(start))
		s.clock.stats.Verify.Count += len(signed)
		s.clock.stats.Verify.Batches++
		s.clock.stats.Verify.ElapsedNS += elapsed
		if a := s.clock.current; a != nil {
			a.Verify.Count += len(signed)
			a.Verify.Batches++
			a.Verify.ElapsedNS += elapsed
		}
	}()
	return s.verifier.VerifyBatch(ctx, signed)
}
func (a *AttemptDiagnostics) captureRange(proofs []inputProof) {
	a.CandidateCount = len(proofs)
	var earliest, latest time.Time
	for _, p := range proofs {
		n := p.capture.CapturedUnixNS
		if a.MinCaptureUnixNS == 0 || n < a.MinCaptureUnixNS {
			a.MinCaptureUnixNS = n
		}
		if n > a.MaxCaptureUnixNS {
			a.MaxCaptureUnixNS = n
		}
		sample, e := p.capture.Sample()
		if e != nil {
			continue
		}
		end := p.record.Publication.Timing.SampleTime
		start := end
		if sample.Audio != nil {
			start = start.Add(-time.Duration(sample.Audio.DurationNS))
		}
		if earliest.IsZero() || start.Before(earliest) {
			earliest = start
		}
		if latest.IsZero() || end.After(latest) {
			latest = end
		}
	}
	if !earliest.IsZero() {
		a.CaptureSpanNS = int64(latest.Sub(earliest))
	}
}
