package anchors

import (
	"cmp"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/spatial"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/geom"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

// ModelVersion is the provenance model version of the records this package
// derives. It changes with the signature format or the estimators.
const ModelVersion = "1"

// idPrefix starts every identifier the store assigns: "anchor-<n>".
const idPrefix = "anchor-"

type frameKey struct {
	device   spatial.DeviceID
	sequence uint64
}

// mapFrame is one keyframe of the map with its base pose in the site frame.
type mapFrame struct {
	kf   spatial.Keyframe
	site geom.Pose2
	cov  geom.Mat3 // of site over (x, z, yaw)
}

// Store is the durable set of anchors of one site. Anchors survive a restart;
// the keyframes do not, they are the map's to keep and are fed again through
// ApplyKeyframes.
type Store struct {
	cfg     Config
	file    *os.File
	size    int64
	anchors map[string]spatial.Anchor
	next    uint64 // number of the next identifier
	frames  map[frameKey]mapFrame
}

// Open opens the store kept in dir, creating the directory and an empty
// journal when there is none, and replays the journal. A torn last record is
// dropped; any other damage is ErrCorrupt.
func Open(dir string, cfg Config) (*Store, error) {
	cfg, err := cfg.withDefaults()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, journalFile), os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, err
	}
	s := &Store{cfg: cfg, file: f, anchors: map[string]spatial.Anchor{}, next: 1, frames: map[frameKey]mapFrame{}}
	if err := s.replay(); err != nil {
		f.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) replay() error {
	entries, good, err := readJournal(s.file)
	if err != nil {
		return err
	}
	for i, e := range entries {
		if err := s.apply(e); err != nil {
			return fmt.Errorf("%w: record %d: %w", ErrCorrupt, i, err)
		}
	}
	// Cut a torn tail off so the next record follows an intact one.
	if err := s.file.Truncate(good); err != nil {
		return err
	}
	s.size = good
	_, err = s.file.Seek(good, 0)
	return err
}

// apply folds one journal entry into the in-memory state.
func (s *Store) apply(e entry) error {
	id := e.Anchor.ID
	old, known := s.anchors[id]
	switch e.Op {
	case opPut:
		if err := e.Anchor.Validate(); err != nil {
			return err
		}
		if n, err := strconv.ParseUint(strings.TrimPrefix(id, idPrefix), 10, 64); err == nil && strings.HasPrefix(id, idPrefix) && n >= s.next {
			s.next = n + 1
		}
		s.anchors[id] = e.Anchor
	case opPose:
		if !known {
			return fmt.Errorf("pose of unknown anchor %q", id)
		}
		old.Location, old.Yaw, old.Revision = e.Anchor.Location, e.Anchor.Yaw, e.Anchor.Revision
		if err := old.Validate(); err != nil {
			return err
		}
		s.anchors[id] = old
	case opDelete:
		if !known {
			return fmt.Errorf("delete of unknown anchor %q", id)
		}
		delete(s.anchors, id)
	default:
		return fmt.Errorf("unknown operation %q", e.Op)
	}
	return nil
}

// commit makes the entries durable, then applies them. On a failed write the
// journal is cut back to its previous length and the state is unchanged.
//
// ponytail: the journal is never compacted, so it grows by one small record
// per anchor per map correction. Rewrite the live anchors to a new file and
// rename it over the old one when that becomes megabytes.
func (s *Store) commit(entries ...entry) error {
	if len(entries) == 0 {
		return nil
	}
	if s.file == nil {
		return os.ErrClosed
	}
	data, err := encodeEntries(entries)
	if err != nil {
		return err
	}
	_, err = s.file.Write(data)
	if err == nil {
		err = s.file.Sync()
	}
	if err != nil {
		if cut := s.file.Truncate(s.size); cut == nil {
			_, cut = s.file.Seek(s.size, 0)
		}
		return err
	}
	s.size += int64(len(data))
	for _, e := range entries {
		if err := s.apply(e); err != nil {
			return err // unreachable: every entry was validated before it was written
		}
	}
	return nil
}

// Close releases the journal. The store must not be used afterwards.
func (s *Store) Close() error {
	if s.file == nil {
		return nil
	}
	err := s.file.Close()
	s.file = nil
	return err
}

// Get returns the anchor with the identifier.
func (s *Store) Get(id string) (spatial.Anchor, bool) {
	a, ok := s.anchors[id]
	return a, ok
}

// Anchors returns every anchor as the payload to publish, ordered by
// identifier.
func (s *Store) Anchors() []spatial.Anchor {
	out := make([]spatial.Anchor, 0, len(s.anchors))
	for _, a := range s.anchors {
		out = append(out, a)
	}
	slices.SortFunc(out, func(a, b spatial.Anchor) int { return compareIDs(a.ID, b.ID) })
	return out
}

// compareIDs orders identifiers by length and then bytes, so that anchor-2
// precedes anchor-10.
func compareIDs(a, b string) int {
	return cmp.Or(cmp.Compare(len(a), len(b)), cmp.Compare(a, b))
}

// Correction is the outcome of one ApplyKeyframes call.
type Correction struct {
	// Moved lists the anchors whose site pose changed, each under its new
	// revision, ordered by identifier.
	Moved []spatial.Anchor
	// Unbound lists the anchors none of whose keyframes is in the map. Their
	// pose is the last one resolved and no longer follows anything.
	Unbound []string
}

// ApplyKeyframes adds keyframes to the map, or replaces the ones already
// there after a pose-graph correction, and moves every anchor bound to them.
// frameToSite maps the frame the keyframes' Local poses are in to the site
// frame: the device's frame alignment, or the zero Pose2 when they already
// are site poses. An anchor whose pose changes by more than MinShiftM or
// MinShiftYaw gets a new revision and a journal record.
func (s *Store) ApplyKeyframes(frameToSite geom.Pose2, keyframes []spatial.Keyframe) (Correction, error) {
	if !finitePose(frameToSite) {
		return Correction{}, fmt.Errorf("%w: frame-to-site transform", ErrConfig)
	}
	for _, kf := range keyframes {
		if err := kf.Validate(); err != nil {
			return Correction{}, err
		}
	}
	for _, kf := range keyframes {
		s.frames[frameKey{kf.Device, kf.Sequence}] = mapFrame{kf: kf, site: frameToSite.Compose(kf.Local).Normalized(), cov: geom.TransformCov3(frameToSite, kf.Covariance)}
	}
	var out Correction
	var entries []entry
	for _, a := range s.Anchors() {
		pose, sigma, ok := s.locate(a.Keyframes)
		if !ok {
			out.Unbound = append(out.Unbound, a.ID)
			continue
		}
		if pose.Pos().Dist(a.Pose2().Pos()) <= s.cfg.MinShiftM && math.Abs(geom.AngleDiff(pose.Yaw, a.Yaw)) <= s.cfg.MinShiftYaw {
			continue
		}
		revision, err := strconv.ParseUint(a.Revision, 10, 64)
		if err != nil {
			return Correction{}, fmt.Errorf("anchors: revision %q of %s: %w", a.Revision, a.ID, err)
		}
		a.Location = spatial.At(pose.Pos().Lift(a.PositionM[1]), sigma)
		a.Yaw, a.Revision = pose.Yaw, strconv.FormatUint(revision+1, 10)
		if err := a.Validate(); err != nil {
			return Correction{}, err
		}
		out.Moved = append(out.Moved, a)
		entries = append(entries, entry{Op: opPose, Anchor: spatial.Anchor{ID: a.ID, Location: a.Location, Yaw: a.Yaw, Revision: a.Revision}})
	}
	if err := s.commit(entries...); err != nil {
		return Correction{}, err
	}
	return out, nil
}

// locate is the site pose of an anchor from its bindings: the pose its first
// binding with a keyframe in the map gives, the binding it was placed
// against when that keyframe is still there. The uncertainty is the larger
// of that keyframe's 1σ carried through the offset and the farthest any
// other binding puts the anchor from there, so a disagreement between
// keyframes is reported instead of averaged away.
func (s *Store) locate(bindings []spatial.KeyframeRef) (pose geom.Pose2, sigma float64, ok bool) {
	for _, b := range bindings {
		f, known := s.frames[frameKey{b.Device, b.Sequence}]
		if !known {
			continue
		}
		p := f.site.Compose(b.Offset).Normalized()
		if !ok {
			pose, ok = p, true
			sigma = geom.ApplyCov(f.site, f.cov, b.Offset.Pos(), geom.Cov2{}).MaxSigma()
			continue
		}
		sigma = math.Max(sigma, p.Pos().Dist(pose.Pos()))
	}
	return pose, sigma, ok
}

// Bind expresses a site pose on a floor as an offset from the nearest
// keyframe of the map, for Place and Update. It returns ErrNoKeyframe when no
// keyframe of that floor lies within BindRadius. It is only as good as the
// map: in a map that still awaits correction, place against the keyframe the
// pose was observed from instead.
func (s *Store) Bind(floor spatial.FloorID, pose geom.Pose2) (spatial.KeyframeRef, error) {
	var best frameKey
	distance := math.Inf(1)
	for k, f := range s.frames {
		if f.kf.Floor != floor {
			continue
		}
		d := f.site.Pos().Dist(pose.Pos())
		if d < distance || (d == distance && cmp.Or(cmp.Compare(k.device, best.device), cmp.Compare(k.sequence, best.sequence)) < 0) {
			best, distance = k, d
		}
	}
	if !(distance <= s.cfg.BindRadius) || !finitePose(pose) {
		return spatial.KeyframeRef{}, fmt.Errorf("%w: floor %q within %g m", ErrNoKeyframe, floor, s.cfg.BindRadius)
	}
	return spatial.KeyframeRef{Device: best.device, Sequence: best.sequence, Offset: s.frames[best].site.Between(pose).Normalized()}, nil
}

// Place creates an anchor at at.Offset in the base frame of the keyframe
// (at.Device, at.Sequence), which must be in the map and have a floor. The
// anchor takes that floor, the given elevation (site Y, metres), the next
// identifier and revision "1". It is also bound to that keyframe's sequence
// neighbours within BindRadius, and its signature holds the scan of each
// bound keyframe.
func (s *Store) Place(name string, at spatial.KeyframeRef, elevation float64) (spatial.Anchor, error) {
	return s.put(idPrefix+strconv.FormatUint(s.next, 10), 1, name, at, elevation)
}

// Update renames and re-places an existing anchor under its next revision,
// with new bindings and a new signature.
func (s *Store) Update(id, name string, at spatial.KeyframeRef, elevation float64) (spatial.Anchor, error) {
	old, ok := s.anchors[id]
	if !ok {
		return spatial.Anchor{}, fmt.Errorf("%w: %q", ErrNotFound, id)
	}
	revision, err := strconv.ParseUint(old.Revision, 10, 64)
	if err != nil {
		return spatial.Anchor{}, fmt.Errorf("anchors: revision %q of %s: %w", old.Revision, id, err)
	}
	return s.put(id, revision+1, name, at, elevation)
}

// Delete removes an anchor. Its identifier is not reused.
func (s *Store) Delete(id string) error {
	if _, ok := s.anchors[id]; !ok {
		return fmt.Errorf("%w: %q", ErrNotFound, id)
	}
	return s.commit(entry{Op: opDelete, Anchor: spatial.Anchor{ID: id}})
}

func (s *Store) put(id string, revision uint64, name string, at spatial.KeyframeRef, elevation float64) (spatial.Anchor, error) {
	primary, ok := s.frames[frameKey{at.Device, at.Sequence}]
	if !ok {
		return spatial.Anchor{}, fmt.Errorf("%w: %s/%d is not in the map", ErrNoKeyframe, at.Device, at.Sequence)
	}
	if primary.kf.Floor == "" {
		return spatial.Anchor{}, fmt.Errorf("%w: %s/%d has no floor", ErrNoKeyframe, at.Device, at.Sequence)
	}
	if !finitePose(at.Offset) {
		return spatial.Anchor{}, fmt.Errorf("%w: offset", ErrConfig)
	}
	at.Offset = at.Offset.Normalized()
	pose := primary.site.Compose(at.Offset).Normalized()
	// ponytail: a neighbour's offset comes from the map's own relative pose
	// between the two keyframes, so it inherits the local map error at the
	// time of placement (centimetres between consecutive keyframes). It only
	// decides the pose once the first keyframe has left the map. Measure the
	// relative pose with scanmatch.ScanToScan if that ever matters.
	bindings := []spatial.KeyframeRef{at}
	for d := uint64(1); d < uint64(s.cfg.MaxBindings); d++ {
		for _, sequence := range []uint64{at.Sequence - d, at.Sequence + d} {
			n, ok := s.frames[frameKey{at.Device, sequence}]
			if !ok || sequence > at.Sequence != (sequence == at.Sequence+d) || len(bindings) == s.cfg.MaxBindings ||
				n.kf.Floor != primary.kf.Floor || n.site.Pos().Dist(primary.site.Pos()) > s.cfg.BindRadius {
				continue
			}
			bindings = append(bindings, spatial.KeyframeRef{Device: at.Device, Sequence: sequence, Offset: n.site.Between(pose).Normalized()})
		}
	}
	patches := make([]patch, len(bindings))
	for i, b := range bindings {
		kf := s.frames[frameKey{b.Device, b.Sequence}].kf
		patches[i] = patch{anchor: kf.SensorToBase.Between(b.Offset).Normalized(), scan: kf.Scan}
	}
	signature, err := encodeSignature(patches)
	if err != nil {
		return spatial.Anchor{}, err
	}
	_, sigma, _ := s.locate(bindings)
	a := spatial.Anchor{
		ID: id, Name: name, Floor: primary.kf.Floor, Location: spatial.At(pose.Pos().Lift(elevation), sigma), Yaw: pose.Yaw,
		Signature: signature, Keyframes: bindings, Revision: strconv.FormatUint(revision, 10),
	}
	if err := a.Validate(); err != nil {
		return spatial.Anchor{}, err
	}
	if err := s.commit(entry{Op: opPut, Anchor: a}); err != nil {
		return spatial.Anchor{}, err
	}
	return a, nil
}

func finitePose(p geom.Pose2) bool {
	return !math.IsNaN(p.X+p.Z+p.Yaw) && !math.IsInf(p.X+p.Z+p.Yaw, 0) && !math.IsInf(p.X, 0) && !math.IsInf(p.Z, 0)
}

// Component wraps an anchor as the World component to publish: Inferred
// provenance under the model "wendy.spatial.anchors" with the given inputs,
// reduced to spatial.MaxInputs. The inputs are what the anchor was derived
// from: the scans of its bound keyframes, or their keyframe records.
func Component(scope spatial.Scope, version world.Version, frame world.FrameRef, timing world.Timing, ttl time.Duration, appVersion string, a spatial.Anchor, inputs []world.Input) (world.Component, error) {
	provenance, err := spatial.InferredProvenance("anchors", ModelVersion, appVersion, spatial.ReduceInputs(inputs))
	if err != nil {
		return world.Component{}, err
	}
	return spatial.ToComponent(scope, version, frame, timing, ttl, provenance, a)
}
