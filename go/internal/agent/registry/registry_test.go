package registry

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/core/leases"
	"github.com/containerd/errdefs"
	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// testImageStore is a minimal in-memory images.Store for testing.
type testImageStore struct {
	images.Store
	mu     sync.Mutex
	store  map[string]images.Image
}

func newTestImageStore() *testImageStore {
	return &testImageStore{store: make(map[string]images.Image)}
}

func (s *testImageStore) Get(_ context.Context, name string) (images.Image, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	img, ok := s.store[name]
	if !ok {
		return images.Image{}, errdefs.ErrNotFound
	}
	return img, nil
}

func (s *testImageStore) List(_ context.Context, _ ...string) ([]images.Image, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]images.Image, 0, len(s.store))
	for _, img := range s.store {
		out = append(out, img)
	}
	return out, nil
}

func (s *testImageStore) Create(_ context.Context, img images.Image) (images.Image, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.store[img.Name]; ok {
		return images.Image{}, errdefs.ErrAlreadyExists
	}
	s.store[img.Name] = img
	return img, nil
}

func (s *testImageStore) Update(_ context.Context, img images.Image, fieldpaths ...string) (images.Image, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	existing, ok := s.store[img.Name]
	if !ok {
		return images.Image{}, errdefs.ErrNotFound
	}
	if len(fieldpaths) == 0 {
		s.store[img.Name] = img
		return img, nil
	}
	for _, fp := range fieldpaths {
		if fp == "target" {
			existing.Target = img.Target
		}
	}
	s.store[img.Name] = existing
	return existing, nil
}

func (s *testImageStore) Delete(_ context.Context, name string, _ ...images.DeleteOpt) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.store[name]; !ok {
		return errdefs.ErrNotFound
	}
	delete(s.store, name)
	return nil
}

// testContentStore is a minimal content.Store for testing.
// Abort always returns NotFound (no in-progress ingest).
// Writer always returns AlreadyExists (simulating content already present).
type testContentStore struct {
	content.Store
}

func (s *testContentStore) Abort(_ context.Context, _ string) error {
	return errdefs.ErrNotFound
}

func (s *testContentStore) Writer(_ context.Context, _ ...content.WriterOpt) (content.Writer, error) {
	return nil, errdefs.ErrAlreadyExists
}

// testLeasesManager is a no-op leases.Manager for testing.
type testLeasesManager struct {
	leases.Manager
}

func (m *testLeasesManager) Create(_ context.Context, opts ...leases.Opt) (leases.Lease, error) {
	l := leases.Lease{ID: "test-lease", CreatedAt: time.Now()}
	for _, opt := range opts {
		if err := opt(&l); err != nil {
			return leases.Lease{}, err
		}
	}
	return l, nil
}

func (m *testLeasesManager) Delete(_ context.Context, _ leases.Lease, _ ...leases.DeleteOpt) error {
	return nil
}

func (m *testLeasesManager) List(_ context.Context, _ ...string) ([]leases.Lease, error) {
	return nil, nil
}

func (m *testLeasesManager) AddResource(_ context.Context, _ leases.Lease, _ leases.Resource) error {
	return nil
}

func (m *testLeasesManager) DeleteResource(_ context.Context, _ leases.Lease, _ leases.Resource) error {
	return nil
}

func (m *testLeasesManager) ListResources(_ context.Context, _ leases.Lease) ([]leases.Resource, error) {
	return nil, nil
}

// newTestRegistry creates a containerdRegistry backed by in-memory test stores.
// This avoids requiring a real containerd socket.
func newTestRegistry(t *testing.T, imgStore *testImageStore) containerdRegistry {
	t.Helper()
	cs := &testContentStore{}
	lm := &testLeasesManager{}

	client, err := containerd.New("",
		containerd.WithDefaultNamespace("default"),
		containerd.WithServices(
			containerd.WithImageStore(imgStore),
			containerd.WithContentStore(cs),
			containerd.WithLeasesService(lm),
		),
	)
	if err != nil {
		t.Fatalf("creating test containerd client: %v", err)
	}
	t.Cleanup(func() { client.Close() })

	return containerdRegistry{
		client:              client,
		imagePrefix:         "localhost:5000/",
		blobLeaseExpiration: time.Minute,
		manifestSizeLimit:   4 * 1024 * 1024,
	}
}

// makeManifest returns a minimal OCI image manifest JSON with the given fake
// config digest so that each call produces a distinct manifest digest.
func makeManifest(configDigest string) []byte {
	return []byte(fmt.Sprintf(
		`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json",`+
			`"config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":%q,"size":1},`+
			`"layers":[]}`,
		configDigest,
	))
}

// TestPushManifestUpdatesImageServiceOnRepush is the regression test for the
// bug where a second push to the embedded registry left the containerd image
// service entry pointing at the OLD manifest, causing CreateContainer to use
// a stale image.
func TestPushManifestUpdatesImageServiceOnRepush(t *testing.T) {
	imgStore := newTestImageStore()
	reg := newTestRegistry(t, imgStore)
	ctx := context.Background()

	const (
		repo    = "wendy-home"
		tag     = "latest"
		mType   = "application/vnd.oci.image.manifest.v1+json"
		imgName = "localhost:5000/wendy-home:latest"
	)

	manifest1 := makeManifest("sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	manifest2 := makeManifest("sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")

	// Sanity check: the two manifests have different digests.
	if digest.FromBytes(manifest1) == digest.FromBytes(manifest2) {
		t.Fatal("test setup error: manifest1 and manifest2 must have different digests")
	}

	// First push registers the image.
	desc1, err := reg.PushManifest(ctx, repo, tag, manifest1, mType)
	if err != nil {
		t.Fatalf("first PushManifest: %v", err)
	}

	img, err := imgStore.Get(ctx, imgName)
	if err != nil {
		t.Fatalf("Get after first push: %v", err)
	}
	if img.Target.Digest != ocispec.Descriptor(desc1).Digest {
		t.Errorf("after first push: image target = %s; want %s", img.Target.Digest, desc1.Digest)
	}

	// Second push must update the image service entry to the new manifest.
	desc2, err := reg.PushManifest(ctx, repo, tag, manifest2, mType)
	if err != nil {
		t.Fatalf("second PushManifest: %v", err)
	}

	img, err = imgStore.Get(ctx, imgName)
	if err != nil {
		t.Fatalf("Get after second push: %v", err)
	}
	if img.Target.Digest != ocispec.Descriptor(desc2).Digest {
		t.Errorf("after second push: image target = %s; want %s (got stale target %s)",
			img.Target.Digest, desc2.Digest, desc1.Digest)
	}
}

// TestPushManifestCreatesImageServiceEntryForNewTag verifies that pushing to a
// new tag creates the image service entry when none exists yet.
func TestPushManifestCreatesImageServiceEntryForNewTag(t *testing.T) {
	imgStore := newTestImageStore()
	reg := newTestRegistry(t, imgStore)
	ctx := context.Background()

	manifest := makeManifest("sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc")
	desc, err := reg.PushManifest(ctx, "new-app", "v1.0", manifest, "application/vnd.oci.image.manifest.v1+json")
	if err != nil {
		t.Fatalf("PushManifest: %v", err)
	}

	img, err := imgStore.Get(ctx, "localhost:5000/new-app:v1.0")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if img.Target.Digest != ocispec.Descriptor(desc).Digest {
		t.Errorf("image target = %s; want %s", img.Target.Digest, desc.Digest)
	}
}

// TestPushManifestNoopForEmptyTag verifies that pushing without a tag does not
// create an image service entry (digest-only push).
func TestPushManifestNoopForEmptyTag(t *testing.T) {
	imgStore := newTestImageStore()
	reg := newTestRegistry(t, imgStore)
	ctx := context.Background()

	manifest := makeManifest("sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd")
	if _, err := reg.PushManifest(ctx, "some-app", "", manifest, "application/vnd.oci.image.manifest.v1+json"); err != nil {
		t.Fatalf("PushManifest: %v", err)
	}

	imgs, _ := imgStore.List(ctx)
	if len(imgs) != 0 {
		t.Errorf("expected no image service entries after tagless push, got %d", len(imgs))
	}
}
