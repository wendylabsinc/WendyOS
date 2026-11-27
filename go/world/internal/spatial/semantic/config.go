package semantic

import (
	"errors"
	"fmt"
	"math"
	"time"
)

var (
	// ErrConfig reports a Config outside its documented ranges.
	ErrConfig = errors.New("semantic: invalid config")
	// ErrInsufficient reports a map that does not hold a room-sized stretch
	// of observed free space: there is nothing to segment.
	ErrInsufficient = errors.New("semantic: insufficient map")
	// ErrUnknownFloor reports a floor the Segmenter holds no map for.
	ErrUnknownFloor = errors.New("semantic: unknown floor")
)

// Config holds every threshold and noise assumption of the package. A zero
// field takes its default; DefaultConfig lists them. Lengths are metres,
// areas square metres, angles radians.
type Config struct {
	// WallMinLength is the shortest straight run of occupied cells taken
	// for a wall line. Default 1.5: longer than a door leaf, shorter than
	// any wall section between two doors worth mapping.
	WallMinLength float64
	// LineGap is the longest hole tolerated inside one straight run, which
	// covers lidar dropouts and beam spacing at range. Default 0.25.
	LineGap float64
	// LineFill is the least share of a run's length that must be occupied.
	// Default 0.5.
	LineFill float64
	// AngleStep is the angular resolution of the line search. Default 2°:
	// a line at most 1° off a wall stays inside a three-cell bin for more
	// than a hundred cells.
	AngleStep float64
	// ClearProbability is the occupancy probability up to which a cell that
	// is not occupied is clear. An observed cell above it is contested.
	// Default 0.25.
	ClearProbability float64
	// SettledNeighbours is how many of the eight neighbours of a contested
	// cell must sit at the map's clear limit for the cell to count as hit.
	// Where the neighbours were observed so often that their evidence
	// saturated, a cell that is still contested has collected hits; in a
	// fan of sparse beams nothing has settled and a contested cell means
	// nothing. Hit cells are wall evidence and no room space. Default 3.
	SettledNeighbours int
	// BeamGap is the widest unobserved streak between two beams that is
	// still room space. Default 0.10.
	BeamGap float64
	// WallThickness is how far from a wall line an occupied cell still
	// belongs to the wall: the blur of range noise and pose error across
	// the cell lattice. Default 0.10.
	WallThickness float64
	// FurnitureMinDepth and FurnitureMaxDepth bound how deep a piece of
	// furniture is behind its face. A straight run with observed space on
	// one side and, on the other, unobserved cells for a depth in this
	// range before the map shows something again is the face of a piece of
	// furniture and not a wall: a wall seen from both sides is thinner, and
	// behind an outer wall nothing is observed at all. Defaults 0.3 and 1.5.
	FurnitureMinDepth, FurnitureMaxDepth float64
	// FurnitureShare is the share of a straight run that must look like a
	// furniture face for the run to be furniture. Default 0.5.
	FurnitureShare float64
	// FlankReach is how far past the two ends of a run the wall behind it is
	// looked for. Furniture that stands against an outer wall hides the wall
	// behind itself, but the wall shows again on either side. Default 1.
	FlankReach float64
	// FurnitureMinArea and FurnitureMaxArea bound an island of cells that
	// are not room space, surrounded by room space, that is taken for a
	// piece of furniture standing in the room and counted to it. Defaults
	// 0.1 and 6.
	FurnitureMinArea, FurnitureMaxArea float64
	// DoorMinWidth and DoorMaxWidth bound a door. A narrower passage is a
	// leak in a noisy wall and no portal; a wider one is an opening.
	// Defaults 0.5 and 1.5.
	DoorMinWidth, DoorMaxWidth float64

	// OpeningMax is the widest gap between two wall ends on one line that
	// is closed as a region boundary. The distance transform has no saddle
	// at an opening wider than the room behind it; the wall line still
	// shows it. Default 5.
	OpeningMax float64
	// TipClearance is the radius around a wall end that must hold no other
	// structure for the end to be the jamb of a door or opening. A wall
	// that ends in a corner is not interrupted, so two corners facing each
	// other across a corridor are no opening. It must exceed twice
	// WallThickness. Default 0.3.
	TipClearance float64
	// MergeRatio decides whether two watershed basins are one room: they
	// merge when the passage between them is at least this fraction of the
	// width of the narrower basin. Default 0.8: door and room differ by
	// much more, ridge noise and furniture by much less.
	MergeRatio float64
	// MinRoomArea is the smallest region kept; a smaller basin joins the
	// neighbour it shares the longest boundary with. Default 2.
	MinRoomArea float64
	// MinEnclosure is the least share of a region's outline that must be
	// structure or a passage to another region. A region below it borders
	// mostly on unobserved space: a view through a doorway into a room
	// nobody mapped, which is reported as no room rather than as a guess.
	// Default 0.75.
	MinEnclosure float64
	// PolygonTolerance is the simplification tolerance of a region outline.
	// Default 0.10.
	PolygonTolerance float64

	// RoomTypes is the class-to-room-type table: an object of a class adds
	// each listed weight to that room type's votes in the region it stands
	// in. A row should sum to at most 1, so one object is one vote. Classes
	// without a row (robots, people) do not vote. Nil takes
	// DefaultRoomTypes.
	RoomTypes map[string]map[string]float64
	// MinVotes is the least total vote weight a region needs to be
	// labelled. Default 1: one confirmed object of a listed class.
	MinVotes float64
	// MinShare is the share of the votes the winning room type must exceed.
	// Default 0.5, a strict majority; a tie stays unlabelled.
	MinShare float64
	// CorridorElongation and CorridorVotes add a geometric vote: a region
	// whose area is at least CorridorElongation times the square of its
	// widest inscribed circle's diameter gets CorridorVotes for
	// CorridorType. Corridors hold no furniture to vote for them.
	// Defaults 4, 1 and "corridor"; CorridorVotes < 0 disables the vote.
	CorridorElongation, CorridorVotes float64
	CorridorType                      string
	// SnapRadius is how far an object that falls on a wall or unobserved
	// cell is moved to reach a region. An object is placed at its centre,
	// which lies inside it: half the depth of deep furniture plus
	// MaxPlacementSigma. Default 0.75.
	SnapRadius float64

	// FootPixelSigma is the 1σ of an object detection's foot pixel.
	// Default 2 px.
	FootPixelSigma float64
	// MaxPlacementSigma drops a detection whose foot pixel constrains its
	// floor position worse than this, which happens at grazing angles.
	// Default 0.25.
	MaxPlacementSigma float64
	// ObjectRadius is the distance within which detections are fused into
	// one object. Default 0.75, three MaxPlacementSigma.
	ObjectRadius float64
	// MinObservations is the number of detections that confirms an object.
	// Default 5: a false positive does not repeat in one place.
	MinObservations int
	// MinClassShare is the share of an object's detections a class needs
	// to be reported for it. Default 0.25, well above the detector's
	// confusion rate; two classes pass when two objects stand together.
	MinClassShare float64
	// Probation is how long an unconfirmed object is kept without a new
	// detection. Default 30 s.
	Probation time.Duration
	// FloorTolerance is the elevation difference within which an object
	// belongs to a floor. Default 0.3.
	FloorTolerance float64
	// MaxPoseAge is the oldest device pose used to place a mobile camera's
	// detections. Default 100 ms.
	MaxPoseAge time.Duration
}

// DefaultRoomTypes is the default class-to-room-type table: the plain reading
// of each class name, split where the name fits more than one kind of room.
func DefaultRoomTypes() map[string]map[string]float64 {
	return map[string]map[string]float64{
		"stove":  {"kitchen": 1},
		"fridge": {"kitchen": 1},
		"desk":   {"office": 1},
		"chair":  {"office": 0.5, "meeting": 0.5},
		"table":  {"meeting": 0.5, "kitchen": 0.5},
		"sofa":   {"lounge": 0.6, "lobby": 0.4},
		"plant":  {"lobby": 0.4, "lounge": 0.3, "office": 0.3},
		"bench":  {"lab": 1},
		"shelf":  {"storage": 0.4, "office": 0.3, "lab": 0.3},
		"crate":  {"storage": 1},
		"barrel": {"storage": 1},
	}
}

// DefaultConfig returns the defaults documented on Config.
func DefaultConfig() Config {
	return Config{
		WallMinLength: 1.5, LineGap: 0.25, LineFill: 0.5, AngleStep: math.Pi / 90,
		ClearProbability: 0.25, SettledNeighbours: 3, BeamGap: 0.10, WallThickness: 0.10,
		FurnitureMinDepth: 0.3, FurnitureMaxDepth: 1.5, FurnitureShare: 0.5, FlankReach: 1, FurnitureMinArea: 0.1, FurnitureMaxArea: 6,
		DoorMinWidth: 0.5, DoorMaxWidth: 1.5,
		OpeningMax: 5, TipClearance: 0.3, MergeRatio: 0.8, MinRoomArea: 2, MinEnclosure: 0.75, PolygonTolerance: 0.10,
		RoomTypes: DefaultRoomTypes(), MinVotes: 1, MinShare: 0.5,
		CorridorElongation: 4, CorridorVotes: 1, CorridorType: "corridor", SnapRadius: 0.75,
		FootPixelSigma: 2, MaxPlacementSigma: 0.25, ObjectRadius: 0.75, MinObservations: 5, MinClassShare: 0.25,
		Probation: 30 * time.Second, FloorTolerance: 0.3, MaxPoseAge: 100 * time.Millisecond,
	}
}

// withDefaults fills zero fields and checks the ranges.
func (c Config) withDefaults() (Config, error) {
	d := DefaultConfig()
	for _, f := range []struct {
		v   *float64
		def float64
	}{
		{&c.WallMinLength, d.WallMinLength}, {&c.LineGap, d.LineGap}, {&c.LineFill, d.LineFill}, {&c.AngleStep, d.AngleStep},
		{&c.ClearProbability, d.ClearProbability}, {&c.BeamGap, d.BeamGap}, {&c.WallThickness, d.WallThickness}, {&c.FurnitureMinDepth, d.FurnitureMinDepth}, {&c.FurnitureMaxDepth, d.FurnitureMaxDepth},
		{&c.FurnitureShare, d.FurnitureShare}, {&c.FlankReach, d.FlankReach}, {&c.FurnitureMinArea, d.FurnitureMinArea}, {&c.FurnitureMaxArea, d.FurnitureMaxArea},
		{&c.DoorMinWidth, d.DoorMinWidth}, {&c.DoorMaxWidth, d.DoorMaxWidth},
		{&c.OpeningMax, d.OpeningMax}, {&c.TipClearance, d.TipClearance},
		{&c.MergeRatio, d.MergeRatio}, {&c.MinRoomArea, d.MinRoomArea}, {&c.MinEnclosure, d.MinEnclosure}, {&c.PolygonTolerance, d.PolygonTolerance},
		{&c.MinVotes, d.MinVotes}, {&c.MinShare, d.MinShare},
		{&c.CorridorElongation, d.CorridorElongation}, {&c.CorridorVotes, d.CorridorVotes}, {&c.SnapRadius, d.SnapRadius},
		{&c.FootPixelSigma, d.FootPixelSigma}, {&c.MaxPlacementSigma, d.MaxPlacementSigma}, {&c.ObjectRadius, d.ObjectRadius},
		{&c.MinClassShare, d.MinClassShare}, {&c.FloorTolerance, d.FloorTolerance},
	} {
		if *f.v == 0 {
			*f.v = f.def
		}
		if math.IsNaN(*f.v) || math.IsInf(*f.v, 0) || (*f.v < 0 && f.v != &c.CorridorVotes) {
			return Config{}, fmt.Errorf("%w: negative or non-finite field", ErrConfig)
		}
	}
	if c.RoomTypes == nil {
		c.RoomTypes = d.RoomTypes
	}
	if c.CorridorType == "" {
		c.CorridorType = d.CorridorType
	}
	if c.SettledNeighbours == 0 {
		c.SettledNeighbours = d.SettledNeighbours
	}
	if c.MinObservations == 0 {
		c.MinObservations = d.MinObservations
	}
	if c.Probation == 0 {
		c.Probation = d.Probation
	}
	if c.MaxPoseAge == 0 {
		c.MaxPoseAge = d.MaxPoseAge
	}
	switch {
	case c.AngleStep > math.Pi/8, c.LineFill > 1, c.ClearProbability >= 0.5, c.FurnitureShare > 1, c.MergeRatio >= 1, c.MinEnclosure > 1, c.MinShare >= 1, c.MinClassShare > 1,
		c.FurnitureMinDepth > c.FurnitureMaxDepth, c.FurnitureMinArea > c.FurnitureMaxArea, c.SettledNeighbours < 0, c.SettledNeighbours > 8, c.DoorMinWidth > c.DoorMaxWidth, c.DoorMaxWidth > c.OpeningMax,
		c.TipClearance <= 2*c.WallThickness, c.MinObservations < 0, c.Probation < 0, c.MaxPoseAge < 0:
		return Config{}, fmt.Errorf("%w: field out of range", ErrConfig)
	}
	for class, row := range c.RoomTypes {
		for kind, w := range row {
			if class == "" || kind == "" || math.IsNaN(w) || w < 0 || w > 1 {
				return Config{}, fmt.Errorf("%w: room type table entry %q -> %q", ErrConfig, class, kind)
			}
		}
	}
	return c, nil
}
