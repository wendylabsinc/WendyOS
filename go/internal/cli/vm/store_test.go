package vm

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestConcurrentCreatesHaveExactlyOneOwner(t *testing.T) {
	for range 25 {
		s := newTestStore(t)
		start := make(chan struct{})
		results := make(chan error, 16)
		var wg sync.WaitGroup
		for range 16 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				results <- s.CreateFrom("dev", strings.NewReader("image"), 5, 1<<20, Meta{})
			}()
		}
		close(start)
		wg.Wait()
		close(results)
		winners := 0
		for err := range results {
			if err == nil {
				winners++
			} else if !errors.Is(err, ErrLifecycleBusy) && !strings.Contains(err.Error(), "already exists") {
				t.Fatal(err)
			}
		}
		if winners != 1 {
			t.Fatalf("got %d successful creators, want exactly one", winners)
		}
		f, err := os.Open(s.DiskPath("dev"))
		if err != nil {
			t.Fatal(err)
		}
		var prefix [5]byte
		_, err = io.ReadFull(f, prefix[:])
		f.Close()
		if err != nil || string(prefix[:]) != "image" {
			t.Fatalf("winner's disk damaged: %q %v", prefix, err)
		}
		if _, ok := s.ReadMeta("dev"); !ok {
			t.Fatal("winner's metadata removed")
		}
	}
}

func TestValidName(t *testing.T) {
	for _, ok := range []string{"wendy-vm", "vm1", "a-b-c", "dev"} {
		if err := ValidName(ok); err != nil {
			t.Errorf("ValidName(%q) = %v, want nil", ok, err)
		}
	}
	// Rejected because the name becomes both a directory and an mDNS-visible
	// hostname: separators, traversal and shouting all have to go.
	for _, bad := range []string{"", "-lead", "trail-", "Up", "a b", "../esc", "a/b", strings.Repeat("x", 64)} {
		if err := ValidName(bad); err == nil {
			t.Errorf("ValidName(%q) = nil, want an error", bad)
		}
	}
}

// createFromFile is the path-taking convenience the production code no longer
// needs; the tests still find it clearer than opening the file at each site.
func (s *Store) createFromFile(name, sourceImage string, diskBytes int64) error {
	f, err := os.Open(sourceImage)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	return s.CreateFrom(name, f, info.Size(), diskBytes, Meta{})
}

func newTestStore(t *testing.T) *Store {
	t.Helper()
	return &Store{Root: filepath.Join(t.TempDir(), "vms")}
}

func TestCreateCopiesTheImageAndSizesTheDisk(t *testing.T) {
	s := newTestStore(t)

	src := filepath.Join(t.TempDir(), "image.wic")
	if err := os.WriteFile(src, []byte("disk-image-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}

	const want = int64(32 << 20)
	if err := s.createFromFile("dev", src, want); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	info, err := os.Stat(s.DiskPath("dev"))
	if err != nil {
		t.Fatalf("disk not created: %v", err)
	}
	if info.Size() != want {
		t.Errorf("disk size = %d, want %d", info.Size(), want)
	}

	// The copy must be a copy, not a link: starting the VM writes to it and the
	// downloaded image has to stay pristine for the next `vm create`.
	head := make([]byte, len("disk-image-bytes"))
	f, err := os.Open(s.DiskPath("dev"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.Read(head); err != nil {
		t.Fatal(err)
	}
	if string(head) != "disk-image-bytes" {
		t.Errorf("disk head = %q, want the source image bytes", head)
	}

	vars, err := os.Stat(s.VarsPath("dev"))
	if err != nil {
		t.Fatalf("varstore not created: %v", err)
	}
	if vars.Size() != pflashBytes {
		t.Errorf("varstore size = %d, want %d", vars.Size(), pflashBytes)
	}
}

func TestCreateRefusesToClobberAnExistingVM(t *testing.T) {
	s := newTestStore(t)
	src := filepath.Join(t.TempDir(), "image.wic")
	if err := os.WriteFile(src, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.createFromFile("dev", src, 1<<20); err != nil {
		t.Fatal(err)
	}
	if err := s.createFromFile("dev", src, 1<<20); err == nil {
		t.Fatal("second Create() succeeded; it must refuse rather than discard a VM's disk")
	}
}

func TestCreateRefusesToShrinkTheImage(t *testing.T) {
	s := newTestStore(t)
	src := filepath.Join(t.TempDir(), "image.wic")
	if err := os.WriteFile(src, make([]byte, 4096), 0o644); err != nil {
		t.Fatal(err)
	}
	// Truncating below the image would cut the GPT backup header and the last
	// partition off the disk.
	if err := s.createFromFile("dev", src, 1024); err == nil {
		t.Fatal("Create() accepted a disk smaller than the image")
	}
}

func TestListAndRemove(t *testing.T) {
	s := newTestStore(t)
	src := filepath.Join(t.TempDir(), "image.wic")
	if err := os.WriteFile(src, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"beta", "alpha"} {
		if err := s.createFromFile(n, src, 1<<20); err != nil {
			t.Fatal(err)
		}
	}

	got, err := s.List()
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(got) != 2 || got[0] != "alpha" || got[1] != "beta" {
		t.Errorf("List() = %v, want sorted [alpha beta]", got)
	}

	if err := s.Remove("alpha"); err != nil {
		t.Fatalf("Remove() error = %v", err)
	}
	got, err = s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "beta" {
		t.Errorf("after Remove, List() = %v, want [beta]", got)
	}

	if err := s.Remove("alpha"); err == nil {
		t.Fatal("Remove() of an absent VM succeeded; it should report the name")
	}
}

func TestListOnAnAbsentRootIsEmptyNotAnError(t *testing.T) {
	// `wendy vm list` before the first `create` is the common first command.
	s := newTestStore(t)
	got, err := s.List()
	if err != nil {
		t.Fatalf("List() on a missing root = %v, want nil error", err)
	}
	if len(got) != 0 {
		t.Errorf("List() = %v, want empty", got)
	}
}

func TestRemoveRejectsAnInvalidNameRatherThanDeletingByPath(t *testing.T) {
	s := newTestStore(t)
	if err := s.Remove("../.."); err == nil {
		t.Fatal("Remove() accepted a traversal name")
	}
}

func TestCreateLeavesNothingBehindWhenItFails(t *testing.T) {
	// A failed create must not hoard the space it half-consumed: on ENOSPC the
	// partial disk.img holds exactly the space that caused the failure, and the
	// next attempt then reports "already exists" for a VM that never existed.
	//
	// A directory as the source image fails inside io.Copy (EISDIR) -- after
	// MkdirAll and the O_EXCL open, which is the window that leaked.
	s := newTestStore(t)
	srcDir := t.TempDir()

	if err := s.createFromFile("dev", srcDir, 1<<20); err == nil {
		t.Fatal("Create() from a directory succeeded, want an error")
	}

	if _, err := os.Stat(s.Dir("dev")); !os.IsNotExist(err) {
		t.Errorf("failed Create left %s behind", s.Dir("dev"))
	}

	names, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 0 {
		t.Errorf("failed Create left a listed VM: %v", names)
	}
}

func TestCreateAfterAFailureIsNotBlocked(t *testing.T) {
	s := newTestStore(t)
	if err := s.createFromFile("dev", t.TempDir(), 1<<20); err == nil {
		t.Fatal("expected the seeding failure")
	}
	// The retry must not hit "VM already exists".
	good := filepath.Join(t.TempDir(), "image.wic")
	if err := os.WriteFile(good, []byte("image"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.createFromFile("dev", good, 1<<20); err != nil {
		t.Fatalf("retry after a failed Create = %v, want success", err)
	}
}

func TestCreateFromStreamsAnImage(t *testing.T) {
	// The published image is a compressed artifact; decompressing it to a
	// temporary 7 GiB file just to copy it again would double both the disk cost
	// and the wait, so Create has to accept a stream.
	s := newTestStore(t)
	const body = "streamed-image-bytes"

	if err := s.CreateFrom("dev", strings.NewReader(body), int64(len(body)), 32<<20, Meta{}); err != nil {
		t.Fatalf("CreateFrom() error = %v", err)
	}

	info, err := os.Stat(s.DiskPath("dev"))
	if err != nil {
		t.Fatalf("disk not created: %v", err)
	}
	if info.Size() != 32<<20 {
		t.Errorf("disk size = %d, want %d", info.Size(), 32<<20)
	}
	head := make([]byte, len(body))
	f, err := os.Open(s.DiskPath("dev"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.Read(head); err != nil {
		t.Fatal(err)
	}
	if string(head) != body {
		t.Errorf("disk head = %q, want %q", head, body)
	}
}

func TestCreateFromRefusesADiskSmallerThanTheImage(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateFrom("dev", strings.NewReader("0123456789"), 10, 4, Meta{}); err == nil {
		t.Fatal("CreateFrom() accepted a disk smaller than the image")
	}
	if _, err := os.Stat(s.Dir("dev")); !os.IsNotExist(err) {
		t.Error("rejected CreateFrom left a directory behind")
	}
}

func TestCreateFromCleansUpWhenTheStreamFails(t *testing.T) {
	s := newTestStore(t)
	failing := io.MultiReader(strings.NewReader("partial"), errReader{})
	if err := s.CreateFrom("dev", failing, 1024, 1<<20, Meta{}); err == nil {
		t.Fatal("CreateFrom() succeeded despite a failing stream")
	}
	if _, err := os.Stat(s.Dir("dev")); !os.IsNotExist(err) {
		t.Error("failed CreateFrom left a partial VM behind")
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("stream broke") }

func TestCheckCreatableRejectsBeforeAnyWork(t *testing.T) {
	// Called before a multi-gigabyte download, so an invalid name or an existing
	// VM costs nothing instead of costing the whole fetch.
	s := newTestStore(t)
	if err := s.CheckCreatable("Bad Name"); err == nil {
		t.Error("CheckCreatable accepted an invalid name")
	}
	if err := s.CheckCreatable("dev"); err != nil {
		t.Errorf("CheckCreatable(fresh) = %v, want nil", err)
	}

	src := filepath.Join(t.TempDir(), "image.wic")
	if err := os.WriteFile(src, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.createFromFile("dev", src, 1<<20); err != nil {
		t.Fatal(err)
	}
	if err := s.CheckCreatable("dev"); err == nil {
		t.Error("CheckCreatable accepted a name that already exists")
	}
}

func TestCreateFromRefusesToTruncateBelowWhatItWrote(t *testing.T) {
	// A compressed source reports uncompressedSize 0 when its size is not known
	// ahead of time (readImageSizeSidecar). The up-front guard cannot fire on 0,
	// so the written length is what must be checked before truncating -- else the
	// disk is cut below the image and reported as created.
	s := newTestStore(t)
	body := strings.Repeat("x", 4096)

	if err := s.CreateFrom("dev", strings.NewReader(body), 0, 1024, Meta{}); err == nil {
		t.Fatal("CreateFrom truncated the disk below the bytes it wrote")
	}
	if _, err := os.Stat(s.Dir("dev")); !os.IsNotExist(err) {
		t.Error("the rejected VM was left behind")
	}
}

func TestCreateFromAcceptsAnUnknownSizeThatFits(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateFrom("dev", strings.NewReader("small"), 0, 1<<20, Meta{}); err != nil {
		t.Fatalf("CreateFrom with unknown size that fits = %v, want nil", err)
	}
	info, err := os.Stat(s.DiskPath("dev"))
	if err != nil || info.Size() != 1<<20 {
		t.Errorf("disk = %v/%v, want 1 MiB", info, err)
	}
}

func TestCreateFromRefusesAnImageLargerThanTheDisk(t *testing.T) {
	// A compressed source reports size 0, so the up-front guard cannot fire.
	// Without a bound on the copy the whole stream lands on the host disk
	// before the size is rejected -- a decompression bomb fills the disk.
	s := newTestStore(t)
	const disk = 1 << 16
	oversize := strings.NewReader(strings.Repeat("A", disk*4))

	err := s.CreateFrom("dev", oversize, 0, disk, Meta{})
	if err == nil {
		t.Fatal("CreateFrom() accepted an image larger than the disk")
	}
	if _, statErr := os.Stat(s.Dir("dev")); !errors.Is(statErr, fs.ErrNotExist) {
		t.Errorf("failed create left %s behind: %v", s.Dir("dev"), statErr)
	}
	if n := oversize.Len(); n == 0 {
		t.Error("the whole oversized stream was consumed; the copy must stop at the disk size")
	}
}

func TestRemoveDeletesTheDiskWhileStillHoldingTheLock(t *testing.T) {
	// Remove has to release the lock before unlinking (Windows cannot delete an
	// open file). The disk therefore goes first, under the lock, so a start
	// that wins the lock in the gap fails its own existence check instead of
	// booting an image mid-deletion.
	s := newTestStore(t)
	createTestVM(t, s, "dev", Meta{})
	if err := s.Remove("dev"); err != nil {
		t.Fatalf("Remove() = %v", err)
	}
	for _, p := range []string{s.DiskPath("dev"), s.Dir("dev")} {
		if _, err := os.Stat(p); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s survived Remove: %v", p, err)
		}
	}
}
