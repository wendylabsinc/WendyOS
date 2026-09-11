package commands

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/stagefile/spec"
)

// readLayerEntries un-gzips a native layer blob and returns name → header/data
// for every tar entry, plus the raw uncompressed bytes.
func readLayerEntries(t *testing.T, blob []byte) (map[string]*tar.Header, map[string][]byte, []byte) {
	t.Helper()
	gr, err := gzip.NewReader(bytes.NewReader(blob))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(gr)
	if err != nil {
		t.Fatal(err)
	}
	hdrs := map[string]*tar.Header{}
	data := map[string][]byte{}
	tr := tar.NewReader(bytes.NewReader(raw))
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		hdrs[hdr.Name] = hdr
		data[hdr.Name] = b
	}
	return hdrs, data, raw
}

func TestBuildNativeCopyLayerFileToFile(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "main.py", "print('hello')\n")

	l, err := buildNativeCopyLayer(dir, spec.CopyEntry{From: "local", Paths: []string{"main.py"}}, "")
	if err != nil {
		t.Fatal(err)
	}
	hdrs, data, raw := readLayerEntries(t, l.Blob)

	h, ok := hdrs["main.py"]
	if !ok {
		t.Fatalf("missing main.py entry; have %v", l.FileNames)
	}
	if string(data["main.py"]) != "print('hello')\n" {
		t.Fatal("content mismatch")
	}
	if h.Uid != 0 || h.Gid != 0 || h.Uname != "" || h.Gname != "" {
		t.Fatalf("ownership must be numeric root:root, got %d:%d %q:%q", h.Uid, h.Gid, h.Uname, h.Gname)
	}
	if !h.ModTime.Equal(nativeLayerEpoch) {
		t.Fatalf("mtime must be the fixed epoch, got %v", h.ModTime)
	}
	if want := "sha256:" + sha256Hex(raw); l.DiffID != want {
		t.Fatalf("DiffID = %s, want %s", l.DiffID, want)
	}
	if want := "sha256:" + sha256Hex(l.Blob); l.Digest != want {
		t.Fatalf("Digest = %s, want %s", l.Digest, want)
	}
}

func TestBuildNativeCopyLayerDestForms(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "main.py", "m\n")
	writeFile(t, dir, "util.py", "u\n")

	t.Run("file into dir dest", func(t *testing.T) {
		l, err := buildNativeCopyLayer(dir, spec.CopyEntry{From: "local", Paths: []string{"main.py"}, Dest: "app/"}, "")
		if err != nil {
			t.Fatal(err)
		}
		hdrs, _, _ := readLayerEntries(t, l.Blob)
		if _, ok := hdrs["app/main.py"]; !ok {
			t.Fatalf("want app/main.py, have %v", l.FileNames)
		}
		if d, ok := hdrs["app/"]; !ok || d.Typeflag != tar.TypeDir {
			t.Fatal("missing synthesized parent dir entry app/")
		}
	})

	t.Run("multi-source forces dir dest", func(t *testing.T) {
		l, err := buildNativeCopyLayer(dir, spec.CopyEntry{From: "local", Paths: []string{"main.py", "util.py"}, Dest: "app"}, "")
		if err != nil {
			t.Fatal(err)
		}
		hdrs, _, _ := readLayerEntries(t, l.Blob)
		if _, ok := hdrs["app/main.py"]; !ok {
			t.Fatalf("want app/main.py, have %v", l.FileNames)
		}
		if _, ok := hdrs["app/util.py"]; !ok {
			t.Fatal("want app/util.py")
		}
	})

	t.Run("relative dest resolves against workdir", func(t *testing.T) {
		l, err := buildNativeCopyLayer(dir, spec.CopyEntry{From: "local", Paths: []string{"main.py"}}, "/srv")
		if err != nil {
			t.Fatal(err)
		}
		hdrs, _, _ := readLayerEntries(t, l.Blob)
		if _, ok := hdrs["srv/main.py"]; !ok {
			t.Fatalf("want srv/main.py, have %v", l.FileNames)
		}
	})
}

func TestBuildNativeCopyLayerDirSourceCopiesContents(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "srcpkg/mod.py", "mod\n")
	writeFile(t, dir, "srcpkg/sub/deep.py", "deep\n")

	l, err := buildNativeCopyLayer(dir, spec.CopyEntry{From: "local", Paths: []string{"srcpkg"}, Dest: "/app"}, "")
	if err != nil {
		t.Fatal(err)
	}
	hdrs, _, _ := readLayerEntries(t, l.Blob)
	for _, want := range []string{"app/", "app/mod.py", "app/sub/", "app/sub/deep.py"} {
		if _, ok := hdrs[want]; !ok {
			t.Fatalf("missing %q; have %v", want, l.FileNames)
		}
	}
	if _, ok := hdrs["app/srcpkg/mod.py"]; ok {
		t.Fatal("dir source must copy CONTENTS, not the dir itself")
	}
}

func TestBuildNativeCopyLayerPreservesModeAndSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("modes/symlinks not applicable on windows")
	}
	dir := t.TempDir()
	writeFile(t, dir, "tool/run.sh", "#!/bin/sh\n")
	if err := os.Chmod(filepath.Join(dir, "tool", "run.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("run.sh", filepath.Join(dir, "tool", "alias")); err != nil {
		t.Fatal(err)
	}

	l, err := buildNativeCopyLayer(dir, spec.CopyEntry{From: "local", Paths: []string{"tool"}, Dest: "/tool"}, "")
	if err != nil {
		t.Fatal(err)
	}
	hdrs, _, _ := readLayerEntries(t, l.Blob)
	if h := hdrs["tool/run.sh"]; h == nil || h.Mode&0o777 != 0o755 {
		t.Fatalf("mode not preserved: %+v", h)
	}
	if h := hdrs["tool/alias"]; h == nil || h.Typeflag != tar.TypeSymlink || h.Linkname != "run.sh" {
		t.Fatalf("symlink not preserved: %+v", h)
	}
}

func TestBuildNativeCopyLayerDeterministic(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a/x.py", "x\n")
	writeFile(t, dir, "a/y.py", "y\n")
	writeFile(t, dir, "main.py", "m\n")

	entry := spec.CopyEntry{From: "local", Paths: []string{"main.py", "a"}, Dest: "app/"}
	l1, err := buildNativeCopyLayer(dir, entry, "")
	if err != nil {
		t.Fatal(err)
	}
	l2, err := buildNativeCopyLayer(dir, entry, "")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(l1.Blob, l2.Blob) {
		t.Fatal("two builds of identical inputs must be byte-identical")
	}
	if !sort.StringsAreSorted(l1.FileNames) {
		t.Fatalf("FileNames must be sorted: %v", l1.FileNames)
	}
}

// nativeDepsHash must cover every build input EXCEPT the final stage's local
// copy files: those are the app inputs whose changes the native path handles
// without buildx.
func TestNativeDepsHash(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "Dockerfile.generated", "FROM python@sha256:abc\nCOPY requirements.txt requirements.txt\nRUN pip install -r requirements.txt\nCOPY main.py main.py\n")
	writeFile(t, dir, "build.stagefile.lock.yaml", "version: 1\nimages:\n  python:3.11-slim: sha256:abc\n")
	writeFile(t, dir, "requirements.txt", "mcp\n")
	writeFile(t, dir, "main.py", "print('v1')\n")
	sf := &spec.File{Version: 1, Stages: []spec.Stage{{
		Name:    "app",
		From:    "python:3.11-slim",
		Install: &spec.Install{Pip: []spec.PipInstall{{Requirements: "requirements.txt"}}},
		Copy:    []spec.CopyEntry{{From: "local", Paths: []string{"main.py"}}},
	}}}

	hash := func(args map[string]string, platform string) string {
		t.Helper()
		h, err := nativeDepsHash(dir, "Dockerfile.generated", platform, args, sf)
		if err != nil {
			t.Fatal(err)
		}
		return h
	}

	base := hash(nil, "linux/arm64")
	if got := hash(nil, "linux/arm64"); got != base {
		t.Fatal("deps hash not stable")
	}
	writeFile(t, dir, "main.py", "print('v2')\n")
	if got := hash(nil, "linux/arm64"); got != base {
		t.Fatal("deps hash must ignore final-stage copy files (app inputs)")
	}
	writeFile(t, dir, "requirements.txt", "mcp\nuvicorn\n")
	afterReqs := hash(nil, "linux/arm64")
	if afterReqs == base {
		t.Fatal("deps hash unchanged after requirements.txt edit")
	}
	writeFile(t, dir, "Dockerfile.generated", "FROM python@sha256:def\n")
	afterDF := hash(nil, "linux/arm64")
	if afterDF == afterReqs {
		t.Fatal("deps hash unchanged after Dockerfile.generated edit")
	}
	if got := hash(map[string]string{"K": "V"}, "linux/arm64"); got == afterDF {
		t.Fatal("deps hash unchanged after build-arg change")
	}
	if got := hash(nil, "linux/amd64"); got == afterDF {
		t.Fatal("deps hash unchanged after platform change")
	}
}

func TestNativeSemanticDepsKeyTracksDependencyFrontier(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "build.stagefile.lock.yaml", "version: 1\nimages:\n  python:3.11-slim: sha256:base\n")
	writeFile(t, dir, "requirements.txt", "mcp\n")
	writeFile(t, dir, "main.py", "print('v1')\n")
	sf := &spec.File{Version: 1, Stages: []spec.Stage{{
		Name: "app",
		From: "python:3.11-slim",
		Install: &spec.Install{
			Apt: &spec.AptInstall{Packages: []string{"ca-certificates"}},
			Pip: []spec.PipInstall{{Requirements: "requirements.txt"}},
		},
		Copy: []spec.CopyEntry{{From: "local", Paths: []string{"main.py"}}},
	}}}

	key := func() string {
		t.Helper()
		got, err := nativeSemanticDepsKey(dir, "Dockerfile.generated", "linux/arm64", sf)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(got, "sha256:") {
			t.Fatalf("semantic key = %q, want sha256 digest", got)
		}
		return got
	}

	base := key()
	writeFile(t, dir, "main.py", "print('v2')\n")
	if got := key(); got != base {
		t.Fatal("final app copy changed the dependency-frontier key")
	}
	writeFile(t, dir, "requirements.txt", "mcp\nuvicorn\n")
	afterRequirements := key()
	if afterRequirements == base {
		t.Fatal("requirements edit did not change the dependency-frontier key")
	}
	sf.Stages[0].Install.Apt.Packages = append(sf.Stages[0].Install.Apt.Packages, "curl")
	if got := key(); got == afterRequirements {
		t.Fatal("semantic apt change did not change the dependency-frontier key")
	}
}

// Local copies in NON-final stages feed deps layers and must be part of the
// deps hash (only the final stage's local copies are app inputs).
// A uv install's dependency set lives entirely in pyproject.toml and uv.lock.
// Neither appears in the generated Dockerfile — `COPY pyproject.toml uv.lock ./`
// and `RUN uv sync --frozen` are the same bytes whatever the lock says — so if
// the deps hash does not read them, editing a dependency leaves the native path
// splicing new app layers onto STALE dependency layers and shipping an image
// that never contains the change.
func TestNativeDepsHashCoversUvManifests(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "Dockerfile.generated", "FROM python@sha256:abc\nCOPY pyproject.toml uv.lock ./\nRUN uv sync --frozen --no-dev\nCOPY main.py main.py\n")
	writeFile(t, dir, "pyproject.toml", "[project]\nname = \"app\"\n")
	writeFile(t, dir, "uv.lock", "version = 1\n")
	writeFile(t, dir, "main.py", "print('v1')\n")
	sf := &spec.File{Version: 1, Stages: []spec.Stage{{
		Name:    "app",
		From:    "python:3.11-slim",
		Install: &spec.Install{Uv: &spec.UvInstall{}},
		Copy:    []spec.CopyEntry{{From: "local", Paths: []string{"main.py"}}},
	}}}

	hash := func() string {
		t.Helper()
		h, err := nativeDepsHash(dir, "Dockerfile.generated", "linux/arm64", nil, sf)
		if err != nil {
			t.Fatal(err)
		}
		return h
	}

	base := hash()
	writeFile(t, dir, "uv.lock", "version = 1\n# pinned httpx 0.28\n")
	afterLock := hash()
	if afterLock == base {
		t.Fatal("deps hash unchanged after uv.lock edit: native path would ship stale dependencies")
	}
	writeFile(t, dir, "pyproject.toml", "[project]\nname = \"app\"\ndependencies = [\"httpx\"]\n")
	if got := hash(); got == afterLock {
		t.Fatal("deps hash unchanged after pyproject.toml edit: native path would ship stale dependencies")
	}
}

func TestNativeDepsHashIncludesNonFinalCopies(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "Dockerfile.generated", "FROM base\n")
	writeFile(t, dir, "settings.conf", "cfg v1\n")
	writeFile(t, dir, "main.py", "print('v1')\n")
	sf := &spec.File{Version: 1, Stages: []spec.Stage{
		{Name: "cfg", From: "python:3.11-slim", Copy: []spec.CopyEntry{{From: "local", Paths: []string{"settings.conf"}, Dest: "/etc/app/"}}},
		{Name: "app", From: "python:3.11-slim", Copy: []spec.CopyEntry{
			{From: "cfg", Paths: []string{"/etc/app"}, Dest: "/etc/app"},
			{From: "local", Paths: []string{"main.py"}},
		}},
	}}

	hash := func() string {
		t.Helper()
		h, err := nativeDepsHash(dir, "Dockerfile.generated", "linux/arm64", nil, sf)
		if err != nil {
			t.Fatal(err)
		}
		return h
	}

	base := hash()
	writeFile(t, dir, "main.py", "print('v2')\n")
	if got := hash(); got != base {
		t.Fatal("final-stage copy file must not affect the deps hash")
	}
	writeFile(t, dir, "settings.conf", "cfg v2\n")
	if got := hash(); got == base {
		t.Fatal("non-final-stage copy file must affect the deps hash")
	}
}

func TestNativeStateRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if _, ok := loadNativeState(dir); ok {
		t.Fatal("missing state must load as not-ok")
	}
	want := nativeState{
		DepsHash:          "sha256:aaa",
		ManifestDigest:    "sha256:bbb",
		AppLayerDigests:   []string{"sha256:ccc"},
		AppLayerPositions: []int{2},
	}
	if err := saveNativeState(dir, want); err != nil {
		t.Fatal(err)
	}
	got, ok := loadNativeState(dir)
	if !ok || got.DepsHash != want.DepsHash || got.ManifestDigest != want.ManifestDigest || len(got.AppLayerDigests) != 1 || got.AppLayerDigests[0] != "sha256:ccc" || len(got.AppLayerPositions) != 1 || got.AppLayerPositions[0] != 2 {
		t.Fatalf("round-trip mismatch: %+v ok=%v", got, ok)
	}

	for name, state := range map[string]nativeState{
		"missing positions": {
			DepsHash: "sha256:aaa", ManifestDigest: "sha256:bbb", AppLayerDigests: []string{"sha256:ccc"},
		},
		"duplicate positions": {
			DepsHash: "sha256:aaa", ManifestDigest: "sha256:bbb", AppLayerDigests: []string{"sha256:ccc", "sha256:ddd"}, AppLayerPositions: []int{2, 2},
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := saveNativeState(dir, state); err != nil {
				t.Fatal(err)
			}
			if _, ok := loadNativeState(dir); ok {
				t.Fatal("invalid position mapping must load as not-ok")
			}
		})
	}

	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte("{corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := loadNativeState(dir); ok {
		t.Fatal("corrupt state must load as not-ok")
	}
}

// tarBytes builds an uncompressed tar with the given entries (a trailing "/"
// in the name makes a directory entry).
func tarBytes(t *testing.T, entries map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	names := make([]string, 0, len(entries))
	for n := range entries {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		hdr := &tar.Header{Name: n, Mode: 0o644, Size: int64(len(entries[n]))}
		if strings.HasSuffix(n, "/") {
			hdr.Typeflag = tar.TypeDir
			hdr.Mode = 0o755
			hdr.Size = 0
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if hdr.Typeflag != tar.TypeDir {
			if _, err := tw.Write([]byte(entries[n])); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// writeTwoLayerLayoutDir builds a layout dir whose image has a deps layer and
// an app layer (both uncompressed tars), with aligned diff_ids and the given
// WorkingDir. Returns the app layer's digest.
func writeTwoLayerLayoutDir(t *testing.T, dir string, depsTar, appTar []byte, workingDir string) (appDigest string) {
	t.Helper()
	depsDigest := "sha256:" + sha256Hex(depsTar)
	appDigest = "sha256:" + sha256Hex(appTar)
	config := []byte(`{"architecture":"arm64","os":"linux","config":{"Entrypoint":["python","main.py"],"WorkingDir":"` + workingDir + `"},"rootfs":{"type":"layers","diff_ids":["` + depsDigest + `","` + appDigest + `"]}}`)
	configDigest := "sha256:" + sha256Hex(config)
	manifest := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json",` +
		`"config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":"` + configDigest + `","size":` + fmt.Sprint(len(config)) + `},` +
		`"layers":[` +
		`{"mediaType":"application/vnd.oci.image.layer.v1.tar","digest":"` + depsDigest + `","size":` + fmt.Sprint(len(depsTar)) + `},` +
		`{"mediaType":"application/vnd.oci.image.layer.v1.tar","digest":"` + appDigest + `","size":` + fmt.Sprint(len(appTar)) + `}]}`)
	manifestDigest := sha256Hex(manifest)
	index := []byte(`{"schemaVersion":2,"manifests":[{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"sha256:` + manifestDigest + `","size":` + fmt.Sprint(len(manifest)) + `,"platform":{"os":"linux","architecture":"arm64"}}]}`)
	writeOCILayoutDir(t, dir, map[string][]byte{
		"oci-layout":                         []byte(`{"imageLayoutVersion":"1.0.0"}`),
		"index.json":                         index,
		"blobs/sha256/" + manifestDigest:     manifest,
		"blobs/sha256/" + sha256Hex(config):  config,
		"blobs/sha256/" + sha256Hex(depsTar): depsTar,
		"blobs/sha256/" + sha256Hex(appTar):  appTar,
	})
	return appDigest
}

// writeLayerLayoutDir builds a layout with an arbitrary ordered layer list and
// aligned diff IDs. Layers are uncompressed tar blobs, which exercises the
// same reader path as buildx's OCI exporter without making tests depend on
// Docker.
func writeLayerLayoutDir(t *testing.T, dir string, layerTars [][]byte, workingDir string) []string {
	t.Helper()
	digests := make([]string, len(layerTars))
	diffIDs := make([]string, len(layerTars))
	descs := make([]ociManifestDesc, len(layerTars))
	entries := map[string][]byte{
		"oci-layout": []byte(`{"imageLayoutVersion":"1.0.0"}`),
	}
	for i, layer := range layerTars {
		digests[i] = "sha256:" + sha256Hex(layer)
		diffIDs[i] = digests[i]
		descs[i] = ociManifestDesc{
			MediaType: "application/vnd.oci.image.layer.v1.tar",
			Digest:    digests[i],
			Size:      int64(len(layer)),
		}
		entries["blobs/sha256/"+sha256Hex(layer)] = layer
	}
	config, err := json.Marshal(map[string]any{
		"architecture": "arm64",
		"os":           "linux",
		"config":       map[string]any{"WorkingDir": workingDir},
		"rootfs":       map[string]any{"type": "layers", "diff_ids": diffIDs},
	})
	if err != nil {
		t.Fatal(err)
	}
	configDigest := "sha256:" + sha256Hex(config)
	manifest, err := json.Marshal(ociManifest{
		SchemaVersion: 2,
		MediaType:     "application/vnd.oci.image.manifest.v1+json",
		Config: ociManifestDesc{
			MediaType: "application/vnd.oci.image.config.v1+json",
			Digest:    configDigest,
			Size:      int64(len(config)),
		},
		Layers: descs,
	})
	if err != nil {
		t.Fatal(err)
	}
	manifestDigest := "sha256:" + sha256Hex(manifest)
	index, err := json.Marshal(map[string]any{
		"schemaVersion": 2,
		"manifests": []map[string]any{{
			"mediaType": "application/vnd.oci.image.manifest.v1+json",
			"digest":    manifestDigest,
			"size":      len(manifest),
			"platform":  map[string]any{"os": "linux", "architecture": "arm64"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	entries["index.json"] = index
	entries["blobs/sha256/"+sha256Hex(config)] = config
	entries["blobs/sha256/"+sha256Hex(manifest)] = manifest
	writeOCILayoutDir(t, dir, entries)
	return digests
}

func TestNativeAppLayerPositions(t *testing.T) {
	sf := &spec.File{Stages: []spec.Stage{{Copy: []spec.CopyEntry{
		{From: "local", Paths: []string{"a"}},
		{From: "builder", Paths: []string{"/out"}},
		{From: "local", Paths: []string{"b"}},
	}}}}
	got, ok := nativeAppLayerPositions(7, sf)
	if !ok || len(got) != 2 || got[0] != 4 || got[1] != 6 {
		t.Fatalf("nativeAppLayerPositions = %v, %v; want [4 6], true", got, ok)
	}
	if _, ok := nativeAppLayerPositions(2, sf); ok {
		t.Fatal("a manifest shorter than the complete final copy list must be rejected")
	}
	sf.Stages[0].Build = &spec.Build{Lang: "go"}
	if _, ok := nativeAppLayerPositions(7, sf); ok {
		t.Fatal("a final-stage build after copy layers must be rejected")
	}
}

func TestAdoptNativeLayersInterleavedWithStageCopies(t *testing.T) {
	proj := t.TempDir()
	writeFile(t, proj, "a.txt", "a-v1\n")
	writeFile(t, proj, "b.txt", "b-v1\n")
	sf := &spec.File{Version: 1, Stages: []spec.Stage{
		{Name: "builder", From: "debian:12"},
		{Name: "app", From: "debian:12", Copy: []spec.CopyEntry{
			{From: "local", Paths: []string{"a.txt"}, Dest: "app/"},
			{From: "builder", Paths: []string{"/out"}, Dest: "/runtime/out"},
			{From: "local", Paths: []string{"b.txt"}, Dest: "app/"},
		}},
	}}

	baseTar := tarBytes(t, map[string]string{"usr/lib/base": "base"})
	aTar := tarBytes(t, map[string]string{"app/": "", "app/a.txt": "a-v1\n"})
	bTar := tarBytes(t, map[string]string{"app/": "", "app/b.txt": "b-v1\n"})
	// Deliberately duplicate the following local layer blob. Digest-only
	// splicing would find this cross-stage layer first and replace the wrong
	// descriptor; explicit instruction positions must leave it untouched.
	crossStageTar := bTar
	dir := t.TempDir()
	original := writeLayerLayoutDir(t, dir, [][]byte{baseTar, aTar, crossStageTar, bTar}, "/")

	ok, err := adoptNativeLayers(dir, "linux/arm64", proj, sf, "sha256:deps1")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("adoption should accept verified local copies interleaved with stage copies")
	}
	st, stateOK := loadNativeState(dir)
	if !stateOK || len(st.AppLayerPositions) != 2 || st.AppLayerPositions[0] != 1 || st.AppLayerPositions[1] != 3 {
		t.Fatalf("state positions = %+v, ok=%v; want [1 3]", st, stateOK)
	}
	layers, _, err := readOCILayoutDirLayers(dir, "linux/arm64")
	if err != nil {
		t.Fatal(err)
	}
	if layers[0].Digest != original[0] || layers[2].Digest != original[2] {
		t.Fatal("adoption changed a base or interleaved cross-stage layer")
	}
	if layers[1].Digest == original[1] || layers[3].Digest == original[3] {
		t.Fatal("adoption did not replace both mapped local-copy layers")
	}

	firstNative := layers[1].Digest
	writeFile(t, proj, "b.txt", "b-v2\n")
	rebuilt, err := tryNativeRebuild(dir, "linux/arm64", proj, sf, st)
	if err != nil {
		t.Fatal(err)
	}
	if !rebuilt {
		t.Fatal("interleaved local copies should rebuild without buildx")
	}
	layers, _, err = readOCILayoutDirLayers(dir, "linux/arm64")
	if err != nil {
		t.Fatal(err)
	}
	if layers[0].Digest != original[0] || layers[2].Digest != original[2] {
		t.Fatal("native rebuild changed a base or interleaved cross-stage layer")
	}
	if layers[1].Digest != firstNative {
		t.Fatal("unchanged first local-copy layer changed")
	}
	if layers[3].Digest == st.AppLayerDigests[1] {
		t.Fatal("edited second local-copy layer did not change")
	}
}

func TestAdoptAndSpliceNativeLayers(t *testing.T) {
	proj := t.TempDir()
	writeFile(t, proj, "main.py", "print('v1')\n")
	sf := &spec.File{Version: 1, Stages: []spec.Stage{{
		Name: "app", From: "python:3.11-slim",
		Copy: []spec.CopyEntry{{From: "local", Paths: []string{"main.py"}, Dest: "app/"}},
	}}}

	dir := t.TempDir()
	depsTar := tarBytes(t, map[string]string{"usr/lib/python/dep.py": "dep"})
	appTar := tarBytes(t, map[string]string{"app/": "", "app/main.py": "print('v1')\n"})
	oldAppDigest := writeTwoLayerLayoutDir(t, dir, depsTar, appTar, "")

	ok, err := adoptNativeLayers(dir, "linux/arm64", proj, sf, "sha256:deps1")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("adoption should accept a matching file set")
	}

	st, stOK := loadNativeState(dir)
	if !stOK || st.DepsHash != "sha256:deps1" || len(st.AppLayerDigests) != 1 {
		t.Fatalf("state not recorded: %+v ok=%v", st, stOK)
	}
	if st.AppLayerDigests[0] == oldAppDigest {
		t.Fatal("adopted app layer should be the native rebuild, not the buildx layer")
	}

	// The layout must remain readable and now expose the native layer with the
	// deps layer untouched.
	layers, cfg, err := readOCILayoutDirLayers(dir, "linux/arm64")
	if err != nil {
		t.Fatal(err)
	}
	if len(layers) != 2 {
		t.Fatalf("want 2 layers, got %d", len(layers))
	}
	if layers[0].Digest != "sha256:"+sha256Hex(depsTar) {
		t.Fatal("deps layer must be untouched")
	}
	if layers[1].Digest != st.AppLayerDigests[0] {
		t.Fatal("manifest must reference the native app layer")
	}
	if !bytes.Contains(cfg, []byte(`"Entrypoint":["python","main.py"]`)) {
		t.Fatal("config runtime fields must survive the splice")
	}
	if !bytes.Contains(cfg, []byte(layers[1].DiffID)) {
		t.Fatal("config diff_ids must reference the native layer's diff ID")
	}

	// Now the iteration loop: edit the app file, rebuild natively without any
	// buildx involvement.
	writeFile(t, proj, "main.py", "print('v2')\n")
	rebuilt, err := tryNativeRebuild(dir, "linux/arm64", proj, sf, st)
	if err != nil {
		t.Fatal(err)
	}
	if !rebuilt {
		t.Fatal("native rebuild should succeed after an app-only edit")
	}
	layers2, cfg2, err := readOCILayoutDirLayers(dir, "linux/arm64")
	if err != nil {
		t.Fatal(err)
	}
	if layers2[1].Digest == layers[1].Digest {
		t.Fatal("app layer digest should change after the edit")
	}
	if layers2[0].Digest != layers[0].Digest {
		t.Fatal("deps layer must still be untouched")
	}
	if !bytes.Contains(cfg2, []byte(layers2[1].DiffID)) {
		t.Fatal("config diff_ids must track the rebuilt layer")
	}
	st2, _ := loadNativeState(dir)
	if st2 == nil || st2.AppLayerDigests[0] != layers2[1].Digest {
		t.Fatal("state must track the rebuilt layer")
	}

	// A second pass with byte-identical app inputs must be a true no-op. In
	// particular, do not rewrite index/state merely because the native fast path
	// was entered; those writes made zero-change Compose runs look dirty.
	indexPath := filepath.Join(dir, "index.json")
	statePath := filepath.Join(dir, nativeStateName)
	indexBefore, err := os.Stat(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	stateBefore, err := os.Stat(statePath)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	unchanged, err := tryNativeRebuild(dir, "linux/arm64", proj, sf, st2)
	if err != nil || !unchanged {
		t.Fatalf("unchanged native rebuild = %v, %v", unchanged, err)
	}
	indexAfter, _ := os.Stat(indexPath)
	stateAfter, _ := os.Stat(statePath)
	if !indexAfter.ModTime().Equal(indexBefore.ModTime()) || !stateAfter.ModTime().Equal(stateBefore.ModTime()) {
		t.Fatal("unchanged native rebuild rewrote index.json or state.json")
	}
}

func TestBuildOrUpdateOCILayoutSkipsBuildxAfterAdoption(t *testing.T) {
	proj := t.TempDir()
	writeFile(t, proj, "build.stagefile.yaml", `version: 1
stages:
  - name: app
    from: python:3.11-slim
    copy:
      - from: local
        paths: [main.py]
        dest: app/
`)
	writeFile(t, proj, "Dockerfile.generated", "FROM python:3.11-slim AS app\nCOPY main.py app/\n")
	writeFile(t, proj, "main.py", "print('v1')\n")

	layout := t.TempDir()
	buildxCalls := 0
	buildx := func() error {
		buildxCalls++
		depsTar := tarBytes(t, map[string]string{"usr/lib/python/dep.py": "dep"})
		appTar := tarBytes(t, map[string]string{"app/": "", "app/main.py": "print('v1')\n"})
		writeTwoLayerLayoutDir(t, layout, depsTar, appTar, "")
		return nil
	}

	native, err := buildOrUpdateOCILayout(proj, "Dockerfile.generated", "linux/arm64", nil, layout, buildx)
	if err != nil {
		t.Fatal(err)
	}
	if native || buildxCalls != 1 {
		t.Fatalf("first build: native=%v buildxCalls=%d, want false/1", native, buildxCalls)
	}
	if _, ok := loadNativeState(layout); !ok {
		t.Fatal("first build should adopt native app layers")
	}

	writeFile(t, proj, "main.py", "print('v2')\n")
	native, err = buildOrUpdateOCILayout(proj, "Dockerfile.generated", "linux/arm64", nil, layout, buildx)
	if err != nil {
		t.Fatal(err)
	}
	if !native || buildxCalls != 1 {
		t.Fatalf("warm app edit: native=%v buildxCalls=%d, want true/1", native, buildxCalls)
	}
}

func TestAdoptNativeLayersRejectsFileSetMismatch(t *testing.T) {
	proj := t.TempDir()
	writeFile(t, proj, "main.py", "print('v1')\n")
	sf := &spec.File{Version: 1, Stages: []spec.Stage{{
		Name: "app", From: "python:3.11-slim",
		Copy: []spec.CopyEntry{{From: "local", Paths: []string{"main.py"}, Dest: "app/"}},
	}}}

	dir := t.TempDir()
	depsTar := tarBytes(t, map[string]string{"usr/lib/python/dep.py": "dep"})
	// The "buildx" app layer holds an extra file the copy entry doesn't declare
	// — the position→instruction assumption is wrong, adoption must refuse.
	appTar := tarBytes(t, map[string]string{"app/": "", "app/main.py": "m", "app/surprise.py": "s"})
	writeTwoLayerLayoutDir(t, dir, depsTar, appTar, "")

	before, _ := os.ReadFile(filepath.Join(dir, "index.json"))
	ok, err := adoptNativeLayers(dir, "linux/arm64", proj, sf, "sha256:deps1")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("adoption must reject a mismatched file set")
	}
	after, _ := os.ReadFile(filepath.Join(dir, "index.json"))
	if !bytes.Equal(before, after) {
		t.Fatal("a rejected adoption must leave the layout untouched")
	}
	if _, stOK := loadNativeState(dir); stOK {
		t.Fatal("a rejected adoption must not record state")
	}
}

func TestTryNativeRebuildRefusesExternalManifestChange(t *testing.T) {
	proj := t.TempDir()
	writeFile(t, proj, "main.py", "print('v1')\n")
	sf := &spec.File{Version: 1, Stages: []spec.Stage{{
		Name: "app", From: "python:3.11-slim",
		Copy: []spec.CopyEntry{{From: "local", Paths: []string{"main.py"}, Dest: "app/"}},
	}}}
	dir := t.TempDir()
	depsTar := tarBytes(t, map[string]string{"dep.py": "dep"})
	appTar := tarBytes(t, map[string]string{"app/": "", "app/main.py": "print('v1')\n"})
	writeTwoLayerLayoutDir(t, dir, depsTar, appTar, "")

	// A state whose manifest digest doesn't match the dir (someone rebuilt
	// behind our back) must refuse the native path.
	stale := &nativeState{DepsHash: "sha256:d", ManifestDigest: "sha256:not-the-real-one", AppLayerDigests: []string{"sha256:x"}}
	ok, err := tryNativeRebuild(dir, "linux/arm64", proj, sf, stale)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("native rebuild must refuse when the manifest changed externally")
	}
}

func TestNativeBuildEligibility(t *testing.T) {
	t.Setenv("WENDY_NATIVE_LAYERS", "")

	eligibleYAML := "version: 1\nstages:\n  - name: app\n    from: python:3.11-slim\n    install:\n      pip:\n        - requirements: requirements.txt\n    copy:\n      - from: local\n        paths: [main.py]\n    entrypoint:\n      exec: [python, main.py]\n"

	t.Run("eligible python project", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, dir, "build.stagefile.yaml", eligibleYAML)
		sf, ok := nativeBuildEligibility(dir, "Dockerfile.generated")
		if !ok || sf == nil {
			t.Fatal("python-style stagefile project should be eligible")
		}
	})

	t.Run("plain dockerfile name is ineligible", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, dir, "build.stagefile.yaml", eligibleYAML)
		if _, ok := nativeBuildEligibility(dir, "Dockerfile"); ok {
			t.Fatal("non-generated dockerfile must be ineligible")
		}
	})

	t.Run("no stagefile is ineligible", func(t *testing.T) {
		if _, ok := nativeBuildEligibility(t.TempDir(), "Dockerfile.generated"); ok {
			t.Fatal("missing stagefile must be ineligible")
		}
	})

	t.Run("final-stage build.lang is ineligible", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, dir, "build.stagefile.yaml", "version: 1\nstages:\n  - name: app\n    from: rust:1\n    copy:\n      - from: local\n        paths: [src]\n    build:\n      lang: rust\n")
		if _, ok := nativeBuildEligibility(dir, "Dockerfile.generated"); ok {
			t.Fatal("a compiling final stage must be ineligible")
		}
	})

	t.Run("no local copies is ineligible", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, dir, "build.stagefile.yaml", "version: 1\nstages:\n  - name: builder\n    from: golang:1\n    copy:\n      - from: local\n        paths: [main.go]\n  - name: app\n    from: debian:12\n    copy:\n      - from: builder\n        paths: [/out]\n        dest: /out\n")
		if _, ok := nativeBuildEligibility(dir, "Dockerfile.generated"); ok {
			t.Fatal("a final stage without local copies must be ineligible")
		}
	})

	t.Run("local copy before a stage copy is eligible", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, dir, "build.stagefile.yaml", "version: 1\nstages:\n  - name: builder\n    from: golang:1\n    copy:\n      - from: local\n        paths: [main.go]\n  - name: app\n    from: debian:12\n    copy:\n      - from: local\n        paths: [main.go]\n      - from: builder\n        paths: [/out]\n        dest: /out\n")
		if _, ok := nativeBuildEligibility(dir, "Dockerfile.generated"); !ok {
			t.Fatal("interleaved local and stage copies should be eligible")
		}
	})

	t.Run("env kill switch", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, dir, "build.stagefile.yaml", eligibleYAML)
		t.Setenv("WENDY_NATIVE_LAYERS", "off")
		if _, ok := nativeBuildEligibility(dir, "Dockerfile.generated"); ok {
			t.Fatal("WENDY_NATIVE_LAYERS=off must disable the native path")
		}
	})
}

func TestBuildNativeCopyLayerRejectsEscapes(t *testing.T) {
	dir := t.TempDir()
	if _, err := buildNativeCopyLayer(dir, spec.CopyEntry{From: "local", Paths: []string{"../outside"}}, ""); err == nil {
		t.Fatal("want error for source escaping the context")
	}
	if _, err := buildNativeCopyLayer(dir, spec.CopyEntry{From: "local", Paths: []string{"/etc/passwd"}}, ""); err == nil {
		t.Fatal("want error for absolute source path")
	}
	if _, err := buildNativeCopyLayer(dir, spec.CopyEntry{From: "local", Paths: []string{"missing.py"}}, ""); err == nil {
		t.Fatal("want error for missing source")
	}
}
