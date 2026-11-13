//go:build windows

package analytics

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/flock"
	"golang.org/x/sys/windows"
)

func TestLoadOrCreateIDRechecksSharingViolationAfterWriter(t *testing.T) {
	dir := isolateIdentityHome(t)
	t.Setenv("WENDY_CONFIG_DIR", t.TempDir())
	idPath := filepath.Join(dir, "analytics_id")
	const want = "69eedfb8-a7d5-43c5-b61d-3083caa38f63"
	if err := os.WriteFile(idPath, []byte(want), 0o600); err != nil {
		t.Fatal(err)
	}
	release, err := flock.Acquire(filepath.Join(dir, "analytics_id.lock"), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(release)
	path, err := windows.UTF16PtrFromString(idPath)
	if err != nil {
		t.Fatal(err)
	}
	// Deny read sharing while the writer holds the lock, as a rename can do.
	handle, err := windows.CreateFile(path, windows.GENERIC_READ, 0, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatal(err)
	}
	closeHandle := sync.OnceFunc(func() {
		if err := windows.CloseHandle(handle); err != nil {
			t.Error(err)
		}
	})
	t.Cleanup(closeHandle)
	if _, err := readAnalyticsID(idPath); !errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
		t.Fatalf("blocked read = %v, want sharing violation", err)
	}
	type result struct {
		id  string
		err error
	}
	loaded := make(chan result, 1)
	go func() {
		id, err := loadOrCreateID()
		loaded <- result{id, err}
	}()
	select {
	case got := <-loaded:
		t.Fatalf("read did not wait for the writer: %+v", got)
	case <-time.After(30 * time.Millisecond):
	}
	closeHandle()
	release()
	select {
	case got := <-loaded:
		if got.err != nil || got.id != want {
			t.Fatalf("identity after writer = %+v, want %s", got, want)
		}
	case <-time.After(time.Second):
		t.Fatal("identity read did not resume after the writer released the lock")
	}
}
