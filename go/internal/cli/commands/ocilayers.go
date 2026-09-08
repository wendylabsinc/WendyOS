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
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/klauspost/compress/zstd"
	"github.com/wendylabsinc/wendy/go/internal/shared/chunk"
	"github.com/wendylabsinc/wendy/go/internal/shared/flock"
)

// localLayer addresses a single image layer's COMPRESSED blob plus the metadata
// needed to decompress it. The compressed bytes are NOT held in memory for the
// real deploy path: TarPath/Offset/Size point at the layer's bytes inside the
// on-disk OCI tar, streamed on demand (see compressedReader). Decompression is
// deferred so callers that resolve a layer from the manifest cache never pay to
// read or decompress it. Blob is an in-memory fallback used by tests.
type localLayer struct {
	Digest    string // compressed OCI layer blob digest ("sha256:<hex>") — stable cache key
	DiffID    string // uncompressed layer digest from the image config's rootfs.diff_ids; "" when unavailable
	MediaType string // OCI/Docker layer media type (drives decompression)

	Blob []byte // compressed bytes, when held in memory (tests / small blobs)

	TarPath string // path to the OCI tar holding the compressed blob, when file-backed
	Offset  int64  // byte offset of the compressed blob within TarPath
	Size    int64  // compressed blob length
}

// compressedReader opens the layer's compressed bytes as a stream. The caller
// must Close it. File-backed layers reopen the OCI tar (one fd per call, so
// concurrent layers don't share state); in-memory layers wrap Blob.
func (l localLayer) compressedReader() (io.ReadCloser, error) {
	if l.TarPath != "" {
		f, err := os.Open(l.TarPath)
		if err != nil {
			return nil, fmt.Errorf("open OCI tar: %w", err)
		}
		return &sectionReadCloser{Reader: io.NewSectionReader(f, l.Offset, l.Size), c: f}, nil
	}
	return io.NopCloser(bytes.NewReader(l.Blob)), nil
}

// sectionReadCloser couples a SectionReader with the file it reads from so the
// fd is released on Close.
type sectionReadCloser struct {
	io.Reader
	c io.Closer
}

func (s *sectionReadCloser) Close() error { return s.c.Close() }

// decompress returns the raw (uncompressed) tar bytes for the layer in memory.
// Prefer decompressLayerToTemp for large layers so the tar never sits in RAM.
func (l localLayer) decompress() ([]byte, error) {
	cr, err := l.compressedReader()
	if err != nil {
		return nil, err
	}
	defer cr.Close()
	r, cleanup, err := layerTarReader(cr, l.MediaType)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	out, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("decompress layer: %w", err)
	}
	return out, nil
}

// totalCompressedLayerBytes sums the compressed blob sizes of the image's
// layers (Size for file-backed layers, len(Blob) for in-memory ones).
func totalCompressedLayerBytes(layers []localLayer) int64 {
	var total int64
	for _, l := range layers {
		if l.TarPath != "" {
			total += l.Size
		} else {
			total += int64(len(l.Blob))
		}
	}
	return total
}

// ociDescriptor is a descriptor entry as it appears in an OCI index.json or a
// nested image-index manifest list.
type ociDescriptor struct {
	MediaType string `json:"mediaType"`
	Digest    string `json:"digest"`
	Platform  *struct {
		Architecture string `json:"architecture"`
		OS           string `json:"os"`
	} `json:"platform"`
}

func isOCIImageIndexMediaType(mt string) bool {
	return mt == "application/vnd.oci.image.index.v1+json" ||
		mt == "application/vnd.docker.distribution.manifest.list.v2+json"
}

func isOCIImageManifestMediaType(mt string) bool {
	// "" is treated as a leaf image manifest to preserve buildx layouts whose
	// index.json entries omit the mediaType.
	return mt == "" ||
		mt == "application/vnd.oci.image.manifest.v1+json" ||
		mt == "application/vnd.docker.distribution.manifest.v2+json"
}

// parseOCIPlatform splits a "os/arch[/variant]" platform string into its os and
// architecture components. Empty parts are returned when absent.
func parseOCIPlatform(platform string) (os, arch string) {
	parts := strings.Split(platform, "/")
	if len(parts) > 0 {
		os = parts[0]
	}
	if len(parts) > 1 {
		arch = parts[1]
	}
	return os, arch
}

// resolveOCIImageManifest follows OCI descriptors from an index to the concrete
// image manifest blob for the target platform, descending through nested
// image-indexes (Apple Container's `image save` produces one or two levels).
// It returns the raw image-manifest JSON.
func resolveOCIImageManifest(descs []ociDescriptor, getBlob func(hex string) ([]byte, error), wantOS, wantArch string, depth int) ([]byte, error) {
	if depth > 4 {
		return nil, fmt.Errorf("OCI index nesting too deep")
	}
	chosen := pickOCIDescriptor(descs, wantOS, wantArch)
	if chosen == nil {
		return nil, fmt.Errorf("no image manifest found in OCI layout")
	}
	hex, err := digestToHex(chosen.Digest)
	if err != nil {
		return nil, fmt.Errorf("invalid manifest digest %q: %w", chosen.Digest, err)
	}
	blob, err := getBlob(hex)
	if err != nil {
		return nil, fmt.Errorf("manifest blob %s: %w", chosen.Digest, err)
	}
	if isOCIImageIndexMediaType(chosen.MediaType) {
		var nested struct {
			Manifests []ociDescriptor `json:"manifests"`
		}
		if err := json.Unmarshal(blob, &nested); err != nil {
			return nil, fmt.Errorf("parsing nested image index: %w", err)
		}
		if len(nested.Manifests) == 0 {
			return nil, fmt.Errorf("nested image index has no manifests")
		}
		return resolveOCIImageManifest(nested.Manifests, getBlob, wantOS, wantArch, depth+1)
	}
	return blob, nil
}

// pickOCIDescriptor chooses the best descriptor for the target platform:
// the NEWEST exact os/arch match if present, otherwise the newest image
// manifest or image index (skipping attestation/unknown entries).
//
// Newest means LAST in slice order: buildx's tar=false dir export APPENDS
// each build's manifest to index.json, so several entries can match the
// platform and the current build is always the final one. Resolving the
// first match shipped the layout dir's first-ever build forever (stale
// deps bug, on-device pass 2026-08-08).
func pickOCIDescriptor(descs []ociDescriptor, wantOS, wantArch string) *ociDescriptor {
	for i := len(descs) - 1; i >= 0; i-- {
		d := &descs[i]
		if d.Platform != nil && d.Platform.OS == wantOS && d.Platform.Architecture == wantArch {
			return d
		}
	}
	for i := len(descs) - 1; i >= 0; i-- {
		d := &descs[i]
		if d.Platform != nil && d.Platform.OS == "unknown" && d.Platform.Architecture == "unknown" {
			// buildx attestation manifest — never a deployable image.
			continue
		}
		if isOCIImageManifestMediaType(d.MediaType) || isOCIImageIndexMediaType(d.MediaType) {
			return d
		}
	}
	return nil
}

// readOCILayoutLayers opens an OCI-layout tar at ociTarPath, walks the
// index.json → manifest → layer descriptors, decompresses each layer to its
// raw tar (by media type), and returns layers in manifest order with
//
//	DiffID = "sha256:" + hex(sha256(rawTar))
//
// It also returns the raw OCI image config blob (the JSON carrying
// Cmd/Entrypoint/Env/WorkingDir/User) so the agent can preserve the original
// runtime config when assembling the image from chunks.
func readOCILayoutLayers(ociTarPath, platform string) ([]localLayer, []byte, error) {
	f, err := os.Open(ociTarPath)
	if err != nil {
		return nil, nil, fmt.Errorf("open OCI tar: %w", err)
	}
	defer f.Close()

	// First pass: index each blob's byte range within the tar WITHOUT reading the
	// (potentially multi-GiB) layer bytes into memory. Only index.json is held;
	// manifest/config blobs are read back on demand below, and layer blobs are
	// streamed from the tar later via localLayer.compressedReader.
	blobOffsets := map[string]blobLoc{} // hex digest → byte range in the tar
	var indexJSON []byte

	cr := &offsetCountingReader{r: f}
	tr := tar.NewReader(cr)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, nil, fmt.Errorf("reading OCI tar: %w", err)
		}
		// After Next() the counting reader sits exactly at this entry's data
		// (tar headers/padding are 512-byte blocks read straight from f).
		dataOff := cr.n
		switch {
		case hdr.Name == "index.json":
			indexJSON, err = io.ReadAll(tr)
			if err != nil {
				return nil, nil, fmt.Errorf("reading index.json: %w", err)
			}
		case strings.HasPrefix(hdr.Name, "blobs/sha256/"):
			blobHex := strings.TrimPrefix(hdr.Name, "blobs/sha256/")
			blobOffsets[blobHex] = blobLoc{off: dataOff, size: hdr.Size}
			if _, err := io.Copy(io.Discard, tr); err != nil {
				return nil, nil, fmt.Errorf("scanning blob %q: %w", hdr.Name, err)
			}
		}
	}

	if indexJSON == nil {
		return nil, nil, fmt.Errorf("OCI tar missing index.json")
	}

	// getBlob reads a (small) blob — manifest, image-index, or config — back from
	// the tar by its recorded byte range. Layer blobs are NOT fetched this way;
	// they stay on disk and are streamed during the push.
	getBlob := func(hex string) ([]byte, error) {
		loc, ok := blobOffsets[hex]
		if !ok {
			return nil, fmt.Errorf("blob sha256:%s not found in OCI tar", hex)
		}
		return io.ReadAll(io.NewSectionReader(f, loc.off, loc.size))
	}

	img, err := ociImageFromIndex(indexJSON, getBlob, platform)
	if err != nil {
		return nil, nil, err
	}

	layers := make([]localLayer, 0, len(img.Layers))
	for i, desc := range img.Layers {
		layerHex, err := digestToHex(desc.Digest)
		if err != nil {
			return nil, nil, fmt.Errorf("layer %d: invalid digest %q: %w", i, desc.Digest, err)
		}
		loc, ok := blobOffsets[layerHex]
		if !ok {
			return nil, nil, fmt.Errorf("layer %d blob %s not found in OCI tar", i, desc.Digest)
		}
		// Reference the compressed blob by its range in the on-disk tar; it is
		// streamed (never fully buffered) during the push, and decompression is
		// deferred so cache-resolved layers are never read at all.
		layers = append(layers, localLayer{
			Digest:    desc.Digest,
			DiffID:    desc.DiffID,
			MediaType: desc.MediaType,
			TarPath:   ociTarPath,
			Offset:    loc.off,
			Size:      loc.size,
		})
	}
	return layers, img.ImageConfig, nil
}

// ociResolvedImage is the platform-resolved content of an OCI layout: the raw
// image config plus the manifest's layer descriptors, labelled with diff IDs
// when the config's rootfs.diff_ids align 1:1 with the layer list.
type ociResolvedImage struct {
	ImageConfig []byte
	Layers      []ociResolvedLayer
}

// ociResolvedLayer is one manifest layer descriptor, independent of where its
// compressed bytes live (tar byte range or layout-dir blob file).
type ociResolvedLayer struct {
	Digest    string // "sha256:<hex>" compressed blob digest
	DiffID    string // "" when the config's diff_ids don't align with the layer list
	MediaType string
	Size      int64 // from the manifest descriptor; 0 when absent
}

// ociImageFromIndex resolves an OCI layout's index.json to the image manifest
// for the target platform and returns its layer descriptors plus the raw image
// config blob. getBlob fetches small blobs (manifests, indexes, the config) by
// hex digest; layer blobs are never fetched here.
func ociImageFromIndex(indexJSON []byte, getBlob func(hex string) ([]byte, error), platform string) (*ociResolvedImage, error) {
	// Parse index.json and resolve to a concrete image manifest. buildx emits
	// index.json → image-manifest directly, while Apple Container's `image save`
	// wraps the image in one (or two) nested image-indexes; both are handled by
	// following index descriptors to the manifest matching the target platform.
	var index struct {
		Manifests []ociDescriptor `json:"manifests"`
	}
	if err := json.Unmarshal(indexJSON, &index); err != nil {
		return nil, fmt.Errorf("parsing index.json: %w", err)
	}
	if len(index.Manifests) == 0 {
		return nil, fmt.Errorf("index.json has no manifests")
	}

	wantOS, wantArch := parseOCIPlatform(platform)
	manifestData, err := resolveOCIImageManifest(index.Manifests, getBlob, wantOS, wantArch, 0)
	if err != nil {
		return nil, err
	}

	// Parse the manifest to get the config descriptor and layer descriptors.
	var manifest struct {
		Config struct {
			Digest string `json:"digest"`
		} `json:"config"`
		Layers []struct {
			MediaType string `json:"mediaType"`
			Digest    string `json:"digest"`
			Size      int64  `json:"size"`
		} `json:"layers"`
	}
	if err := json.Unmarshal(manifestData, &manifest); err != nil {
		return nil, fmt.Errorf("parsing manifest: %w", err)
	}

	// Fetch the image config blob so the runtime config (Cmd/Entrypoint/Env/
	// WorkingDir/User) survives reassembly on the device. It also carries
	// rootfs.diff_ids — the uncompressed digest of each layer, in layer order —
	// which lets the push pre-check layer presence on the device WITHOUT
	// decompressing anything.
	var imageConfig []byte
	var diffIDs []string
	if manifest.Config.Digest != "" {
		configHex, err := digestToHex(manifest.Config.Digest)
		if err != nil {
			return nil, fmt.Errorf("invalid config digest %q: %w", manifest.Config.Digest, err)
		}
		imageConfig, err = getBlob(configHex)
		if err != nil {
			return nil, fmt.Errorf("config blob %s: %w", manifest.Config.Digest, err)
		}
		var cfg struct {
			RootFS struct {
				DiffIDs []string `json:"diff_ids"`
			} `json:"rootfs"`
		}
		// A malformed/absent rootfs just leaves diffIDs empty: the push falls back
		// to deriving each diff ID by decompressing, so this is a pure optimization.
		if err := json.Unmarshal(imageConfig, &cfg); err == nil {
			diffIDs = cfg.RootFS.DiffIDs
		}
	}

	// diff_ids align 1:1 with manifest layers (both bottom-to-top, empty layers
	// excluded) per the OCI image-config spec. Only trust them to label layers
	// when the counts match; otherwise leave DiffID empty and let the push derive
	// it the slow way rather than risk mislabelling a layer.
	diffIDsAligned := len(diffIDs) == len(manifest.Layers)

	img := &ociResolvedImage{ImageConfig: imageConfig}
	for i, desc := range manifest.Layers {
		var diffID string
		if diffIDsAligned {
			diffID = diffIDs[i]
		}
		img.Layers = append(img.Layers, ociResolvedLayer{
			Digest:    desc.Digest,
			DiffID:    diffID,
			MediaType: desc.MediaType,
			Size:      desc.Size,
		})
	}
	return img, nil
}

// readOCILayoutDirLayers is readOCILayoutLayers' sibling for an OCI layout
// DIRECTORY (buildx `--output type=oci,tar=false`): dir holds index.json plus
// blobs/sha256/<hex> files. Layers come back file-backed against the blob
// files themselves (offset 0, whole file). Each layer blob's on-disk size is
// validated against the manifest descriptor so a partially-written blob (e.g.
// a killed export) fails loudly instead of pushing garbage — callers respond
// by wiping the directory and rebuilding.
func readOCILayoutDirLayers(dir, platform string) ([]localLayer, []byte, error) {
	indexJSON, err := os.ReadFile(filepath.Join(dir, "index.json"))
	if err != nil {
		return nil, nil, fmt.Errorf("read OCI layout index: %w", err)
	}
	blobPath := func(hex string) string {
		return filepath.Join(dir, "blobs", "sha256", hex)
	}
	getBlob := func(hex string) ([]byte, error) {
		return os.ReadFile(blobPath(hex))
	}

	img, err := ociImageFromIndex(indexJSON, getBlob, platform)
	if err != nil {
		return nil, nil, err
	}

	layers := make([]localLayer, 0, len(img.Layers))
	for i, desc := range img.Layers {
		layerHex, err := digestToHex(desc.Digest)
		if err != nil {
			return nil, nil, fmt.Errorf("layer %d: invalid digest %q: %w", i, desc.Digest, err)
		}
		fi, err := os.Stat(blobPath(layerHex))
		if err != nil {
			return nil, nil, fmt.Errorf("layer %d blob %s not found in OCI layout dir: %w", i, desc.Digest, err)
		}
		// Descriptor sizes are REQUIRED by the OCI spec; refusing a size-less
		// descriptor keeps the partial-write check from being silently skipped.
		// SECURITY: content is intentionally NOT re-hashed here — the cache dir
		// is 0700 (same-user trust boundary), and re-hashing every layer per
		// iteration would reintroduce the O(image size) per-build cost this
		// layout-dir path exists to remove.
		if desc.Size <= 0 {
			return nil, nil, fmt.Errorf("layer blob sha256:%s has no size in its manifest descriptor", layerHex)
		}
		if fi.Size() != desc.Size {
			return nil, nil, fmt.Errorf("layer blob sha256:%s is %d bytes on disk but the manifest says %d (partial write?)", layerHex, fi.Size(), desc.Size)
		}
		layers = append(layers, localLayer{
			Digest:    desc.Digest,
			DiffID:    desc.DiffID,
			MediaType: desc.MediaType,
			TarPath:   blobPath(layerHex),
			Offset:    0,
			Size:      fi.Size(),
		})
	}
	return layers, img.ImageConfig, nil
}

// chunkExportModeEnv forces the chunk-diff deploy's legacy temp-tar export
// ("tar") instead of the persistent layout directory. Escape hatch only.
const chunkExportModeEnv = "WENDY_CHUNK_EXPORT"

// resolveOCIExportBuilder resolves the builder an OCI-layout export will use,
// applying the on-device BuildKit default that an unset --builder gets when no
// docker is present. Returns normalizeImageBuilder's error for unknown builders.
func resolveOCIExportBuilder(builder string) (string, error) {
	if !imageBuilderWasExplicit(builder) && shouldUseBuildkitOnDevice() {
		builder = imageBuilderBuildkit
	}
	return normalizeImageBuilder(builder)
}

// chunkExportPlan decides how the chunk-diff deploy exports the built image:
// "dir" — persistent OCI layout directory (buildx tar=false, blob-deduped) —
// for the docker backend, "tar" for backends that can only emit a tar
// (apple-container, on-device buildctl), for the WENDY_CHUNK_EXPORT=tar
// escape hatch, and for unknown builders (whose normalize error then surfaces
// on the tar path exactly as before).
func chunkExportPlan(builder string) string {
	if os.Getenv(chunkExportModeEnv) == "tar" {
		return "tar"
	}
	normalized, err := resolveOCIExportBuilder(builder)
	if err != nil || normalized != imageBuilderDocker {
		return "tar"
	}
	return "dir"
}

// gcOCILayoutDir deletes blobs in dir that are no longer reachable from
// index.json. buildx's tar=false export only ever ADDS blobs, so superseded
// manifests/configs/layers accumulate until pruned. Reachability keeps every
// index entry (nested indexes and attestation manifests included), each
// manifest's config, and each manifest's layers. A missing dir or index is a
// no-op; unreadable blobs during the walk are kept (best-effort, never breaks
// a deployable layout).
func gcOCILayoutDir(dir string) error {
	indexJSON, err := os.ReadFile(filepath.Join(dir, "index.json"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read OCI layout index for GC: %w", err)
	}
	var index struct {
		Manifests []ociDescriptor `json:"manifests"`
	}
	if err := json.Unmarshal(indexJSON, &index); err != nil {
		return fmt.Errorf("parsing index.json for GC: %w", err)
	}

	referenced := map[string]bool{}
	var walk func(descs []ociDescriptor, depth int)
	walk = func(descs []ociDescriptor, depth int) {
		if depth > 6 {
			return
		}
		for _, d := range descs {
			hexDigest, err := digestToHex(d.Digest)
			if err != nil {
				continue
			}
			referenced[hexDigest] = true
			blob, err := os.ReadFile(filepath.Join(dir, "blobs", "sha256", hexDigest))
			if err != nil {
				continue
			}
			if isOCIImageIndexMediaType(d.MediaType) {
				var nested struct {
					Manifests []ociDescriptor `json:"manifests"`
				}
				if err := json.Unmarshal(blob, &nested); err == nil {
					walk(nested.Manifests, depth+1)
				}
				continue
			}
			// Image manifests (attestation manifests share the media type): keep
			// the config and every layer blob.
			var manifest struct {
				Config struct {
					Digest string `json:"digest"`
				} `json:"config"`
				Layers []struct {
					Digest string `json:"digest"`
				} `json:"layers"`
			}
			if err := json.Unmarshal(blob, &manifest); err != nil {
				continue
			}
			if hexDigest, err := digestToHex(manifest.Config.Digest); err == nil {
				referenced[hexDigest] = true
			}
			for _, l := range manifest.Layers {
				if hexDigest, err := digestToHex(l.Digest); err == nil {
					referenced[hexDigest] = true
				}
			}
		}
	}
	walk(index.Manifests, 0)

	blobsDir := filepath.Join(dir, "blobs", "sha256")
	dirEntries, err := os.ReadDir(blobsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("listing blobs for GC: %w", err)
	}
	for _, e := range dirEntries {
		if e.IsDir() || referenced[e.Name()] {
			continue
		}
		if err := os.Remove(filepath.Join(blobsDir, e.Name())); err != nil {
			return fmt.Errorf("pruning blob %s: %w", e.Name(), err)
		}
	}
	return nil
}

// pruneOCILayoutDirIndex rewrites dir's index.json to reference only the
// newest image manifest matching platform. buildx's tar=false export
// appends manifests, so without pruning the index accumulates one entry
// per build; older entries (and attestation manifests) keep superseded
// blobs GC-reachable and — before pickOCIDescriptor preferred the newest
// — pinned every reader to the first build. Runs under the caller's
// layout-dir flock. A single already-pruned index is left byte-identical.
func pruneOCILayoutDirIndex(dir, platform string) error {
	indexPath := filepath.Join(dir, "index.json")
	data, err := os.ReadFile(indexPath)
	if err != nil {
		return fmt.Errorf("reading index.json for prune: %w", err)
	}
	var typed struct {
		Manifests []ociDescriptor `json:"manifests"`
	}
	var raw struct {
		Manifests []json.RawMessage `json:"manifests"`
	}
	if err := json.Unmarshal(data, &typed); err != nil {
		return fmt.Errorf("parsing index.json for prune: %w", err)
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("parsing index.json for prune: %w", err)
	}
	if len(typed.Manifests) != len(raw.Manifests) {
		return fmt.Errorf("index.json manifest parse mismatch (%d vs %d)", len(typed.Manifests), len(raw.Manifests))
	}

	wantOS, wantArch := parseOCIPlatform(platform)
	chosen := pickOCIDescriptor(typed.Manifests, wantOS, wantArch)
	// pickOCIDescriptor falls back to the newest manifest/index of ANY
	// platform when no exact match exists (the right call for best-effort
	// resolution). Pruning must not inherit that permissiveness: keeping a
	// wrong-platform entry because it merely LOOKED like a manifest would
	// silently discard the layout's only real candidate for wantOS/wantArch.
	if chosen == nil || chosen.Platform == nil || chosen.Platform.OS != wantOS || chosen.Platform.Architecture != wantArch {
		return fmt.Errorf("no manifest for %s in index.json; refusing to prune", platform)
	}
	chosenIdx := -1
	for i := range typed.Manifests {
		if &typed.Manifests[i] == chosen {
			chosenIdx = i
			break
		}
	}
	if chosenIdx < 0 {
		return fmt.Errorf("internal: chosen descriptor not found in index")
	}
	if len(typed.Manifests) == 1 && chosenIdx == 0 {
		return nil
	}

	out := map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.oci.image.index.v1+json",
		"manifests":     []json.RawMessage{raw.Manifests[chosenIdx]},
	}
	pruned, err := json.Marshal(out)
	if err != nil {
		return fmt.Errorf("marshaling pruned index.json: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".index-*.json")
	if err != nil {
		return fmt.Errorf("staging pruned index.json: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(pruned); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("writing pruned index.json: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("closing pruned index.json: %w", err)
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("chmod pruned index.json: %w", err)
	}
	if err := os.Rename(tmpName, indexPath); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("replacing index.json: %w", err)
	}
	return nil
}

// lockOCILayoutDir serializes use of a persistent layout directory across
// wendy processes (build → read → push → GC). The lock file sits NEXT TO the
// directory (dir+".lock") so a self-heal RemoveAll(dir) never deletes a held
// lock. Returns a release func that must be called exactly once.
func lockOCILayoutDir(ctx context.Context, dir string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return nil, fmt.Errorf("creating OCI layout parent: %w", err)
	}
	f, err := os.OpenFile(dir+".lock", os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("opening OCI layout lock: %w", err)
	}
	locked, err := flock.TryLock(f)
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("acquiring OCI layout lock: %w", err)
	}
	if !locked {
		if err := blockLockFile(ctx, f); err != nil {
			_ = f.Close()
			return nil, fmt.Errorf("acquiring OCI layout lock: %w", err)
		}
	}
	return func() {
		_ = flock.Unlock(f)
		_ = f.Close()
	}, nil
}

// blobLoc is a blob's byte range within an OCI-layout tar.
type blobLoc struct {
	off  int64
	size int64
}

// offsetCountingReader tracks how many bytes have been read from the wrapped
// reader, so the tar scan can record each entry's absolute data offset.
type offsetCountingReader struct {
	r io.Reader
	n int64
}

func (c *offsetCountingReader) Read(p []byte) (int, error) {
	m, err := c.r.Read(p)
	c.n += int64(m)
	return m, err
}

// layerTarReader wraps a compressed layer stream with the decompressor selected
// by media type, returning the raw (uncompressed) tar reader plus a cleanup func
// that releases the decompressor. The reader should be fully consumed first.
func layerTarReader(compressed io.Reader, mediaType string) (io.Reader, func(), error) {
	switch {
	case mediaType == "application/vnd.oci.image.layer.v1.tar" ||
		mediaType == "application/vnd.docker.image.rootfs.diff.tar":
		// Uncompressed — the stream is already the raw tar.
		return compressed, func() {}, nil

	case strings.HasSuffix(mediaType, ".tar+gzip") ||
		strings.HasSuffix(mediaType, ".tar.gzip") ||
		mediaType == "application/vnd.docker.image.rootfs.diff.tar.gzip":
		gr, err := gzip.NewReader(compressed)
		if err != nil {
			return nil, nil, fmt.Errorf("gzip reader: %w", err)
		}
		return gr, func() { _ = gr.Close() }, nil

	case strings.HasSuffix(mediaType, ".tar+zstd") ||
		strings.HasSuffix(mediaType, ".tar.zstd"):
		dec, err := zstd.NewReader(compressed)
		if err != nil {
			return nil, nil, fmt.Errorf("zstd reader: %w", err)
		}
		return dec, func() { dec.Close() }, nil

	default:
		return nil, nil, fmt.Errorf("unsupported layer media type: %q", mediaType)
	}
}

// decompressedLayer is a layer's uncompressed tar spilled to a temp file so the
// whole layer never resides in RAM. Chunk it via ChunkReaderAt(f, size) and
// read missing chunk bytes with f.ReadAt; call Close to delete the temp file.
type decompressedLayer struct {
	f      *os.File
	size   int64
	diffID string // "sha256:<hex>" of the uncompressed tar
}

// Close closes and removes the backing temp file. It is safe to call once.
func (d *decompressedLayer) Close() {
	name := d.f.Name()
	_ = d.f.Close()
	_ = os.Remove(name)
}

// decompressLayerToTemp streams the layer's uncompressed tar into a temp file,
// computing its DiffID as it writes. Peak memory is the decompressor window
// (a few MiB) rather than the whole layer. The returned file is positioned for
// random access via ReadAt; the caller must Close it.
func decompressLayerToTemp(l localLayer) (*decompressedLayer, error) {
	return decompressLayerToTempProgress(l, nil)
}

type progressWriteFunc func(int64)

func (fn progressWriteFunc) Write(p []byte) (int, error) {
	if fn != nil {
		fn(int64(len(p)))
	}
	return len(p), nil
}

// decompressLayerToTempProgress is decompressLayerToTemp with byte telemetry
// for the uncompressed stream. The callback runs on the copying goroutine.
func decompressLayerToTempProgress(l localLayer, progress func(int64)) (*decompressedLayer, error) {
	cr, err := l.compressedReader()
	if err != nil {
		return nil, err
	}
	defer cr.Close()
	r, cleanup, err := layerTarReader(cr, l.MediaType)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	f, err := os.CreateTemp("", "wendy-layer-*")
	if err != nil {
		return nil, fmt.Errorf("create layer temp file: %w", err)
	}
	h := sha256.New()
	writers := []io.Writer{f, h}
	if progress != nil {
		writers = append(writers, progressWriteFunc(progress))
	}
	n, err := io.Copy(io.MultiWriter(writers...), r)
	if err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return nil, fmt.Errorf("decompress layer to disk: %w", err)
	}
	return &decompressedLayer{
		f:      f,
		size:   n,
		diffID: "sha256:" + hex.EncodeToString(h.Sum(nil)),
	}, nil
}

// decompressAndChunkLayerToTemp fuses decompression, DiffID hashing, temp-file
// materialization, and content indexing into one pass over the raw tar. The
// previous cache-miss path wrote the whole layer and then reread it through
// ChunkReaderAt, which made a multi-GiB CUDA layer pay two full disk passes
// before upload could begin.
func decompressAndChunkLayerToTemp(l localLayer, progress func(completed int64)) (*decompressedLayer, []chunk.Ref, error) {
	cr, err := l.compressedReader()
	if err != nil {
		return nil, nil, err
	}
	defer cr.Close()
	r, cleanup, err := layerTarReader(cr, l.MediaType)
	if err != nil {
		return nil, nil, err
	}
	defer cleanup()

	f, err := os.CreateTemp("", "wendy-layer-*")
	if err != nil {
		return nil, nil, fmt.Errorf("create layer temp file: %w", err)
	}
	h := sha256.New()
	refs, n, err := chunk.ChunkStream(io.TeeReader(r, io.MultiWriter(f, h)), progress)
	if err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return nil, nil, fmt.Errorf("decompress and index layer: %w", err)
	}
	return &decompressedLayer{
		f:      f,
		size:   n,
		diffID: "sha256:" + hex.EncodeToString(h.Sum(nil)),
	}, refs, nil
}

// digestToHex converts a "sha256:<hex>" digest string to the bare hex portion.
func digestToHex(digest string) (string, error) {
	const prefix = "sha256:"
	if !strings.HasPrefix(digest, prefix) {
		return "", fmt.Errorf("only sha256 digests supported, got %q", digest)
	}
	return strings.TrimPrefix(digest, prefix), nil
}

// imageBuildFailedError marks a failure of the actual image build (the buildx
// or Apple Container *solve* of the Dockerfile) in the chunk-diff deploy path,
// as opposed to a builder-setup or OCI-export failure. The registry-push
// fallback rebuilds the same image from the same Dockerfile, so a solve failure
// there recurs identically — falling back only buries the actionable build
// error behind a confusing secondary failure (e.g. buildx /etc/hosts setup).
// Callers surface this error directly instead of falling back. See issue #1166.
type imageBuildFailedError struct{ err error }

func (e *imageBuildFailedError) Error() string { return e.err.Error() }
func (e *imageBuildFailedError) Unwrap() error { return e.err }

// isImageBuildFailure reports whether err (or anything it wraps) is an
// imageBuildFailedError, i.e. the Dockerfile build itself failed.
func isImageBuildFailure(err error) bool {
	var bErr *imageBuildFailedError
	return errors.As(err, &bErr)
}

// buildkitOCIArgs builds the buildctl argument vector (excluding the leading
// "buildctl") for a Dockerfile build that exports an OCI-layout tar to dest.
// Build-arg keys are sorted for a reproducible command line.
func buildkitOCIArgs(contextDir, dockerfileDir, dockerfileName, platform string, buildArgs map[string]string, dest string) []string {
	args := []string{
		"build",
		"--frontend", "dockerfile.v0",
		"--local", "context=" + contextDir,
		"--local", "dockerfile=" + dockerfileDir,
	}
	if dockerfileName != "" {
		args = append(args, "--opt", "filename="+dockerfileName)
	}
	if platform != "" {
		args = append(args, "--opt", "platform="+platform)
	}
	keys := make([]string, 0, len(buildArgs))
	for k := range buildArgs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		args = append(args, "--opt", "build-arg:"+k+"="+buildArgs[k])
	}
	args = append(args, "--output", "type=oci,dest="+dest)
	return args
}

// redactBuildctlArgsForLog masks build-arg values in a buildctl command line
// (the key is kept). buildctl passes build args as "build-arg:KEY=VALUE" tokens.
func redactBuildctlArgsForLog(args []string) []string {
	out := make([]string, len(args))
	copy(out, args)
	for i, a := range out {
		const p = "build-arg:"
		if strings.HasPrefix(a, p) {
			if k, _, ok := strings.Cut(a[len(p):], "="); ok && k != "" {
				out[i] = p + k + "=<redacted>"
			}
		}
	}
	return out
}

// buildImageToOCILayoutWithBuildkit builds the image with buildctl against the
// in-container buildkitd (its address comes from BUILDKIT_HOST in the container
// env) and exports it as an OCI-layout tar at dest for the chunk-diff deploy
// path. This is the no-Docker on-device builder; it mirrors the Apple-Container
// backend's contract (produce the tar, no registry push).
func buildImageToOCILayoutWithBuildkit(ctx context.Context, cwd, dockerfile, platform string, buildArgs map[string]string, dest string, stdout, stderr io.Writer) error {
	dfDir := cwd
	dfName := ""
	if dockerfile != "" {
		resolved, err := confinedDockerfilePath(cwd, dockerfile)
		if err != nil {
			return err
		}
		dfDir = filepath.Dir(resolved)
		dfName = filepath.Base(resolved)
	}
	if _, err := sortedValidatedBuildArgKeys(buildArgs); err != nil {
		return err
	}
	args := buildkitOCIArgs(cwd, dfDir, dfName, platform, buildArgs, dest)
	fmt.Fprintf(stderr, "[buildkit] starting OCI export: buildctl %s\n", strings.Join(redactBuildctlArgsForLog(args), " "))
	// NOTE: redactBuildctlArgsForLog only masks values in the log line above.
	// The unredacted `--opt build-arg:KEY=VALUE` tokens are still placed in
	// buildctl's argv (below) and are visible in the host process table
	// (/proc/<pid>/cmdline). Build args are NOT a secret channel — never pass
	// credentials via --build-arg; use buildctl's `--secret` for those.
	cmd := exec.CommandContext(ctx, "buildctl", args...)
	cmd.Dir = cwd
	// BuildKit writes its build progress to stderr, not stdout (see
	// tui/buildrawjson.go); only stdout feeds the build parser (stream). Point
	// cmd.Stderr at the *same* stdout value, not a separate writer, so
	// os/exec collapses stdout+stderr into a single pipe/copy goroutine (the
	// same same-writer-value trick buildAndPushImage uses, docker.go
	// ~:2034-2038) — otherwise the whole build's progress lands in the
	// setup-log buffer instead of the parser, which WDY-2432's synthetic
	// builder-setup step then renders as a flood of garbled
	// "preparing buildx builder" lines.
	cmd.Stdout = stdout
	cmd.Stderr = stdout
	if err := cmd.Run(); err != nil {
		return &imageBuildFailedError{fmt.Errorf("buildctl build (OCI export) failed: %w", err)}
	}
	return nil
}

// buildImageToOCILayout builds an OCI-layout tar to dest for the chunk-diff
// deploy path. When builder is apple-container, it uses the Apple Container CLI;
// otherwise it runs `docker buildx build` with `--output type=oci,dest=<dest>`.
// It mirrors the flag/cache/env setup of buildAndPushImage but skips registry
// push entirely.
func buildImageToOCILayout(ctx context.Context, cwd, dockerfile, platform string, buildArgs map[string]string, builder, dest string, stdout, stderr io.Writer) error {
	normalized, err := resolveOCIExportBuilder(builder)
	if err != nil {
		return err
	}
	if normalized == imageBuilderAppleContainer {
		return buildImageToOCILayoutWithAppleContainer(ctx, cwd, dockerfile, platform, buildArgs, dest, stdout, stderr)
	}
	if normalized == imageBuilderBuildkit {
		return buildImageToOCILayoutWithBuildkit(ctx, cwd, dockerfile, platform, buildArgs, dest, stdout, stderr)
	}

	return buildImageWithBuildxOCIExport(ctx, cwd, dockerfile, platform, buildArgs, dest, false, stdout, stderr)
}

// buildImageToOCILayoutDirWithDocker builds with the shared buildx builder and
// exports into an OCI layout DIRECTORY (`--output type=oci,tar=false`). Unlike
// the tar export, BuildKit skips blobs already present at dest, so a warm
// persistent directory turns the per-build export cost from O(image size)
// into O(changed bytes). Callers own dest's lifecycle (locking and GC).
func buildImageToOCILayoutDirWithDocker(ctx context.Context, cwd, dockerfile, platform string, buildArgs map[string]string, destDir string, stdout, stderr io.Writer) error {
	// 0700: image layers/config can embed build-time material, and the legacy
	// temp-tar this replaces lived in a MkdirTemp (0700) dir. Restricting the
	// top-level dir gates traversal regardless of the modes BuildKit gives the
	// blob files it writes beneath it.
	if err := os.MkdirAll(destDir, 0o700); err != nil {
		return fmt.Errorf("creating OCI layout directory: %w", err)
	}
	if err := buildImageWithBuildxOCIExport(ctx, cwd, dockerfile, platform, buildArgs, destDir, true, stdout, stderr); err != nil {
		return err
	}
	// The export appended this build's manifest; drop every older entry so
	// readers and GC see exactly one current image. Prune is hygiene, not
	// correctness: pickOCIDescriptor (newest-last) and gcOCILayoutDir (keeps
	// everything index-reachable) are both correct even without pruning, and
	// prune is strictly LESS tolerant than either (exact top-level platform
	// match only, no nested indexes). A buildx environment whose index shapes
	// prune refuses must not lose the ability to build, so a prune failure is
	// only a warning here.
	if err := pruneOCILayoutDirIndex(destDir, platform); err != nil {
		fmt.Fprintf(stderr, "warning: could not prune OCI layout index (continuing; superseded entries accumulate until a successful prune): %v\n", err)
	}
	return nil
}

// chunkLayoutDir is the persistent per-app OCI layout directory used by the
// chunk-diff deploy path's tar=false export. Keyed by app AND platform so a
// device-architecture switch never GC-thrashes the other platform's blobs.
func chunkLayoutDir(userCacheDir, appID, platform string) string {
	return filepath.Join(userCacheDir, "wendy", "ocilayout",
		sanitizeCacheKey(appID)+"-"+sanitizeCacheKey(platform))
}

// buildxOCIExportArgs assembles the `docker buildx build` argument vector for
// an OCI export (tar or layout-dir). Pure function for testability; build-arg
// keys are sorted for a reproducible command line.
func buildxOCIExportArgs(builder, platform, dockerfile, cacheFromDir, dest string, tarFalse bool, buildArgs map[string]string) []string {
	// buildkitd inside the Linux VM appends "/index.json" to the cache src, so
	// pass forward-slash paths to avoid mixed-separator warnings on Windows.
	args := []string{
		"buildx", "build",
		"--builder", builder,
		"--platform", platform,
		"--progress", "plain",
	}
	if dockerfile != "" {
		args = append(args, "-f", dockerfile)
	}
	if cacheFromDir != "" {
		args = append(args, "--cache-from", "type=local,src="+filepath.ToSlash(cacheFromDir))
	}
	// Embed the rebuild cache INTO the exported image (inline) rather than a
	// separate local cache store. The persistent OCI layout at dest then doubles
	// as the cache source (--cache-from the layout on the next build), so each
	// layer is stored once instead of in both a buildx cache dir and the deploy
	// layout. Inline is min-mode, matching the old default-mode local export, so
	// warm-rebuild reuse is unchanged.
	args = append(args, "--cache-to", "type=inline")
	keys := make([]string, 0, len(buildArgs))
	for k := range buildArgs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		args = append(args, "--build-arg", k+"="+buildArgs[k])
	}
	// The chunk-diff path consumes uncompressed layer tar streams and hashes
	// their DiffIDs before sending only missing chunks to the agent. Asking the
	// OCI exporter to gzip those layers first adds a full-image compression pass
	// (especially expensive for multi-gigabyte CUDA libraries), only for the
	// chunking path to decompress them again. Keep the persistent layout in the
	// representation the deploy path actually consumes.
	output := "type=oci,dest=" + dest + ",compression=uncompressed"
	if tarFalse {
		output += ",tar=false"
	}
	args = append(args, "--output", output, ".")
	return args
}

// buildImageWithBuildxOCIExport is the shared docker-buildx body behind both
// OCI export shapes: dest is a tar path (tarFalse=false, legacy) or a layout
// directory (tarFalse=true).
var (
	ensureOCIExportBuilderForBuild = ensureOCIExportBuilder
	runBuildxOCIExportCommand      = func(ctx context.Context, cwd string, args, env []string, stdout, stderr io.Writer) error {
		cmd := exec.CommandContext(ctx, "docker", args...)
		cmd.Dir = cwd
		cmd.Stdout = stdout
		cmd.Stderr = stderr
		if env != nil {
			cmd.Env = env
		}
		return cmd.Run()
	}
)

func buildImageWithBuildxOCIExport(ctx context.Context, cwd, dockerfile, platform string, buildArgs map[string]string, dest string, tarFalse bool, stdout, stderr io.Writer) error {
	// Sub-phase timing (gated on WENDY_TIMING) to split the "build (oci export)"
	// phase into lock acquisition, builder verification (the buildx inspect
	// calls), and the actual buildx solve.
	submark := phaseTimer()

	// Serialize only builder discovery/bootstrap. The OCI builder has stable
	// configuration (it never knows about a deployment's dynamic registry
	// proxy), so BuildKit can safely execute independent solves concurrently
	// after setup. Holding this process-wide lock for the whole solve made
	// unrelated `wendy run` invocations queue behind one another.
	releaseLock, err := buildLock.acquire(ctx, stderr)
	if err != nil {
		return err
	}
	submark("  build: acquire setup lock")

	buildxBuilder, err := ensureOCIExportBuilderForBuild(ctx, stderr)
	releaseLock()
	if err != nil {
		return err
	}
	submark("  build: ensure builder (inspects)")

	// Mirror the clean DOCKER_CONFIG setup from buildAndPushImage (non-Windows).
	cleanDockerConfigDir, err := ensureCleanDockerConfig()
	if err != nil {
		return err
	}

	resolvedDockerfile := ""
	if dockerfile != "" {
		resolvedDockerfile, err = confinedDockerfilePath(cwd, dockerfile)
		if err != nil {
			return err
		}
	}
	// The persistent OCI layout at dest doubles as the rebuild cache: on a warm
	// build it already holds the previous image with inline cache metadata, which
	// we import via --cache-from. A cold build (no layout yet) or the ephemeral
	// tar-export escape hatch (dest is a tar file, not a layout dir) simply has no
	// cache source and relies on the builder's own store. This replaces the
	// separate per-app buildx local cache, whose isolation existed ONLY because
	// BuildKit's local cache EXPORTER is not concurrency-safe (WDY-1689/1711); the
	// inline exporter writes only into the per-app layout dir callers serialize.
	cacheFromDir := ""
	if dest != "" {
		if _, statErr := os.Stat(filepath.Join(dest, "index.json")); statErr == nil {
			cacheFromDir = dest
		}
	}
	args := buildxOCIExportArgs(buildxBuilder, platform, resolvedDockerfile, cacheFromDir, dest, tarFalse, buildArgs)

	submark("  build: setup (cache/env)")

	fmt.Fprintf(stderr, "[buildx] starting OCI export: docker %s\n", strings.Join(redactBuildArgsForLog(args), " "))
	// BuildKit writes its build progress to stderr, not stdout (see
	// tui/buildrawjson.go); only stdout feeds the build parser (stream). Pass
	// the *same* stdout value for both streams so os/exec collapses
	// stdout+stderr into a single pipe/copy goroutine (the same
	// same-writer-value trick buildAndPushImage uses, docker.go ~:2034-2038)
	// — otherwise the whole build's progress lands in the setup-log buffer
	// instead of the parser, which WDY-2432's synthetic builder-setup step
	// then renders as a flood of garbled "preparing buildx builder" lines.
	// The announce line above stays on the stderr *parameter* directly (it's
	// genuine setup chatter, unaffected by the command's stream wiring).
	if err := runBuildxOCIExportCommand(ctx, cwd, args, dockerConfigEnv(cleanDockerConfigDir), stdout, stdout); err != nil {
		return &imageBuildFailedError{fmt.Errorf("docker buildx build (OCI export) failed: %w", err)}
	}
	return nil
}

// buildImageToOCILayoutWithAppleContainer builds the image with the Apple
// Container CLI and exports it as an OCI-layout tar at dest for the chunk-diff
// deploy path. Apple Container cannot stream an OCI tar straight from `build`
// (its `-o type=oci,dest=` writes inside the build VM and never reaches the
// host), so we build into the local image store under a unique temporary tag,
// `image save` it to the host, and remove the tag afterward. This lets the whole
// fast-path deploy run without Docker on Apple silicon.
//
// The caller is responsible for ensuring the Apple Container system is running
// (see ensureAppleContainerSystemForBuilder). There is no local build-cache
// export equivalent to buildx's --cache-to; Apple Container reuses its own
// build cache across runs.
func buildImageToOCILayoutWithAppleContainer(ctx context.Context, cwd, dockerfile, platform string, buildArgs map[string]string, dest string, stdout, stderr io.Writer) error {
	submark := phaseTimer()

	// Apple Container owns its build scheduler and this path already uses a
	// unique temporary image tag and destination per invocation. It does not
	// share BuildKit's local cache exporter or the Docker builder configuration,
	// so it must not participate in Docker's cross-process setup lock.
	buildContext, err := appleContainerBuildContextPath(cwd)
	if err != nil {
		return fmt.Errorf("resolving project path: %w", err)
	}
	contextMonitor := newAppleContainerBuildContextMonitor(buildContext)
	stdout = contextMonitor.wrapStream(stdout)
	stderr = contextMonitor.wrapStream(stderr)

	// Unique per-build tag: dest is a fresh wendy-oci-* tempdir, so concurrent
	// invocations and watch cycles never collide on the temporary image.
	imageRef := "wendy-oci-build:" + sanitizeAppleContainerTag(filepath.Base(filepath.Dir(dest)))

	// --progress plain so the shared build parser can read the output (see
	// buildImageWithAppleContainer for the format rationale).
	args := []string{"build", "--progress", "plain", "--platform", platform, "-t", imageRef}
	if dockerfile != "" {
		resolvedDockerfile, err := appleContainerBuildFilePath(cwd, dockerfile)
		if err != nil {
			return err
		}
		args = append(args, "-f", resolvedDockerfile)
	}
	keys, err := sortedValidatedBuildArgKeys(buildArgs)
	if err != nil {
		return err
	}
	for _, k := range keys {
		args = append(args, "--build-arg", k+"="+buildArgs[k])
	}
	args = append(args, buildContext)
	submark("  build: setup")

	fmt.Fprintf(stderr, "[apple-container] building OCI image: container %s\n", strings.Join(redactBuildArgsForLog(args), " "))
	buildCmd := imageBuilderCommandContext(ctx, "container", args...)
	buildCmd.Dir = buildContext
	// The container CLI writes its --progress plain build output to stderr,
	// not stdout; only stdout feeds the build parser (stream). Point
	// buildCmd.Stderr at the *same* stdout value, not the separate stderr
	// param, so os/exec collapses stdout+stderr into a single pipe/copy
	// goroutine (the same same-writer-value trick buildAndPushImage uses,
	// docker.go ~:2034-2038, and A6 applied to the buildx/buildctl OCI-export
	// paths above) — otherwise the whole build's progress lands in the
	// setup-log writer instead of the parser, which WDY-2432's synthetic
	// builder-setup step then renders as a flood of garbled lines. The
	// announce line above stays on the stderr *parameter* directly (it's
	// genuine setup chatter, unaffected by cmd's stdout/stderr wiring).
	buildCmd.Stdout = stdout
	buildCmd.Stderr = stdout
	if err := buildCmd.Run(); err != nil {
		return &imageBuildFailedError{fmt.Errorf("container build (OCI layout) failed: %w", contextMonitor.wrapBuildError(err))}
	}
	// The image is in the store now — remove the temporary tag once we are done,
	// even if the export below is cancelled.
	defer func() {
		rm := imageBuilderCommandContext(context.Background(), "container", "image", "rm", imageRef)
		_ = rm.Run()
	}()

	saveArgs := []string{"image", "save", imageRef, "--platform", platform, "-o", dest}
	fmt.Fprintf(stderr, "[apple-container] exporting OCI layout: container %s\n", strings.Join(saveArgs, " "))
	saveCmd := imageBuilderCommandContext(ctx, "container", saveArgs...)
	// Same same-writer-value trick as buildCmd above, for the same reason:
	// route saveCmd's stderr into the parser feed (stdout), not the separate
	// setup-log writer.
	saveCmd.Stdout = stdout
	saveCmd.Stderr = stdout
	if err := saveCmd.Run(); err != nil {
		return fmt.Errorf("container image save (OCI layout) failed: %w", err)
	}
	submark("  build: oci save")
	return nil
}

// sanitizeAppleContainerTag maps an arbitrary string to a valid image tag
// ([a-z0-9._-]); anything else becomes '-'.
func sanitizeAppleContainerTag(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	if b.Len() == 0 {
		return "latest"
	}
	return b.String()
}
