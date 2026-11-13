//go:build darwin || linux

package commands

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// runHelperAsRoot makes the helper's input check see this process as root
// with sudo's SUDO_UID set to sudoUID ("" for root that sudo didn't start).
func runHelperAsRoot(t *testing.T, sudoUID string) {
	t.Helper()
	old := helperInputEUID
	helperInputEUID = func() int { return 0 }
	t.Cleanup(func() { helperInputEUID = old })
	t.Setenv("SUDO_UID", sudoUID)
	t.Setenv("SUDO_GID", sudoUID)
}

// runHelperAsRootFor makes the helper's input check see this process as root
// started by sudo from uid.
func runHelperAsRootFor(t *testing.T, uid int) {
	t.Helper()
	runHelperAsRoot(t, strconv.Itoa(uid))
}

// skipIfRoot skips tests that need a file this process can't own: as root,
// sudo from the test's own uid is sudo from root, which the check allows.
func skipIfRoot(t *testing.T) {
	t.Helper()
	if os.Getuid() == 0 {
		t.Skip("needs a non-root test user")
	}
}

func writeTestFile(t *testing.T, path string, data []byte) string {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func copySampleBmap(t *testing.T, dir string) string {
	t.Helper()
	data, err := os.ReadFile("testdata/sample.bmap")
	if err != nil {
		t.Fatal(err)
	}
	return writeTestFile(t, filepath.Join(dir, "image.bmap"), data)
}

// Linux sudo resets HOME to /root, so the helper must accept the user's cached
// image without deriving anything from HOME.
func TestBmapWriteSeekableIgnoresHOME(t *testing.T) {
	cache := t.TempDir()
	bmap := copySampleBmap(t, cache)
	// 10000 bytes against the bmap's 32768: the helper stops right after it has
	// read both inputs, before it opens the device.
	src := writeTestFile(t, filepath.Join(cache, "image.img.zst"), encodeSeekable(t, make([]byte, 10_000), 4096))
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", "")
	runHelperAsRootFor(t, os.Getuid())

	err := runBmapWriteSeekable("/dev/null", bmap, src, 0, 0, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "seekable image size 10000 != bmap image size 32768") {
		t.Fatalf("runBmapWriteSeekable = %v, want it to read the source and stop at the size check", err)
	}
}

func TestOpenHelperInput(t *testing.T) {
	dir := t.TempDir()
	file := writeTestFile(t, filepath.Join(dir, "image.img.zst"), []byte("x"))
	link := filepath.Join(dir, "link.img.zst")
	if err := os.Symlink("/etc/passwd", link); err != nil {
		t.Fatal(err)
	}

	uid := strconv.Itoa(os.Getuid())
	tests := []struct {
		name        string
		path        string
		sudoUID     string
		needNonRoot bool
		wantRefused bool
	}{
		{"file the sudo user owns", file, uid, true, false},
		{"file another user owns", file, strconv.Itoa(os.Getuid() + 1), false, true},
		{"symlink to a file the sudo user doesn't own", link, uid, true, true},
		{"directory the sudo user owns", dir, uid, true, true},
		{"sudo from root", file, "0", false, false},
		// A sudo shim that sets no SUDO_UID must keep the fast write.
		{"root without SUDO_UID", file, "", false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.needNonRoot {
				skipIfRoot(t)
			}
			runHelperAsRoot(t, tt.sudoUID)
			f, err := openHelperInput(tt.path)
			if f != nil {
				f.Close()
			}
			if got := errors.Is(err, errHelperInputRefused); got != tt.wantRefused {
				t.Fatalf("openHelperInput(%s) = %v, refused %v, want refused %v", tt.path, err, got, tt.wantRefused)
			}
			if !tt.wantRefused && err != nil {
				t.Fatalf("openHelperInput(%s) = %v, want nil", tt.path, err)
			}
		})
	}
}

// Opening a FIFO blocks until a writer appears, and opening a device as root
// can have side effects, so the type is checked before the file is opened.
func TestOpenHelperInputRefusesFIFOWithoutOpeningIt(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), "image.img.zst")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	runHelperAsRootFor(t, os.Getuid())

	done := make(chan error, 1)
	go func() {
		f, err := openHelperInput(fifo)
		if f != nil {
			f.Close()
		}
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, errHelperInputRefused) {
			t.Fatalf("openHelperInput(fifo) = %v, want refused", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("openHelperInput blocked opening a FIFO")
	}
}

// A path swapped after the first check must still not be read: the opened
// file is checked again.
func TestOpenHelperInputRechecksTheOpenedFile(t *testing.T) {
	skipIfRoot(t)
	path := writeTestFile(t, filepath.Join(t.TempDir(), "image.img.zst"), []byte("x"))
	runHelperAsRootFor(t, os.Getuid())
	old := helperInputOpen
	helperInputOpen = func(name string) (*os.File, error) {
		if err := os.Remove(name); err != nil {
			return nil, err
		}
		if err := os.Symlink("/etc/passwd", name); err != nil {
			return nil, err
		}
		return old(name)
	}
	t.Cleanup(func() { helperInputOpen = old })

	f, err := openHelperInput(path)
	if f != nil {
		f.Close()
	}
	if !errors.Is(err, errHelperInputRefused) {
		t.Fatalf("openHelperInput after a swap = %v, want refused", err)
	}
}

// The root helper reads --bmap as well as --source, so both are checked.
func TestBmapHelperRefusesBmapTheSudoUserDoesNotOwn(t *testing.T) {
	bmap := copySampleBmap(t, t.TempDir())
	runHelperAsRootFor(t, os.Getuid()+1)

	// A missing source keeps the seekable case from being refused for the
	// source instead of the bmap.
	missing := filepath.Join(t.TempDir(), "missing.img.zst")
	if err := runBmapWriteSeekable("/dev/null", bmap, missing, 0, 0, io.Discard); !errors.Is(err, errHelperInputRefused) {
		t.Errorf("runBmapWriteSeekable = %v, want the bmap refused", err)
	}
	if err := runBmapWrite("/dev/null", bmap, bytes.NewReader(nil), 0); !errors.Is(err, errHelperInputRefused) {
		t.Errorf("runBmapWrite = %v, want the bmap refused", err)
	}
}

func TestBmapWriteSeekableRefusesSourceTheSudoUserDoesNotOwn(t *testing.T) {
	skipIfRoot(t)
	dir := t.TempDir()
	bmap := copySampleBmap(t, dir)
	src := filepath.Join(dir, "image.img.zst")
	if err := os.Symlink("/etc/passwd", src); err != nil {
		t.Fatal(err)
	}
	runHelperAsRootFor(t, os.Getuid())

	if err := runBmapWriteSeekable("/dev/null", bmap, src, 0, 0, io.Discard); !errors.Is(err, errHelperInputRefused) {
		t.Fatalf("runBmapWriteSeekable = %v, want the source refused", err)
	}
}
