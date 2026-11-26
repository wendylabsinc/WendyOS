package query

import (
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/spatial"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/geom"
)

var (
	// ErrInvalid reports a Query, World or reference that fails validation.
	ErrInvalid = errors.New("query: invalid")
	// ErrConfig reports a Config outside its documented ranges.
	ErrConfig = errors.New("query: invalid config")
	// ErrUnknownEntity reports a reference that names no row.
	ErrUnknownEntity = errors.New("query: unknown entity")
	// ErrAmbiguous reports a reference that names more than one row.
	ErrAmbiguous = errors.New("query: ambiguous reference")
	// ErrUnknownRegion reports a region name that matches no region entity,
	// or a same-region relation asked of a world without regions.
	ErrUnknownRegion = errors.New("query: unknown region")
	// ErrNoFloor reports a point reference whose floor cannot be determined.
	ErrNoFloor = errors.New("query: floor not determined")
	// ErrNoGrid reports a visibility or path query on a floor without an
	// occupancy grid.
	ErrNoGrid = errors.New("query: no occupancy grid for the floor")
	// ErrNoExtent reports an inside relation to an entity without a footprint.
	ErrNoExtent = errors.New("query: entity has no extent")
)

// Ref names a place: an entity, or a point on a floor. Exactly one of Entity
// and Point is set.
type Ref struct {
	// Entity is an entity ID ("rover-1") or a key ("device/rover-1"). An ID
	// shared by rows of several kinds needs the key form.
	Entity string `json:"entity,omitempty"`
	// Point is a site (x, z) position.
	Point *geom.Vec2 `json:"point,omitempty"`
	// Floor is the floor of Point. Empty takes Query.Floor, or the only
	// floor of the world.
	Floor spatial.FloorID `json:"floor,omitempty"`
}

// EntityRef refers to an entity by ID or key.
func EntityRef(id string) *Ref { return &Ref{Entity: id} }

// PointRef refers to a position on a floor.
func PointRef(p geom.Vec2, floor spatial.FloorID) *Ref { return &Ref{Point: &p, Floor: floor} }

// Box is an axis-aligned rectangle in site (x, z), bounds included.
type Box struct {
	Min geom.Vec2 `json:"min"`
	Max geom.Vec2 `json:"max"`
}

// RelationKind names how a result must relate to another place.
type RelationKind string

const (
	// RelNear keeps entities within DistanceM of the target.
	RelNear RelationKind = "near"
	// RelSameRegion keeps entities in a region that also holds the target.
	RelSameRegion RelationKind = "same_region"
	// RelVisibleFrom keeps entities with a clear sight line from the target.
	RelVisibleFrom RelationKind = "visible_from"
	// RelInside keeps entities whose position lies in the target's footprint.
	RelInside RelationKind = "inside"
)

// Relation is one predicate between a result and a target place. Several
// relations in a Query must all hold.
type Relation struct {
	Kind      RelationKind `json:"kind"`
	To        Ref          `json:"to"`
	DistanceM float64      `json:"distance_m,omitempty"` // RelNear only
}

// Order names the key results are sorted by.
type Order string

const (
	// OrderDefault is OrderDistance with an origin and OrderKey without.
	OrderDefault Order = ""
	// OrderDistance sorts by planar distance from the origin.
	OrderDistance Order = "distance"
	// OrderPath sorts by path distance from the origin; unreachable last.
	OrderPath Order = "path"
	// OrderTime sorts by ObservedAt.
	OrderTime Order = "time"
	// OrderKey sorts by spatial.CompareEntities.
	OrderKey Order = "key"
)

// Query is a conjunction of filters with an ordering and a limit. The zero
// Query selects every row in key order.
type Query struct {
	// Kinds and Classes keep rows whose kind or class is listed.
	Kinds   []spatial.EntityKind `json:"kinds,omitempty"`
	Classes []string             `json:"classes,omitempty"`
	// Floor keeps rows on one floor.
	Floor spatial.FloorID `json:"floor,omitempty"`

	// Origin is the place distances, sight and paths are measured from. It
	// restricts the answer to its floor.
	Origin *Ref `json:"origin,omitempty"`
	// Nearest keeps the K rows nearest to Origin among those that pass every
	// filter. Zero keeps all.
	Nearest int `json:"nearest,omitempty"`
	// WithinM keeps rows at most this far from Origin. Zero is no bound.
	WithinM float64 `json:"within_m,omitempty"`
	// Visible keeps rows with a clear sight line from Origin.
	Visible bool `json:"visible,omitempty"`
	// PathWithinM keeps rows whose path distance from Origin is at most
	// this. Zero is no bound.
	PathWithinM float64 `json:"path_within_m,omitempty"`

	// Box keeps rows positioned in the rectangle.
	Box *Box `json:"box,omitempty"`
	// Region keeps rows inside the region entity with this ID, or, when no
	// ID matches, inside any region entity with this label.
	Region string `json:"region,omitempty"`
	// Polygon keeps rows positioned in the polygon (even-odd rule).
	Polygon []geom.Vec2 `json:"polygon,omitempty"`

	Relations []Relation `json:"relations,omitempty"`

	// SeenSince, SeenBefore and ValidAt are the temporal filters; a zero
	// time is no filter.
	SeenSince  time.Time `json:"seen_since,omitzero"`
	SeenBefore time.Time `json:"seen_before,omitzero"`
	ValidAt    time.Time `json:"valid_at,omitzero"`

	// Order and Descending sort the answer; Limit then keeps the first rows.
	// Descending needs an explicit Order. Zero Limit keeps all.
	Order      Order `json:"order,omitempty"`
	Descending bool  `json:"descending,omitempty"`
	Limit      int   `json:"limit,omitempty"`
}

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

func finite(values ...float64) bool {
	for _, v := range values {
		if math.IsNaN(v) || math.IsInf(v, 0) || math.Abs(v) > spatial.MaxCoordinateM {
			return false
		}
	}
	return true
}

func (r Ref) validate() error {
	switch {
	case (r.Entity == "") == (r.Point == nil):
		return invalid("a reference names an entity or a point, not both or neither")
	case r.Point != nil && !finite(r.Point.X, r.Point.Z):
		return invalid("reference point is not a finite site coordinate")
	case r.Entity != "" && r.Floor != "":
		return invalid("an entity reference takes its floor from the entity")
	}
	return nil
}

// Validate checks the query on its own, before any world is consulted. The
// errors wrap ErrInvalid.
func (q Query) Validate() error {
	for _, k := range q.Kinds {
		if !k.Valid() {
			return invalid("unknown entity kind %q", k)
		}
	}
	if q.Nearest < 0 || q.Limit < 0 {
		return invalid("nearest and limit must not be negative")
	}
	if !finite(q.WithinM, q.PathWithinM) || q.WithinM < 0 || q.PathWithinM < 0 {
		return invalid("distance bounds must be finite and not negative")
	}
	if q.Origin != nil {
		if err := q.Origin.validate(); err != nil {
			return err
		}
	} else if q.Nearest > 0 || q.WithinM > 0 || q.PathWithinM > 0 || q.Visible || q.Order == OrderDistance || q.Order == OrderPath {
		return invalid("nearest, within, visible, path and distance order need an origin")
	}
	if b := q.Box; b != nil && (!finite(b.Min.X, b.Min.Z, b.Max.X, b.Max.Z) || b.Min.X > b.Max.X || b.Min.Z > b.Max.Z) {
		return invalid("box must be finite with min <= max")
	}
	if n := len(q.Polygon); n != 0 && (n < 3 || n > spatial.MaxPolygonVertices) {
		return invalid("polygon needs 3 to %d vertices", spatial.MaxPolygonVertices)
	}
	for _, v := range q.Polygon {
		if !finite(v.X, v.Z) {
			return invalid("polygon vertex is not a finite site coordinate")
		}
	}
	for _, r := range q.Relations {
		if err := r.To.validate(); err != nil {
			return err
		}
		switch r.Kind {
		case RelNear:
			if !finite(r.DistanceM) || r.DistanceM <= 0 {
				return invalid("near needs a positive distance")
			}
		case RelSameRegion, RelVisibleFrom, RelInside:
			if r.DistanceM != 0 {
				return invalid("%s takes no distance", r.Kind)
			}
		default:
			return invalid("unknown relation %q", r.Kind)
		}
	}
	switch q.Order {
	case OrderDefault:
		if q.Descending {
			return invalid("descending needs an explicit order")
		}
	case OrderDistance, OrderPath, OrderTime, OrderKey:
	default:
		return invalid("unknown order %q", q.Order)
	}
	return nil
}
