package occupancy

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/spatial"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/grid"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/spatialsim"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

// Design targets of feature 9 (specs/superpowers/specs/2026-11-25-spatial-intelligence-design.md).
const (
	// targetCellAgreement: the fused map agrees with truth on more than this
	// fraction of the cells any robot observed.
	targetCellAgreement = 0.95
	// targetPayloadBytes: a tile payload stays within the component limit.
	targetPayloadBytes = spatial.MaxPayloadBytes
)

const (
	// poseStep is the rate at which the upstream stand-in publishes poses.
	poseStep = 50 * time.Millisecond
	// syncEvery is how often the robots commit and gossip.
	syncEvery = 10 * time.Second
)

// acceptanceSeeds reads SPATIAL_SEEDS (comma-separated unsigned integers).
func acceptanceSeeds(t *testing.T) []uint64 {
	t.Helper()
	env := os.Getenv("SPATIAL_SEEDS")
	if env == "" {
		return []uint64{1}
	}
	var seeds []uint64
	for _, f := range strings.Split(env, ",") {
		seed, err := strconv.ParseUint(strings.TrimSpace(f), 10, 64)
		if err != nil {
			t.Fatalf("SPATIAL_SEEDS: %v", err)
		}
		seeds = append(seeds, seed)
	}
	return seeds
}

// Several robots each integrate their own scans into their own replica, at
// the poses, lidar mounts and clock offsets of the upstream stand-in
// (Site.Reference with its default noise: 2 cm and 0.5 degree poses, 1 cm
// mounts, 3 ms clock offsets). The replicas gossip layers as components
// around a ring, and the fused map is scored against Truth occupancy on
// every cell any robot observed. Truth is used for nothing else.
func TestAcceptanceFleetMap(t *testing.T) {
	for _, seed := range acceptanceSeeds(t) {
		t.Run(fmt.Sprintf("seed-%d", seed), func(t *testing.T) { fleetMap(t, seed) })
	}
}

type robot struct {
	id       spatial.DeviceID
	replica  *Replica
	mapper   *Mapper
	offset   time.Duration // reference lidar clock offset
	nextPose time.Duration // next pose sample, since the epoch
	scans    int
}

func fleetMap(t *testing.T, seed uint64) {
	s, err := spatialsim.Office(seed, spatialsim.Options{})
	if err != nil {
		t.Fatal(err)
	}
	ref, err := s.Reference(spatialsim.DefaultReferenceConfig())
	if err != nil {
		t.Fatal(err)
	}
	// Every lidar that runs from the start: three on the ground floor, one
	// that climbs to the upper floor, one that patrols the yard.
	ids := []spatial.DeviceID{spatialsim.Rover1, spatialsim.Rover2, spatialsim.Rover3, spatialsim.Climber, spatialsim.Ranger}
	robots := map[spatial.DeviceID]*robot{}
	fleet := make([]*robot, len(ids))
	for i, id := range ids {
		r := &robot{id: id, replica: mustReplica(t, DefaultConfig())}
		r.mapper = mustMapper(t, r.replica, id)
		cal, ok := ref.Calibration(id, spatialsim.SensorLidar, s.At(0))
		if !ok {
			t.Fatalf("%s: no reference lidar calibration", id)
		}
		if err := r.mapper.Calibrate(cal); err != nil {
			t.Fatal(err)
		}
		r.offset = cal.TimeOffset
		robots[id], fleet[i] = r, r
	}

	publication := func(source spatial.DeviceID) Publication {
		return Publication{
			Scope: spatial.Scope{Tenant: "tenant", Site: "office", Source: world.Source{Device: string(source), App: "wendy", Service: "spatial", Stream: "occupancy"}},
			Epoch: 1, Frame: spatialsim.SiteFrame, Timing: world.Timing{ClockDomain: spatialsim.ClockDomain, ErrorKnown: true, ErrorBound: 5 * time.Millisecond},
			TTL: time.Hour, AppVersion: "test",
		}
	}
	largestPayload, largestComponent, sent := 0, 0, 0
	// sync commits every robot's new scans and lets each pull from its ring
	// neighbour, layers travelling as components.
	sync := func() {
		t.Helper()
		for _, r := range fleet {
			mustCommit(t, r.mapper)
		}
		for i, to := range fleet {
			from := fleet[(i+1)%len(fleet)]
			for _, l := range from.replica.Delta(to.replica.Summary()) {
				c, err := l.Component(publication(l.Source))
				if err != nil {
					t.Fatal(err)
				}
				whole, _ := json.Marshal(c)
				largestPayload, largestComponent, sent = max(largestPayload, len(c.Payload)), max(largestComponent, len(whole)), sent+1
				if changed, err := to.replica.ApplyComponent(c); err != nil || !changed {
					t.Fatalf("%s <- %s: layer %+v v%d: changed %v, %v", to.id, from.id, l.LayerKey, l.Version, changed, err)
				}
			}
		}
	}

	nextSync := syncEvery
	stream := s.Stream(s.At(0), s.At(acceptanceWindow), spatialsim.Filter{Devices: ids, Kinds: []spatial.SensorKind{spatial.KindLidar2D}})
	for m := range stream {
		scan, ok := m.(spatial.Scan)
		if !ok {
			continue
		}
		r := robots[scan.Device]
		// The localization stand-in publishes the device pose at 20 Hz; the
		// mapper interpolates it at the scan's site time.
		at := scan.Time.Add(-r.offset).Sub(s.Epoch())
		for ; r.nextPose <= at+poseStep; r.nextPose += poseStep {
			if pose, ok := ref.DevicePose(r.id, s.At(r.nextPose)); ok {
				if err := r.mapper.Locate(s.At(r.nextPose), pose); err != nil {
					t.Fatal(err)
				}
			}
		}
		if err := r.mapper.Ingest(scan, s.Input(scan)); err != nil {
			t.Fatal(err)
		}
		r.scans++
		if at >= nextSync {
			sync()
			nextSync += syncEvery
		}
	}
	// A unidirectional ring spreads everything in one lap.
	for range fleet {
		sync()
	}

	for _, r := range fleet {
		st := r.mapper.Stats()
		if st.Uncalibrated+st.Uncertain+st.WrongFrame != 0 || float64(st.Integrated) < 0.95*float64(r.scans) {
			t.Errorf("%s: %d scans, stats %+v", r.id, r.scans, st)
		}
		if r.replica.Digest() != fleet[0].replica.Digest() {
			t.Errorf("%s: replica differs from %s after the final lap", r.id, fleet[0].id)
		}
	}
	fused := fleet[0].replica

	// Score against truth: every cell any source holds evidence for.
	tr := s.Truth()
	cell := fused.Config().Grid.CellSize
	agree, observed, decidedAgree, decided := 0, 0, 0, 0
	for _, floor := range fused.Floors() {
		g := mustGrid(t, fused, floor)
		raster, err := tr.Occupancy(floor, s.At(acceptanceWindow), cell)
		if err != nil {
			t.Fatal(err)
		}
		// seen marks the raster cells some layer holds evidence for.
		seen := make([]bool, raster.Width*raster.Height)
		side := int32(fused.Config().TileSide)
		for _, l := range fused.Delta(nil) {
			if l.Floor != floor {
				continue
			}
			for i, b := range l.Cells {
				if b == 0 {
					continue
				}
				p := g.Center(grid.Cell{IX: l.IX*side + int32(i)%side, IZ: l.IZ*side + int32(i)/side})
				ix, iz := int((p.X-raster.Origin.X)/cell), int((p.Z-raster.Origin.Z)/cell)
				if p.X >= raster.Origin.X && p.Z >= raster.Origin.Z && ix < raster.Width && iz < raster.Height {
					seen[iz*raster.Width+ix] = true
				}
			}
		}
		fAgree, fObserved, fDecidedAgree, fDecided := 0, 0, 0, 0
		wrong := map[string]int{}
		for iz := 0; iz < raster.Height; iz++ {
			for ix := 0; ix < raster.Width; ix++ {
				want := raster.Cells[iz*raster.Width+ix]
				if want == spatialsim.CellUnknown || !seen[iz*raster.Width+ix] {
					continue
				}
				got := g.StateAt(raster.Centre(ix, iz))
				ok := (want == spatialsim.CellOccupied && got == grid.Occupied) || (want == spatialsim.CellFree && got == grid.Free)
				fObserved++
				if ok {
					fAgree++
				} else {
					wrong[fmt.Sprintf("truth %s, map %s", raster.Classes[iz*raster.Width+ix], got)]++
				}
				if got != grid.Unknown {
					fDecided++
					if ok {
						fDecidedAgree++
					}
				}
			}
		}
		t.Logf("%s: agreement %.4f on %d observed cells (%.4f on the %d the map decides); disagreements %v",
			floor, float64(fAgree)/float64(fObserved), fObserved, float64(fDecidedAgree)/float64(fDecided), fDecided, wrong)
		if frac := float64(fAgree) / float64(fObserved); fObserved < minObservedCells || frac <= targetCellAgreement {
			t.Errorf("%s: agreement %.4f on %d observed cells; want > %.2f on at least %d", floor, frac, fObserved, targetCellAgreement, minObservedCells)
		}
		agree, observed, decidedAgree, decided = agree+fAgree, observed+fObserved, decidedAgree+fDecidedAgree, decided+fDecided
	}
	if got := len(fused.Floors()); got != 3 {
		t.Errorf("the fleet mapped %d floors, want ground, upper and yard", got)
	}
	t.Logf("METRIC cell_agreement=%.4f fraction (target > %.2f)", float64(agree)/float64(observed), targetCellAgreement)
	t.Logf("METRIC cell_agreement_decided=%.4f fraction (target > %.2f)", float64(decidedAgree)/float64(decided), targetCellAgreement)
	t.Logf("METRIC observed_cells=%d cells (target >= %d)", observed, 3*minObservedCells)
	if frac := float64(agree) / float64(observed); frac <= targetCellAgreement {
		t.Errorf("cell agreement %.4f, want > %.2f", frac, targetCellAgreement)
	}

	// The fused map holds more than any robot saw alone, and what the fleet
	// holds of a robot is exactly what that robot mapped: Commit missed no
	// tile and the trip through components lost no cell.
	for _, r := range fleet {
		own, all := 0, 0
		alone := mustReplica(t, DefaultConfig())
		for _, l := range fused.Delta(nil) {
			n := len(l.Cells) - strings.Count(string(l.Cells), "\x00")
			all += n
			if l.Source == r.id {
				own += n
				if _, err := alone.Apply(l); err != nil {
					t.Fatal(err)
				}
			}
		}
		if own == 0 || own >= all {
			t.Errorf("%s contributed %d of %d layer cells", r.id, own, all)
		}
		for floor, fm := range r.mapper.floors {
			if !mustGrid(t, alone, floor).Equal(fm.g) {
				t.Errorf("%s: the fleet's copy of its %s layers differs from its own grid", r.id, floor)
			}
		}
	}

	// Every fused tile is publishable within the component limits.
	tiles := 0
	for _, floor := range fused.Floors() {
		ts, err := fused.Tiles(floor)
		if err != nil {
			t.Fatal(err)
		}
		for _, tile := range ts {
			c, err := fused.Component(publication("map-server"), floor, tile.IX, tile.IZ)
			if err != nil {
				t.Fatal(err)
			}
			if c.Provenance.ModelID != spatial.ModelPrefix+Feature || len(c.Provenance.Inputs) == 0 || c.Provenance.Inputs[0].Origin != world.Simulated {
				t.Fatalf("fused tile provenance: %+v", c.Provenance)
			}
			whole, _ := json.Marshal(c)
			largestPayload, largestComponent, tiles = max(largestPayload, len(c.Payload)), max(largestComponent, len(whole)), tiles+1
		}
	}
	t.Logf("%d layer components synced, %d fused tiles; largest component %d bytes (limit %d)", sent, tiles, largestComponent, spatial.MaxComponentBytes)
	t.Logf("METRIC max_tile_payload=%d bytes (target <= %d)", largestPayload, targetPayloadBytes)
	t.Logf("METRIC replicas_converged=%d replicas (target = %d)", len(fleet), len(fleet))
	if largestPayload > targetPayloadBytes || largestComponent > spatial.MaxComponentBytes {
		t.Errorf("tile payload %d bytes, component %d bytes", largestPayload, largestComponent)
	}
}
