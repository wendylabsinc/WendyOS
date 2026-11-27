package fusion

import (
	"bytes"
	"errors"
	"image"
	"math"

	"github.com/wendylabsinc/wendy/go/world/internal/sensing"
)

// CameraObservation describes the largest connected red region in the actual
// image. Coordinates use image pixel centres and the sample's site frame.
type CameraObservation struct {
	Pixels              int        `json:"pixels"`
	CentroidPX          [2]float64 `json:"centroid_px"`
	BoundsPX            [4]int     `json:"bounds_px"`
	RayOriginM          [3]float64 `json:"ray_origin_m"`
	RayDirection        [3]float64 `json:"ray_direction"`
	ReprojectionErrorPX float64    `json:"reprojection_error_px"`
}

type SpatialFeatures struct {
	Associated            bool            `json:"associated"`
	Reason                string          `json:"reason"`
	PositionM             [3]float64      `json:"position_m"`
	TriangulatedM         [3]float64      `json:"triangulated_m"`
	UncertaintyM          float64         `json:"uncertainty_m"`
	RaySeparationM        float64         `json:"ray_separation_m"`
	RayAngleDeg           float64         `json:"ray_angle_deg"`
	ReprojectionErrorPX   float64         `json:"reprojection_error_px"`
	Cameras               []CameraFeature `json:"cameras"`
	LidarPointsConsidered int             `json:"lidar_points_considered"`
	LidarSupportCount     int             `json:"lidar_support_count"`
	LidarSupportPoints    [][3]float64    `json:"lidar_support_points"`
}

type redObservation struct {
	feature       CameraObservation
	fraction      float64
	mask          []bool
	width, height int
}
type spatialCamera struct {
	sample      sensing.Sample
	observation redObservation
}

func observeRed(data []byte, camera sensing.Camera, contentType string) (redObservation, error) {
	var out redObservation
	if len(data) == 0 || len(data) > sensing.MaxEvidenceBytes {
		return out, errors.New("image bounds")
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || cfg.Width != camera.Width || cfg.Height != camera.Height || cfg.Width < 1 || cfg.Height < 1 || cfg.Width > 1024 || cfg.Height > 1024 || (format != "png" && format != "jpeg") || format == "png" && contentType != "image/png" || format == "jpeg" && contentType != "image/jpeg" {
		return out, errors.New("image calibration or type differs")
	}
	im, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return out, err
	}
	w, h := cfg.Width, cfg.Height
	red := make([]bool, w*h)
	seen := make([]bool, w*h)
	total := 0
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			r, g, b, a := im.At(im.Bounds().Min.X+x, im.Bounds().Min.Y+y).RGBA()
			if a == 65535 && r > 120*257 && float64(r) > 1.8*float64(g) && float64(r) > 1.8*float64(b) {
				red[y*w+x] = true
				total++
			}
		}
	}
	var largest []int
	queue := make([]int, 0, w*h)
	for first, on := range red {
		if !on || seen[first] {
			continue
		}
		queue = append(queue[:0], first)
		seen[first] = true
		for at := 0; at < len(queue); at++ {
			index := queue[at]
			x, y := index%w, index/w
			for dy := -1; dy <= 1; dy++ {
				for dx := -1; dx <= 1; dx++ {
					nx, ny := x+dx, y+dy
					if nx < 0 || nx >= w || ny < 0 || ny >= h {
						continue
					}
					n := ny*w + nx
					if red[n] && !seen[n] {
						seen[n] = true
						queue = append(queue, n)
					}
				}
			}
		}
		if len(queue) > len(largest) {
			largest = append(largest[:0], queue...)
		}
	}
	out = redObservation{width: w, height: h, mask: make([]bool, w*h), fraction: float64(total) / float64(w*h)}
	out.feature.Pixels = len(largest)
	if len(largest) == 0 {
		return out, nil
	}
	bounds := [4]int{w, h, 0, 0}
	for _, index := range largest {
		x, y := index%w, index/w
		out.mask[index] = true
		out.feature.CentroidPX[0] += float64(x) + .5
		out.feature.CentroidPX[1] += float64(y) + .5
		bounds[0] = min(bounds[0], x)
		bounds[1] = min(bounds[1], y)
		bounds[2] = max(bounds[2], x+1)
		bounds[3] = max(bounds[3], y+1)
	}
	for i := range out.feature.CentroidPX {
		out.feature.CentroidPX[i] /= float64(len(largest))
	}
	out.feature.BoundsPX = bounds
	m := camera.CameraToSite
	out.feature.RayOriginM = [3]float64{m[3], m[7], m[11]}
	v := [3]float64{(out.feature.CentroidPX[0] - camera.CX) / camera.FX, (out.feature.CentroidPX[1] - camera.CY) / camera.FY, 1}
	for i := 0; i < 3; i++ {
		out.feature.RayDirection[i] = m[i*4]*v[0] + m[i*4+1]*v[1] + m[i*4+2]*v[2]
	}
	out.feature.RayDirection = scale(out.feature.RayDirection, 1/norm(out.feature.RayDirection))
	return out, nil
}

func add(a, b [3]float64) [3]float64           { return [3]float64{a[0] + b[0], a[1] + b[1], a[2] + b[2]} }
func sub(a, b [3]float64) [3]float64           { return [3]float64{a[0] - b[0], a[1] - b[1], a[2] - b[2]} }
func scale(a [3]float64, s float64) [3]float64 { return [3]float64{a[0] * s, a[1] * s, a[2] * s} }
func dot(a, b [3]float64) float64              { return a[0]*b[0] + a[1]*b[1] + a[2]*b[2] }
func norm(a [3]float64) float64                { return math.Sqrt(dot(a, a)) }

func project(camera sensing.Camera, p [3]float64) ([2]float64, bool) {
	m := camera.CameraToSite
	d := sub(p, [3]float64{m[3], m[7], m[11]})
	var v [3]float64
	for i := 0; i < 3; i++ {
		v[i] = m[i]*d[0] + m[4+i]*d[1] + m[8+i]*d[2]
	}
	if v[2] <= .01 {
		return [2]float64{}, false
	}
	return [2]float64{camera.FX*v[0]/v[2] + camera.CX, camera.FY*v[1]/v[2] + camera.CY}, true
}
func (o redObservation) supports(p [2]float64) bool {
	x, y := int(math.Floor(p[0])), int(math.Floor(p[1]))
	// A two-pixel rim tolerates rasterization at the observed silhouette.
	for dy := -2; dy <= 2; dy++ {
		for dx := -2; dx <= 2; dx++ {
			nx, ny := x+dx, y+dy
			if nx >= 0 && nx < o.width && ny >= 0 && ny < o.height && o.mask[ny*o.width+nx] {
				return true
			}
		}
	}
	return false
}

func localize(cameras []spatialCamera, lidars []sensing.Sample) SpatialFeatures {
	out := SpatialFeatures{Reason: "red fixture needs two camera observations and LiDAR support", Cameras: []CameraFeature{}, LidarSupportPoints: [][3]float64{}}
	for _, c := range cameras {
		f := c.observation.feature
		out.Cameras = append(out.Cameras, CameraFeature{SensorID: c.sample.SensorID, RedFraction: c.observation.fraction, Observation: &f})
	}
	if len(cameras) != 2 || len(lidars) == 0 || cameras[0].observation.feature.Pixels < 4 || cameras[1].observation.feature.Pixels < 4 {
		return out
	}
	a, b := cameras[0].observation.feature, cameras[1].observation.feature
	u, v := a.RayDirection, b.RayDirection
	w := sub(a.RayOriginM, b.RayOriginM)
	cosine := dot(u, v)
	den := 1 - cosine*cosine
	out.RayAngleDeg = math.Acos(math.Min(1, math.Abs(cosine))) * 180 / math.Pi
	if den < math.Pow(math.Sin(3*math.Pi/180), 2) {
		out.Reason = "camera rays do not constrain depth"
		return out
	}
	ta, tb := (cosine*dot(v, w)-dot(u, w))/den, (dot(v, w)-cosine*dot(u, w))/den
	if ta <= 0 || tb <= 0 {
		out.Reason = "camera association is behind an observer"
		return out
	}
	pa, pb := add(a.RayOriginM, scale(u, ta)), add(b.RayOriginM, scale(v, tb))
	out.TriangulatedM = scale(add(pa, pb), .5)
	out.RaySeparationM = norm(sub(pa, pb))
	measurementError := math.Max(cameras[0].sample.UncertaintyM, cameras[1].sample.UncertaintyM)
	if out.RaySeparationM > .35+2*measurementError {
		out.Reason = "camera rays disagree spatially"
		return out
	}
	for _, scan := range lidars {
		measurementError = math.Max(measurementError, scan.UncertaintyM)
	}
	var sum [3]float64
	for _, scan := range lidars {
		origin := [3]float64{scan.PositionM[0], scan.PositionM[1], scan.PositionM[2]}
		for _, point := range scan.Lidar.PointsM {
			out.LidarPointsConsidered++
			if norm(sub(point, origin)) > scan.Lidar.MaxRangeM+scan.UncertaintyM || norm(sub(point, out.TriangulatedM)) > .65+measurementError {
				continue
			}
			matched := true
			for _, c := range cameras {
				pixel, front := project(*c.sample.Camera, point)
				if !front || !c.observation.supports(pixel) {
					matched = false
					break
				}
			}
			if matched {
				out.LidarSupportPoints = append(out.LidarSupportPoints, point)
				sum = add(sum, point)
			}
		}
	}
	out.LidarSupportCount = len(out.LidarSupportPoints)
	if len(out.LidarSupportPoints) == 0 {
		out.Reason = "no LiDAR return associates with both observed camera regions"
		return out
	}
	mean := scale(sum, 1/float64(len(out.LidarSupportPoints)))
	out.PositionM = scale(add(out.TriangulatedM, mean), .5)
	spread := 0.0
	for _, p := range out.LidarSupportPoints {
		d := norm(sub(p, mean))
		spread += d * d
	}
	spread = math.Sqrt(spread / float64(len(out.LidarSupportPoints)))
	pixelError := math.Max(ta/cameras[0].sample.Camera.FX, tb/cameras[1].sample.Camera.FX) / math.Sqrt(den)
	out.UncertaintyM = measurementError + .5*out.RaySeparationM + spread + pixelError + norm(sub(mean, out.TriangulatedM))*.5
	for i, c := range cameras {
		pixel, front := project(*c.sample.Camera, out.PositionM)
		residual := math.Hypot(pixel[0]-c.observation.feature.CentroidPX[0], pixel[1]-c.observation.feature.CentroidPX[1])
		out.Cameras[i].Observation.ReprojectionErrorPX = residual
		out.ReprojectionErrorPX = math.Max(out.ReprojectionErrorPX, residual)
		bounds := c.observation.feature.BoundsPX
		if !front || residual > math.Max(6, .6*math.Hypot(float64(bounds[2]-bounds[0]), float64(bounds[3]-bounds[1]))) {
			out.Reason = "camera and LiDAR reprojection disagree"
			return out
		}
	}
	out.Associated = true
	out.Reason = "two camera rays and observed LiDAR surfaces agree"
	return out
}
