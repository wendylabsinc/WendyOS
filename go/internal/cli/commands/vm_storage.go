package commands

import (
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"

	"github.com/wendylabsinc/wendy/go/internal/cli/vm"
)

var vmDiskAvailBytes = diskAvailBytes

func vmDiskBytes(diskGiB int) (int64, error) {
	if diskGiB <= 0 || int64(diskGiB) > math.MaxInt64>>30 {
		return 0, fmt.Errorf("VM disk size must be between 1 and %d GiB", math.MaxInt64>>30)
	}
	return int64(diskGiB) << 30, nil
}

// A new VM's directory does not exist yet. Query its closest existing parent,
// which also respects a configured store on a different volume from the home.
func vmStorageAvailable(path string) (int64, bool) {
	for {
		if _, err := os.Stat(path); err == nil {
			return vmDiskAvailBytes(path)
		} else if !os.IsNotExist(err) {
			return 0, false
		}
		parent := filepath.Dir(path)
		if parent == path {
			return 0, false
		}
		path = parent
	}
}

func reportVMStorage(out io.Writer, path string, diskBytes int64) {
	fmt.Fprintf(out, "VM disk: %.1f GiB virtual capacity at %s", float64(diskBytes)/(1<<30), path)
	if available, ok := vmStorageAvailable(filepath.Dir(path)); ok {
		fmt.Fprintf(out, "; %.1f GiB free on its volume", float64(available)/(1<<30))
	}
	fmt.Fprintln(out, ".")
	fmt.Fprintln(out, "Allow space for the decompressed image, any download, and later VM disk growth. Sparse files allocate space as data is written.")
}

// Only reject a known image requirement. Compressed images may not declare
// their unpacked size; the virtual disk capacity is not an allocation estimate.
func checkVMImageSpace(path string, imageBytes int64) error {
	if imageBytes <= 0 {
		return nil
	}
	available, ok := vmStorageAvailable(filepath.Dir(path))
	if !ok || available >= imageBytes {
		return nil
	}
	return fmt.Errorf("not enough free disk space for VM image at %s: needs at least %.1f GiB, but only %.1f GiB is free; free up space and try again",
		path, float64(imageBytes)/(1<<30), float64(available)/(1<<30))
}

func vmStorageError(err error, path string, imageBytes, diskBytes int64) error {
	if !vm.IsDiskFull(err) {
		return err
	}
	if spaceErr := checkVMImageSpace(path, imageBytes); spaceErr != nil {
		return fmt.Errorf("%w: %w", spaceErr, err)
	}
	detail := fmt.Sprintf("free up space on the volume containing %s (%.1f GiB virtual disk capacity)", path, float64(diskBytes)/(1<<30))
	if available, ok := vmStorageAvailable(filepath.Dir(path)); ok {
		detail += fmt.Sprintf("; %.1f GiB is currently free", float64(available)/(1<<30))
	}
	return fmt.Errorf("%w; %s and try again", err, detail)
}
