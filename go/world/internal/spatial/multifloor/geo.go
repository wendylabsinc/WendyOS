package multifloor

import (
	"errors"
	"fmt"
	"math"
	"sort"

	"github.com/wendylabsinc/wendy/go/world/internal/geography"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/geom"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

// ErrBaseline reports GNSS fixes that do not pin the heading of the site
// frame: too few of them, too noisy, or taken too close together.
var ErrBaseline = errors.New("multifloor: GNSS baseline too short")

// GeoMethod is the Method of the geo references this package estimates.
const GeoMethod = "gnss-trajectory-alignment"

// GeoConfig tunes Georeference. A zero field takes its default.
type GeoConfig struct {
	// MinFixes is the fewest fixes at full weight a reference is built from.
	// Default 10.
	MinFixes int
	// MaxYawSigmaRad is the largest predicted 1σ heading error accepted. The
	// heading is known to about (fix noise) / (spread of the fixes · √count),
	// so this is the baseline requirement. Default 0.05 rad (3°).
	MaxYawSigmaRad float64
	// HuberSigmas is the horizontal residual, in units of a fix's reported
	// accuracy, beyond which the fix is down-weighted as multipath. Default 2.
	HuberSigmas float64
	// BiasM is the slowly varying GNSS error that averaging fixes does not
	// remove (atmosphere, multipath, orbit error). It is a floor on the
	// reported uncertainty. Default 0.5 m, typical of a standalone receiver.
	BiasM float64
}

func (c GeoConfig) withDefaults() GeoConfig {
	if c.MinFixes == 0 {
		c.MinFixes = 10
	}
	if c.MaxYawSigmaRad == 0 {
		c.MaxYawSigmaRad = 0.05
	}
	if c.HuberSigmas == 0 {
		c.HuberSigmas = 2
	}
	if c.BiasM == 0 {
		c.BiasM = 0.5
	}
	return c
}

// GeoSample is a GNSS fix with the site-frame position (x, elevation, z) of
// the receiver when it was taken.
type GeoSample struct {
	Site geom.Vec3
	Fix  spatial.GnssFix
}

// GeoFit is a geo reference with the evidence behind it.
type GeoFit struct {
	// Reference ties the centroid of the sampled site positions (its
	// PositionM) to a geographic coordinate (its Origin). UncertaintyM is the
	// 1σ horizontal error of that tie and YawSigmaRad the 1σ heading error:
	// a site point d metres from PositionM is placed on the Earth to about
	// hypot(UncertaintyM, d·YawSigmaRad).
	Reference spatial.GeoReference
	// RMSE is the weighted horizontal residual of the fixes, metres.
	RMSE float64
	// BaselineM is the RMS distance of the sampled positions from their
	// centroid.
	BaselineM float64
	// Inliers is the number of fixes that kept full weight.
	Inliers int
	// Inputs are the InputIDs of the earliest and latest fix of every
	// receiver, sorted.
	Inputs []string
}

// irlsRounds is the number of reweighting rounds; the Huber fit of a rigid
// 2D alignment is settled well before.
const irlsRounds = 8

// Georeference aligns site-frame positions with the GNSS fixes taken there:
// a rigid 2D fit (rotation and translation, no scale) in the local tangent
// plane of package geography, robust to outlying fixes. It returns
// geom.ErrInsufficient with fewer than MinFixes good fixes and ErrBaseline
// when the fixes do not determine the heading to MaxYawSigmaRad.
func Georeference(samples []GeoSample, cfg GeoConfig) (GeoFit, error) {
	cfg = cfg.withDefaults()
	n := len(samples)
	if n < cfg.MinFixes {
		return GeoFit{}, fmt.Errorf("multifloor: %d GNSS fixes, need %d: %w", n, cfg.MinFixes, geom.ErrInsufficient)
	}
	src, dst, base, w := make([]geom.Vec2, n), make([]geom.Vec2, n), make([]float64, n), make([]float64, n)
	var siteY, alt float64
	for i, s := range samples {
		if err := s.Fix.Validate(); err != nil {
			return GeoFit{}, err
		}
		src[i], base[i] = s.Site.XZ(), 1/(s.Fix.HorizontalAccuracy*s.Fix.HorizontalAccuracy)
		siteY, alt = siteY+s.Site.Y/float64(n), alt+s.Fix.AltitudeMetres/float64(n)
	}
	anchor := world.GeographicAnchor{
		LatitudeDegrees: samples[0].Fix.LatitudeDegrees, LongitudeDegrees: samples[0].Fix.LongitudeDegrees,
		AltitudeMetres: alt, AltitudeDatum: geography.WGS84Ellipsoid,
	}
	var (
		fit                     geom.Pose2
		rmse, sumW, spread, chi float64
		centroid                geom.Vec2
		inliers                 int
	)
	// Two passes: the second works in the tangent plane at the answer of the
	// first, so SiteYaw is the heading at Origin and not at the first fix
	// (the two planes differ by the convergence of the meridians).
	for pass := 0; pass < 2; pass++ {
		tangent, err := geography.NewTangent(anchor)
		if err != nil {
			return GeoFit{}, err
		}
		for i, s := range samples {
			local, err := tangent.ToLocal(coordinate(s.Fix))
			if err != nil {
				return GeoFit{}, err
			}
			dst[i] = geom.Vec2{X: local[0], Z: local[2]}
		}
		copy(w, base)
		for round := 0; ; round++ {
			if fit, rmse, err = geom.AlignPoints2(src, dst, w); err != nil {
				return GeoFit{}, fmt.Errorf("%w: %w", ErrBaseline, err)
			}
			sumW, chi, inliers, centroid = 0, 0, 0, geom.Vec2{}
			for i := range src {
				r := fit.Apply(src[i]).Dist(dst[i]) * math.Sqrt(base[i])
				h := geom.HuberWeight(r, cfg.HuberSigmas)
				if round < irlsRounds {
					w[i] = base[i] * h
				}
				if h == 1 {
					inliers, chi = inliers+1, chi+r*r
				}
				sumW, centroid = sumW+w[i], centroid.Add(src[i].Scale(w[i]))
			}
			if round == irlsRounds {
				break
			}
		}
		centroid = centroid.Scale(1 / sumW)
		spread = 0
		for i := range src {
			spread += w[i] * src[i].Sub(centroid).Norm2()
		}
		at := fit.Apply(centroid)
		origin, err := tangent.ToGeographic([3]float64{at.X, 0, at.Z})
		if err != nil {
			return GeoFit{}, err
		}
		anchor.LatitudeDegrees, anchor.LongitudeDegrees = origin.LatitudeDegrees, origin.LongitudeDegrees
	}
	if inliers < cfg.MinFixes {
		return GeoFit{}, fmt.Errorf("multifloor: %d of %d GNSS fixes agree, need %d: %w", inliers, n, cfg.MinFixes, geom.ErrInsufficient)
	}
	// The noise of the fixes relative to what the receivers report, from the
	// fixes at full weight. Their squared residuals are a chi-square of two
	// degrees cut off at k², whose mean is short of 2 by the factor below.
	// The residuals may show more noise than reported, never less: a short
	// run of fixes that happen to agree proves nothing.
	k2 := cfg.HuberSigmas * cfg.HuberSigmas
	cut := (2 - (k2+2)*math.Exp(-k2/2)) / (1 - math.Exp(-k2/2)) / 2
	scale := math.Max(1, chi/(cut*float64(2*inliers-3)))
	yawSigma := math.Sqrt(scale / spread)
	if !(yawSigma <= cfg.MaxYawSigmaRad) {
		return GeoFit{}, fmt.Errorf("%w: heading known to %.3f rad over a %.1f m spread of %d fixes, need %.3f", ErrBaseline, yawSigma, math.Sqrt(spread/sumW), n, cfg.MaxYawSigmaRad)
	}
	uncertainty := math.Hypot(math.Sqrt(2*scale/sumW), cfg.BiasM)
	anchor.UncertaintyMetres = uncertainty
	return GeoFit{
		Reference: spatial.GeoReference{
			Origin: anchor, SiteYaw: geom.WrapAngle(fit.Yaw), YawSigmaRad: yawSigma, Method: GeoMethod, Fixes: n,
			Location: spatial.At(centroid.Lift(siteY), uncertainty),
		},
		RMSE: rmse, BaselineM: math.Sqrt(spread / sumW), Inliers: inliers, Inputs: fixSpan(samples),
	}, nil
}

func coordinate(f spatial.GnssFix) geography.Coordinate {
	return geography.Coordinate{LatitudeDegrees: f.LatitudeDegrees, LongitudeDegrees: f.LongitudeDegrees, AltitudeMetres: f.AltitudeMetres, AltitudeDatum: geography.WGS84Ellipsoid}
}

// fixSpan is the InputIDs of each receiver's earliest and latest fix.
func fixSpan(samples []GeoSample) []string {
	type key struct {
		dev spatial.DeviceID
		sen spatial.SensorID
	}
	span := map[key][2]int{}
	for i, s := range samples {
		k := key{s.Fix.Device, s.Fix.Sensor}
		v, ok := span[k]
		if !ok || s.Fix.Time.Before(samples[v[0]].Fix.Time) {
			v[0] = i
		}
		if !ok || !s.Fix.Time.Before(samples[v[1]].Fix.Time) {
			v[1] = i
		}
		span[k] = v
	}
	var ids []string
	for _, v := range span {
		ids = append(ids, spatial.InputID(samples[v[0]].Fix))
		if v[1] != v[0] {
			ids = append(ids, spatial.InputID(samples[v[1]].Fix))
		}
	}
	sort.Strings(ids)
	return ids
}

// siteOf maps a fix into the site frame through ref, whose tangent plane the
// caller built once with geography.NewTangent(ref.Origin).
func siteOf(ref spatial.GeoReference, tangent *geography.Tangent, f spatial.GnssFix) (geom.Vec2, error) {
	local, err := tangent.ToLocal(coordinate(f))
	if err != nil {
		return geom.Vec2{}, err
	}
	return ref.SiteToTangent().InverseApply(geom.Vec2{X: local[0], Z: local[2]}), nil
}
