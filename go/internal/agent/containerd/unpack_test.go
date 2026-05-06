package containerd

import (
	"context"
	"errors"
	"testing"

	"github.com/containerd/containerd/v2/core/snapshots"
	"github.com/containerd/errdefs"
)

// mockStatter implements snapshotStatter for testing.
type mockStatter struct {
	// exists maps chain ID → true if the snapshot exists.
	exists map[string]bool
	// errs maps chain ID → error to return (overrides exists).
	errs map[string]error
}

func (m *mockStatter) Stat(_ context.Context, key string) (snapshots.Info, error) {
	if err, ok := m.errs[key]; ok {
		return snapshots.Info{}, err
	}
	if m.exists[key] {
		return snapshots.Info{Name: key}, nil
	}
	return snapshots.Info{}, errdefs.ErrNotFound
}

func TestStatLayers_AllExist(t *testing.T) {
	ids := []string{"sha256:aaa", "sha256:bbb", "sha256:ccc"}
	sn := &mockStatter{exists: map[string]bool{
		"sha256:aaa": true, "sha256:bbb": true, "sha256:ccc": true,
	}}
	got, err := statLayers(context.Background(), sn, ids)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for i, v := range got {
		if !v {
			t.Errorf("exists[%d] = false; want true", i)
		}
	}
}

func TestStatLayers_NoneExist(t *testing.T) {
	ids := []string{"sha256:aaa", "sha256:bbb"}
	sn := &mockStatter{exists: map[string]bool{}}
	got, err := statLayers(context.Background(), sn, ids)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for i, v := range got {
		if v {
			t.Errorf("exists[%d] = true; want false", i)
		}
	}
}

func TestStatLayers_Mixed(t *testing.T) {
	ids := []string{"sha256:aaa", "sha256:bbb", "sha256:ccc"}
	sn := &mockStatter{exists: map[string]bool{"sha256:bbb": true}}
	got, err := statLayers(context.Background(), sn, ids)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got[0] {
		t.Error("exists[0] should be false")
	}
	if !got[1] {
		t.Error("exists[1] should be true")
	}
	if got[2] {
		t.Error("exists[2] should be false")
	}
}

func TestStatLayers_PropagatesNonNotFoundError(t *testing.T) {
	sentinel := errors.New("storage failure")
	ids := []string{"sha256:aaa", "sha256:bbb"}
	sn := &mockStatter{errs: map[string]error{"sha256:aaa": sentinel}}
	_, err := statLayers(context.Background(), sn, ids)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("error = %v; want sentinel", err)
	}
}

func TestStatLayers_EmptyInput(t *testing.T) {
	sn := &mockStatter{}
	got, err := statLayers(context.Background(), sn, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("len(got) = %d; want 0", len(got))
	}
}
