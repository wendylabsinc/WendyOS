package grid

import (
	"fmt"
	"maps"
	"sort"

	"github.com/wendylabsinc/wendy/go/world/internal/spatial"
)

// tileStep is one MapTile step in units of LogOddsStep.
const tileStep = 5

// TileLogOddsScale is the MapTile.LogOddsScale of every tile this package
// writes: one int8 step is 0.05 log-odds, so a tile holds ±6.35.
//
// With the default Config (Hit 0.85, Miss 0.40, clamps −2 and 3.5) every
// cell is a multiple of 0.05 inside that span, so tiles are lossless:
// FromTiles of Tiles is Equal to the grid, and replicas that hold equal cells
// publish tiles with equal digests whether they integrated scans or merged
// tiles. Any Config whose Hit, Miss, Min and Max are multiples of 0.05 within
// ±6.35 keeps that property. Otherwise a cell rounds to the nearest step and
// saturates at ±6.35; a known cell never becomes 0, the unknown value.
const TileLogOddsScale = tileStep * LogOddsStep

// TileOf is the index of the tile of side cells that holds c: floor(c/side)
// on each axis, the tiling spatial.TileOrigin describes.
func TileOf(c Cell, side int) (ix, iz int32) {
	floor := func(v int32) int32 {
		q := v / int32(side)
		if v%int32(side) < 0 {
			q--
		}
		return q
	}
	return floor(c.IX), floor(c.IZ)
}

func checkTile(ix, iz int32, side int) error {
	if side < 1 || side > spatial.MaxTileSide {
		return fmt.Errorf("%w: tile side %d", ErrInvalid, side)
	}
	if limit := int32(maxIndex / side); ix < -limit || ix >= limit || iz < -limit || iz >= limit {
		return fmt.Errorf("%w: tile (%d, %d) beyond the addressable map", ErrInvalid, ix, iz)
	}
	return nil
}

// Tile exports the side × side cells of tile (ix, iz) as a sealed MapTile of
// the given floor at TileLogOddsScale. elevation is the floor's World Y and
// versions the per-source version vector of the evidence in the grid; both
// are the caller's knowledge. A tile without a known cell is exported too,
// which is how a cleared area is published.
func (g *Grid) Tile(floor spatial.FloorID, ix, iz int32, side int, elevation float64, versions map[string]uint64) (spatial.MapTile, error) {
	if err := checkTile(ix, iz, side); err != nil {
		return spatial.MapTile{}, err
	}
	t := spatial.MapTile{
		Floor: floor, IX: ix, IZ: iz, Side: side, CellSizeM: g.cfg.CellSize, OriginXZ: spatial.TileOrigin(ix, iz, side, g.cfg.CellSize),
		LogOddsScale: TileLogOddsScale, Cells: make([]byte, side*side), Versions: maps.Clone(versions),
	}
	cu := cursor{g: g}
	x0, z0 := ix*int32(side), iz*int32(side)
	for z := 0; z < side; z++ {
		for x := 0; x < side; x++ {
			raw := cu.get(x0+int32(x), z0+int32(z))
			if raw == 0 {
				continue
			}
			// Round half away from zero, saturate, and keep known cells known.
			v := (raw + tileStep/2) / tileStep
			if raw < 0 {
				v = (raw - tileStep/2) / tileStep
			}
			if v == 0 {
				v = raw / max(raw, -raw)
			}
			t.Cells[z*side+x] = byte(int8(min(max(v, -127), 127)))
		}
	}
	t.Seal(elevation)
	if err := t.Validate(); err != nil {
		return spatial.MapTile{}, err
	}
	return t, nil
}

// Tiles exports every tile of side cells that holds a known cell, ordered by
// (IZ, IX). See Tile.
func (g *Grid) Tiles(floor spatial.FloorID, side int, elevation float64, versions map[string]uint64) ([]spatial.MapTile, error) {
	if err := checkTile(0, 0, side); err != nil {
		return nil, err
	}
	type index struct{ x, z int32 }
	held := map[index]bool{}
	g.Each(func(c Cell, _ float64) bool {
		ix, iz := TileOf(c, side)
		held[index{ix, iz}] = true
		return true
	})
	order := make([]index, 0, len(held))
	for i := range held {
		order = append(order, i)
	}
	sort.Slice(order, func(i, j int) bool {
		if order[i].z != order[j].z {
			return order[i].z < order[j].z
		}
		return order[i].x < order[j].x
	})
	out := make([]spatial.MapTile, 0, len(order))
	for _, i := range order {
		t, err := g.Tile(floor, i.x, i.z, side, elevation, versions)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, nil
}

// SetTile overwrites the cells the tile covers with its content: a zero tile
// cell makes the grid cell unknown. The tile must be valid and have the
// grid's cell size (ErrIncompatible); which floor it belongs to is the
// caller's to check. Values are rounded to LogOddsStep and clamped to the
// grid's range, and a known tile cell stays known.
func (g *Grid) SetTile(t spatial.MapTile) error {
	if err := t.Validate(); err != nil {
		return err
	}
	if t.CellSizeM != g.cfg.CellSize {
		return fmt.Errorf("%w: tile cells of %g m in a grid of %g m", ErrIncompatible, t.CellSizeM, g.cfg.CellSize)
	}
	if err := checkTile(t.IX, t.IZ, t.Side); err != nil {
		return err
	}
	cu := cursor{g: g}
	x0, z0 := t.IX*int32(t.Side), t.IZ*int32(t.Side)
	var flipped []Cell
	for i, b := range t.Cells {
		c := Cell{x0 + int32(i%t.Side), z0 + int32(i/t.Side)}
		raw := 0
		if v := int(int8(b)); v != 0 {
			raw = toRaw(float64(v) * t.LogOddsScale)
			if raw == 0 {
				raw = v / max(v, -v)
			}
			raw = min(max(raw, g.lo), g.hi)
		}
		old := cu.get(c.IX, c.IZ)
		if raw == old {
			continue
		}
		cu.chunk(c.IX, c.IZ, true)[local(c.IX, c.IZ)] = int16(raw)
		if (old >= g.occ) != (raw >= g.occ) {
			flipped = append(flipped, c)
		}
	}
	g.stale(flipped...)
	return nil
}

// FromTiles builds a grid from the tiles of one floor, as a consumer of
// published map tiles does. Tiles of different floors or of another cell
// size than cfg's are ErrIncompatible. A later tile overwrites an earlier one
// with the same index.
func FromTiles(cfg Config, tiles []spatial.MapTile) (*Grid, error) {
	g, err := New(cfg)
	if err != nil {
		return nil, err
	}
	for _, t := range tiles {
		if t.Floor != tiles[0].Floor {
			return nil, fmt.Errorf("%w: tiles of floors %q and %q", ErrIncompatible, tiles[0].Floor, t.Floor)
		}
		if err := g.SetTile(t); err != nil {
			return nil, err
		}
	}
	return g, nil
}

// Sum adds grids cell by cell in exact integer arithmetic and clamps each
// total once, to cfg's range. Unlike a chain of Merge calls, whose clamp
// engages at every step, the result does not depend on the order of the
// grids. The cell sizes must equal cfg's.
//
// This is the order-independent fusion of evidence from several sources:
// keep one grid per source, replace a source's grid when a newer version of
// it arrives (which makes the fusion idempotent), and Sum them on read.
func Sum(cfg Config, grids ...*Grid) (*Grid, error) {
	out, err := New(cfg)
	if err != nil {
		return nil, err
	}
	totals := map[chunkKey]*[chunkCells]int32{}
	for _, g := range grids {
		if err := out.compatible(g); err != nil {
			return nil, err
		}
		for k, ch := range g.chunks {
			acc := totals[k]
			if acc == nil {
				acc = new([chunkCells]int32)
				totals[k] = acc
			}
			for i, v := range ch {
				acc[i] += int32(v)
			}
		}
	}
	for k, acc := range totals {
		var ch *chunk
		for i, v := range acc {
			if v == 0 {
				continue
			}
			if ch == nil {
				ch = new(chunk)
				out.chunks[k] = ch
			}
			ch[i] = int16(min(max(int(v), out.lo), out.hi))
		}
	}
	return out, nil
}
