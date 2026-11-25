// Package derivedreader shares the ordinary result read loop between the
// person and hazard apps. Domain decoders do not perform I/O or grant authority.
package derivedreader

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
	appv1 "github.com/wendylabsinc/wendy/go/world/production/api/world/app/v1"
	"github.com/wendylabsinc/wendy/go/world/production/appclock"
	"github.com/wendylabsinc/wendy/go/world/production/jws"
	"github.com/wendylabsinc/wendy/go/world/production/resultrecord"
	"github.com/wendylabsinc/wendy/go/world/production/serviceaccess"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
	"github.com/wendylabsinc/wendy/go/world/production/trustedtime"
	"github.com/wendylabsinc/wendy/go/world/production/wendycert"
	"golang.org/x/sys/unix"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var ErrRefused = errors.New("current derived result read refused")
var ErrUnavailable = errors.New("current derived result temporarily unavailable")

type Source struct {
	Device  string `json:"device"`
	App     string `json:"app"`
	Service string `json:"service"`
	Stream  string `json:"stream"`
}

func (s Source) selector() serviceaccess.Source {
	return serviceaccess.Source{Device: s.Device, App: s.App, Service: s.Service, Stream: s.Stream, ComponentType: resultrecord.ComponentType}
}

type Config struct {
	Version            int            `json:"version"`
	Tenant             string         `json:"tenant"`
	Site               string         `json:"site"`
	Device             string         `json:"device"`
	App                string         `json:"app"`
	Service            string         `json:"service"`
	ExpectedGeneration uint64         `json:"expected_generation,omitempty"`
	RootsDER           [][]byte       `json:"roots_der"`
	RunID              string         `json:"run_id"`
	Frame              world.FrameRef `json:"frame"`
	Source             Source         `json:"source"`
	DurationMS         int64          `json:"duration_ms"`
	MaxTicks           int            `json:"max_ticks,omitempty"`
	PollIntervalMS     int64          `json:"poll_interval_ms,omitempty"`
	RequestTimeoutMS   int64          `json:"request_timeout_ms,omitempty"`
	ClockMaxWidthMS    int64          `json:"clock_max_width_ms,omitempty"`
	// MissionFloorY opts into a fixed-location handoff from an already verified
	// original. It neither proposes nor signs a mission.
	MissionFloorY *float64 `json:"mission_floor_y,omitempty"`
}

func (c Config) Normalized() (Config, error) {
	if c.MissionFloorY != nil {
		floor := *c.MissionFloorY
		if math.IsNaN(floor) || math.IsInf(floor, 0) || math.Abs(floor) > 10000 {
			return c, ErrRefused
		}
		c.MissionFloorY = &floor
	}
	if c.MaxTicks == 0 {
		c.MaxTicks = 120
	}
	if c.PollIntervalMS == 0 {
		c.PollIntervalMS = 250
	}
	if c.RequestTimeoutMS == 0 {
		c.RequestTimeoutMS = 1000
	}
	if c.ClockMaxWidthMS == 0 {
		c.ClockMaxWidthMS = 5000
	}
	scope := serviceorigin.Scope{Tenant: c.Tenant, Site: c.Site, Device: c.Device, App: c.App, Service: c.Service, DeploymentGeneration: max(1, c.ExpectedGeneration), ExecutionDomain: "simulation"}
	resolved, e := c.Source.selector().Resolve(c.Device)
	if e != nil || c.Version != 1 || scope.Validate() != nil || c.Frame.Validate() != nil || c.RunID == "" || len(c.RunID) > 80 || len(c.RootsDER) < 1 || len(c.RootsDER) > 8 || c.DurationMS < 1 || c.DurationMS > 120000 || c.MaxTicks < 1 || c.MaxTicks > 480 || c.PollIntervalMS < 50 || c.PollIntervalMS > 10000 || c.RequestTimeoutMS < 10 || c.RequestTimeoutMS > 5000 || c.ClockMaxWidthMS < 1 || c.ClockMaxWidthMS > 10000 {
		return c, ErrRefused
	}
	c.Source.Device = resolved.Device
	total := 0
	roots := make([][]byte, len(c.RootsDER))
	for i, r := range c.RootsDER {
		total += len(r)
		if len(r) == 0 || total > 64<<10 {
			return c, ErrRefused
		}
		roots[i] = bytes.Clone(r)
	}
	c.RootsDER = roots
	return c, nil
}
func LoadConfig(path string) (Config, error) {
	var c Config
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return c, ErrRefused
	}
	fd, e := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if e != nil {
		return c, ErrRefused
	}
	f := os.NewFile(uintptr(fd), "people-reader-config")
	defer f.Close()
	st, e := f.Stat()
	if e != nil || !st.Mode().IsRegular() || st.Mode().Perm()&0022 != 0 || st.Size() < 1 || st.Size() > 128<<10 {
		return c, ErrRefused
	}
	raw, e := io.ReadAll(io.LimitReader(f, 128<<10+1))
	if e != nil || len(raw) > 128<<10 || jws.DecodeStrict(raw, &c) != nil {
		return c, ErrRefused
	}
	return c.Normalized()
}

type Item[T any] struct {
	EnvelopeJSON   []byte        `json:"envelope_json"`
	EnvelopeSHA256 string        `json:"envelope_sha256"`
	RecordSHA256   string        `json:"record_sha256"`
	Version        world.Version `json:"version"`
	Payload        T             `json:"payload"`
	MissionInput   *MissionInput `json:"mission_input,omitempty"`
}
type Result[T any] struct {
	Version                 int       `json:"version"`
	Tick                    int       `json:"tick"`
	RunID                   string    `json:"run_id"`
	SampleTime              time.Time `json:"sample_time"`
	ReaderGeneration        uint64    `json:"reader_generation"`
	Status                  string    `json:"status"`
	RPCCode                 string    `json:"rpc_code,omitempty"`
	StartupAttempt          int       `json:"startup_attempt,omitempty"`
	Clock                   *Clock    `json:"clock,omitempty"`
	Items                   []Item[T] `json:"items"`
	AbsentIDs               []string  `json:"absent_ids,omitempty"`
	OriginAttestedInference bool      `json:"origin_attested_inference"`
}
type Summary struct {
	Ticks        int    `json:"ticks"`
	ItemsRead    int    `json:"items_read"`
	StartupWaits int    `json:"startup_waits"`
	Status       string `json:"status"`
}
type parsed[T any] struct {
	raw     []byte
	signed  serviceorigin.SignedRecord
	record  serviceorigin.Record
	payload Interpretation[T]
	retired bool
}

type readPreparationKey struct{}
type readPreparation struct {
	empty   *serviceorigin.BatchAuthorization
	failure error
}
type highwater struct {
	version       world.Version
	hash          string
	sampleUntil   time.Time
	physicalUntil time.Time
	verified      bool
}
type Session[T any] struct {
	decode          Decoder[T]
	mu              sync.Mutex
	cfg             Config
	api             appv1.WorldAppServiceClient
	clock           *appclock.Provider
	verifier        *serviceorigin.BatchVerifier
	scope           *serviceorigin.Scope
	epoch           uint64
	delegation      string
	water           map[string]highwater
	last            map[string]bool
	ticks           int
	fault           bool
	querySeen       bool
	startupBegan    time.Time
	startupAttempts int
	continuous      bool
	retired         world.Version
	operation       *readOperation
	verifierConfig  serviceorigin.BatchVerifierConfig
}

const startupBudget = 10 * time.Second
const startupSpacing = 200 * time.Millisecond
const startupMaxAttempts = 32

type startupFailure struct {
	code  codes.Code
	retry bool
}

func (e *startupFailure) Error() string { return "initial source Query not ready" }

func hash(raw []byte) string { d := sha256.Sum256(raw); return hex.EncodeToString(d[:]) }
func New[T any](config Config, api appv1.WorldAppServiceClient, decode Decoder[T]) (*Session[T], error) {
	return newSession(config, api, decode, false)
}

// NewContinuous keeps one pinned session across caller-owned requests. The
// existing duration/tick settings still validate, but do not stop this session.
func NewContinuous[T any](config Config, api appv1.WorldAppServiceClient, decode Decoder[T]) (*Session[T], error) {
	return newSession(config, api, decode, true)
}

func newSession[T any](config Config, api appv1.WorldAppServiceClient, decode Decoder[T], continuous bool) (*Session[T], error) {
	c, e := config.Normalized()
	if e != nil || api == nil || decode == nil {
		return nil, ErrRefused
	}
	var clockClient appclock.ClockClient = api
	if continuous {
		clockClient = operationClockClient{api: api}
	}
	clock, e := appclock.New(clockClient, appclock.Config{Tenant: c.Tenant, Site: c.Site, Device: c.Device, App: c.App, Service: c.Service, ExpectedGeneration: c.ExpectedGeneration, ExecutionDomain: "simulation", SampleClockDomain: "simulation:" + c.RunID, MaxRoundTrip: min(time.Duration(c.RequestTimeoutMS)*time.Millisecond, appv1.MaxClockResponseAge), MaxWidth: time.Duration(c.ClockMaxWidthMS) * time.Millisecond, DriftPPM: 100})
	if e != nil {
		return nil, e
	}
	certs, e := wendycert.NewDevice(wendycert.Config{Tenant: c.Tenant, Roots: c.RootsDER})
	if e != nil {
		clock.Close()
		return nil, e
	}
	s := &Session[T]{decode: decode, cfg: c, api: api, clock: clock, water: map[string]highwater{}, last: map[string]bool{}, continuous: continuous}
	var verificationClock trustedtime.Provider = clock
	if continuous {
		verificationClock = &operationClock[T]{session: s}
	}
	s.verifierConfig = serviceorigin.BatchVerifierConfig{Tenant: c.Tenant, Site: c.Site, ExecutionDomain: "simulation", Certificates: certs, Clock: verificationClock, Authorize: s.authorize, MaxRecords: serviceaccess.MaxQueryRecords, MaxBytes: 4 << 20}
	s.verifier, e = serviceorigin.NewBatchVerifier(s.verifierConfig)
	if e != nil {
		clock.Close()
		return nil, e
	}
	return s, nil
}
func (s *Session[T]) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fault = true
	return s.clock.Close()
}
func (s *Session[T]) parse(raw []byte) (parsed[T], error) {
	var p parsed[T]
	if len(raw) == 0 || len(raw) > 256<<10 || authority.DecodeStrictLimit(raw, &p.signed, 256<<10) != nil || jws.DecodeStrict(p.signed.Record, &p.record) != nil {
		return p, ErrRefused
	}
	r := p.record
	source := s.cfg.Source
	if r.Scope.Tenant != s.cfg.Tenant || r.Scope.Site != s.cfg.Site || r.Scope.Device != source.Device || r.Scope.App != source.App || r.Scope.Service != source.Service || r.Scope.ExecutionDomain != "simulation" || len(r.Scope.Publish) != 1 || r.Scope.Publish[0] != (serviceorigin.PublishScope{ComponentType: resultrecord.ComponentType, Stream: source.Stream}) || r.Publication.Stream != source.Stream || r.DelegationSHA256 != hash(p.signed.Delegation.Claims) {
		return p, ErrRefused
	}
	var e error
	p.payload, e = s.decode(r.Publication)
	if e != nil || p.payload.RunID != s.cfg.RunID || p.payload.Frame != s.cfg.Frame {
		return p, ErrRefused
	}
	p.raw = bytes.Clone(raw)
	return p, nil
}
func (s *Session[T]) query(ctx context.Context) ([]parsed[T], error) {
	source := s.cfg.Source
	timeout := time.Duration(s.cfg.RequestTimeoutMS) * time.Millisecond
	if !s.continuous && !s.querySeen {
		remaining := startupBudget - time.Since(s.startupBegan)
		if remaining <= 0 {
			return nil, &startupFailure{code: codes.DeadlineExceeded}
		}
		timeout = min(timeout, remaining)
		s.startupAttempts++
	}
	rpc, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	reply, e := s.api.Query(rpc, &appv1.QueryRequest{Sources: []*appv1.SourceSelector{{Device: source.Device, App: source.App, Service: source.Service, ComponentType: resultrecord.ComponentType, Stream: source.Stream}}, Limit: serviceaccess.MaxQueryRecords}, grpc.MaxCallRecvMsgSize(serviceaccess.MaxQueryBytes+1024), grpc.MaxRetryRPCBufferSize(0))
	if s.continuous && e != nil {
		if !s.querySeen && status.Code(e) == codes.PermissionDenied {
			return nil, errors.Join(ErrUnavailable, e)
		}
		return nil, e
	}
	if e != nil && !s.querySeen && ctx.Err() == nil {
		code := status.Code(e)
		if code == codes.PermissionDenied || code == codes.Unavailable || code == codes.DeadlineExceeded {
			return nil, &startupFailure{code: code, retry: s.startupAttempts < startupMaxAttempts && time.Since(s.startupBegan) < startupBudget}
		}
	}
	if e != nil || reply == nil || reply.Next != "" || len(reply.ProtoReflect().GetUnknown()) != 0 || len(reply.SignedRecordJson) > serviceaccess.MaxQueryRecords {
		return nil, ErrRefused
	}
	out := []parsed[T]{}
	seen := map[string]bool{}
	total := 0
	for _, raw := range reply.SignedRecordJson {
		total += len(raw)
		if total > serviceaccess.MaxQueryBytes {
			return nil, ErrRefused
		}
		p, e := s.parse(raw)
		if e != nil {
			return nil, e
		}
		if seen[p.record.Publication.Entity] {
			return nil, ErrRefused
		}
		seen[p.record.Publication.Entity] = true
		if s.scope != nil && (!reflect.DeepEqual(*s.scope, p.record.Scope) || s.epoch != p.record.Epoch || s.delegation != p.record.DelegationSHA256) {
			return nil, ErrRefused
		}
		if s.scope == nil {
			scope := p.record.Scope
			s.scope = &scope
			s.epoch = p.record.Epoch
			s.delegation = p.record.DelegationSHA256
		}
		v := world.Version{Epoch: p.record.Epoch, Sequence: p.record.Sequence}
		h := hash(raw)
		old, ok := s.water[p.record.Publication.Entity]
		if ok && (v.Compare(old.version) < 0 || v == old.version && h != old.hash) {
			return nil, ErrRefused
		}
		if s.continuous && !ok && v.Compare(s.retired) <= 0 {
			p.retired = true
			out = append(out, p)
			continue
		}
		if !ok && len(s.water) >= 4096 {
			return nil, ErrRefused
		}
		if s.continuous {
			s.stageHistory(p.record.Publication.Entity, old, ok)
		}
		mark := highwater{version: v, hash: h}
		if s.continuous {
			mark.sampleUntil, mark.physicalUntil = old.sampleUntil, old.physicalUntil
			mark.verified = old.verified && old.version == v && old.hash == h
		}
		s.water[p.record.Publication.Entity] = mark
		out = append(out, p)
	}
	s.querySeen = true
	return out, nil
}
func (s *Session[T]) authorize(ctx context.Context, b serviceorigin.BatchAuthorization) error {
	preparation, _ := ctx.Value(readPreparationKey{}).(*readPreparation)
	if preparation != nil {
		preparation.empty = nil
		preparation.failure = nil
	}
	if len(b.Claims) < 1 || len(b.Claims) != len(b.Devices) {
		return ErrRefused
	}
	first := b.Claims[0]
	for i, c := range b.Claims {
		if !reflect.DeepEqual(first, c) || b.Devices[i].Principal != s.cfg.Source.Device {
			return ErrRefused
		}
	}
	current, e := s.query(ctx)
	if e != nil {
		if preparation != nil {
			preparation.failure = e
		}
		return e
	}
	if len(current) == 0 {
		if preparation != nil {
			// The verifier owns these callback values. Retain them only for
			// this read's expiry classification, never as an authority lease.
			preparation.empty = &b
		}
		return ErrRefused
	}
	for _, p := range current {
		var claims serviceorigin.DelegationClaims
		if jws.DecodeStrict(p.signed.Delegation.Claims, &claims) != nil || !reflect.DeepEqual(claims, first) {
			return ErrRefused
		}
	}
	return nil
}

func originalTimeCurrent[T any](p parsed[T], now appclock.Observation) bool {
	return !p.record.Publication.Timing.SampleTime.After(now.SampleTime) &&
		now.Physical.Earliest.UnixMilli() >= p.record.IssuedAtUnixMS &&
		now.Physical.Latest.UnixMilli() < p.record.ExpiresAtUnixMS
}

func interpretationExpired[T any](p parsed[T], now appclock.Observation) bool {
	timing := p.record.Publication.Timing
	until := timing.SampleTime.Add(p.record.Publication.TTL - max(timing.ErrorBound, timing.AgeHint))
	return !now.SampleTime.Before(until) || !now.SampleTime.Before(p.payload.SampleValidUntil) ||
		!now.Physical.Latest.Before(p.payload.PhysicalValidUntil)
}

// A successful authorization Query can become empty while the originals age.
// All immutable signatures passed before that callback. Omission is permitted
// only if every original has now expired and signing windows remain valid.
func (s *Session[T]) expiredAfterEmptyQuery(ctx context.Context, records []parsed[T], b serviceorigin.BatchAuthorization) error {
	now, err := s.clock.Observe(ctx)
	if err != nil {
		return err
	}
	for i, claims := range b.Claims {
		if now.Physical.Earliest.UnixMilli() < claims.IssuedAtUnixMS || now.Physical.Latest.UnixMilli() >= claims.ExpiresAtUnixMS || now.Physical.Earliest.Before(b.Devices[i].ValidFrom) || !now.Physical.Latest.Before(b.Devices[i].ValidUntil) {
			return ErrRefused
		}
	}
	for _, p := range records {
		if !originalTimeCurrent(p, now) || !interpretationExpired(p, now) {
			return ErrRefused
		}
	}
	return ctx.Err()
}

// Step reports a complete bounded current Query. An empty successful Query is
// absence at this tick; a refused or incomplete Query is never called absence.
func (s *Session[T]) Step(ctx context.Context) (out Result[T], err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out = Result[T]{Version: 1, Tick: s.ticks + 1, RunID: s.cfg.RunID, Status: "failed", Items: []Item[T]{}}
	if ctx == nil || s.fault || !s.continuous && s.ticks >= s.cfg.MaxTicks {
		return out, ErrRefused
	}
	s.operation = &readOperation{ctx: ctx, history: map[string]*highwater{}, verified: map[string]highwater{}}
	if s.continuous {
		ctx = context.WithValue(ctx, readOperationKey{}, s.operation)
		s.operation.ctx = ctx
	}
	defer func() {
		if s.continuous {
			s.finishHistory()
		}
		s.operation = nil
	}()
	var initialClock *Clock
	defer func() {
		if err != nil {
			if s.continuous && errors.Is(err, appclock.ErrUnavailable) && s.operation.clockRPCError != nil && !temporaryRead(s.operation.clockRPCError) {
				err = s.operation.clockRPCError
			}
			out.Items = []Item[T]{}
			out.AbsentIDs = nil
			out.OriginAttestedInference = false
			if s.continuous && temporaryRead(err) {
				// BatchVerifier latches even transient Now errors. Replace only
				// that wrapper after a witnessed temporary clock failure; the
				// pinned appclock and all source/history state remain untouched.
				if errors.Is(s.operation.clockError, appclock.ErrUnavailable) {
					s.verifier, _ = serviceorigin.NewBatchVerifier(s.verifierConfig)
				}
				out.Status, out.Clock = "unavailable", nil
				out.RPCCode = status.Code(err).String()
				if !errors.Is(err, ErrUnavailable) {
					err = errors.Join(ErrUnavailable, err)
				}
				return
			}
			var startup *startupFailure
			if errors.As(err, &startup) {
				out.RPCCode = startup.code.String()
				out.StartupAttempt = s.startupAttempts
				if startup.retry {
					out.Status = "waiting_for_source"
					out.Clock = initialClock
					return
				}
			}
			s.fault = true
		}
	}()
	if s.startupBegan.IsZero() {
		s.startupBegan = time.Now()
	}
	initial, err := s.clock.Observe(ctx)
	if err != nil {
		return out, err
	}
	out.SampleTime, out.ReaderGeneration = initial.SampleTime, initial.DeploymentGeneration
	initialClock = observedClock(initial)
	if s.continuous {
		s.pruneHistory(initial)
	}
	records, err := s.query(ctx)
	if err != nil {
		return out, err
	}
	var verified []serviceorigin.VerifiedRecord
	if len(records) > 0 {
		preparation := &readPreparation{}
		signed := make([]serviceorigin.SignedRecord, len(records))
		for i := range records {
			signed[i] = records[i].signed
		}
		verified, err = s.verifier.VerifyBatch(context.WithValue(ctx, readPreparationKey{}, preparation), signed)
		if err != nil {
			if s.continuous && s.operation.clockError != nil {
				return out, s.operation.clockError
			}
			if s.continuous && preparation.failure != nil {
				return out, preparation.failure
			}
			if !errors.Is(err, serviceorigin.ErrDenied) || preparation.empty == nil {
				return out, err
			}
			if err = s.expiredAfterEmptyQuery(ctx, records, *preparation.empty); err != nil {
				return out, err
			}
			records, verified = nil, nil
		}
		if len(verified) != len(records) {
			return out, ErrRefused
		}
		for i := range verified {
			if !bytes.Equal(verified[i].Bytes, records[i].signed.Record) || !reflect.DeepEqual(verified[i].Record, records[i].record) {
				return out, ErrRefused
			}
		}
		if s.continuous {
			for _, p := range records {
				s.retainVerifiedWindow(p)
			}
		}
	}
	now, err := s.clock.Observe(ctx)
	if err != nil {
		return out, err
	}
	out.SampleTime = now.SampleTime
	out.ReaderGeneration = now.DeploymentGeneration
	current := map[string]bool{}
	for i, p := range records {
		v := verified[i]
		if !originalTimeCurrent(p, now) || now.Physical.Latest.UnixMilli() >= v.Delegation.ExpiresAtUnixMS || !now.Physical.Latest.Before(v.Device.ValidUntil) || now.Physical.Earliest.Before(v.Device.ValidFrom) || now.Physical.Earliest.UnixMilli() < v.Delegation.IssuedAtUnixMS {
			return out, ErrRefused
		}
		if interpretationExpired(p, now) {
			continue
		}
		if p.retired {
			return out, ErrUnavailable
		}
		current[p.payload.ID] = true
		item := Item[T]{EnvelopeJSON: bytes.Clone(p.raw), EnvelopeSHA256: hash(p.raw), RecordSHA256: hash(p.signed.Record), Version: world.Version{Epoch: p.record.Epoch, Sequence: p.record.Sequence}, Payload: p.payload.Payload}
		if s.cfg.MissionFloorY != nil {
			item.MissionInput, err = prepareMissionInput(v, p.raw, s.cfg)
			if err != nil {
				return out, err
			}
		}
		out.Items = append(out.Items, item)
	}
	for id := range s.last {
		if !current[id] {
			out.AbsentIDs = append(out.AbsentIDs, id)
		}
	}
	sort.Strings(out.AbsentIDs)
	if err = ctx.Err(); err != nil {
		return out, err
	}
	s.last = current
	s.ticks++
	out.Status = "ready"
	out.OriginAttestedInference = true
	out.Clock = observedClock(now)
	return out, nil
}
func Run[T any](ctx context.Context, c Config, api appv1.WorldAppServiceClient, decode Decoder[T], emit func(Result[T]) error) (out Summary, err error) {
	if ctx == nil || emit == nil {
		return out, ErrRefused
	}
	s, err := New(c, api, decode)
	if err != nil {
		return out, err
	}
	defer s.Close()
	ctx, cancel := context.WithTimeout(ctx, time.Duration(s.cfg.DurationMS)*time.Millisecond)
	defer cancel()
	out.Status = "failed"
	for out.Ticks < s.cfg.MaxTicks {
		r, e := s.Step(ctx)
		if emitErr := emit(r); emitErr != nil {
			return out, emitErr
		}
		var startup *startupFailure
		waiting := errors.As(e, &startup) && startup.retry
		if e != nil && !waiting {
			return out, e
		}
		if waiting {
			out.StartupWaits++
		} else {
			out.Ticks++
			out.ItemsRead += len(r.Items)
		}
		if out.Ticks == s.cfg.MaxTicks {
			break
		}
		delay := time.Duration(s.cfg.PollIntervalMS) * time.Millisecond
		if waiting {
			delay = min(startupSpacing, max(time.Duration(0), startupBudget-time.Since(s.startupBegan)))
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return out, ctx.Err()
		case <-timer.C:
		}
	}
	out.Status = "complete"
	return out, nil
}
