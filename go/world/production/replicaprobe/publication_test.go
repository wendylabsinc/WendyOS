package replicaprobe

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestPublishedReceiptIsSingleLinkBeforeWriterCompletes(t *testing.T) {
	_, _, path := paths(t)
	d, err := openDirectory(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.close()
	// Reproduce why the old two-step publication was incompatible with read.
	if err = os.WriteFile(filepath.Join(path, "old-pending"), []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = unix.Linkat(int(d.file.Fd()), "old-pending", int(d.file.Fd()), "old-final", 0); err != nil {
		t.Fatal(err)
	}
	if _, err = d.read("old-final", 64); !errors.Is(err, ErrConfig) {
		t.Fatal("two-link window not reproduced", err)
	}

	published, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	var once sync.Once
	defer once.Do(func() { close(release) })
	raw := []byte("exact original receipt")
	go func() {
		done <- d.writeWithPublisher("new-final", raw, 64, func(fd int, from, to string) error {
			if err := renameNoReplace(fd, from, to); err != nil {
				return err
			}
			close(published)
			<-release
			return nil
		})
	}()
	select {
	case <-published:
	case err = <-done:
		t.Fatal("rename failed", err)
	case <-time.After(2 * time.Second):
		t.Fatal("publication did not happen")
	}
	for i := 0; i < 20; i++ {
		got, e := d.read("new-final", 64)
		if e != nil || !bytes.Equal(got, raw) {
			t.Fatal("reader saw invalid intermediate file", e)
		}
	}
	if _, err = os.Stat(filepath.Join(path, ".pending-new-final")); !os.IsNotExist(err) {
		t.Fatal("rename retained pending name", err)
	}
	once.Do(func() { close(release) })
	if err = <-done; err != nil {
		t.Fatal(err)
	}
}

func TestUnsupportedAtomicRenameCannotFallBackOrOverwrite(t *testing.T) {
	_, _, path := paths(t)
	d, err := openDirectory(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.close()
	err = d.writeWithPublisher("final", []byte("complete"), 64, func(int, string, string) error { return unix.ENOTSUP })
	if !errors.Is(err, unix.ENOTSUP) {
		t.Fatal("unsupported publication succeeded", err)
	}
	if _, err = os.Stat(filepath.Join(path, "final")); !os.IsNotExist(err) {
		t.Fatal("fallback exposed final file", err)
	}
	raw, err := d.read(".pending-final", 64)
	if err != nil || string(raw) != "complete" {
		t.Fatal("failed operation lost pending bytes", err)
	}
}
