package history

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/spatial"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/geom"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

// seeds reads SPATIAL_SEEDS (comma-separated unsigned integers); the default
// is one seed.
func seeds(t *testing.T) []uint64 {
	t.Helper()
	env := os.Getenv("SPATIAL_SEEDS")
	if env == "" {
		return []uint64{1}
	}
	var out []uint64
	for _, field := range strings.Split(env, ",") {
		seed, err := strconv.ParseUint(strings.TrimSpace(field), 10, 64)
		if err != nil {
			t.Fatalf("SPATIAL_SEEDS: %v", err)
		}
		out = append(out, seed)
	}
	return out
}

// fakeClock is the injected clock. Only the goroutine that appends reads it.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time { return c.t }

const testDomain = "history.test"

var (
	testEpoch = spatial.Stamp(1_767_225_600_000_000_000) // 2026-01-01T00:00:00Z
	testFrame = world.FrameRef{ID: "site", Revision: "test-1"}
	testScope = spatial.Scope{Tenant: "tenant", Site: "site", Source: world.Source{Device: "node-1", App: "spatial", Service: "engine", Stream: "state"}}
	testSim   = world.SimulationContext{RunID: "history-unit", ScenarioVersion: "1", Seed: 1, ClockDomain: testDomain}
)

func at(seconds float64) time.Time {
	return testEpoch.Add(time.Duration(seconds * float64(time.Second)))
}

func testProvenance(t testing.TB, feature, input string) world.Provenance {
	t.Helper()
	prov, err := spatial.InferredProvenance(feature, "1", "history-test", []world.Input{{ID: input, Origin: world.Simulated, Simulation: &testSim}})
	if err != nil {
		t.Fatal(err)
	}
	return prov
}

// poseAt builds a DevicePose component the way a localizer publishes one.
func poseAt(t testing.TB, scope spatial.Scope, device string, sequence uint64, sample time.Time, ttl time.Duration, x, z float64) world.Component {
	t.Helper()
	p := spatial.DevicePose{
		Device: spatial.DeviceID(device), Floor: "ground", FrameID: testFrame.ID,
		Location: spatial.At(geom.Vec3{X: x, Z: z}, 0.05), Yaw: 0.25,
		Covariance: geom.Mat3{1e-4, 0, 0, 0, 1e-4, 0, 0, 0, 1e-5}, Method: "test", Score: 0.9,
	}
	timing := world.Timing{SampleTime: sample, ClockDomain: testDomain, ErrorKnown: true}
	c, err := spatial.ToComponent(scope, world.Version{Epoch: 1, Sequence: sequence}, testFrame, timing, ttl,
		testProvenance(t, "localize", fmt.Sprintf("%s/odom/odometry/%d", device, sample.UnixNano())), p)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// tombstoneOf is the tombstone of c's key at a later version.
func tombstoneOf(c world.Component, sequence uint64, sample time.Time) world.Component {
	c.Version.Sequence = sequence
	c.Timing.SampleTime = sample
	c.Payload, c.Tombstone = nil, true
	return c
}

// canon is the form a component has after a trip through JSON, which is what
// world.Replica and the journal both hand back.
func canon(t testing.TB, c world.Component) world.Component {
	t.Helper()
	c.Timing.SampleTime = c.Timing.SampleTime.Round(0)
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	var out world.Component
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func keyOrder(k world.Key) string {
	raw, _ := json.Marshal(k)
	return string(raw)
}

func label(c world.Component) string {
	return fmt.Sprintf("%s|%s|%s@%d.%d", c.Key.Source.Stream, c.Key.Type, c.Key.Entity, c.Version.Epoch, c.Version.Sequence)
}

func labels(cs []world.Component) string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = label(c)
	}
	return strings.Join(out, " ")
}

// sameState reports whether two reconstructions are exactly equal: same keys
// in the same order with deeply equal components.
func sameState(got, want []world.Component) bool {
	return len(got) == len(want) && (len(got) == 0 || reflect.DeepEqual(got, want))
}

func components(recs []Record) []world.Component {
	out := make([]world.Component, len(recs))
	for i, r := range recs {
		out[i] = r.Component
	}
	return out
}

// model is the brute-force oracle: every record the journal accepted, in
// canonical form. Its answers are computed from the definition, with no
// index, so they do not share the journal's code.
type model []Record

func (r Record) time(axis Axis) time.Time {
	if axis == ApplyTime {
		return r.Applied
	}
	return r.Component.Timing.SampleTime
}

// inForce is the highest-version record per key timed at or before t.
func (m model) inForce(t time.Time, axis Axis) map[world.Key]Record {
	best := map[world.Key]Record{}
	for _, r := range m {
		if r.time(axis).After(t) {
			continue
		}
		if cur, ok := best[r.Component.Key]; !ok || r.Component.Version.Compare(cur.Component.Version) > 0 {
			best[r.Component.Key] = r
		}
	}
	return best
}

// asOf is the model state. TTL is modelled on the sample axis only; on the
// apply axis the oracle for expiry is world.Replica itself.
func (m model) asOf(t time.Time, v View) []world.Component {
	if v.Axis == ApplyTime && v.HonourTTL {
		panic("the apply-axis TTL oracle is world.Replica")
	}
	var out []world.Component
	for _, r := range m.inForce(t, v.Axis) {
		c := r.Component
		if c.Tombstone || (v.HonourTTL && !t.Before(c.Timing.SampleTime.Add(c.TTL))) {
			continue
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, k int) bool { return keyOrder(out[i].Key) < keyOrder(out[k].Key) })
	return out
}

func (m model) diff(from, to time.Time, v View) Difference {
	before := map[world.Key]world.Component{}
	for _, c := range m.asOf(from, v) {
		before[c.Key] = c
	}
	var d Difference
	for _, c := range m.asOf(to, v) {
		old, ok := before[c.Key]
		delete(before, c.Key)
		if !ok {
			d.Added = append(d.Added, c)
		} else if old.Version != c.Version {
			d.Changed = append(d.Changed, Change{From: old, To: c})
		}
	}
	for _, c := range m.asOf(from, v) {
		if _, gone := before[c.Key]; gone {
			d.Removed = append(d.Removed, c)
		}
	}
	return d
}

// timeline replays the definition instant by instant: at each time a record
// of the key carries, the record in force; an entry whenever that changes.
func (m model) timeline(key world.Key, from, to time.Time, axis Axis) []Record {
	var times []time.Time
	for _, r := range m {
		if r.Component.Key == key && !r.time(axis).After(to) {
			times = append(times, r.time(axis))
		}
	}
	sort.Slice(times, func(i, k int) bool { return times[i].Before(times[k]) })
	var out []Record
	var current uint64
	for _, at := range times {
		r, ok := m.inForce(at, axis)[key]
		if !ok || r.Seq == current {
			continue
		}
		current = r.Seq
		if !at.Before(from) {
			out = append(out, r)
		}
	}
	return out
}

func sameDifference(got, want Difference) bool {
	if !sameState(got.Added, want.Added) || !sameState(got.Removed, want.Removed) || len(got.Changed) != len(want.Changed) {
		return false
	}
	return len(got.Changed) == 0 || reflect.DeepEqual(got.Changed, want.Changed)
}

func sameRecords(got, want []Record) bool {
	return len(got) == len(want) && (len(got) == 0 || reflect.DeepEqual(got, want))
}

func mustOpen(t testing.TB, dir string, cfg Config) *Journal {
	t.Helper()
	j, err := Open(dir, cfg)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { j.Close() })
	return j
}

func mustAppend(t testing.TB, j *Journal, c world.Component) {
	t.Helper()
	appended, err := j.Append(c)
	if err != nil || !appended {
		t.Fatalf("append %s: appended=%v err=%v", label(c), appended, err)
	}
}
