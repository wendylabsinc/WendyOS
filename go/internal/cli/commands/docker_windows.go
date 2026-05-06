//go:build windows

package commands

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
)

// linkOrCopyDir makes the contents of src reachable from dst on Windows.
// os.Symlink requires Developer Mode or admin, which a typical end-user
// doesn't have, so we try in order:
//
//  1. os.Symlink (works in Developer Mode / admin)
//  2. NTFS directory junction via DeviceIoControl(FSCTL_SET_REPARSE_POINT) —
//     no privileges required for local NTFS volumes
//  3. recursive copy — last-resort fallback for cross-volume targets, where
//     junctions are unsupported
//
// The junction is created via the Win32 API rather than `cmd.exe /C mklink`
// so that paths containing cmd metacharacters (`&`, `|`, `%`, `^`) are passed
// to the kernel as literals rather than being reparsed by the shell.
func linkOrCopyDir(src, dst string) error {
	if err := os.Symlink(src, dst); err == nil {
		return nil
	}
	if err := makeJunction(src, dst); err == nil {
		return nil
	}
	if err := copyDir(src, dst); err != nil {
		return fmt.Errorf("link/junction/copy all failed for %s: %w", src, err)
	}
	return nil
}

// makeJunction creates an NTFS directory junction at link that points to
// target. The implementation calls FSCTL_SET_REPARSE_POINT directly so the
// target path is never interpreted by a shell.
//
// Junctions only support absolute, local NTFS paths; the function resolves
// target to absolute and bails out (without touching the filesystem on
// failure paths past the mkdir) if any step rejects it.
func makeJunction(target, link string) (retErr error) {
	abs, err := filepath.Abs(target)
	if err != nil {
		return fmt.Errorf("resolving junction target: %w", err)
	}

	if err := os.Mkdir(link, 0o755); err != nil {
		return fmt.Errorf("creating junction directory: %w", err)
	}
	defer func() {
		if retErr != nil {
			_ = os.Remove(link)
		}
	}()

	linkUTF16, err := windows.UTF16PtrFromString(link)
	if err != nil {
		return fmt.Errorf("encoding junction link path: %w", err)
	}

	handle, err := windows.CreateFile(
		linkUTF16,
		windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS,
		0,
	)
	if err != nil {
		return fmt.Errorf("opening junction directory: %w", err)
	}
	defer windows.CloseHandle(handle)

	buf, err := buildMountPointReparseBuffer(abs)
	if err != nil {
		return err
	}

	var bytesReturned uint32
	if err := windows.DeviceIoControl(
		handle,
		windows.FSCTL_SET_REPARSE_POINT,
		&buf[0],
		uint32(len(buf)),
		nil,
		0,
		&bytesReturned,
		nil,
	); err != nil {
		return fmt.Errorf("setting reparse point: %w", err)
	}
	return nil
}

// buildMountPointReparseBuffer serializes a REPARSE_DATA_BUFFER for an
// IO_REPARSE_TAG_MOUNT_POINT (junction). Layout per WinSDK ntifs.h:
//
//	ULONG  ReparseTag                  // 4
//	USHORT ReparseDataLength           // 2
//	USHORT Reserved                    // 2
//	USHORT SubstituteNameOffset        // 2
//	USHORT SubstituteNameLength        // 2
//	USHORT PrintNameOffset             // 2
//	USHORT PrintNameLength             // 2
//	WCHAR  PathBuffer[]                // substitute + NUL + print + NUL
//
// The substitute name is the NT-namespace form (\??\C:\...). The print name
// is the friendly Win32 path. Both lengths are byte counts excluding the NUL.
func buildMountPointReparseBuffer(targetAbs string) ([]byte, error) {
	subst := utf16.Encode([]rune(`\??\` + targetAbs))
	print := utf16.Encode([]rune(targetAbs))
	if len(subst) == 0 {
		return nil, errors.New("empty junction target")
	}

	substBytes := len(subst) * 2
	printBytes := len(print) * 2
	const headerSize = 16 // tag + len + reserved + 4×USHORT
	const nameOverhead = 4
	bufSize := headerSize + substBytes + printBytes + nameOverhead
	if bufSize > 0xFFFF {
		return nil, errors.New("junction target path too long")
	}

	buf := make([]byte, bufSize)
	binary.LittleEndian.PutUint32(buf[0:4], windows.IO_REPARSE_TAG_MOUNT_POINT)
	binary.LittleEndian.PutUint16(buf[4:6], uint16(bufSize-8)) // ReparseDataLength
	// buf[6:8] Reserved = 0
	binary.LittleEndian.PutUint16(buf[8:10], 0)                          // SubstituteNameOffset
	binary.LittleEndian.PutUint16(buf[10:12], uint16(substBytes))        // SubstituteNameLength
	binary.LittleEndian.PutUint16(buf[12:14], uint16(substBytes+2))      // PrintNameOffset
	binary.LittleEndian.PutUint16(buf[14:16], uint16(printBytes))        // PrintNameLength

	pathBuf := unsafe.Slice((*uint16)(unsafe.Pointer(&buf[headerSize])), (bufSize-headerSize)/2)
	copy(pathBuf, subst)
	// pathBuf[len(subst)] is implicit NUL
	copy(pathBuf[len(subst)+1:], print)
	// trailing NUL is implicit
	return buf, nil
}

func copyDir(src, dst string) error {
	srcInfo, err := os.Stat(src)
	if err != nil {
		return err
	}
	if !srcInfo.IsDir() {
		return errors.New("copyDir: source is not a directory")
	}
	if err := os.MkdirAll(dst, srcInfo.Mode()); err != nil {
		return err
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		srcPath := filepath.Join(src, entry.Name())
		dstPath := filepath.Join(dst, entry.Name())
		if entry.IsDir() {
			if err := copyDir(srcPath, dstPath); err != nil {
				return err
			}
			continue
		}
		if err := copyFile(srcPath, dstPath); err != nil {
			return err
		}
	}
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Sync()
}
