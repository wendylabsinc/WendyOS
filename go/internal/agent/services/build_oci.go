package services

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/klauspost/compress/zstd"
)

// exportedImage is one platform's image as read out of the OCI layout tar that
// buildctl exports on the build host. Layer blobs stay in the tar and are
// streamed from it by byte range when a device turns out to need them; only the
// small index, manifest and config blobs are held in memory.
type exportedImage struct {
	tarPath string
	// manifestDigest is the image manifest's "sha256:<hex>" — the digest the
	// result reports to the CLI.
	manifestDigest string
	// config is the raw image config blob. It travels to the device unchanged so
	// Cmd, Entrypoint, Env, WorkingDir and User survive reassembly from chunks;
	// the device re-derives only rootfs.diff_ids from the layers it assembles.
	config []byte
	layers []ociLayer
}

// ociLayer addresses one layer's compressed blob inside the exported tar.
type ociLayer struct {
	digest    string // compressed blob digest, "sha256:<hex>"
	diffID    string // uncompressed digest, from the config's rootfs.diff_ids
	mediaType string
	offset    int64 // where the blob's bytes start within tarPath
	size      int64 // compressed length
}

// ociDescriptor is a descriptor as it appears in an OCI index or manifest.
type ociDescriptor struct {
	MediaType string `json:"mediaType"`
	Digest    string `json:"digest"`
	Size      int64  `json:"size"`
	Platform  *struct {
		Architecture string `json:"architecture"`
		OS           string `json:"os"`
	} `json:"platform"`
}

// maxOCIMetadataBytes bounds the index, manifest and config blobs read into
// memory. Real ones are a few KiB; the tar is produced by this host's own
// buildkitd, so the bound guards against a corrupt export, not an adversary.
const maxOCIMetadataBytes = 16 << 20

// maxOCIIndexDepth bounds how far a nested image index is followed. One level
// of nesting is what a multi-platform export produces; two is generous.
const maxOCIIndexDepth = 2

type blobRange struct {
	off  int64
	size int64
}

// offsetTrackingReader records how many bytes have passed through it, so the tar
// scan can note where each entry's data starts within the file.
type offsetTrackingReader struct {
	r io.Reader
	n int64
}

func (c *offsetTrackingReader) Read(p []byte) (int, error) {
	m, err := c.r.Read(p)
	c.n += int64(m)
	return m, err
}

// readExportedImage indexes an OCI layout tar without reading its layer bytes:
// one pass records where every blob lives, then the index, the platform's
// manifest and its config are read back by range.
//
// Every layer must have a diff ID, because chunked delivery addresses layers by
// their uncompressed digest: that is the identity QueryLayers and PrepareImage
// speak, and it is what lets a device recognise a layer it already holds no
// matter how it was compressed on the way in.
func readExportedImage(tarPath, platform string) (*exportedImage, error) {
	f, err := os.Open(tarPath)
	if err != nil {
		return nil, fmt.Errorf("opening the exported image: %w", err)
	}
	defer f.Close()

	blobs := map[string]blobRange{}
	var indexJSON []byte
	cr := &offsetTrackingReader{r: f}
	tr := tar.NewReader(cr)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("reading the exported image: %w", err)
		}
		// After Next the counting reader sits exactly at this entry's data: tar
		// headers and padding are 512-byte blocks read straight from f.
		dataOff := cr.n
		switch {
		case hdr.Name == "index.json":
			indexJSON, err = readBounded(tr, "index.json")
			if err != nil {
				return nil, err
			}
		case strings.HasPrefix(hdr.Name, "blobs/sha256/"):
			blobs["sha256:"+strings.TrimPrefix(hdr.Name, "blobs/sha256/")] = blobRange{off: dataOff, size: hdr.Size}
			if _, err := io.Copy(io.Discard, tr); err != nil {
				return nil, fmt.Errorf("scanning blob %q: %w", hdr.Name, err)
			}
		}
	}
	if indexJSON == nil {
		return nil, fmt.Errorf("the exported image has no index.json")
	}

	getBlob := func(digest string) ([]byte, error) {
		loc, ok := blobs[digest]
		if !ok {
			return nil, fmt.Errorf("blob %s is not in the exported image", digest)
		}
		if loc.size > maxOCIMetadataBytes {
			return nil, fmt.Errorf("blob %s is %d bytes, too large for image metadata", digest, loc.size)
		}
		return io.ReadAll(io.NewSectionReader(f, loc.off, loc.size))
	}

	var index struct {
		Manifests []ociDescriptor `json:"manifests"`
	}
	if err := json.Unmarshal(indexJSON, &index); err != nil {
		return nil, fmt.Errorf("parsing index.json: %w", err)
	}
	wantOS, wantArch := splitPlatform(platform)
	manifestDesc, err := selectImageManifest(index.Manifests, getBlob, wantOS, wantArch, 0)
	if err != nil {
		return nil, err
	}
	manifestJSON, err := getBlob(manifestDesc.Digest)
	if err != nil {
		return nil, fmt.Errorf("reading the image manifest: %w", err)
	}
	var manifest struct {
		Config ociDescriptor   `json:"config"`
		Layers []ociDescriptor `json:"layers"`
	}
	if err := json.Unmarshal(manifestJSON, &manifest); err != nil {
		return nil, fmt.Errorf("parsing the image manifest: %w", err)
	}
	config, err := getBlob(manifest.Config.Digest)
	if err != nil {
		return nil, fmt.Errorf("reading the image config: %w", err)
	}
	var cfg struct {
		RootFS struct {
			DiffIDs []string `json:"diff_ids"`
		} `json:"rootfs"`
	}
	if err := json.Unmarshal(config, &cfg); err != nil {
		return nil, fmt.Errorf("parsing the image config: %w", err)
	}
	if len(cfg.RootFS.DiffIDs) != len(manifest.Layers) {
		return nil, fmt.Errorf("the image config lists %d diff IDs for %d layers", len(cfg.RootFS.DiffIDs), len(manifest.Layers))
	}

	layers := make([]ociLayer, 0, len(manifest.Layers))
	for i, d := range manifest.Layers {
		loc, ok := blobs[d.Digest]
		if !ok {
			return nil, fmt.Errorf("layer %d (%s) is not in the exported image", i, d.Digest)
		}
		if !supportedLayerMediaType(d.MediaType) {
			return nil, fmt.Errorf("layer %d has unsupported media type %q", i, d.MediaType)
		}
		if !strings.HasPrefix(cfg.RootFS.DiffIDs[i], "sha256:") {
			return nil, fmt.Errorf("layer %d has an unsupported diff ID %q", i, cfg.RootFS.DiffIDs[i])
		}
		layers = append(layers, ociLayer{
			digest:    d.Digest,
			diffID:    cfg.RootFS.DiffIDs[i],
			mediaType: d.MediaType,
			offset:    loc.off,
			size:      loc.size,
		})
	}
	return &exportedImage{
		tarPath:        tarPath,
		manifestDigest: manifestDesc.Digest,
		config:         config,
		layers:         layers,
	}, nil
}

func readBounded(r io.Reader, name string) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxOCIMetadataBytes+1))
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", name, err)
	}
	if len(data) > maxOCIMetadataBytes {
		return nil, fmt.Errorf("%s exceeds %d bytes", name, maxOCIMetadataBytes)
	}
	return data, nil
}

// splitPlatform returns the os and architecture halves of "os/arch[/variant]".
func splitPlatform(platform string) (os, arch string) {
	parts := strings.Split(platform, "/")
	if len(parts) >= 1 {
		os = parts[0]
	}
	if len(parts) >= 2 {
		arch = parts[1]
	}
	return os, arch
}

func isOCIIndexMediaType(mt string) bool {
	return mt == "application/vnd.oci.image.index.v1+json" ||
		mt == "application/vnd.docker.distribution.manifest.list.v2+json"
}

// selectImageManifest picks the one image manifest for the requested platform,
// following a nested index when the export produced one.
//
// Descriptors are matched on os/arch when they carry a platform and taken as
// they are when they do not — a single-platform export need not label its only
// manifest. Attestation manifests, which buildkit labels unknown/unknown, are
// never candidates. Several candidates is an error rather than a guess: the
// wrong manifest here is an image built for another machine delivered as if it
// were this one.
func selectImageManifest(descs []ociDescriptor, getBlob func(string) ([]byte, error), wantOS, wantArch string, depth int) (ociDescriptor, error) {
	if depth > maxOCIIndexDepth {
		return ociDescriptor{}, fmt.Errorf("the exported image nests indexes more than %d deep", maxOCIIndexDepth)
	}
	var chosen *ociDescriptor
	for i := range descs {
		d := &descs[i]
		if p := d.Platform; p != nil {
			if p.OS == "unknown" && p.Architecture == "unknown" {
				continue
			}
			if wantOS != "" && wantArch != "" && (p.OS != wantOS || p.Architecture != wantArch) {
				continue
			}
		}
		if chosen != nil {
			return ociDescriptor{}, fmt.Errorf("the exported image has several manifests for %s/%s", wantOS, wantArch)
		}
		chosen = d
	}
	if chosen == nil {
		return ociDescriptor{}, fmt.Errorf("the exported image has no manifest for %s/%s", wantOS, wantArch)
	}
	if !isOCIIndexMediaType(chosen.MediaType) {
		return *chosen, nil
	}
	nestedJSON, err := getBlob(chosen.Digest)
	if err != nil {
		return ociDescriptor{}, fmt.Errorf("reading nested image index: %w", err)
	}
	var nested struct {
		Manifests []ociDescriptor `json:"manifests"`
	}
	if err := json.Unmarshal(nestedJSON, &nested); err != nil {
		return ociDescriptor{}, fmt.Errorf("parsing nested image index: %w", err)
	}
	return selectImageManifest(nested.Manifests, getBlob, wantOS, wantArch, depth+1)
}

// layerCompression is how a layer blob is compressed inside the export.
type layerCompression int

const (
	layerUncompressed layerCompression = iota
	layerGzip
	layerZstd
)

// layerCompressionOf classifies a layer media type. It recognises the same
// types as the CLI's layerTarReader so both ends of the mesh decompress the
// same layer to the same bytes — chunk hashes only match if they do.
func layerCompressionOf(mediaType string) (layerCompression, bool) {
	switch {
	case mediaType == "application/vnd.oci.image.layer.v1.tar" ||
		mediaType == "application/vnd.docker.image.rootfs.diff.tar":
		return layerUncompressed, true
	case strings.HasSuffix(mediaType, ".tar+gzip") ||
		strings.HasSuffix(mediaType, ".tar.gzip") ||
		mediaType == "application/vnd.docker.image.rootfs.diff.tar.gzip":
		return layerGzip, true
	case strings.HasSuffix(mediaType, ".tar+zstd") ||
		strings.HasSuffix(mediaType, ".tar.zstd"):
		return layerZstd, true
	default:
		return 0, false
	}
}

func supportedLayerMediaType(mediaType string) bool {
	_, ok := layerCompressionOf(mediaType)
	return ok
}

// layerDecompressor wraps a compressed layer stream with its decompressor,
// returning the raw tar reader and a release func to call once it is consumed.
func layerDecompressor(compressed io.Reader, mediaType string) (io.Reader, func(), error) {
	c, ok := layerCompressionOf(mediaType)
	if !ok {
		return nil, nil, fmt.Errorf("unsupported layer media type %q", mediaType)
	}
	switch c {
	case layerGzip:
		gr, err := gzip.NewReader(compressed)
		if err != nil {
			return nil, nil, fmt.Errorf("gzip reader: %w", err)
		}
		return gr, func() { _ = gr.Close() }, nil
	case layerZstd:
		dec, err := zstd.NewReader(compressed)
		if err != nil {
			return nil, nil, fmt.Errorf("zstd reader: %w", err)
		}
		return dec, dec.Close, nil
	default:
		return compressed, func() {}, nil
	}
}
