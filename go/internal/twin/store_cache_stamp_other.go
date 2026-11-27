//go:build !darwin && !linux

package twin

import "os"

// Do not retain snapshots where we cannot detect an in-place rewrite whose
// modification time was restored by another process.
func roomFileChange(os.FileInfo) ([2]int64, bool) { return [2]int64{}, false }
