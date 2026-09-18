package registry

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"cuelabs.dev/go/oci/ociregistry"
	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/core/leases"
	"github.com/containerd/errdefs"
	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// testImageStore is a minimal in-memory images.Store for testing.
type testImageStore struct {
	images.Store
	mu    sync.Mutex
	store map[string]images.Image
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

// resumeWriter is a content.Writer whose reported ingest offset is driven by
// its parent store, letting a test simulate a containerd ingest that already
// holds bytes from an earlier (interrupted) upload attempt.
type resumeWriter struct {
	store *resumeContentStore
	buf   bytes.Buffer
}

func (w *resumeWriter) Write(p []byte) (int, error) { return w.buf.Write(p) }
func (w *resumeWriter) Close() error                { return nil }
func (w *resumeWriter) Digest() digest.Digest       { return "" }
func (w *resumeWriter) Truncate(int64) error        { return nil }
func (w *resumeWriter) Status() (content.Status, error) {
	return content.Status{Ref: "resume-ref", Offset: w.store.offset}, nil
}
func (w *resumeWriter) Commit(context.Context, int64, digest.Digest, ...content.Opt) error {
	return nil
}

// resumeContentStore hands out resumeWriters and records Abort calls. Abort
// resets the reported offset to 0, mimicking containerd discarding a stale
// ingest so the next writer starts from the beginning.
type resumeContentStore struct {
	content.Store
	offset      int64
	abortCalled bool
}

func (s *resumeContentStore) Writer(context.Context, ...content.WriterOpt) (content.Writer, error) {
	return &resumeWriter{store: s}, nil
}

func (s *resumeContentStore) Abort(context.Context, string) error {
	s.abortCalled = true
	s.offset = 0
	return nil
}

func newTestRegistryWithContentStore(t *testing.T, cs content.Store) containerdRegistry {
	t.Helper()
	client, err := containerd.New("",
		containerd.WithDefaultNamespace("default"),
		containerd.WithServices(
			containerd.WithImageStore(newTestImageStore()),
			containerd.WithContentStore(cs),
			containerd.WithLeasesService(&testLeasesManager{}),
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

// TestPushBlobChunkedResumeRestartFromZero covers the case where a client
// (e.g. Apple's `container image push`) restarts an interrupted blob upload
// from offset 0 even though the containerd ingest still holds bytes from the
// earlier attempt. The registry must abort the stale ingest and reopen a fresh
// writer rather than rejecting with 416 Range Not Satisfiable, which would
// wedge the client into retrying the same mismatched request forever.
func TestPushBlobChunkedResumeRestartFromZero(t *testing.T) {
	cs := &resumeContentStore{offset: 223728952}
	r := newTestRegistryWithContentStore(t, cs)

	// chunkSize must be non-zero so the offset==0 path is not short-circuited
	// to the "info" (offset == -1) case.
	w, err := r.PushBlobChunkedResume(context.Background(), "llm", "upload-id", 0, 4096)
	if err != nil {
		t.Fatalf("expected restart-from-zero to succeed, got error: %v", err)
	}
	if w == nil {
		t.Fatal("expected a blob writer, got nil")
	}
	if !cs.abortCalled {
		t.Error("expected the stale ingest to be aborted before reopening")
	}
}

// TestPushBlobChunkedResumeGenuineMismatch ensures we still reject a real
// mid-stream offset mismatch (a client resuming at the wrong non-zero offset)
// with 416, since that indicates lost data we cannot recover.
func TestPushBlobChunkedResumeGenuineMismatch(t *testing.T) {
	cs := &resumeContentStore{offset: 1000}
	r := newTestRegistryWithContentStore(t, cs)

	_, err := r.PushBlobChunkedResume(context.Background(), "llm", "upload-id", 500, 4096)
	if err == nil {
		t.Fatal("expected a 416 error for a genuine non-zero offset mismatch, got nil")
	}
	if cs.abortCalled {
		t.Error("must not abort the ingest on a genuine mid-stream mismatch")
	}
}

// ---------------------------------------------------------------------------
// gatedRegistry (WDY-3127 ingest gate)
// ---------------------------------------------------------------------------

// fakeOCIRegistryCalls counts calls that reached a fakeOCIRegistry's methods,
// letting a test assert whether gatedRegistry let a call through to the
// backend or refused it beforehand.
type fakeOCIRegistryCalls struct {
	getBlob               int
	getManifest           int
	pushBlob              int
	pushBlobChunked       int
	pushBlobChunkedResume int
	mountBlob             int
	pushManifest          int
}

// newFakeOCIRegistry returns an ociregistry.Interface (via ociregistry.Funcs)
// that records every call it receives and always succeeds, so tests can wrap
// it in a gatedRegistry without needing a real containerd socket.
func newFakeOCIRegistry(calls *fakeOCIRegistryCalls) *ociregistry.Funcs {
	return &ociregistry.Funcs{
		GetBlob_: func(_ context.Context, _ string, _ ociregistry.Digest) (ociregistry.BlobReader, error) {
			calls.getBlob++
			return nil, nil
		},
		GetManifest_: func(_ context.Context, _ string, _ ociregistry.Digest) (ociregistry.BlobReader, error) {
			calls.getManifest++
			return nil, nil
		},
		PushBlob_: func(_ context.Context, _ string, desc ociregistry.Descriptor, _ io.Reader) (ociregistry.Descriptor, error) {
			calls.pushBlob++
			return desc, nil
		},
		PushBlobChunked_: func(_ context.Context, _ string, _ int) (ociregistry.BlobWriter, error) {
			calls.pushBlobChunked++
			return nil, nil
		},
		PushBlobChunkedResume_: func(_ context.Context, _, _ string, _ int64, _ int) (ociregistry.BlobWriter, error) {
			calls.pushBlobChunkedResume++
			return nil, nil
		},
		MountBlob_: func(_ context.Context, _, _ string, _ ociregistry.Digest) (ociregistry.Descriptor, error) {
			calls.mountBlob++
			return ociregistry.Descriptor{}, nil
		},
		PushManifest_: func(_ context.Context, _ string, _ string, contents []byte, mediaType string) (ociregistry.Descriptor, error) {
			calls.pushManifest++
			return ociregistry.Descriptor{MediaType: mediaType, Size: int64(len(contents))}, nil
		},
	}
}

// failingGate returns a check function shaped like ContainerStorageGate.Check:
// a gRPC FailedPrecondition status error whose message is the plain text a
// caller should see (WDY-3127).
func failingGate(msg string) func() error {
	return func() error { return status.Error(codes.FailedPrecondition, msg) }
}

func healthyGate() func() error {
	return func() error { return nil }
}

// assertStorageDegradedError checks that err marshals (per
// ociregistry.MarshalError, the same path the HTTP handler uses) to HTTP 507
// with a body containing the WENDY_STORAGE_DEGRADED code and the gate's
// plain-text message -- not the "rpc error: code = ... desc = ..." wrapper.
func assertStorageDegradedError(t *testing.T, err error, wantMsg string) {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	if strings.Contains(err.Error(), "rpc error") {
		t.Errorf("error %q leaks the gRPC status wrapper; want the plain gate message", err.Error())
	}
	body, httpStatus := ociregistry.MarshalError(err)
	if httpStatus != http.StatusInsufficientStorage {
		t.Errorf("HTTP status = %d, want %d", httpStatus, http.StatusInsufficientStorage)
	}
	if !strings.Contains(string(body), ErrCodeStorageDegraded) {
		t.Errorf("body = %s, want it to contain %q", body, ErrCodeStorageDegraded)
	}
	if !strings.Contains(string(body), wantMsg) {
		t.Errorf("body = %s, want it to contain %q", body, wantMsg)
	}
}

// TestGatedRegistryRefusesWritesWhenStorageDegraded verifies that every write
// method gatedRegistry overrides refuses with a 507 WENDY_STORAGE_DEGRADED
// error and never reaches the backend when the gate reports an error.
func TestGatedRegistryRefusesWritesWhenStorageDegraded(t *testing.T) {
	const wantMsg = "container storage is on the OS root slot; image ingestion is disabled"
	calls := &fakeOCIRegistryCalls{}
	g := gatedRegistry{Interface: newFakeOCIRegistry(calls), check: failingGate(wantMsg)}
	ctx := context.Background()

	_, err := g.PushBlob(ctx, "repo", ociregistry.Descriptor{}, bytes.NewReader(nil))
	assertStorageDegradedError(t, err, wantMsg)

	_, err = g.PushBlobChunked(ctx, "repo", 4096)
	assertStorageDegradedError(t, err, wantMsg)

	_, err = g.PushBlobChunkedResume(ctx, "repo", "upload-id", 0, 4096)
	assertStorageDegradedError(t, err, wantMsg)

	_, err = g.MountBlob(ctx, "from-repo", "to-repo", digest.FromString("x"))
	assertStorageDegradedError(t, err, wantMsg)

	_, err = g.PushManifest(ctx, "repo", "latest", []byte("{}"), "application/vnd.oci.image.manifest.v1+json")
	assertStorageDegradedError(t, err, wantMsg)

	if *calls != (fakeOCIRegistryCalls{}) {
		t.Errorf("backend was called while the gate was degraded: %+v", calls)
	}
}

// TestGatedRegistryPassesReadsThrough verifies that gatedRegistry does not
// gate read operations: they must reach the backend even while the gate
// reports the storage as degraded.
func TestGatedRegistryPassesReadsThrough(t *testing.T) {
	calls := &fakeOCIRegistryCalls{}
	g := gatedRegistry{Interface: newFakeOCIRegistry(calls), check: failingGate("degraded")}
	ctx := context.Background()

	if _, err := g.GetBlob(ctx, "repo", digest.FromString("x")); err != nil {
		t.Fatalf("GetBlob: %v", err)
	}
	if _, err := g.GetManifest(ctx, "repo", digest.FromString("x")); err != nil {
		t.Fatalf("GetManifest: %v", err)
	}
	if calls.getBlob != 1 || calls.getManifest != 1 {
		t.Errorf("reads did not reach the backend: %+v", calls)
	}
}

// TestGatedRegistryAllowsWritesWhenHealthy verifies that gatedRegistry lets
// every gated write method through to the backend, unchanged, when the gate
// reports no error.
func TestGatedRegistryAllowsWritesWhenHealthy(t *testing.T) {
	calls := &fakeOCIRegistryCalls{}
	g := gatedRegistry{Interface: newFakeOCIRegistry(calls), check: healthyGate()}
	ctx := context.Background()

	if _, err := g.PushBlob(ctx, "repo", ociregistry.Descriptor{}, bytes.NewReader(nil)); err != nil {
		t.Errorf("PushBlob: %v", err)
	}
	if _, err := g.PushBlobChunked(ctx, "repo", 4096); err != nil {
		t.Errorf("PushBlobChunked: %v", err)
	}
	if _, err := g.PushBlobChunkedResume(ctx, "repo", "upload-id", 0, 4096); err != nil {
		t.Errorf("PushBlobChunkedResume: %v", err)
	}
	if _, err := g.MountBlob(ctx, "from-repo", "to-repo", digest.FromString("x")); err != nil {
		t.Errorf("MountBlob: %v", err)
	}
	desc, err := g.PushManifest(ctx, "repo", "latest", []byte("{}"), "application/vnd.oci.image.manifest.v1+json")
	if err != nil {
		t.Errorf("PushManifest: %v", err)
	}
	if desc.MediaType != "application/vnd.oci.image.manifest.v1+json" {
		t.Errorf("PushManifest descriptor = %+v, want the backend's descriptor to come through unchanged", desc)
	}

	want := fakeOCIRegistryCalls{pushBlob: 1, pushBlobChunked: 1, pushBlobChunkedResume: 1, mountBlob: 1, pushManifest: 1}
	if *calls != want {
		t.Errorf("backend calls = %+v, want %+v", calls, want)
	}
}
