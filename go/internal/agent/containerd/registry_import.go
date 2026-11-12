package containerd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/containerd/containerd/v2/core/containers"
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
	return releaseImagePrefix + appID + ":" + releaseID
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
	return withImportLease(ctx, cleanupCtx, c.client.LeasesService(), c.logger, func(ctx context.Context) error {
		return c.importRelease(ctx, img, manifest, manifestDigest)
	})
}

// withImportLease runs fn under a lease that roots what it writes, then
// deletes the lease with a synchronous garbage collection: whatever no image
// record took over (a cancelled or failed import) is collected before it
// returns, so no decrypted layer stays in the content store. A kill leaves
// the lease to its expiration (assembleLeaseExpiration).
func withImportLease(ctx, cleanupCtx context.Context, lm leaseGCer, logger *zap.Logger, fn func(context.Context) error) error {
	l, err := lm.Create(ctx, leases.WithRandomID(), leases.WithExpiration(assembleLeaseExpiration))
	if err != nil {
		return fmt.Errorf("creating import lease: %w", err)
	}
	defer func() {
		if err := lm.Delete(cleanupCtx, l, leases.SynchronousDelete); err != nil {
			logger.Warn("Failed to release import lease; relying on expiration backstop", zap.Error(err))
		}
	}()
	return fn(leases.WithLease(ctx, l.ID))
}

func (c *Client) importRelease(ctx context.Context, img services.ReleaseImage, manifest ocispec.Manifest, manifestDigest digest.Digest) error {
	cs := c.client.ContentStore()
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

// The postbox finds these through an interface check; this keeps them in
// step with it.
var _ interface {
	ReleaseApps(ctx context.Context, app string, keep []string) ([]string, error)
	ReleasesInUse(ctx context.Context, app string) ([]string, error)
	DeleteReleaseImages(ctx context.Context, app string, keep []string) error
} = (*Client)(nil)

// releaseImagePrefix starts every ReleaseImageName.
const releaseImagePrefix = "localhost/wendy-release/"

// removableRelease reports whether image name is a release of app (any app's
// when app is "") whose release id is not in keep.
func removableRelease(name, app string, keep []string) bool {
	rest, ok := strings.CutPrefix(name, releaseImagePrefix)
	if !ok {
		return false
	}
	owner, release, ok := strings.Cut(rest, ":")
	return ok && (app == "" || owner == app) && !slices.Contains(keep, release)
}

// ReleaseApps lists the apps that have a container running a removable
// release (removableRelease); the caller deletes them through the agent's
// app delete path before DeleteReleaseImages.
func (c *Client) ReleaseApps(ctx context.Context, app string, keep []string) ([]string, error) {
	ctrs, err := c.client.ContainerService().List(c.withNamespace(ctx), fmt.Sprintf("labels.%q", labelKeyAppID))
	if err != nil {
		return nil, fmt.Errorf("listing containers: %w", err)
	}
	return releaseApps(ctrs, app, keep), nil
}

// ReleasesInUse lists the release ids of app's releases that a container
// was created from, so pruning superseded releases never takes one from
// under its app.
func (c *Client) ReleasesInUse(ctx context.Context, app string) ([]string, error) {
	ctrs, err := c.client.ContainerService().List(c.withNamespace(ctx), fmt.Sprintf("labels.%q", labelKeyAppID))
	if err != nil {
		return nil, fmt.Errorf("listing containers: %w", err)
	}
	return releasesInUse(ctrs, app), nil
}

func releasesInUse(ctrs []containers.Container, app string) []string {
	var used []string
	for _, ctr := range ctrs {
		rest, ok := strings.CutPrefix(ctr.Image, releaseImagePrefix+app+":")
		if ok && !slices.Contains(used, rest) {
			used = append(used, rest)
		}
	}
	return used
}

func releaseApps(ctrs []containers.Container, app string, keep []string) []string {
	var apps []string
	for _, ctr := range ctrs {
		if owner := ctr.Labels[labelKeyAppID]; removableRelease(ctr.Image, app, keep) && !slices.Contains(apps, owner) {
			apps = append(apps, owner)
		}
	}
	return apps
}

// DeleteReleaseImages deletes the removable release images and collects
// their content before returning, so no decrypted layer stays on disk.
func (c *Client) DeleteReleaseImages(ctx context.Context, app string, keep []string) error {
	return deleteReleaseImages(c.withNamespace(ctx), c.client.ImageService(), c.logger, app, keep)
}

func deleteReleaseImages(ctx context.Context, is images.Store, logger *zap.Logger, app string, keep []string) error {
	imgs, err := is.List(ctx)
	if err != nil {
		return fmt.Errorf("listing images: %w", err)
	}
	var errs []error
	for _, img := range imgs {
		if !removableRelease(img.Name, app, keep) {
			continue
		}
		if err := is.Delete(ctx, img.Name, images.SynchronousDelete()); err != nil && !errdefs.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("deleting release image %q: %w", img.Name, err))
			continue
		}
		logger.Info("Removed registry release", zap.String("name", img.Name))
	}
	return errors.Join(errs...)
}
