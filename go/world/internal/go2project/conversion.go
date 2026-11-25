package go2project

import (
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"regexp"

	"github.com/wendylabsinc/wendy/go/world/internal/worldpackage"
)

const MaxConversionBytes = 2 << 20

type Source struct {
	PackageSHA256        string          `json:"package_sha256"`
	SiteID               string          `json:"site_id"`
	PackageRevision      string          `json:"package_revision"`
	FrameRevision        string          `json:"frame_revision"`
	Provenance           string          `json:"provenance"`
	RunID                string          `json:"run_id"`
	ClockDomain          string          `json:"clock_domain"`
	GeographicAnchor     json.RawMessage `json:"geographic_anchor"`
	LocalFrame           json.RawMessage `json:"local_frame"`
	ModelToLocalPosition []string        `json:"model_to_local_position"`
}
type File struct {
	Path   string `json:"path"`
	Size   int    `json:"size"`
	SHA256 string `json:"sha256"`
}
type Coordinates struct {
	Units      string `json:"units"`
	Up         string `json:"up"`
	Quaternion string `json:"quaternion"`
}
type Target struct {
	Profile         string      `json:"profile"`
	Coordinates     Coordinates `json:"coordinates"`
	ProjectRevision string      `json:"project_revision"`
	Files           []File      `json:"files"`
}
type Geom struct {
	Name       string    `json:"name"`
	Type       string    `json:"type"`
	Size       []float64 `json:"size"`
	Position   []float64 `json:"position"`
	Quaternion []float64 `json:"quaternion"`
	RGBA       []float64 `json:"rgba"`
}
type Selection struct {
	SourceGeomIndex int       `json:"source_geom_index"`
	SourceGeomName  string    `json:"source_geom_name"`
	ProjectGeomName string    `json:"project_geom_name"`
	Type            string    `json:"type"`
	Size            []float64 `json:"size"`
	Position        []float64 `json:"position"`
	Quaternion      []float64 `json:"quaternion"`
	RGBA            []float64 `json:"rgba"`
}
type Exclusion struct {
	Index  int    `json:"index"`
	Name   string `json:"name"`
	Reason string `json:"reason"`
}
type Contact struct {
	Friction    []float64 `json:"friction"`
	Solref      []float64 `json:"solref"`
	Solimp      []float64 `json:"solimp"`
	Contype     int       `json:"contype"`
	Conaffinity int       `json:"conaffinity"`
	Condim      int       `json:"condim"`
	Margin      float64   `json:"margin"`
	Gap         float64   `json:"gap"`
	Priority    int       `json:"priority"`
}
type PropertyDifference struct {
	SourceGeomName string  `json:"source_geom_name"`
	MaterialIndex  int     `json:"material_index"`
	MaterialName   string  `json:"material_name"`
	SourceContact  Contact `json:"source_contact"`
	TargetContact  string  `json:"target_contact"`
	Appearance     string  `json:"appearance"`
}
type Compiler struct {
	Name      string           `json:"name"`
	Version   string           `json:"version"`
	Limits    map[string]int64 `json:"limits"`
	Isolation string           `json:"isolation"`
}
type Conversion struct {
	Schema                      string                 `json:"schema"`
	Domain                      string                 `json:"domain"`
	Operational                 bool                   `json:"operational"`
	Source                      Source                 `json:"source"`
	Target                      Target                 `json:"target"`
	Selection                   []Selection            `json:"selection"`
	Exclusions                  map[string][]Exclusion `json:"exclusions"`
	SelectedPropertyDifferences []PropertyDifference   `json:"selected_property_differences"`
	Differences                 []string               `json:"differences"`
	Compiler                    Compiler               `json:"compiler"`
	SourceImportReport          json.RawMessage        `json:"source_import_report"`
}
type Projection struct {
	Source           Source `json:"source"`
	ConversionSHA256 string `json:"conversion_sha256"`
	LossSHA256       string `json:"loss_sha256"`
	ProjectRevision  string `json:"project_revision"`
	Files            []File `json:"files"`
	SelectedGeoms    int    `json:"selected_geoms"`
}

func finiteVector(v []float64, n int, limit float64) bool {
	if len(v) != n {
		return false
	}
	for _, f := range v {
		if math.IsNaN(f) || math.IsInf(f, 0) || math.Abs(f) > limit {
			return false
		}
	}
	return true
}
func jsonEqual(a, b []byte) bool {
	var x, y any
	return json.Unmarshal(a, &x) == nil && json.Unmarshal(b, &y) == nil && reflect.DeepEqual(x, y)
}
func requiredFields(raw []byte, names ...string) bool {
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return false
	}
	for _, name := range names {
		if len(m[name]) == 0 || string(m[name]) == "null" {
			return false
		}
	}
	return true
}

var geomName = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{0,47}$`)

func ValidateConversion(packagePath, projectDir, reportPath string) (Projection, error) {
	bad := func() (Projection, error) { return Projection{}, errors.New("invalid or mismatched Go2 conversion") }
	raw, err := ReadFile(reportPath, MaxConversionBytes)
	if err != nil {
		return Projection{}, err
	}
	var c Conversion
	if strict(raw, &c, MaxConversionBytes) != nil || !requiredFields(raw, "schema", "domain", "operational", "source", "target", "selection", "exclusions", "selected_property_differences", "differences", "compiler", "source_import_report") || c.Schema != "wendy.world.go2-project-conversion.v1" || c.Domain != "simulation" || c.Operational {
		return bad()
	}
	// The original package and every declared asset are independently rechecked.
	if _, err := ReadFile(packagePath, worldpackage.MaxRootBytes); err != nil {
		return Projection{}, err
	}
	bundle, err := worldpackage.Load(packagePath)
	if err != nil {
		return Projection{}, errors.New("source World package refused")
	}
	var root map[string]json.RawMessage
	if strict(bundle.Root, &root, worldpackage.MaxRootBytes) != nil {
		return bad()
	}
	var provenance string
	var model struct {
		ToLocalPosition []string `json:"to_local_position"`
	}
	if json.Unmarshal(root["provenance"], &provenance) != nil || json.Unmarshal(root["mujoco"], &model) != nil {
		return bad()
	}
	s := c.Source
	if s.PackageSHA256 != bundle.Manifest.SHA256 || s.SiteID != bundle.Manifest.Site || s.PackageRevision != bundle.Manifest.Revision || s.FrameRevision != bundle.Manifest.FrameRevision || s.Provenance != provenance || provenance != "synthetic" || !token(s.RunID) || s.ClockDomain != "simulation:"+s.RunID || !jsonEqual(s.GeographicAnchor, root["geographic_anchor"]) || !jsonEqual(s.LocalFrame, root["local_frame"]) || !reflect.DeepEqual(s.ModelToLocalPosition, model.ToLocalPosition) || !reflect.DeepEqual(s.ModelToLocalPosition, []string{"x", "z", "-y"}) {
		return bad()
	}
	if !jsonEqual(s.LocalFrame, []byte(`{"units":"metres","handedness":"right","up":"Y","axes":["east","up","south"]}`)) {
		return bad()
	}
	var anchor struct {
		Latitude  float64 `json:"latitude_deg"`
		Longitude float64 `json:"longitude_deg"`
		Altitude  float64 `json:"altitude_m"`
		Datum     string  `json:"altitude_datum"`
		Placement string  `json:"placement"`
	}
	if strict(s.GeographicAnchor, &anchor, MaxConversionBytes) != nil || math.Abs(anchor.Latitude) > 90 || math.Abs(anchor.Longitude) > 180 || math.IsInf(anchor.Altitude, 0) || math.IsNaN(anchor.Altitude) || anchor.Datum != "WGS84-ellipsoid" || anchor.Placement != "illustrative" {
		return bad()
	}
	if c.Target.Profile != "go2" || c.Target.Coordinates != (Coordinates{"metres", "Z", "wxyz"}) || !hexDigest(c.Target.ProjectRevision) || len(c.Target.Files) != 1 || c.Target.Files[0].Path != "scene.json" {
		return bad()
	}
	// This converter version emits primitives only. Extra files, even harmless
	// ones, would change the runtime's project digest and are refused.
	info, err := os.Lstat(projectDir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return bad()
	}
	dir, err := os.Open(projectDir)
	if err != nil {
		return bad()
	}
	names, readErr := dir.Readdirnames(2)
	dir.Close()
	if readErr != nil && len(names) == 0 {
		return bad()
	}
	if len(names) != 1 || names[0] != "scene.json" {
		return bad()
	}
	sceneRaw, err := ReadFile(filepath.Join(projectDir, "scene.json"), 2<<20)
	if err != nil {
		return Projection{}, err
	}
	file := c.Target.Files[0]
	if file.Size != len(sceneRaw) || file.SHA256 != hash(sceneRaw) || c.Target.ProjectRevision != hash(append(append([]byte("scene.json\x00"), sceneRaw...), 0)) {
		return bad()
	}
	var scene struct {
		Version int    `json:"version"`
		Profile string `json:"profile"`
		Geoms   []Geom `json:"geoms"`
	}
	if strict(sceneRaw, &scene, 2<<20) != nil || scene.Version != 1 || scene.Profile != "go2" || len(scene.Geoms) < 1 || len(scene.Geoms) > 64 || len(c.Selection) != len(scene.Geoms) || len(c.SelectedPropertyDifferences) != len(scene.Geoms) {
		return bad()
	}
	selectedNames := map[string]bool{}
	selectedIndices := map[int]bool{}
	projectNames := map[string]bool{}
	for i, g := range scene.Geoms {
		sizeN := map[string]int{"box": 3, "sphere": 1, "cylinder": 2, "capsule": 2, "ellipsoid": 3}[g.Type]
		if sizeN == 0 || !geomName.MatchString(g.Name) || projectNames[g.Name] || !finiteVector(g.Size, sizeN, 4) || !finiteVector(g.Position, 3, 20) || !finiteVector(g.Quaternion, 4, 1) || !finiteVector(g.RGBA, 4, 1) {
			return bad()
		}
		for _, v := range g.Size {
			if v <= 0 {
				return bad()
			}
		}
		for _, v := range g.RGBA {
			if v < 0 {
				return bad()
			}
		}
		if g.RGBA[3] <= 0 {
			return bad()
		}
		norm := 0.0
		for _, v := range g.Quaternion {
			norm += v * v
		}
		if math.Abs(norm-1) > 1e-5 {
			return bad()
		}
		sel := c.Selection[i]
		selected := Geom{sel.ProjectGeomName, sel.Type, sel.Size, sel.Position, sel.Quaternion, sel.RGBA}
		if !reflect.DeepEqual(g, selected) || sel.SourceGeomIndex < 0 || sel.SourceGeomIndex >= 4096 || len(sel.SourceGeomName) < 1 || len(sel.SourceGeomName) > 256 || selectedNames[sel.SourceGeomName] || selectedIndices[sel.SourceGeomIndex] {
			return bad()
		}
		selectedNames[sel.SourceGeomName] = true
		selectedIndices[sel.SourceGeomIndex] = true
		projectNames[g.Name] = true
		d := c.SelectedPropertyDifferences[i]
		if d.SourceGeomName != sel.SourceGeomName || d.MaterialIndex < -1 || d.MaterialIndex >= 4096 || len(d.MaterialName) > 256 || d.TargetContact != "Go2 runtime MJCF defaults; source contact properties are not transferred" || d.Appearance != "compiled geom RGBA only; material, texture, lighting and camera appearance are not preserved" || !finiteVector(d.SourceContact.Friction, 3, 1e15) || !finiteVector(d.SourceContact.Solref, 2, 1e15) || !finiteVector(d.SourceContact.Solimp, 5, 1e15) {
			return bad()
		}
	}
	categories := []string{"geoms", "bodies", "joints", "actuators", "sensors", "cameras", "materials", "textures"}
	if len(c.Exclusions) != len(categories) {
		return bad()
	}
	for _, category := range categories {
		items, ok := c.Exclusions[category]
		if !ok || items == nil || len(items) > 4096 {
			return bad()
		}
		seen := map[int]bool{}
		for _, x := range items {
			if x.Index < 0 || x.Index >= 4096 || seen[x.Index] || len(x.Name) > 256 || len(x.Reason) < 1 || len(x.Reason) > 256 || (category == "geoms" && selectedIndices[x.Index]) {
				return bad()
			}
			seen[x.Index] = true
		}
	}
	if len(c.Differences) < 1 || len(c.Differences) > 32 {
		return bad()
	}
	for _, d := range c.Differences {
		if len(d) < 1 || len(d) > 1024 {
			return bad()
		}
	}
	// Keep historical converter receipts readable without relabeling their
	// runtime. New receipts use 4.0.0; other versions require an explicit review.
	if c.Compiler.Name != "MuJoCo" || (c.Compiler.Version != "3.3.7" && c.Compiler.Version != "4.0.0") || c.Compiler.Isolation != "Linux process resource limits; not an OS security sandbox" || !reflect.DeepEqual(c.Compiler.Limits, map[string]int64{"address_space_bytes": 2 << 30, "cpu_seconds": 10, "wall_seconds": 15, "result_bytes": 2 << 20}) {
		return bad()
	}
	var importReport map[string]json.RawMessage
	if strict(c.SourceImportReport, &importReport, MaxConversionBytes) != nil || importReport == nil {
		return bad()
	}
	// Loss digest commits to the exact serialized loss fields. It does not claim
	// an independent MuJoCo compilation or calibrated physical fidelity.
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(raw, &fields)
	lossBytes, _ := json.Marshal(struct {
		Exclusions  json.RawMessage `json:"exclusions"`
		Properties  json.RawMessage `json:"selected_property_differences"`
		Differences json.RawMessage `json:"differences"`
	}{fields["exclusions"], fields["selected_property_differences"], fields["differences"]})
	return Projection{Source: s, ConversionSHA256: hash(raw), LossSHA256: hash(lossBytes), ProjectRevision: c.Target.ProjectRevision, Files: c.Target.Files, SelectedGeoms: len(c.Selection)}, nil
}
