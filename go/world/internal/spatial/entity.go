package spatial

import (
	"cmp"
	"sort"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/spatial/geom"
)

// EntityKind is the category of a spatial entity.
type EntityKind string

const (
	EntityDevice   EntityKind = "device"
	EntitySensor   EntityKind = "sensor"
	EntityAnchor   EntityKind = "anchor"
	EntityTrack    EntityKind = "track"
	EntityRegion   EntityKind = "region"
	EntityChange   EntityKind = "change"
	EntityPortal   EntityKind = "portal"
	EntityLandmark EntityKind = "landmark"
	EntityObject   EntityKind = "object"
)

// EntityKinds lists every kind in a fixed order.
func EntityKinds() []EntityKind {
	return []EntityKind{EntityDevice, EntitySensor, EntityAnchor, EntityTrack, EntityRegion, EntityChange, EntityPortal, EntityLandmark, EntityObject}
}

func (k EntityKind) Valid() bool {
	for _, v := range EntityKinds() {
		if k == v {
			return true
		}
	}
	return false
}

// MaxPolygonVertices bounds every polygon in this package.
const MaxPolygonVertices = 256

// Extent is the planar footprint of something around its position: a circle
// of RadiusM, or a Polygon in site (x, z) coordinates, or neither for a point.
type Extent struct {
	RadiusM float64     `json:"radius_m,omitempty"`
	Polygon []geom.Vec2 `json:"polygon,omitempty"`
}

func (e Extent) check() error {
	if !finite(e.RadiusM) || e.RadiusM < 0 || e.RadiusM > MaxCoordinateM || (e.RadiusM > 0 && len(e.Polygon) > 0) {
		return invalid("extent")
	}
	return checkPolygon(e.Polygon, true)
}

func checkPolygon(poly []geom.Vec2, optional bool) error {
	if len(poly) == 0 && optional {
		return nil
	}
	if len(poly) < 3 || len(poly) > MaxPolygonVertices {
		return invalid("polygon vertex count")
	}
	for _, v := range poly {
		if !coordinates(v.X, v.Z) {
			return invalid("polygon vertex")
		}
	}
	return nil
}

// Entity is the common representation of anything with a place in the site.
// Query and history work on entities regardless of which feature produced
// them. Position is in the site frame.
type Entity struct {
	Kind         EntityKind        `json:"kind"`
	ID           string            `json:"id"`
	Floor        FloorID           `json:"floor,omitempty"`
	Position     geom.Vec3         `json:"position"`
	HasYaw       bool              `json:"has_yaw,omitempty"`
	Yaw          float64           `json:"yaw,omitempty"` // valid when HasYaw
	Extent       Extent            `json:"extent"`
	Class        string            `json:"class,omitempty"`
	Label        string            `json:"label,omitempty"`
	Confidence   float64           `json:"confidence"`
	UncertaintyM float64           `json:"uncertainty_m"`
	ObservedAt   time.Time         `json:"observed_at"`
	ValidUntil   time.Time         `json:"valid_until"`
	Source       DeviceID          `json:"source,omitempty"`
	Attributes   map[string]string `json:"attributes,omitempty"`
}

// Pose2 returns the planar pose when the entity has a heading.
func (e Entity) Pose2() (geom.Pose2, bool) {
	return geom.Pose2{X: e.Position.X, Z: e.Position.Z, Yaw: e.Yaw}, e.HasYaw
}

// Key identifies the entity across updates: "<kind>/<id>".
func (e Entity) Key() string { return string(e.Kind) + "/" + e.ID }

// Validate checks identity and numeric bounds. ObservedAt and ValidUntil may
// be zero for a projection that has not been stamped yet; when both are set
// ValidUntil must not precede ObservedAt.
func (e Entity) Validate() error {
	if !e.Kind.Valid() || e.ID == "" || len(e.ID) > 2*MaxIDLength+1 || (e.Floor != "" && !validID(string(e.Floor))) {
		return invalid("entity identity")
	}
	if !coordinates(e.Position.X, e.Position.Y, e.Position.Z) || !finite(e.Yaw) || (!e.HasYaw && e.Yaw != 0) ||
		!unit(e.Confidence) || !finite(e.UncertaintyM) || e.UncertaintyM < 0 || e.UncertaintyM > MaxCoordinateM {
		return invalid("entity geometry")
	}
	if !validText(e.Class) || !validText(e.Label) || (e.Source != "" && !validID(string(e.Source))) || len(e.Attributes) > 32 {
		return invalid("entity description")
	}
	for k, v := range e.Attributes {
		if !validID(k) || !validText(v) {
			return invalid("entity attribute")
		}
	}
	if !e.ObservedAt.IsZero() && !e.ValidUntil.IsZero() && e.ValidUntil.Before(e.ObservedAt) {
		return invalid("entity validity interval")
	}
	return e.Extent.check()
}

// CompareEntities orders by kind, id, floor, observation time and source. It
// is a total order on entities that differ in any of those fields.
func CompareEntities(a, b Entity) int {
	return cmp.Or(
		cmp.Compare(a.Kind, b.Kind),
		cmp.Compare(a.ID, b.ID),
		cmp.Compare(a.Floor, b.Floor),
		a.ObservedAt.Compare(b.ObservedAt),
		cmp.Compare(a.Source, b.Source),
	)
}

// SortEntities sorts in place by CompareEntities; equal entities keep their order.
func SortEntities(es []Entity) {
	sort.SliceStable(es, func(i, j int) bool { return CompareEntities(es[i], es[j]) < 0 })
}
