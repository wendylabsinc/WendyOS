package commands

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	wendymcp "github.com/wendylabsinc/wendy/go/internal/cli/mcp"
	"github.com/wendylabsinc/wendy/go/internal/cli/vm"
)

func TestVMStorageReportsCapacityAndVolumeBeforeDownload(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv("WENDY_CONFIG_DIR", configDir)
	oldAvailable, oldTransport := vmDiskAvailBytes, http.DefaultTransport
	t.Cleanup(func() { vmDiskAvailBytes, http.DefaultTransport = oldAvailable, oldTransport })
	vmDiskAvailBytes = func(path string) (int64, bool) {
		if path != configDir {
			t.Fatalf("queried volume at %s, want %s", path, configDir)
		}
		return 8 << 30, true
	}
	var out strings.Builder
	requested := false
	http.DefaultTransport = vmImageTestTransport(func(*http.Request) (*http.Response, error) {
		requested = true
		for _, want := range []string{"16.0 GiB virtual capacity", "8.0 GiB free", filepath.Join(configDir, "vms", "test1", "disk.img"), "decompressed image"} {
			if !strings.Contains(out.String(), want) {
				t.Errorf("before-download output missing %q: %s", want, out.String())
			}
		}
		return nil, errors.New("stop before downloading the image")
	})
	if err := createVMContext(context.Background(), &out, "test1", "", "", 16, false, 0); err == nil || !requested {
		t.Fatalf("create error = %v, request started = %v", err, requested)
	}
}

func TestVMImageSpaceUsesKnownImageSize(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "vms", "test1", "disk.img")
	old := vmDiskAvailBytes
	t.Cleanup(func() { vmDiskAvailBytes = old })
	available, known := int64(1<<30), true
	vmDiskAvailBytes = func(queried string) (int64, bool) {
		if queried != root {
			t.Fatalf("query = %s, want closest existing parent %s", queried, root)
		}
		return available, known
	}
	err := checkVMImageSpace(path, 2<<30)
	for _, want := range []string{path, "needs at least 2.0 GiB", "only 1.0 GiB is free"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("space error = %v, want %q", err, want)
		}
	}
	if err := checkVMImageSpace(path, 1<<30); err != nil {
		t.Fatal(err)
	}
	available = 0
	if err := checkVMImageSpace(path, 0); err != nil {
		t.Fatalf("unknown image size rejected: %v", err)
	}
	known = false
	if err := checkVMImageSpace(path, 2<<30); err != nil {
		t.Fatalf("unavailable volume information rejected: %v", err)
	}
	permission := fmt.Errorf("write failed: %w", os.ErrPermission)
	if err := vmStorageError(permission, path, 2<<30, 16<<30); !errors.Is(err, permission) || err.Error() != permission.Error() {
		t.Fatalf("non-space failure changed: %v", err)
	}
}

func TestVMCreateChecksImageSpaceBeforeWriting(t *testing.T) {
	for _, mode := range []string{"cli", "mcp"} {
		t.Run(mode, func(t *testing.T) {
			configDir := t.TempDir()
			t.Setenv("WENDY_CONFIG_DIR", configDir)
			store := &vm.Store{Root: filepath.Join(configDir, "vms")}
			image := filepath.Join(t.TempDir(), "source.wic")
			if err := os.WriteFile(image, []byte("image data"), 0600); err != nil {
				t.Fatal(err)
			}
			old := vmDiskAvailBytes
			t.Cleanup(func() { vmDiskAvailBytes = old })
			vmDiskAvailBytes = func(string) (int64, bool) { return 0, true }
			var err error
			if mode == "cli" {
				err = createVMContext(context.Background(), io.Discard, "test1", image, "", 16, false, 0)
			} else {
				_, err = createMCPSimulator(context.Background(), store, wendymcp.SimulatorCreateOptions{
					Name: "test1", Profile: "generic", Image: image, DiskGiB: 16,
				}, nil)
			}
			if err == nil || !strings.Contains(err.Error(), "not enough free disk space") {
				t.Fatalf("create = %v", err)
			}
			if _, err := os.Stat(store.Dir("test1")); !os.IsNotExist(err) {
				t.Fatalf("preflight wrote a partial VM: %v", err)
			}
		})
	}
}

func TestVMCreateRejectsInvalidCapacityBeforeDownload(t *testing.T) {
	t.Setenv("WENDY_CONFIG_DIR", t.TempDir())
	for _, diskGiB := range []int{-1, 0, int(math.MaxInt64>>30) + 1} {
		if err := createVMContext(context.Background(), io.Discard, "test1", "", "", diskGiB, false, 0); err == nil || !strings.Contains(err.Error(), "VM disk size must be between") {
			t.Fatalf("disk %d: %v", diskGiB, err)
		}
	}
}
