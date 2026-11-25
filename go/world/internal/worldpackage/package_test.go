package worldpackage

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
)

func fixture(t *testing.T) ([]byte, map[string][]byte) {
	t.Helper()
	model := []byte(`<mujoco><worldbody><geom type="sphere" size="1"/></worldbody></mujoco>`)
	texture := []byte("opaque texture fixture bytes")
	doc := map[string]any{"schema_version": "wendy.world.package.v0", "site_id": "office", "package_revision": "v1", "frame_revision": "floor-v1", "provenance": "synthetic", "mujoco": map[string]any{"path": "model/main.xml", "sha256": checksum(model)}, "assets": []any{map[string]any{"path": "textures/wall.bin", "sha256": checksum(texture), "size": len(texture)}}, "fidelity": map[string]any{"physics": "unvalidated fixture"}}
	raw, e := json.MarshalIndent(doc, "", "  ")
	if e != nil {
		t.Fatal(e)
	}
	return raw, map[string][]byte{"model/main.xml": model, "textures/wall.bin": texture}
}
func alter(t *testing.T, raw []byte, change func(map[string]any)) []byte {
	t.Helper()
	var doc map[string]any
	if e := json.Unmarshal(raw, &doc); e != nil {
		t.Fatal(e)
	}
	change(doc)
	out, e := json.Marshal(doc)
	if e != nil {
		t.Fatal(e)
	}
	return out
}
func TestInspectVerifyPreserveRootAndOwnBytes(t *testing.T) {
	raw, files := fixture(t)
	manifest, e := Inspect(raw)
	if e != nil {
		t.Fatal(e)
	}
	if manifest.SHA256 != checksum(raw) || manifest.Files[0].Path != "model/main.xml" || manifest.Files[0].Size != 0 {
		t.Fatal(manifest)
	}
	bundle, e := Verify(raw, files)
	if e != nil {
		t.Fatal(e)
	}
	if !bytes.Equal(bundle.Root, raw) || bundle.Manifest.Files[0].Size != int64(len(files["model/main.xml"])) {
		t.Fatal("root bytes or resolved size changed")
	}
	original := append([]byte(nil), bundle.Root...)
	raw[0] = 'x'
	files["model/main.xml"][0] = 'x'
	if !bytes.Equal(bundle.Root, original) || bundle.Files["model/main.xml"][0] != '<' {
		t.Fatal("bundle aliases input")
	}
}
func TestInspectRejectsMalformedMetadata(t *testing.T) {
	raw, _ := fixture(t)
	tests := map[string][]byte{
		"duplicate":         bytes.Replace(raw, []byte(`"site_id": "office"`), []byte(`"site_id":"office","site_id":"other"`), 1),
		"null":              alter(t, raw, func(d map[string]any) { d["mujoco"] = nil }),
		"missing site":      alter(t, raw, func(d map[string]any) { delete(d, "site_id") }),
		"physical":          alter(t, raw, func(d map[string]any) { d["provenance"] = "measured" }),
		"unknown schema":    alter(t, raw, func(d map[string]any) { d["schema_version"] = "wendy.world.package.v2" }),
		"null assets":       alter(t, raw, func(d map[string]any) { d["assets"] = nil }),
		"missing size":      alter(t, raw, func(d map[string]any) { delete(d["assets"].([]any)[0].(map[string]any), "size") }),
		"fraction size":     alter(t, raw, func(d map[string]any) { d["assets"].([]any)[0].(map[string]any)["size"] = 1.2 }),
		"oversize":          alter(t, raw, func(d map[string]any) { d["assets"].([]any)[0].(map[string]any)["size"] = MaxFileBytes + 1 }),
		"unknown asset key": alter(t, raw, func(d map[string]any) { d["assets"].([]any)[0].(map[string]any)["url"] = "https://example.invalid" }),
		"overflow number":   append(raw[:len(raw)-1], []byte(`,"metadata":1e999}`)...),
		"trailing":          append(append([]byte(nil), raw...), []byte(` {}`)...),
	}
	for name, b := range tests {
		t.Run(name, func(t *testing.T) {
			if _, e := Inspect(b); e == nil {
				t.Fatal("accepted invalid manifest")
			}
		})
	}
}
func TestPathsArePortableAndCollisionFree(t *testing.T) {
	raw, _ := fixture(t)
	for _, name := range []string{"../outside", "/tmp/out", "a/../b", "a//b", "a/./b", "a\\b", "C:thing", "name.", "space name", ".hidden", "CON", "nul.xml", "com1.bin", "lpt9", "café.png", "a/", strings.Repeat("a", 129)} {
		if e := SafePath(name); e == nil {
			t.Fatalf("unsafe path accepted %q", name)
		}
	}
	for _, name := range []string{"world.json", "WORLD.JSON", "model", "model/MAIN.xml", "Model/texture.bin", "world.json/child"} {
		mutated := alter(t, raw, func(d map[string]any) { d["assets"].([]any)[0].(map[string]any)["path"] = name })
		if _, e := Inspect(mutated); e == nil {
			t.Fatalf("collision accepted %q", name)
		}
	}
}
func TestVerifyRejectsMissingExtraTamperAndWrongSize(t *testing.T) {
	raw, files := fixture(t)
	cases := map[string]map[string][]byte{
		"missing": {"model/main.xml": files["model/main.xml"]},
		"extra":   {"model/main.xml": files["model/main.xml"], "textures/wall.bin": files["textures/wall.bin"], "secret": []byte("extra")},
		"tamper":  {"model/main.xml": []byte("different"), "textures/wall.bin": files["textures/wall.bin"]},
		"size":    {"model/main.xml": files["model/main.xml"], "textures/wall.bin": append(append([]byte(nil), files["textures/wall.bin"]...), 1)},
	}
	for name, blobs := range cases {
		t.Run(name, func(t *testing.T) {
			if _, e := Verify(raw, blobs); e == nil {
				t.Fatal("invalid files accepted")
			}
		})
	}
}
func TestReferenceCountAndTotalBounds(t *testing.T) {
	raw, _ := fixture(t)
	overCount := alter(t, raw, func(d map[string]any) {
		items := []any{}
		for i := 0; i < MaxFiles; i++ {
			items = append(items, map[string]any{"path": strings.Repeat("a", 1) + string(rune('A'+i/26)) + string(rune('A'+i%26)), "sha256": checksum([]byte("x")), "size": 1})
		}
		d["assets"] = items
	})
	if _, e := Inspect(overCount); e == nil {
		t.Fatal("file count bound ignored")
	}
	overTotal := alter(t, raw, func(d map[string]any) {
		items := []any{}
		for i := 0; i < 9; i++ {
			items = append(items, map[string]any{"path": string(rune('a'+i)) + ".bin", "sha256": checksum([]byte("x")), "size": MaxFileBytes})
		}
		d["assets"] = items
	})
	if _, e := Inspect(overTotal); e == nil {
		t.Fatal("total byte bound ignored")
	}
	if _, e := Inspect(bytes.Repeat([]byte(" "), MaxRootBytes+1)); e == nil {
		t.Fatal("root byte bound ignored")
	}
}
func TestLoadAndInstallRealFixture(t *testing.T) {
	source := "../../examples/live-office/world.json"
	bundle, e := Load(source)
	if e != nil {
		t.Fatal(e)
	}
	dest := filepath.Join(t.TempDir(), "installed")
	if e = Install(bundle, dest); e != nil {
		t.Fatal(e)
	}
	loaded, e := Load(filepath.Join(dest, "world.json"))
	if e != nil {
		t.Fatal(e)
	}
	if loaded.Manifest.SHA256 != bundle.Manifest.SHA256 || !bytes.Equal(loaded.Root, bundle.Root) {
		t.Fatal("installed manifest changed")
	}
	for _, file := range bundle.Manifest.Files {
		st, e := os.Stat(filepath.Join(dest, file.Path))
		if e != nil {
			t.Fatal(e)
		}
		if st.Mode().Perm() != 0600 {
			t.Fatal(st.Mode())
		}
	}
	st, e := os.Stat(dest)
	if e != nil || st.Mode().Perm() != 0700 {
		t.Fatal(st, e)
	}
	if e = Install(bundle, dest); !errors.Is(e, os.ErrExist) {
		t.Fatal("existing installation not refused", e)
	}
}
func writeFixture(t *testing.T) (string, []byte, map[string][]byte) {
	t.Helper()
	dir := t.TempDir()
	raw, files := fixture(t)
	if e := os.WriteFile(filepath.Join(dir, "world.json"), raw, 0600); e != nil {
		t.Fatal(e)
	}
	for name, data := range files {
		if e := os.MkdirAll(filepath.Join(dir, filepath.Dir(name)), 0700); e != nil {
			t.Fatal(e)
		}
		if e := os.WriteFile(filepath.Join(dir, name), data, 0600); e != nil {
			t.Fatal(e)
		}
	}
	return dir, raw, files
}
func TestLoadRejectsSymlinksAndNonregularFiles(t *testing.T) {
	for _, mode := range []string{"root-link", "file-link", "directory-link", "fifo"} {
		t.Run(mode, func(t *testing.T) {
			dir, _, files := writeFixture(t)
			target := filepath.Join(dir, "model/main.xml")
			switch mode {
			case "root-link":
				if e := os.Rename(filepath.Join(dir, "world.json"), filepath.Join(dir, "original.json")); e != nil {
					t.Fatal(e)
				}
				if e := os.Symlink("original.json", filepath.Join(dir, "world.json")); e != nil {
					t.Fatal(e)
				}
			case "directory-link":
				outside := t.TempDir()
				if e := os.WriteFile(filepath.Join(outside, "main.xml"), files["model/main.xml"], 0600); e != nil {
					t.Fatal(e)
				}
				os.RemoveAll(filepath.Join(dir, "model"))
				if e := os.Symlink(outside, filepath.Join(dir, "model")); e != nil {
					t.Fatal(e)
				}
			case "file-link":
				os.Remove(target)
				if e := os.Symlink("../textures/wall.bin", target); e != nil {
					t.Fatal(e)
				}
			case "fifo":
				os.Remove(target)
				if e := syscall.Mkfifo(target, 0600); e != nil {
					t.Fatal(e)
				}
			}
			if _, e := Load(filepath.Join(dir, "world.json")); e == nil {
				t.Fatal("unsafe package source accepted")
			}
		})
	}
}
func TestInstallRevalidatesAndNeverOverwrites(t *testing.T) {
	raw, files := fixture(t)
	bundle, e := Verify(raw, files)
	if e != nil {
		t.Fatal(e)
	}
	parent := t.TempDir()
	dest := filepath.Join(parent, "installed")
	bundle.Manifest.Files[0].Path = "../outside" // Mutable convenience metadata is not trusted.
	if e = Install(bundle, dest); e != nil {
		t.Fatal(e)
	}
	if _, e = os.Stat(filepath.Join(parent, "outside")); !errors.Is(e, os.ErrNotExist) {
		t.Fatal("used mutated manifest")
	}
	another := filepath.Join(parent, "bad")
	bundle.Files["model/main.xml"][0] = 'x'
	if e = Install(bundle, another); e == nil {
		t.Fatal("tampered bundle installed")
	}
	if _, e = os.Stat(another); !errors.Is(e, os.ErrNotExist) {
		t.Fatal("partial failed install visible")
	}
}
func TestInstallRacePreservesCompetingDestination(t *testing.T) {
	raw, files := fixture(t)
	bundle, e := Verify(raw, files)
	if e != nil {
		t.Fatal(e)
	}
	parent := t.TempDir()
	dest := filepath.Join(parent, "installed")
	e = install(bundle, dest, func(fd int, from, to string) error {
		if e := os.Mkdir(dest, 0700); e != nil {
			t.Fatal(e)
		}
		if e := os.WriteFile(filepath.Join(dest, "owner"), []byte("competing process"), 0600); e != nil {
			t.Fatal(e)
		}
		return renameNoReplace(fd, from, to)
	})
	if e == nil {
		t.Fatal("replaced racing destination")
	}
	contents, e := os.ReadFile(filepath.Join(dest, "owner"))
	if e != nil || string(contents) != "competing process" {
		t.Fatal("competing directory damaged", e)
	}
	stages, e := filepath.Glob(filepath.Join(parent, ".world-package-*"))
	if e != nil || len(stages) != 0 {
		t.Fatal("staging leaked", stages, e)
	}
}
func TestConcurrentInstallHasOneWinner(t *testing.T) {
	raw, files := fixture(t)
	bundle, e := Verify(raw, files)
	if e != nil {
		t.Fatal(e)
	}
	dest := filepath.Join(t.TempDir(), "installed")
	var count atomic.Int64
	var wg sync.WaitGroup
	installOne := func() {
		defer wg.Done()
		if Install(bundle, dest) == nil {
			count.Add(1)
		}
	}
	wg.Add(2)
	go installOne()
	go installOne()
	wg.Wait()
	if count.Load() != 1 {
		t.Fatal("install winner count", count.Load())
	}
	loaded, e := Load(filepath.Join(dest, "world.json"))
	if e != nil || loaded.Manifest.SHA256 != bundle.Manifest.SHA256 {
		t.Fatal("winner incomplete", e)
	}
}
