package containerd

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/errdefs"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"go.uber.org/zap"
)

// UnpackProgress reports progress during the image unpack operation.
type UnpackProgress struct {
	// Phase is one of "start", "layer", "complete".
	Phase string
	// LayerIndex is the zero-based index of the current layer being unpacked.
	LayerIndex int
	// TotalLayers is the total number of layers in the image.
	TotalLayers int
	// LayerSize is the compressed size of the current layer in bytes.
	LayerSize int64
	// Reused indicates whether the layer snapshot already existed and was reused.
	Reused bool
}

// UnpackImage unpacks an image's layers into the snapshotter, returning the
// snapshot key that should be used as the container's rootfs. It computes
// chain IDs for each layer and creates snapshots incrementally, reusing
// existing snapshots when possible.
//
// The progress callback, if non-nil, is invoked for each phase of the unpack
// operation to allow callers to report progress upstream.
func (c *Client) UnpackImage(ctx context.Context, imageName string, progress func(UnpackProgress)) (string, error) {
	ctx = c.withNamespace(ctx)
	cs := c.client.ContentStore()
	sn := c.client.SnapshotService("")

	// Look up the image to get the manifest descriptor.
	img, err := c.client.GetImage(ctx, imageName)
	if err != nil {
		return "", fmt.Errorf("getting image %q: %w", imageName, err)
	}

	target := img.Target()

	// Resolve through index if needed (platform selection).
	manifest, err := images.Manifest(ctx, cs, target, img.Platform())
	if err != nil {
		return "", fmt.Errorf("reading manifest for %q: %w", imageName, err)
	}

	totalLayers := len(manifest.Layers)
	if progress != nil {
		progress(UnpackProgress{Phase: "start", TotalLayers: totalLayers})
	}

	// For each layer, compute the chain ID and ensure a committed snapshot exists.
	var parentChainID string
	for i, layerDesc := range manifest.Layers {
		diffID, err := layerDiffID(ctx, cs, layerDesc)
		if err != nil {
			return "", fmt.Errorf("getting diff ID for layer %d: %w", i, err)
		}

		chainID := computeChainID(parentChainID, diffID)
		snapshotKey := chainID

		// Check if the snapshot already exists (committed).
		_, err = sn.Stat(ctx, snapshotKey)
		if err == nil {
			// Snapshot exists, reuse it.
			if progress != nil {
				progress(UnpackProgress{
					Phase:       "layer",
					LayerIndex:  i,
					TotalLayers: totalLayers,
					LayerSize:   layerDesc.Size,
					Reused:      true,
				})
			}
			c.logger.Debug("Reusing existing snapshot",
				zap.Int("layer", i),
				zap.String("chain_id", chainID),
			)
			parentChainID = chainID
			continue
		}

		if !errdefs.IsNotFound(err) {
			return "", fmt.Errorf("stat snapshot %q: %w", snapshotKey, err)
		}

		// Snapshot does not exist; create it by preparing from parent.
		parentKey := ""
		if parentChainID != "" {
			parentKey = parentChainID
		}

		// Prepare the active snapshot (this will be committed).
		activeKey := fmt.Sprintf("extract-%s-%d", imageName, i)
		mounts, err := sn.Prepare(ctx, activeKey, parentKey)
		if err != nil {
			// If the active key already exists (from a previous failed attempt), remove and retry.
			if errdefs.IsAlreadyExists(err) {
				_ = sn.Remove(ctx, activeKey)
				mounts, err = sn.Prepare(ctx, activeKey, parentKey)
			}
			if err != nil {
				return "", fmt.Errorf("preparing snapshot for layer %d: %w", i, err)
			}
		}

		// Apply the layer diff onto the snapshot mounts.
		differ := c.client.DiffService()
		_, err = differ.Apply(ctx, layerDesc, mounts)
		if err != nil {
			_ = sn.Remove(ctx, activeKey)
			return "", fmt.Errorf("applying layer %d: %w", i, err)
		}

		// Commit the snapshot with the chain ID as its key.
		err = sn.Commit(ctx, snapshotKey, activeKey)
		if err != nil {
			if !errdefs.IsAlreadyExists(err) {
				return "", fmt.Errorf("committing snapshot for layer %d: %w", i, err)
			}
			// Another process committed it concurrently; clean up our active key.
			_ = sn.Remove(ctx, activeKey)
		}

		if progress != nil {
			progress(UnpackProgress{
				Phase:       "layer",
				LayerIndex:  i,
				TotalLayers: totalLayers,
				LayerSize:   layerDesc.Size,
				Reused:      false,
			})
		}

		c.logger.Debug("Unpacked layer",
			zap.Int("layer", i),
			zap.String("chain_id", chainID),
			zap.Int64("size", layerDesc.Size),
		)

		parentChainID = chainID
	}

	// Create an ephemeral (active) snapshot for the container's rootfs.
	ephemeralKey := fmt.Sprintf("wendy-%s-%d", imageName, len(manifest.Layers))
	_, err = sn.Prepare(ctx, ephemeralKey, parentChainID)
	if err != nil {
		if errdefs.IsAlreadyExists(err) {
			// Remove stale ephemeral and retry.
			_ = sn.Remove(ctx, ephemeralKey)
			_, err = sn.Prepare(ctx, ephemeralKey, parentChainID)
		}
		if err != nil {
			return "", fmt.Errorf("preparing ephemeral snapshot: %w", err)
		}
	}

	if progress != nil {
		progress(UnpackProgress{Phase: "complete", TotalLayers: totalLayers})
	}

	return ephemeralKey, nil
}

// layerDiffID resolves the uncompressed diff ID for a layer descriptor.
// It first checks the image config's diff_ids via the content store label,
// and falls back to using the images.GetDiffID helper.
func layerDiffID(ctx context.Context, cs content.Store, desc ocispec.Descriptor) (string, error) {
	diffID, err := images.GetDiffID(ctx, cs, desc)
	if err != nil {
		return "", err
	}
	return diffID.String(), nil
}

// images.Manifest resolves a manifest from a descriptor, handling index lookups.
// This is a thin helper to keep the unpack code clean.
func readManifest(ctx context.Context, cs content.Store, desc ocispec.Descriptor) (*ocispec.Manifest, error) {
	p, err := content.ReadBlob(ctx, cs, desc)
	if err != nil {
		return nil, err
	}
	var manifest ocispec.Manifest
	if err := json.Unmarshal(p, &manifest); err != nil {
		return nil, err
	}
	return &manifest, nil
}
