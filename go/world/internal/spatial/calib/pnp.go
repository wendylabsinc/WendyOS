package calib

import (
	"fmt"
	"math"
	"math/rand/v2"

	"github.com/wendylabsinc/wendy/go/world/internal/spatial/geom"
)

// dltSample is the minimal sample of the linear projection solve.
const dltSample = 6

// dlt returns the 3x4 projection matrix P, row-major and up to scale, with
// pixel ~ P·(X, 1), by the normalised direct linear transform. Coplanar
// points leave P undetermined: geom.ErrDegenerate.
func dlt(pts []geom.Vec3, img []geom.Pixel) (p [12]float64, err error) {
	n := len(pts)
	if n < dltSample {
		return p, geom.ErrInsufficient
	}
	var c geom.Vec3
	for _, x := range pts {
		c = c.Add(x)
	}
	c = c.Scale(1 / float64(n))
	d := 0.0
	for _, x := range pts {
		d += x.Dist(c)
	}
	if !(d > 0) || math.IsInf(d, 0) {
		return p, geom.ErrDegenerate
	}
	s3 := math.Sqrt(3) * float64(n) / d
	mu, mv, s2, err := similarity2(n, func(i int) (float64, float64) { return img[i].U, img[i].V })
	if err != nil {
		return p, err
	}
	ata := geom.NewMat(12, 12)
	for i, x := range pts {
		x = x.Sub(c).Scale(s3)
		u, v := (img[i].U-mu)*s2, (img[i].V-mv)*s2
		for _, row := range [2][12]float64{
			{x.X, x.Y, x.Z, 1, 0, 0, 0, 0, -u * x.X, -u * x.Y, -u * x.Z, -u},
			{0, 0, 0, 0, x.X, x.Y, x.Z, 1, -v * x.X, -v * x.Y, -v * x.Z, -v},
		} {
			for j, a := range row {
				if a == 0 {
					continue
				}
				for k, b := range row {
					ata.Data[j*12+k] += a * b
				}
			}
		}
	}
	pn, gap, err := nullVector(ata)
	if err != nil {
		return p, err
	}
	if gap < rankTol {
		return p, geom.ErrDegenerate
	}
	// Undo the normalisations: P = T2⁻¹·Pn·T3.
	for r := 0; r < 3; r++ {
		a, b, cc, dd := pn[r*4], pn[r*4+1], pn[r*4+2], pn[r*4+3]
		pn[r*4], pn[r*4+1], pn[r*4+2], pn[r*4+3] = s3*a, s3*b, s3*cc, dd-s3*(a*c.X+b*c.Y+cc*c.Z)
	}
	for k := 0; k < 4; k++ {
		p[k] = pn[k]/s2 + mu*pn[8+k]
		p[4+k] = pn[4+k]/s2 + mv*pn[8+k]
		p[8+k] = pn[8+k]
	}
	return p, nil
}

// decompose splits a projection matrix K·[R|t] into a camera by an RQ
// factorisation, dropping the skew.
func decompose(p [12]float64, width, height int) (camera, error) {
	m := geom.Mat3{p[0], p[1], p[2], p[4], p[5], p[6], p[8], p[9], p[10]}
	t := geom.Vec3{X: p[3], Y: p[7], Z: p[11]}
	det := m.Det()
	if det == 0 || math.IsNaN(det) {
		return camera{}, geom.ErrDegenerate
	}
	if det < 0 { // P is defined up to sign; points must end up in front
		m, t = m.Scale(-1), t.Scale(-1)
	}
	k, q, err := geom.RQ3(m)
	if err != nil {
		return camera{}, geom.ErrDegenerate
	}
	kinv, err := k.Inverse()
	if err != nil {
		return camera{}, geom.ErrDegenerate
	}
	cam := camera{
		in:   geom.Intrinsics{FX: k[0] / k[8], FY: k[4] / k[8], CX: k[2] / k[8], CY: k[5] / k[8], Width: width, Height: height},
		pose: geom.Pose3{R: q.T(), T: q.T().MulVec(kinv.MulVec(t)).Scale(-1)},
	}
	if cam.in.Validate() != nil || !cam.pose.R.IsRotation(1e-6) {
		return camera{}, geom.ErrDegenerate
	}
	return cam, nil
}

// p3pSamples is the number of depths at which the three-point solve brackets
// its roots along the first ray.
const p3pSamples = 96

// p3p returns the optical-to-world poses of a calibrated camera that sees
// three world points along three unit rays: up to four. With s the depth
// along ray i, the law of cosines gives one equation per pair of points. Two
// of them give s1 and s2 as functions of s0 (two branches each); the third
// is then one equation in s0, whose roots are bracketed by sampling and
// refined by bisection. Unlike a linear solve it needs no particular spread
// of the points: any three that are not collinear will do.
func p3p(rays, pts [3]geom.Vec3) []geom.Pose3 {
	a2, b2, c2 := pts[1].Sub(pts[2]).Norm2(), pts[0].Sub(pts[2]).Norm2(), pts[0].Sub(pts[1]).Norm2()
	ca, cb, cg := rays[1].Dot(rays[2]), rays[0].Dot(rays[2]), rays[0].Dot(rays[1])
	sb2, sg2 := 1-cb*cb, 1-cg*cg
	if !(sb2 > 1e-12) || !(sg2 > 1e-12) || !(a2 > 0) || !(b2 > 0) || !(c2 > 0) {
		return nil
	}
	limit := math.Sqrt(math.Min(c2/sg2, b2/sb2))
	var out []geom.Pose3
	for branch := 0; branch < 4; branch++ {
		sign1, sign2 := float64(1-2*(branch&1)), float64(1-(branch&2))
		depths := func(x float64) (s1, s2 float64) {
			return x*cg + sign1*math.Sqrt(math.Max(c2-x*x*sg2, 0)), x*cb + sign2*math.Sqrt(math.Max(b2-x*x*sb2, 0))
		}
		g := func(x float64) float64 {
			s1, s2 := depths(x)
			return s1*s1 + s2*s2 - 2*s1*s2*ca - a2
		}
		lo, glo := 0.0, g(0)
		for k := 1; k <= p3pSamples; k++ {
			hi := limit * float64(k) / p3pSamples
			ghi := g(hi)
			if (glo < 0) != (ghi < 0) {
				x0, x1, g0 := lo, hi, glo
				for i := 0; i < 60; i++ {
					mid := (x0 + x1) / 2
					if gm := g(mid); (gm < 0) == (g0 < 0) {
						x0, g0 = mid, gm
					} else {
						x1 = mid
					}
				}
				x := (x0 + x1) / 2
				if s1, s2 := depths(x); x > 0 && s1 > 0 && s2 > 0 {
					cam := []geom.Vec3{rays[0].Scale(x), rays[1].Scale(s1), rays[2].Scale(s2)}
					if worldToCam, _, err := geom.AlignPoints3(pts[:], cam, nil); err == nil {
						out = append(out, worldToCam.Inverse())
					}
				}
			}
			lo, glo = hi, ghi
		}
	}
	return out
}

// ransacCamera finds a camera that most of the correspondences agree with.
// With known intrinsics each hypothesis is a three-point solve, a fourth
// point choosing among its solutions; without, a six-point linear solve.
func ransacCamera(pts []Correspondence, known *geom.Intrinsics, width, height int, cfg Config) (camera, error) {
	// Thin a large set evenly; the sample only has to find the basin.
	idx := make([]int, 0, cfg.RansacPoints)
	step := math.Max(1, float64(len(pts))/float64(cfg.RansacPoints))
	for f := 0.0; int(f) < len(pts); f += step {
		idx = append(idx, int(f))
	}
	world, img, rays := make([]geom.Vec3, len(idx)), make([]geom.Pixel, len(idx)), make([]geom.Vec3, len(idx))
	for i, j := range idx {
		world[i], img[i] = pts[j].Point, pts[j].Pixel
		if known != nil {
			if ray, err := known.Undistort(pts[j].Pixel); err == nil {
				rays[i] = ray.Unit()
			}
		}
	}
	residual := func(c camera, i int) float64 {
		px, ok := c.in.ProjectWorld(c.pose, world[i])
		if !ok {
			return math.Inf(1)
		}
		return px.Dist(img[i])
	}
	sample := dltSample
	fit := func(sample []int) (camera, error) {
		if len(sample) != dltSample {
			return camera{}, geom.ErrDimension // the caller refines the consensus set itself
		}
		w, m := make([]geom.Vec3, len(sample)), make([]geom.Pixel, len(sample))
		for k, i := range sample {
			w[k], m[k] = world[i], img[i]
		}
		p, err := dlt(w, m)
		if err != nil {
			return camera{}, err
		}
		return decompose(p, width, height)
	}
	if known != nil {
		sample = 4
		fit = func(sample []int) (camera, error) {
			if len(sample) != 4 {
				return camera{}, geom.ErrDimension
			}
			i, j, k, check := sample[0], sample[1], sample[2], sample[3]
			if rays[i] == (geom.Vec3{}) || rays[j] == (geom.Vec3{}) || rays[k] == (geom.Vec3{}) {
				return camera{}, geom.ErrDegenerate
			}
			best, bestErr := camera{}, math.Inf(1)
			for _, pose := range p3p([3]geom.Vec3{rays[i], rays[j], rays[k]}, [3]geom.Vec3{world[i], world[j], world[k]}) {
				c := camera{in: *known, pose: pose}
				if e := residual(c, check); e < bestErr {
					best, bestErr = c, e
				}
			}
			if math.IsInf(bestErr, 1) {
				return camera{}, geom.ErrDegenerate
			}
			return best, nil
		}
	}
	rng := rand.New(rand.NewPCG(cfg.Seed, 0x63616c6962)) // "calib"
	res, err := geom.Ransac(len(idx), geom.RansacConfig{SampleSize: sample, Threshold: cfg.InlierPx, Iterations: cfg.RansacIterations, MinInliers: cfg.MinInliers}, rng, fit, residual)
	if err != nil {
		return camera{}, err
	}
	return res.Model, nil
}

func worldGroup(pts []Correspondence, sigma float64) group {
	g := group{points: make([]point, len(pts))}
	for i, c := range pts {
		g.points[i] = point{px: c.Pixel, w: 1 / sigma, p: c.Point}
	}
	return g
}

// solveCamera is the shared body of SolvePnP and Resect: a consensus camera,
// a robust refinement over every point, then a refinement over the points
// that agree with it.
func solveCamera(pts []Correspondence, known *geom.Intrinsics, width, height int, free []int, cfg Config) (Estimate, error) {
	for _, c := range pts {
		if math.IsNaN(c.Point.Norm2()+c.Pixel.U+c.Pixel.V) || math.IsInf(c.Point.Norm2()+c.Pixel.U+c.Pixel.V, 0) {
			return Estimate{}, geom.ErrNonFinite
		}
	}
	if len(pts) < cfg.MinInliers {
		return Estimate{}, fmt.Errorf("calib: %d correspondences, need %d: %w", len(pts), cfg.MinInliers, geom.ErrInsufficient)
	}
	cam, err := ransacCamera(pts, known, width, height, cfg)
	if err != nil {
		return Estimate{}, err
	}
	b := &bundle{free: free, huber: cfg.HuberSigmas, iters: cfg.MaxIterations, groups: []group{worldGroup(pts, cfg.PixelSigma)}}
	res, err := b.solve(newState(cam, 1))
	if err != nil {
		return Estimate{}, err
	}
	var inliers []int
	var kept []Correspondence
	for i, d := range res.dist {
		if d <= cfg.InlierPx {
			inliers, kept = append(inliers, i), append(kept, pts[i])
		}
	}
	if len(inliers) < cfg.MinInliers {
		return Estimate{}, fmt.Errorf("calib: %d of %d correspondences agree, need %d: %w", len(inliers), len(pts), cfg.MinInliers, geom.ErrNoConsensus)
	}
	b.groups = []group{worldGroup(kept, cfg.PixelSigma)}
	if res, err = b.solve(newState(res.state.cam, 1)); err != nil {
		return Estimate{}, err
	}
	e := b.estimate(res)
	e.Inliers = inliers
	return e, nil
}

// SolvePnP estimates the pose of a camera with known intrinsics from points
// with known site positions, such as surveyed fiducials or mapped landmarks:
// RANSAC over three-point solves, then Levenberg-Marquardt on the
// reprojection error. The points may be coplanar. Estimate.Covariance is 6x6
// over rotation and position.
//
// It returns geom.ErrInsufficient for fewer than Config.MinInliers points,
// geom.ErrNoConsensus when no camera explains that many of them and an error
// wrapping geom.ErrSingular when the pose is not observable.
func SolvePnP(in geom.Intrinsics, pts []Correspondence, cfg Config) (Estimate, error) {
	if err := in.Validate(); err != nil {
		return Estimate{}, err
	}
	return solveCamera(pts, &in, in.Width, in.Height, freePose, cfg.withDefaults())
}

// Resect estimates the intrinsics and the pose of a camera together from
// points with known site positions and nothing else: a projection matrix by
// RANSAC DLT, its RQ factorisation, then Levenberg-Marquardt over fx, fy, cx,
// cy, k1, k2 and the pose. Estimate.Covariance is 12x12.
//
// The errors are those of SolvePnP, and it needs at least seven points. The
// linear solve needs points well off a common plane: a planar or nearly
// planar set yields geom.ErrNoConsensus. Points at one depth leave the focal
// length trading against the distance; the covariance says so and a caller
// should gate on Estimate.FocalSigma and TranslationSigmaM.
//
// ponytail: no start for a near-planar scene without intrinsics. Upgrade
// path: seed the focal length from the image size and use the three-point
// solve, as AutoCalibrator does with a camera's declared intrinsics.
func Resect(pts []Correspondence, width, height int, cfg Config) (Estimate, error) {
	if width <= 0 || height <= 0 {
		return Estimate{}, geom.ErrDimension
	}
	return solveCamera(pts, nil, width, height, append(append([]int(nil), freeIntrinsics...), freePose...), cfg.withDefaults())
}
