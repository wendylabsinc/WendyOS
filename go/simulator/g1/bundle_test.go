package g1

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"
)

func cacheRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	// Published directories are intentionally read-only. Restore permissions
	// before testing.TempDir performs its recursive cleanup.
	t.Cleanup(func() { removeTemporary(root) })
	return root
}

type reversedDirectoryFS struct{ fstest.MapFS }

func (f reversedDirectoryFS) ReadDir(name string) ([]fs.DirEntry, error) {
	entries, err := f.MapFS.ReadDir(name)
	slices.Reverse(entries)
	return entries, err
}

func TestSourceDigestCanonical(t *testing.T) {
	tree := fstest.MapFS{
		"nested/b.txt": {Data: []byte("two"), Mode: 0o600, ModTime: time.Unix(100, 0)},
		"a.txt":        {Data: []byte("one"), Mode: 0o644},
	}
	want := "sha256:d1a379e30d8af734a522840ca8eefebddb3020c30681afe0eb8021b157e215e2"
	for _, input := range []fs.FS{tree, reversedDirectoryFS{tree}} {
		got, err := digestTree(input)
		if err != nil || got != want {
			t.Fatalf("canonical digest = %q, %v; want %q", got, err, want)
		}
	}
	tree["nested/b.txt"].Mode = 0o444
	tree["nested/b.txt"].ModTime = time.Unix(200, 0)
	if got, err := digestTree(tree); err != nil || got != want {
		t.Fatalf("metadata changed source digest: %q, %v", got, err)
	}
	tree["nested/b.txt"].Data = []byte("changed")
	if got, err := digestTree(tree); err != nil || got == want {
		t.Fatalf("content change was not detected: %q, %v", got, err)
	}
	tree["nested/b.txt"].Data = []byte("two")
	tree["renamed.txt"] = tree["a.txt"]
	delete(tree, "a.txt")
	if got, err := digestTree(tree); err != nil || got == want {
		t.Fatalf("path change was not detected: %q, %v", got, err)
	}
	if got := SourceDigest(); !regexp.MustCompile(`^sha256:[a-f0-9]{64}$`).MatchString(got) {
		t.Fatalf("invalid public source digest %q", got)
	}
}

func TestEmbeddedSourcesAllowlist(t *testing.T) {
	want := []string{
		".dockerignore", "Dockerfile", "Dockerfile.ros", "UPSTREAM.md", "assets.lock.json", "compatibility.json", "cyclonedds.xml", "entrypoint.sh",
		"g1_sim/__init__.py", "g1_sim/camera.py", "g1_sim/commands.py", "g1_sim/index.html", "g1_sim/isolation.py", "g1_sim/isolation_legacy.py", "g1_sim/lidar.py", "g1_sim/policy.py",
		"g1_sim/native_commands.py", "g1_sim/native_state.py", "g1_sim/observations.py",
		"g1_sim/ros.py", "g1_sim/runtime.py", "g1_sim/scene.py", "g1_sim/sensors.py", "g1_sim/server.py",
		"g1_sim/simulation.py", "g1_sim/slow_sensors.py", "g1_sim/unitree_crc.py",
		"g1_sim/viewer.js", "g1_sim/vendor/OrbitControls.js", "g1_sim/vendor/three.core.js", "g1_sim/vendor/three.module.js",
		"licenses/unitree_rl_lab.LICENSE", "licenses/three.LICENSE", "licenses/unitree_mujoco.LICENSE",
		"licenses/unitree_ros2.LICENSE", "licenses/unitree_sdk2_python.LICENSE",
		"requirements-dev.txt", "requirements.txt",
		"ros_ws/src/g1_command_ingress/CMakeLists.txt", "ros_ws/src/g1_command_ingress/package.xml",
		"ros_ws/src/g1_command_ingress/src/command_ingress.cpp",
		"tools/fetch_assets.py", "tools/fetch_unitree.py", "tools/prepare_unitree.py",
		"unitree.lock.json", "wendy.json",
	}
	slices.Sort(want)
	got, err := treeFiles(sources)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("embedded payload differs from reviewed allowlist:\ngot %v\nwant %v", got, want)
	}
}

func TestMaterializeIdentity(t *testing.T) {
	root := cacheRoot(t)
	first, err := Materialize(root)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(root, "g1", strings.TrimPrefix(SourceDigest(), "sha256:"))
	if first != want || !filepath.IsAbs(first) {
		t.Fatalf("materialized path = %q, want %q", first, want)
	}
	before, err := os.Stat(first)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Materialize(root)
	if err != nil || second != first {
		t.Fatalf("reuse = %q, %v; want %q", second, err, first)
	}
	after, err := os.Stat(second)
	if err != nil || !os.SameFile(before, after) || before.ModTime() != after.ModTime() {
		t.Fatalf("cache directory was replaced or changed: %v", err)
	}
	files, _ := treeFiles(sources)
	for _, name := range files {
		want, err := sources.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(first, filepath.FromSlash(name))
		got, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("materialized bytes differ for %s: %v", name, err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if runtime.GOOS != "windows" && info.Mode().Perm()&0o222 != 0 {
			t.Fatalf("published file %s is writable: %s", name, info.Mode())
		}
	}
	if runtime.GOOS != "windows" && after.Mode().Perm()&0o222 != 0 {
		t.Fatalf("published directory is writable: %s", after.Mode())
	}
}

func TestMaterializeRejectsModifiedCache(t *testing.T) {
	for _, modification := range []string{"content", "missing", "extra-file", "extra-directory", "file-symlink", "directory-symlink", "root-symlink"} {
		t.Run(modification, func(t *testing.T) {
			root := cacheRoot(t)
			path, err := Materialize(root)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, 0o755); err != nil {
				t.Fatal(err)
			}
			file := filepath.Join(path, "wendy.json")
			switch modification {
			case "content":
				if err := os.Chmod(file, 0o644); err != nil {
					t.Fatal(err)
				}
				err = os.WriteFile(file, []byte("user-changed"), 0o644)
			case "missing":
				err = os.Remove(file)
			case "extra-file":
				err = os.WriteFile(filepath.Join(path, "user.txt"), []byte("user content"), 0o644)
			case "extra-directory":
				err = os.Mkdir(filepath.Join(path, "user-directory"), 0o755)
			case "file-symlink":
				if err := os.Remove(file); err != nil {
					t.Fatal(err)
				}
				err = os.Symlink("Dockerfile", file)
			case "directory-symlink":
				err = os.Symlink("g1_sim", filepath.Join(path, "extra-link"))
			case "root-symlink":
				original := path + ".original"
				if err := os.Rename(path, original); err != nil {
					t.Fatal(err)
				}
				err = os.Symlink(original, path)
			}
			if err != nil {
				t.Fatal(err)
			}
			before, err := os.Lstat(path)
			if err != nil {
				t.Fatal(err)
			}
			if got, err := Materialize(root); err == nil || got != "" {
				t.Fatalf("modified cache was accepted: %q, %v", got, err)
			}
			after, err := os.Lstat(path)
			if err != nil || !os.SameFile(before, after) {
				t.Fatalf("invalid cache was replaced: %v", err)
			}
			if modification == "content" {
				got, err := os.ReadFile(file)
				if err != nil || string(got) != "user-changed" {
					t.Fatalf("user data was overwritten: %q, %v", got, err)
				}
			}
		})
	}
}

func TestMaterializeConcurrent(t *testing.T) {
	root := cacheRoot(t)
	const workers = 16
	paths := make([]string, workers)
	errors := make([]error, workers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range workers {
		wg.Go(func() {
			<-start
			paths[i], errors[i] = Materialize(root)
		})
	}
	close(start)
	wg.Wait()
	for i := range workers {
		if errors[i] != nil || paths[i] == "" || paths[i] != paths[0] {
			t.Fatalf("worker %d = %q, %v; first path %q", i, paths[i], errors[i], paths[0])
		}
	}
	if err := verifyCache(paths[0], SourceDigest()); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(root, "g1"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected only published tree and persistent lock, got %v", entries)
	}
}

type failingReadFS struct{ fstest.MapFS }

func (f failingReadFS) ReadFile(name string) ([]byte, error) {
	if name == "z.txt" {
		return nil, errors.New("injected source read failure")
	}
	return f.MapFS.ReadFile(name)
}

func TestMaterializeCleansOnlyOwnedTemporary(t *testing.T) {
	root := cacheRoot(t)
	foreign := filepath.Join(root, "g1", ".materialize-foreign")
	if err := os.MkdirAll(foreign, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(foreign, "keep.txt")
	if err := os.WriteFile(marker, []byte("another writer"), 0o600); err != nil {
		t.Fatal(err)
	}
	tree := fstest.MapFS{"a.txt": {Data: []byte("written first")}, "z.txt": {Data: []byte("fails later")}}
	digest, err := digestTree(tree)
	if err != nil {
		t.Fatal(err)
	}
	if path, err := materialize(root, failingReadFS{tree}, digest); err == nil || path != "" || !strings.Contains(err.Error(), "injected") {
		t.Fatalf("failed read result = %q, %v", path, err)
	}
	entries, err := os.ReadDir(filepath.Join(root, "g1"))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	if !reflect.DeepEqual(names, []string{".materialize-foreign", ".materialize.lock"}) {
		t.Fatalf("partial tree leaked or foreign tree removed: %v", names)
	}
	if data, err := os.ReadFile(marker); err != nil || string(data) != "another writer" {
		t.Fatalf("foreign temporary tree changed: %q, %v", data, err)
	}
}

func TestMaterializeRejectsInvalidRoot(t *testing.T) {
	if _, err := Materialize(""); err == nil {
		t.Fatal("empty root accepted")
	}
	root := cacheRoot(t)
	if err := os.WriteFile(filepath.Join(root, "g1"), []byte("user data"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Materialize(root); err == nil {
		t.Fatal("file used as cache directory")
	}
	if got, err := os.ReadFile(filepath.Join(root, "g1")); err != nil || string(got) != "user data" {
		t.Fatalf("existing file overwritten: %q, %v", got, err)
	}
}
