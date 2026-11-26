package containerd

import (
	"context"
	"fmt"
	"strings"

	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/core/snapshots"
	"github.com/containerd/errdefs"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// snapshotWalker is the small part of snapshots.Snapshotter needed to locate
// a previously materialized copy of one parent-independent layer.
type snapshotWalker interface {
	Walk(context.Context, snapshots.WalkFunc, ...string) error
}

// layerReuseSnapshotter is the snapshot API needed by the overlayfs rebase
// fast path. Keeping it narrow makes the selection and fallback behavior
// independently testable without a live containerd daemon.
type layerReuseSnapshotter interface {
	snapshotWalker
	Stat(context.Context, string) (snapshots.Info, error)
	View(context.Context, string, string, ...snapshots.Opt) ([]mount.Mount, error)
	Prepare(context.Context, string, string, ...snapshots.Opt) ([]mount.Mount, error)
	Commit(context.Context, string, string, ...snapshots.Opt) error
	Remove(context.Context, string) error
	Update(context.Context, snapshots.Info, ...string) (snapshots.Info, error)
}

// findReusableLayerSnapshot returns a committed Wendy snapshot whose own
// upper directory is exactly diffID. Snapshot names are chain IDs and therefore
// parent-dependent; the explicit label is what makes this lookup sound.
func findReusableLayerSnapshot(ctx context.Context, sn snapshotWalker, diffID, exclude string) (string, error) {
	var found string
	filter := fmt.Sprintf(`kind==committed,labels.%q==%s`, labelKeyWendyDiffID, diffID)
	err := sn.Walk(ctx, func(_ context.Context, info snapshots.Info) error {
		if found != "" || info.Kind != snapshots.KindCommitted || info.Name == exclude {
			return nil
		}
		if info.Labels[labelKeyWendySnapshot] == "true" && info.Labels[labelKeyWendyDiffID] == diffID {
			found = info.Name
		}
		return nil
	}, filter)
	return found, err
}

// snapshotUpperPath extracts the top immutable upper directory exposed by an
// overlay snapshot view. A one-layer view is a bind mount. A deeper view is an
// overlay whose first lowerdir is the selected snapshot's own upper directory.
// Active overlay mounts are rejected because their upperdir is mutable.
func snapshotUpperPath(mounts []mount.Mount) (string, bool) {
	if len(mounts) != 1 {
		return "", false
	}
	m := mounts[0]
	switch m.Type {
	case "bind":
		return m.Source, m.Source != ""
	case "overlay":
		for _, opt := range m.Options {
			if strings.HasPrefix(opt, "upperdir=") || strings.HasPrefix(opt, "workdir=") {
				return "", false
			}
		}
		for _, opt := range m.Options {
			if value, ok := strings.CutPrefix(opt, "lowerdir="); ok {
				upper, _, _ := strings.Cut(value, ":")
				return upper, upper != ""
			}
		}
	}
	return "", false
}

// standaloneLayerApplyMounts mirrors containerd's parallel unpack path. A
// parentless overlay snapshot is exposed as a bind mount, but applying an OCI
// tar through an overlay mount is required for correct whiteout conversion.
// The resulting upper directory is a parent-independent representation of the
// layer and can therefore be safely rebased later.
func standaloneLayerApplyMounts(mounts []mount.Mount) []mount.Mount {
	if len(mounts) != 1 || mounts[0].Type != "bind" {
		return mounts
	}
	m := mount.Mount{Type: "overlay", Source: "overlay"}
	for _, opt := range mounts[0].Options {
		if opt != "rbind" {
			m.Options = append(m.Options, opt)
		}
	}
	m.Options = append(m.Options, "upperdir="+mounts[0].Source)
	return []mount.Mount{m}
}

// tryReuseLayerSnapshot creates chainID by hard-linking an immutable overlayfs
// layer upper directory that was already extracted for another parent, then
// commits it with parentChainID. OCI ordering remains represented by chainID;
// only the physical layer files are shared.
//
// The optimization is deliberately best-effort. Any unsupported snapshotter,
// rebase restriction, stale cache entry, or filesystem error returns false so
// the caller can replay the canonical OCI tar through DiffService.Apply.
func (c *Client) tryReuseLayerSnapshot(
	ctx, cleanupCtx context.Context,
	sn layerReuseSnapshotter,
	imageName string,
	layer int,
	diffID, parentChainID, chainID string,
) bool {
	if c.snapshotter != "overlayfs" || diffID == "" {
		return false
	}

	candidate, err := findReusableLayerSnapshot(ctx, sn, diffID, chainID)
	if err != nil || candidate == "" {
		if err != nil {
			c.logger.Debug("Layer snapshot reuse lookup failed; applying layer normally",
				zap.Int("layer", layer), zap.String("diff_id", diffID), zap.Error(err))
		}
		return false
	}

	viewKey := fmt.Sprintf("reuse-view-%s-%d-%s", imageName, layer, uuid.NewString())
	viewMounts, err := sn.View(ctx, viewKey, candidate)
	if err != nil {
		return false
	}
	defer func() {
		ctx, cancel := context.WithTimeout(cleanupCtx, unpackCleanupTimeout)
		defer cancel()
		if err := sn.Remove(ctx, viewKey); err != nil && !errdefs.IsNotFound(err) {
			c.logger.Debug("Failed to remove layer-reuse view", zap.String("key", viewKey), zap.Error(err))
		}
	}()
	source, ok := snapshotUpperPath(viewMounts)
	if !ok {
		return false
	}

	activeKey := fmt.Sprintf("reuse-%s-%d-%s", imageName, layer, uuid.NewString())
	activeMounts, err := sn.Prepare(ctx, activeKey, "")
	if err != nil {
		return false
	}
	committed := false
	defer func() {
		if !committed {
			ctx, cancel := context.WithTimeout(cleanupCtx, unpackCleanupTimeout)
			defer cancel()
			if err := sn.Remove(ctx, activeKey); err != nil && !errdefs.IsNotFound(err) {
				c.logger.Warn("Failed to remove active layer-reuse snapshot",
					zap.String("active_key", activeKey), zap.Int("layer", layer), zap.Error(err))
			}
		}
	}()
	target, ok := snapshotUpperPath(activeMounts)
	if !ok {
		return false
	}

	if err := cloneSnapshotUpper(target, source); err != nil {
		c.logger.Debug("Layer snapshot clone failed; applying layer normally",
			zap.Int("layer", layer), zap.String("diff_id", diffID), zap.Error(err))
		return false
	}

	labels := map[string]string{
		labelKeyGCRoot:        gcTimestamp(),
		labelKeyWendySnapshot: "true",
		labelKeyWendyDiffID:   diffID,
	}
	err = sn.Commit(ctx, chainID, activeKey,
		snapshots.WithParent(parentChainID),
		snapshots.WithLabels(labels),
	)
	switch {
	case err == nil:
		committed = true
	case errdefs.IsAlreadyExists(err):
		// Another concurrent preparation won. Our active clone is discarded and
		// the existing ordered chain is the desired result.
		return true
	default:
		c.logger.Debug("Layer snapshot rebase unavailable; applying layer normally",
			zap.Int("layer", layer), zap.String("diff_id", diffID), zap.Error(err))
		return false
	}

	// Refresh the source root as well: it is now a useful physical cache entry
	// even if no current image still references its old ordered chain.
	c.refreshSnapshotLabels(ctx, sn, candidate)
	c.logger.Debug("Reused extracted layer across parent chains",
		zap.Int("layer", layer),
		zap.String("diff_id", diffID),
		zap.String("source_chain_id", candidate),
		zap.String("chain_id", chainID),
	)
	return true
}

// refreshSnapshotLabels moves the cache root timestamp of an existing Wendy
// snapshot forward. Crucially, it does not add labelKeyWendyDiffID: legacy
// snapshots were extracted against a parent and are not safe rebase sources.
// The refresh is best-effort and never decides image correctness.
func (c *Client) refreshSnapshotLabels(ctx context.Context, sn interface {
	Stat(context.Context, string) (snapshots.Info, error)
	Update(context.Context, snapshots.Info, ...string) (snapshots.Info, error)
}, chainID string) {
	info, err := sn.Stat(ctx, chainID)
	if err != nil {
		return
	}
	if info.Labels[labelKeyWendySnapshot] != "true" {
		return
	}
	if info.Labels == nil {
		info.Labels = make(map[string]string)
	}
	info.Labels[labelKeyGCRoot] = gcTimestamp()
	if _, err := sn.Update(ctx, info, "labels"); err != nil {
		c.logger.Debug("Failed to refresh reusable layer snapshot labels",
			zap.String("chain_id", chainID), zap.Error(err))
	}
}
