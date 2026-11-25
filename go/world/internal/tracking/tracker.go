package tracking

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

type sourceState struct {
	version  world.Version
	sampleID string
	time     time.Time
	members  map[uint32][32]byte
}

type trackState struct {
	track      Track
	position   [2]float64
	covariance [4]float64
}

// Tracker is a bounded single-owner stream processor. Call Step serially using
// the configured sample clock. No wall clock, storage or network is consulted.
type Tracker struct {
	cfg     Config
	next    uint64
	last    time.Time
	tracks  []trackState
	sources map[string]sourceState
}

func New(cfg Config) (*Tracker, error) {
	if err := cfg.defaults(); err != nil {
		return nil, err
	}
	return &Tracker{cfg: cfg, sources: map[string]sourceState{}}, nil
}

type item struct {
	detection Detection
	novel     bool
}
type group struct {
	items       []item
	position    [2]float64
	covariance  [4]float64
	time, until time.Time
	novel       bool
	ambiguous   bool
}
type pair struct {
	a, b int
	cost float64
}

func less(a, b Detection) bool {
	if a.Source != b.Source {
		return a.Source < b.Source
	}
	if n := a.Version.Compare(b.Version); n != 0 {
		return n < 0
	}
	if a.SampleID != b.SampleID {
		return a.SampleID < b.SampleID
	}
	return a.MemberIndex < b.MemberIndex
}
func cloneDetection(d Detection) Detection {
	d.Evidence = append(d.Evidence[:0:0], d.Evidence...)
	return d
}
func cloneTrack(t Track) Track {
	t.Members = append(t.Members[:0:0], t.Members...)
	for i := range t.Members {
		t.Members[i] = cloneDetection(t.Members[i])
	}
	return t
}

// Step incorporates a complete batch and returns fresh tracks at now. Repeated
// observations do not count as new measurements or refresh expiry. Reordered
// older source versions are ignored. Invalid batches leave the tracker unchanged.
func (t *Tracker) Step(now time.Time, detections []Detection) ([]Track, error) {
	if now.IsZero() || !t.last.IsZero() && now.Before(t.last) || len(detections) > t.cfg.MaxDetections {
		return nil, errors.New("invalid tracking time or batch size")
	}
	in := make([]Detection, len(detections))
	for i, d := range detections {
		if err := d.validate(t.cfg); err != nil {
			return nil, err
		}
		if d.SampleTime.After(now) {
			return nil, errors.New("detection is in the future")
		}
		in[i] = cloneDetection(d)
	}
	sort.Slice(in, func(i, j int) bool { return less(in[i], in[j]) })
	sources := make(map[string]sourceState, len(t.sources))
	for key, s := range t.sources {
		members := make(map[uint32][32]byte, len(s.members))
		for k, v := range s.members {
			members[k] = v
		}
		s.members = members
		sources[key] = s
	}
	// Select the latest version for each source before considering members.
	for _, d := range in {
		s, ok := sources[d.Source]
		if !ok || d.Version.Compare(s.version) > 0 {
			if ok && d.SampleTime.Before(s.time) {
				return nil, errors.New("new source version regresses sample time")
			}
			sources[d.Source] = sourceState{version: d.Version, sampleID: d.SampleID, time: d.SampleTime, members: map[uint32][32]byte{}}
		}
	}
	if len(sources) > t.cfg.MaxSources {
		return nil, errors.New("tracking source bound exceeded")
	}
	items := []item{}
	batchSeen := map[string]map[uint32]bool{}
	for _, d := range in {
		s := sources[d.Source]
		if d.Version.Compare(s.version) < 0 {
			continue
		}
		if d.SampleID != s.sampleID || !d.SampleTime.Equal(s.time) {
			return nil, errors.New("conflicting source sample identity")
		}
		encoded, err := json.Marshal(d)
		if err != nil {
			return nil, err
		}
		digest := sha256.Sum256(encoded)
		old, seen := s.members[d.MemberIndex]
		if seen && old != digest {
			return nil, errors.New("conflicting duplicate detection")
		}
		if batchSeen[d.Source] == nil {
			batchSeen[d.Source] = map[uint32]bool{}
		}
		if batchSeen[d.Source][d.MemberIndex] {
			continue
		}
		batchSeen[d.Source][d.MemberIndex] = true
		s.members[d.MemberIndex] = digest
		if len(s.members) > t.cfg.MaxDetections {
			return nil, errors.New("source sample member bound exceeded")
		}
		sources[d.Source] = s
		if !now.Before(d.ValidUntil) || now.Sub(d.SampleTime) >= t.cfg.MaxMiss {
			continue
		}
		items = append(items, item{detection: d, novel: !seen})
	}
	groups := t.groups(items)
	tracks := make([]trackState, 0, len(t.tracks))
	for _, old := range t.tracks {
		if now.Before(old.track.ValidUntil) {
			old.track = cloneTrack(old.track)
			tracks = append(tracks, old)
		}
	}
	var edges []pair
	trackOptions, groupOptions := make([]int, len(tracks)), make([]int, len(groups))
	for i, s := range tracks {
		for j, g := range groups {
			if !g.novel {
				continue
			}
			if g.time.Before(s.track.LastObserved) {
				continue
			}
			p, cov := t.predict(s, g.time)
			dt := g.time.Sub(s.track.LastObserved).Seconds()
			if distance(s.position, g.position) > t.cfg.MaxFusionDistanceM+t.cfg.MaxSpeedMPS*dt {
				continue
			}
			cost := mahalanobis(p, g.position, cov, g.covariance)
			if cost > t.cfg.GateSquared {
				continue
			}
			edges = append(edges, pair{i, j, cost})
			trackOptions[i]++
			groupOptions[j]++
		}
	}
	sort.Slice(edges, func(i, j int) bool {
		a, b := edges[i], edges[j]
		if a.cost != b.cost {
			return a.cost < b.cost
		}
		if a.a != b.a {
			return tracks[a.a].track.ID < tracks[b.a].track.ID
		}
		return a.b < b.b
	})
	usedTracks, usedGroups := make([]bool, len(tracks)), make([]bool, len(groups))
	observed := map[string]bool{}
	for _, edge := range edges {
		if usedTracks[edge.a] || usedGroups[edge.b] {
			continue
		}
		usedTracks[edge.a], usedGroups[edge.b] = true, true
		g := groups[edge.b]
		if !g.novel {
			continue
		}
		s := &tracks[edge.a]
		p, cov := t.predict(*s, g.time)
		position, combined := intersection(p, g.position, cov, g.covariance)
		dt := g.time.Sub(s.track.LastObserved).Seconds()
		if dt > 0 {
			for axis := 0; axis < 2; axis++ {
				s.track.VelocityXZ[axis] = .5*s.track.VelocityXZ[axis] + .5*(position[axis]-s.position[axis])/dt
			}
			speed := math.Hypot(s.track.VelocityXZ[0], s.track.VelocityXZ[1])
			if speed > t.cfg.MaxSpeedMPS {
				for axis := 0; axis < 2; axis++ {
					s.track.VelocityXZ[axis] *= t.cfg.MaxSpeedMPS / speed
				}
			}
		}
		s.position, s.covariance = position, combined
		s.track.Revision++
		s.track.LastObserved = g.time
		s.track.ValidUntil = t.expiry(g)
		s.track.Members = members(g)
		s.track.Ambiguous = g.ambiguous || trackOptions[edge.a] > 1 || groupOptions[edge.b] > 1
		observed[s.track.ID] = true
	}
	next := t.next
	for j, g := range groups {
		if usedGroups[j] || !g.novel {
			continue
		}
		if len(tracks) >= t.cfg.MaxTracks {
			return nil, errors.New("tracking live track bound exceeded")
		}
		next++
		if next == 0 {
			return nil, errors.New("tracking ID space exhausted")
		}
		id := fmt.Sprintf("%s/%06d", t.cfg.TrackerID, next)
		tr := Track{Schema: Schema, ID: id, Revision: 1, RunID: t.cfg.RunID, ClockDomain: t.cfg.ClockDomain, Frame: t.cfg.Frame, FirstSeen: g.time, LastObserved: g.time, ValidUntil: t.expiry(g), Members: members(g), Ambiguous: g.ambiguous || groupOptions[j] > 0}
		tracks = append(tracks, trackState{track: tr, position: g.position, covariance: g.covariance})
		observed[id] = true
	}
	result := make([]Track, 0, len(tracks))
	for _, s := range tracks {
		tr := cloneTrack(s.track)
		tr.Time = now
		tr.State = "coasting"
		if observed[tr.ID] {
			tr.State = "observed"
		}
		tr.ObservedPositionXZ, tr.ObservedCovariance = s.position, s.covariance
		tr.PositionXZ, tr.Covariance = t.predict(s, now)
		result = append(result, tr)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	t.sources, t.tracks, t.next, t.last = sources, tracks, next, now
	return result, nil
}

func (t *Tracker) predict(s trackState, at time.Time) ([2]float64, [4]float64) {
	dt := at.Sub(s.track.LastObserved).Seconds()
	p, cov := s.position, s.covariance
	for i := 0; i < 2; i++ {
		p[i] += s.track.VelocityXZ[i] * dt
	}
	variance := math.Pow(t.cfg.ProcessNoiseMPS*dt, 2)
	cov[0] += variance
	cov[3] += variance
	return p, cov
}
func (t *Tracker) expiry(g group) time.Time {
	until := g.time.Add(t.cfg.MaxMiss)
	if g.until.Before(until) {
		until = g.until
	}
	return until
}
func members(g group) []Detection {
	out := make([]Detection, len(g.items))
	for i, item := range g.items {
		out[i] = cloneDetection(item.detection)
	}
	return out
}

func (t *Tracker) compatible(a, b Detection) (float64, bool) {
	if a.Source == b.Source {
		return 0, false
	}
	dt := a.SampleTime.Sub(b.SampleTime)
	if dt < 0 {
		dt = -dt
	}
	if dt > t.cfg.MaxSkew || distance(a.PositionXZ, b.PositionXZ) > t.cfg.MaxFusionDistanceM {
		return 0, false
	}
	cost := mahalanobis(a.PositionXZ, b.PositionXZ, a.Covariance, b.Covariance)
	return cost, cost <= t.cfg.GateSquared
}
func (t *Tracker) groups(items []item) []group {
	sets := make([][]int, len(items))
	owner := make([]int, len(items))
	var edges []pair
	choices := make([]map[string]int, len(items))
	for i := range choices {
		choices[i] = map[string]int{}
	}
	for i := range items {
		sets[i] = []int{i}
		owner[i] = i
		for j := 0; j < i; j++ {
			if cost, ok := t.compatible(items[j].detection, items[i].detection); ok {
				edges = append(edges, pair{j, i, cost})
				choices[i][items[j].detection.Source]++
				choices[j][items[i].detection.Source]++
			}
		}
	}
	sort.Slice(edges, func(i, j int) bool {
		a, b := edges[i], edges[j]
		if a.cost != b.cost {
			return a.cost < b.cost
		}
		if a.a != b.a {
			return a.a < b.a
		}
		return a.b < b.b
	})
	for _, edge := range edges {
		a, b := owner[edge.a], owner[edge.b]
		if a == b {
			continue
		}
		ok := true
		// Complete-link compatibility prevents A~B~C from joining A and C
		// when they are separated, and enforces at most one member per source.
		for _, i := range sets[a] {
			for _, j := range sets[b] {
				if _, compatible := t.compatible(items[i].detection, items[j].detection); !compatible {
					ok = false
				}
			}
		}
		if !ok {
			continue
		}
		if a > b {
			a, b = b, a
		}
		sets[a] = append(sets[a], sets[b]...)
		for _, j := range sets[b] {
			owner[j] = a
		}
		sets[b] = nil
	}
	out := []group{}
	for _, set := range sets {
		if len(set) == 0 {
			continue
		}
		sort.Ints(set)
		g := group{}
		for _, index := range set {
			if items[index].detection.SampleTime.After(g.time) {
				g.time = items[index].detection.SampleTime
			}
		}
		for i, index := range set {
			item := items[index]
			d := item.detection
			g.items = append(g.items, item)
			g.novel = g.novel || item.novel
			for _, count := range choices[index] {
				g.ambiguous = g.ambiguous || count > 1
			}
			// The estimate is at the newest member's time. Older measurements
			// have no observed velocity, so account for possible motion without
			// inventing a translated measurement or changing its original data.
			covariance := d.Covariance
			motion := t.cfg.MaxSpeedMPS * g.time.Sub(d.SampleTime).Seconds()
			covariance[0] += motion * motion
			covariance[3] += motion * motion
			if i == 0 {
				g.position, g.covariance, g.until = d.PositionXZ, covariance, d.ValidUntil
			} else {
				g.position, g.covariance = intersection(g.position, d.PositionXZ, g.covariance, covariance)
				if d.ValidUntil.Before(g.until) {
					g.until = d.ValidUntil
				}
			}
		}
		out = append(out, g)
	}
	return out
}
