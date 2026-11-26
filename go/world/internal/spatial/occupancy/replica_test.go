package occupancy

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/spatial"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/grid"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

var testEpoch = spatial.Stamp(1_767_258_000_000_000_000)

func testConfig(side int) Config {
	cfg := DefaultConfig()
	cfg.TileSide = side
	return cfg
}

func mustReplica(t testing.TB, cfg Config) *Replica {
	t.Helper()
	r, err := NewReplica(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func testInput(source spatial.DeviceID, n int) world.Input {
	return world.Input{ID: fmt.Sprintf("%s/lidar/lidar2d/%d", source, n), Origin: world.Measured}
}

// testLayer builds a layer of side x side cells from int8 steps.
func testLayer(key LayerKey, version uint64, steps ...int8) Layer {
	cells := make([]byte, len(steps))
	for i, v := range steps {
		cells[i] = byte(v)
	}
	return Layer{
		LayerKey: key, Version: version, CellSizeM: grid.DefaultConfig().CellSize, Time: testEpoch.Add(time.Duration(version) * time.Second),
		Cells: cells, Inputs: []world.Input{testInput(key.Source, int(version))},
	}
}

func testPublication(source spatial.DeviceID) Publication {
	return Publication{
		Scope: spatial.Scope{Tenant: "tenant", Site: "site-1", Source: world.Source{Device: string(source), App: "wendy", Service: "spatial", Stream: "occupancy"}},
		Epoch: 1, Frame: world.FrameRef{ID: "site", Revision: "r1"},
		Timing: world.Timing{ClockDomain: "test", ErrorKnown: true}, TTL: time.Hour, AppVersion: "test",
	}
}

// history is a seeded write history: every version of every layer that
// sources sources ever wrote, in write order. Versions of one key increase;
// earlier versions are superseded and must lose every merge.
func history(rng *rand.Rand, side, sources, writes int, versions map[LayerKey]uint64) []Layer {
	floors := []spatial.FloorID{"ground", "upper"}
	out := make([]Layer, 0, writes)
	for range writes {
		key := LayerKey{
			Floor: floors[rng.IntN(len(floors))], IX: int32(rng.IntN(3) - 1), IZ: int32(rng.IntN(3) - 1),
			Source: spatial.DeviceID(fmt.Sprintf("robot-%d", rng.IntN(sources))),
		}
		versions[key]++
		steps := make([]int8, side*side)
		for i := range steps {
			if rng.IntN(3) > 0 {
				steps[i] = int8(rng.IntN(111) - 40) // the default per-source range, −2 to 3.5
			}
		}
		l := testLayer(key, versions[key], steps...)
		l.ElevationM = float64(rng.IntN(3)) * 0.01
		out = append(out, l)
	}
	return out
}

// fusedDigests is the content of every fused tile of every floor.
func fusedDigests(t testing.TB, r *Replica) string {
	t.Helper()
	var b strings.Builder
	for _, floor := range r.Floors() {
		tiles, err := r.Tiles(floor)
		if err != nil {
			t.Fatal(err)
		}
		for _, tile := range tiles {
			fmt.Fprintf(&b, "%s %s %v\n", tile.EntityKey(), tile.Digest, tile.PositionM)
		}
	}
	return b.String()
}

// Random delivery orders and duplications across N replicas converge to one
// digest and one fused map.
func TestMergeIsOrderIndependent(t *testing.T) {
	const (
		side     = 4
		replicas = 8
	)
	for seed := uint64(1); seed <= mergeSeeds; seed++ {
		rng := rand.New(rand.NewPCG(seed, 9))
		ops := history(rng, side, 5, 120, map[LayerKey]uint64{})
		ref := mustReplica(t, testConfig(side))
		for _, l := range ops {
			if _, err := ref.Apply(l); err != nil {
				t.Fatal(err)
			}
		}
		want, wantFused := ref.Digest(), fusedDigests(t, ref)
		for n := range replicas {
			delivery := append([]Layer(nil), ops...)
			for _, l := range ops { // each message up to three times
				for range rng.IntN(3) {
					delivery = append(delivery, l)
				}
			}
			rng.Shuffle(len(delivery), func(i, j int) { delivery[i], delivery[j] = delivery[j], delivery[i] })
			r := mustReplica(t, testConfig(side))
			for _, l := range delivery {
				if _, err := r.Apply(l); err != nil {
					t.Fatal(err)
				}
			}
			if got := r.Digest(); got != want {
				t.Fatalf("seed %d replica %d: digest %s, want %s", seed, n, got, want)
			}
			if got := fusedDigests(t, r); got != wantFused {
				t.Fatalf("seed %d replica %d: fused tiles differ", seed, n)
			}
		}
	}
}

func TestMergeIsCommutativeAssociativeIdempotent(t *testing.T) {
	const side = 4
	for seed := uint64(1); seed <= mergeSeeds; seed++ {
		rng := rand.New(rand.NewPCG(seed, 10))
		ops := history(rng, side, 4, 90, map[LayerKey]uint64{})
		subset := func() *Replica {
			r := mustReplica(t, testConfig(side))
			for _, l := range ops {
				if rng.IntN(2) == 0 {
					if _, err := r.Apply(l); err != nil {
						t.Fatal(err)
					}
				}
			}
			return r
		}
		join := func(parts ...*Replica) *Replica {
			r := mustReplica(t, testConfig(side))
			for _, p := range parts {
				if _, err := r.Merge(p); err != nil {
					t.Fatal(err)
				}
			}
			return r
		}
		a, b, c := subset(), subset(), subset()
		if join(a, b).Digest() != join(b, a).Digest() {
			t.Fatalf("seed %d: merge is not commutative", seed)
		}
		if join(join(a, b), c).Digest() != join(a, join(b, c)).Digest() {
			t.Fatalf("seed %d: merge is not associative", seed)
		}
		ab := join(a, b)
		before := ab.Digest()
		for _, again := range []*Replica{a, b, ab} {
			if n, err := ab.Merge(again); err != nil || n != 0 || ab.Digest() != before {
				t.Fatalf("seed %d: merge is not idempotent (%d changes, %v)", seed, n, err)
			}
		}
	}
}

// A source that breaks the contract and writes two layers with one version
// still converges: the order of arrival does not pick the winner.
func TestSameVersionConflictConverges(t *testing.T) {
	key := LayerKey{Floor: "ground", Source: "robot-a"}
	x, y := testLayer(key, 3, 10, 0, 0, 0), testLayer(key, 3, -10, 0, 0, 0)
	a, b := mustReplica(t, testConfig(2)), mustReplica(t, testConfig(2))
	for _, l := range []Layer{x, y, x} {
		if _, err := a.Apply(l); err != nil {
			t.Fatal(err)
		}
	}
	for _, l := range []Layer{y, x, y} {
		if _, err := b.Apply(l); err != nil {
			t.Fatal(err)
		}
	}
	if a.Digest() != b.Digest() {
		t.Fatal("equal versions with different content diverged")
	}
	// The summary tells them apart, so a delta sync heals it too.
	c, d := mustReplica(t, testConfig(2)), mustReplica(t, testConfig(2))
	c.Apply(x)
	d.Apply(y)
	for _, l := range c.Delta(d.Summary()) {
		d.Apply(l)
	}
	for _, l := range d.Delta(c.Summary()) {
		c.Apply(l)
	}
	if c.Digest() != d.Digest() || c.Digest() != a.Digest() {
		t.Fatal("delta sync did not resolve a same-version conflict")
	}
}

// Delta answers a summary with exactly the layers that are newer.
func TestDeltaIsExactlyTheNewerLayers(t *testing.T) {
	const side = 4
	for seed := uint64(1); seed <= mergeSeeds; seed++ {
		rng := rand.New(rand.NewPCG(seed, 11))
		ops := history(rng, side, 4, 100, map[LayerKey]uint64{})
		a, b := mustReplica(t, testConfig(side)), mustReplica(t, testConfig(side))
		for _, l := range ops {
			if rng.IntN(2) == 0 {
				a.Apply(l)
			}
			if rng.IntN(2) == 0 {
				b.Apply(l)
			}
		}
		want := map[LayerKey]uint64{}
		for _, v := range a.Summary() {
			if held, ok := b.Layer(v.LayerKey); !ok || held.Version < v.Version {
				want[v.LayerKey] = v.Version
			}
		}
		delta := a.Delta(b.Summary())
		if len(delta) != len(want) {
			t.Fatalf("seed %d: delta has %d layers, want %d", seed, len(delta), len(want))
		}
		for i, l := range delta {
			if want[l.LayerKey] != l.Version {
				t.Fatalf("seed %d: delta carries %+v v%d, which the peer does not need", seed, l.LayerKey, l.Version)
			}
			if i > 0 && delta[i-1].LayerKey.Compare(l.LayerKey) >= 0 {
				t.Fatalf("seed %d: delta is not ordered by key", seed)
			}
			if changed, err := b.Apply(l); err != nil || !changed {
				t.Fatalf("seed %d: a delta layer did not change the peer (%v)", seed, err)
			}
		}
		if rest := a.Delta(b.Summary()); len(rest) != 0 {
			t.Fatalf("seed %d: %d layers still newer after the delta", seed, len(rest))
		}
		if own := a.Delta(a.Summary()); len(own) != 0 {
			t.Fatalf("seed %d: a replica is %d layers newer than itself", seed, len(own))
		}
		if all := a.Delta(nil); len(all) != len(a.Summary()) {
			t.Fatalf("seed %d: an empty summary got %d of %d layers", seed, len(all), len(a.Summary()))
		}
		for _, l := range b.Delta(a.Summary()) {
			a.Apply(l)
		}
		if a.Digest() != b.Digest() {
			t.Fatalf("seed %d: a two-way delta did not converge", seed)
		}
	}
}

// network delivers the layers of one sync answer as components, the way they
// travel between nodes, and may drop, duplicate and reorder them.
type network struct {
	t    testing.TB
	rng  *rand.Rand
	drop float64 // probability a message is lost
	dup  float64 // probability a message is delivered twice
}

// pull runs one delta sync: to summarizes, from answers.
func (n *network) pull(to, from *Replica) {
	n.t.Helper()
	var wire []world.Component
	for _, l := range from.Delta(to.Summary()) {
		c, err := l.Component(testPublication(l.Source))
		if err != nil {
			n.t.Fatal(err)
		}
		if n.rng.Float64() < n.drop {
			continue
		}
		wire = append(wire, c)
		if n.rng.Float64() < n.dup {
			wire = append(wire, c)
		}
	}
	n.rng.Shuffle(len(wire), func(i, j int) { wire[i], wire[j] = wire[j], wire[i] })
	for _, c := range wire {
		if _, err := to.ApplyComponent(c); err != nil {
			n.t.Fatal(err)
		}
	}
}

// gossip runs random pairwise syncs inside group until its replicas agree,
// and returns the number of syncs. It fails after limit.
func (n *network) gossip(replicas []*Replica, group []int, limit int) int {
	n.t.Helper()
	agreed := func() bool {
		for _, i := range group[1:] {
			if replicas[i].Digest() != replicas[group[0]].Digest() {
				return false
			}
		}
		return true
	}
	for round := 0; round < limit; round++ {
		if agreed() {
			return round
		}
		to, from := group[n.rng.IntN(len(group))], group[n.rng.IntN(len(group))]
		if to != from {
			n.pull(replicas[to], replicas[from])
		}
	}
	n.t.Fatalf("no agreement inside %v after %d syncs", group, limit)
	return limit
}

// write applies each layer to the replica of its own source, as a mapper does.
func write(t testing.TB, replicas []*Replica, ref *Replica, ops []Layer) {
	t.Helper()
	for _, l := range ops {
		var i int
		fmt.Sscanf(string(l.Source), "robot-%d", &i)
		for _, r := range []*Replica{replicas[i], ref} {
			if _, err := r.Apply(l); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// Delta sync over a network that loses 30 % of the messages, duplicates
// 30 % and reorders all of them converges to the state of a replica that
// received every write directly.
func TestSyncSurvivesLossDuplicationAndReordering(t *testing.T) {
	const (
		side  = 4
		nodes = 6
	)
	for seed := uint64(1); seed <= syncSeeds; seed++ {
		rng := rand.New(rand.NewPCG(seed, 12))
		replicas := make([]*Replica, nodes)
		all := make([]int, nodes)
		for i := range replicas {
			replicas[i], all[i] = mustReplica(t, testConfig(side)), i
		}
		ref := mustReplica(t, testConfig(side))
		write(t, replicas, ref, history(rng, side, nodes, 150, map[LayerKey]uint64{}))
		net := &network{t: t, rng: rng, drop: 0.3, dup: 0.3}
		rounds := net.gossip(replicas, all, 5000)
		for i, r := range replicas {
			if r.Digest() != ref.Digest() || fusedDigests(t, r) != fusedDigests(t, ref) {
				t.Fatalf("seed %d: replica %d differs from the reference after %d syncs", seed, i, rounds)
			}
		}
	}
}

// Two halves of the fleet keep mapping while they cannot reach each other,
// agree among themselves, differ from the other half, and converge to the
// full state once the partition heals.
func TestPartitionHeals(t *testing.T) {
	const (
		side  = 4
		nodes = 6
	)
	for seed := uint64(1); seed <= syncSeeds; seed++ {
		rng := rand.New(rand.NewPCG(seed, 13))
		replicas := make([]*Replica, nodes)
		for i := range replicas {
			replicas[i] = mustReplica(t, testConfig(side))
		}
		ref := mustReplica(t, testConfig(side))
		versions := map[LayerKey]uint64{}
		net := &network{t: t, rng: rng, drop: 0.2, dup: 0.2}
		all, west, east := []int{0, 1, 2, 3, 4, 5}, []int{0, 1, 2}, []int{3, 4, 5}

		write(t, replicas, ref, history(rng, side, nodes, 60, versions))
		net.gossip(replicas, all, 5000)

		// Partition: every robot keeps rewriting its layers, including tiles
		// robots on the other side also cover.
		write(t, replicas, ref, history(rng, side, nodes, 120, versions))
		net.gossip(replicas, west, 5000)
		net.gossip(replicas, east, 5000)
		if replicas[west[0]].Digest() == replicas[east[0]].Digest() {
			t.Fatalf("seed %d: the two sides agree during the partition; the test proves nothing", seed)
		}
		if replicas[west[0]].Digest() == ref.Digest() {
			t.Fatalf("seed %d: one side already holds everything", seed)
		}

		rounds := net.gossip(replicas, all, 5000)
		for i, r := range replicas {
			if r.Digest() != ref.Digest() || fusedDigests(t, r) != fusedDigests(t, ref) {
				t.Fatalf("seed %d: replica %d differs from the reference after healing (%d syncs)", seed, i, rounds)
			}
		}
	}
}

func TestFusedTileIsTheClampedSum(t *testing.T) {
	r := mustReplica(t, testConfig(2))
	tile := func(source spatial.DeviceID) LayerKey {
		return LayerKey{Floor: "upper", IX: -1, IZ: 2, Source: source}
	}
	a, b, c := testLayer(tile("a"), 1, 10, -10, 60, 8), testLayer(tile("b"), 2, 20, -35, 50, -8), testLayer(tile("c"), 7, 0, 0, 0, 0)
	a.ElevationM, b.ElevationM, c.ElevationM = 3.4, 3.5, 9
	for _, l := range []Layer{c, a, b} {
		if changed, err := r.Apply(l); err != nil || !changed {
			t.Fatal(changed, err)
		}
	}
	got, err := r.Tile("upper", -1, 2)
	if err != nil {
		t.Fatal(err)
	}
	// −2 and 3.5 are −40 and 70 steps; +8 and −8 cancel to unknown.
	want := []int8{30, -40, 70, 0}
	for i, v := range want {
		if int8(got.Cells[i]) != v {
			t.Errorf("cell %d = %d steps, want %d", i, int8(got.Cells[i]), v)
		}
	}
	if len(got.Versions) != 3 || got.Versions["a"] != 1 || got.Versions["b"] != 2 || got.Versions["c"] != 7 {
		t.Errorf("versions = %v", got.Versions)
	}
	if got.PositionM[1] != 3.5 {
		t.Errorf("elevation = %g, want the median 3.5", got.PositionM[1])
	}
	if got.OriginXZ != spatial.TileOrigin(-1, 2, 2, 0.05) || got.LogOddsScale != grid.TileLogOddsScale || got.Validate() != nil {
		t.Errorf("tile geometry: %+v (%v)", got, got.Validate())
	}
	g, err := r.Grid("upper")
	if err != nil {
		t.Fatal(err)
	}
	// Tile (−1, 2) of side 2 starts at cell (−2, 4).
	for i, v := range want {
		c := grid.Cell{IX: -2 + int32(i%2), IZ: 4 + int32(i/2)}
		if got := g.LogOdds(c); got != float64(v)*grid.TileLogOddsScale && !(v == 0 && got == 0) {
			t.Errorf("grid cell %v = %g, want %g", c, got, float64(v)*grid.TileLogOddsScale)
		}
	}
	if g.Counts().Known != 3 {
		t.Errorf("known cells = %d, want 3", g.Counts().Known)
	}

	if _, err := r.Tile("upper", 0, 0); !errors.Is(err, ErrNoEvidence) {
		t.Errorf("unwritten tile: %v", err)
	}
	if _, err := r.Grid("ground"); !errors.Is(err, ErrNoEvidence) {
		t.Errorf("unwritten floor: %v", err)
	}
	if tiles, err := r.Tiles("ground"); err != nil || len(tiles) != 0 {
		t.Errorf("unwritten floor has %d tiles (%v)", len(tiles), err)
	}
}

// A cell a source observed is still known after the trip grid, layer,
// component, replica, fused tile, grid; with the default sensor model the
// trip is exact.
func TestRoundTripKeepsObservedCells(t *testing.T) {
	const side = 8
	g, err := grid.New(grid.Config{})
	if err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewPCG(3, 3))
	exact := g.Clone()
	for iz := int32(0); iz < side; iz++ {
		for ix := int32(-side); ix < 0; ix++ {
			c := grid.Cell{IX: ix, IZ: iz}
			switch rng.IntN(4) {
			case 0: // faint evidence, below one tile step
				g.Set(c, []float64{0.01, -0.01, 0.02, -0.02}[rng.IntN(4)])
			case 1: // hits and misses of the default model, on the tile lattice
				v := float64(rng.IntN(5))*0.85 - float64(rng.IntN(6))*0.40
				g.Set(c, v)
				exact.Set(c, v)
			}
		}
	}
	for name, src := range map[string]*grid.Grid{"faint": g, "default model": exact} {
		tile, err := src.Tile("ground", -1, 0, side, 0, map[string]uint64{"robot-a": 1})
		if err != nil {
			t.Fatal(err)
		}
		l := Layer{LayerKey: LayerKey{Floor: "ground", IX: -1, IZ: 0, Source: "robot-a"}, Version: 1, CellSizeM: tile.CellSizeM, Time: testEpoch, Cells: tile.Cells, Inputs: []world.Input{testInput("robot-a", 1)}}
		c, err := l.Component(testPublication("robot-a"))
		if err != nil {
			t.Fatal(err)
		}
		r := mustReplica(t, testConfig(side))
		if changed, err := r.ApplyComponent(c); err != nil || !changed {
			t.Fatal(changed, err)
		}
		back, err := r.Grid("ground")
		if err != nil {
			t.Fatal(err)
		}
		src.Each(func(c grid.Cell, logOdds float64) bool {
			if !back.Known(c) || (back.LogOdds(c) > 0) != (logOdds > 0) {
				t.Errorf("%s: cell %v with log-odds %g came back as %g", name, c, logOdds, back.LogOdds(c))
			}
			return true
		})
		if back.Counts().Known != src.Counts().Known {
			t.Errorf("%s: %d known cells came back as %d", name, src.Counts().Known, back.Counts().Known)
		}
		if name == "default model" && !back.Equal(src) {
			t.Errorf("the default sensor model does not survive the round trip exactly")
		}
		held, _ := r.Layer(l.LayerKey)
		if string(held.Cells) != string(l.Cells) || held.Version != 1 || !held.Time.Equal(testEpoch) || len(held.Inputs) != 1 {
			t.Errorf("%s: the layer changed on the wire: %+v", name, held)
		}
	}
}

// The largest tile the tiling allows, with incompressible cells, the longest
// identifiers and a full version vector, fits a spatial payload and a signed
// component: tiles need no compression.
func TestLargestTileFitsAComponent(t *testing.T) {
	rng := rand.New(rand.NewPCG(4, 4))
	r := mustReplica(t, testConfig(spatial.MaxTileSide))
	floor := spatial.FloorID(strings.Repeat("f", spatial.MaxIDLength))
	largest := 0
	for n := range spatial.MaxTileSources + 1 {
		source := spatial.DeviceID(fmt.Sprintf("%0*d", spatial.MaxIDLength, n))
		steps := make([]int8, spatial.MaxTileSide*spatial.MaxTileSide)
		for i := range steps {
			steps[i] = int8(rng.IntN(255) - 127)
		}
		l := testLayer(LayerKey{Floor: floor, IX: -1 << 14, IZ: 1<<14 - 1, Source: source}, 1<<56+uint64(n), steps...)
		l.ElevationM = -123456.789012345
		l.Inputs = nil
		for i := range 3 * spatial.MaxInputs { // reduced to the first and last scan
			l.Inputs = append(l.Inputs, testInput(source, 1_767_258_000_000_000_000+i))
		}
		if n == spatial.MaxTileSources {
			// One source too many for a version vector: the layer is held
			// and synced, the fused tile is refused.
			if _, err := r.Apply(l); err != nil {
				t.Fatal(err)
			}
			if _, err := r.Tile(floor, l.IX, l.IZ); !errors.Is(err, ErrTooManySources) {
				t.Fatalf("65 sources: %v", err)
			}
			continue
		}
		c, err := l.Component(testPublication(source))
		if err != nil {
			t.Fatal(err)
		}
		whole, _ := json.Marshal(c)
		largest = max(largest, len(c.Payload))
		if len(c.Payload) > spatial.MaxPayloadBytes || len(whole) > spatial.MaxComponentBytes {
			t.Fatalf("layer payload %d bytes, component %d bytes", len(c.Payload), len(whole))
		}
		if changed, err := r.ApplyComponent(c); err != nil || !changed {
			t.Fatal(changed, err)
		}
		if n == spatial.MaxTileSources-1 {
			fused, err := r.Component(testPublication("map-server"), floor, l.IX, l.IZ)
			if err != nil {
				t.Fatal(err)
			}
			whole, _ := json.Marshal(fused)
			t.Logf("largest layer payload %d bytes; fused tile of %d sources: payload %d bytes, component %d bytes (limits %d and %d)",
				largest, spatial.MaxTileSources, len(fused.Payload), len(whole), spatial.MaxPayloadBytes, spatial.MaxComponentBytes)
			if len(fused.Payload) > spatial.MaxPayloadBytes || len(whole) > spatial.MaxComponentBytes {
				t.Fatalf("fused payload %d bytes, component %d bytes", len(fused.Payload), len(whole))
			}
			if len(fused.Provenance.Inputs) != spatial.MaxInputs || fused.Provenance.ModelID != "wendy.spatial.occupancy" || fused.Provenance.Origin != world.Inferred {
				t.Fatalf("fused provenance: %d inputs, model %q", len(fused.Provenance.Inputs), fused.Provenance.ModelID)
			}
			tile, err := spatial.FromComponent[spatial.MapTile](fused)
			if err != nil || len(tile.Versions) != spatial.MaxTileSources {
				t.Fatalf("fused tile does not decode: %v", err)
			}
			if _, err := r.ApplyComponent(fused); !errors.Is(err, ErrInvalid) {
				t.Fatalf("a fused tile was taken for a layer: %v", err)
			}
		}
	}
}

func TestComponentsCarryVersionTimeAndAncestry(t *testing.T) {
	r := mustReplica(t, testConfig(2))
	a := testLayer(LayerKey{Floor: "ground", Source: "robot-a"}, 4, 1, 2, 3, 4)
	b := testLayer(LayerKey{Floor: "ground", Source: "robot-b"}, 9, -1, -2, -3, -4)
	r.Apply(a)
	r.Apply(b)
	layer, err := a.Component(testPublication("robot-a"))
	if err != nil {
		t.Fatal(err)
	}
	if layer.Version != (world.Version{Epoch: 1, Sequence: 4}) || !layer.Timing.SampleTime.Equal(a.Time) || layer.Key.Entity != "tile/ground/0/0" ||
		layer.Key.Source.Device != "robot-a" || len(layer.Provenance.Inputs) != 1 || layer.Provenance.Inputs[0].ID != a.Inputs[0].ID {
		t.Errorf("layer component: %+v", layer)
	}
	fused, err := r.Component(testPublication("map-server"), "ground", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if fused.Version.Sequence != 13 || !fused.Timing.SampleTime.Equal(b.Time) || len(fused.Provenance.Inputs) != 2 {
		t.Errorf("fused component: %+v", fused)
	}
	// A later layer version moves the fused sequence forward.
	r.Apply(testLayer(a.LayerKey, 5, 1, 2, 3, 5))
	if next, err := r.Component(testPublication("map-server"), "ground", 0, 0); err != nil || next.Version.Sequence != 14 {
		t.Errorf("fused sequence after a rewrite: %+v (%v)", next.Version, err)
	}

	if _, err := a.Component(testPublication("robot-b")); !errors.Is(err, ErrForeign) {
		t.Errorf("layer published under another device's key: %v", err)
	}
	other := testPublication("map-server")
	other.Frame.ID = "odom/robot-a"
	if _, err := r.Component(other, "ground", 0, 0); !errors.Is(err, ErrIncompatible) {
		t.Errorf("fused tile published in another frame: %v", err)
	}
	other = testPublication("robot-a")
	other.Frame.ID = "odom/robot-a"
	if c, err := a.Component(other); err != nil {
		t.Fatal(err)
	} else if _, err := mustReplica(t, testConfig(2)).ApplyComponent(c); !errors.Is(err, ErrIncompatible) {
		t.Errorf("layer of another frame applied: %v", err)
	}
}

func TestInvalidLayersAndConfigsAreRejected(t *testing.T) {
	r := mustReplica(t, testConfig(2))
	key := LayerKey{Floor: "ground", Source: "robot-a"}
	good := testLayer(key, 1, 1, 2, 3, 4)
	mutate := func(f func(*Layer)) Layer {
		l := good
		l.Cells = append([]byte(nil), good.Cells...)
		f(&l)
		return l
	}
	for name, tc := range map[string]struct {
		layer Layer
		want  error
	}{
		"another tile side":  {testLayer(key, 1, 1, 2, 3, 4, 5, 6, 7, 8, 9), ErrIncompatible},
		"not a square":       {testLayer(key, 1, 1, 2, 3), ErrIncompatible},
		"another cell size":  {mutate(func(l *Layer) { l.CellSizeM = 0.1 }), ErrIncompatible},
		"version zero":       {mutate(func(l *Layer) { l.Version = 0 }), ErrInvalid},
		"no time":            {mutate(func(l *Layer) { l.Time = time.Time{} }), ErrInvalid},
		"no ancestry":        {mutate(func(l *Layer) { l.Inputs = nil }), ErrInvalid},
		"inferred ancestry":  {mutate(func(l *Layer) { l.Inputs = []world.Input{{ID: "x", Origin: world.Inferred}} }), ErrInvalid},
		"floor with a slash": {mutate(func(l *Layer) { l.Floor = "a/b" }), ErrInvalid},
		"empty source":       {mutate(func(l *Layer) { l.Source = "" }), ErrInvalid},
		"elevation overflow": {mutate(func(l *Layer) { l.ElevationM = 1e300 }), ErrInvalid},
	} {
		if changed, err := r.Apply(tc.layer); !errors.Is(err, tc.want) || changed {
			t.Errorf("%s: changed %v, error %v, want %v", name, changed, err, tc.want)
		}
	}
	if len(r.Summary()) != 0 {
		t.Errorf("rejected layers left state behind")
	}
	if changed, err := r.Apply(good); err != nil || !changed {
		t.Fatal(changed, err)
	}
	for _, l := range []Layer{good, testLayer(key, 1, 1, 2, 3, 4)} {
		if changed, err := r.Apply(l); err != nil || changed {
			t.Errorf("a duplicate changed the state (%v)", err)
		}
	}
	if _, err := mustReplica(t, testConfig(4)).Merge(r); !errors.Is(err, ErrIncompatible) {
		t.Errorf("merge across tilings: %v", err)
	}

	for name, cfg := range map[string]Config{
		"tile side":       {TileSide: spatial.MaxTileSide + 1},
		"negative side":   {TileSide: -1},
		"clamp too tight": {Grid: grid.Config{Min: -0.04, Free: -0.02}},
		"grid":            {Grid: grid.Config{CellSize: -1}},
		"pose gate":       {MaxPoseUncertaintyM: -1},
		"pending":         {MaxPending: -1},
	} {
		if _, err := NewReplica(cfg); !errors.Is(err, ErrConfig) {
			t.Errorf("config %s: %v", name, err)
		}
	}
	if cfg := mustReplica(t, Config{}).Config(); cfg != DefaultConfig() {
		t.Errorf("zero config resolves to %+v, want the defaults %+v", cfg, DefaultConfig())
	}
}
