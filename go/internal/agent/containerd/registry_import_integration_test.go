//go:build linux

package containerd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/google/uuid"
	digest "github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/identity"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/wendylabsinc/wendy/go/internal/agent/services"
)

// A verified registry release imports as a labelled image whose layers
// unpack into one stacked rootfs; importing it again keeps it.
func TestIntegrationImportRelease(t *testing.T) {
	c, ctx := newIntegrationClient(t)
	dir := t.TempDir()
	release := services.ReleaseImage{AppID: "sh.wendy.test", ReleaseID: uuid.NewString(), Blobs: map[string]string{}}
	stage := func(data []byte) digest.Digest {
		d := digest.FromBytes(data)
		path := filepath.Join(dir, d.Encoded())
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		release.Blobs[d.String()] = path
		return d
	}
	var layers []ocispec.Descriptor
	var diffIDs []digest.Digest
	for _, file := range integrationLayerFiles {
		blob := singleFileTar(t, file)
		d := stage(blob)
		layers = append(layers, ocispec.Descriptor{MediaType: ocispec.MediaTypeImageLayer, Digest: d, Size: int64(len(blob))})
		diffIDs = append(diffIDs, d)
	}
	config, err := json.Marshal(ocispec.Image{
		Platform: ocispec.Platform{OS: "linux", Architecture: runtime.GOARCH},
		RootFS:   ocispec.RootFS{Type: "layers", DiffIDs: diffIDs},
	})
	if err != nil {
		t.Fatal(err)
	}
	manifest := ocispec.Manifest{
		MediaType: ocispec.MediaTypeImageManifest,
		Config:    ocispec.Descriptor{MediaType: ocispec.MediaTypeImageConfig, Digest: stage(config), Size: int64(len(config))},
		Layers:    layers,
	}
	manifest.SchemaVersion = 2
	if release.Manifest, err = json.Marshal(manifest); err != nil {
		t.Fatal(err)
	}
	release.ManifestDigest = digest.FromBytes(release.Manifest).String()

	for range 2 {
		if err := c.ImportRelease(ctx, release); err != nil {
			t.Fatalf("importing release: %v", err)
		}
	}
	name := ReleaseImageName(release.AppID, release.ReleaseID)
	image, err := c.client.GetImage(ctx, name)
	if err != nil {
		t.Fatalf("loading imported image: %v", err)
	}
	labels := image.Labels()
	if labels[labelKeyAppID] != release.AppID || labels[labelKeyReleaseID] != release.ReleaseID || labels[labelKeyImageDigest] != release.ManifestDigest {
		t.Fatalf("image labels = %v, want the release identity", labels)
	}
	if image.Target().Digest.String() != release.ManifestDigest {
		t.Fatalf("image target = %s, want %s", image.Target().Digest, release.ManifestDigest)
	}
	ensureUnpacked(t, c, ctx, image)
	img := integrationImage{name: name}
	for i := range diffIDs {
		img.chainIDs = append(img.chainIDs, identity.ChainID(diffIDs[:i+1]).String())
	}
	requireImageRootfs(t, c, ctx, img)
}
