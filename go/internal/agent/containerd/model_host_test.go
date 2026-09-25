package containerd

import (
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/agent/models"
	localoci "github.com/wendylabsinc/wendy/go/internal/agent/oci"
	sharedenv "github.com/wendylabsinc/wendy/go/internal/shared/env"
)

func testModelHost() models.HostSpec {
	sha := strings.Repeat("1", 64)
	return models.HostSpec{
		InstanceID: "m-0000beef", AppID: models.AppIDPrefix + "m-0000beef",
		Image:  "ghcr.io/wendylabsinc/wendy-model-host-cpu@sha256:" + strings.Repeat("a", 64),
		Engine: models.EngineONNXRuntime, ModelID: "coco-detector", VariantID: "d-cpu", FileSHA256: sha,
		ModelFile: "/var/lib/wendy/models/files/sha256/" + sha, LabelsFile: "/var/lib/wendy/models/run/m-0000beef/labels.txt",
		CameraNode: "/dev/video255", CameraSource: "v4l2:/dev/video0", LogPath: "/var/lib/wendy/models/run/m-0000beef/host.log",
	}
}

func fakeCameraNode(t *testing.T) {
	t.Helper()
	orig := resolveModelCamera
	t.Cleanup(func() { resolveModelCamera = orig })
	resolveModelCamera = func(path, kind string, follow bool) (int64, int64, error) {
		if path != "/dev/video255" || kind != "c" || follow {
			t.Fatalf("resolved %q %q %v", path, kind, follow)
		}
		return 81, 255, nil
	}
}

var testHostImage = modelHostImage{Args: []string{"/app/host"}, Env: []string{"PATH=/usr/bin", "PYTHONPATH=/app"}, Cwd: "/app"}

func TestModelHostBaseSpecIsLockedDown(t *testing.T) {
	fakeCameraNode(t)
	spec, err := modelHostBaseSpec(testModelHost(), testHostImage)
	if err != nil {
		t.Fatal(err)
	}
	u := spec.Process.User
	if u.UID != models.HostUID || u.GID != models.HostGID || !slices.Contains(u.AdditionalGids, uint32(modelHostVideoGID)) {
		t.Fatalf("user = %+v", u)
	}
	if !spec.Root.Readonly {
		t.Fatal("the root filesystem is writable")
	}
	if c := spec.Process.Capabilities; c == nil || len(c.Bounding)+len(c.Effective)+len(c.Permitted)+len(c.Inheritable)+len(c.Ambient) != 0 {
		t.Fatalf("capabilities = %+v", c)
	}
	if !slices.ContainsFunc(spec.Linux.Namespaces, func(ns localoci.LinuxNamespace) bool { return ns.Type == "network" }) {
		t.Fatal("the host shares the device's network; it must have none")
	}
	if !slices.Equal(spec.Process.Args, []string{"/app/host"}) || spec.Process.Cwd != "/app" {
		t.Fatalf("process = %+v", spec.Process)
	}
	for _, want := range []string{"PYTHONPATH=/app", "WENDY_CAMERA_NODE=/dev/video255", "WENDY_MODEL_FILE=" + models.HostModelFile} {
		if !slices.Contains(spec.Process.Env, want) {
			t.Fatalf("env lacks %q: %v", want, spec.Process.Env)
		}
	}
	var paths []string
	for _, kv := range spec.Process.Env {
		if strings.HasPrefix(kv, "PATH=") {
			paths = append(paths, kv)
		}
	}
	if !slices.Equal(paths, []string{"PATH=/usr/bin"}) {
		t.Fatalf("PATH entries = %v, want only the image's", paths)
	}
	var cameraRules int
	for _, d := range spec.Linux.Resources.Devices {
		if d.Allow && d.Major != nil && *d.Major == 81 {
			cameraRules++
			if d.Minor == nil || *d.Minor != 255 {
				t.Fatalf("the camera rule is not scoped to one node: %+v", d)
			}
		}
	}
	if cameraRules != 1 {
		t.Fatalf("%d camera device rules, want 1", cameraRules)
	}
	mounts := map[string]localoci.Mount{}
	for _, m := range spec.Mounts {
		mounts[m.Destination] = m
	}
	for _, dst := range []string{models.HostModelFile, models.HostLabelsFile} {
		if m, ok := mounts[dst]; !ok || !slices.Contains(m.Options, "ro") {
			t.Fatalf("%s mount = %+v", dst, m)
		}
	}
	if m, ok := mounts["/dev"]; ok && m.Type == "bind" {
		t.Fatal("the device's /dev is bound into a model host")
	}
	if _, ok := mounts[models.HostEngineCacheDir]; ok {
		t.Fatal("an ONNX Runtime host got an engine cache")
	}
	if spec.Linux.CgroupsPath != "" {
		t.Fatal("the base spec must leave the cgroup path to finishModelHostSpec")
	}
}

func TestModelHostTensorRTGetsEngineCache(t *testing.T) {
	fakeCameraNode(t)
	h := testModelHost()
	h.Engine, h.EngineCache = models.EngineTensorRT, "/var/lib/wendy/models/engines/"+h.FileSHA256
	spec, err := modelHostBaseSpec(h, testHostImage)
	if err != nil {
		t.Fatal(err)
	}
	var cache *localoci.Mount
	for i := range spec.Mounts {
		if spec.Mounts[i].Destination == models.HostEngineCacheDir {
			cache = &spec.Mounts[i]
		}
	}
	if cache == nil || cache.Source != h.EngineCache || !slices.Contains(cache.Options, "rw") {
		t.Fatalf("engine cache mount = %+v", cache)
	}
	if !slices.Contains(spec.Process.Env, "WENDY_MODEL_ENGINE_CACHE="+models.HostEngineCacheDir) {
		t.Fatalf("env = %v", spec.Process.Env)
	}
}

func TestFinishModelHostSpecGrantsSocketAndScope(t *testing.T) {
	fakeCameraNode(t)
	dir, err := os.MkdirTemp("/tmp", "wmh")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	lis, err := net.Listen("unix", filepath.Join(dir, "data.sock"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lis.Close() })

	h := testModelHost()
	spec, err := modelHostBaseSpec(h, testHostImage)
	if err != nil {
		t.Fatal(err)
	}
	if err := finishModelHostSpec(spec, h, dir); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(spec.Process.Env, "WENDY_DATA_SOCKET=/run/wendy/data/data.sock") {
		t.Fatalf("env = %v", spec.Process.Env)
	}
	if !slices.Contains(spec.Process.User.AdditionalGids, uint32(2000)) {
		t.Fatal("no data socket group")
	}
	if want := "system.slice:" + sharedenv.SystemdServiceName() + ":" + h.AppID; spec.Linux.CgroupsPath != want {
		t.Fatalf("cgroup = %q, want %q", spec.Linux.CgroupsPath, want)
	}
}
