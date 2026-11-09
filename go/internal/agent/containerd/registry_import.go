package containerd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"

	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/core/leases"
	"github.com/containerd/errdefs"
	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"go.uber.org/zap"

	"github.com/wendylabsinc/wendy/go/internal/agent/services"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
)

// ReleaseImageName is the local image name of a registry release.
func ReleaseImageName(appID, releaseID string) string {
	return "localhost/wendy-release/" + appID + ":" + releaseID
}

// ImportRelease writes a verified registry release (registry plan T14) into
// the content store and names it ReleaseImageName, labelled with its app and
// release identity. containerd checks every blob against its digest again as
// it commits it.
func (c *Client) ImportRelease(ctx context.Context, img services.ReleaseImage) error {
	if err := appconfig.ValidateAppID(img.AppID); err != nil {
		return fmt.Errorf("invalid app ID: %w", err)
	}
	if !isCanonicalUUID(img.ReleaseID) {
		return errors.New("invalid release id: want a canonical lowercase UUID")
	}
	manifestDigest, err := digest.Parse(img.ManifestDigest)
	if err != nil || digest.FromBytes(img.Manifest) != manifestDigest {
		return fmt.Errorf("manifest does not match %q", img.ManifestDigest)
	}
	var manifest ocispec.Manifest
	if err := json.Unmarshal(img.Manifest, &manifest); err != nil {
		return fmt.Errorf("parsing manifest: %w", err)
	}

	ctx = c.withNamespace(ctx)
	cleanupCtx := c.withNamespace(context.Background())
	cs := c.client.ContentStore()
	// Blobs are unreferenced until the image record roots the manifest.
	ctx, doneLease, err := c.client.WithLease(ctx, leases.WithExpiration(assembleLeaseExpiration))
	if err != nil {
		return fmt.Errorf("creating import lease: %w", err)
	}
	defer func() {
		if err := doneLease(cleanupCtx); err != nil {
			c.logger.Warn("Failed to release import lease; relying on expiration backstop", zap.Error(err))
		}
	}()

	children := append([]ocispec.Descriptor{manifest.Config}, manifest.Layers...)
	manifestLabels := map[string]string{"containerd.io/gc.ref.content.config": manifest.Config.Digest.String()}
	for i, d := range children {
		if i > 0 {
			manifestLabels["containerd.io/gc.ref.content.l."+strconv.Itoa(i-1)] = d.Digest.String()
		}
		path, ok := img.Blobs[d.Digest.String()]
		if !ok {
			return fmt.Errorf("blob %s missing from the release", d.Digest)
		}
		if err := writeBlobFile(ctx, cs, path, d); err != nil {
			return err
		}
	}
	mediaType := manifest.MediaType
	if mediaType == "" {
		mediaType = ocispec.MediaTypeImageManifest
	}
	manifestDesc := ocispec.Descriptor{MediaType: mediaType, Digest: manifestDigest, Size: int64(len(img.Manifest))}
	if err := content.WriteBlob(ctx, cs, manifestDigest.String(), bytes.NewReader(img.Manifest), manifestDesc, content.WithLabels(manifestLabels)); err != nil {
		if !errdefs.IsAlreadyExists(err) {
			return fmt.Errorf("writing manifest blob: %w", err)
		}
		fieldpaths := make([]string, 0, len(manifestLabels))
		for k := range manifestLabels {
			fieldpaths = append(fieldpaths, "labels."+k)
		}
		if _, err := cs.Update(ctx, content.Info{Digest: manifestDigest, Labels: manifestLabels}, fieldpaths...); err != nil {
			return fmt.Errorf("labelling existing manifest blob: %w", err)
		}
	}

	labels := map[string]string{labelKeyAppID: img.AppID}
	addReleaseIdentity(labels, img.ManifestDigest, &img.ReleaseID)
	record := images.Image{Name: ReleaseImageName(img.AppID, img.ReleaseID), Target: manifestDesc, Labels: labels}
	is := c.client.ImageService()
	if _, err := is.Create(ctx, record); err != nil {
		if !errdefs.IsAlreadyExists(err) {
			return fmt.Errorf("creating image %q: %w", record.Name, err)
		}
		if _, err := is.Update(ctx, record, "target", "labels"); err != nil {
			return fmt.Errorf("updating image %q: %w", record.Name, err)
		}
	}
	c.logger.Info("Imported registry release",
		zap.String("name", record.Name), zap.String("app_id", img.AppID),
		zap.String("release_id", img.ReleaseID), zap.String("manifest_digest", img.ManifestDigest))
	return nil
}

func writeBlobFile(ctx context.Context, cs content.Store, path string, desc ocispec.Descriptor) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	err = content.WriteBlob(ctx, cs, desc.Digest.String(), f, desc)
	if err != nil && !errdefs.IsAlreadyExists(err) {
		return fmt.Errorf("writing blob %s: %w", desc.Digest, err)
	}
	return nil
}
