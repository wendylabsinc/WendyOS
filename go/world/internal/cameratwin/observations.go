// Package cameratwin publishes bounded, local inferred observations. It does
// not certify device identity or grant hardware execution authority.
package cameratwin

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/camerapose"
	"github.com/wendylabsinc/wendy/go/world/internal/stationarycamera"
	worldstore "github.com/wendylabsinc/wendy/go/world/internal/store"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

const Schema = "wendy.camera-twin.observations.v1"
const Lifetime = 1250 * time.Millisecond

type Input struct {
	CameraID       string       `json:"camera_id"`
	ReceiptID      string       `json:"receipt_id"`
	SHA256         string       `json:"sha256"`
	SourceSession  string       `json:"source_session"`
	Sequence       uint64       `json:"sequence"`
	ReceivedAt     time.Time    `json:"received_at"`
	Origin         world.Origin `json:"origin"`
	PoseObservedAt time.Time    `json:"pose_observed_at"`
}
type Observation struct {
	ID          string       `json:"id"`
	Position    [3]float64   `json:"position"`
	ObservedAt  time.Time    `json:"observed_at"`
	ValidUntil  time.Time    `json:"valid_until"`
	Provenance  string       `json:"provenance"`
	InputOrigin world.Origin `json:"input_origin"`
	Inputs      []Input      `json:"inputs"`
}
type Batch struct {
	Schema          string        `json:"schema"`
	CoordinateFrame string        `json:"coordinate_frame"`
	ModelSHA256     string        `json:"model_sha256"`
	FloorY          float64       `json:"floor_y"`
	Observations    []Observation `json:"observations"`
}
type Camera struct {
	State stationarycamera.State
	Pose  camerapose.State
}
type Lookup func(string) (Camera, bool)
type Store struct {
	mu              sync.Mutex
	now             func() time.Time
	lookup          Lookup
	replica         *worldstore.Store
	source          world.Source
	frame           world.FrameRef
	site            camerapose.Site
	epoch, sequence uint64
	batch           Batch
	digest          string
	// Receipts cannot be reused to refresh a person claim or resurrect a cleared
	// batch. The bounded input high-water marks survive every accepted update.
	seen        map[string]uint64
	poseDigests map[string]string
}

func Open(dir string, site camerapose.Site, lookup Lookup, now func() time.Time) (*Store, error) {
	if now == nil {
		now = time.Now
	}
	if site.ID == "" || site.ModelRevision == "" || lookup == nil {
		return nil, errors.New("site and camera lookup required")
	}
	frame := world.FrameRef{ID: "wendy-site:" + site.ID + ":" + site.ModelRevision + ":scan-local-y-up", Revision: site.ModelRevision}
	source := world.Source{Device: "local-camera-viewer", App: "wendy-world", Service: "people", Stream: site.ID}
	r, err := worldstore.Open(filepath.Join(dir, "people-world.json"), world.Config{Tenant: "local-camera-demo", Site: site.ID, ClockDomain: "local-camera-receipt", Now: now, MaxEntries: 17, AllowSynthetic: true, Frames: []world.FrameRef{frame}})
	if err != nil {
		return nil, err
	}
	epoch, err := r.NextEpoch(source)
	if err != nil {
		r.Close()
		return nil, err
	}
	s := &Store{now: now, lookup: lookup, replica: r, source: source, frame: frame, site: site, epoch: epoch, seen: map[string]uint64{}, poseDigests: map[string]string{}, batch: Batch{Schema: Schema, CoordinateFrame: frame.ID, ModelSHA256: site.ModelSHA256, Observations: []Observation{}}}
	// A crashed publisher can leave a long-lived calibration component. Retire
	// its current claim durably before exposing a new service session.
	records, err := r.Records()
	if err != nil {
		r.Close()
		return nil, err
	}
	for _, old := range records {
		if old.Key.Source != source {
			r.Close()
			return nil, errors.New("unexpected twin source in local replica")
		}
		if old.Tombstone {
			continue
		}
		s.sequence++
		retired := world.Component{Key: old.Key, Version: world.Version{Epoch: epoch, Sequence: s.sequence}, Frame: frame, Timing: world.Timing{SampleTime: now(), ClockDomain: "local-camera-receipt", ErrorKnown: true}, TTL: time.Second, Tombstone: true, Provenance: world.Provenance{Origin: world.Measured, AppVersion: "world-camera-twin-v1"}}
		if _, err = r.Apply(source, retired); err != nil {
			r.Close()
			return nil, err
		}
	}
	return s, nil
}
func (s *Store) Close() error { return s.replica.Close() }
func (s *Store) validate(b Batch, now time.Time) error {
	if b.Schema != Schema || b.CoordinateFrame != s.frame.ID || b.ModelSHA256 != s.site.ModelSHA256 || !finite(b.FloorY) || math.Abs(b.FloorY) > 1000 || len(b.Observations) > 64 {
		return errors.New("invalid observation batch or site")
	}
	ids := map[string]bool{}
	for _, o := range b.Observations {
		if o.ID == "" || len(o.ID) > 128 || ids[o.ID] || o.Provenance != "inferred" || (o.InputOrigin != world.Measured && o.InputOrigin != world.Simulated) || len(o.Inputs) < 1 || len(o.Inputs) > 16 {
			return errors.New("invalid anonymous observation")
		}
		ids[o.ID] = true
		for _, p := range o.Position {
			if !finite(p) || math.Abs(p) > 1000 {
				return errors.New("invalid position")
			}
		}
		if math.Abs(o.Position[1]-b.FloorY) > 0.001 {
			return errors.New("position is off selected floor")
		}
		var earliest, latest time.Time
		cameras := map[string]bool{}
		until := now.Add(Lifetime)
		for _, in := range o.Inputs {
			c, ok := s.lookup(in.CameraID)
			if !ok || cameras[in.CameraID] || !c.State.Fresh || c.State.Latest == nil || !c.Pose.Fresh || c.Pose.Status != "located" || c.Pose.Pose == nil || c.Pose.CoordinateFrame != b.CoordinateFrame || c.Pose.Site.ModelSHA256 != b.ModelSHA256 || c.Pose.SourceSession != in.SourceSession || c.State.Latest.SourceSession != in.SourceSession || !c.Pose.Pose.ObservedAt.Equal(in.PoseObservedAt) || c.Pose.InputOrigin != in.Origin || in.Origin != o.InputOrigin {
				return errors.New("camera or pose is unavailable or changed")
			}
			cameras[in.CameraID] = true
			found := false
			for _, r := range c.State.Recent {
				if r.ID == in.ReceiptID && r.SHA256 == in.SHA256 && r.SourceSession == in.SourceSession && r.Sequence == in.Sequence && r.ReceivedAt.Equal(in.ReceivedAt) && r.Origin == in.Origin && r.RetainedUntil.After(now) {
					found = true
					break
				}
			}
			if !found || in.ReceivedAt.After(now) || !now.Before(in.ReceivedAt.Add(Lifetime)) {
				return errors.New("original camera receipt unavailable or expired")
			}
			if earliest.IsZero() || in.ReceivedAt.Before(earliest) {
				earliest = in.ReceivedAt
			}
			if in.ReceivedAt.After(latest) {
				latest = in.ReceivedAt
			}
			if c.Pose.Pose.ValidUntil.Before(until) {
				until = c.Pose.Pose.ValidUntil
			}
			d := 0.0
			for i := range o.Position {
				v := o.Position[i] - c.Pose.Pose.Position[i]
				d += v * v
			}
			if d > 144 || c.Pose.Pose.Position[1] < b.FloorY+0.2 {
				return errors.New("projection outside camera floor bounds")
			}
		}
		if latest.Sub(earliest) > 350*time.Millisecond {
			return errors.New("camera observations are not contemporaneous")
		}
		if earliest.Add(Lifetime).Before(until) {
			until = earliest.Add(Lifetime)
		}
		if !o.ObservedAt.Equal(latest) || !o.ValidUntil.Equal(until) || !now.Before(o.ValidUntil) {
			return errors.New("observation lifetime differs from original inputs")
		}
	}
	return nil
}

// normalize preserves exact server receipt times despite JSON clients rounding
// fractional seconds. Caller values must still agree within one millisecond.
func (s *Store) normalize(b *Batch) error {
	for oi := range b.Observations {
		o := &b.Observations[oi]
		var earliest, latest, until time.Time
		for ii := range o.Inputs {
			in := &o.Inputs[ii]
			c, ok := s.lookup(in.CameraID)
			if !ok || c.Pose.Pose == nil {
				return errors.New("camera pose unavailable")
			}
			if !closeTime(in.PoseObservedAt, c.Pose.Pose.ObservedAt) {
				return errors.New("pose changed")
			}
			in.PoseObservedAt = c.Pose.Pose.ObservedAt
			found := false
			for _, r := range c.State.Recent {
				if r.ID == in.ReceiptID && r.SourceSession == in.SourceSession && closeTime(in.ReceivedAt, r.ReceivedAt) {
					in.ReceivedAt = r.ReceivedAt
					found = true
					break
				}
			}
			if !found {
				return errors.New("receipt unavailable")
			}
			if earliest.IsZero() || in.ReceivedAt.Before(earliest) {
				earliest = in.ReceivedAt
			}
			if in.ReceivedAt.After(latest) {
				latest = in.ReceivedAt
			}
			if until.IsZero() || c.Pose.Pose.ValidUntil.Before(until) {
				until = c.Pose.Pose.ValidUntil
			}
		}
		if earliest.Add(Lifetime).Before(until) {
			until = earliest.Add(Lifetime)
		}
		if !closeTime(o.ObservedAt, latest) || !closeTime(o.ValidUntil, until) {
			return errors.New("claim timing differs from original inputs")
		}
		o.ObservedAt = latest
		o.ValidUntil = until
	}
	return nil
}
func closeTime(a, b time.Time) bool {
	return !a.IsZero() && !b.IsZero() && a.Sub(b) <= time.Millisecond && b.Sub(a) <= time.Millisecond
}

func (s *Store) Publish(b Batch) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	if err := s.normalize(&b); err != nil {
		return err
	}
	if err := s.validate(b, now); err != nil {
		return err
	}
	data, err := json.Marshal(b)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(data)
	digest := hex.EncodeToString(sum[:])
	if digest == s.digest {
		return nil
	}
	next := map[string]uint64{}
	for k, v := range s.seen {
		next[k] = v
	}
	inputs := map[string]world.Input{}
	advanced := false
	for _, o := range b.Observations {
		for _, in := range o.Inputs {
			key := in.CameraID + ":" + in.SourceSession
			if in.Sequence < s.seen[key] {
				return errors.New("observation input was superseded")
			}
			if in.Sequence > s.seen[key] {
				advanced = true
			}
			if in.Sequence > next[key] {
				next[key] = in.Sequence
			}
			id := key + ":" + in.ReceiptID
			leaf := s.leaf(in.CameraID, in.SourceSession+":"+in.ReceiptID, in.Origin)
			inputs[id] = leaf
		}
	}
	if len(b.Observations) > 0 && !advanced {
		return errors.New("observation batch needs a new camera receipt")
	}
	if len(next) > 256 {
		return errors.New("too many camera sessions; restart local viewer service")
	}
	provenance := world.Provenance{Origin: world.Inferred, AppVersion: "world-camera-twin-v1", ModelID: "camera-twin-floor-projection", ModelVersion: "v1"}
	keys := make([]string, 0, len(inputs))
	for key := range inputs {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		provenance.Inputs = append(provenance.Inputs, inputs[key])
	}
	ttl := Lifetime
	for _, o := range b.Observations {
		if d := o.ValidUntil.Sub(now); d < ttl {
			ttl = d
		}
	}
	component := world.Component{Key: world.Key{Tenant: "local-camera-demo", Site: s.site.ID, Entity: "people", Type: Schema, Source: s.source}, Version: world.Version{Epoch: s.epoch, Sequence: s.sequence + 1}, Frame: s.frame, Timing: world.Timing{SampleTime: now, ClockDomain: "local-camera-receipt", ErrorKnown: true}, TTL: ttl, Provenance: provenance, Payload: data}
	// An empty batch is a source-owned tombstone; it contains no fabricated input.
	if len(b.Observations) == 0 {
		component.Tombstone = true
		component.Payload = nil
		component.Provenance = world.Provenance{Origin: world.Measured, AppVersion: "world-camera-twin-v1"}
	}
	if _, err = s.replica.Apply(s.source, component); err != nil {
		return err
	}
	s.sequence++
	s.batch = b
	s.digest = digest
	s.seen = next
	return nil
}
func (s *Store) Snapshot() Batch {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.batch
	out.Observations = []Observation{}
	for _, o := range s.batch.Observations {
		one := s.batch
		one.Observations = []Observation{o}
		if s.validate(one, s.now()) == nil {
			copy := o
			copy.Inputs = append([]Input(nil), o.Inputs...)
			out.Observations = append(out.Observations, copy)
		}
	}
	return out
}
func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }
