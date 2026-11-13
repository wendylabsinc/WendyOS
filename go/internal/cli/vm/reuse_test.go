package vm

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPrepareForReuseKeepsStoppedAndRunningVMs(t *testing.T) {
	for _, running := range []bool{false, true} {
		t.Run(map[bool]string{false: "stopped", true: "running"}[running], func(t *testing.T) {
			s := newTestStore(t)
			if err := s.CreateFrom("tour", strings.NewReader("original disk"), 13, 4096, Meta{}); err != nil {
				t.Fatal(err)
			}
			before, _ := os.Stat(s.DiskPath("tour"))
			if running {
				lock, err := s.acquireRunLock("tour")
				if err != nil {
					t.Fatal(err)
				}
				defer lock.Close()
			}
			ready, backup, err := s.PrepareForReuse("tour")
			if err != nil || !ready || backup != "" {
				t.Fatalf("reuse = %v, %q, %v", ready, backup, err)
			}
			after, _ := os.Stat(s.DiskPath("tour"))
			if !os.SameFile(before, after) {
				t.Fatal("existing disk was replaced")
			}
		})
	}
}

func TestPrepareForReusePreservesInterruptedCreation(t *testing.T) {
	for _, state := range []string{"empty directory", "disk only", "missing vars", "truncated vars", "truncated disk", "invalid metadata"} {
		t.Run(state, func(t *testing.T) {
			s := newTestStore(t)
			if err := s.CreateFrom("tour", strings.NewReader("original disk"), 13, 4096, Meta{}); err != nil {
				t.Fatal(err)
			}
			switch state {
			case "empty directory":
				for _, path := range []string{s.MetaPath("tour"), s.VarsPath("tour"), s.DiskPath("tour")} {
					if err := os.Remove(path); err != nil {
						t.Fatal(err)
					}
				}
			case "disk only":
				for _, path := range []string{s.MetaPath("tour"), s.VarsPath("tour")} {
					if err := os.Remove(path); err != nil {
						t.Fatal(err)
					}
				}
			case "missing vars":
				if err := os.Remove(s.VarsPath("tour")); err != nil {
					t.Fatal(err)
				}
			case "truncated vars":
				if err := os.Truncate(s.VarsPath("tour"), 1024); err != nil {
					t.Fatal(err)
				}
			case "truncated disk":
				if err := os.Truncate(s.DiskPath("tour"), 16); err != nil {
					t.Fatal(err)
				}
			case "invalid metadata":
				if err := os.WriteFile(s.MetaPath("tour"), []byte("{"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			before, _ := os.Stat(s.DiskPath("tour"))
			ready, backup, err := s.PrepareForReuse("tour")
			if err != nil || ready || backup == "" {
				t.Fatalf("recovery = %v, %q, %v", ready, backup, err)
			}
			if before != nil {
				after, err := os.Stat(filepath.Join(backup, "disk.img"))
				if err != nil || !os.SameFile(before, after) || before.Size() != after.Size() {
					t.Fatalf("original disk was not preserved: %v", err)
				}
			}
			if err := s.CheckCreatable("tour"); err != nil {
				t.Fatalf("recovery did not free original name: %v", err)
			}
			names, err := s.List()
			if err != nil || len(names) != 0 {
				t.Fatalf("recovery files appeared as VM: %v, %v", names, err)
			}
			if err := s.CreateFrom("tour", strings.NewReader("new disk"), 8, 4096, Meta{}); err != nil {
				t.Fatalf("cannot resume creation: %v", err)
			}
		})
	}
}

func TestPrepareForReuseDoesNotMoveActiveOrUnreadableVM(t *testing.T) {
	for _, blocked := range []string{"creation lock", "running lock", "metadata directory", "metadata symlink"} {
		t.Run(blocked, func(t *testing.T) {
			s := newTestStore(t)
			if err := s.CreateFrom("tour", strings.NewReader("original disk"), 13, 4096, Meta{}); err != nil {
				t.Fatal(err)
			}
			before, _ := os.Stat(s.DiskPath("tour"))
			if err := os.Remove(s.MetaPath("tour")); err != nil {
				t.Fatal(err)
			}
			var want error
			switch blocked {
			case "creation lock":
				lock, err := s.acquireLifecycleLock("tour")
				if err != nil {
					t.Fatal(err)
				}
				defer lock.Close()
				want = ErrLifecycleBusy
			case "running lock":
				lock, err := s.acquireRunLock("tour")
				if err != nil {
					t.Fatal(err)
				}
				defer lock.Close()
				want = ErrAlreadyRunning
			case "metadata directory":
				if err := os.Mkdir(s.MetaPath("tour"), 0700); err != nil {
					t.Fatal(err)
				}
			case "metadata symlink":
				if err := os.Symlink("missing-meta.json", s.MetaPath("tour")); err != nil {
					t.Skipf("symlinks unavailable: %v", err)
				}
			}
			ready, backup, err := s.PrepareForReuse("tour")
			if err == nil || ready || backup != "" || (want != nil && !errors.Is(err, want)) {
				t.Fatalf("blocked recovery = %v, %q, %v; want %v", ready, backup, err, want)
			}
			after, err := os.Stat(s.DiskPath("tour"))
			if err != nil || !os.SameFile(before, after) {
				t.Fatalf("blocked recovery moved original VM: %v", err)
			}
		})
	}
}

func TestPrepareForReuseAbsentVMAndInvalidName(t *testing.T) {
	s := newTestStore(t)
	ready, backup, err := s.PrepareForReuse("tour")
	if err != nil || ready || backup != "" {
		t.Fatalf("absent VM = %v, %q, %v", ready, backup, err)
	}
	if _, err := os.Stat(s.Dir("tour")); !os.IsNotExist(err) {
		t.Fatalf("absent VM directory created: %v", err)
	}
	if _, _, err := s.PrepareForReuse("../escape"); err == nil {
		t.Fatal("accepted invalid VM name")
	}
}
