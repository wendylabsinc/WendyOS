package worldview

import (
	"math"
	"sort"
	"strconv"
	"time"
)

// Lifecycle event kinds.
const (
	KindAppeared = "appeared"
	KindPeak     = "peak"
	KindMoved    = "moved"
	KindLost     = "lost"
)

// peakEpsilon absorbs floating point error when comparing a confidence rise
// against PeakDelta, so a rise from 0.80 to 0.85 counts as 0.05.
const peakEpsilon = 1e-9

// Match is one proposal that passed fusion in one frame.
type Match struct {
	Box      [4]float64
	Fused    Fused
	Proposal Proposal
}

// Lifecycle is one event in a track's life.
type Lifecycle struct {
	Kind     string // appeared, peak, moved, lost
	TrackID  string // "<source>/<object>/<n>", n from 1 per (source, object)
	Object   string
	SourceID string
	Match    *Match // nil for lost
	// Last is the track's most recent association, set only for lost, so a
	// lost record can carry the box and scores the object was last seen with.
	Last *Match
	At   time.Time
}

type trackKey struct{ source, object string }

type track struct {
	id       string
	box      [4]float64 // latest box, used for association
	anchor   [4]float64 // box at the last emitted event, used for moved
	peak     float64    // confidence at the last emitted appeared or peak
	lastSeen time.Time
	last     Match // the latest match associated with this track
	silent   bool  // created inside the appeared cooldown and not yet announced
}

// Tracker turns per-frame matches into lifecycle events per (source, object).
// Nothing is emitted for a track that is seen again unchanged. A Tracker is
// not safe for concurrent use.
type Tracker struct {
	ClearAfter, Cooldown time.Duration
	IoUMin               float64
	MoveFraction         float64
	PeakDelta            float64

	tracks       map[trackKey][]*track
	serial       map[trackKey]int
	lastAppeared map[trackKey]time.Time
	suppressed   int
}

// NewTracker returns a Tracker with IoUMin 0.3, MoveFraction 0.25 and
// PeakDelta 0.05.
func NewTracker(clearAfter, cooldown time.Duration) *Tracker {
	return &Tracker{ClearAfter: clearAfter, Cooldown: cooldown, IoUMin: 0.3, MoveFraction: 0.25, PeakDelta: 0.05}
}

// Suppressed returns how many new tracks stayed silent because an appeared
// event for the same source and object was emitted less than Cooldown before.
func (t *Tracker) Suppressed() int { return t.suppressed }

func (t *Tracker) init() {
	if t.tracks == nil {
		t.tracks = map[trackKey][]*track{}
		t.serial = map[trackKey]int{}
		t.lastAppeared = map[trackKey]time.Time{}
	}
}

// Observe processes one frame's matches for one source and object. Order of
// events: lost for stale tracks, then updates to existing tracks in creation
// order (moved before peak when both apply), then appeared for new tracks in
// match order.
//
// Tracks unseen for at least ClearAfter are lost first, so a reappearing
// object starts a new track. Matches are associated to the remaining tracks by
// greedy best Intersection over Union (IoU) of at least IoUMin; ties go to the
// older track, then the earlier match. An unmatched match starts a track, which
// emits appeared unless an appeared for the same source and object was emitted
// less than Cooldown ago. Such a silent track emits nothing (not even lost)
// until it is seen once the cooldown has elapsed, when it emits appeared.
//
// An announced track emits moved when its box centre is more than MoveFraction
// of the larger side of the box at its last emitted event away from that box's
// centre, and peak when its confidence exceeds the last reported peak by at
// least PeakDelta.
func (t *Tracker) Observe(sourceID, object string, now time.Time, matches []Match) []Lifecycle {
	t.init()
	key := trackKey{sourceID, object}
	var events []Lifecycle
	events = append(events, t.expireKey(key, now)...)

	live := t.tracks[key]
	type pair struct {
		track, match int
		iou          float64
	}
	var pairs []pair
	for ti, tr := range live {
		for mi, m := range matches {
			if iou := IoU(tr.box, m.Box); iou >= t.IoUMin && iou > 0 {
				pairs = append(pairs, pair{ti, mi, iou})
			}
		}
	}
	sort.SliceStable(pairs, func(i, j int) bool {
		if pairs[i].iou != pairs[j].iou {
			return pairs[i].iou > pairs[j].iou
		}
		if pairs[i].track != pairs[j].track {
			return pairs[i].track < pairs[j].track
		}
		return pairs[i].match < pairs[j].match
	})
	trackMatch := make([]int, len(live))
	for i := range trackMatch {
		trackMatch[i] = -1
	}
	matchUsed := make([]bool, len(matches))
	for _, p := range pairs {
		if trackMatch[p.track] >= 0 || matchUsed[p.match] {
			continue
		}
		trackMatch[p.track] = p.match
		matchUsed[p.match] = true
	}

	for ti, tr := range live {
		mi := trackMatch[ti]
		if mi < 0 {
			continue
		}
		m := matches[mi]
		tr.box, tr.lastSeen, tr.last = m.Box, now, m
		if tr.silent {
			if t.inCooldown(key, now) {
				continue
			}
			tr.silent = false
			tr.anchor, tr.peak = m.Box, m.Fused.Confidence
			t.lastAppeared[key] = now
			events = append(events, t.event(KindAppeared, key, tr, &m, now))
			continue
		}
		if t.moved(tr.anchor, m.Box) {
			tr.anchor = m.Box
			events = append(events, t.event(KindMoved, key, tr, &m, now))
		}
		if m.Fused.Confidence-tr.peak >= t.PeakDelta-peakEpsilon {
			tr.peak = m.Fused.Confidence
			events = append(events, t.event(KindPeak, key, tr, &m, now))
		}
	}

	for mi := range matches {
		if matchUsed[mi] {
			continue
		}
		m := matches[mi]
		t.serial[key]++
		tr := &track{
			id:  sourceID + "/" + object + "/" + strconv.Itoa(t.serial[key]),
			box: m.Box, anchor: m.Box, peak: m.Fused.Confidence, lastSeen: now, last: m,
		}
		t.tracks[key] = append(t.tracks[key], tr)
		if t.inCooldown(key, now) {
			tr.silent = true
			t.suppressed++
			continue
		}
		t.lastAppeared[key] = now
		events = append(events, t.event(KindAppeared, key, tr, &m, now))
	}
	return events
}

// Expire emits lost for every announced track, across all sources and
// objects, unseen for at least ClearAfter, and forgets silent tracks unseen as
// long. Events are ordered by source, then object, then track creation.
func (t *Tracker) Expire(now time.Time) []Lifecycle {
	t.init()
	keys := make([]trackKey, 0, len(t.tracks))
	for k := range t.tracks {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].source != keys[j].source {
			return keys[i].source < keys[j].source
		}
		return keys[i].object < keys[j].object
	})
	var events []Lifecycle
	for _, k := range keys {
		events = append(events, t.expireKey(k, now)...)
	}
	return events
}

func (t *Tracker) expireKey(key trackKey, now time.Time) []Lifecycle {
	var events []Lifecycle
	kept := t.tracks[key][:0]
	for _, tr := range t.tracks[key] {
		if now.Sub(tr.lastSeen) < t.ClearAfter {
			kept = append(kept, tr)
			continue
		}
		if !tr.silent {
			lost := t.event(KindLost, key, tr, nil, now)
			last := tr.last
			lost.Last = &last
			events = append(events, lost)
		}
	}
	for i := len(kept); i < len(t.tracks[key]); i++ {
		t.tracks[key][i] = nil
	}
	if len(kept) == 0 {
		delete(t.tracks, key)
	} else {
		t.tracks[key] = kept
	}
	return events
}

func (t *Tracker) inCooldown(key trackKey, now time.Time) bool {
	last, ok := t.lastAppeared[key]
	return ok && now.Sub(last) < t.Cooldown
}

func (t *Tracker) moved(from, to [4]float64) bool {
	side := math.Max(from[2], from[3])
	dx := (to[0] + to[2]/2) - (from[0] + from[2]/2)
	dy := (to[1] + to[3]/2) - (from[1] + from[3]/2)
	return math.Hypot(dx, dy) > t.MoveFraction*side
}

func (t *Tracker) event(kind string, key trackKey, tr *track, m *Match, now time.Time) Lifecycle {
	var match *Match
	if m != nil {
		copied := *m
		match = &copied
	}
	return Lifecycle{Kind: kind, TrackID: tr.id, Object: key.object, SourceID: key.source, Match: match, At: now}
}
