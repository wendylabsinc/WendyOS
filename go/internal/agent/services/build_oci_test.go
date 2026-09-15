package services

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"os"
	"strings"
	"testing"
)

// testImageLayer is one layer of a synthetic image. content stands in for the
// uncompressed layer tar; nothing under test parses it.
type testImageLayer struct {
	content    []byte
	compressed bool // gzip in the export when true, stored raw otherwise
}

// testImageLayers is the standard fixture: two layers of a few hundred KiB of
// incompressible bytes, so each content-chunks into several chunks, one stored
// gzip and one raw. Deterministic, so the buildctl helper process and the test
// that inspects its output agree on every hash.
func testImageLayers() []testImageLayer {
	return []testImageLayer{
		{content: pseudoRandomBytes(1, 300<<10), compressed: true},
		{content: pseudoRandomBytes(2, 200<<10), compressed: false},
	}
}

func pseudoRandomBytes(seed int64, n int) []byte {
	b := make([]byte, n)
	rand.New(rand.NewSource(seed)).Read(b) //nolint:gosec // test fixture
	return b
}

type testImageOptions struct {
	OS, Arch string
	// DropLastDiffID writes a config whose rootfs.diff_ids is one short, the
	// inconsistency readExportedImage must refuse.
	DropLastDiffID bool
	// WithAttestation adds an unknown/unknown manifest to the index, as buildkit
	// does for provenance attestations.
	WithAttestation bool
	// NestIndex points index.json at a nested image index holding the manifest,
	// as a multi-platform export does.
	NestIndex bool
}

type testImage struct {
	diffIDs        []string
	config         []byte
	manifestDigest string
	blobs          map[string][]byte // digest -> bytes
}

func sha256Digest(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func gzipBytes(t testing.TB, b []byte) []byte {
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	if _, err := gw.Write(b); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// writeTestOCITar writes an OCI layout tar of the given layers to path, the
// shape `buildctl --output type=oci,dest=path` produces.
func writeTestOCITar(t testing.TB, path string, layers []testImageLayer, opt testImageOptions) testImage {
	t.Helper()
	ti := testImage{blobs: map[string][]byte{}}
	type desc struct {
		MediaType string         `json:"mediaType"`
		Digest    string         `json:"digest"`
		Size      int64          `json:"size"`
		Platform  map[string]any `json:"platform,omitempty"`
	}
	var layerDescs []desc
	for _, l := range layers {
		ti.diffIDs = append(ti.diffIDs, sha256Digest(l.content))
		blob, mt := l.content, "application/vnd.oci.image.layer.v1.tar"
		if l.compressed {
			blob, mt = gzipBytes(t, l.content), "application/vnd.oci.image.layer.v1.tar+gzip"
		}
		d := sha256Digest(blob)
		ti.blobs[d] = blob
		layerDescs = append(layerDescs, desc{MediaType: mt, Digest: d, Size: int64(len(blob))})
	}
	configDiffIDs := ti.diffIDs
	if opt.DropLastDiffID {
		configDiffIDs = configDiffIDs[:len(configDiffIDs)-1]
	}
	config, err := json.Marshal(map[string]any{
		"architecture": opt.Arch,
		"os":           opt.OS,
		"config":       map[string]any{"Cmd": []string{"/bin/app"}, "Env": []string{"A=1"}},
		"rootfs":       map[string]any{"type": "layers", "diff_ids": configDiffIDs},
	})
	if err != nil {
		t.Fatal(err)
	}
	ti.config = config
	ti.blobs[sha256Digest(config)] = config

	manifest, err := json.Marshal(map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.oci.image.manifest.v1+json",
		"config":        desc{MediaType: "application/vnd.oci.image.config.v1+json", Digest: sha256Digest(config), Size: int64(len(config))},
		"layers":        layerDescs,
	})
	if err != nil {
		t.Fatal(err)
	}
	ti.manifestDigest = sha256Digest(manifest)
	ti.blobs[ti.manifestDigest] = manifest

	manifests := []desc{{
		MediaType: "application/vnd.oci.image.manifest.v1+json",
		Digest:    ti.manifestDigest,
		Size:      int64(len(manifest)),
		Platform:  map[string]any{"os": opt.OS, "architecture": opt.Arch},
	}}
	if opt.WithAttestation {
		att := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","layers":[]}`)
		ti.blobs[sha256Digest(att)] = att
		manifests = append(manifests, desc{
			MediaType: "application/vnd.oci.image.manifest.v1+json",
			Digest:    sha256Digest(att),
			Size:      int64(len(att)),
			Platform:  map[string]any{"os": "unknown", "architecture": "unknown"},
		})
	}
	if opt.NestIndex {
		nested, err := json.Marshal(map[string]any{"schemaVersion": 2, "manifests": manifests})
		if err != nil {
			t.Fatal(err)
		}
		ti.blobs[sha256Digest(nested)] = nested
		manifests = []desc{{
			MediaType: "application/vnd.oci.image.index.v1+json",
			Digest:    sha256Digest(nested),
			Size:      int64(len(nested)),
		}}
	}
	index, err := json.Marshal(map[string]any{"schemaVersion": 2, "manifests": manifests})
	if err != nil {
		t.Fatal(err)
	}

	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	tw := tar.NewWriter(f)
	write := func(name string, data []byte) {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(data))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	write("oci-layout", []byte(`{"imageLayoutVersion":"1.0.0"}`))
	write("index.json", index)
	for d, b := range ti.blobs {
		write("blobs/sha256/"+strings.TrimPrefix(d, "sha256:"), b)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return ti
}

func TestReadExportedImage_ReadsLayersConfigAndManifestDigest(t *testing.T) {
	path := t.TempDir() + "/image.tar"
	layers := testImageLayers()
	ti := writeTestOCITar(t, path, layers, testImageOptions{OS: "linux", Arch: "arm64"})

	img, err := readExportedImage(path, "linux/arm64")
	if err != nil {
		t.Fatalf("readExportedImage: %v", err)
	}
	if img.manifestDigest != ti.manifestDigest {
		t.Fatalf("manifest digest = %s, want %s", img.manifestDigest, ti.manifestDigest)
	}
	if string(img.config) != string(ti.config) {
		t.Fatal("config blob must be returned byte for byte")
	}
	if len(img.layers) != len(layers) {
		t.Fatalf("got %d layers, want %d", len(img.layers), len(layers))
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for i, l := range img.layers {
		if l.diffID != ti.diffIDs[i] {
			t.Fatalf("layer %d diff ID = %s, want %s", i, l.diffID, ti.diffIDs[i])
		}
		// The recorded byte range must be the blob itself: delivery streams
		// layers straight out of the tar by it.
		blob, err := io.ReadAll(io.NewSectionReader(f, l.offset, l.size))
		if err != nil {
			t.Fatal(err)
		}
		if got := sha256Digest(blob); got != l.digest {
			t.Fatalf("layer %d: bytes at offset %d hash to %s, descriptor says %s", i, l.offset, got, l.digest)
		}
		raw, release, err := layerDecompressor(bytes.NewReader(blob), l.mediaType)
		if err != nil {
			t.Fatal(err)
		}
		content, err := io.ReadAll(raw)
		release()
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(content, layers[i].content) {
			t.Fatalf("layer %d does not decompress to the fixture content", i)
		}
	}
}

// Chunked delivery addresses layers by diff ID; an export whose config does not
// name one per layer cannot be delivered that way and must say so, rather than
// pair layers with the wrong identities.
func TestReadExportedImage_RejectsConfigThatDoesNotMatchLayers(t *testing.T) {
	path := t.TempDir() + "/image.tar"
	writeTestOCITar(t, path, testImageLayers(), testImageOptions{OS: "linux", Arch: "arm64", DropLastDiffID: true})
	if _, err := readExportedImage(path, "linux/arm64"); err == nil || !strings.Contains(err.Error(), "diff IDs") {
		t.Fatalf("got %v, want a diff ID count mismatch", err)
	}
}

func TestReadExportedImage_SkipsAttestationManifests(t *testing.T) {
	path := t.TempDir() + "/image.tar"
	ti := writeTestOCITar(t, path, testImageLayers(), testImageOptions{OS: "linux", Arch: "arm64", WithAttestation: true})
	img, err := readExportedImage(path, "linux/arm64")
	if err != nil {
		t.Fatalf("readExportedImage: %v", err)
	}
	if img.manifestDigest != ti.manifestDigest {
		t.Fatalf("selected %s, want the platform manifest %s, not the attestation", img.manifestDigest, ti.manifestDigest)
	}
}

func TestReadExportedImage_FollowsNestedIndex(t *testing.T) {
	path := t.TempDir() + "/image.tar"
	ti := writeTestOCITar(t, path, testImageLayers(), testImageOptions{OS: "linux", Arch: "arm64", NestIndex: true})
	img, err := readExportedImage(path, "linux/arm64")
	if err != nil {
		t.Fatalf("readExportedImage: %v", err)
	}
	if img.manifestDigest != ti.manifestDigest {
		t.Fatalf("selected %s, want %s", img.manifestDigest, ti.manifestDigest)
	}
}

// The wrong manifest is an image built for another machine delivered as if it
// were this one, so a platform with no manifest is an error, not a best guess.
func TestReadExportedImage_RejectsMissingPlatform(t *testing.T) {
	path := t.TempDir() + "/image.tar"
	writeTestOCITar(t, path, testImageLayers(), testImageOptions{OS: "linux", Arch: "arm64"})
	if _, err := readExportedImage(path, "linux/amd64"); err == nil || !strings.Contains(err.Error(), "no manifest for linux/amd64") {
		t.Fatalf("got %v, want no manifest for the requested platform", err)
	}
}

func TestSelectImageManifest_RefusesAmbiguity(t *testing.T) {
	descs := []ociDescriptor{
		{MediaType: "application/vnd.oci.image.manifest.v1+json", Digest: "sha256:aa"},
		{MediaType: "application/vnd.oci.image.manifest.v1+json", Digest: "sha256:bb"},
	}
	_, err := selectImageManifest(descs, nil, "linux", "arm64", 0)
	if err == nil || !strings.Contains(err.Error(), "several manifests") {
		t.Fatalf("got %v, want a refusal to guess between unlabelled manifests", err)
	}
}

func TestSelectImageManifest_BoundsNesting(t *testing.T) {
	// An index that points at itself forever.
	self := `{"manifests":[{"mediaType":"application/vnd.oci.image.index.v1+json","digest":"sha256:loop"}]}`
	getBlob := func(string) ([]byte, error) { return []byte(self), nil }
	_, err := selectImageManifest([]ociDescriptor{{MediaType: "application/vnd.oci.image.index.v1+json", Digest: "sha256:loop"}}, getBlob, "linux", "arm64", 0)
	if err == nil || !strings.Contains(err.Error(), "nests indexes") {
		t.Fatalf("got %v, want the nesting bound to end the recursion", err)
	}
}

func TestLayerCompressionOf(t *testing.T) {
	cases := map[string]struct {
		want layerCompression
		ok   bool
	}{
		"application/vnd.oci.image.layer.v1.tar":              {layerUncompressed, true},
		"application/vnd.oci.image.layer.v1.tar+gzip":         {layerGzip, true},
		"application/vnd.docker.image.rootfs.diff.tar.gzip":   {layerGzip, true},
		"application/vnd.oci.image.layer.v1.tar+zstd":         {layerZstd, true},
		"application/vnd.oci.image.layer.nondistributable.v1": {0, false},
		"": {0, false},
	}
	for mt, tc := range cases {
		got, ok := layerCompressionOf(mt)
		if ok != tc.ok || (ok && got != tc.want) {
			t.Errorf("%q: got (%v, %v), want (%v, %v)", mt, got, ok, tc.want, tc.ok)
		}
	}
}

// Sanity check on the fixture itself: a helper process and a test must derive
// the same image from testImageLayers, or the end-to-end tests compare hashes
// of two different images.
func TestTestImageLayers_IsDeterministic(t *testing.T) {
	a := writeTestOCITar(t, t.TempDir()+"/a.tar", testImageLayers(), testImageOptions{OS: "linux", Arch: "arm64"})
	b := writeTestOCITar(t, t.TempDir()+"/b.tar", testImageLayers(), testImageOptions{OS: "linux", Arch: "arm64"})
	if a.manifestDigest != b.manifestDigest {
		t.Fatalf("fixture differs between calls: %s vs %s", a.manifestDigest, b.manifestDigest)
	}
	if fmt.Sprint(a.diffIDs) != fmt.Sprint(b.diffIDs) {
		t.Fatal("fixture diff IDs differ between calls")
	}
}
