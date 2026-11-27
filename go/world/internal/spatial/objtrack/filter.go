package objtrack

import (
	"math"

	"github.com/wendylabsinc/wendy/go/world/internal/spatial/geom"
)

// filter is a constant-velocity Kalman filter over (x, z, vx, vz) in the
// site frame, driven by continuous white-noise acceleration. p is row-major,
// the layout of spatial.Track.Covariance.
//
// ponytail: one motion model. A turn or a stop is absorbed by ProcessNoise,
// which costs smoothing on straight legs; an interacting multiple model
// (constant velocity plus stopped) is the upgrade if RMSE on real walkers
// needs it.
//
// ponytail: measurement errors are taken as white. Most of a camera
// detection's error is its calibration, the same in every frame, so a track
// seen by one distant camera is as biased as that camera and its covariance
// understates it. Per-sensor consider states (Schmidt-Kalman) are the
// upgrade.
type filter struct {
	x [4]float64
	p [16]float64
}

// newFilter starts at a position with no velocity knowledge beyond speedSigma.
func newFilter(z geom.Vec2, r geom.Cov2, speedSigma float64) filter {
	v := speedSigma * speedSigma
	return filter{
		x: [4]float64{z.X, z.Z, 0, 0},
		p: [16]float64{
			r[0], r[1], 0, 0,
			r[2], r[3], 0, 0,
			0, 0, v, 0,
			0, 0, 0, v,
		},
	}
}

func (f *filter) pos() geom.Vec2    { return geom.Vec2{X: f.x[0], Z: f.x[1]} }
func (f *filter) vel() geom.Vec2    { return geom.Vec2{X: f.x[2], Z: f.x[3]} }
func (f *filter) posCov() geom.Cov2 { return geom.Cov2{f.p[0], f.p[1], f.p[4], f.p[5]} }

// predict advances the state by dt seconds with acceleration density q
// (m²/s³): P = F·P·Fᵀ + Q with Q = q·[dt³/3, dt²/2; dt²/2, dt] per axis.
func (f *filter) predict(dt, q float64) {
	if !(dt > 0) {
		return
	}
	f.x[0] += dt * f.x[2]
	f.x[1] += dt * f.x[3]
	p := &f.p
	for j := 0; j < 4; j++ { // rows of F·P
		p[j] += dt * p[8+j]
		p[4+j] += dt * p[12+j]
	}
	for i := 0; i < 4; i++ { // columns of (F·P)·Fᵀ
		p[4*i] += dt * p[4*i+2]
		p[4*i+1] += dt * p[4*i+3]
	}
	q3, q2, q1 := q*dt*dt*dt/3, q*dt*dt/2, q*dt
	p[0] += q3
	p[5] += q3
	p[2] += q2
	p[8] += q2
	p[7] += q2
	p[13] += q2
	p[10] += q1
	p[15] += q1
}

// stop removes the velocity and its uncertainty: with no process noise the
// state then stays a fixed position, refined by further measurements.
func (f *filter) stop() {
	f.x[2], f.x[3] = 0, 0
	for i := 0; i < 4; i++ {
		for j := 2; j < 4; j++ {
			f.p[4*i+j], f.p[4*j+i] = 0, 0
		}
	}
}

// distance is the squared Mahalanobis distance of a position measurement
// from the predicted position, and the log determinant of the innovation
// covariance S it is measured in: d² + ln|S| is minus twice the log
// likelihood of the measurement, up to a constant. ok is false when S is not
// invertible.
func (f *filter) distance(z geom.Vec2, r geom.Cov2) (d2, logDet float64, ok bool) {
	s := f.posCov().Add(r)
	d2, err := geom.MahalanobisSq2(z.Sub(f.pos()), s)
	det := s.Det()
	return d2, math.Log(det), err == nil && !math.IsNaN(d2) && det > 0
}

// update folds in a position measurement z with covariance r. It reports
// false, and leaves the state alone, when the innovation covariance is
// singular.
func (f *filter) update(z geom.Vec2, r geom.Cov2) bool {
	si, err := f.posCov().Add(r).Inverse()
	if err != nil {
		return false
	}
	y := z.Sub(f.pos())
	p := &f.p
	var k [4][2]float64 // K = P·Hᵀ·S⁻¹, H = [I 0]
	for i := 0; i < 4; i++ {
		k[i][0] = p[4*i]*si[0] + p[4*i+1]*si[2]
		k[i][1] = p[4*i]*si[1] + p[4*i+1]*si[3]
		f.x[i] += k[i][0]*y.X + k[i][1]*y.Z
	}
	// P ← P − K·H·P, then symmetrized against round-off.
	h := [2][4]float64{{p[0], p[1], p[2], p[3]}, {p[4], p[5], p[6], p[7]}}
	for i := 0; i < 4; i++ {
		for j := 0; j < 4; j++ {
			p[4*i+j] -= k[i][0]*h[0][j] + k[i][1]*h[1][j]
		}
	}
	for i := 0; i < 4; i++ {
		for j := i + 1; j < 4; j++ {
			m := (p[4*i+j] + p[4*j+i]) / 2
			p[4*i+j], p[4*j+i] = m, m
		}
	}
	return true
}

// assign solves the rectangular assignment problem: it returns, for each
// row of cost, a distinct column such that the total cost is least. Every
// row must have at least as many columns as there are rows. Kuhn-Munkres
// with potentials, O(rows²·columns).
func assign(cost [][]float64) []int {
	n := len(cost)
	if n == 0 {
		return nil
	}
	m := len(cost[0])
	u, v := make([]float64, n+1), make([]float64, m+1)
	rowOf, way := make([]int, m+1), make([]int, m+1) // 1-based; rowOf[0] is the row being placed
	minv, used := make([]float64, m+1), make([]bool, m+1)
	for i := 1; i <= n; i++ {
		rowOf[0] = i
		j0 := 0
		for j := range minv {
			minv[j], used[j] = math.Inf(1), false
		}
		for {
			used[j0] = true
			i0, delta, j1 := rowOf[j0], math.Inf(1), 0
			for j := 1; j <= m; j++ {
				if used[j] {
					continue
				}
				if c := cost[i0-1][j-1] - u[i0] - v[j]; c < minv[j] {
					minv[j], way[j] = c, j0
				}
				if minv[j] < delta {
					delta, j1 = minv[j], j
				}
			}
			for j := 0; j <= m; j++ {
				if used[j] {
					u[rowOf[j]] += delta
					v[j] -= delta
				} else {
					minv[j] -= delta
				}
			}
			j0 = j1
			if rowOf[j0] == 0 {
				break
			}
		}
		for j0 != 0 {
			j1 := way[j0]
			rowOf[j0] = rowOf[j1]
			j0 = j1
		}
	}
	out := make([]int, n)
	for j := 1; j <= m; j++ {
		if rowOf[j] > 0 {
			out[rowOf[j]-1] = j - 1
		}
	}
	return out
}

// match assigns rows to columns given the cost of each admissible pair
// (ok false = not admissible) and the cost of leaving a row unassigned.
// It returns the column of each row, or -1. A pair is used only when it
// costs no more than leaving its row unassigned, and among such choices the
// total is least: the global nearest neighbour.
//
// ponytail: one hypothesis per batch. Two bodies merged into one cluster, or
// swapping places inside each other's gates, can exchange identities; joint
// probabilistic or multi-hypothesis association is the upgrade.
func match(rows, cols int, unassigned float64, pair func(i, j int) (float64, bool)) []int {
	if rows == 0 {
		return nil
	}
	const forbidden = 1e18
	cost := make([][]float64, rows)
	for i := range cost {
		cost[i] = make([]float64, cols+rows)
		for j := range cost[i] {
			cost[i][j] = unassigned // columns past cols are "no track"
			if j < cols {
				cost[i][j] = forbidden
				if c, ok := pair(i, j); ok && c <= unassigned {
					cost[i][j] = c
				}
			}
		}
	}
	out := assign(cost)
	for i, j := range out {
		if j >= cols || cost[i][j] >= forbidden {
			out[i] = -1
		}
	}
	return out
}
