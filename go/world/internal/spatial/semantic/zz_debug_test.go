package semantic

import (
	"image"
	"image/color"
	"image/png"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/spatial"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/geom"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/grid"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/spatialsim"
)

func TestDebugDump(t *testing.T) {
	dir := os.Getenv("SEM_DUMP")
	if dir == "" {
		t.Skip()
	}
	seed, _ := strconv.ParseUint(os.Getenv("SEM_SEED"), 10, 64)
	s, err := spatialsim.Office(seed, spatialsim.Options{})
	if err != nil {
		t.Fatal(err)
	}
	ref, _ := s.Reference(spatialsim.DefaultReferenceConfig())
	tr := s.Truth()
	for _, fl := range []spatial.FloorID{spatialsim.FloorGround, spatialsim.FloorUpper, spatialsim.FloorYard} {
		start := time.Now()
		g, err := ref.Grid(fl, s.At(0), s.At(240*time.Second))
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("%s counts %+v grid in %v", fl, g.Counts(), time.Since(start))
		start = time.Now()
		seenO := map[[4]int]bool{}
		debugOpenings = func(a, b geom.Vec2, la, lb bool) {
			k := [4]int{int(a.X * 5), int(a.Z * 5), int(b.X * 5), int(b.Z * 5)}
			if !seenO[k] {
				seenO[k] = true
				t.Logf("  barrier (%.2f,%.2f)-(%.2f,%.2f) long %v %v", a.X, a.Z, b.X, b.Z, la, lb)
			}
		}
		sc, err := Segment(Config{}, Input{Floor: fl, Map: g})
		if err != nil {
			t.Log(err)
			lay, _ := classify(DefaultConfig(), g, nil)
			sc = &Scene{layer: lay, region: make([]int32, lay.w*lay.h)}
		}
		t.Logf("%s segment in %v: %d rooms, %d doors, unexplained %.3f", fl, time.Since(start), len(sc.Rooms), len(sc.Doors), sc.Unexplained)
		for _, rm := range sc.Rooms {
			c := rm.Region.Point()
			tru, _ := tr.RoomAt(fl, geom.Vec2{X: c.X, Z: c.Z})
			t.Logf("  %s area %.1f inscribed %.2f enclosure %.2f centre (%.1f,%.1f) truth %s votes %v doors %v", rm.Region.ID, rm.Region.AreaM2, rm.InscribedM, rm.Enclosure, c.X, c.Z, tru.ID, rm.Votes, rm.Region.Doors)
		}
		for _, d := range sc.Doors {
			t.Logf("  door %s %s %s w %.2f at (%.2f,%.2f) %s-%s", d.ID, d.Kind, d.State, d.WidthM, d.PositionM[0], d.PositionM[2], d.A.Region, d.B.Region)
		}
		for _, rm := range tr.Rooms() {
			if rm.Floor != fl {
				continue
			}
			n, known := 0, 0
			lo, hi := rm.Polygon[0], rm.Polygon[0]
			for _, p := range rm.Polygon {
				lo = geom.Vec2{X: min(lo.X, p.X), Z: min(lo.Z, p.Z)}
				hi = geom.Vec2{X: max(hi.X, p.X), Z: max(hi.Z, p.Z)}
			}
			for z := lo.Z + 0.025; z < hi.Z; z += 0.05 {
				for x := lo.X + 0.025; x < hi.X; x += 0.05 {
					if p := (geom.Vec2{X: x, Z: z}); geom.PointInPolygon(p, rm.Polygon) {
						n++
						if g.Known(g.CellAt(p)) {
							known++
						}
					}
				}
			}
			t.Logf("  truth %s (%s) portal %q coverage %.2f", rm.ID, rm.Label, rm.Portal, float64(known)/float64(n))
		}
		l := sc.layer
		palette := []color.RGBA{{230, 25, 75, 255}, {60, 180, 75, 255}, {255, 225, 25, 255}, {0, 130, 200, 255}, {245, 130, 48, 255}, {145, 30, 180, 255}, {70, 240, 240, 255}, {240, 50, 230, 255}, {210, 245, 60, 255}, {250, 190, 190, 255}, {0, 128, 128, 255}, {230, 190, 255, 255}}
		const z = 3
		img := image.NewRGBA(image.Rect(0, 0, l.w*z, l.h*z))
		for i := range l.class {
			c := color.RGBA{128, 128, 128, 255}
			if k := sc.region[i]; k != 0 {
				c = palette[int(k-1)%len(palette)]
			}
			switch l.class[i] {
			case ClassWall:
				c = color.RGBA{0, 0, 0, 255}
			case ClassDoor:
				c = color.RGBA{255, 255, 255, 255}
			case ClassFurniture:
				c = color.RGBA{90, 60, 30, 255}
			case ClassFree:
				if sc.region[i] == 0 {
					c = color.RGBA{200, 200, 200, 255}
				}
				if !l.interior[i] {
					c = color.RGBA{255, 0, 255, 255}
				}
			}
			if lo := g.LogOdds(l.gridCell(i)); os.Getenv("SEM_CONTESTED") != "" && g.Known(l.gridCell(i)) && l.state[i] != grid.Occupied {
				c = color.RGBA{255, 255, 255, 255}
				if lo > -1.0 {
					c = color.RGBA{255, 0, 0, 255}
				}
			}
			for d := 0; d < z*z; d++ {
				img.Set((i%l.w)*z+d%z, (i/l.w)*z+d/z, c)
			}
		}
		f, _ := os.Create(dir + "/sem-" + string(fl) + ".png")
		png.Encode(f, img)
		f.Close()
		_ = grid.Free
	}
}
