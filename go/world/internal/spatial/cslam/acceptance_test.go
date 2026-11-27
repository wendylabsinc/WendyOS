package cslam

import (
	"math"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/spatial"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/geom"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/grid"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/spatialsim"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

// Design targets (specs/superpowers/specs/2026-11-25-spatial-intelligence-design.md, feature 3).
const (
	targetTrajectoryRMSE = 0.15 // metres, after optimisation and a rigid alignment to Truth
	targetAlignmentM     = 0.15 // metres, inter-robot frame alignment
	targetAlignmentDeg   = 2.0  // degrees, inter-robot frame alignment
	// "Clearly better than odometry": the optimised error is at most this
	// fraction of the odometry-only error.
	targetOdometryRatio = 0.5
	// Not a design target: an alignment further from the truth than this many
	// of its reported σ would mean the reported uncertainty is not honest.
	maxAlignmentSigmas = 5.0
	// Not design targets either: the share of the merged map's occupied
	// cells that must lie within mapToleranceCells of true static structure.
	minMapOnStructure = 0.85
	mapToleranceCells = 2
)

func seeds(t *testing.T) []uint64 {
	env := os.Getenv("SPATIAL_SEEDS")
	if env == "" {
		return []uint64{1}
	}
	var out []uint64
	for _, f := range strings.Split(env, ",") {
		v, err := strconv.ParseUint(strings.TrimSpace(f), 10, 64)
		if err != nil {
			t.Fatalf("SPATIAL_SEEDS: %v", err)
		}
		out = append(out, v)
	}
	return out
}

var officeRovers = []spatial.DeviceID{spatialsim.Rover1, spatialsim.Rover2, spatialsim.Rover3}

// officeMapper returns a mapper holding the lidar calibrations that sensor
// registration would have published: the sanctioned Reference stand-in with
// its default errors (mount 1 cm and 0.5°, clock offset 3 ms).
func officeMapper(t *testing.T, site *spatialsim.Site) *Mapper {
	t.Helper()
	ref, err := site.Reference(spatialsim.DefaultReferenceConfig())
	if err != nil {
		t.Fatal(err)
	}
	m, err := New(Config{})
	if err != nil {
		t.Fatal(err)
	}
	for _, dev := range officeRovers {
		cal, ok := ref.Calibration(dev, spatialsim.SensorLidar, site.At(0))
		if !ok {
			t.Fatalf("no reference calibration for %s", dev)
		}
		if err := m.SetCalibration(cal); err != nil {
			t.Fatal(err)
		}
	}
	return m
}

func officeStream(site *spatialsim.Site, window time.Duration) []spatial.Measurement {
	var out []spatial.Measurement
	filter := spatialsim.Filter{Devices: officeRovers, Kinds: []spatial.SensorKind{spatial.KindLidar2D, spatial.KindOdometry}}
	for meas := range site.Stream(site.At(0), site.At(window), filter) {
		out = append(out, meas)
	}
	return out
}

func TestAcceptanceOffice(t *testing.T) {
	for _, seed := range seeds(t) {
		t.Run("seed"+strconv.FormatUint(seed, 10), func(t *testing.T) {
			site, err := spatialsim.Office(seed, spatialsim.Options{})
			if err != nil {
				t.Fatal(err)
			}
			m := officeMapper(t, site)
			stream := officeStream(site, officeWindow)
			odom := map[spatial.DeviceID][]spatial.Odometry{}
			for _, meas := range stream {
				if err := m.Ingest(meas); err != nil {
					t.Fatal(err)
				}
				if o, ok := meas.(spatial.Odometry); ok {
					odom[o.Device] = append(odom[o.Device], o)
				}
			}
			sol, err := m.Solve()
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("closures %d, removed %d, chi2/dof %.2f (%d), converged %v, optimizations %d", sol.Closures, sol.Removed, sol.Chi2/float64(sol.DOF), sol.DOF, sol.Converged, m.back.optimizations)

			// Scoring only from here on: Truth never reached the mapper.
			truth := site.Truth()
			truthAt := func(dev spatial.DeviceID, at time.Time) geom.Pose2 {
				st, ok := truth.Device(dev, at)
				if !ok {
					t.Fatalf("no truth for %s", dev)
				}
				return st.Pose
			}
			var est, want []geom.Vec2
			var odomSq float64
			for _, r := range sol.Robots {
				if !r.Shared {
					t.Fatalf("%s is not in the shared frame", r.Device)
				}
				var dead, real []geom.Vec2
				for i, p := range r.Trajectory() {
					at := r.Keyframes[i].Time
					est, want = append(est, p.Pos()), append(want, truthAt(r.Device, at).Pos())
					dead, real = append(dead, odometryAt(odom[r.Device], at).Pos()), append(real, truthAt(r.Device, at).Pos())
				}
				// Odometry alone, each robot aligned to Truth on its own:
				// the most favourable reading of raw odometry.
				_, rmse, err := geom.AlignPoints2(dead, real, nil)
				if err != nil {
					t.Fatal(err)
				}
				odomSq += rmse * rmse * float64(len(dead))
				t.Logf("%s: %d keyframes, odometry-only RMSE %.3f m", r.Device, len(dead), rmse)
			}
			toSite, rmse, err := geom.AlignPoints2(est, want, nil)
			if err != nil {
				t.Fatal(err)
			}
			odomRMSE := math.Sqrt(odomSq / float64(len(est)))
			t.Logf("METRIC odometry_only_rmse=%.3f m (reference, no target)", odomRMSE)
			t.Logf("METRIC trajectory_rmse=%.3f m (target < %.2f)", rmse, targetTrajectoryRMSE)
			t.Logf("METRIC trajectory_rmse_over_odometry=%.3f ratio (target < %.2f)", rmse/odomRMSE, targetOdometryRatio)
			if rmse >= targetTrajectoryRMSE {
				t.Errorf("trajectory RMSE %.3f m, target < %.2f", rmse, targetTrajectoryRMSE)
			}
			if rmse >= targetOdometryRatio*odomRMSE {
				t.Errorf("trajectory RMSE %.3f m is not clearly better than odometry %.3f m", rmse, odomRMSE)
			}

			// Inter-robot alignment, scored as the relative transform between
			// two robots' own frames: no gauge involved. A robot's own frame
			// is where its first keyframe is the identity, so its true place
			// is the true base pose at that keyframe. Scored this way the
			// error includes the lidar-mount errors of the calibration
			// stand-in (0.5° per robot, which turns that robot's whole map
			// about its origin); those are the bulk of it.
			ownFrame := func(r Robot) geom.Pose2 {
				return truthAt(r.Device, r.Keyframes[0].Time).Compose(r.Keyframes[0].Local.Inverse())
			}
			// The same transform with each own frame placed by its whole
			// trajectory instead of its first keyframe: how well the two
			// robots' maps line up, whatever the mount calibration.
			fitFrame := func(r Robot) geom.Pose2 {
				var local, real []geom.Vec2
				for _, kf := range r.Keyframes {
					local, real = append(local, kf.Local.Pos()), append(real, truthAt(r.Device, kf.Time).Pos())
				}
				fit, _, err := geom.AlignPoints2(local, real, nil)
				if err != nil {
					t.Fatal(err)
				}
				return fit
			}
			ref, _ := sol.Robot(sol.Reference)
			var worstM, worstDeg, mapM, mapDeg, worstSigmas float64
			for _, r := range sol.Robots {
				if r.Device == sol.Reference {
					continue
				}
				if r.Alignment == nil {
					t.Fatalf("%s has no frame alignment", r.Device)
				}
				got, real, fit := r.Alignment.ChildToParent, ownFrame(ref).Between(ownFrame(r)), fitFrame(ref).Between(fitFrame(r))
				dm, dyaw := got.Pos().Dist(real.Pos()), math.Abs(geom.AngleDiff(got.Yaw, real.Yaw))
				fm, fdeg := got.Pos().Dist(fit.Pos()), math.Abs(geom.AngleDiff(got.Yaw, fit.Yaw))*180/math.Pi
				sigmaM, sigmaYaw := geom.PositionCov(r.Alignment.Covariance).MaxSigma(), math.Sqrt(r.Alignment.Covariance[8])
				t.Logf("%s in %s: %.3f m, %.2f° (reported σ %.3f m, %.2f°); maps %.3f m, %.2f°; %d closures, RMSE %.3f m",
					r.Device, sol.Reference, dm, dyaw*180/math.Pi, sigmaM, sigmaYaw*180/math.Pi, fm, fdeg, r.Alignment.Inliers, r.Alignment.RMSE)
				worstM, worstDeg = math.Max(worstM, dm), math.Max(worstDeg, dyaw*180/math.Pi)
				mapM, mapDeg = math.Max(mapM, fm), math.Max(mapDeg, fdeg)
				worstSigmas = math.Max(worstSigmas, math.Max(dm/sigmaM, dyaw/sigmaYaw))
			}
			t.Logf("METRIC frame_alignment_translation=%.3f m (target < %.2f)", worstM, targetAlignmentM)
			t.Logf("METRIC frame_alignment_rotation=%.2f deg (target < %.1f)", worstDeg, targetAlignmentDeg)
			t.Logf("METRIC frame_alignment_map_translation=%.3f m (target < %.2f)", mapM, targetAlignmentM)
			t.Logf("METRIC frame_alignment_map_rotation=%.2f deg (target < %.1f)", mapDeg, targetAlignmentDeg)
			t.Logf("METRIC frame_alignment_error_over_sigma=%.1f ratio (reported uncertainty must cover the error: < %.0f)", worstSigmas, maxAlignmentSigmas)
			if worstM >= targetAlignmentM || worstDeg >= targetAlignmentDeg {
				t.Errorf("inter-robot alignment %.3f m, %.2f°; targets < %.2f m, < %.1f°", worstM, worstDeg, targetAlignmentM, targetAlignmentDeg)
			}
			if mapM >= targetAlignmentM || mapDeg >= targetAlignmentDeg {
				t.Errorf("inter-robot map alignment %.3f m, %.2f°; targets < %.2f m, < %.1f°", mapM, mapDeg, targetAlignmentM, targetAlignmentDeg)
			}
			if !(worstSigmas < maxAlignmentSigmas) {
				t.Errorf("alignment error is %.1f times its reported σ", worstSigmas)
			}

			// The merged map of the floor: its occupied cells, carried to the
			// site frame by the rigid alignment above, lie on what Truth says
			// is static structure. People and the other robots are not in
			// Truth's raster, so the bound is a sanity check, not a target.
			merged, err := sol.Grid(spatialsim.FloorGround, grid.DefaultConfig())
			if err != nil {
				t.Fatal(err)
			}
			raster, err := truth.Occupancy(spatialsim.FloorGround, site.At(0), merged.CellSize())
			if err != nil {
				t.Fatal(err)
			}
			occupied, onStructure := 0, 0
			merged.Each(func(c grid.Cell, _ float64) bool {
				if merged.State(c) != grid.Occupied {
					return true
				}
				occupied++
				p := toSite.Apply(merged.Center(c))
				for dx := -mapToleranceCells; dx <= mapToleranceCells; dx++ {
					for dz := -mapToleranceCells; dz <= mapToleranceCells; dz++ {
						if raster.At(p.Add(geom.Vec2{X: float64(dx), Z: float64(dz)}.Scale(raster.CellSize))) == spatialsim.CellOccupied {
							onStructure++
							return true
						}
					}
				}
				return true
			})
			share := float64(onStructure) / float64(max(occupied, 1))
			t.Logf("METRIC merged_map_on_structure=%.3f ratio of %d occupied cells within %d cells of true structure (sanity > %.2f)", share, occupied, mapToleranceCells, minMapOnStructure)
			if share <= minMapOnStructure {
				t.Errorf("only %.0f%% of the merged map's occupied cells lie on true structure", share*100)
			}

			// Everything the solution publishes becomes a valid component
			// under this feature's model id, with ingested scans as ancestry.
			ancestry := map[string]world.Input{}
			for _, meas := range stream {
				if _, ok := meas.(spatial.Scan); ok {
					ancestry[spatial.InputID(meas)] = site.Input(meas)
				}
			}
			tiles, err := sol.MapRecords(spatialsim.FloorGround, grid.DefaultConfig(), 64)
			if err != nil {
				t.Fatal(err)
			}
			scope := spatial.Scope{Tenant: "tenant", Site: "office", Source: world.Source{Device: "hub", App: "wendy", Service: "spatial", Stream: Feature}}
			for i, rec := range append(sol.Records(), tiles...) {
				var inputs []world.Input
				for _, id := range rec.Inputs {
					in, ok := ancestry[id]
					if !ok {
						t.Fatalf("record %d names input %q, which was never ingested", i, id)
					}
					inputs = append(inputs, in)
				}
				prov, err := spatial.InferredProvenance(Feature, "1", "test", spatial.ReduceInputs(inputs))
				if err != nil {
					t.Fatal(err)
				}
				_, err = spatial.ToComponent(scope, world.Version{Epoch: 1, Sequence: uint64(i + 1)}, world.FrameRef{ID: rec.FrameID, Revision: "r1"},
					world.Timing{SampleTime: rec.Time, ClockDomain: spatialsim.ClockDomain}, time.Minute, prov, rec.Payload)
				if err != nil {
					t.Fatalf("record %d (%s): %v", i, rec.Payload.Type(), err)
				}
			}
		})
	}
}

// odometryAt interpolates raw odometry, for the odometry-only baseline.
func odometryAt(samples []spatial.Odometry, at time.Time) geom.Pose2 {
	for i := 1; i < len(samples); i++ {
		if !samples[i].Time.Before(at) {
			a, b := samples[i-1], samples[i]
			return a.Pose.Interpolate(b.Pose, float64(at.Sub(a.Time))/float64(b.Time.Sub(a.Time)))
		}
	}
	return samples[len(samples)-1].Pose
}
