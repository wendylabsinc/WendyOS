package vm

import (
	"errors"
	"io"
	"os"
	"strings"
	"testing"
)

type lifecycleTestReader func([]byte) (int, error)

func (r lifecycleTestReader) Read(p []byte) (int, error) { return r(p) }

func TestCreationExcludesRemovalAndStartThroughRollback(t *testing.T) {
	s := &Store{Root: t.TempDir()}
	copyFailure := errors.New("simulated download failure")
	image := lifecycleTestReader(func([]byte) (int, error) {
		// Invoked during the copy, after the directory and disk exist. Separate
		// handles model another CLI acting on the visible partial VM.
		other := &Store{Root: s.Root}
		if err := other.Remove("dev"); !errors.Is(err, ErrLifecycleBusy) {
			t.Fatalf("remove during creation = %v", err)
		}
		if lock, err := other.acquireRunLock("dev"); !errors.Is(err, ErrLifecycleBusy) {
			if lock != nil {
				lock.Close()
			}
			t.Fatalf("start during creation = %v", err)
		}
		if err := other.CreateFrom("dev", strings.NewReader("replacement"), 11, 1024, Meta{}); !errors.Is(err, ErrLifecycleBusy) {
			t.Fatalf("replacement during creation = %v", err)
		}
		return 0, copyFailure
	})
	if err := s.CreateFrom("dev", image, 0, 1024, Meta{}); !errors.Is(err, copyFailure) {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.Dir("dev")); !os.IsNotExist(err) {
		t.Fatalf("rollback left directory: %v", err)
	}
	if err := s.CreateFrom("dev", strings.NewReader("replacement"), 11, 1024, Meta{}); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(s.DiskPath("dev"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	got := make([]byte, 11)
	if _, err := io.ReadFull(f, got); err != nil || string(got) != "replacement" {
		t.Fatalf("replacement disk = %q: %v", got, err)
	}
}

func TestLifecycleLockIdentitySurvivesRemoval(t *testing.T) {
	s := &Store{Root: t.TempDir()}
	if err := s.CreateFrom("dev", strings.NewReader("disk"), 4, 1024, Meta{}); err != nil {
		t.Fatal(err)
	}
	lock, err := s.acquireLifecycleLock("dev")
	if err != nil {
		t.Fatal(err)
	}
	before, err := lock.Stat()
	lock.Close()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Remove("dev"); err != nil {
		t.Fatal(err)
	}
	lock, err = s.acquireLifecycleLock("dev")
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	after, err := lock.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) {
		t.Fatal("removal replaced the lifecycle lock inode")
	}
}
