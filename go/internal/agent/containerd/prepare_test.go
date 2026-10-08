package containerd

import (
	"context"
	"crypto/sha256"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestWaitForChunksUnblocksWhenChunkIsStaged(t *testing.T) {
	c := &Client{chunkIndex: newTestChunkIndex(t), staging: newStaging(filepath.Join(t.TempDir(), "staging"))}
	data := []byte("arrives while preparation is waiting")
	hash := sha256.Sum256(data)

	done := make(chan error, 2)
	for range 2 {
		go func() {
			done <- c.waitForChunks(context.Background(), [][32]byte{hash})
		}()
	}

	if err := c.StageChunk(context.Background(), hash, data); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("waitForChunks: %v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("waitForChunks did not broadcast the staged chunk")
		}
	}
}

func TestWaitForChunksHonorsCancellation(t *testing.T) {
	c := &Client{chunkIndex: newTestChunkIndex(t), staging: newStaging(filepath.Join(t.TempDir(), "staging"))}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := c.waitForChunks(ctx, [][32]byte{{1}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("waitForChunks error = %v, want context.Canceled", err)
	}
}

func TestWaitForChunksNarrowsRepeatedChecksToMissingHashes(t *testing.T) {
	all := [][32]byte{{1}, {2}, {3}, {4}}
	closed := make(chan struct{})
	close(closed)
	var checked [][][32]byte
	responses := [][][32]byte{
		{{2}, {3}, {4}}, // initial full check
		{{3}, {4}},      // first arrival: only prior misses are checked
		nil,             // the narrowed set is now complete
		nil,             // final full-layer validation
	}
	missing := func(_ context.Context, hashes [][32]byte) ([][32]byte, error) {
		checked = append(checked, append([][32]byte(nil), hashes...))
		response := responses[len(checked)-1]
		return response, nil
	}

	if err := waitForChunksWith(context.Background(), all, func() <-chan struct{} { return closed }, missing); err != nil {
		t.Fatalf("waitForChunksWith: %v", err)
	}
	want := [][][32]byte{all, {{2}, {3}, {4}}, {{3}, {4}}, all}
	if len(checked) != len(want) {
		t.Fatalf("checked %d sets, want %d: %v", len(checked), len(want), checked)
	}
	for i := range want {
		if len(checked[i]) != len(want[i]) {
			t.Fatalf("check %d examined %d hashes, want %d", i, len(checked[i]), len(want[i]))
		}
		for j := range want[i] {
			if checked[i][j] != want[i][j] {
				t.Fatalf("check %d hash %d = %x, want %x", i, j, checked[i][j], want[i][j])
			}
		}
	}
}

func TestPrepareChunkHashesRejectsMalformedHash(t *testing.T) {
	if _, err := prepareChunkHashes([][]byte{{1, 2, 3}}); err == nil {
		t.Fatal("expected malformed chunk hash error")
	}
}
