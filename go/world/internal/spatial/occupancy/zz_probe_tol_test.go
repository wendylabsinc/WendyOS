package occupancy

import (
	"os"
	"testing"

	"github.com/wendylabsinc/wendy/go/world/internal/spatial"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/geom"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/grid"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/spatialsim"
)

// Tolerant and class-balanced scoring of the same fused map, plus the cells
// the acceptance test leaves unscored.
func TestZZProbeTolerant(t *testing.T) {
	if os.Getenv("PROBE") == "" {
		t.Skip()
	}
	for _, k := range []probeKnobs{{name: "baseline"}, {name: "shift-all-0.5", shift: geom.Vec2{X: 0.5}}, {name: "shift-all-0.2", shift: geom.Vec2{X: 0.2}}, {name: "shift-rover1-0.5", shift: geom.Vec2{X: 0.5}, shiftDev: spatialsim.Rover1}} {
		fused, s := probeBuild(t, 1, k)
		tr := s.Truth()
		cell := fused.Config().Grid.CellSize
		side := int32(fused.Config().TileSide)
		sibling := map[spatial.FloorID]spatial.FloorID{"ground": "yard", "yard": "ground"}
		for _, floor := range fused.Floors() {
			g := mustGrid(t, fused, floor)
			raster, _ := tr.Occupancy(floor, s.At(acceptanceWindow), cell)
			var sib *spatialsim.Raster
			if o, ok := sibling[floor]; ok {
				r2, _ := tr.Occupancy(o, s.At(acceptanceWindow), cell)
				sib = &r2
			}
			inR := func(r *spatialsim.Raster, p geom.Vec2) (int, bool) {
				ix, iz := int((p.X-r.Origin.X)/cell), int((p.Z-r.Origin.Z)/cell)
				if p.X < r.Origin.X || p.Z < r.Origin.Z || ix >= r.Width || iz >= r.Height {
					return 0, false
				}
				return iz*r.Width + ix, true
			}
			uniq := map[grid.Cell]bool{}
			outside, outsideSib, sibAgree, sibScored, nowhere := 0, 0, 0, 0, 0
			for _, l := range fused.Delta(nil) {
				if l.Floor != floor {
					continue
				}
				for i, b := range l.Cells {
					if b == 0 {
						continue
					}
					c := grid.Cell{IX: l.IX*side + int32(i)%side, IZ: l.IZ*side + int32(i)/side}
					if uniq[c] {
						continue
					}
					uniq[c] = true
					p := g.Center(c)
					if _, ok := inR(&raster, p); ok {
						continue
					}
					outside++
					if sib == nil {
						nowhere++
						continue
					}
					j, ok := inR(sib, p)
					if !ok || sib.Cells[j] == spatialsim.CellUnknown {
						nowhere++
						continue
					}
					outsideSib++
					sibScored++
					got := g.State(c)
					if (sib.Cells[j] == spatialsim.CellOccupied && got == grid.Occupied) || (sib.Cells[j] == spatialsim.CellFree && got == grid.Free) {
						sibAgree++
					}
				}
			}
			// Tolerant occupied scores over observed cells of the floor's own raster.
			near := func(ix, iz, rad int, pred func(ix, iz int) bool) bool {
				for dz := -rad; dz <= rad; dz++ {
					for dx := -rad; dx <= rad; dx++ {
						x, z := ix+dx, iz+dz
						if x >= 0 && z >= 0 && x < raster.Width && z < raster.Height && pred(x, z) {
							return true
						}
					}
				}
				return false
			}
			mapOcc := func(ix, iz int) bool { return g.StateAt(raster.Centre(ix, iz)) == grid.Occupied }
			truthOcc := func(ix, iz int) bool { return raster.Cells[iz*raster.Width+ix] == spatialsim.CellOccupied }
			truthWall := func(ix, iz int) bool {
				c := raster.Classes[iz*raster.Width+ix]
				return c == spatialsim.ClassWall || c == spatialsim.ClassDoor
			}
			var wallSeen, wallHit0, wallHit1, occMap, occOK0, occOK1, freeT, freeOK, occT, occOK int
			for iz := 0; iz < raster.Height; iz++ {
				for ix := 0; ix < raster.Width; ix++ {
					c := g.CellAt(raster.Centre(ix, iz))
					if !uniq[c] || raster.Cells[iz*raster.Width+ix] == spatialsim.CellUnknown {
						continue
					}
					if truthWall(ix, iz) {
						wallSeen++
						if mapOcc(ix, iz) {
							wallHit0++
						}
						if near(ix, iz, 1, mapOcc) {
							wallHit1++
						}
					}
					if mapOcc(ix, iz) {
						occMap++
						if truthOcc(ix, iz) {
							occOK0++
						}
						if near(ix, iz, 1, truthOcc) {
							occOK1++
						}
					}
					if truthOcc(ix, iz) {
						occT++
						if mapOcc(ix, iz) {
							occOK++
						}
					} else {
						freeT++
						if g.StateAt(raster.Centre(ix, iz)) == grid.Free {
							freeOK++
						}
					}
				}
			}
			f := func(a, b int) float64 { return float64(a) / float64(max(b, 1)) }
			t.Logf("PROBE %s %s: seen %d, outside own raster %d (scorable on sibling %d: agreement %.4f; nowhere %d) | wall recall strict %.3f, within 1 cell %.3f (%d walls) | map-occupied precision strict %.3f, within 1 cell %.3f (%d) | balanced accuracy %.3f (free %.4f, occupied %.4f)",
				k.name, floor, len(uniq), outside, outsideSib, f(sibAgree, sibScored), nowhere, f(wallHit0, wallSeen), f(wallHit1, wallSeen), wallSeen, f(occOK0, occMap), f(occOK1, occMap), occMap,
				(f(freeOK, freeT)+f(occOK, occT))/2, f(freeOK, freeT), f(occOK, occT))
		}
	}
}
