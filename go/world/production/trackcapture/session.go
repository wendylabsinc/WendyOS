package trackcapture

import (
	"errors"
	"reflect"
	"sort"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/sensing"
	"github.com/wendylabsinc/wendy/go/world/internal/tracking"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"github.com/wendylabsinc/wendy/go/world/production/resultrecord"
	"github.com/wendylabsinc/wendy/go/world/production/trustedtime"
)

var errExpired = errors.New("original camera capture expired")

type measurement struct {
	at, until  time.Time
	position   [2]float64
	covariance [4]float64
	members    []Member
	inputs     []Input
}
type Session struct {
	cfg      Config
	tracker  *tracking.Tracker
	previous map[string]measurement
}

func New(cfg Config) (*Session, error) {
	if cfg.Tenant == "" || cfg.Site == "" || len(cfg.Sources) < 1 || len(cfg.Sources) > MaxMembers || cfg.ModelID == "" || cfg.ModelVersion == "" || !sensing.DigestValid(cfg.ModelSHA256) {
		return nil, errors.New("explicit camera sources and exact inference model required")
	}
	if cfg.Tracking.MaxMiss == 0 {
		cfg.Tracking.MaxMiss = DefaultLifetime
	}
	if cfg.Tracking.MaxMiss > DefaultLifetime {
		return nil, errors.New("anonymous capture lifetime exceeds 1.25 seconds")
	}
	sources := map[string]world.Source{}
	seen := map[world.Source]bool{}
	for name, source := range cfg.Sources {
		if name == "" || source.Validate() != nil || seen[source] {
			return nil, errors.New("invalid or aliased camera source")
		}
		sources[name] = source
		seen[source] = true
	}
	cfg.Sources = sources
	t, err := tracking.New(cfg.Tracking)
	if err != nil {
		return nil, err
	}
	return &Session{cfg: cfg, tracker: t, previous: map[string]measurement{}}, nil
}

// Support returns previous original inputs for fresh verification by the
// caller. Cached Verified values are not authority for a later operation.
func (s *Session) Support(now time.Time) []Input {
	byHash := map[string]Input{}
	for _, m := range s.previous {
		if !now.Before(m.until) {
			continue
		}
		for _, in := range m.inputs {
			in.SupportOnly = true
			byHash[digest(in.Envelope)] = in
		}
	}
	keys := make([]string, 0, len(byHash))
	for key := range byHash {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := []Input{}
	for _, key := range keys {
		if len(out) == resultrecord.MaxInputs {
			break
		}
		out = append(out, own(byHash[key]))
	}
	return out
}

func (s *Session) Step(now time.Time, w trustedtime.Interval, inputs []Input) (Snapshot, error) {
	out := Snapshot{Tracks: []tracking.Track{}, Cameras: []Camera{}, candidates: map[string]candidate{}, now: now, physical: w.Latest}
	if now.IsZero() || w.Validate() != nil || len(inputs) > resultrecord.MaxInputs {
		return out, errors.New("current source and physical time and bounded input set required")
	}
	current := map[string]prepared{}
	all := map[string]prepared{}
	detections := []tracking.Detection{}
	proofBytes, imageBytes := 0, 0
	for _, in := range inputs {
		proofBytes += len(in.Envelope)
		imageBytes += len(in.Image)
		if proofBytes > 4<<20 || imageBytes > 32<<20 {
			return out, errors.New("camera batch byte bound exceeded")
		}
		p, err := s.prepare(in, now, w)
		if errors.Is(err, errExpired) && in.SupportOnly {
			continue
		}
		if err != nil {
			return out, err
		}
		hash := p.validated.Reference.EnvelopeSHA256
		if _, duplicate := all[hash]; duplicate {
			return out, errors.New("duplicate camera proof")
		}
		all[hash] = p
		if in.SupportOnly {
			continue
		}
		if _, duplicate := current[p.sample.SensorID]; duplicate {
			return out, errors.New("multiple current versions of camera")
		}
		current[p.sample.SensorID] = p
		out.Cameras = append(out.Cameras, Camera{p.sample.SensorID, p.sample.ID, len(in.Poses.Detections), len(p.projected.Detections), p.projected.Rejected})
		for _, d := range p.projected.Detections {
			detections = append(detections, p.detection(d))
		}
	}
	sort.Slice(out.Cameras, func(i, j int) bool { return out.Cameras[i].SensorID < out.Cameras[j].SensorID })
	tracks, err := s.tracker.Step(now, detections)
	if err != nil {
		return out, err
	}
	out.Tracks = tracks
	next := map[string]measurement{}
	for _, track := range tracks {
		if previous, ok := s.previous[track.ID]; ok && now.Before(previous.until) {
			next[track.ID] = previous
		}
		if track.State != "observed" {
			continue
		}
		// Re-run only this associated group's measurements through a new
		// tracker. Its first observation is CI, with no temporal filter state.
		one, err := tracking.New(s.cfg.Tracking)
		if err != nil {
			return out, err
		}
		fresh, err := one.Step(track.LastObserved, track.Members)
		if err != nil || len(fresh) != 1 {
			return out, errors.New("current track members do not form one observation")
		}
		m := measurement{at: track.LastObserved, until: track.ValidUntil, position: fresh[0].PositionXZ, covariance: fresh[0].Covariance}
		for _, d := range track.Members {
			p, ok := current[d.Source]
			if !ok || p.sample.ID != d.SampleID || p.validated.Component.Version != d.Version {
				return out, errors.New("track member is not a current original")
			}
			m.members = append(m.members, p.member(d))
			m.inputs = append(m.inputs, own(p.input))
			m.until = minimum(m.until, p.deadline)
		}
		body := Payload{Schema: ResultSchema, Algorithm: AlgorithmID, AlgorithmVersion: AlgorithmVersion, TrackID: track.ID, Revision: track.Revision, RunID: track.RunID, ClockDomain: track.ClockDomain, Frame: track.Frame, ObservedAt: m.at, ValidUntil: m.until, PositionXZ: m.position, Covariance: m.covariance, Ambiguous: track.Ambiguous, Current: m.members}
		if previous, ok := s.previous[track.ID]; ok && now.Before(previous.until) && m.at.After(previous.at) {
			complete := true
			for i, member := range previous.members {
				p, found := all[member.Proof.EnvelopeSHA256]
				if !found || p.validated.Reference != member.Proof || !reflect.DeepEqual(p.input.Poses, previous.inputs[i].Poses) || !now.Before(member.ValidUntil) || !w.Latest.Before(member.PhysicalUntil) {
					complete = false
					break
				}
			}
			if complete {
				dt := m.at.Sub(previous.at).Seconds()
				velocity := [2]float64{(m.position[0] - previous.position[0]) / dt, (m.position[1] - previous.position[1]) / dt}
				var covariance [4]float64
				for i := range covariance {
					covariance[i] = 2 * (m.covariance[i] + previous.covariance[i]) / (dt * dt)
				}
				body.MotionKnown = true
				body.VelocityXZ = &velocity
				body.VelocityCovariance = &covariance
				at := previous.at
				body.PreviousObservedAt = &at
				body.Previous = previous.members
				body.ValidUntil = minimum(body.ValidUntil, previous.until)
			}
		}
		used := []resultrecord.ValidatedInput{}
		seen := map[string]bool{}
		for _, member := range append(append([]Member{}, body.Current...), body.Previous...) {
			key := member.Proof.EnvelopeSHA256
			if !seen[key] {
				used = append(used, all[key].validated)
				seen[key] = true
			}
		}
		out.candidates[track.ID] = candidate{body, used}
		next[track.ID] = m
	}
	s.previous = next
	return out, nil
}
