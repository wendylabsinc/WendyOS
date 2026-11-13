//go:build windows

package vm

import (
	"os"
	"strings"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

func TestCreateWindowsDiskAndVarsAreSparse(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateFrom("test1", strings.NewReader("image"), 5, 128<<20, Meta{}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{s.DiskPath("test1"), s.VarsPath("test1")} {
		t.Run(path, func(t *testing.T) {
			f, err := os.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			handle := windows.Handle(f.Fd())
			var flags uint32
			if err := windows.GetVolumeInformationByHandle(handle, nil, 0, nil, nil, &flags, nil, 0); err != nil {
				t.Fatal(err)
			}
			if flags&windows.FILE_SUPPORTS_SPARSE_FILES == 0 {
				t.Skip("test volume does not support sparse files")
			}
			// FILE_STANDARD_INFO requires an aligned buffer for its LARGE_INTEGER fields.
			var info struct {
				AllocationSize int64
				EndOfFile      int64
				NumberOfLinks  uint32
				DeletePending  byte
				Directory      byte
			}
			if err := windows.GetFileInformationByHandleEx(handle, windows.FileStandardInfo, (*byte)(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
				t.Fatal(err)
			}
			allocated, size := info.AllocationSize, info.EndOfFile
			if allocated >= size {
				t.Fatalf("%s allocated %d bytes for %d bytes of capacity", path, allocated, size)
			}
			var tail [1]byte
			if _, err := f.ReadAt(tail[:], int64(size)-1); err != nil || tail[0] != 0 {
				t.Fatalf("extended range is not zero: %v, %v", tail, err)
			}
		})
	}
}
