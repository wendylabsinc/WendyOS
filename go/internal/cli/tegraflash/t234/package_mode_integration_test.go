//go:build darwin || linux || windows

package t234

import (
	"crypto/sha256"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"
)

// Supply an unmarked, production flashpkg.ext4 to exercise the actual editor
// against independent e2fsprogs readers. No block device or mount is used.
func TestPrepareProductionPackage(t *testing.T) {
	source := os.Getenv("WENDY_T234_TEST_PACKAGE")
	if source == "" {
		t.Skip("set WENDY_T234_TEST_PACKAGE to a production command image")
	}
	for _, tool := range []string{"e2fsck", "debugfs"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Fatalf("integration check requires %s: %v", tool, err)
		}
	}
	before := fileHash(t, source)
	prepared, mode, err := prepareUSBMode(source, t.TempDir(), true)
	if err != nil {
		t.Fatal(err)
	}
	if prepared == source || mode != USBModeSingle {
		t.Fatal("expected a private single-mode copy")
	}
	if out, err := exec.Command("e2fsck", "-fn", prepared).CombinedOutput(); err != nil {
		t.Fatalf("prepared filesystem: %v\n%s", err, out)
	}
	inventory := func(path string) map[string][32]byte {
		dir := t.TempDir()
		if out, err := exec.Command("debugfs", "-R", "rdump /flashpkg "+strconv.Quote(dir), path).CombinedOutput(); err != nil {
			t.Fatalf("extracting package: %v\n%s", err, out)
		}
		files := make(map[string][32]byte)
		err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.Type().IsRegular() {
				rel, err := filepath.Rel(dir, path)
				if err != nil {
					return err
				}
				files[filepath.ToSlash(rel)] = fileHash(t, path)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(files) == 0 {
			t.Fatal("independent reader extracted no files")
		}
		return files
	}
	original, edited := inventory(source), inventory(prepared)
	wantMarker := sha256.Sum256([]byte("single\n"))
	if edited[usbModePath] != wantMarker {
		t.Fatal("independent reader did not find the single marker")
	}
	delete(original, usbModePath)
	delete(edited, usbModePath)
	if !reflect.DeepEqual(original, edited) {
		t.Fatal("editing USB mode changed other package files")
	}
	if fileHash(t, source) != before {
		t.Fatal("cached production package changed")
	}
	t.Logf("e2fsck clean; all %d original payload files preserved; source hash unchanged", len(original))
}

func fileHash(t *testing.T, path string) [32]byte {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		t.Fatal(err)
	}
	var sum [32]byte
	copy(sum[:], h.Sum(nil))
	return sum
}
