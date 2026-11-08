//go:build linux

package containerd

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/core/snapshots"
	"github.com/containerd/containerd/v2/pkg/oci"
	"github.com/google/uuid"
	digest "github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/identity"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"go.uber.org/zap/zaptest"

	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// These tests unpack images through a live containerd and mount the
// resulting snapshots, so they need root and a working overlayfs snapshotter.
// Each test uses a fresh namespace and leaves its snapshots behind, so point
// the variable at a disposable containerd:
//
//	WENDY_CONTAINERD_INTEGRATION=/run/containerd/containerd.sock go test -run Integration ./internal/agent/containerd
//
// WDY-3799: on containerd 1.7, whose snapshot service ignores the parent on
// Commit, every layer above the base was committed with no parent, so a
// container rootfs contained only the image's top layer.
const containerdIntegrationEnv = "WENDY_CONTAINERD_INTEGRATION"

// integrationLayerFiles holds one file per image layer, base first. A rootfs
// built on the image's top layer must contain all of them.
var integrationLayerFiles = []string{"base.txt", "middle.txt", "top.txt"}

type integrationImage struct {
	name     string
	headers  []*agentpb.RunContainerLayerHeader
	config   []byte
	chainIDs []string
}

func (img integrationImage) topChainID() string {
	return img.chainIDs[len(img.chainIDs)-1]
}

func newIntegrationClient(t *testing.T) (*Client, context.Context) {
	t.Helper()
	address := os.Getenv(containerdIntegrationEnv)
	if address == "" {
		t.Skipf("set %s to a containerd socket to run", containerdIntegrationEnv)
	}
	if os.Geteuid() != 0 {
		t.Fatal("integration socket was configured, but mounting snapshots requires root")
	}
	cc, err := containerd.New(address)
	if err != nil {
		t.Fatalf("connecting to containerd: %v", err)
	}
	t.Cleanup(func() { _ = cc.Close() })
	c := &Client{
		client:      cc,
		logger:      zaptest.NewLogger(t),
		namespace:   "wdy3799-" + uuid.NewString()[:8],
		snapshotter: "overlayfs",
	}
	t.Cleanup(c.discardAllPreparedSnapshots)
	return c, c.withNamespace(context.Background())
}

func singleFileTar(t *testing.T, name string) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(name))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(name)); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// newIntegrationImage writes one uncompressed layer per integrationLayerFiles
// entry to the content store, the way a chunked deploy leaves them for
// PrepareImage.
func newIntegrationImage(t *testing.T, c *Client, ctx context.Context) integrationImage {
	t.Helper()
	img := integrationImage{name: "wdy3799-" + uuid.NewString()[:8] + ":latest"}
	var diffIDs []digest.Digest
	for _, file := range integrationLayerFiles {
		blob := singleFileTar(t, file)
		dgst := digest.FromBytes(blob)
		if err := c.WriteLayer(ctx, dgst.String(), bytes.NewReader(blob), int64(len(blob))); err != nil {
			t.Fatalf("writing layer %s: %v", file, err)
		}
		img.headers = append(img.headers, &agentpb.RunContainerLayerHeader{
			Digest:      dgst.String(),
			DiffId:      dgst.String(),
			Size:        int64(len(blob)),
			Compression: agentpb.RunContainerLayerHeader_COMPRESSION_NONE,
		})
		diffIDs = append(diffIDs, dgst)
	}
	for i := range diffIDs {
		img.chainIDs = append(img.chainIDs, identity.ChainID(diffIDs[:i+1]).String())
	}
	config, err := json.Marshal(ocispec.Image{Platform: ocispec.Platform{OS: "linux", Architecture: runtime.GOARCH}})
	if err != nil {
		t.Fatal(err)
	}
	img.config = config
	return img
}

// assembleIntegrationImage registers the image record that RunContainer's
// assemble step creates and returns it as CreateContainer looks it up.
func assembleIntegrationImage(t *testing.T, c *Client, ctx context.Context, img integrationImage) containerd.Image {
	t.Helper()
	if err := c.AssembleImage(ctx, img.name, img.headers, img.config); err != nil {
		t.Fatalf("assembling image: %v", err)
	}
	image, err := c.client.GetImage(ctx, normalizeImageName(img.name))
	if err != nil {
		t.Fatalf("loading image: %v", err)
	}
	return image
}

// detachLayers commits every layer under its chain ID without a parent,
// reproducing what agents from 2026.08.13 left on containerd 1.7.
func detachLayers(t *testing.T, c *Client, ctx context.Context, img integrationImage) {
	t.Helper()
	ctx, done, err := c.client.WithLease(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = done(context.WithoutCancel(ctx)) }()
	sn := c.client.SnapshotService(c.snapshotter)
	for i, h := range img.headers {
		key := "detach-" + uuid.NewString()
		mounts, err := sn.Prepare(ctx, key, "")
		if err != nil {
			t.Fatalf("preparing detached layer %d: %v", i, err)
		}
		desc := ocispec.Descriptor{MediaType: ocispec.MediaTypeImageLayer, Digest: digest.Digest(h.Digest), Size: h.Size}
		if _, err := c.client.DiffService().Apply(ctx, desc, mounts); err != nil {
			t.Fatalf("applying detached layer %d: %v", i, err)
		}
		labels := map[string]string{
			labelKeyGCRoot:        gcTimestamp(),
			labelKeyWendySnapshot: "true",
			labelKeyWendyDiffID:   h.DiffId,
		}
		if err := sn.Commit(ctx, img.chainIDs[i], key, snapshots.WithLabels(labels)); err != nil {
			t.Fatalf("committing detached layer %d: %v", i, err)
		}
	}
}

// requireRootfsFiles mounts a rootfs and fails unless every layer's file is
// visible in it.
func requireRootfsFiles(t *testing.T, ctx context.Context, mounts []mount.Mount) {
	t.Helper()
	var missing []string
	err := mount.WithTempMount(ctx, mounts, func(root string) error {
		for _, f := range integrationLayerFiles {
			if _, err := os.Stat(filepath.Join(root, f)); err != nil {
				missing = append(missing, f)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("mounting rootfs: %v", err)
	}
	if len(missing) > 0 {
		t.Fatalf("rootfs is missing %v: layers are not stacked", missing)
	}
}

// requireImageRootfs checks that imageUnpacked reports the image ready, so
// the next create reuses its snapshots as they are, and that the rootfs a new
// container gets from WithNewSnapshot, which stacks a snapshot on the image's
// top chain ID, holds every layer.
func requireImageRootfs(t *testing.T, c *Client, ctx context.Context, img integrationImage) {
	t.Helper()
	image, err := c.client.GetImage(ctx, normalizeImageName(img.name))
	if err != nil {
		t.Fatalf("loading image: %v", err)
	}
	unpacked, err := c.imageUnpacked(ctx, image)
	if err != nil {
		t.Fatalf("checking unpack state: %v", err)
	}
	if !unpacked {
		t.Fatal("imageUnpacked = false for a correctly stacked image")
	}
	sn := c.client.SnapshotService(c.snapshotter)
	key := "verify-" + uuid.NewString()
	mounts, err := sn.View(ctx, key, img.topChainID())
	if err != nil {
		t.Fatalf("viewing top layer snapshot: %v", err)
	}
	defer func() { _ = sn.Remove(ctx, key) }()
	requireRootfsFiles(t, ctx, mounts)
}

// ensureUnpacked runs CreateContainer's check-then-unpack step.
func ensureUnpacked(t *testing.T, c *Client, ctx context.Context, image containerd.Image) {
	t.Helper()
	unpacked, err := c.imageUnpacked(ctx, image)
	if err != nil {
		t.Fatalf("checking unpack state: %v", err)
	}
	if !unpacked {
		if err := c.UnpackImage(ctx, image, nil); err != nil {
			t.Fatalf("unpacking image: %v", err)
		}
	}
}

func TestIntegrationUnpackImageStacksLayers(t *testing.T) {
	c, ctx := newIntegrationClient(t)
	img := newIntegrationImage(t, c, ctx)
	image := assembleIntegrationImage(t, c, ctx, img)

	ensureUnpacked(t, c, ctx, image)

	requireImageRootfs(t, c, ctx, img)
}

func TestIntegrationUnpackRepairsDetachedLayers(t *testing.T) {
	c, ctx := newIntegrationClient(t)
	img := newIntegrationImage(t, c, ctx)
	image := assembleIntegrationImage(t, c, ctx, img)
	detachLayers(t, c, ctx, img)

	ensureUnpacked(t, c, ctx, image)

	requireImageRootfs(t, c, ctx, img)
}

func TestIntegrationUnpackNamesContainerBlockingRepair(t *testing.T) {
	c, ctx := newIntegrationClient(t)
	img := newIntegrationImage(t, c, ctx)
	image := assembleIntegrationImage(t, c, ctx, img)
	detachLayers(t, c, ctx, img)

	// Another app built from identical layers still runs on the detached top
	// layer, from a rootfs staged by PrepareImage.
	lctx, done, err := c.client.WithLease(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.client.SnapshotService(c.snapshotter).Prepare(lctx, "wendy-prepared-sibling", img.topChainID()); err != nil {
		t.Fatalf("preparing sibling rootfs: %v", err)
	}
	if _, err := c.client.NewContainer(lctx, "sibling-app",
		containerd.WithSnapshotter(c.snapshotter),
		containerd.WithSnapshot("wendy-prepared-sibling"),
		containerd.WithSpec(&oci.Spec{}),
	); err != nil {
		t.Fatalf("creating sibling container: %v", err)
	}
	_ = done(context.WithoutCancel(ctx))

	err = c.UnpackImage(ctx, image, nil)
	if err == nil || !strings.Contains(err.Error(), "sibling-app") {
		t.Fatalf("UnpackImage error = %v; want it to name the container still using the layers", err)
	}
}

func TestIntegrationPrepareImageStacksLayers(t *testing.T) {
	c, ctx := newIntegrationClient(t)
	img := newIntegrationImage(t, c, ctx)

	if err := c.PrepareImage(ctx, img.name, img.headers, img.config); err != nil {
		t.Fatalf("PrepareImage: %v", err)
	}

	requireImageRootfs(t, c, ctx, img)
}

func TestIntegrationPrepareImageDoesNotStageRootfsOnDetachedLayers(t *testing.T) {
	c, ctx := newIntegrationClient(t)
	img := newIntegrationImage(t, c, ctx)
	detachLayers(t, c, ctx, img)

	// Compose requires PrepareImage to succeed: it registers the image that
	// containers are then created from by name.
	if err := c.PrepareImage(ctx, img.name, img.headers, img.config); err != nil {
		t.Fatalf("PrepareImage: %v", err)
	}

	// CreateContainer uses a staged rootfs as-is, without unpacking.
	if prepared := c.takePreparedSnapshot(normalizeImageName(img.name), img.topChainID()); prepared != nil {
		defer prepared.discard()
		mounts, err := c.client.SnapshotService(c.snapshotter).Mounts(ctx, prepared.key)
		if err != nil {
			t.Fatalf("reading staged rootfs mounts: %v", err)
		}
		requireRootfsFiles(t, ctx, mounts)
	}

	// Without a staged rootfs, create repairs the image before using it.
	image, err := c.client.GetImage(ctx, normalizeImageName(img.name))
	if err != nil {
		t.Fatalf("loading image: %v", err)
	}
	if unpacked, err := c.imageUnpacked(ctx, image); err != nil || unpacked {
		t.Fatalf("imageUnpacked = %v, %v after PrepareImage left detached layers; want false", unpacked, err)
	}
	ensureUnpacked(t, c, ctx, image)
	requireImageRootfs(t, c, ctx, img)
}
