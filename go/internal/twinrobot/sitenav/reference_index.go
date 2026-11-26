package sitenav

import (
	"cmp"
	"errors"
	"math"
	"slices"
)

type referenceCell struct{ X, Y, Z int }
type surfaceReferenceIndex struct {
	points    []Surface
	neighbors map[referenceCell]referenceSpan
	indices   []uint16
	expanded  bool
}

type referenceSpan struct{ offset, count uint32 }

func surfaceReferenceKey(p Vertex) referenceCell {
	return referenceCell{int(math.Floor(p.X / .2)), int(math.Floor(p.Y / .2)), int(math.Floor(p.Z / .2))}
}

// Expand occupied neighborhoods once so each candidate needs one map probe.
// Store uint16 indices, not 27 copies of each Surface. The existing 3000-point
// limit bounds expansion to 81000 indices. Sparse maps fall back to occupied
// cells once neighborhood keys exceed four per point plus 128. Global cell Z/Y/X ordering equals
// the old query's dz/dy/dx traversal; original indices break same-cell ties.
func newSurfaceReferenceIndex(points []Surface) (surfaceReferenceIndex, error) {
	if len(points) > 3000 {
		return surfaceReferenceIndex{}, errors.New("Localization map exceeds its bounded sample budget.")
	}
	if len(points) == 0 {
		return surfaceReferenceIndex{}, nil
	}
	keys := make([]referenceCell, len(points))
	order := make([]uint16, len(points))
	for i, p := range points {
		if !(Point{p.Position.X, p.Position.Y}).Valid() || !finite(p.Position.Z) || math.Abs(p.Position.Z) > 1000 || !finite(p.Direction.X) || !finite(p.Direction.Y) || (!p.Horizontal && math.Abs(math.Hypot(p.Direction.X, p.Direction.Y)-1) > .05) {
			return surfaceReferenceIndex{}, errors.New("Invalid saved surface map.")
		}
		keys[i], order[i] = surfaceReferenceKey(p.Position), uint16(i)
	}
	slices.SortFunc(order, func(a, b uint16) int {
		ka, kb := keys[a], keys[b]
		if c := cmp.Compare(ka.Z, kb.Z); c != 0 {
			return c
		}
		if c := cmp.Compare(ka.Y, kb.Y); c != 0 {
			return c
		}
		if c := cmp.Compare(ka.X, kb.X); c != 0 {
			return c
		}
		return cmp.Compare(a, b)
	})
	index := surfaceReferenceIndex{points: slices.Clone(points), neighbors: make(map[referenceCell]referenceSpan), expanded: true}
	// Count first, then pack all neighborhoods into one backing array. Even a
	// spatially sparse map has no per-cell backing-array allocation.
	visit := func(key referenceCell, add func(referenceCell)) {
		for dz := -1; dz <= 1; dz++ {
			for dy := -1; dy <= 1; dy++ {
				for dx := -1; dx <= 1; dx++ {
					add(referenceCell{key.X - dx, key.Y - dy, key.Z - dz})
				}
			}
		}
	}
	for _, key := range keys {
		visit(key, func(query referenceCell) { span := index.neighbors[query]; span.count++; index.neighbors[query] = span })
		if len(index.neighbors) > 4*len(points)+128 {
			// Stop before constructing a large sparse neighborhood map. Pack only
			// occupied cells, retaining the original 27-probe query order.
			index.expanded = false
			index.neighbors = make(map[referenceCell]referenceSpan, len(points))
			index.indices = order
			for offset, i := range order {
				key := keys[i]
				span := index.neighbors[key]
				if span.count == 0 {
					span.offset = uint32(offset)
				}
				span.count++
				index.neighbors[key] = span
			}
			return index, nil
		}
	}
	var total uint32
	for key, span := range index.neighbors {
		span.offset = total
		total += span.count
		span.count = 0
		index.neighbors[key] = span
	}
	index.indices = make([]uint16, total)
	for _, i := range order {
		visit(keys[i], func(query referenceCell) {
			span := index.neighbors[query]
			index.indices[span.offset+span.count] = i
			span.count++
			index.neighbors[query] = span
		})
	}

	return index, nil
}
func (index surfaceReferenceIndex) distance(p Vertex, direction Point, horizontal bool) (float64, bool) {
	best, stable := .0625, false
	key := surfaceReferenceKey(p)
	if !index.expanded {
		for dz := -1; dz <= 1; dz++ {
			for dy := -1; dy <= 1; dy++ {
				for dx := -1; dx <= 1; dx++ {
					span := index.neighbors[referenceCell{key.X + dx, key.Y + dy, key.Z + dz}]
					for _, i := range index.indices[span.offset : span.offset+span.count] {
						v := index.points[i]
						if v.Horizontal != horizontal || (!horizontal && math.Abs(v.Direction.X*direction.X+v.Direction.Y*direction.Y) < .966) {
							continue
						}
						distance := (v.Position.X-p.X)*(v.Position.X-p.X) + (v.Position.Y-p.Y)*(v.Position.Y-p.Y) + (v.Position.Z-p.Z)*(v.Position.Z-p.Z)
						if distance < best {
							best, stable = distance, v.Stable
						}
					}
				}
			}
		}
		return best, stable
	}
	span := index.neighbors[key]
	for _, i := range index.indices[span.offset : span.offset+span.count] {
		v := index.points[i]
		if v.Horizontal != horizontal || (!horizontal && math.Abs(v.Direction.X*direction.X+v.Direction.Y*direction.Y) < .966) {
			continue
		}
		distance := (v.Position.X-p.X)*(v.Position.X-p.X) + (v.Position.Y-p.Y)*(v.Position.Y-p.Y) + (v.Position.Z-p.Z)*(v.Position.Z-p.Z)
		if distance < best {
			best, stable = distance, v.Stable
		}
	}
	return best, stable
}
