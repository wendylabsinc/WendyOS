package twin

import (
	"errors"
	"github.com/google/uuid"
	"math"
	"sort"
	"sync"
	"time"
)

type PersonObservation struct {
	Camera     string  `json:"camera"`
	Position   Point   `json:"position"`
	ObservedAt float64 `json:"observedAt"`
}
type PersonTrack struct {
	ID         string  `json:"id"`
	Position   Point   `json:"position"`
	ObservedAt float64 `json:"observedAt"`
}
type People struct {
	mu           sync.Mutex
	rooms        map[string][]PersonTrack
	observations map[string]map[string][]PersonObservation
}

func distance(a, b Point) float64 {
	return math.Sqrt((a.X-b.X)*(a.X-b.X) + (a.Y-b.Y)*(a.Y-b.Y) + (a.Z-b.Z)*(a.Z-b.Z))
}
func (p *People) Update(room string, observations []PersonObservation, now time.Time) ([]PersonTrack, error) {
	if len(observations) > 256 {
		return nil, errors.New("too many people observations")
	}
	at := float64(now.UnixNano()) / 1e9
	for _, o := range observations {
		if !validID(o.Camera) || math.IsNaN(o.ObservedAt) || math.IsInf(o.ObservedAt, 0) {
			return nil, errors.New("invalid observation")
		}
		for _, v := range []float64{o.Position.X, o.Position.Y, o.Position.Z} {
			if math.IsNaN(v) || math.IsInf(v, 0) || math.Abs(v) > 10000 {
				return nil, errors.New("invalid position")
			}
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.rooms == nil {
		p.rooms = map[string][]PersonTrack{}
	}
	live := []PersonTrack{}
	for _, t := range p.rooms[room] {
		if age := at - t.ObservedAt; age >= 0 && age <= 1.25 {
			live = append(live, t)
		}
	}

	if p.observations == nil {
		p.observations = map[string]map[string][]PersonObservation{}
	}
	cameras := p.observations[room]
	if cameras == nil {
		cameras = map[string][]PersonObservation{}
	}
	batch := map[string][]PersonObservation{}
	for _, o := range observations {
		if age := at - o.ObservedAt; age >= 0 && age <= 1.25 {
			batch[o.Camera] = append(batch[o.Camera], o)
		}
	}
	for camera, values := range batch {
		if old := cameras[camera]; len(old) == 0 || values[0].ObservedAt >= old[0].ObservedAt {
			cameras[camera] = values
		}
	}
	fresh := []PersonObservation{}
	for camera, values := range cameras {
		kept := []PersonObservation{}
		for _, o := range values {
			if age := at - o.ObservedAt; age >= 0 && age <= 1.25 {
				kept = append(kept, o)
			}
		}
		if len(kept) == 0 {
			delete(cameras, camera)
		} else {
			cameras[camera] = kept
			fresh = append(fresh, kept...)
		}
	}
	p.observations[room] = cameras
	sort.Slice(fresh, func(i, j int) bool { return fresh[i].ObservedAt > fresh[j].ObservedAt })
	groups := [][]PersonObservation{}
	for _, o := range fresh {
		matched := -1
		for i, g := range groups {
			compatible := true
			for _, other := range g {
				if other.Camera == o.Camera || math.Abs(other.ObservedAt-o.ObservedAt) > .35 || distance(other.Position, o.Position) >= .65 {
					compatible = false
					break
				}
			}
			if compatible {
				matched = i
				break
			}
		}
		if matched >= 0 {
			groups[matched] = append(groups[matched], o)
		} else {
			groups = append(groups, []PersonObservation{o})
		}
	}
	used := map[string]bool{}
	for _, group := range groups {
		position := Point{}
		observedAt := 0.0
		for _, o := range group {
			position.X += o.Position.X
			position.Y += o.Position.Y
			position.Z += o.Position.Z
			observedAt = math.Max(observedAt, o.ObservedAt)
		}
		count := float64(len(group))
		position.X /= count
		position.Y /= count
		position.Z /= count
		best := -1
		closest := math.Inf(1)
		for i, t := range live {
			if used[t.ID] {
				continue
			}
			limit := math.Min(2, .4+math.Max(0, observedAt-t.ObservedAt)*3)
			d := distance(position, t.Position)
			if d < limit && d < closest {
				best = i
				closest = d
			}
		}
		if best >= 0 {
			live[best].Position = position
			live[best].ObservedAt = observedAt
			used[live[best].ID] = true
		} else {
			t := PersonTrack{uuid.NewString(), position, observedAt}
			live = append(live, t)
			used[t.ID] = true
		}
	}

	// Human positions are deliberately transient. No disk persistence or recovery
	// of old live readings after the service restarts.
	p.rooms[room] = live
	return append([]PersonTrack{}, live...), nil
}
