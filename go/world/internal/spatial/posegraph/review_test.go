package posegraph

import (
	"math"
	"testing"

	"github.com/wendylabsinc/wendy/go/world/internal/spatial/geom"
)

// A scan match in a corridor reports 100 m of sigma along the axis it cannot
// observe, tilted a little off the axis. Its covariance must still become an
// edge: the graph then takes the corridor position from odometry and the
// rest from the scan match.
func TestDegenerateScanMatchCovarianceBecomesAnEdge(t *testing.T) {
	const (
		unobservedSigma = 100.0 // scanmatch.Config.UnobservedSigma
		// Along the corridor only odometry speaks.
		maxAlongError = 1e-3
		// Agreement of the optimiser with the closed-form weighted mean.
		maxFusionError = 1e-6
	)
	weak := geom.Vec3{X: 0.999, Y: 0.03, Z: 0.02}.Unit().Array() // over (x, z, yaw)
	var matchCov geom.Mat3
	for i := 0; i < 3; i++ {
		for j := 0; j < 3; j++ {
			matchCov[i*3+j] = unobservedSigma * unobservedSigma * weak[i] * weak[j]
		}
	}
	matchCov[0], matchCov[4], matchCov[8] = matchCov[0]+1e-4, matchCov[4]+1e-4, matchCov[8]+1e-6
	matchInfo, err := matchCov.Inverse()
	if err != nil {
		t.Fatalf("corridor covariance has no inverse: %v", err)
	}
	odomInfo := must(geom.Mat3{4e-4, 0, 0, 0, 4e-4, 0, 0, 0, 1e-4}.Inverse())(t) // 2 cm, 0.01 rad

	g := New(Config{})
	a := must(g.AddNode("r", geom.Pose2{}))(t)
	b := must(g.AddNode("r", geom.Pose2{X: 1}))(t)
	must(0, g.SetFixed(a, true))(t)
	odom := geom.Pose2{X: 1.00, Z: 0.05, Yaw: 0.020}
	match := geom.Pose2{X: 0.40, Z: 0.02, Yaw: 0.001} // arbitrary along x, precise elsewhere
	must(g.AddEdge(Relative(a, b, odom, odomInfo)))(t)
	must(g.AddEdge(Relative(a, b, match, matchInfo)))(t)
	if _, err := g.Optimize(); err != nil {
		t.Fatal(err)
	}
	got := must(g.Pose(b))(t)
	if math.Abs(got.X-odom.X) > maxAlongError {
		t.Errorf("along the corridor x = %.4f, want odometry's %.2f", got.X, odom.X)
	}
	// From a fixed identity node the problem is linear, so the optimum is the
	// information-weighted mean of the two measurements.
	vec := func(p geom.Pose2) geom.Vec3 { return geom.Vec3{X: p.X, Y: p.Z, Z: p.Yaw} }
	sum := must(odomInfo.Add(matchInfo).Inverse())(t)
	want := sum.MulVec(odomInfo.MulVec(vec(odom)).Add(matchInfo.MulVec(vec(match))))
	if d := vec(got).Sub(want); math.Abs(d.X) > maxFusionError || math.Abs(d.Y) > maxFusionError || math.Abs(d.Z) > maxFusionError {
		t.Errorf("optimum %+v, want the weighted mean %+v", got, want)
	}
	// Across the corridor and in heading the match is five to ten times
	// stronger than odometry, so the result sits near the match's line.
	shift := (got.X - match.X) / weak[0]
	if dz, dOdom := math.Abs(got.Z-(match.Z+shift*weak[1])), math.Abs(got.Z-odom.Z); dz > dOdom/2 {
		t.Errorf("across the corridor z = %.4f is not held by the scan match", got.Z)
	}
}
