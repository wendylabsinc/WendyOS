package containerd

import (
	"context"
	"fmt"

	"github.com/containerd/containerd/v2/core/leases"
	"github.com/containerd/errdefs"
	"github.com/google/uuid"
	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"go.uber.org/zap"

	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// PrepareImage waits for chunk-backed layers to arrive, reassembles them, and
// applies their snapshots from base to top. It is intended to run concurrently
// with the CLI's WriteChunks calls, so device-side disk and decompression work
// overlaps network transfer. The final image record is assembled before return;
// RunContainer safely repeats all operations through their existing fast paths.
func (c *Client) PrepareImage(ctx context.Context, imageName string, layers []*agentpb.RunContainerLayerHeader, imageConfig []byte) error {
	// Marks the chunk store busy from the moment the CLI starts uploading, so
	// maintenance cannot sweep a staged chunk this preparation will need.
	defer c.chunkActivity.begin()()

	ctx = c.withNamespace(ctx)
	cleanupCtx := c.withNamespace(context.Background())

	ctx, doneLease, err := c.client.WithLease(ctx, leases.WithExpiration(unpackLeaseExpiration))
	if err != nil {
		return fmt.Errorf("creating image preparation lease: %w", err)
	}
	releaseLease := true
	defer func() {
		if releaseLease {
			if err := doneLease(cleanupCtx); err != nil {
				c.logger.Warn("Failed to release image preparation lease; relying on expiration backstop",
					zap.Duration("expiration", unpackLeaseExpiration),
					zap.Error(err),
				)
			}
		}
	}()

	sn := c.client.SnapshotService(c.snapshotter)
	parentChainID := ""
	for i, layer := range layers {
		if layer == nil {
			return fmt.Errorf("layer %d is nil", i)
		}

		diffID := layer.GetDiffId()
		if diffID == "" {
			diffID = layer.GetDigest()
		}
		if _, err := digest.Parse(diffID); err != nil {
			return fmt.Errorf("parsing diff ID for layer %d: %w", i, err)
		}
		chainID := computeChainID(parentChainID, diffID)

		hashes, err := prepareChunkHashes(layer.GetChunkHashes())
		if err != nil {
			return fmt.Errorf("layer %d: %w", i, err)
		}
		if len(hashes) > 0 {
			if err := c.waitForChunks(ctx, hashes); err != nil {
				return fmt.Errorf("waiting for layer %d chunks: %w", i, err)
			}
			if err := c.AssembleLayerFromChunks(ctx, diffID, hashes); err != nil {
				return fmt.Errorf("reassembling layer %d: %w", i, err)
			}
		}

		if _, err := sn.Stat(ctx, chainID); err == nil {
			c.refreshSnapshotLabels(ctx, sn, chainID)
			parentChainID = chainID
			continue
		} else if !errdefs.IsNotFound(err) {
			return fmt.Errorf("checking snapshot for layer %d: %w", i, err)
		}

		layerDigest, err := digest.Parse(layer.GetDigest())
		if err != nil {
			return fmt.Errorf("parsing digest for layer %d: %w", i, err)
		}
		desc := ocispec.Descriptor{
			MediaType: layerMediaType(layer.GetCompression(), layer.GetGzip()),
			Digest:    layerDigest,
			Size:      layer.GetSize(),
		}
		if _, err := c.applyLayerSnapshot(ctx, cleanupCtx, sn, imageName, i, desc, diffID, parentChainID, chainID); err != nil {
			return err
		}
		parentChainID = chainID
	}

	if err := c.AssembleImage(ctx, imageName, layers, imageConfig); err != nil {
		return fmt.Errorf("assembling prepared image: %w", err)
	}

	// Prepare a pristine writable rootfs while the caller is still outside the
	// container replacement window. Failure is deliberately best-effort: all
	// immutable layers and the image record are already prepared, so create can
	// safely fall back to containerd.WithNewSnapshot.
	preparedKey := "wendy-prepared-" + uuid.NewString()
	if _, err := sn.Prepare(ctx, preparedKey, parentChainID); err != nil {
		c.logger.Debug("Could not stage writable rootfs; create will prepare it synchronously",
			zap.String("image", normalizeImageName(imageName)),
			zap.Error(err),
		)
	} else {
		c.storePreparedSnapshot(normalizeImageName(imageName), &preparedSnapshot{
			key:     preparedKey,
			parent:  parentChainID,
			release: func() { _ = doneLease(cleanupCtx) },
			remove:  func() { _ = sn.Remove(cleanupCtx, preparedKey) },
		})
		// The stored snapshot owns this lease until CreateContainer consumes or
		// replaces it. The 30-minute expiration remains a crash/disconnect
		// backstop.
		releaseLease = false
	}
	c.logger.Info("Prepared image before container start",
		zap.String("image", normalizeImageName(imageName)),
		zap.Int("layers", len(layers)),
	)
	return nil
}

func prepareChunkHashes(raw [][]byte) ([][32]byte, error) {
	hashes := make([][32]byte, 0, len(raw))
	for i, b := range raw {
		if len(b) != 32 {
			return nil, fmt.Errorf("chunk hash %d must be 32 bytes, got %d", i, len(b))
		}
		var h [32]byte
		copy(h[:], b)
		hashes = append(hashes, h)
	}
	return hashes, nil
}

func (c *Client) waitForChunks(ctx context.Context, hashes [][32]byte) error {
	return waitForChunksWith(ctx, hashes, c.staging.changes, c.MissingChunks)
}

func waitForChunksWith(ctx context.Context, hashes [][32]byte, changes func() <-chan struct{}, missingChunks func(context.Context, [][32]byte) ([][32]byte, error)) error {
	pending := hashes
	validatingAll := true
	for {
		changed := changes()
		missing, err := missingChunks(ctx, pending)
		if err != nil {
			return err
		}
		if len(missing) == 0 {
			if validatingAll {
				return nil
			}
			// A different layer preparation can consume a shared staged chunk
			// after an earlier check. Revalidate the full layer once at the end
			// so narrowing the hot-path checks never weakens correctness.
			pending = hashes
			validatingAll = true
			continue
		}
		// Only recheck hashes that were still absent. The old loop rescanned
		// every hash after every staged chunk, making a layer with N hashes and
		// M misses perform O(N*M) filesystem/index checks while WriteChunks was
		// trying to feed it. Keeping the shrinking missing set makes progress
		// proportional to the actual delta instead.
		pending = missing
		validatingAll = false
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-changed:
		}
	}
}
