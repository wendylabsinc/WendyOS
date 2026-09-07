package commands

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestIsImageBuildFailure verifies the classification used by the chunk-diff
// deploy path to decide whether the registry-push fallback would help. Only an
// imageBuildFailedError (the Dockerfile build itself failing) suppresses the
// fallback; builder-setup and transport failures stay eligible for it. (#1166)
func TestIsImageBuildFailure(t *testing.T) {
	buildErr := &imageBuildFailedError{errors.New("colcon: error: unrecognized arguments: --log-base log")}

	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"plain transport error", errors.New("QueryChunks unimplemented"), false},
		{"setup error", errors.New("adding hosts entry to builder: sed: can't move"), false},
		{"build failure", buildErr, true},
		{"wrapped build failure", fmt.Errorf("deploy failed: %w", buildErr), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isImageBuildFailure(tc.err); got != tc.want {
				t.Fatalf("isImageBuildFailure(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}

	// The error message must surface the underlying build error verbatim so the
	// user sees the actionable failure, not a fallback-specific wrapper.
	if got := buildErr.Error(); got != "colcon: error: unrecognized arguments: --log-base log" {
		t.Fatalf("imageBuildFailedError.Error() = %q, want underlying message", got)
	}
	if !errors.Is(buildErr, buildErr.err) {
		t.Fatal("imageBuildFailedError should unwrap to its underlying error")
	}
}

// TestTotalCompressedLayerBytes covers file-backed layers (Size), in-memory
// layers (len(Blob)), a mix of both, and the empty-slice case.
func TestTotalCompressedLayerBytes(t *testing.T) {
	cases := []struct {
		name   string
		layers []localLayer
		want   int64
	}{
		{"empty", nil, 0},
		{"file-backed", []localLayer{{TarPath: "/tmp/a.tar", Size: 100}, {TarPath: "/tmp/b.tar", Size: 250}}, 350},
		{"in-memory", []localLayer{{Blob: make([]byte, 10)}, {Blob: make([]byte, 5)}}, 15},
		{"mixed", []localLayer{{TarPath: "/tmp/a.tar", Size: 100}, {Blob: make([]byte, 5)}, {TarPath: "/tmp/c.tar", Size: 20}}, 125},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := totalCompressedLayerBytes(tc.layers); got != tc.want {
				t.Fatalf("totalCompressedLayerBytes(%+v) = %d, want %d", tc.layers, got, tc.want)
			}
		})
	}
}

// sha256Hex returns the lowercase hex-encoded SHA-256 digest of b.
func sha256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// writeOCITar writes a tar file at path containing the provided entries
// (name → data).
func writeOCITar(t *testing.T, path string, entries map[string][]byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	tw := tar.NewWriter(f)
	// Deterministic entry order: oci-layout, index.json, then blobs.
	orderedNames := []string{"oci-layout", "index.json"}
	blobNames := []string{}
	for name := range entries {
		if name != "oci-layout" && name != "index.json" {
			blobNames = append(blobNames, name)
		}
	}
	orderedNames = append(orderedNames, blobNames...)
	for _, name := range orderedNames {
		data, ok := entries[name]
		if !ok {
			continue
		}
		hdr := &tar.Header{
			Name: name,
			Mode: 0o644,
			Size: int64(len(data)),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
}

// writeMinimalOCILayout builds a minimal OCI-layout tar at path with a single
// layer whose blob bytes are blobData. mediaType is the layer media type.
// For uncompressed layers pass the raw tar as blobData; for compressed layers
// pass the compressed form. diffIDBytes is the UNCOMPRESSED bytes used to
// compute the DiffID in the config (for uncompressed layers it equals blobData).
func writeMinimalOCILayout(t *testing.T, path string, blobData []byte, mediaType string, diffIDBytes []byte) {
	t.Helper()
	writeOCITar(t, path, minimalOCILayoutEntries(t, blobData, mediaType, diffIDBytes))
}

// minimalOCILayoutEntries builds the entry map (name → data) for a minimal
// single-layer OCI layout, shared by the tar and directory fixture writers.
func minimalOCILayoutEntries(t *testing.T, blobData []byte, mediaType string, diffIDBytes []byte) map[string][]byte {
	t.Helper()

	diffID := "sha256:" + sha256Hex(diffIDBytes)
	layerDigest := "sha256:" + sha256Hex(blobData)

	configBytes := []byte(`{"architecture":"amd64","os":"linux","config":{"Cmd":["python","app.py"],"WorkingDir":"/app"},"rootfs":{"type":"layers","diff_ids":["` + diffID + `"]}}`)
	configDigest := "sha256:" + sha256Hex(configBytes)

	manifest := map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.oci.image.manifest.v1+json",
		"config": map[string]any{
			"mediaType": "application/vnd.oci.image.config.v1+json",
			"digest":    configDigest,
			"size":      len(configBytes),
		},
		"layers": []map[string]any{
			{
				"mediaType": mediaType,
				"digest":    layerDigest,
				"size":      len(blobData),
			},
		},
	}
	manifestBytes, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	manifestDigest := "sha256:" + sha256Hex(manifestBytes)

	index := map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.oci.image.index.v1+json",
		"manifests": []map[string]any{
			{
				"mediaType": "application/vnd.oci.image.manifest.v1+json",
				"digest":    manifestDigest,
				"size":      len(manifestBytes),
			},
		},
	}
	indexBytes, err := json.Marshal(index)
	if err != nil {
		t.Fatal(err)
	}

	return map[string][]byte{
		"oci-layout": []byte(`{"imageLayoutVersion":"1.0.0"}`),
		"index.json": indexBytes,
		"blobs/sha256/" + sha256Hex(manifestBytes): manifestBytes,
		"blobs/sha256/" + sha256Hex(configBytes):   configBytes,
		"blobs/sha256/" + sha256Hex(blobData):      blobData,
	}
}

// writeOCILayoutDir materializes entries (same shape writeOCITar takes) as an
// OCI layout directory: "index.json" and "blobs/sha256/<hex>" become files.
func writeOCILayoutDir(t *testing.T, dir string, entries map[string][]byte) {
	t.Helper()
	for name, data := range entries {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// The layout-dir reader must return file-backed layers pointing at the blob
// files themselves (offset 0, whole file), with DiffID and config populated
// exactly like the tar reader.
func TestReadOCILayoutDirLayers(t *testing.T) {
	dir := t.TempDir()

	raw := bytes.Repeat([]byte("wendy-layout-dir-payload-"), 4000)
	var gz bytes.Buffer
	gw := gzip.NewWriter(&gz)
	if _, err := gw.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	compressed := gz.Bytes()

	writeOCILayoutDir(t, dir, minimalOCILayoutEntries(t, compressed, "application/vnd.oci.image.layer.v1.tar+gzip", raw))

	layers, imageConfig, err := readOCILayoutDirLayers(dir, "linux/arm64")
	if err != nil {
		t.Fatal(err)
	}
	if len(layers) != 1 {
		t.Fatalf("want 1 layer, got %d", len(layers))
	}
	l := layers[0]

	wantPath := filepath.Join(dir, "blobs", "sha256", sha256Hex(compressed))
	if l.TarPath != wantPath {
		t.Fatalf("layer path = %q, want %q", l.TarPath, wantPath)
	}
	if l.Offset != 0 {
		t.Fatalf("layer offset = %d, want 0", l.Offset)
	}
	if l.Size != int64(len(compressed)) {
		t.Fatalf("layer size = %d, want %d", l.Size, len(compressed))
	}
	if want := "sha256:" + sha256Hex(raw); l.DiffID != want {
		t.Fatalf("diffID = %q, want %q", l.DiffID, want)
	}

	got, err := l.decompress()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, raw) {
		t.Fatal("decompressed bytes do not match original raw tar")
	}
	if !bytes.Contains(imageConfig, []byte(`"WorkingDir":"/app"`)) {
		t.Fatal("image config not round-tripped")
	}
}

// A blob file whose size disagrees with the manifest descriptor is a partial
// write (e.g. a killed export); the reader must fail loudly so the caller can
// self-heal by wiping the directory.
func TestReadOCILayoutDirLayersSizeMismatch(t *testing.T) {
	dir := t.TempDir()

	raw := []byte("layer-tar-bytes-for-truncation")
	entries := minimalOCILayoutEntries(t, raw, "application/vnd.oci.image.layer.v1.tar", raw)
	writeOCILayoutDir(t, dir, entries)

	blobPath := filepath.Join(dir, "blobs", "sha256", sha256Hex(raw))
	if err := os.Truncate(blobPath, int64(len(raw)-5)); err != nil {
		t.Fatal(err)
	}

	_, _, err := readOCILayoutDirLayers(dir, "linux/arm64")
	if err == nil {
		t.Fatal("want error for truncated layer blob, got nil")
	}
	if !strings.Contains(err.Error(), sha256Hex(raw)) {
		t.Fatalf("error should name the bad blob digest, got: %v", err)
	}
}

func TestReadOCILayoutDirLayersMissingIndex(t *testing.T) {
	_, _, err := readOCILayoutDirLayers(t.TempDir(), "linux/arm64")
	if err == nil {
		t.Fatal("want error for missing index.json, got nil")
	}
}

// Descriptor sizes are required by the OCI spec; without one the partial-write
// check would be silently skipped, so the reader must refuse.
func TestReadOCILayoutDirLayersRequiresDescriptorSize(t *testing.T) {
	dir := t.TempDir()
	raw := []byte("sizeless-layer-tar")
	layerDigest := "sha256:" + sha256Hex(raw)
	config := []byte(`{"architecture":"arm64","os":"linux","rootfs":{"type":"layers","diff_ids":["` + layerDigest + `"]}}`)
	configDigest := "sha256:" + sha256Hex(config)
	manifest := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json",` +
		`"config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":"` + configDigest + `","size":` + fmt.Sprint(len(config)) + `},` +
		`"layers":[{"mediaType":"application/vnd.oci.image.layer.v1.tar","digest":"` + layerDigest + `"}]}`)
	manifestDigest := sha256Hex(manifest)
	index := []byte(`{"schemaVersion":2,"manifests":[{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"sha256:` + manifestDigest + `","size":` + fmt.Sprint(len(manifest)) + `}]}`)
	writeOCILayoutDir(t, dir, map[string][]byte{
		"oci-layout":                        []byte(`{"imageLayoutVersion":"1.0.0"}`),
		"index.json":                        index,
		"blobs/sha256/" + manifestDigest:    manifest,
		"blobs/sha256/" + sha256Hex(config): config,
		"blobs/sha256/" + sha256Hex(raw):    raw,
	})

	_, _, err := readOCILayoutDirLayers(dir, "linux/arm64")
	if err == nil || !strings.Contains(err.Error(), "no size") {
		t.Fatalf("want a no-size error, got %v", err)
	}
}

// The persistent cache must not be world-readable: the layout parent dir is
// created 0700 and the lock file 0600.
func TestLockOCILayoutDirPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permission bits")
	}
	parent := filepath.Join(t.TempDir(), "ocilayout")
	dir := filepath.Join(parent, "app-linux_arm64")
	release, err := lockOCILayoutDir(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if fi, err := os.Stat(parent); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("layout parent perm = %v (err %v), want 0700", fi.Mode().Perm(), err)
	}
	if fi, err := os.Stat(dir + ".lock"); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("lock file perm = %v (err %v), want 0600", fi.Mode().Perm(), err)
	}
}

// GC must keep every blob reachable from index.json (all index entries, nested
// indexes and attestation manifests included) and delete only orphans left
// behind by superseded builds.
func TestGCOCILayoutDir(t *testing.T) {
	dir := t.TempDir()
	raw := []byte("gc-test-layer-tar")
	entries := minimalOCILayoutEntries(t, raw, "application/vnd.oci.image.layer.v1.tar", raw)
	orphan := []byte("orphaned-blob-from-a-previous-build")
	entries["blobs/sha256/"+sha256Hex(orphan)] = orphan
	writeOCILayoutDir(t, dir, entries)

	if err := gcOCILayoutDir(dir); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(filepath.Join(dir, "blobs", "sha256", sha256Hex(orphan))); !os.IsNotExist(err) {
		t.Fatal("orphan blob should have been deleted")
	}
	for name := range minimalOCILayoutEntries(t, raw, "application/vnd.oci.image.layer.v1.tar", raw) {
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(name))); err != nil {
			t.Fatalf("referenced entry %q should survive GC: %v", name, err)
		}
	}
}

func TestGCOCILayoutDirMissingDirIsNoop(t *testing.T) {
	if err := gcOCILayoutDir(filepath.Join(t.TempDir(), "nope")); err != nil {
		t.Fatalf("missing dir should be a no-op, got %v", err)
	}
}

// writeLayoutIndex writes an index.json with the given raw manifest entries.
func writeLayoutIndex(t *testing.T, dir string, entries ...string) {
	t.Helper()
	idx := `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[` +
		strings.Join(entries, ",") + `]}`
	if err := os.WriteFile(filepath.Join(dir, "index.json"), []byte(idx), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestPruneOCILayoutDirIndexKeepsOnlyNewestPlatformManifest(t *testing.T) {
	dir := t.TempDir()
	old := `{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"sha256:aaaa","size":1,"platform":{"architecture":"arm64","os":"linux"}}`
	att := `{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"sha256:cccc","size":1,"platform":{"architecture":"unknown","os":"unknown"},"annotations":{"vnd.docker.reference.type":"attestation-manifest"}}`
	niu := `{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"sha256:bbbb","size":1,"platform":{"architecture":"arm64","os":"linux"},"annotations":{"org.opencontainers.image.created":"x"}}`
	writeLayoutIndex(t, dir, old, niu, att)

	if err := pruneOCILayoutDirIndex(dir, "linux/arm64"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "index.json"))
	if err != nil {
		t.Fatal(err)
	}
	var idx struct {
		Manifests []json.RawMessage `json:"manifests"`
	}
	if err := json.Unmarshal(data, &idx); err != nil {
		t.Fatal(err)
	}
	if len(idx.Manifests) != 1 {
		t.Fatalf("want exactly 1 manifest entry after prune, got %d: %s", len(idx.Manifests), data)
	}
	// The kept entry must be the newest platform match, raw bytes preserved
	// (annotations intact).
	if !strings.Contains(string(idx.Manifests[0]), "sha256:bbbb") ||
		!strings.Contains(string(idx.Manifests[0]), "org.opencontainers.image.created") {
		t.Fatalf("kept entry lost identity or annotations: %s", idx.Manifests[0])
	}
}

func TestPruneOCILayoutDirIndexSingleEntryNoop(t *testing.T) {
	dir := t.TempDir()
	only := `{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"sha256:aaaa","size":1,"platform":{"architecture":"arm64","os":"linux"}}`
	writeLayoutIndex(t, dir, only)
	before, _ := os.ReadFile(filepath.Join(dir, "index.json"))
	if err := pruneOCILayoutDirIndex(dir, "linux/arm64"); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(filepath.Join(dir, "index.json"))
	if !bytes.Equal(before, after) {
		t.Fatalf("single-entry index must be untouched\nbefore: %s\nafter: %s", before, after)
	}
}

func TestPruneOCILayoutDirIndexNoMatchErrorsAndPreservesIndex(t *testing.T) {
	dir := t.TempDir()
	amd := `{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"sha256:aaaa","size":1,"platform":{"architecture":"amd64","os":"linux"}}`
	writeLayoutIndex(t, dir, amd)
	before, _ := os.ReadFile(filepath.Join(dir, "index.json"))
	if err := pruneOCILayoutDirIndex(dir, "linux/arm64"); err == nil {
		t.Fatal("want error when no manifest matches the platform")
	}
	after, _ := os.ReadFile(filepath.Join(dir, "index.json"))
	if !bytes.Equal(before, after) {
		t.Fatal("index must be untouched on error")
	}
}

// The per-app layout lock must exclude a second acquirer until released. The
// lock file lives NEXT TO the directory so self-heal's RemoveAll(dir) can
// never delete a held lock.
func TestLockOCILayoutDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "app-layout")
	release, err := lockOCILayoutDir(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir + ".lock"); err != nil {
		t.Fatalf("lock file should sit next to the dir: %v", err)
	}

	acquired := make(chan struct{})
	go func() {
		r2, err := lockOCILayoutDir(context.Background(), dir)
		if err != nil {
			t.Error(err)
			close(acquired)
			return
		}
		close(acquired)
		r2()
	}()

	select {
	case <-acquired:
		t.Fatal("second acquire should block while the lock is held")
	case <-time.After(150 * time.Millisecond):
	}

	release()
	select {
	case <-acquired:
	case <-time.After(3 * time.Second):
		t.Fatal("second acquire never succeeded after release")
	}
}

// The export-mode dispatcher: docker buildx gets the persistent layout dir;
// backends that can only emit tars (apple-container, on-device buildctl) and
// the escape-hatch env keep the legacy temp tar.
func TestChunkExportPlan(t *testing.T) {
	t.Setenv("WENDY_CHUNK_EXPORT", "")
	if got := chunkExportPlan(""); got != "dir" {
		t.Fatalf("default builder: got %q, want dir", got)
	}
	if got := chunkExportPlan("docker"); got != "dir" {
		t.Fatalf("docker builder: got %q, want dir", got)
	}
	if got := chunkExportPlan("apple-container"); got != "tar" {
		t.Fatalf("apple-container builder: got %q, want tar", got)
	}
	if got := chunkExportPlan("buildkit"); got != "tar" {
		t.Fatalf("buildkit builder: got %q, want tar", got)
	}
	if got := chunkExportPlan("no-such-builder"); got != "tar" {
		t.Fatalf("invalid builder: got %q, want tar (error surfaces on the tar path)", got)
	}
	t.Setenv("WENDY_CHUNK_EXPORT", "tar")
	if got := chunkExportPlan("docker"); got != "tar" {
		t.Fatalf("env override: got %q, want tar", got)
	}
}

func TestChunkLayoutDir(t *testing.T) {
	got := chunkLayoutDir("/cache", "Com.Wendylabs.Examples.App", "linux/arm64")
	want := filepath.Join("/cache", "wendy", "ocilayout", "com.wendylabs.examples.app-linux_arm64")
	if got != want {
		t.Fatalf("chunkLayoutDir = %q, want %q", got, want)
	}
}

// Independent OCI solves must overlap. This is the regression test for the
// process-wide build.lock queue: both fake buildx commands must start before
// either is allowed to finish. It also verifies they receive distinct OCI output
// layout destinations — with the cache now embedded inline in each app's own
// layout dir, distinct per-app output dirs are the safety condition for
// concurrent exporters (the old separate per-app local cache dirs are gone).
func TestBuildxOCIExportAllowsIndependentConcurrentSolves(t *testing.T) {
	isolateBuildLockDir(t)
	originalLock := buildLock
	buildLock = &processBuildLock{}
	defer func() { buildLock = originalLock }()

	originalEnsure := ensureOCIExportBuilderForBuild
	ensureOCIExportBuilderForBuild = func(context.Context, io.Writer) (string, error) {
		return "wendy-oci", nil
	}
	defer func() { ensureOCIExportBuilderForBuild = originalEnsure }()

	entered := make(chan string, 2)
	allowFinish := make(chan struct{})
	originalRun := runBuildxOCIExportCommand
	runBuildxOCIExportCommand = func(_ context.Context, _ string, args, _ []string, _, _ io.Writer) error {
		outputDest := ""
		for _, arg := range args {
			if strings.HasPrefix(arg, "type=oci,dest=") {
				outputDest = arg
			}
		}
		entered <- outputDest
		<-allowFinish
		return nil
	}
	defer func() { runBuildxOCIExportCommand = originalRun }()

	cwd := t.TempDir()
	if err := os.WriteFile(filepath.Join(cwd, "Dockerfile"), []byte("FROM scratch\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	results := make(chan error, 2)
	for i, app := range []string{"app-a", "app-b"} {
		dest := filepath.Join(t.TempDir(), fmt.Sprintf("layout-%d", i))
		go func(app, dest string) {
			results <- buildImageWithBuildxOCIExport(context.Background(), cwd, "Dockerfile", "linux/arm64", nil, dest, true, io.Discard, io.Discard)
		}(app, dest)
	}

	outputDests := map[string]bool{}
	for range 2 {
		select {
		case dest := <-entered:
			outputDests[dest] = true
		case <-time.After(2 * time.Second):
			close(allowFinish)
			t.Fatal("independent OCI solve was still queued behind the first")
		}
	}
	close(allowFinish)
	for range 2 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	if len(outputDests) != 2 {
		t.Fatalf("concurrent solves used %d output destinations, want 2: %v", len(outputDests), outputDests)
	}
}

func TestBuildxOCIExportArgs(t *testing.T) {
	t.Run("dir mode, no cache index, sorted build args", func(t *testing.T) {
		got := buildxOCIExportArgs("wendy-oci", "linux/arm64", "/proj/Dockerfile", "", "/dest/layout", true,
			map[string]string{"ZED": "2", "ALPHA": "1"})
		want := []string{
			"buildx", "build",
			"--builder", "wendy-oci",
			"--platform", "linux/arm64",
			"--progress", "plain",
			"-f", "/proj/Dockerfile",
			"--cache-to", "type=inline",
			"--build-arg", "ALPHA=1",
			"--build-arg", "ZED=2",
			"--output", "type=oci,dest=/dest/layout,compression=uncompressed,tar=false",
			".",
		}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("args mismatch:\n got  %q\n want %q", got, want)
		}
	})
	t.Run("tar mode with cache index, no dockerfile", func(t *testing.T) {
		got := buildxOCIExportArgs("wendy-oci", "linux/arm64", "", "/c/prev-layout", "/tmp/image.tar", false, nil)
		want := []string{
			"buildx", "build",
			"--builder", "wendy-oci",
			"--platform", "linux/arm64",
			"--progress", "plain",
			"--cache-from", "type=local,src=/c/prev-layout",
			"--cache-to", "type=inline",
			"--output", "type=oci,dest=/tmp/image.tar,compression=uncompressed",
			".",
		}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("args mismatch:\n got  %q\n want %q", got, want)
		}
	})
}

// readOCILayoutLayers must reference layer blobs by their byte range in the
// on-disk tar (never buffering them in RAM), and that range must be exact —
// the compressed bytes read back have to hash to the layer digest.
func TestReadOCILayoutLayersStreamsBlobByOffset(t *testing.T) {
	dir := t.TempDir()
	ociTar := filepath.Join(dir, "image.tar")

	// Compressible payload large enough to span many tar blocks.
	raw := bytes.Repeat([]byte("wendy-layer-payload-"), 5000)
	var gz bytes.Buffer
	gw := gzip.NewWriter(&gz)
	if _, err := gw.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	compressed := gz.Bytes()

	writeMinimalOCILayout(t, ociTar, compressed, "application/vnd.oci.image.layer.v1.tar+gzip", raw)

	layers, _, err := readOCILayoutLayers(ociTar, "linux/arm64")
	if err != nil {
		t.Fatal(err)
	}
	if len(layers) != 1 {
		t.Fatalf("want 1 layer, got %d", len(layers))
	}
	l := layers[0]

	if l.Blob != nil {
		t.Fatalf("layer unexpectedly holds %d compressed bytes in memory", len(l.Blob))
	}
	if l.TarPath == "" {
		t.Fatal("file-backed layer missing TarPath")
	}

	cr, err := l.compressedReader()
	if err != nil {
		t.Fatal(err)
	}
	defer cr.Close()
	gotCompressed, err := io.ReadAll(cr)
	if err != nil {
		t.Fatal(err)
	}
	if "sha256:"+sha256Hex(gotCompressed) != l.Digest {
		t.Fatal("compressed bytes read by recorded offset/size do not match the layer digest")
	}
	if !bytes.Equal(gotCompressed, compressed) {
		t.Fatalf("compressed bytes mismatch: got %d want %d", len(gotCompressed), len(compressed))
	}

	got, err := l.decompress()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, raw) {
		t.Fatal("decompressed bytes do not match original raw tar")
	}
}

func TestReadOCILayoutLayersUncompressed(t *testing.T) {
	dir := t.TempDir()
	ociTar := filepath.Join(dir, "image.tar")
	want := []byte("hello-tar-bytes")
	writeMinimalOCILayout(t, ociTar, want, "application/vnd.oci.image.layer.v1.tar", want)

	layers, imageConfig, err := readOCILayoutLayers(ociTar, "linux/arm64")
	if err != nil {
		t.Fatal(err)
	}
	if len(layers) != 1 {
		t.Fatalf("expected 1 layer, got %d", len(layers))
	}
	got, err := layers[0].decompress()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("layer bytes mismatch: got %q, want %q", got, want)
	}
	if layers[0].Digest != "sha256:"+sha256Hex(want) {
		t.Fatalf("layer digest mismatch: %s", layers[0].Digest)
	}
	// The image config blob must be returned and carry the runtime config
	// (Cmd/WorkingDir); otherwise the assembled container would have no command
	// and exit immediately.
	if len(imageConfig) == 0 {
		t.Fatal("expected non-empty image config blob")
	}
	if !bytes.Contains(imageConfig, []byte(`"app.py"`)) || !bytes.Contains(imageConfig, []byte(`/app`)) {
		t.Fatalf("image config missing Cmd/WorkingDir: %s", imageConfig)
	}
}

func TestReadOCILayoutLayersGzip(t *testing.T) {
	dir := t.TempDir()
	ociTar := filepath.Join(dir, "image.tar")
	want := []byte("hello-tar-bytes-gzip")

	// Gzip-compress the layer bytes to store in the OCI layout.
	var compressed bytes.Buffer
	gw := gzip.NewWriter(&compressed)
	if _, err := gw.Write(want); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	compressedBytes := compressed.Bytes()

	writeMinimalOCILayout(t, ociTar, compressedBytes, "application/vnd.oci.image.layer.v1.tar+gzip", want)

	layers, _, err := readOCILayoutLayers(ociTar, "linux/arm64")
	if err != nil {
		t.Fatal(err)
	}
	if len(layers) != 1 {
		t.Fatalf("expected 1 layer, got %d", len(layers))
	}
	got, err := layers[0].decompress()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("layer bytes mismatch after gzip decompression")
	}
	// The layer digest is the COMPRESSED blob digest (the stable cache key).
	if layers[0].Digest != "sha256:"+sha256Hex(compressedBytes) {
		t.Fatalf("layer digest mismatch (should be sha256 of compressed blob): %s", layers[0].Digest)
	}
}

// TestReadOCILayoutLayersPopulatesDiffID verifies the uncompressed diff ID is
// read from the image config's rootfs.diff_ids — without decompressing — and is
// distinct from the compressed layer digest for a gzip layer.
func TestReadOCILayoutLayersPopulatesDiffID(t *testing.T) {
	dir := t.TempDir()
	ociTar := filepath.Join(dir, "image.tar")
	want := []byte("diffid-probe-payload")

	var compressed bytes.Buffer
	gw := gzip.NewWriter(&compressed)
	if _, err := gw.Write(want); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	compressedBytes := compressed.Bytes()

	writeMinimalOCILayout(t, ociTar, compressedBytes, "application/vnd.oci.image.layer.v1.tar+gzip", want)

	layers, _, err := readOCILayoutLayers(ociTar, "linux/arm64")
	if err != nil {
		t.Fatal(err)
	}
	if len(layers) != 1 {
		t.Fatalf("expected 1 layer, got %d", len(layers))
	}
	wantDiffID := "sha256:" + sha256Hex(want)
	if layers[0].DiffID != wantDiffID {
		t.Fatalf("DiffID = %q, want %q (from config rootfs.diff_ids)", layers[0].DiffID, wantDiffID)
	}
	if layers[0].DiffID == layers[0].Digest {
		t.Fatal("DiffID must differ from the compressed Digest for a gzip layer")
	}
}

// imageManifestBytes builds a single-layer image manifest (+ its config and
// layer blobs) for the given architecture, returning the manifest JSON and the
// blob entries to embed in an OCI tar.
func imageManifestBytes(t *testing.T, arch string, layerRaw []byte) (manifest []byte, entries map[string][]byte) {
	t.Helper()
	diffID := "sha256:" + sha256Hex(layerRaw)
	layerDigest := "sha256:" + sha256Hex(layerRaw)
	configBytes := []byte(`{"architecture":"` + arch + `","os":"linux","config":{"Cmd":["python","app.py"],"WorkingDir":"/app"},"rootfs":{"type":"layers","diff_ids":["` + diffID + `"]}}`)
	configDigest := "sha256:" + sha256Hex(configBytes)
	m := map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.oci.image.manifest.v1+json",
		"config":        map[string]any{"mediaType": "application/vnd.oci.image.config.v1+json", "digest": configDigest, "size": len(configBytes)},
		"layers":        []map[string]any{{"mediaType": "application/vnd.oci.image.layer.v1.tar", "digest": layerDigest, "size": len(layerRaw)}},
	}
	manifest, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	entries = map[string][]byte{
		"blobs/sha256/" + sha256Hex(manifest):    manifest,
		"blobs/sha256/" + sha256Hex(configBytes): configBytes,
		"blobs/sha256/" + sha256Hex(layerRaw):    layerRaw,
	}
	return manifest, entries
}

// TestReadOCILayoutLayersNestedIndex covers Apple Container's `image save`
// shape: index.json → image-index → platform image-manifest.
func TestReadOCILayoutLayersNestedIndex(t *testing.T) {
	dir := t.TempDir()
	ociTar := filepath.Join(dir, "image.tar")
	layerRaw := []byte("nested-index-layer")

	manifestBytes, entries := imageManifestBytes(t, "arm64", layerRaw)
	manifestDigest := "sha256:" + sha256Hex(manifestBytes)

	// Inner image-index referencing the arm64 manifest by platform.
	innerIndex, err := json.Marshal(map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.oci.image.index.v1+json",
		"manifests": []map[string]any{
			{"mediaType": "application/vnd.oci.image.manifest.v1+json", "digest": manifestDigest, "size": len(manifestBytes), "platform": map[string]any{"architecture": "arm64", "os": "linux"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	innerIndexDigest := "sha256:" + sha256Hex(innerIndex)

	// Top-level index.json points at the inner image-index (no platform).
	indexBytes, err := json.Marshal(map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.oci.image.index.v1+json",
		"manifests": []map[string]any{
			{"mediaType": "application/vnd.oci.image.index.v1+json", "digest": innerIndexDigest, "size": len(innerIndex)},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	entries["oci-layout"] = []byte(`{"imageLayoutVersion":"1.0.0"}`)
	entries["index.json"] = indexBytes
	entries["blobs/sha256/"+innerIndexDigest[len("sha256:"):]] = innerIndex
	writeOCITar(t, ociTar, entries)

	layers, imageConfig, err := readOCILayoutLayers(ociTar, "linux/arm64")
	if err != nil {
		t.Fatalf("readOCILayoutLayers: %v", err)
	}
	if len(layers) != 1 {
		t.Fatalf("expected 1 layer, got %d", len(layers))
	}
	got, err := layers[0].decompress()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, layerRaw) {
		t.Fatalf("layer bytes mismatch: got %q want %q", got, layerRaw)
	}
	if len(imageConfig) == 0 {
		t.Fatal("expected non-empty image config blob")
	}
}

// TestReadOCILayoutLayersPlatformSelection ensures a multi-arch index resolves
// to the manifest matching the requested platform.
func TestReadOCILayoutLayersPlatformSelection(t *testing.T) {
	dir := t.TempDir()
	ociTar := filepath.Join(dir, "image.tar")
	amdLayer := []byte("amd64-layer")
	armLayer := []byte("arm64-layer")

	amdManifest, amdEntries := imageManifestBytes(t, "amd64", amdLayer)
	armManifest, armEntries := imageManifestBytes(t, "arm64", armLayer)

	indexBytes, err := json.Marshal(map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.oci.image.index.v1+json",
		"manifests": []map[string]any{
			{"mediaType": "application/vnd.oci.image.manifest.v1+json", "digest": "sha256:" + sha256Hex(amdManifest), "size": len(amdManifest), "platform": map[string]any{"architecture": "amd64", "os": "linux"}},
			{"mediaType": "application/vnd.oci.image.manifest.v1+json", "digest": "sha256:" + sha256Hex(armManifest), "size": len(armManifest), "platform": map[string]any{"architecture": "arm64", "os": "linux"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	entries := map[string][]byte{
		"oci-layout": []byte(`{"imageLayoutVersion":"1.0.0"}`),
		"index.json": indexBytes,
	}
	for k, v := range amdEntries {
		entries[k] = v
	}
	for k, v := range armEntries {
		entries[k] = v
	}
	writeOCITar(t, ociTar, entries)

	layers, _, err := readOCILayoutLayers(ociTar, "linux/arm64")
	if err != nil {
		t.Fatalf("readOCILayoutLayers: %v", err)
	}
	got, err := layers[0].decompress()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, armLayer) {
		t.Fatalf("selected wrong platform layer: got %q want %q", got, armLayer)
	}
}

func TestPickOCIDescriptorPrefersNewestPlatformMatch(t *testing.T) {
	arm := &struct {
		Architecture string `json:"architecture"`
		OS           string `json:"os"`
	}{Architecture: "arm64", OS: "linux"}
	descs := []ociDescriptor{
		{MediaType: "application/vnd.oci.image.manifest.v1+json", Digest: "sha256:aaaa", Platform: arm},
		{MediaType: "application/vnd.oci.image.manifest.v1+json", Digest: "sha256:bbbb", Platform: arm},
	}
	got := pickOCIDescriptor(descs, "linux", "arm64")
	if got == nil || got.Digest != "sha256:bbbb" {
		t.Fatalf("want newest (last) match sha256:bbbb, got %+v", got)
	}
}

func TestPickOCIDescriptorFallbackPrefersNewest(t *testing.T) {
	// No platform info at all: the fallback loop must also prefer the last
	// image-manifest entry (buildx appends newest last).
	descs := []ociDescriptor{
		{MediaType: "application/vnd.oci.image.manifest.v1+json", Digest: "sha256:aaaa"},
		{MediaType: "application/vnd.oci.image.manifest.v1+json", Digest: "sha256:bbbb"},
	}
	got := pickOCIDescriptor(descs, "linux", "arm64")
	if got == nil || got.Digest != "sha256:bbbb" {
		t.Fatalf("want newest (last) fallback sha256:bbbb, got %+v", got)
	}
}

// TestPickOCIDescriptorFallbackSkipsAttestationManifest ensures that with
// newest-last iteration, a trailing buildx attestation manifest
// (platform os=unknown/arch=unknown) does NOT win the fallback over a real
// image manifest when no exact platform match exists.
func TestPickOCIDescriptorFallbackSkipsAttestationManifest(t *testing.T) {
	arm := &struct {
		Architecture string `json:"architecture"`
		OS           string `json:"os"`
	}{Architecture: "arm64", OS: "linux"}
	unknown := &struct {
		Architecture string `json:"architecture"`
		OS           string `json:"os"`
	}{Architecture: "unknown", OS: "unknown"}
	descs := []ociDescriptor{
		{MediaType: "application/vnd.oci.image.manifest.v1+json", Digest: "sha256:image", Platform: arm},
		{MediaType: "application/vnd.oci.image.manifest.v1+json", Digest: "sha256:attestation", Platform: unknown},
	}
	// No exact match for linux/amd64: the fallback must skip the trailing
	// attestation manifest and return the real image manifest instead.
	got := pickOCIDescriptor(descs, "linux", "amd64")
	if got == nil || got.Digest != "sha256:image" {
		t.Fatalf("want fallback to skip attestation manifest and return sha256:image, got %+v", got)
	}
}

// TestPickOCIDescriptorFallbackNilPlatformStillWins ensures the fallback still
// picks a platform-NIL descriptor when it is the only candidate (nil platform
// must not be mistaken for the unknown/unknown attestation shape).
func TestPickOCIDescriptorFallbackNilPlatformStillWins(t *testing.T) {
	descs := []ociDescriptor{
		{MediaType: "application/vnd.oci.image.manifest.v1+json", Digest: "sha256:onlycandidate"},
	}
	got := pickOCIDescriptor(descs, "linux", "amd64")
	if got == nil || got.Digest != "sha256:onlycandidate" {
		t.Fatalf("want the only nil-platform candidate to win the fallback, got %+v", got)
	}
}

// writeBlob writes content under blobs/sha256/<sha256(content)> and returns
// the digest string.
func writeBlob(t *testing.T, dir string, content []byte) string {
	t.Helper()
	sum := sha256.Sum256(content)
	hexd := hex.EncodeToString(sum[:])
	p := filepath.Join(dir, "blobs", "sha256")
	if err := os.MkdirAll(p, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p, hexd), content, 0o600); err != nil {
		t.Fatal(err)
	}
	return "sha256:" + hexd
}

func TestPruneThenGCDropsSupersededBuild(t *testing.T) {
	dir := t.TempDir()
	// Old build: config + one layer + manifest referencing them.
	oldCfg := writeBlob(t, dir, []byte(`{"old":"config"}`))
	oldLayer := writeBlob(t, dir, []byte("old-layer"))
	oldManifest := writeBlob(t, dir, []byte(`{"config":{"digest":"`+oldCfg+`"},"layers":[{"digest":"`+oldLayer+`"}]}`))
	// New build: shares nothing with the old one.
	newCfg := writeBlob(t, dir, []byte(`{"new":"config"}`))
	newLayer := writeBlob(t, dir, []byte("new-layer"))
	newManifest := writeBlob(t, dir, []byte(`{"config":{"digest":"`+newCfg+`"},"layers":[{"digest":"`+newLayer+`"}]}`))

	entry := func(digest string) string {
		return `{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"` + digest + `","size":1,"platform":{"architecture":"arm64","os":"linux"}}`
	}
	writeLayoutIndex(t, dir, entry(oldManifest), entry(newManifest))

	if err := pruneOCILayoutDirIndex(dir, "linux/arm64"); err != nil {
		t.Fatal(err)
	}
	if err := gcOCILayoutDir(dir); err != nil {
		t.Fatal(err)
	}

	mustExist := func(digest string) {
		t.Helper()
		if _, err := os.Stat(filepath.Join(dir, "blobs", "sha256", strings.TrimPrefix(digest, "sha256:"))); err != nil {
			t.Fatalf("blob %s should survive GC: %v", digest, err)
		}
	}
	mustBeGone := func(digest string) {
		t.Helper()
		if _, err := os.Stat(filepath.Join(dir, "blobs", "sha256", strings.TrimPrefix(digest, "sha256:"))); !os.IsNotExist(err) {
			t.Fatalf("blob %s should be GC'd, stat err=%v", digest, err)
		}
	}
	mustExist(newManifest)
	mustExist(newCfg)
	mustExist(newLayer)
	mustBeGone(oldManifest)
	mustBeGone(oldCfg)
	mustBeGone(oldLayer)
}
