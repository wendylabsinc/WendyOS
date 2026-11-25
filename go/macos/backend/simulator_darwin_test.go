//go:build darwin && !ios

package backend

import (
	"math"
	"testing"
)

func validSimulatorScene() SimulatorScene {
	return SimulatorScene{Version: 1, ID: "scene", Bodies: []SimulatorBody{{ID: 0}}, Meshes: []SimulatorMesh{{ID: 3, Positions: []float32{0, 0, 0, 1, 0, 0, 0, 1, 0}, Indices: []uint32{0, 1, 2}}}, Geoms: []SimulatorGeom{{Body: 0, Type: "mesh", Mesh: 3, Size: []float32{1, 1, 1}, Position: []float32{0, 0, 0}, Quaternion: []float32{0, 0, 0, 1}, RGBA: []float32{1, 1, 1, 1}}}}
}
func TestSimulatorSceneRejectsUnsafeGeometry(t *testing.T) {
	if err := validSimulatorScene().Validate(); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*SimulatorScene){
		"bad index":            func(s *SimulatorScene) { s.Meshes[0].Indices[2] = 3 },
		"missing mesh":         func(s *SimulatorScene) { s.Geoms[0].Mesh = 8 },
		"duplicate mesh":       func(s *SimulatorScene) { s.Meshes = append(s.Meshes, s.Meshes[0]) },
		"missing body":         func(s *SimulatorScene) { s.Geoms[0].Body = 1 },
		"sparse body IDs":      func(s *SimulatorScene) { s.Bodies[0].ID = 10 },
		"nonfinite vertex":     func(s *SimulatorScene) { s.Meshes[0].Positions[0] = float32(math.NaN()) },
		"invalid orientation":  func(s *SimulatorScene) { s.Geoms[0].Quaternion = []float32{0, 0, 0, 0} },
		"unsupported protocol": func(s *SimulatorScene) { s.Version = 2 },
	} {
		t.Run(name, func(t *testing.T) {
			s := validSimulatorScene()
			change(&s)
			if s.Validate() == nil {
				t.Fatal("accepted invalid scene")
			}
		})
	}
}
func TestSimulatorPoseValidatesCompleteFiniteBodyTransforms(t *testing.T) {
	valid := func() SimulatorPose {
		return SimulatorPose{SceneID: "scene", Positions: []float32{1, 2, 3}, Quaternions: []float32{0, 0, 0, 1}, Time: 2}
	}
	p := valid()
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*SimulatorPose){
		func(p *SimulatorPose) { p.Positions = p.Positions[:2] },
		func(p *SimulatorPose) { p.Quaternions = nil },
		func(p *SimulatorPose) { p.Time = math.Inf(1) },
		func(p *SimulatorPose) { p.Positions[0] = float32(math.Inf(1)) },
	} {
		p := valid()
		change(&p)
		if p.Validate() == nil {
			t.Fatal("accepted invalid pose")
		}
	}
}
