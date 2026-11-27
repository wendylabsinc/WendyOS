package go2project

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/worldpackage"
	"golang.org/x/sys/unix"
)

func temp(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}
func encoded(t *testing.T, v any) []byte {
	t.Helper()
	b, e := json.Marshal(v)
	if e != nil {
		t.Fatal(e)
	}
	return b
}
func write(t *testing.T, path string, b []byte) {
	t.Helper()
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
}
func fixture(t *testing.T) (string, string, string, Conversion) {
	t.Helper()
	dir := temp(t)
	root := filepath.Join(dir, "world.json")
	b, err := worldpackage.Load("../../examples/office/world.json")
	if err != nil {
		t.Fatal(err)
	}
	write(t, root, b.Root)
	for name, data := range b.Files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
			t.Fatal(err)
		}
		write(t, p, data)
	}
	var source map[string]json.RawMessage
	if err = json.Unmarshal(b.Root, &source); err != nil {
		t.Fatal(err)
	}
	project := filepath.Join(dir, "project")
	if err = os.Mkdir(project, 0700); err != nil {
		t.Fatal(err)
	}
	g := Geom{"world_geom_000", "box", []float64{.1, .2, .3}, []float64{3, 2, .3}, []float64{1, 0, 0, 0}, []float64{.6, .7, .8, 1}}
	scene := encoded(t, map[string]any{"version": 1, "profile": "go2", "geoms": []Geom{g}})
	write(t, filepath.Join(project, "scene.json"), scene)
	c := Conversion{Schema: "wendy.world.go2-project-conversion.v1", Domain: "simulation", Source: Source{PackageSHA256: b.Manifest.SHA256, SiteID: b.Manifest.Site, PackageRevision: b.Manifest.Revision, FrameRevision: b.Manifest.FrameRevision, Provenance: "synthetic", RunID: "test-run", ClockDomain: "simulation:test-run", GeographicAnchor: source["geographic_anchor"], LocalFrame: source["local_frame"], ModelToLocalPosition: []string{"x", "z", "-y"}}, Target: Target{"go2", Coordinates{"metres", "Z", "wxyz"}, hash(append(append([]byte("scene.json\x00"), scene...), 0)), []File{{"scene.json", len(scene), hash(scene)}}}, Selection: []Selection{{0, "fixture-crate", g.Name, g.Type, g.Size, g.Position, g.Quaternion, g.RGBA}}, Exclusions: map[string][]Exclusion{}, SelectedPropertyDifferences: []PropertyDifference{{SourceGeomName: "fixture-crate", MaterialIndex: -1, SourceContact: Contact{Friction: []float64{1, .005, .0001}, Solref: []float64{.02, 1}, Solimp: []float64{.9, .95, .001, .5, 2}}, TargetContact: "Go2 runtime MJCF defaults; source contact properties are not transferred", Appearance: "compiled geom RGBA only; material, texture, lighting and camera appearance are not preserved"}}, Differences: []string{"Synthetic fixture uses the actual converter report schema; no native compilation in this Go unit test."}, Compiler: Compiler{"MuJoCo", "3.3.7", map[string]int64{"address_space_bytes": 2 << 30, "cpu_seconds": 10, "wall_seconds": 15, "result_bytes": 2 << 20}, "Linux process resource limits; not an OS security sandbox"}, SourceImportReport: json.RawMessage(`{}`)}
	for _, name := range []string{"geoms", "bodies", "joints", "actuators", "sensors", "cameras", "materials", "textures"} {
		c.Exclusions[name] = []Exclusion{}
	}
	report := filepath.Join(dir, "conversion.json")
	write(t, report, encoded(t, c))
	return root, project, report, c
}
func pins(endpoint, revision string) Pins {
	return Pins{endpoint, "project-test", "sha256:" + strings.Repeat("a", 64), strings.Repeat("b", 32), revision, 4}
}

// Field names and the separate project/current epochs match public
// go/simulator/go2/go2_sim/{runtime,simulation,project}.py.
func status(p Pins) map[string]any {
	return map[string]any{"simulation": true, "robot": "go2", "robot_kind": "go2", "profile_version": 1, "vm_name": p.VMName, "source_digest": p.SourceDigest, "dds_isolation": "udp-rtps-loopback", "policy_bundle": PolicyBundle, "clock_mode": "device", "healthy": true, "ready": false, "mode": "paused", "epoch": p.WorldEpoch, "time": 1.0, "error": nil, "project": map[string]any{"project_id": p.ProjectID, "revision": p.ProjectRevision, "error": nil, "watcher_seen": 1700000000.0, "epoch": 3}, "metrics": map[string]any{"physics_steps": 123}, "position": []float64{0, 0, .3}}
}

func TestConversionClosureAndStrictProjection(t *testing.T) {
	root, project, report, c := fixture(t)
	got, err := ValidateConversion(root, project, report)
	if err != nil || got.ConversionSHA256 == "" || got.LossSHA256 == "" || got.ProjectRevision != c.Target.ProjectRevision {
		t.Fatalf("projection: %+v %v", got, err)
	}
	tests := map[string]func(*Conversion){"package": func(c *Conversion) { c.Source.PackageSHA256 = strings.Repeat("0", 64) }, "frame": func(c *Conversion) { c.Source.FrameRevision = "other" }, "operational": func(c *Conversion) { c.Operational = true }, "scene-digest": func(c *Conversion) { c.Target.Files[0].SHA256 = strings.Repeat("0", 64) }, "project-revision": func(c *Conversion) { c.Target.ProjectRevision = strings.Repeat("0", 64) }, "selection": func(c *Conversion) { c.Selection[0].Position = []float64{4, 0, 0} }, "loss-omitted": func(c *Conversion) { delete(c.Exclusions, "bodies") }, "compiler": func(c *Conversion) { c.Compiler.Version = "unreviewed" }}
	original := encoded(t, c)
	for name, change := range tests {
		t.Run(name, func(t *testing.T) {
			var modified Conversion
			if err := json.Unmarshal(original, &modified); err != nil {
				t.Fatal(err)
			}
			change(&modified)
			write(t, report, encoded(t, modified))
			if _, err := ValidateConversion(root, project, report); err == nil {
				t.Fatal("invalid projection accepted")
			}
		})
	}
	write(t, report, original)
	write(t, filepath.Join(project, "extra.json"), []byte("{}"))
	if _, err := ValidateConversion(root, project, report); err == nil {
		t.Fatal("extra runtime file accepted")
	}
	os.Remove(filepath.Join(project, "extra.json"))
	write(t, filepath.Join(filepath.Dir(root), "office.xml"), []byte("changed"))
	if _, err := ValidateConversion(root, project, report); err == nil {
		t.Fatal("changed source closure accepted")
	}
}

func TestRuntimeIdentityAndHTTPReadOnly(t *testing.T) {
	p := pins("http://127.0.0.1:8890", strings.Repeat("c", 64))
	good := status(p)
	if o, err := ValidateStatus(encoded(t, good), p); err != nil || o.ProjectEpoch != 3 || o.RuntimeReady {
		t.Fatalf("paused read-only status: %+v %v", o, err)
	}
	for name, value := range map[string]any{"simulation": false, "source_digest": "wrong", "epoch": 5, "healthy": false, "ready": "false", "mode": "fault", "error": "failed", "vm_name": "different", "dds_isolation": "unmanaged", "profile_version": 2} {
		t.Run(name, func(t *testing.T) {
			copy := status(p)
			copy[name] = value
			if _, err := ValidateStatus(encoded(t, copy), p); err == nil {
				t.Fatal("mismatch accepted")
			}
		})
	}
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != "GET" || r.URL.RequestURI() != "/api/status" {
			t.Error("unexpected simulator operation")
		}
		json.NewEncoder(w).Encode(status(p))
	}))
	defer server.Close()
	p.Endpoint = server.URL
	if _, err := Inspect(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatal("unexpected request count")
	}
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, server.URL+"/api/status", http.StatusFound)
	}))
	defer redirect.Close()
	p.Endpoint = redirect.URL
	if _, err := Inspect(context.Background(), p); err == nil {
		t.Fatal("redirect accepted")
	}
	if calls.Load() != 1 {
		t.Fatal("redirect followed")
	}
}

func TestBindingPausedThenResetAndReportChange(t *testing.T) {
	root, project, report, c := fixture(t)
	p := pins("http://127.0.0.1:8890", c.Target.ProjectRevision)
	var epoch atomic.Uint64
	epoch.Store(4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s := status(p)
		s["epoch"] = epoch.Load()
		json.NewEncoder(w).Encode(s)
	}))
	defer server.Close()
	p.Endpoint = server.URL
	binding, err := Bind(context.Background(), root, project, report, p)
	if err != nil {
		t.Fatal(err)
	}
	if binding.Operational || binding.OperationalAuthority {
		t.Fatal("observation authorizes operations")
	}
	path := filepath.Join(temp(t), "binding.json")
	if err = WriteNew(path, binding); err != nil {
		t.Fatal(err)
	}
	check, err := CheckBinding(context.Background(), path, root, project, report)
	if err != nil || !check.Valid {
		t.Fatalf("unchanged paused binding: %+v %v", check, err)
	}
	epoch.Store(5)
	check, err = CheckBinding(context.Background(), path, root, project, report)
	if err == nil || check.Valid || check.OperationalAuthority {
		t.Fatal("reset binding survived")
	}
	p.WorldEpoch = 5
	if _, err = Bind(context.Background(), root, project, report, p); err != nil {
		t.Fatal("fresh binding cannot record later current epoch", err)
	}
	epoch.Store(4)
	c.Differences = append(c.Differences, "new disclosure")
	write(t, report, encoded(t, c))
	check, err = CheckBinding(context.Background(), path, root, project, report)
	if err == nil || check.Reason != "conversion_changed" {
		t.Fatal("conversion-byte change not invalidated", check, err)
	}
	if err = WriteNew(path, binding); err == nil {
		t.Fatal("immutable binding overwritten")
	}
}

func TestPathBoundsAliasesAndJSON(t *testing.T) {
	for _, url := range []string{"http://localhost:8890", "http://127.0.0.2:8890", "http://127.0.0.1:08890", "http://127.0.0.1:8890?", "http://user@127.0.0.1:8890", "http://127.0.0.1:8890/api/status", "http://[::ffff:127.0.0.1]:8890", "https://127.0.0.1:8890"} {
		if _, err := Endpoint(url); err == nil {
			t.Fatal("ambiguous endpoint accepted", url)
		}
	}
	dir := temp(t)
	file := filepath.Join(dir, "file")
	write(t, file, []byte("{}"))
	link := filepath.Join(dir, "alias")
	if err := os.Symlink(file, link); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadFile(link, 10); err == nil {
		t.Fatal("symlink accepted")
	}
	fifo := filepath.Join(dir, "fifo")
	if err := unix.Mkfifo(fifo, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadFile(fifo, 10); err == nil {
		t.Fatal("FIFO accepted")
	}
	if _, err := ReadFile(file, 1); err == nil {
		t.Fatal("oversize accepted")
	}
	var target struct {
		Value int `json:"value"`
	}
	for _, raw := range []string{`{"value":1,"value":2}`, `{"Value":1}`, `{"value":1} {}`, `{"other":1}`} {
		if strict([]byte(raw), &target, 1024) == nil {
			t.Fatal("non-strict JSON accepted")
		}
	}
}

func TestRealLinuxConverterOutputs(t *testing.T) {
	for _, name := range []string{"corridor", "islands"} {
		t.Run(name, func(t *testing.T) {
			projection, err := ValidateConversion("../../examples/go2-worlds/"+name+"/world.json", "testdata/"+name+"/project", "testdata/"+name+"/conversion.json")
			if err != nil || projection.SelectedGeoms < 2 {
				t.Fatalf("real MuJoCo 3.3.7 conversion: %+v %v", projection, err)
			}
		})
	}
}

func TestConsistentButUnsupportedSourceFrameRefused(t *testing.T) {
	root, project, report, c := fixture(t)
	data, err := os.ReadFile(root)
	if err != nil {
		t.Fatal(err)
	}
	var source map[string]json.RawMessage
	if err = json.Unmarshal(data, &source); err != nil {
		t.Fatal(err)
	}
	changed := json.RawMessage(`{"units":"feet","handedness":"right","up":"Y","axes":["east","up","south"]}`)
	source["local_frame"] = changed
	c.Source.LocalFrame = changed
	newRoot := encoded(t, source)
	write(t, root, newRoot)
	c.Source.PackageSHA256 = hash(newRoot)
	write(t, report, encoded(t, c))
	if _, err = ValidateConversion(root, project, report); err == nil {
		t.Fatal("mutually consistent unsupported coordinate frame accepted")
	}
}

func TestStatusOmittedAliasedNullAndBoundedNetwork(t *testing.T) {
	p := pins("http://127.0.0.1:8890", strings.Repeat("c", 64))
	for _, key := range []string{"ready", "healthy", "project", "epoch", "profile_version", "error"} {
		for _, variant := range []string{"omit", "alias", "null"} {
			t.Run(key+"/"+variant, func(t *testing.T) {
				value := status(p)
				switch variant {
				case "omit":
					delete(value, key)
				case "alias":
					value[strings.ToUpper(key)] = value[key]
					delete(value, key)
				case "null":
					value[key] = nil
				}
				if key == "error" && variant == "null" {
					return
				} // Explicit null means no runtime fault.
				if _, err := ValidateStatus(encoded(t, value), p); err == nil {
					t.Fatal("missing required metadata accepted")
				}
			})
		}
	}
	oversize := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1048577")
		w.WriteHeader(200)
	}))
	defer oversize.Close()
	p.Endpoint = oversize.URL
	if _, err := Inspect(context.Background(), p); err == nil {
		t.Fatal("oversized response accepted")
	}
	blocked := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer blocked.Close()
	p.Endpoint = blocked.URL
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := Inspect(ctx, p); err == nil {
		t.Fatal("request deadline ignored")
	}
}

func TestEqualRuntimeStateCannotProveNoRestart(t *testing.T) {
	// The upstream runtime has no boot identifier. Equal process-local counters
	// and time after a restart cannot be distinguished from this identical read.
	root, project, report, c := fixture(t)
	p := pins("http://127.0.0.1:8890", c.Target.ProjectRevision)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { json.NewEncoder(w).Encode(status(p)) }))
	defer server.Close()
	p.Endpoint = server.URL
	b, err := Bind(context.Background(), root, project, report, p)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(temp(t), "binding.json")
	if err = WriteNew(path, b); err != nil {
		t.Fatal(err)
	}
	check, err := CheckBinding(context.Background(), path, root, project, report)
	if err != nil || !check.Valid || b.RuntimeInstanceVerified || check.RuntimeInstanceVerified {
		t.Fatal("incorrect runtime-instance claim", check, err)
	}
}
