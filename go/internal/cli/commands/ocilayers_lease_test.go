package commands

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLockOCILayoutDirNotifyReportsWaitOnlyWhenContended(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "app-layout")
	waited := 0
	release, err := lockOCILayoutDirNotify(context.Background(), dir, func() { waited++ })
	if err != nil {
		t.Fatal(err)
	}
	if waited != 0 {
		t.Fatalf("onWait ran %d times for an uncontended lock", waited)
	}

	waiting := make(chan struct{})
	acquired := make(chan struct{})
	go func() {
		r2, err := lockOCILayoutDirNotify(context.Background(), dir, func() { close(waiting) })
		if err != nil {
			t.Error(err)
		} else {
			r2()
		}
		close(acquired)
	}()
	select {
	case <-waiting:
	case <-time.After(3 * time.Second):
		t.Fatal("a contended acquire never reported that it was waiting")
	}
	release()
	release() // idempotent: deployByChunkDiff releases early and again on return
	select {
	case <-acquired:
	case <-time.After(3 * time.Second):
		t.Fatal("second acquire never succeeded after release")
	}
}

// writeLeaseLayout writes a readable single-layer layout into dir plus one
// unreferenced blob, and returns that stray blob's path.
func writeLeaseLayout(t *testing.T, dir string) string {
	t.Helper()
	layer := bytes.Repeat([]byte("lease-layer-"), 64)
	writeOCILayoutDir(t, dir, minimalOCILayoutEntries(t, layer, "application/vnd.oci.image.layer.v1.tar", layer))
	stray := filepath.Join(dir, "blobs", "sha256", strings.Repeat("ab", 32))
	if err := os.WriteFile(stray, []byte("superseded"), 0o644); err != nil {
		t.Fatal(err)
	}
	return stray
}

func TestOCILayoutLeaseReleaseGCsThenUnlocksOnce(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "app-layout")
	stray := writeLeaseLayout(t, dir)
	lease, err := acquireOCILayoutLease(context.Background(), dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	lease.markBuilt()
	lease.release()
	lease.release() // idempotent
	if _, err := os.Stat(stray); !os.IsNotExist(err) {
		t.Fatalf("release did not GC the superseded blob: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	again, err := lockOCILayoutDir(ctx, dir)
	if err != nil {
		t.Fatalf("layout still locked after release: %v", err)
	}
	again()
}

// A failed build must not GC: the directory may hold a half-written export
// whose blobs the next run's self-heal decides about.
func TestOCILayoutLeaseUnbuiltReleaseSkipsGC(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "app-layout")
	stray := writeLeaseLayout(t, dir)
	lease, err := acquireOCILayoutLease(context.Background(), dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	lease.release()
	if _, err := os.Stat(stray); err != nil {
		t.Fatalf("unbuilt release garbage-collected the layout: %v", err)
	}
}

// The registry fallback reuses the layout after the chunk-diff deploy released
// it. When another run rebuilt the directory meanwhile, pushing it would ship
// that run's image under this run's deploy, so the reuse must refuse.
func TestTryPushExistingOCILayoutRefusesARebuiltLayout(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "app-layout")
	writeLeaseLayout(t, dir)
	hint := &ociReuseHint{layoutDir: dir, platform: "linux/amd64", configDigest: ociConfigDigest([]byte(`{"a":"different build"}`))}
	err := tryPushExistingOCILayout(context.Background(), nil, 5000, hint, "app")
	if err == nil || !strings.Contains(err.Error(), "rebuilt by another wendy run") {
		t.Fatalf("err = %v, want a refusal to reuse a rebuilt layout", err)
	}
}

func TestTryPushExistingOCILayoutWaitsForTheLayoutLock(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "app-layout")
	writeLeaseLayout(t, dir)
	release, err := lockOCILayoutDir(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	out := captureStderr(t, func() {
		err = tryPushExistingOCILayout(ctx, nil, 5000, &ociReuseHint{layoutDir: dir, platform: "linux/amd64"}, "app")
	})
	if err == nil || !strings.Contains(err.Error(), "acquiring OCI layout lock") {
		t.Fatalf("err = %v, want it to wait on the held layout lock", err)
	}
	if !strings.Contains(out, "Waiting for another wendy run of app to release the build cache") {
		t.Fatalf("missing wait notice: %q", out)
	}
}
