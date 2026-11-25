package patrolkit

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"github.com/wendylabsinc/wendy/go/world/internal/worldpackage"
	"io"
	"os"
	"path/filepath"
	"sort"
	"syscall"
)

var controllerPins = map[string]string{
	"go2_sim/__init__.py":   "fcce4b00268817450fca3b816bf78ddf70c86c777dff74f07a3f706c8e3e51c8",
	"go2_sim/simulation.py": "02ff85a2e26ca434efeeb09c3dba1c2207f53875dfe2dd816f3cf868ac8904bc",
	"go2_sim/policy.py":     "8e5d80e63ffe5548b216f510ed3a39f713bce8b87fc11af442f5c106779c21c8",
}

func ReadRegular(name string, max int64) ([]byte, error) {
	info, e := os.Lstat(name)
	if e != nil {
		return nil, e
	}
	if !info.Mode().IsRegular() || info.Size() < 1 || info.Size() > max {
		return nil, errors.New("expected bounded regular file")
	}
	f, e := os.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if e != nil {
		return nil, e
	}
	defer f.Close()
	after, e := f.Stat()
	if e != nil || !os.SameFile(info, after) {
		return nil, errors.New("file changed while opening")
	}
	raw, e := io.ReadAll(io.LimitReader(f, max+1))
	if e != nil {
		return nil, e
	}
	if int64(len(raw)) != info.Size() {
		return nil, errors.New("file changed while reading")
	}
	return raw, nil
}

// BuildPackage copies only the fixed pinned adapter modules and declared asset
// inventory. The returned package remains data; preparing it never runs code.
func BuildPackage(source, assets, office string, frame world.FrameRef, id string) (worldpackage.Bundle, error) {
	var zero worldpackage.Bundle
	if !token.MatchString(id) || frame.Validate() != nil {
		return zero, errors.New("invalid kit identity/frame")
	}
	lock, e := ReadRegular(filepath.Join(source, "assets.lock.json"), 128<<10)
	if e != nil {
		return zero, e
	}
	if Digest(lock) != "2cb77d9ff236617cf414816617d676d23ec9e6c623cc599d333e45f4432c9d16" {
		return zero, errors.New("asset inventory differs from supported pinned profile")
	}
	var inventory struct {
		Files []worldpackage.File `json:"files"`
	}
	if e = json.Unmarshal(lock, &inventory); e != nil || len(inventory.Files) < 1 || len(inventory.Files) > 64 {
		return zero, errors.New("invalid pinned asset inventory")
	}
	files := map[string][]byte{"source/assets.lock.json": lock}
	for _, name := range []string{"go2_sim/__init__.py", "go2_sim/simulation.py", "go2_sim/policy.py"} {
		raw, e := ReadRegular(filepath.Join(source, filepath.FromSlash(name)), 1<<20)
		if e != nil {
			return zero, e
		}
		if pin := controllerPins[name]; pin != "" && Digest(raw) != pin {
			return zero, errors.New("controller differs from supported pinned Go2 profile")
		}
		files["source/"+name] = raw
	}
	for _, f := range inventory.Files {
		if worldpackage.SafePath(f.Path) != nil || !validDigest(f.SHA256) || f.Size < 1 || f.Size > worldpackage.MaxFileBytes {
			return zero, errors.New("invalid pinned asset declaration")
		}
		name := "assets/" + f.Path
		if _, ok := files[name]; ok {
			return zero, errors.New("duplicate asset")
		}
		raw, e := ReadRegular(filepath.Join(assets, filepath.FromSlash(f.Path)), worldpackage.MaxFileBytes)
		if e != nil {
			return zero, e
		}
		if int64(len(raw)) != f.Size || Digest(raw) != f.SHA256 {
			return zero, fmt.Errorf("pinned asset differs: %s", f.Path)
		}
		files[name] = raw
	}
	model, e := ReadRegular(office, worldpackage.MaxModelBytes)
	if e != nil {
		return zero, e
	}
	files["office.xml"] = model
	declared := []worldpackage.File{}
	for name, raw := range files {
		if name != "office.xml" {
			declared = append(declared, worldpackage.File{Path: name, SHA256: Digest(raw), Size: int64(len(raw))})
		}
	}
	sort.Slice(declared, func(i, j int) bool { return declared[i].Path < declared[j].Path })
	root := map[string]any{"schema_version": worldpackage.SchemaV1, "site_id": "studio-01", "package_revision": id, "frame_revision": frame.Revision, "provenance": "synthetic", "geographic_anchor": map[string]any{"latitude_deg": 52.0, "longitude_deg": 5.0, "altitude_m": 0, "altitude_datum": "WGS84-ellipsoid", "placement": "illustrative"}, "local_frame": map[string]any{"units": "metres", "handedness": "right", "up": "Y", "axes": []string{"east", "up", "south"}}, "mujoco": map[string]any{"path": "office.xml", "sha256": Digest(model), "to_local_position": []string{"x", "z", "-y"}}, "assets": declared, "description": "Complete supported Go2 office composition inputs. The trusted pinned patrol adapter composes articulated Go2 with this office; raw office XML alone is not an execution profile.", "asset_license": "Private fixture; referenced Go2 assets retain their existing pinned licenses and provenance."}
	raw, e := json.Marshal(root)
	if e != nil {
		return zero, e
	}
	return worldpackage.Verify(raw, files)
}
