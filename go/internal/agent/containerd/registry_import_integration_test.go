//go:build linux

package containerd

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/containerd/errdefs"
	"github.com/google/uuid"
	digest "github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/identity"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/wendylabsinc/wendy/go/internal/agent/services"
)

// integrationRelease imports a release of app with one layer per file, base
// first, the way the postbox pull does, and unpacks it.
type integrationRelease struct {
	services.ReleaseImage
	img integrationImage
}

func importIntegrationRelease(t *testing.T, c *Client, ctx context.Context, app string, files []string) integrationRelease {
	t.Helper()
	dir := t.TempDir()
	release := services.ReleaseImage{AppID: app, ReleaseID: uuid.NewString(), Blobs: map[string]string{}}
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
	for _, file := range files {
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

	for range 2 { // importing it again keeps it
		if err := c.ImportRelease(ctx, release); err != nil {
			t.Fatalf("importing release: %v", err)
		}
	}
	r := integrationRelease{ReleaseImage: release, img: integrationImage{name: services.ReleaseImageName(app, release.ReleaseID)}}
	for i := range diffIDs {
		r.img.chainIDs = append(r.img.chainIDs, identity.ChainID(diffIDs[:i+1]).String())
	}
	image, err := c.client.GetImage(ctx, r.img.name)
	if err != nil {
		t.Fatalf("loading imported image: %v", err)
	}
	ensureUnpacked(t, c, ctx, image)
	return r
}

// A verified registry release imports as a labelled image whose layers
// unpack into one stacked rootfs; importing it again keeps it.
func TestIntegrationImportRelease(t *testing.T) {
	c, ctx := newIntegrationClient(t)
	release := importIntegrationRelease(t, c, ctx, "sh.wendy.test", integrationLayerFiles)
	image, err := c.client.GetImage(ctx, release.img.name)
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
	requireImageRootfs(t, c, ctx, release.img)
}

// Removing a release takes its unpacked layers off the disk (#3054 high 2);
// a layer a held release shares stays.
func TestIntegrationDeleteReleaseImagesCollectsLayers(t *testing.T) {
	c, ctx := newIntegrationClient(t)
	const app = "sh.wendy.test"
	removed := importIntegrationRelease(t, c, ctx, app, []string{"base.txt", "removed.txt"})
	held := importIntegrationRelease(t, c, ctx, app, []string{"base.txt", "held.txt"})

	if err := c.DeleteReleaseImages(ctx, app, []string{held.ReleaseID}); err != nil {
		t.Fatalf("deleting releases: %v", err)
	}
	sn := c.client.SnapshotService(c.snapshotter)
	if _, err := sn.Stat(ctx, removed.img.topChainID()); !errdefs.IsNotFound(err) {
		t.Fatalf("removed release's own layer: stat err = %v, want not found", err)
	}
	for _, id := range held.img.chainIDs { // the shared base included
		if _, err := sn.Stat(ctx, id); err != nil {
			t.Fatalf("held release's layer %s: %v", id, err)
		}
	}
	if _, err := c.client.GetImage(ctx, removed.img.name); !errdefs.IsNotFound(err) {
		t.Fatalf("removed release image: err = %v, want not found", err)
	}
}
