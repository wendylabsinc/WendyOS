package containerd

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/containerd/containerd/v2/core/containers"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/core/leases"
	"go.uber.org/zap"
)

// fakeImageStore lists names and records each delete with its options.
type fakeImageStore struct {
	images.Store
	names   []string
	deleted []string
	sync    []bool
	failOn  string
}

func (f *fakeImageStore) List(context.Context, ...string) ([]images.Image, error) {
	var out []images.Image
	for _, n := range f.names {
		out = append(out, images.Image{Name: n})
	}
	return out, nil
}

func (f *fakeImageStore) Delete(_ context.Context, name string, opts ...images.DeleteOpt) error {
	var o images.DeleteOptions
	for _, opt := range opts {
		if err := opt(context.Background(), &o); err != nil {
			return err
		}
	}
	f.deleted, f.sync = append(f.deleted, name), append(f.sync, o.Synchronous)
	if name == f.failOn {
		return errors.New("image in use")
	}
	return nil
}

var releaseImages = []string{
	ReleaseImageName("web", "r1"), ReleaseImageName("web", "r2"), ReleaseImageName("api", "r3"),
	"localhost/web:latest", "docker.io/library/web:r1",
}

// Removal deletes only the app's registry releases outside keep, waits for
// their content to be collected, and goes on past a failure.
func TestDeleteReleaseImages(t *testing.T) {
	for _, tc := range []struct {
		app  string
		keep []string
		want []string
	}{
		{"web", []string{"r2"}, []string{ReleaseImageName("web", "r1")}},
		{"web", nil, []string{ReleaseImageName("web", "r1"), ReleaseImageName("web", "r2")}},
		{"", nil, []string{ReleaseImageName("web", "r1"), ReleaseImageName("web", "r2"), ReleaseImageName("api", "r3")}},
		{"other", nil, nil},
	} {
		is := &fakeImageStore{names: releaseImages}
		if err := deleteReleaseImages(context.Background(), is, zap.NewNop(), tc.app, tc.keep); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(is.deleted, tc.want) || slices.Contains(is.sync, false) {
			t.Fatalf("app %q keep %v: deleted %v (synchronous %v), want %v synchronously", tc.app, tc.keep, is.deleted, is.sync, tc.want)
		}
	}
	is := &fakeImageStore{names: releaseImages, failOn: ReleaseImageName("web", "r1")}
	if err := deleteReleaseImages(context.Background(), is, zap.NewNop(), "web", nil); err == nil || len(is.deleted) != 2 {
		t.Fatalf("err %v, deleted %v: want the failure returned after trying every release", err, is.deleted)
	}
}

// Only containers running a removable release name their app.
func TestReleaseApps(t *testing.T) {
	ctrs := []containers.Container{
		{Image: ReleaseImageName("web", "r1"), Labels: map[string]string{labelKeyAppID: "web"}},
		{Image: ReleaseImageName("web", "r1"), Labels: map[string]string{labelKeyAppID: "web"}},
		{Image: "localhost/web:latest", Labels: map[string]string{labelKeyAppID: "web"}},
		{Image: ReleaseImageName("api", "r3"), Labels: map[string]string{labelKeyAppID: "api"}},
	}
	if got := releaseApps(ctrs, "web", nil); !slices.Equal(got, []string{"web"}) {
		t.Fatalf("web: %v", got)
	}
	if got := releaseApps(ctrs, "web", []string{"r1"}); len(got) != 0 {
		t.Fatalf("web keeping r1: %v", got)
	}
	if got := releaseApps(ctrs, "", nil); !slices.Equal(got, []string{"web", "api"}) {
		t.Fatalf("every app: %v", got)
	}
}

// The import lease is deleted with a synchronous collection whether the
// import succeeds, fails or is cancelled: what no image record took over is
// gone before the import returns.
func TestImportLeaseIsCollectedSynchronously(t *testing.T) {
	for _, importErr := range []error{nil, errors.New("import failed"), context.Canceled} {
		lm := &fakeLeaseGCer{}
		var leased string
		err := withImportLease(context.Background(), context.Background(), lm, zap.NewNop(), func(ctx context.Context) error {
			leased, _ = leases.FromContext(ctx)
			return importErr
		})
		if !errors.Is(err, importErr) || leased == "" || leased != lm.created.ID || lm.deletedID != lm.created.ID {
			t.Fatalf("import err %v: got %v, leased %q, created %q, deleted %q", importErr, err, leased, lm.created.ID, lm.deletedID)
		}
		var opts leases.DeleteOptions
		for _, opt := range lm.deleteOpts {
			if err := opt(context.Background(), &opts); err != nil {
				t.Fatal(err)
			}
		}
		if !opts.Synchronous {
			t.Fatalf("import err %v: lease not deleted synchronously", importErr)
		}
	}
}

func TestReleasesInUse(t *testing.T) {
	ctrs := []containers.Container{
		{ID: "web", Image: releaseImagePrefix + "web:r1"},
		{ID: "web_api", Image: releaseImagePrefix + "web:r1"},
		{ID: "webx", Image: releaseImagePrefix + "webx:r2"},
		{ID: "dev", Image: "docker.io/library/web:latest"},
	}
	if got := releasesInUse(ctrs, "web"); !slices.Equal(got, []string{"r1"}) {
		t.Fatalf("in use %v, want [r1]", got)
	}
}

// The app definition is read only from the image the operator signed.
func TestCheckReleaseDigest(t *testing.T) {
	if checkReleaseDigest("sha256:aa", "sha256:aa") != nil {
		t.Fatal("the signed image refused")
	}
	if checkReleaseDigest("sha256:bb", "sha256:aa") == nil || checkReleaseDigest("sha256:aa", "") == nil {
		t.Fatal("another image, or an empty signed digest, accepted")
	}
}

// Only ImportRelease writes the release prefix: the direct-run assemble path
// refuses it before touching the store.
func TestAssembleImageRefusesTheReleasePrefix(t *testing.T) {
	c := &Client{}
	for _, name := range []string{"localhost/wendy-release/web:r1", "localhost/wendy-release/web"} {
		if err := c.AssembleImage(context.Background(), name, nil, nil); err == nil || !strings.Contains(err.Error(), "reserved for registry releases") {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

// A release is created only from a reference pinned to its signed digest.
func TestSplitReleaseRef(t *testing.T) {
	if name, digest, err := splitReleaseRef("localhost/wendy-release/web:r1@sha256:aa"); err != nil || name != "localhost/wendy-release/web:r1" || digest != "sha256:aa" {
		t.Fatalf("%q %q %v", name, digest, err)
	}
	for _, ref := range []string{"localhost/wendy-release/web:r1", "localhost/wendy-release/web:r1@"} {
		if _, _, err := splitReleaseRef(ref); err == nil {
			t.Fatalf("%q: an unpinned release accepted", ref)
		}
	}
}
