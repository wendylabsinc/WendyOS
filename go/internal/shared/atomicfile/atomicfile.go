// Package atomicfile writes a file so that a power loss cannot leave a partial
// one behind.
//
// It is the implementation that used to live as services.syncWriteFile, lifted
// here unchanged so that code outside the agent's services package can share it
// rather than grow a second, subtly different copy. The agent writes private
// keys and certificates on embedded devices that lose power without warning,
// and a half-written key file is indistinguishable from a corrupt one.
package atomicfile

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

// Seams for the Windows-only branches below, which the Linux and macOS test
// runners cannot otherwise reach: the rename, the platform, and the rename
// retry's error classifier and clock.
var (
	renameFn             = os.Rename
	hostOS               = runtime.GOOS
	isRetryableRenameErr = retryableRenameErr
	sleepFn              = time.Sleep
	nowFn                = time.Now
)

// The Windows rename retry follows cmd/go's internal/robustio: keep retrying a
// transient refusal for up to renameRetryBudget, sleeping a little longer each
// time.
const (
	renameRetryBudget     = 2 * time.Second
	renameRetryFirstSleep = time.Millisecond
	renameRetryMaxSleep   = 500 * time.Millisecond
)

// geteuid and chown are seams so tests can exercise WritePreservingOwner's
// root-only chown below without actually running as root.
var (
	geteuid = os.Geteuid
	chown   = os.Chown
)

// Write atomically writes data to path: write to a temp file in the same
// directory, fsync it, rename it over the target, then fsync the directory so
// the rename itself is durable.
//
// perm is applied to the temp file before the rename, so the target never
// exists with a wider mode than asked for — which is why a 0o600 key file is
// never briefly world-readable.
//
// Write never changes the file's owner (see WritePreservingOwner for that):
// the device agent runs as root by design when it writes device keys and
// certificates, and a key file it creates should stay root-owned.
func Write(path string, data []byte, perm os.FileMode) error {
	return write(path, data, perm, nil)
}

// WritePreservingOwner is Write, except that when the caller is running as
// root it keeps path's existing owner (or, for a path that does not exist
// yet, its directory's owner) on the replacement file instead of leaving it
// root-owned.
//
// wendy re-execs itself under `sudo --preserve-env=HOME` for privileged
// operations (see commands.thorSudoPreserveEnv), so it keeps writing to the
// invoking user's ~/.wendy while running as root. A plain rename-based write
// there would leave config.json (or a lock file) root-owned, and every later
// unprivileged `wendy` command would then fail to read or lock its own
// config. This is the write path config.Save and flock.Acquire's lock file
// need; Write itself must stay owner-agnostic (see above).
func WritePreservingOwner(path string, data []byte, perm os.FileMode) error {
	return write(path, data, perm, preserveOwnerHook)
}

// preRenameHook runs against the temp file, just before it is renamed over
// path. A hook failure aborts the write and removes the temp file, same as
// any other failure in write.
type preRenameHook func(tmpName, path string) error

func write(path string, data []byte, perm os.FileMode, hook preRenameHook) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".pem-tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	removeOnFail := true
	tmpClosed := false
	defer func() {
		if !tmpClosed {
			_ = tmp.Close() // best-effort: file will be removed in error paths
		}
		if removeOnFail {
			os.Remove(tmpName)
		}
	}()

	if err := tmp.Chmod(perm); err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	tmpClosed = true

	if hook != nil {
		if err := hook(tmpName, path); err != nil {
			return err
		}
	}

	if err := replaceFile(tmpName, path); err != nil {
		return err
	}
	removeOnFail = false

	// Windows cannot fsync a directory: os.Open hands back a read-only handle
	// and FlushFileBuffers needs write access, so the sync below would fail
	// every write. NTFS journals the rename's metadata on its own.
	if hostOS == "windows" {
		return nil
	}

	// fsync the directory so the rename is durable on power loss. Open/close
	// failures are reported too: skipping the fsync silently would drop the
	// durability guarantee this helper exists to provide.
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open dir for fsync after rename: %w", err)
	}
	syncErr := d.Sync()
	closeErr := d.Close()
	if syncErr != nil {
		return fmt.Errorf("fsync dir after rename: %w", syncErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close dir after fsync: %w", closeErr)
	}
	return nil
}

// preserveOwnerHook chowns tmpName to path's current owner, or — when path
// does not exist yet — to the owner of its directory, so the rename below
// leaves the replacement file with the same owner the old file (or a fresh
// file in that directory) would have had. It is a no-op off root, on
// Windows (lookupOwner always reports ok=false there — ownership isn't
// chown-based), or when the owner it found is already root: nothing to fix
// up in either case.
func preserveOwnerHook(tmpName, path string) error {
	if geteuid() != 0 {
		return nil
	}
	uid, gid, ok := lookupOwner(path)
	if !ok {
		uid, gid, ok = lookupOwner(filepath.Dir(path))
	}
	if !ok || uid == 0 {
		return nil
	}
	return chown(tmpName, uid, gid)
}

// replaceFile renames tmp over path. On Windows the rename is refused while
// another process has path open — Go opens files without FILE_SHARE_DELETE,
// so a wendy CLI merely reading config.json blocks it — or while an antivirus
// scanner holds the fresh temp file. Like cmd/go's robustio, retry those
// transient refusals (retryableRenameErr) with growing sleeps for up to
// renameRetryBudget instead of failing the write, then return the last error.
// Elsewhere retryableRenameErr matches nothing, so this is a single rename.
func replaceFile(tmp, path string) error {
	err := renameFn(tmp, path)
	if err == nil || !isRetryableRenameErr(err) {
		return err
	}
	deadline := nowFn().Add(renameRetryBudget)
	sleep := renameRetryFirstSleep
	for {
		remaining := deadline.Sub(nowFn())
		if remaining <= 0 {
			return err
		}
		sleepFn(min(sleep, remaining))
		err = renameFn(tmp, path)
		if err == nil || !isRetryableRenameErr(err) {
			return err
		}
		sleep = min(2*sleep, renameRetryMaxSleep)
	}
}
