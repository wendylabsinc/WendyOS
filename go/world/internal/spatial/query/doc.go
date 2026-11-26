// Package query answers spatial questions over the entities, occupancy maps
// and regions the spatial layer holds: the k nearest, everything within a
// radius, a box or a region, what is visible from a place, how far something
// is along the floor, how entities relate to each other, and what was seen
// when.
//
// A Query is a typed conjunction of filters with an ordering and a limit. It
// marshals to JSON and has a small text form (Parse, Query.String):
//
//	nearest 3 kind=object class=chair to rover-1 within 5m visible
//	find kind=track in region kitchen since now-30s order time desc limit 10
//
// An Index is an immutable snapshot built by New from a World: a slice of
// spatial.Entity values (spatial.ComponentEntities projects them from
// published components) and, optionally, one occupancy grid per floor. Named
// regions are the region entities in that slice. Index.Run answers a Query;
// it is safe for concurrent use. When the entities change, Index.With builds
// the next snapshot over the same grids: it is linear, takes about a
// millisecond for 10,000 entities, and keeps what was derived from the grids.
//
// # Semantics
//
// Rows. Every element of World.Entities is one row. Rows are not merged:
// pass one row per entity key (the latest), or resolve duplicates first.
//
// Floors. All geometry is planar (x, z) within one floor. A query with an
// origin, or with a relation to an entity, answers only for that entity's
// floor: distance, sight and path are not defined across floors here. A box or
// a polygon without a floor applies on every floor.
//
// Distance is geom.Vec2.Dist between positions, centre to centre. Footprints
// (Extent) matter only to "inside", to visibility and to path distance.
//
// Self. An entity named as the origin or as the target of a relation is never
// part of the answer.
//
// Visibility. An entity is visible from a place when the straight line
// between them crosses no cell within Config.SightCells cells of an Occupied
// cell (grid.Raycast over the map widened by that much: a hole in a wall
// narrower than the map resolves is not a window), after leaving out the
// part of the line inside either footprint plus Config.SurfaceCells +
// Config.SightCells map cells (a mapped surface is about that thick, and an
// entity that is in the map must not hide itself). Unknown cells do not
// block sight.
//
// Path distance is the shortest 8-connected path over traversable cells of
// the floor's grid (Free, and at least Config.ClearanceM, the radius of what
// travels, from an Occupied cell; diagonal steps never cut a corner), from
// the origin to the entity, with a straight leg at each end joining the
// position to a traversable cell: its own cell, or one within
// Config.SurfaceCells cells of its footprint (plus the clearance). Lengths
// are summed in exact integer millionths of a cell, so the result does not
// depend on evaluation order. An 8-connected path is at most 8.3 % longer
// than the true shortest path.
//
// Sight and path are statements about the map. Where a mapped wall has a
// hole wider than SightCells or ClearanceM close, a sight line and a path go
// through it.
//
// Time. "seen since T" keeps ObservedAt >= T, "seen before T" keeps
// ObservedAt < T and "valid at T" keeps ObservedAt <= T < ValidUntil (a zero
// ValidUntil never expires). A row without an observation time fails every
// temporal filter: its age is unknown. The package never reads a clock; the
// caller passes the instant.
//
// Order and ties. Results are ordered by the requested key (the default is
// distance when the query has an origin and entity key otherwise), then by
// spatial.CompareEntities, then by row index in World.Entities. "nearest K"
// selects with the same rule, so two entities at exactly the same distance
// are taken in CompareEntities order. Descending reverses the key only, never
// the tie-break.
//
// Refusals. Run returns a typed error instead of a guess when a reference
// names no entity or several, when a floor cannot be determined, when sight
// or path is asked for a floor without a grid, or when a named region is
// unknown.
//
// Cost. Over 10,000 rows on one floor the queries the index narrows (nearest,
// within, box, polygon, region, near, inside, same region) answer in well
// under a millisecond, and a query it cannot narrow reads every row in about
// 3 ms. Sight costs one ray per row that passes the other filters. A path
// query floods the floor's map from the origin: its cost follows the area
// flooded, not the number of rows (about 40 ms for 400,000 cells, 5 ms when
// PathWithinM stops it at 10 m).
//
// The package derives no state of its own and publishes no component: a
// result is an answer over records other features published, and each Match
// carries the entity with its own uncertainty plus the uncertainty of the
// reported distance.
package query
