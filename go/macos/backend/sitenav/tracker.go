package sitenav

import (
	"math"
	"sort"
	"time"
)

type trackedView struct {
	at    time.Time
	pose  Pose
	cells map[[3]int]bool
	fits  []RegistrationFit
}

const localizationViewMaxAge = 30 * time.Second

type PoseTracker struct {
	views  []trackedView
	lastAt time.Time
}
type TrackingResult struct {
	Fits             []RegistrationFit `json:"fits"`
	IndependentViews int               `json:"independentViews"`
	NovelFraction    float64           `json:"novelFraction"`
	Selected         int               `json:"selected"`
	Message          string            `json:"message"`
}

func normalizedCandidate(c PoseCandidate, origin Point) PoseCandidate {
	s, co := math.Sincos(c.Rotation)
	c.Position = Point{c.Position.X - co*origin.X + s*origin.Y, c.Position.Y - s*origin.X - co*origin.Y}
	return c
}
func compatiblePose(a, b PoseCandidate) bool {
	return a.Position.Distance(b.Position) < .3 && math.Abs(math.Remainder(a.Rotation-b.Rotation, 2*math.Pi)) < .18
}

// Update replaces evidence from repeated accumulated maps. A new independent
// view requires both new geometry and a changed viewpoint. Replaying packets
// cannot make an ambiguous position become confident.
func (t *PoseTracker) Update(pose Pose, cloud SurfaceCloud, fits []RegistrationFit, now time.Time) TrackingResult {
	if !t.lastAt.IsZero() && now.Before(t.lastAt) {
		t.views = nil
	}
	t.lastAt = now
	// Age each view from its original acquisition. Continuous failed matches
	// and replayed accumulated packets must not keep old evidence alive.
	for len(t.views) > 0 && now.Sub(t.views[0].at) >= localizationViewMaxAge {
		t.views[0] = trackedView{}
		t.views = t.views[1:]
	}
	cells := map[[3]int]bool{}
	for _, p := range cloud.Surfaces {
		cells[[3]int{int(math.Round(p.Position.X / .15)), int(math.Round(p.Position.Y / .15)), int(math.Round(p.Position.Z / .15))}] = true
	}
	novel := 1.
	if len(t.views) > 0 {
		previous := t.views[len(t.views)-1]
		common := 0
		for k := range cells {
			if previous.cells[k] {
				common++
			}
		}
		novel = 1 - float64(common)/float64(max(1, len(cells)))
	}
	result := TrackingResult{Fits: append([]RegistrationFit(nil), fits...), Selected: -1, NovelFraction: novel}
	for i := range result.Fits {
		fit := &result.Fits[i]
		normalized := normalizedCandidate(fit.Pose, pose.Position)
		for _, view := range t.views {
			best := .075
			for _, old := range view.fits {
				if old.Valid && compatiblePose(normalized, normalizedCandidate(old.Pose, view.pose.Position)) {
					best = math.Min(best, old.Cost)
				}
			}
			// A capped historical penalty breaks ties, without rewarding packet count
			// or turning a geometrically invalid current fit into a valid one.
			fit.Cost += math.Max(0, best-fit.Cost) * .15 / float64(max(1, len(t.views)))
		}
	}
	sort.SliceStable(result.Fits, func(i, j int) bool { return result.Fits[i].Cost < result.Fits[j].Cost })
	independent := len(t.views) == 0
	if len(t.views) > 0 {
		last := t.views[len(t.views)-1].pose
		independent = novel >= .20 && (pose.Position.Distance(last.Position) >= .20 || math.Abs(math.Remainder(pose.Yaw-last.Yaw, 2*math.Pi)) >= .18)
	}
	if independent && len(cells) >= 24 {
		t.views = append(t.views, trackedView{at: now, pose: pose, cells: cells, fits: append([]RegistrationFit(nil), fits...)})
		if len(t.views) > 4 {
			t.views = t.views[len(t.views)-4:]
		}
	}
	result.IndependentViews = len(t.views)
	for i, c := range result.Fits {
		if !c.Valid {
			continue
		}
		result.Selected = i
		break
	}
	if result.Selected < 0 {
		result.Message = "No pose fits the observed heights, surface directions and clear floor."
		if len(result.Fits) > 0 && result.Fits[0].Rejection != "" {
			result.Message = result.Fits[0].Rejection
		}
		return result
	}
	best := result.Fits[result.Selected]
	for i, other := range result.Fits {
		if i == result.Selected || (!other.Valid && !other.Plausible) || compatiblePose(best.Pose, other.Pose) {
			continue
		}
		if other.Cost-best.Cost <= math.Max(.003, best.Cost*.18) {
			result.Selected = -1
			result.Message = "Several room positions still fit. Waiting for distinct geometry or a verified camera match."
			return result
		}
	}
	if best.NeedsCorroboration && !corroboratedView(best, pose, cloud, t.views) {
		result.Selected = -1
		result.Message = "One surface could be deciding this position. Waiting for new geometry from another view or a verified camera match."
		return result
	}
	result.Message = "One pose fits the current geometry and available camera evidence. Check the preview before confirming."
	return result
}

// Old accumulated returns cannot corroborate themselves after a turn. Require
// the same alignment in an earlier view and new architectural columns in two
// directions. Unknown and furniture returns do not count as new wall evidence.
func corroboratedView(fit RegistrationFit, pose Pose, cloud SurfaceCloud, views []trackedView) bool {
	for _, view := range views {
		if pose.Position.Distance(view.pose.Position) < .20 && math.Abs(math.Remainder(pose.Yaw-view.pose.Yaw, 2*math.Pi)) < .18 {
			continue
		}
		agrees := false
		for _, previous := range view.fits {
			if previous.Valid && compatiblePose(normalizedCandidate(fit.Pose, pose.Position), normalizedCandidate(previous.Pose, view.pose.Position)) {
				agrees = true
				break
			}
		}
		if !agrees {
			continue
		}
		type directionGroup struct {
			direction Point
			columns   map[Cell]bool
		}
		groups := map[int]*directionGroup{}
		novel := 0
		for i, sample := range cloud.Surfaces {
			key := [3]int{int(math.Round(sample.Position.X / .15)), int(math.Round(sample.Position.Y / .15)), int(math.Round(sample.Position.Z / .15))}
			if view.cells[key] {
				continue
			}
			novel++
			if sample.Horizontal || i >= len(fit.Classes) || fit.Classes[i] != 1 {
				continue
			}
			angle := math.Mod(math.Atan2(sample.Direction.Y, sample.Direction.X)+math.Pi, math.Pi)
			bucket := int(math.Round(angle/(math.Pi/18))) % 18
			g := groups[bucket]
			if g == nil {
				g = &directionGroup{sample.Direction, map[Cell]bool{}}
				groups[bucket] = g
			}
			g.columns[cell(Point{sample.Position.X, sample.Position.Y})] = true
		}
		if float64(novel)/float64(max(1, len(cloud.Surfaces))) < .20 {
			continue
		}
		for _, a := range groups {
			for _, b := range groups {
				if len(a.columns) >= 6 && len(b.columns) >= 6 && math.Abs(a.direction.X*b.direction.Y-a.direction.Y*b.direction.X) > .342 {
					return true
				}
			}
		}
	}
	return false
}

// SiteReference transforms the observed map into a confirmed site's coordinates.
// It is only a localization target. Navigation always uses the current live grid.
func SiteReference(cloud SurfaceCloud, pose Pose, candidate PoseCandidate, floorY float64) []Surface {
	s, c := math.Sincos(candidate.Rotation)
	seen := map[[3]int]bool{}
	var out []Surface
	for _, v := range cloud.Surfaces {
		x, y := v.Position.X-pose.Position.X, v.Position.Y-pose.Position.Y
		v.Position = Vertex{candidate.Position.X + c*x - s*y, candidate.Position.Y + s*x + c*y, floorY + v.Position.Z - cloud.Floor}
		v.Direction = Point{c*v.Direction.X - s*v.Direction.Y, s*v.Direction.X + c*v.Direction.Y}
		key := [3]int{int(math.Round(v.Position.X / .1)), int(math.Round(v.Position.Y / .1)), int(math.Round(v.Position.Z / .1))}
		if !seen[key] {
			out = append(out, v)
			seen[key] = true
		}
	}
	return out
}
