package semantic

import (
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/spatial"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/geom"
)

// Observation is one object detection placed on a floor plane.
type Observation struct {
	Time     time.Time
	Position geom.Vec3 // site frame; Y is the floor plane
	// SigmaM is the 1σ of the planar position that the foot pixel's noise
	// leaves along the viewing direction, the worse of its two axes.
	SigmaM     float64
	Class      string
	Confidence float64
}

// Place puts the object detections of a camera frame on the horizontal plane
// at World elevation floorY by intersecting each foot pixel's ray with it.
// cameraToSite maps the optical frame to the site frame at the frame's
// instant. A detection is dropped when its ray does not come down onto the
// plane or when the pixel constrains the position worse than
// Config.MaxPlacementSigma, as it does near the horizon. Observations carry
// the frame's own timestamp.
func Place(cfg Config, frame spatial.CameraFrame, in geom.Intrinsics, cameraToSite geom.Pose3, floorY float64) ([]Observation, error) {
	cfg, err := cfg.withDefaults()
	if err != nil {
		return nil, err
	}
	if err := in.Validate(); err != nil {
		return nil, err
	}
	var out []Observation
	for _, d := range frame.Detections {
		if d.Kind != spatial.DetectObject || d.Class == "" {
			continue
		}
		origin, dir, err := in.PixelRay(cameraToSite, d.Pixel)
		if err != nil {
			continue
		}
		p, ok := geom.RayPlaneY(origin, dir, floorY)
		height := origin.Y - floorY
		if !ok || !(height > 0) {
			continue
		}
		// Ground range r = h / tan(depression): a pixel error of σ/f radians
		// moves it by (r² + h²) / h times that.
		r := p.XZ().Dist(origin.XZ())
		sigma := cfg.FootPixelSigma / in.FX * (r*r + height*height) / height
		if sigma > cfg.MaxPlacementSigma {
			continue
		}
		out = append(out, Observation{Time: frame.Time, Position: p, SigmaM: sigma, Class: d.Class, Confidence: d.Confidence})
	}
	return out, nil
}

// Objects fuses placed detections into static objects: detections that land
// within Config.ObjectRadius of each other on one floor plane are one object,
// its class the classes that hold a real share of them. Everything detected
// takes part, movers included, so that a place robots drive through does not
// turn the detector's occasional confusions into furniture. Not safe for
// concurrent use.
type Objects struct {
	cfg      Config
	clusters []*cluster
	next     int
	pruned   time.Time
}

type cluster struct {
	id          int
	pos         geom.Vec3
	weight      float64 // Σ 1/σ²
	n           int
	class       map[string]float64 // Σ confidence per class
	total       float64
	first, last time.Time
}

// NewObjects returns an empty object map.
func NewObjects(cfg Config) (*Objects, error) {
	cfg, err := cfg.withDefaults()
	if err != nil {
		return nil, err
	}
	return &Objects{cfg: cfg}, nil
}

// Add fuses one observation. Observations are expected in time order;
// unconfirmed objects not seen for Config.Probation are forgotten.
func (o *Objects) Add(obs Observation) {
	if !(obs.SigmaM > 0) || !(obs.Confidence > 0) || obs.Class == "" {
		return
	}
	var best *cluster
	bestD := o.cfg.ObjectRadius
	for _, c := range o.clusters {
		if d := c.pos.XZ().Dist(obs.Position.XZ()); d <= bestD && math.Abs(c.pos.Y-obs.Position.Y) <= o.cfg.FloorTolerance {
			best, bestD = c, d
		}
	}
	if best == nil {
		o.next++
		best = &cluster{id: o.next, class: map[string]float64{}, first: obs.Time, last: obs.Time}
		o.clusters = append(o.clusters, best)
	}
	w := 1 / (obs.SigmaM * obs.SigmaM)
	best.pos = best.pos.Scale(best.weight / (best.weight + w)).Add(obs.Position.Scale(w / (best.weight + w)))
	best.weight += w
	best.n++
	best.class[obs.Class] += obs.Confidence
	best.total += obs.Confidence
	if obs.Time.After(best.last) {
		best.last = obs.Time
	}
	if obs.Time.Sub(o.pruned) >= time.Second {
		o.pruned = obs.Time
		kept := o.clusters[:0]
		for _, c := range o.clusters {
			if c.n >= o.cfg.MinObservations || obs.Time.Sub(c.last) <= o.cfg.Probation {
				kept = append(kept, c)
			}
		}
		clear(o.clusters[len(kept):])
		o.clusters = kept
	}
}

// List returns the confirmed objects on the floor plane at the elevation
// whose class has a row in Config.RoomTypes, as Object payloads of the
// floor: one per class that holds at least Config.MinClassShare of the
// detections at that place. Confidence is that share and UncertaintyM the
// 1σ of the fused position.
func (o *Objects) List(floor spatial.FloorID, elevation float64) []spatial.Object {
	var out []spatial.Object
	for _, c := range o.clusters {
		if c.n < o.cfg.MinObservations || math.Abs(c.pos.Y-elevation) > o.cfg.FloorTolerance {
			continue
		}
		classes := make([]string, 0, len(c.class))
		for class := range c.class {
			classes = append(classes, class)
		}
		sort.Strings(classes)
		for _, class := range classes {
			share := c.class[class] / c.total
			if _, votes := o.cfg.RoomTypes[class]; !votes || share < o.cfg.MinClassShare {
				continue
			}
			out = append(out, spatial.Object{
				ID: fmt.Sprintf("%s-o%d-%s", floor, c.id, class), Class: class, Confidence: min(share, 1), Floor: floor,
				Location: spatial.At(c.pos, 1/math.Sqrt(c.weight)), FirstSeen: c.first, LastSeen: c.last,
			})
		}
	}
	return out
}
