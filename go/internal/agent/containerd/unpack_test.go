package containerd

import (
	"context"
	"errors"
	"fmt"
	"maps"
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
	for i, info := range got {
		if info == nil {
			t.Errorf("got[%d] = nil; want snapshot info", i)
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
	for i, info := range got {
		if info != nil {
			t.Errorf("got[%d] = %+v; want nil", i, info)
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
	if got[0] != nil {
		t.Error("got[0] should be nil")
	}
	if got[1] == nil {
		t.Error("got[1] should be set")
	}
	if got[2] != nil {
		t.Error("got[2] should be nil")
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

func TestStackedLayers(t *testing.T) {
	chainIDs := []string{"sha256:c0", "sha256:c1", "sha256:c2"}
	snap := func(parent string) *snapshots.Info { return &snapshots.Info{Parent: parent} }
	tests := []struct {
		name  string
		infos []*snapshots.Info
		want  int
	}{
		{"all stacked", []*snapshots.Info{snap(""), snap("sha256:c0"), snap("sha256:c1")}, 3},
		{"none unpacked", []*snapshots.Info{nil, nil, nil}, 0},
		{"top not unpacked yet", []*snapshots.Info{snap(""), snap("sha256:c0"), nil}, 2},
		// WDY-3799: containerd 1.7 committed layers above the base with no parent.
		{"layers above base detached", []*snapshots.Info{snap(""), snap(""), snap("")}, 1},
		// Correctly parented, but on a layer that is not.
		{"stacked on a detached layer", []*snapshots.Info{snap(""), snap(""), snap("sha256:c1")}, 1},
		{"detached above a gap", []*snapshots.Info{snap(""), nil, snap("")}, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := stackedLayers(chainIDs, tt.infos); got != tt.want {
				t.Errorf("stackedLayers = %d; want %d", got, tt.want)
			}
		})
	}
}

// parentTrackingSnapshotter models containerd's rule that a snapshot cannot
// be removed while another snapshot uses it as a parent.
type parentTrackingSnapshotter struct {
	parents map[string]string // snapshot name → parent name
}

func (s *parentTrackingSnapshotter) Remove(_ context.Context, key string) error {
	if _, ok := s.parents[key]; !ok {
		return errdefs.ErrNotFound
	}
	for name, parent := range s.parents {
		if parent == key {
			return fmt.Errorf("cannot remove snapshot %s with child %s: %w", key, name, errdefs.ErrFailedPrecondition)
		}
	}
	delete(s.parents, key)
	return nil
}

func TestRemoveLayerSnapshotsRemovesFromTheTopDown(t *testing.T) {
	chainIDs := []string{"sha256:c0", "sha256:c1", "sha256:c2"}
	sn := &parentTrackingSnapshotter{parents: map[string]string{
		"sha256:c0": "",
		"sha256:c1": "",          // detached
		"sha256:c2": "sha256:c1", // stacked on the detached layer
	}}
	infos := []*snapshots.Info{{}, {}, {Parent: "sha256:c1"}}

	if err := removeLayerSnapshots(context.Background(), sn, chainIDs, infos, 1); err != nil {
		t.Fatalf("removeLayerSnapshots: %v", err)
	}
	want := map[string]string{"sha256:c0": ""}
	if !maps.Equal(sn.parents, want) {
		t.Errorf("remaining snapshots = %v; want %v", sn.parents, want)
	}
}

func TestRemoveLayerSnapshotsSkipsLayersNeverUnpacked(t *testing.T) {
	chainIDs := []string{"sha256:c0", "sha256:c1", "sha256:c2"}
	sn := &parentTrackingSnapshotter{parents: map[string]string{"sha256:c0": "", "sha256:c2": ""}}
	infos := []*snapshots.Info{{}, nil, {}}

	if err := removeLayerSnapshots(context.Background(), sn, chainIDs, infos, 1); err != nil {
		t.Fatalf("removeLayerSnapshots: %v", err)
	}
	want := map[string]string{"sha256:c0": ""}
	if !maps.Equal(sn.parents, want) {
		t.Errorf("remaining snapshots = %v; want %v", sn.parents, want)
	}
}

func TestRemoveLayerSnapshotsReportsSnapshotInUse(t *testing.T) {
	chainIDs := []string{"sha256:c0", "sha256:c1"}
	sn := &parentTrackingSnapshotter{parents: map[string]string{
		"sha256:c0":         "",
		"sha256:c1":         "",
		"app-rootfs-active": "sha256:c1", // another container still runs on it
	}}
	infos := []*snapshots.Info{{}, {}}

	err := removeLayerSnapshots(context.Background(), sn, chainIDs, infos, 1)
	if !errdefs.IsFailedPrecondition(err) {
		t.Fatalf("err = %v; want the snapshotter's failed precondition", err)
	}
	if _, ok := sn.parents["sha256:c1"]; !ok {
		t.Error("snapshot in use was removed")
	}
}
