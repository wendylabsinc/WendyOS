//go:build darwin && !ios

package backend

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"

	"github.com/wendylabsinc/wendy/go/internal/cli/commands"
)

type SimulatorBody struct {
	ID   int
	Name string
}
type SimulatorMesh struct {
	ID        int
	Positions []float32
	Indices   []uint32
}
type SimulatorGeom struct {
	Body       int
	Type       string
	Size       []float32
	Position   []float32
	Quaternion []float32
	RGBA       []float32
	Mesh       int
}
type SimulatorScene struct {
	Version   int
	ID        string
	Bodies    []SimulatorBody
	Geoms     []SimulatorGeom
	Meshes    []SimulatorMesh
	RobotBody int `json:"robot_body"`
}
type SimulatorPose struct {
	SceneID     string `json:"scene_id"`
	Positions   []float32
	Quaternions []float32
	Time        float64
	Epoch       uint64
	Generation  uint64
	Mode        string
	Valid       bool
}

func readSimulator(ctx context.Context, name, path string, result any) error {
	response, err := commands.DesktopSimulatorRequest(ctx, name, path)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, (32<<20)+1))
	if err != nil {
		return err
	}
	if len(data) > 32<<20 {
		return fmt.Errorf("simulator response exceeds 32 MiB")
	}
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("simulator returned HTTP %d: %.512s", response.StatusCode, data)
	}
	if result == nil {
		return nil
	}
	return json.Unmarshal(data, result)
}

func ReadSimulatorScene(ctx context.Context, name string) (*SimulatorScene, error) {
	var scene SimulatorScene
	if err := readSimulator(ctx, name, "/api/scene", &scene); err != nil {
		return nil, err
	}
	if err := scene.Validate(); err != nil {
		return nil, err
	}
	return &scene, nil
}
func ReadSimulatorPose(ctx context.Context, name string) (*SimulatorPose, error) {
	var pose SimulatorPose
	if err := readSimulator(ctx, name, "/api/scene/state", &pose); err != nil {
		return nil, err
	}
	if err := pose.Validate(); err != nil {
		return nil, err
	}
	return &pose, nil
}
func ControlSimulator(ctx context.Context, name, action string) error {
	switch action {
	case "pause", "resume", "reset":
	default:
		return fmt.Errorf("unsupported simulator control")
	}
	return readSimulator(ctx, name, "/api/"+action, nil)
}
func finite(values []float32, count int) bool {
	if len(values) != count {
		return false
	}
	for _, v := range values {
		if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) {
			return false
		}
	}
	return true
}
func quaternion(values []float32) bool {
	if !finite(values, 4) {
		return false
	}
	n := float32(0)
	for _, v := range values {
		n += v * v
	}
	return n > .9 && n < 1.1
}
func (s SimulatorScene) Validate() error {
	bad := fmt.Errorf("invalid simulator scene geometry")
	if s.Version != 1 || s.ID == "" || len(s.Bodies) == 0 || len(s.Bodies) > 10000 || len(s.Geoms) > 50000 {
		return bad
	}
	for i, b := range s.Bodies {
		if b.ID != i {
			return bad
		}
	}
	meshes := map[int]bool{}
	for _, m := range s.Meshes {
		if meshes[m.ID] || len(m.Positions) == 0 || len(m.Positions)%3 != 0 || len(m.Indices)%3 != 0 || !finite(m.Positions, len(m.Positions)) {
			return bad
		}
		meshes[m.ID] = true
		for _, index := range m.Indices {
			if uint64(index) >= uint64(len(m.Positions)/3) {
				return bad
			}
		}
	}
	for _, g := range s.Geoms {
		if g.Body < 0 || g.Body >= len(s.Bodies) || !finite(g.Size, 3) || !finite(g.Position, 3) || !quaternion(g.Quaternion) || !finite(g.RGBA, 4) {
			return bad
		}
		for _, size := range g.Size {
			if size < 0 {
				return bad
			}
		}
		switch g.Type {
		case "plane", "box", "sphere", "ellipsoid", "cylinder", "capsule":
		case "mesh":
			if !meshes[g.Mesh] {
				return bad
			}
		default:
			return bad
		}
	}
	return nil
}
func (p SimulatorPose) Validate() error {
	if p.SceneID == "" || len(p.Positions) == 0 || len(p.Positions)%3 != 0 || len(p.Positions) > 30000 || !finite(p.Positions, len(p.Positions)) || len(p.Quaternions) != len(p.Positions)/3*4 || math.IsNaN(p.Time) || math.IsInf(p.Time, 0) {
		return fmt.Errorf("invalid simulator pose")
	}
	for i := 0; i < len(p.Quaternions); i += 4 {
		if !quaternion(p.Quaternions[i : i+4]) {
			return fmt.Errorf("invalid simulator orientation")
		}
	}
	return nil
}
