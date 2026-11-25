package go2nav

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"os"
	"testing"
)

type mujocoRay struct {
	Direction      [3]float64  `json:"direction_world"`
	Range          *float64    `json:"range_m"`
	Endpoint       *[3]float64 `json:"reference_endpoint_world_m"`
	ReferenceRange *float64    `json:"reference_range_m"`
}
type mujocoBox struct {
	Name       string     `json:"name"`
	Size       [3]float64 `json:"half_size_m"`
	Position   [3]float64 `json:"position_m"`
	Quaternion [4]float64 `json:"quaternion_wxyz"`
}
type mujocoCase struct {
	Name       string      `json:"name"`
	Angles     [3]float64  `json:"rpy_rad"`
	Body       [3]float64  `json:"body_position_world_m"`
	Quaternion [4]float64  `json:"body_quaternion_wxyz"`
	Mount      [3]float64  `json:"mount_translation_body_m"`
	MountQ     [4]float64  `json:"mount_quaternion_wxyz"`
	Origin     [3]float64  `json:"origin_world_m"`
	AngleMin   float64     `json:"angle_min_rad"`
	Increment  float64     `json:"angle_increment_rad"`
	RangeMin   float64     `json:"range_min_m"`
	RangeMax   float64     `json:"range_max_m"`
	MinHeight  *float64    `json:"solid_height_lower_bound_m"`
	Rays       []mujocoRay `json:"rays"`
	Boxes      []mujocoBox `json:"boxes"`
}

func TestPinnedMuJoCoTiltRayGeometry(t *testing.T) {
	raw, e := os.ReadFile("../../tests/fixtures/go2_tilt_rays.json")
	if e != nil {
		t.Fatal(e)
	}
	h := sha256.Sum256(raw)
	if hex.EncodeToString(h[:]) != "7b06ec19946f96f8e42cb175c0f3fc415eb68f86d6df2dd57c2d0c51d5c9a9dd" {
		t.Fatal("review fixture changed")
	}
	var f struct {
		Schema  string       `json:"schema"`
		Version string       `json:"mujoco_version"`
		Cases   []mujocoCase `json:"cases"`
	}
	if e = json.Unmarshal(raw, &f); e != nil {
		t.Fatal(e)
	}
	if f.Schema != "wendy.world.go2-tilt-ray-fixtures.v1" || f.Version != "3.3.7" || len(f.Cases) != 14 {
		t.Fatal("wrong independent fixture")
	}
	count := 0
	for _, c := range f.Cases {
		t.Run(c.Name, func(t *testing.T) {
			s, p := tiltFixture()
			p.MaxAbsRollRad = .25
			p.MaxAbsPitchRad = .25
			s.Robot = Pose{c.Body[0], c.Body[1], c.Angles[2]}
			s.AngleMin = c.AngleMin
			s.AngleIncrement = c.Increment
			s.RangeMin = c.RangeMin
			s.RangeMax = c.RangeMax
			s.TiltedPose.QuaternionWXYZ = c.Quaternion
			s.TiltedPose.OdomPositionXYZ = c.Body
			s.TiltedPose.MountTranslationXYZ = c.Mount
			s.TiltedPose.MountQuaternionWXYZ = c.MountQ
			s.TiltedPose.BaseHeightMinM = c.Body[2] - .055
			s.TiltedPose.BaseHeightMaxM = c.Body[2] + .055
			s.Ranges = make([]float64, len(c.Rays))
			for i, r := range c.Rays {
				if r.Range == nil {
					s.Ranges[i] = math.Inf(1)
				} else {
					s.Ranges[i] = *r.Range
				}
			}
			// Transform comparison is independent of whether this environment can be
			// commissioned. Unsupported low objects are refused separately below.
			if c.MinHeight != nil && *c.MinHeight >= .5 {
				p.TiltedScan.ObstacleMinHeightM = *c.MinHeight
			}
			proj, e := projectTilted(s, p)
			if e != nil {
				t.Fatal(e)
			}
			if distance(proj.origin, Point{c.Origin[0], c.Origin[1]}) > 1e-12 {
				t.Fatal("full mount differs from pinned Lidar origin")
			}
			rot, _ := quaternionRotation(c.Quaternion)
			mount := rot.apply(c.Mount)
			if math.Abs(c.Body[2]+mount[2]-c.Origin[2]) > 1e-12 {
				t.Fatal("mount height differs")
			}
			for i, ray := range c.Rays {
				a := c.AngleMin + float64(i)*c.Increment
				dir := rot.apply([3]float64{math.Cos(a), math.Sin(a), 0})
				for k := range dir {
					if math.Abs(dir[k]-ray.Direction[k]) > 2e-13 {
						t.Fatalf("ray%d rotation mismatch", i)
					}
				}
				bearing := math.Atan2(dir[1], dir[0])
				if bearing < 0 {
					bearing += 2 * math.Pi
				}
				var projected *projectedRay
				for j := range proj.rays {
					if math.Abs(normalize(proj.rays[j].angle-bearing)) < 1e-12 {
						projected = &proj.rays[j]
						break
					}
				}
				if projected == nil {
					t.Fatal("projected ray missing")
				}
				if ray.Range == nil {
					if projected.support != 0 || projected.finiteReturn {
						t.Fatal("dropout gained support")
					}
				} else {
					if ray.Endpoint == nil || ray.ReferenceRange == nil {
						t.Fatal("reference absent")
					}
					if distance(projected.end, Point{ray.Endpoint[0], ray.Endpoint[1]}) > 5e-7 {
						t.Fatal("projected return differs from independent mj_ray")
					}
				}
				count++
			}
			if c.MinHeight == nil {
				return
			} // No inferred environment commissioning.
			p.TiltedScan.ObstacleMinHeightM = *c.MinHeight
			g, e := Build(s, Bounds{Point{-4, -4}, Point{4, 4}}, p)
			if *c.MinHeight < .5 {
				if e == nil {
					t.Fatal("low-obstacle environment silently commissioned")
				}
				return
			}
			if e != nil {
				t.Fatal(e)
			}
			for _, box := range c.Boxes {
				_, _, yaw, e := QuaternionRPY(box.Quaternion)
				if e != nil {
					t.Fatal(e)
				}
				for c, state := range g.cells {
					if state != free {
						continue
					}
					q := center(c)
					dx, dy := q.X-box.Position[0], q.Y-box.Position[1]
					cs, sn := math.Cos(yaw), math.Sin(yaw)
					// Independent rectangle-cell SAT. A free cell cannot intersect any of
					// the source fixture's floor-connected solid obstacle columns.
					overlap := math.Abs(dx) < math.Abs(cs)*box.Size[0]+math.Abs(sn)*box.Size[1]+CellSize/2-1e-10 && math.Abs(dy) < math.Abs(sn)*box.Size[0]+math.Abs(cs)*box.Size[1]+CellSize/2-1e-10 && math.Abs(dx*cs+dy*sn) < box.Size[0]+CellSize/2*(math.Abs(cs)+math.Abs(sn))-1e-10 && math.Abs(-dx*sn+dy*cs) < box.Size[1]+CellSize/2*(math.Abs(cs)+math.Abs(sn))-1e-10
					if overlap {
						t.Fatalf("free cell%v overlaps actual obstacle %s", c, box.Name)
					}
				}
			}
		})
	}
	if count != 5040 {
		t.Fatalf("checked %d rays", count)
	}
}
