package occupancy

import (
	"math"
	"os"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/spatial"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/geom"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/grid"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

func TestZZProbeRefusal(t *testing.T) {
	if os.Getenv("PROBE") == "" {
		t.Skip()
	}
	try := func(name string, f func() error) {
		defer func() {
			if p := recover(); p != nil {
				t.Logf("PROBE %-44s PANIC %v", name, p)
			}
		}()
		t.Logf("PROBE %-44s err=%v", name, f())
	}
	pose := testPose("robot-a", "ground", 1.02, 1.02, 0)
	scan := quarterScan("robot-a", testEpoch, 1, 0, 0, 0)
	in := testInput("robot-a", 1)
	fresh := func() (*Replica, *Mapper) {
		r := mustReplica(t, DefaultConfig())
		return r, mustMapper(t, r, "robot-a")
	}

	for name, mount := range map[string]geom.Pose2{"NaN mount": {X: math.NaN()}, "Inf mount": {Z: math.Inf(1)}, "NaN mount yaw": {Yaw: math.NaN()}, "huge mount": {X: 1e9}} {
		_, m := fresh()
		try("Integrate "+name, func() error { return m.Integrate(testEpoch, pose, mount, scan, in) })
		t.Logf("PROBE   stats %+v", m.Stats())
	}
	{
		_, m := fresh()
		s := scan
		s.Ranges = nil
		try("Integrate scan without beams", func() error { return m.Integrate(testEpoch, pose, geom.Pose2{}, s, in) })
		s = quarterScan("robot-a", testEpoch, 0, 0, 0, 0) // all invalid
		try("Integrate scan of only invalid beams", func() error { return m.Integrate(testEpoch, pose, geom.Pose2{}, s, in) })
		layers, err := m.Commit()
		t.Logf("PROBE   stats %+v; commit %d layers err=%v", m.Stats(), len(layers), err)
		s = quarterScan("robot-a", testEpoch, 1, 0, 0, 0)
		s.Ranges[0] = math.NaN()
		try("Integrate scan with NaN range", func() error { return m.Integrate(testEpoch, pose, geom.Pose2{}, s, in) })
		s = quarterScan("robot-a", testEpoch, 1, 0, 0, 0)
		s.AngleIncrement = 0
		try("Integrate scan with zero increment", func() error { return m.Integrate(testEpoch, pose, geom.Pose2{}, s, in) })
		p := pose
		p.Yaw = math.NaN()
		try("Integrate NaN pose yaw", func() error { return m.Integrate(testEpoch, p, geom.Pose2{}, scan, in) })
		p = pose
		p.PositionM[0] = math.Inf(1)
		try("Integrate Inf pose x", func() error { return m.Integrate(testEpoch, p, geom.Pose2{}, scan, in) })
		p = pose
		p.UncertaintyM = math.NaN()
		try("Integrate NaN pose uncertainty", func() error { return m.Integrate(testEpoch, p, geom.Pose2{}, scan, in) })
		p = pose
		p.Covariance = geom.Mat3{4e-4, 0, 0, 0, 4e-4, 0, 0, 0, 1.0} // yaw sigma 1 rad
		try("Integrate pose with yaw sigma 1 rad", func() error { return m.Integrate(testEpoch, p, geom.Pose2{}, scan, in) })
		p.Covariance = geom.Mat3{100, 0, 0, 0, 100, 0, 0, 0, 1.0} // position sigma 10 m in covariance, UncertaintyM 0.03
		try("Integrate pose cov 10 m, UncertaintyM 0.03", func() error { return m.Integrate(testEpoch, p, geom.Pose2{}, scan, in) })
		try("Integrate zero site time", func() error { return m.Integrate(time.Time{}, pose, geom.Pose2{}, scan, in) })
		t.Logf("PROBE   stats %+v", m.Stats())
	}
	{
		// Duplicate delivery of one scan to the stream.
		r, m := fresh()
		for i := 0; i < 3; i++ {
			try("Integrate same scan again", func() error { return m.Integrate(testEpoch, pose, geom.Pose2{}, scan, in) })
		}
		ls := mustCommit(t, m)
		g := mustGrid(t, r, "ground")
		t.Logf("PROBE   same scan x3: stats %+v, hit cell log-odds %.2f (one scan = %.2f), inputs %d", m.Stats(), g.LogOddsAt(geom.Vec2{X: 2.02, Z: 1.02}), grid.DefaultConfig().Hit, len(ls[0].Inputs))
	}
	{
		// Streaming: scan failing inside drain (beyond the addressable map) is counted where?
		_, m := fresh()
		cal := spatial.Calibration{
			Device: "robot-a", Sensor: "lidar", Kind: spatial.KindLidar2D, SensorToParent: geom.IdentityPose3(),
			Parent: spatial.ParentDeviceBase, ParentDevice: "robot-a", ResidualUnit: "m", Method: "test", Revision: "1", State: spatial.CalibrationValid,
		}
		try("Calibrate", func() error { return m.Calibrate(cal) })
		far := testPose("robot-a", "ground", 9e5, 9e5, 0)
		try("Locate far 0", func() error { return m.Locate(testEpoch, far) })
		try("Locate far 1", func() error { return m.Locate(testEpoch.Add(100*time.Millisecond), far) })
		try("Ingest scan at far pose", func() error {
			return m.Ingest(quarterScan("robot-a", testEpoch.Add(50*time.Millisecond), 1, 0, 0, 0), in)
		})
		t.Logf("PROBE   stats after a scan failed in drain %+v", m.Stats())
		nonplanar := cal
		nonplanar.SensorToParent = geom.Pose3{R: geom.RotX(0.5), T: geom.Vec3{}}
		try("Calibrate tilted mount", func() error { return m.Calibrate(nonplanar) })
		try("Ingest after tilted mount", func() error {
			return m.Ingest(quarterScan("robot-a", testEpoch.Add(60*time.Millisecond), 1, 0, 0, 0), in)
		})
		t.Logf("PROBE   stats %+v", m.Stats())
		try("Ingest nil measurement", func() error { return m.Ingest(nil, in) })
	}
	{
		// A peer hands the source a layer of its own with the top version.
		r, m := fresh()
		poison := testLayer(LayerKey{Floor: "ground", Source: "robot-a"}, math.MaxUint64, make([]int8, 64*64)...)
		poison.Cells[0] = 5
		try("Apply own-source layer v=MaxUint64", func() error { _, err := r.Apply(poison); return err })
		try("Integrate after poison", func() error { return m.Integrate(testEpoch, pose, geom.Pose2{}, scan, in) })
		try("Commit after poison", func() error { _, err := m.Commit(); return err })
		try("Commit again", func() error { _, err := m.Commit(); return err })
		try("fused Component after poison", func() error {
			_, err := r.Component(testPublication("map-server"), "ground", 0, 0)
			return err
		})
	}
	{
		// Layer cells of -128.
		r, _ := fresh()
		l := testLayer(LayerKey{Floor: "ground", Source: "robot-b"}, 1, make([]int8, 64*64)...)
		l.Cells[0] = 0x80
		try("Apply layer with a -128 cell", func() error { _, err := r.Apply(l); return err })
		l.ElevationM = math.NaN()
		try("Apply layer NaN elevation", func() error { _, err := r.Apply(l); return err })
		try("ApplyComponent zero component", func() error { _, err := r.ApplyComponent(world.Component{}); return err })
		try("Delta with garbage summary", func() error {
			_ = r.Delta(Summary{{LayerKey: LayerKey{Floor: "a/b", Source: ""}, Version: 0}})
			return nil
		})
		try("Tile on unknown floor", func() error { _, err := r.Tile("nope", 0, 0); return err })
		var nilRep *Replica
		_ = nilRep
	}
	{
		// Restart with a same-version collision: old run v1 = Y at peers, new run v1 = X.
		for _, beams := range [][4]float64{{0, 1, 0, 0}, {0, 0, 1, 0}, {0, 0, 0, 1}, {0, 1.5, 0, 0}, {0, 0, 2, 0}, {0, 0, 0, 2.5}} {
			peer := mustReplica(t, DefaultConfig())
			old := mustMapper(t, peer, "robot-a")
			old.Integrate(testEpoch, pose, geom.Pose2{}, quarterScan("robot-a", testEpoch, 1, 0, 0, 0), testInput("robot-a", 1))
			mustCommit(t, old)
			re := mustReplica(t, DefaultConfig())
			nu := mustMapper(t, re, "robot-a")
			at := testEpoch.Add(10 * time.Second)
			nu.Integrate(at, pose, geom.Pose2{}, quarterScan("robot-a", at, beams[0], beams[1], beams[2], beams[3]), testInput("robot-a", 10))
			mustCommit(t, nu)
			n1, _ := re.Merge(peer)
			ls := mustCommit(t, nu)
			n2, _ := peer.Merge(re)
			g := mustGrid(t, peer, "ground")
			t.Logf("PROBE restart collision beams %v: new run took old v1: %d, recommitted %d layers, peer took %d; old run's wall cell at the peer: %v (converged %v)",
				beams, n1, len(ls), n2, g.StateAt(geom.Vec2{X: 2.02, Z: 1.02}), peer.Digest() == re.Digest())
		}
	}
}
