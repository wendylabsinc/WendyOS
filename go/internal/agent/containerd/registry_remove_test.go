package containerd

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/containerd/containerd/v2/core/containers"
	"github.com/containerd/containerd/v2/core/images"
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
