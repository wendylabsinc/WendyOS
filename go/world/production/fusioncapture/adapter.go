// Package fusioncapture connects verified agent capture proofs to the ordinary
// fusion app. It is intentionally separate from sensorrecord, which the agent
// may import for admission without importing a domain inference implementation.
package fusioncapture

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/fusion"
	"github.com/wendylabsinc/wendy/go/world/internal/sensing"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"github.com/wendylabsinc/wendy/go/world/production/jws"
	"github.com/wendylabsinc/wendy/go/world/production/sensorrecord"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
	"github.com/wendylabsinc/wendy/go/world/production/trustedtime"
)

const (
	MaxRecords    = 256
	MaxProofBytes = 256 << 10
	MaxBatchBytes = 4 << 20
)

// VerifyFunc must validate device certificates, signatures, authenticated
// physical time and current exact site/service authority. A normal choice is
// serviceorigin.Verifier.Verify with commissioned public roots, trusted time
// and an Authorize callback for the exact subscribed source scopes. This package
// does not install those trust roots or derive authority from proof content.
type VerifyFunc func(context.Context, serviceorigin.SignedRecord) (serviceorigin.VerifiedRecord, error)

// VerifyBatchFunc must freshly verify the complete bounded set, including all
// immutable signatures before whole-set authority callbacks and common final
// physical validity. It returns owned records in the original input order.
// No cached prior verification or partially trusted result satisfies this call.
type VerifyBatchFunc func(context.Context, []serviceorigin.SignedRecord) ([]serviceorigin.VerifiedRecord, error)

// AdmittedBatchFunc projects exact originals read through a trusted native
// World broker. The broker, rather than this callback, has verified signatures,
// current source authority and read admission. This is not an adapter for
// arbitrary network envelopes. A derived publication must still go through
// the broker's exact-input validation and admission.
type AdmittedBatchFunc func(context.Context, []serviceorigin.SignedRecord) ([]serviceorigin.Record, error)

const MaxBatchRecords = 16

// StaleCaptureError means a verified or broker-admitted batch exceeded an original
// capture deadline. It never represents a future capture, failed authority,
// missing media or an untrusted/regressed clock. Its private reason is set only
// after this package's complete verification and common clock checks.
type StaleCaptureError struct{ reason string }

func (e *StaleCaptureError) Error() string  { return "verified capture expired: " + e.reason }
func (e *StaleCaptureError) Reason() string { return e.reason }

// ErrCaptureMedia is a terminal failure to retrieve or decode the exact media
// bound to a verified capture. It must not be treated as another observation.
var ErrCaptureMedia = errors.New("captured media refused")

// Options keeps physical capture age separate from the domain simulation clock
// and from the verifier's certificate checks. Physical is mandatory and must be
// independently authenticated; no source or machine wall-clock fallback exists.
type Options struct {
	Physical    trustedtime.Provider
	Diagnostics *Diagnostics
}

// Diagnostics records a bounded copy of actual physical checks. Elapsed time
// is local profiling data, never a substitute for authenticated clock bounds.
type Diagnostics struct {
	Checks []PhysicalCheckpoint `json:"checks,omitempty"`
}
type PhysicalCheckpoint struct {
	Phase            string `json:"phase"`
	ElapsedNS        int64  `json:"elapsed_ns"`
	ClockElapsedNS   int64  `json:"clock_elapsed_ns"`
	EarliestUnixNS   int64  `json:"earliest_unix_ns,omitempty"`
	LatestUnixNS     int64  `json:"latest_unix_ns,omitempty"`
	CaptureCount     int    `json:"capture_count"`
	MinCaptureUnixNS int64  `json:"min_capture_unix_ns,omitempty"`
	MaxCaptureUnixNS int64  `json:"max_capture_unix_ns,omitempty"`
	Future           bool   `json:"future"`
	Stale            bool   `json:"stale"`
	Failure          string `json:"failure,omitempty"`
}

type physicalGuard struct {
	provider    trustedtime.Provider
	last        trustedtime.Interval
	seen        bool
	diagnostics *Diagnostics
	phase       string
	started     time.Time
}

func (g *physicalGuard) check(captures []sensorrecord.Capture) error {
	point := PhysicalCheckpoint{Phase: g.phase, CaptureCount: len(captures)}
	defer func() {
		if g.diagnostics != nil && len(g.diagnostics.Checks) < 8 {
			point.ElapsedNS = int64(time.Since(g.started))
			g.diagnostics.Checks = append(g.diagnostics.Checks, point)
		}
	}()
	if g.provider == nil {
		point.Failure = "clock_unavailable"
		return errors.New("authenticated physical capture clock required")
	}
	clockStart := time.Now()
	w, err := g.provider.Now()
	point.ClockElapsedNS = int64(time.Since(clockStart))
	if err != nil || w.Validate() != nil {
		point.Failure = "clock_unavailable"
		return errors.New("authenticated physical capture clock unavailable")
	}
	point.EarliestUnixNS = w.Earliest.UnixNano()
	point.LatestUnixNS = w.Latest.UnixNano()
	for _, c := range captures {
		n := c.CapturedUnixNS
		if point.MinCaptureUnixNS == 0 || n < point.MinCaptureUnixNS {
			point.MinCaptureUnixNS = n
		}
		if n > point.MaxCaptureUnixNS {
			point.MaxCaptureUnixNS = n
		}
		captured := time.Unix(0, n)
		if captured.After(w.Latest) {
			point.Future = true
		}
		if w.Latest.Sub(captured) > sensorrecord.MaxPhysicalAge {
			point.Stale = true
		}
	}
	if g.seen && (w.BootID != g.last.BootID || w.Monotonic < g.last.Monotonic || w.Earliest.Before(g.last.Earliest) || w.Latest.Before(g.last.Latest)) {
		point.Failure = "clock_regressed"
		return errors.New("authenticated physical capture clock regressed")
	}
	g.seen, g.last = true, w
	// Check the whole batch before classifying staleness. A future or malformed
	// capture must not be hidden by another, merely old capture.
	var refusal error
	for _, capture := range captures {
		if err := sensorrecord.PhysicalCurrent(capture, w); err != nil {
			if capture.CapturedUnixNS <= 0 || time.Unix(0, capture.CapturedUnixNS).After(w.Latest) {
				point.Failure = "capture_age_refused"
				return err
			}
			refusal = err
		}
	}
	if refusal != nil {
		point.Failure = "capture_age_refused"
		return &StaleCaptureError{reason: "physical_capture_expired"}
	}
	return nil
}

type prepared struct {
	records  []world.Component
	decoder  fusion.CaptureDecoder
	captures []sensorrecord.Capture
}

// Evaluate verifies each original proof before interpreting it. The exact JSON
// envelope bytes returned by the agent are hashed as input proof identity; the
// signed inner record and capture payload are never reconstructed for signing.
// The loader must fetch evidence using the same exact authorized source scopes.
func Evaluate(ctx context.Context, cfg fusion.Config, options Options, proofs [][]byte, verify VerifyFunc, load fusion.Loader) (fusion.Result, error) {
	return evaluate(ctx, cfg, options, proofs, func(ctx context.Context, owned [][]byte, guard *physicalGuard) (prepared, error) {
		return prepare(ctx, owned, verify, guard)
	}, load, nil)
}

// EvaluateBatch uses an explicit whole-set verifier for both preparation rounds.
// Every round receives the same original proof bytes; a failed batch never falls
// back to single-record verification or supplies a partially trusted decoder.
func EvaluateBatch(ctx context.Context, cfg fusion.Config, options Options, proofs [][]byte, verify VerifyBatchFunc, load fusion.Loader) (fusion.Result, error) {
	if len(proofs) > MaxBatchRecords {
		return fusion.Result{}, errors.New("capture batch cardinality exceeded")
	}
	return evaluate(ctx, cfg, options, proofs, func(ctx context.Context, owned [][]byte, guard *physicalGuard) (prepared, error) {
		return prepareBatch(ctx, owned, verify, guard)
	}, load, nil)
}

// EvaluateAdmitted uses one broker-admitted projection, then the same original
// capture decoding, media validation and final sample/physical checks as
// EvaluateBatch. It performs no independent input signature verification or
// renewed source-authority query. The caller must publish through that broker
// for current authority and exact original dependency checks at admission.
func EvaluateAdmitted(ctx context.Context, cfg fusion.Config, options Options, proofs [][]byte, project AdmittedBatchFunc, load fusion.Loader) (fusion.Result, error) {
	if project == nil || len(proofs) > MaxBatchRecords {
		return fusion.Result{}, errors.New("bounded broker-admitted projection required")
	}
	var saved []serviceorigin.VerifiedRecord
	checkWindow := func(w trustedtime.Interval) error {
		for _, input := range saved {
			if w.Earliest.Before(time.UnixMilli(input.Record.IssuedAtUnixMS)) ||
				!w.Latest.Before(time.UnixMilli(input.Record.ExpiresAtUnixMS)) {
				return serviceorigin.ErrExpired
			}
		}
		return nil
	}
	return evaluate(ctx, cfg, options, proofs, func(ctx context.Context, owned [][]byte, guard *physicalGuard) (prepared, error) {
		batch, err := prepareBatch(ctx, owned, func(ctx context.Context, signed []serviceorigin.SignedRecord) ([]serviceorigin.VerifiedRecord, error) {
			if saved == nil {
				projected, err := project(ctx, signed)
				if err != nil {
					return nil, err
				}
				if len(projected) != len(signed) {
					return nil, errors.New("broker-admitted projection is incomplete")
				}
				saved = make([]serviceorigin.VerifiedRecord, len(signed))
				for i, item := range signed {
					var record serviceorigin.Record
					if jws.DecodeStrict(item.Record, &record) != nil || !reflect.DeepEqual(record, projected[i]) {
						return nil, errors.New("broker-admitted projection changed original record")
					}
					// The legacy private builder consumes this record container.
					// No certificate or independent-verification claim is added.
					saved[i] = serviceorigin.VerifiedRecord{Record: record, Bytes: bytes.Clone(item.Record)}
				}
			}
			if err := checkWindow(guard.last); err != nil {
				return nil, err
			}
			return saved, nil
		}, guard)
		if err != nil {
			return prepared{}, err
		}
		// Reuse the common physical interval already observed by preparation.
		// Even a negative inference cannot outlive an original signing window.
		if err := checkWindow(guard.last); err != nil {
			return prepared{}, err
		}
		return batch, nil
	}, load, checkWindow)
}

type prepareFunc func(context.Context, [][]byte, *physicalGuard) (prepared, error)

func evaluate(ctx context.Context, cfg fusion.Config, options Options, proofs [][]byte, makeBatch prepareFunc, load fusion.Loader, finalWindow func(trustedtime.Interval) error) (fusion.Result, error) {
	if cfg.CaptureType != "" || cfg.DecodeCapture != nil || cfg.Replay {
		return fusion.Result{}, errors.New("fusion capture wrapper requires an unmodified live configuration")
	}
	if err := proofBounds(proofs); err != nil {
		return fusion.Result{}, err
	}
	owned := make([][]byte, len(proofs))
	for i := range proofs {
		owned[i] = bytes.Clone(proofs[i])
	}
	if options.Diagnostics != nil {
		options.Diagnostics.Checks = nil
	}
	guard := &physicalGuard{provider: options.Physical, diagnostics: options.Diagnostics, phase: "initial_prepare", started: time.Now()}
	batch, err := makeBatch(ctx, owned, guard)
	if err != nil {
		return fusion.Result{}, err
	}
	cfg.CaptureType, cfg.DecodeCapture = sensorrecord.ComponentType, batch.decoder
	if cfg.Now == nil || load == nil {
		return fusion.Result{}, errors.New("capture source clock and media loader required")
	}
	sourceNow := cfg.Now
	var first, last time.Time
	clockFault := false
	cfg.Now = func() time.Time {
		now := sourceNow()
		if now.IsZero() || !last.IsZero() && now.Before(last) {
			clockFault = true
		}
		if first.IsZero() {
			first = now
		}
		last = now
		return now
	}
	// Validate the immutable media against every bound sensor's metadata. The
	// domain evaluator reports decoder failures as insufficient evidence; the
	// live wrapper must keep these terminal instead of polling a new tuple.
	var mediaFault error
	checkedLoad := func(ctx context.Context, ref sensing.EvidenceRef) ([]byte, error) {
		data, err := load(ctx, ref)
		if err == nil {
			err = validateMedia(batch.captures, ref, data)
		}
		if err != nil {
			mediaFault = ErrCaptureMedia
			return nil, ErrCaptureMedia
		}
		return data, nil
	}
	result, err := fusion.Evaluate(ctx, cfg, batch.records, checkedLoad)
	if err != nil {
		return result, err
	}
	if mediaFault != nil {
		return fusion.Result{}, mediaFault
	}
	if clockFault {
		return fusion.Result{}, errors.New("capture source clock unavailable or regressed")
	}
	for _, record := range batch.records {
		if record.Timing.SampleTime.After(first) {
			return fusion.Result{}, errors.New("capture source sample is in the future")
		}
	}
	before := cfg.Now()
	if clockFault || before.IsZero() {
		return fusion.Result{}, errors.New("source clock unavailable or regressed during authority revalidation")
	}
	guard.phase = "final_prepare"
	// Media retrieval can wait on another service. Repeat preparation on the
	// same owned proofs. Independent modes renew authority; admitted mode
	// retains its broker projection. Both repeat original capture/time checks.
	if _, err := makeBatch(ctx, owned, guard); err != nil {
		return fusion.Result{}, err
	}
	after := cfg.Now()
	// A source-clock fault is terminal even if the same interval also crosses
	// a physical capture deadline. Never let retryable staleness hide it.
	if clockFault || after.IsZero() || after.Before(before) {
		return fusion.Result{}, errors.New("source clock unavailable or regressed during authority revalidation")
	}
	guard.phase = "final_batch"
	if err := guard.check(batch.captures); err != nil {
		return fusion.Result{}, err
	}
	if finalWindow != nil {
		if err := finalWindow(guard.last); err != nil {
			return fusion.Result{}, err
		}
	}
	// Immutable original sample bounds apply even when the domain evaluator
	// returned insufficient evidence. This follows both complete proof rounds
	// and the final common physical check; it is never a cached authority lease.
	for _, record := range batch.records {
		until, _, err := sensorrecord.LiveDeadlines(record)
		if err != nil {
			return fusion.Result{}, err
		}
		if !after.Before(until) {
			return fusion.Result{}, &StaleCaptureError{reason: "sample_capture_expired"}
		}
	}
	return result, nil
}

func validateMedia(captures []sensorrecord.Capture, ref sensing.EvidenceRef, data []byte) error {
	sum := sha256.Sum256(data)
	if int64(len(data)) != ref.Size || hex.EncodeToString(sum[:]) != ref.SHA256 {
		return ErrCaptureMedia
	}
	found := false
	for _, capture := range captures {
		sample, err := capture.NormalizedSample()
		if err != nil {
			return ErrCaptureMedia
		}
		if sample.Evidence == nil || *sample.Evidence != ref {
			continue
		}
		found = true
		if sample.Camera != nil {
			if _, err := fusion.RedFractionWithType(data, *sample.Camera, ref.ContentType); err != nil {
				return ErrCaptureMedia
			}
		} else if sample.Audio != nil {
			if _, err := fusion.WAVFeatures(data, *sample.Audio); err != nil {
				return ErrCaptureMedia
			}
		} else {
			return ErrCaptureMedia
		}
	}
	if !found {
		return ErrCaptureMedia
	}
	return nil
}

// Prepare creates a bounded, immutable decoder lookup for one verified query.
// Decoder lookup requires the exact component bytes; it cannot be reused with a
// rewritten source, publication sequence, payload, timing or relayed age hint.
// Prepare checks authority and physical capture age for this call only; its
// returned decoder is not a freshness lease. Evaluate checks both clocks again
// after media retrieval. Persisted replica/epoch high-water protection and
// renewed authorization remain the caller's duties.
func Prepare(ctx context.Context, options Options, proofs [][]byte, verify VerifyFunc) ([]world.Component, fusion.CaptureDecoder, error) {
	if options.Diagnostics != nil {
		options.Diagnostics.Checks = nil
	}
	batch, err := prepare(ctx, proofs, verify, &physicalGuard{provider: options.Physical, diagnostics: options.Diagnostics, phase: "prepare", started: time.Now()})
	if err != nil {
		return nil, nil, err
	}
	return batch.records, batch.decoder, nil
}

// PrepareBatch is the explicit whole-set counterpart of Prepare. It does not
// establish a lease; callers must repeat authority and freshness checks after
// later I/O and at their mutation boundary.
func PrepareBatch(ctx context.Context, options Options, proofs [][]byte, verify VerifyBatchFunc) ([]world.Component, fusion.CaptureDecoder, error) {
	if options.Diagnostics != nil {
		options.Diagnostics.Checks = nil
	}
	batch, err := prepareBatch(ctx, proofs, verify, &physicalGuard{provider: options.Physical, diagnostics: options.Diagnostics, phase: "prepare", started: time.Now()})
	if err != nil {
		return nil, nil, err
	}
	return batch.records, batch.decoder, nil
}

func prepare(ctx context.Context, proofs [][]byte, verify VerifyFunc, guard *physicalGuard) (prepared, error) {
	if ctx == nil || verify == nil {
		return prepared{}, errors.New("bounded original proofs and current-authority verifier required")
	}
	if err := proofBounds(proofs); err != nil {
		return prepared{}, err
	}
	if err := ctx.Err(); err != nil {
		return prepared{}, err
	}
	if err := guard.check(nil); err != nil {
		return prepared{}, err
	}
	builder := newPreparedBuilder()
	for _, raw := range proofs {
		if err := ctx.Err(); err != nil {
			return prepared{}, err
		}
		raw = bytes.Clone(raw)
		signed, err := decodeProof(raw)
		if err != nil {
			return prepared{}, err
		}
		original := bytes.Clone(signed.Record)
		verified, err := verify(ctx, signed)
		if err != nil {
			return prepared{}, err
		}
		if err = builder.add(raw, original, verified); err != nil {
			return prepared{}, err
		}
	}
	return builder.finish(ctx, guard)
}

func prepareBatch(ctx context.Context, proofs [][]byte, verify VerifyBatchFunc, guard *physicalGuard) (prepared, error) {
	if ctx == nil || verify == nil || len(proofs) > MaxBatchRecords {
		return prepared{}, errors.New("bounded original proofs and whole-set verifier required")
	}
	if err := proofBounds(proofs); err != nil {
		return prepared{}, err
	}
	if err := ctx.Err(); err != nil {
		return prepared{}, err
	}
	owned := make([][]byte, len(proofs))
	original := make([][]byte, len(proofs))
	signed := make([]serviceorigin.SignedRecord, len(proofs))
	for i, raw := range proofs {
		owned[i] = bytes.Clone(raw)
		var err error
		signed[i], err = decodeProof(owned[i])
		if err != nil {
			return prepared{}, err
		}
		original[i] = bytes.Clone(signed[i].Record)
	}
	if err := guard.check(nil); err != nil {
		return prepared{}, err
	}
	verified, err := verify(ctx, signed)
	if err != nil {
		return prepared{}, err
	}
	if len(verified) != len(proofs) {
		return prepared{}, errors.New("whole-set verifier returned incomplete batch")
	}
	builder := newPreparedBuilder()
	seen := map[world.Source]bool{}
	for i, v := range verified {
		if err := ctx.Err(); err != nil {
			return prepared{}, err
		}
		if err := builder.add(owned[i], original[i], v); err != nil {
			return prepared{}, err
		}
		source := v.Component().Key.Source
		if seen[source] {
			return prepared{}, errors.New("duplicate source in capture batch")
		}
		seen[source] = true
	}
	return builder.finish(ctx, guard)
}

type savedCapture struct {
	capture sensorrecord.Capture
	proof   string
}
type preparedBuilder struct {
	byComponent map[string]savedCapture
	records     []world.Component
	captures    []sensorrecord.Capture
}

func newPreparedBuilder() *preparedBuilder {
	return &preparedBuilder{byComponent: map[string]savedCapture{}}
}
func (b *preparedBuilder) add(raw, original []byte, verified serviceorigin.VerifiedRecord) error {
	var decoded serviceorigin.Record
	if !bytes.Equal(original, verified.Bytes) || jws.DecodeStrict(original, &decoded) != nil || !reflect.DeepEqual(decoded, verified.Record) {
		return errors.New("verifier returned a different record from the original proof")
	}
	component := verified.Component()
	capture, err := sensorrecord.FromComponent(component)
	if err != nil {
		return err
	}
	key, err := componentKey(component)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(raw)
	hash := hex.EncodeToString(sum[:])
	if old, found := b.byComponent[key]; found && old.proof != hash {
		return errors.New("same component has conflicting original proof envelopes")
	}
	b.byComponent[key] = savedCapture{capture, hash}
	b.records = append(b.records, component)
	b.captures = append(b.captures, capture)
	return nil
}
func (b *preparedBuilder) finish(ctx context.Context, guard *physicalGuard) (prepared, error) {
	decoder := func(component world.Component) (fusion.CapturedSample, error) {
		raw, err := componentKey(component)
		if err != nil {
			return fusion.CapturedSample{}, err
		}
		stored, found := b.byComponent[raw]
		if !found {
			return fusion.CapturedSample{}, errors.New("component is not the exact verified query record")
		}
		sample, err := stored.capture.NormalizedSample()
		if err != nil {
			return fusion.CapturedSample{}, err
		}
		out := fusion.CapturedSample{Sample: sample, ProofSHA256: stored.proof, SceneBootID: stored.capture.SceneBootID, SimulationAnchor: time.Unix(0, stored.capture.SimulationAnchorUnixNS).UTC()}
		if stored.capture.SensorLink != nil {
			original, err := stored.capture.Sample()
			if err != nil {
				return fusion.CapturedSample{}, err
			}
			out.OriginalEvidence = original.Evidence
		}
		return out, nil
	}
	if err := guard.check(b.captures); err != nil {
		return prepared{}, err
	}
	if err := ctx.Err(); err != nil {
		return prepared{}, err
	}
	return prepared{b.records, decoder, b.captures}, nil
}

func proofBounds(proofs [][]byte) error {
	if len(proofs) == 0 || len(proofs) > MaxRecords {
		return errors.New("bounded nonempty capture proof query required")
	}
	total := 0
	for _, raw := range proofs {
		if len(raw) == 0 || len(raw) > MaxProofBytes || len(raw) > MaxBatchBytes-total {
			return errors.New("capture proof query exceeds budget")
		}
		total += len(raw)
	}
	return nil
}

func componentKey(component world.Component) (string, error) {
	// Marshal compacts json.RawMessage. Append the original payload as well so
	// lookup cannot accept a whitespace/encoding rewrite of signed bytes.
	raw, err := json.Marshal(component)
	if err != nil {
		return "", err
	}
	raw = append(append(raw, 0), component.Payload...)
	return string(raw), nil
}

// The envelope contains only two object levels and base64 byte fields. Checking
// each object's exact field names before typed decoding rejects duplicate keys,
// case aliases, missing/null values and trailing JSON without narrowing the
// agent's 256 KiB envelope budget to the smaller signed-record JSON budget.
func object(raw []byte, names ...string) (map[string]json.RawMessage, error) {
	d := json.NewDecoder(bytes.NewReader(raw))
	first, err := d.Token()
	if err != nil || first != json.Delim('{') {
		return nil, errors.New("proof object required")
	}
	allowed := map[string]bool{}
	for _, name := range names {
		allowed[name] = true
	}
	fields := map[string]json.RawMessage{}
	for d.More() {
		key, err := d.Token()
		name, ok := key.(string)
		if err != nil || !ok || !allowed[name] || fields[name] != nil {
			return nil, errors.New("unknown or duplicate proof field")
		}
		var value json.RawMessage
		if err := d.Decode(&value); err != nil || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return nil, errors.New("invalid or null proof field")
		}
		fields[name] = value
	}
	last, err := d.Token()
	if err != nil || last != json.Delim('}') || len(fields) != len(names) {
		return nil, errors.New("missing proof field")
	}
	if _, err := d.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("trailing proof JSON")
	}
	return fields, nil
}

func decodeProof(raw []byte) (serviceorigin.SignedRecord, error) {
	var signed serviceorigin.SignedRecord
	fields, err := object(raw, "delegation", "record", "signature")
	if err != nil {
		return signed, err
	}
	delegation, err := object(fields["delegation"], "claims", "chain_der", "signature")
	if err != nil {
		return signed, err
	}
	for _, value := range []json.RawMessage{fields["record"], fields["signature"], delegation["claims"], delegation["signature"]} {
		if len(bytes.TrimSpace(value)) == 0 || bytes.TrimSpace(value)[0] != '"' {
			return signed, errors.New("proof bytes require base64 strings")
		}
	}
	var chain []string
	if err := json.Unmarshal(delegation["chain_der"], &chain); err != nil || len(chain) == 0 {
		return signed, errors.New("proof requires a base64 certificate chain")
	}
	if err := json.Unmarshal(raw, &signed); err != nil {
		return signed, err
	}
	return signed, nil
}
