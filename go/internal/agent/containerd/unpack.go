package containerd

import (
	"context"
	"fmt"
	"time"

	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/core/leases"
	"github.com/containerd/containerd/v2/core/snapshots"
	"github.com/containerd/errdefs"
	"github.com/google/uuid"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"go.uber.org/zap"
)

// unpackLeaseExpiration bounds how long the unpack lease keeps freshly created
// snapshots and content alive. It must be long enough to apply the largest
// expected image, but short enough that a lease orphaned by a crashed agent
// doesn't pin disk for too long. The expiration is a backstop only — the
// happy path releases the lease on return.
const unpackLeaseExpiration = 30 * time.Minute

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

// UnpackImage unpacks an image's layers into the snapshotter so that the
// resulting chain-ID snapshots are present for a subsequent
// `WithNewSnapshot` call to build a container rootfs from. It computes chain
// IDs for each layer and creates snapshots incrementally, reusing existing
// snapshots when possible.
//
// The progress callback, if non-nil, is invoked for each phase of the unpack
// operation to allow callers to report progress upstream.
//
// The unpack runs inside a containerd lease and tags each committed snapshot
// with a `containerd.io/gc.root` label. These work as a pair: the lease pins
// content and active snapshots while the unpack is in progress, and the
// gc.root label on each committed chain-ID snapshot keeps it alive after the
// lease releases. Without both, containerd's metadata GC could reap a
// freshly committed chain-ID snapshot before the next layer's `Prepare`
// references it as a parent — surfacing as a random-layer "parent snapshot
// does not exist" failure.
func (c *Client) UnpackImage(ctx context.Context, imageName string, progress func(UnpackProgress)) error {
	ctx = c.withNamespace(ctx)

	// cleanupCtx is used for best-effort `Remove` calls so a cancelled caller
	// doesn't also cancel cleanup. The namespace is required by containerd
	// services even off the happy path.
	cleanupCtx := c.withNamespace(context.Background())

	ctx, doneLease, err := c.client.WithLease(ctx, leases.WithExpiration(unpackLeaseExpiration))
	if err != nil {
		return fmt.Errorf("creating unpack lease: %w", err)
	}
	defer func() {
		if err := doneLease(cleanupCtx); err != nil {
			c.logger.Warn("Failed to release unpack lease; relying on expiration backstop",
				zap.Duration("expiration", unpackLeaseExpiration),
				zap.Error(err),
			)
		}
	}()

	cs := c.client.ContentStore()
	sn := c.client.SnapshotService("")

	img, err := c.client.GetImage(ctx, imageName)
	if err != nil {
		return fmt.Errorf("getting image %q: %w", imageName, err)
	}

	// Resolve through index if needed (platform selection).
	manifest, err := images.Manifest(ctx, cs, img.Target(), img.Platform())
	if err != nil {
		return fmt.Errorf("reading manifest for %q: %w", imageName, err)
	}

	totalLayers := len(manifest.Layers)
	if progress != nil {
		progress(UnpackProgress{Phase: "start", TotalLayers: totalLayers})
	}

	var parentChainID string
	for i, layerDesc := range manifest.Layers {
		diffID, err := layerDiffID(ctx, cs, layerDesc)
		if err != nil {
			return fmt.Errorf("getting diff ID for layer %d: %w", i, err)
		}

		chainID := computeChainID(parentChainID, diffID)

		if _, err := sn.Stat(ctx, chainID); err == nil {
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
		} else if !errdefs.IsNotFound(err) {
			return fmt.Errorf("stat snapshot %q: %w", chainID, err)
		}

		// Unique per-attempt active key so concurrent unpacks of the same
		// image (or a stale key from a crashed prior attempt) can't collide
		// on the AlreadyExists path and clobber each other's in-progress
		// snapshot. The lease pins the active snapshot during this loop
		// iteration; only the committed chain-ID snapshot needs gc.root
		// to survive lease release.
		activeKey := fmt.Sprintf("extract-%s-%d-%s", imageName, i, uuid.NewString())
		mounts, err := sn.Prepare(ctx, activeKey, parentChainID)
		if err != nil {
			return fmt.Errorf("preparing snapshot for layer %d: %w", i, err)
		}

		if _, err := c.client.DiffService().Apply(ctx, layerDesc, mounts); err != nil {
			c.removeActiveSnapshot(cleanupCtx, sn, activeKey, "active snapshot after apply failure", i)
			return fmt.Errorf("applying layer %d: %w", i, err)
		}

		gcRootOpt := snapshots.WithLabels(map[string]string{
			labelKeyGCRoot: gcTimestamp(),
		})
		commitErr := sn.Commit(ctx, chainID, activeKey, gcRootOpt)
		switch {
		case commitErr == nil:
			c.logger.Debug("Unpacked layer",
				zap.Int("layer", i),
				zap.String("chain_id", chainID),
				zap.Int64("size", layerDesc.Size),
			)
			if progress != nil {
				progress(UnpackProgress{
					Phase:       "layer",
					LayerIndex:  i,
					TotalLayers: totalLayers,
					LayerSize:   layerDesc.Size,
					Reused:      false,
				})
			}
		case errdefs.IsAlreadyExists(commitErr):
			// A concurrent unpack committed the same chain ID first. Our
			// active key still exists; clean it up and report the layer
			// as reused rather than freshly unpacked.
			c.removeActiveSnapshot(cleanupCtx, sn, activeKey, "active snapshot after concurrent commit", i)
			if progress != nil {
				progress(UnpackProgress{
					Phase:       "layer",
					LayerIndex:  i,
					TotalLayers: totalLayers,
					LayerSize:   layerDesc.Size,
					Reused:      true,
				})
			}
		default:
			return fmt.Errorf("committing snapshot for layer %d: %w", i, commitErr)
		}

		parentChainID = chainID
	}

	if progress != nil {
		progress(UnpackProgress{Phase: "complete", TotalLayers: totalLayers})
	}

	return nil
}

// removeActiveSnapshot deletes an active snapshot key as part of error recovery,
// logging at Warn for any failure other than NotFound (which is benign — the
// key was never created or has already been swept).
func (c *Client) removeActiveSnapshot(ctx context.Context, sn snapshots.Snapshotter, key, reason string, layer int) {
	if err := sn.Remove(ctx, key); err != nil && !errdefs.IsNotFound(err) {
		c.logger.Warn("Failed to remove active snapshot",
			zap.String("active_key", key),
			zap.String("reason", reason),
			zap.Int("layer", layer),
			zap.Error(err),
		)
	}
}

// layerDiffID resolves the uncompressed diff ID for a layer descriptor.
func layerDiffID(ctx context.Context, cs content.Store, desc ocispec.Descriptor) (string, error) {
	diffID, err := images.GetDiffID(ctx, cs, desc)
	if err != nil {
		return "", err
	}
	return diffID.String(), nil
}
