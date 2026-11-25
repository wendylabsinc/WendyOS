package livepeople

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/pose"
	"github.com/wendylabsinc/wendy/go/world/internal/tracking"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
	appv1 "github.com/wendylabsinc/wendy/go/world/production/api/world/app/v1"
	"github.com/wendylabsinc/wendy/go/world/production/appclock"
	"github.com/wendylabsinc/wendy/go/world/production/proofref"
	"github.com/wendylabsinc/wendy/go/world/production/resultrecord"
	"github.com/wendylabsinc/wendy/go/world/production/sensorrecord"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
	"github.com/wendylabsinc/wendy/go/world/production/trackcapture"
	"github.com/wendylabsinc/wendy/go/world/production/wendycert"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// InferFunc receives only owned image bytes. Calibration, source identities,
// simulator objects and positions are not inputs to image inference.
type InferFunc func(context.Context, []byte) (pose.ImageResult, error)
type ProofReceipt struct {
	Reference    proofref.Reference `json:"reference"`
	EnvelopeJSON []byte             `json:"envelope_json"`
}
type Publication struct {
	TrackID          string `json:"track_id"`
	RequestJSON      []byte `json:"request_json"`
	SignedRecordJSON []byte `json:"signed_record_json,omitempty"`
	LocallyVerified  bool   `json:"locally_verified"`
	RPCCode          string `json:"rpc_code,omitempty"`
}
type Result struct {
	Version        int              `json:"version"`
	RunID          string           `json:"run_id"`
	Tick           int              `json:"tick"`
	Status         string           `json:"status"`
	SampleTime     time.Time        `json:"sample_time"`
	Tracks         []tracking.Track `json:"tracks"`
	Inputs         []ProofReceipt   `json:"inputs,omitempty"`
	Publications   []Publication    `json:"publications,omitempty"`
	InputAuthority string           `json:"input_authority,omitempty"`
}
type Summary struct {
	Ticks               int    `json:"ticks"`
	PublicationAttempts int    `json:"publication_attempts"`
	Publications        int    `json:"publications"` // Locally verified acknowledgments.
	UnknownOutcomes     int    `json:"unknown_outcomes,omitempty"`
	Status              string `json:"status"`
}
type cachedPose struct {
	envelope string
	result   pose.ImageResult
}
type Session struct {
	mu           sync.Mutex
	cfg          Config
	api          appv1.WorldAppServiceClient
	clock        *appclock.Provider
	verifier     *serviceorigin.BatchVerifier
	certificates *wendycert.DeviceVerifier
	tracker      *trackcapture.Session
	infer        InferFunc
	sources      map[string]Source
	bindings     map[string]binding
	cache        map[string]cachedPose
	ticks        int
	fault        error
	native       bool
}

func New(config Config, api appv1.WorldAppServiceClient, infer InferFunc) (*Session, error) {
	c, err := config.normalized()
	if err != nil || api == nil || infer == nil {
		return nil, ErrConfig
	}
	clock, err := appclock.New(api, appclock.Config{Tenant: c.Tenant, Site: c.Site, Device: c.Device, App: c.App, Service: c.Service, ExpectedGeneration: c.ExpectedGeneration, ExecutionDomain: "simulation", SampleClockDomain: "simulation:" + c.RunID, MaxRoundTrip: min(time.Duration(c.RequestTimeoutMS)*time.Millisecond, appv1.MaxClockResponseAge), MaxWidth: time.Duration(c.ClockMaxWidthMS) * time.Millisecond, DriftPPM: 100})
	if err != nil {
		return nil, err
	}
	s := &Session{cfg: c, api: api, clock: clock, infer: infer, sources: map[string]Source{}, bindings: map[string]binding{}, cache: map[string]cachedPose{}}
	s.certificates, err = wendycert.NewDevice(wendycert.Config{Tenant: c.Tenant, Roots: c.RootsDER})
	if err != nil {
		clock.Close()
		return nil, err
	}
	s.verifier, err = serviceorigin.NewBatchVerifier(serviceorigin.BatchVerifierConfig{Tenant: c.Tenant, Site: c.Site, ExecutionDomain: "simulation", Certificates: s.certificates, Clock: clock, Authorize: s.authorizeBatch, MaxRecords: 16, MaxBytes: 4 << 20})
	if err != nil {
		clock.Close()
		return nil, err
	}
	sources := map[string]world.Source{}
	for _, source := range c.Sources {
		s.sources[source.SensorID] = source
		sources[source.SensorID] = world.Source{Device: source.Device, App: source.App, Service: source.Service, Stream: source.SensorID}
	}
	var id [8]byte
	if _, err = rand.Read(id[:]); err != nil {
		clock.Close()
		return nil, err
	}
	s.tracker, err = trackcapture.New(trackcapture.Config{Tenant: c.Tenant, Site: c.Site, Tracking: tracking.Config{TrackerID: c.Service + "-" + hex.EncodeToString(id[:]), RunID: c.RunID, ClockDomain: "simulation:" + c.RunID, Frame: c.Frame, MaxMiss: 1250 * time.Millisecond, MaxTracks: 48, MaxDetections: 48, MaxSources: len(c.Sources)}, Projection: pose.Config{FloorY: c.FloorY, AnkleHeightM: c.AnkleHeightM, FloorUncertaintyM: .08, PixelUncertainty: 2}, Sources: sources, ModelID: c.ModelID, ModelVersion: c.ModelVersion, ModelSHA256: c.ModelSHA256})
	if err != nil {
		clock.Close()
		return nil, err
	}
	return s, nil
}

func (s *Session) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fault = ErrClosed
	return s.clock.Close()
}
func copyPose(p pose.ImageResult) pose.ImageResult {
	p.Detections = append([]pose.Person(nil), p.Detections...)
	return p
}

// Step performs one current observation cycle. Only novel observed groups are
// published. Duplicate reads and local coasting never renew a published track.
func (s *Session) Step(ctx context.Context) (out Result, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out = Result{Version: 1, RunID: s.cfg.RunID, Tick: s.ticks + 1, Status: "failed", Tracks: []tracking.Track{}}
	if ctx == nil || s.fault != nil || s.ticks >= s.cfg.MaxTicks {
		return out, ErrClosed
	}
	defer func() {
		if err != nil {
			if !continueUnknown(ctx, out, err) {
				s.fault = err
			}
			if errors.Is(err, ErrUncertain) {
				out.Status = "unknown_outcome"
			}
		}
	}()
	s.ticks++
	initial, err := s.clock.Observe(ctx)
	if err != nil {
		return out, err
	}
	current, err := s.query(ctx)
	if err != nil {
		return out, err
	}
	if s.native {
		var release func()
		ctx, release = s.admittedContext(ctx, current)
		defer release()
		out.InputAuthority = "native_agent_admission"
	}
	if len(current) > 0 {
		if _, err = s.prepareCurrent(ctx, current); err != nil {
			if !errors.Is(err, errExpiredCamera) {
				return out, err
			}
			current, err = nil, nil
		}
		usable := current[:0]
		for _, p := range current {
			if personCurrent(p, initial.SampleTime) {
				usable = append(usable, p)
			}
		}
		current = usable
	}
	if len(current) == 0 {
		last, e := s.clock.Observe(ctx)
		if e != nil {
			return out, e
		}
		snapshot, e := s.tracker.Step(last.SampleTime, last.Physical, nil)
		if e != nil {
			return out, e
		}
		out.SampleTime, out.Tracks, out.Status = last.SampleTime, snapshot.Tracks, "waiting"
		return out, nil
	}
	inputs := make([]trackcapture.Input, 0, 16)
	proofs := append([]proof(nil), current...)
	retentions := map[string]time.Time{}
	seen := map[string]bool{}
	var mediaBytes int
	for _, p := range current {
		image, until, e := s.image(ctx, p)
		if e != nil {
			return out, e
		}
		mediaBytes += len(image)
		if mediaBytes > 16<<20 {
			return out, ErrMedia
		}
		id := p.record.Publication.Stream
		cached, ok := s.cache[id]
		var poses pose.ImageResult
		if ok && cached.envelope == p.reference.EnvelopeSHA256 {
			poses = copyPose(cached.result)
		} else {
			poses, e = s.infer(ctx, bytes.Clone(image))
			if e != nil {
				return out, e
			}
			if poses.ModelID != s.cfg.ModelID || poses.ModelVersion != s.cfg.ModelVersion || poses.ModelSHA256 != s.cfg.ModelSHA256 {
				return out, ErrSource
			}
			s.cache[id] = cachedPose{p.reference.EnvelopeSHA256, copyPose(poses)}
		}
		inputs = append(inputs, trackcapture.Input{Envelope: bytes.Clone(p.raw), Image: image, Poses: poses})
		retentions[p.reference.EnvelopeSHA256] = until
		seen[p.reference.EnvelopeSHA256] = true
	}
	// Prior original measurements support finite-difference motion only. They
	// receive fresh proof/current-source checks, never a reusable authority lease.
	available := map[string]bool{}
	for _, p := range current {
		available[p.record.Publication.Stream] = true
	}
	for _, old := range s.tracker.Support(initial.SampleTime) {
		if len(inputs) == resultrecord.MaxInputs {
			break
		}
		p, e := s.parse(old.Envelope)
		if e != nil {
			return out, e
		}
		if seen[p.reference.EnvelopeSHA256] || !available[p.record.Publication.Stream] {
			continue
		}
		if s.native && p.record.Scope.Device != s.cfg.Device && !containsOriginal(current, p) {
			// Remote historical versions are not a current peer snapshot.
			// Local retained originals keep their existing motion semantics.
			continue
		}
		sampleUntil, physicalUntil, e := sensorrecord.LiveDeadlines(old.Verified.Component())
		if e != nil {
			return out, e
		}
		if !initial.SampleTime.Before(sampleUntil) || !initial.Physical.Latest.Before(physicalUntil) || initial.Physical.Latest.UnixMilli() >= p.record.ExpiresAtUnixMS {
			continue
		}
		image, until, e := s.image(ctx, p)
		if e != nil {
			return out, e
		}
		mediaBytes += len(image)
		if mediaBytes > 16<<20 {
			return out, ErrMedia
		}
		old.Envelope, old.Image, old.SupportOnly = bytes.Clone(p.raw), image, true
		inputs = append(inputs, old)
		proofs = append(proofs, p)
		retentions[p.reference.EnvelopeSHA256] = until
		seen[p.reference.EnvelopeSHA256] = true
	}
	// Inference and media reads can consume a previous observation's remaining
	// lifetime. Omit only support already proven expired by this fresh reading;
	// failures in current-source verification remain terminal.
	beforeVerify, err := s.clock.Observe(ctx)
	if err != nil {
		return out, err
	}
	keptInputs := inputs[:0]
	keptProofs := proofs[:0]
	for i, input := range inputs {
		p := proofs[i]
		if !personCurrent(p, beforeVerify.SampleTime) {
			delete(retentions, p.reference.EnvelopeSHA256)
			continue
		}
		if input.SupportOnly {
			sampleUntil, physicalUntil, e := sensorrecord.LiveDeadlines(input.Verified.Component())
			if e != nil {
				return out, e
			}
			if !beforeVerify.SampleTime.Before(sampleUntil) || !beforeVerify.Physical.Latest.Before(physicalUntil) || beforeVerify.Physical.Latest.UnixMilli() >= p.record.ExpiresAtUnixMS || !beforeVerify.Physical.Latest.Before(retentions[p.reference.EnvelopeSHA256]) {
				delete(retentions, p.reference.EnvelopeSHA256)
				continue
			}
		}
		keptInputs, keptProofs = append(keptInputs, input), append(keptProofs, p)
	}
	inputs, proofs = keptInputs, keptProofs
	if !hasCurrent(inputs) {
		snapshot, e := s.tracker.Step(beforeVerify.SampleTime, beforeVerify.Physical, nil)
		if e != nil {
			return out, e
		}
		out.SampleTime, out.Tracks, out.Status = beforeVerify.SampleTime, snapshot.Tracks, "waiting"
		return out, nil
	}
	verified, err := s.verify(ctx, proofs)
	if err != nil {
		return out, err
	}
	for i := range inputs {
		inputs[i].Verified = verified[i]
	}
	final, err := s.clock.Observe(ctx)
	if err != nil {
		return out, err
	}
	keptInputs, keptProofs = inputs[:0], proofs[:0]
	for i, input := range inputs {
		p := proofs[i]
		if !personCurrent(p, final.SampleTime) {
			delete(retentions, p.reference.EnvelopeSHA256)
			continue
		}
		keptInputs, keptProofs = append(keptInputs, input), append(keptProofs, p)
	}
	inputs, proofs = keptInputs, keptProofs
	if !hasCurrent(inputs) {
		snapshot, e := s.tracker.Step(final.SampleTime, final.Physical, nil)
		if e != nil {
			return out, e
		}
		out.SampleTime, out.Tracks, out.Status = final.SampleTime, snapshot.Tracks, "waiting"
		return out, nil
	}
	for i := range inputs {
		out.Inputs = append(out.Inputs, ProofReceipt{proofs[i].reference, bytes.Clone(proofs[i].raw)})
	}
	for _, until := range retentions {
		if !final.Physical.Latest.Before(until) {
			return out, ErrMedia
		}
	}
	if err = ctx.Err(); err != nil {
		return out, err
	}
	snapshot, err := s.tracker.Step(final.SampleTime, final.Physical, inputs)
	if err != nil {
		return out, err
	}
	out.SampleTime, out.Tracks = final.SampleTime, snapshot.Tracks
	for _, track := range snapshot.Tracks {
		if track.State != "observed" {
			continue
		}
		request, validated, e := snapshot.BuildPublication(track.ID, s.cfg.ResultStream, final.Physical.Earliest.Add(time.Duration(s.cfg.RetainForMS)*time.Millisecond))
		if e != nil {
			return out, e
		}
		// Earlier track publications can block. Recheck the exact original
		// support set and both clocks before each subsequent mutation too.
		if _, e = s.verify(ctx, proofs); e != nil {
			return out, e
		}
		now, e := s.clock.Observe(ctx)
		if e != nil {
			return out, e
		}
		v, e := resultrecord.ValidatePublication(request, validated)
		if e != nil {
			return out, ErrSource
		}
		if !now.SampleTime.Before(v.SampleValidUntil) || !now.Physical.Latest.Before(v.PhysicalValidUntil) {
			// No mutation for this track has been attempted. Its original
			// observation expired during validation, so acquire a fresh group
			// on the next tick instead of faulting the whole session.
			if e := ctx.Err(); e != nil {
				return out, e
			}
			out.SampleTime, out.Tracks, out.Status = now.SampleTime, []tracking.Track{}, "waiting"
			return out, nil
		}
		for _, until := range retentions {
			if !now.Physical.Latest.Before(until) {
				return out, ErrMedia
			}
		}
		var mediaUntil time.Time
		for _, until := range retentions {
			if mediaUntil.IsZero() || until.Before(mediaUntil) {
				mediaUntil = until
			}
		}
		publication := Publication{TrackID: track.ID}
		e = s.publish(ctx, request, validated, mediaUntil, &publication)
		out.Publications = append(out.Publications, publication)
		if e != nil {
			return out, e
		}
	}
	out.Status = "complete"
	return out, nil
}

// The camera's raw publication can outlive its usefulness for person tracking.
// This age check never extends raw TTL or classifies clock/authority errors.
func personCurrent(p proof, now time.Time) bool {
	return now.Before(p.record.Publication.Timing.SampleTime.Add(trackcapture.DefaultLifetime))
}
func hasCurrent(inputs []trackcapture.Input) bool {
	for _, input := range inputs {
		if !input.SupportOnly {
			return true
		}
	}
	return false
}

func (s *Session) publish(ctx context.Context, request serviceorigin.PublishRequest, inputs []resultrecord.ValidatedInput, mediaUntil time.Time, out *Publication) error {
	wire, err := appv1.EncodePublish(request)
	if err != nil {
		return err
	}
	out.RequestJSON, err = protojson.MarshalOptions{UseProtoNames: true}.Marshal(wire)
	if err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	rpc, cancel := context.WithTimeout(ctx, time.Duration(s.cfg.RequestTimeoutMS)*time.Millisecond)
	ack, err := s.api.Publish(rpc, wire, grpc.MaxCallRecvMsgSize(256<<10), grpc.MaxRetryRPCBufferSize(0))
	out.RPCCode = status.Code(err).String()
	cancel()
	if ack != nil && len(ack.SignedRecordJson) <= 256<<10 {
		out.SignedRecordJSON = bytes.Clone(ack.SignedRecordJson)
	}
	if err != nil || ack == nil || !ack.Accepted || len(ack.ProtoReflect().GetUnknown()) != 0 {
		return ErrUncertain
	}
	signed, record, err := decodeEnvelope(ack.SignedRecordJson)
	if err != nil {
		return ErrUncertain
	}
	encoded, err := appv1.EncodePublish(record.Publication)
	if err != nil || !proto.Equal(encoded, wire) {
		return ErrUncertain
	}
	v, err := serviceorigin.NewVerifier(serviceorigin.VerifierConfig{Tenant: s.cfg.Tenant, Site: s.cfg.Site, ExecutionDomain: "simulation", Certificates: s.certificates, Clock: s.clock, Authorize: func(ctx context.Context, c serviceorigin.DelegationClaims, d wendycert.DeviceIdentity) error {
		now, e := s.clock.Observe(ctx)
		if e != nil || d.Principal != s.cfg.Device || c.Scope.Device != s.cfg.Device || c.Scope.App != s.cfg.App || c.Scope.Service != s.cfg.Service || c.Scope.DeploymentGeneration != now.DeploymentGeneration || len(c.Scope.Publish) != 1 || c.Scope.Publish[0] != (serviceorigin.PublishScope{ComponentType: resultrecord.ComponentType, Stream: s.cfg.ResultStream}) {
			return ErrSource
		}
		return nil
	}})
	if err != nil {
		return ErrUncertain
	}
	checked, err := v.Verify(ctx, signed)
	if err != nil || !bytes.Equal(checked.Bytes, signed.Record) {
		return ErrUncertain
	}
	validation, err := resultrecord.ValidatePublication(record.Publication, inputs)
	if err != nil {
		return ErrUncertain
	}
	now, err := s.clock.Observe(ctx)
	if err != nil || !now.SampleTime.Before(validation.SampleValidUntil) || !now.Physical.Latest.Before(validation.PhysicalValidUntil) || !now.Physical.Latest.Before(mediaUntil) {
		return ErrUncertain
	}
	if ctx.Err() != nil {
		return ErrUncertain
	}
	out.LocallyVerified = true
	return nil
}

// The tracker committed the original source versions and detection members
// before Publish. Those same observations can only coast on a later Step;
// only a novel observation group can produce another publication. Keep the
// unknown receipt, and continue sensing only after these transport failures.
func continueUnknown(ctx context.Context, out Result, err error) bool {
	if ctx == nil || ctx.Err() != nil || !errors.Is(err, ErrUncertain) || len(out.Publications) == 0 {
		return false
	}
	p := out.Publications[len(out.Publications)-1]
	return !p.LocallyVerified && (p.RPCCode == codes.Unavailable.String() || p.RPCCode == codes.DeadlineExceeded.String())
}

// Run emits each bounded tick receipt rather than retaining an unbounded video
// or proof history. The caller owns the client connection and output writer.
func Run(ctx context.Context, c Config, api appv1.WorldAppServiceClient, infer InferFunc, emit func(Result) error) (out Summary, err error) {
	return run(ctx, c, api, infer, emit, New)
}

func run(ctx context.Context, c Config, api appv1.WorldAppServiceClient, infer InferFunc, emit func(Result) error, create func(Config, appv1.WorldAppServiceClient, InferFunc) (*Session, error)) (out Summary, err error) {
	if ctx == nil || emit == nil {
		return out, ErrConfig
	}
	s, err := create(c, api, infer)
	if err != nil {
		return out, err
	}
	defer s.Close()
	ctx, cancel := context.WithTimeout(ctx, time.Duration(s.cfg.DurationMS)*time.Millisecond)
	defer cancel()
	out.Status = "failed"
	for i := 0; i < s.cfg.MaxTicks; i++ {
		r, e := s.Step(ctx)
		out.Ticks++
		out.PublicationAttempts += len(r.Publications)
		for _, publication := range r.Publications {
			if publication.LocallyVerified {
				out.Publications++
			}
		}
		if errors.Is(e, ErrUncertain) {
			out.UnknownOutcomes++
		}
		if emitErr := emit(r); emitErr != nil {
			return out, emitErr
		}
		if e != nil && !continueUnknown(ctx, r, e) {
			if errors.Is(e, ErrUncertain) {
				out.Status = "unknown_outcome"
			}
			return out, e
		}
		if i+1 == s.cfg.MaxTicks {
			break
		}
		t := time.NewTimer(time.Duration(s.cfg.PollIntervalMS) * time.Millisecond)
		select {
		case <-ctx.Done():
			t.Stop()
			return out, ctx.Err()
		case <-t.C:
		}
	}
	out.Status = "complete"
	if out.UnknownOutcomes > 0 {
		out.Status = "complete_with_unknown_outcomes"
	}
	return out, nil
}
