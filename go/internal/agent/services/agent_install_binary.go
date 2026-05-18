package services

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ErrDirFsync is returned by commitBinaryUpdate when the post-rename directory
// fsync fails. The binary IS installed at this point; only power-loss durability
// of the directory entry is at risk. Callers should log a warning and continue.
var ErrDirFsync = errors.New("dir fsync after rename failed")

// maxAgentBinarySize bounds how many bytes an update stream may write before
// the agent aborts it. Update chunks are written straight to a temp file in
// the executable directory before any size is known, so without this cap a
// stuck, buggy, or malicious client could stream unbounded data and fill the
// partition holding the live binary and runtime state. The agent binary is
// far smaller than this; the cap only needs headroom for legitimate growth.
const maxAgentBinarySize = 256 * 1024 * 1024 // 256 MiB

// resolveExecPath returns the canonicalised path of the running binary.
func resolveExecPath() (string, os.FileMode, error) {
	execPath, err := os.Executable()
	if err != nil {
		return "", 0, status.Errorf(codes.Internal, "failed to get executable path: %v", err)
	}
	execPath, err = filepath.EvalSymlinks(execPath)
	if err != nil {
		return "", 0, status.Errorf(codes.Internal, "failed to resolve executable symlinks: %v", err)
	}
	info, err := os.Stat(execPath)
	if err != nil {
		return "", 0, status.Errorf(codes.Internal, "failed to stat executable: %v", err)
	}
	return execPath, info.Mode(), nil
}

// cleanStaleTempFiles removes any .agent-update-* temp files left behind by
// previous aborted updates. Called at the start of each update attempt so that
// repeated crashed updates cannot fill the filesystem with agent-sized files.
func cleanStaleTempFiles(dir string) {
	matches, _ := filepath.Glob(filepath.Join(dir, ".agent-update-*"))
	for _, m := range matches {
		os.Remove(m) //nolint:errcheck — best-effort cleanup
	}
}

// createUpdateTempFile creates a uniquely-named temp file (mode 0600) in the
// same directory as execPath. Returns the open file (ready for writing), its
// path, and a cleanup func that closes and removes it. The final binary
// permissions are applied by commitBinaryUpdate after hash verification so that
// a partial or unverified binary is never executable.
func createUpdateTempFile(execPath string) (*os.File, string, func(), error) {
	dir := filepath.Dir(execPath)

	cleanStaleTempFiles(dir)

	// Refuse to write an update binary into a world- or group-writable directory.
	// Either condition allows a local attacker in the same group to unlink or replace
	// the temp file between creation and the final rename.
	if info, err := os.Stat(dir); err != nil {
		return nil, "", nil, status.Errorf(codes.Internal, "failed to stat binary directory: %v", err)
	} else if info.Mode()&0o022 != 0 {
		return nil, "", nil, status.Error(codes.FailedPrecondition, "agent binary directory is world- or group-writable; refusing update")
	}

	// os.CreateTemp creates with mode 0600 — not executable until commitBinaryUpdate.
	tmpFile, err := os.CreateTemp(dir, ".agent-update-*")
	if err != nil {
		return nil, "", nil, status.Errorf(codes.Internal, "failed to create update temp file: %v", err)
	}
	tmpPath := tmpFile.Name()
	cleanup := func() { _ = tmpFile.Close(); _ = os.Remove(tmpPath) }
	return tmpFile, tmpPath, cleanup, nil
}

// commitBinaryUpdate sets the file permissions, fsyncs, and closes tmpFile,
// then atomically installs it over execPath via a single rename(2). Hash is
// only passed for logging. Returns the installed size, or ErrDirFsync if the
// post-rename directory fsync fails (binary IS installed in that case).
func commitBinaryUpdate(tmpFile *os.File, tmpPath, execPath, sha256Hash string, perm os.FileMode, logger *zap.Logger) (int64, error) {
	// Apply final permissions before the fsync so that both data and the
	// executable permission bits are durable before the rename. If chmod
	// happened after Sync a power loss between chmod and rename could
	// leave an installed binary that is non-executable (still at 0600).
	if err := os.Chmod(tmpPath, perm); err != nil {
		return 0, status.Errorf(codes.Internal, "failed to set update file permissions: %v", err)
	}
	if err := tmpFile.Sync(); err != nil {
		return 0, status.Errorf(codes.Internal, "failed to sync update file: %v", err)
	}
	if err := tmpFile.Close(); err != nil {
		return 0, status.Errorf(codes.Internal, "failed to close update file: %v", err)
	}

	// Single atomic rename over the live binary — no intermediate backup so
	// execPath is never absent between two renames.
	if err := os.Rename(tmpPath, execPath); err != nil {
		return 0, status.Errorf(codes.Internal, "failed to install update: %v", err)
	}

	info, _ := os.Stat(execPath)
	var size int64
	if info != nil {
		size = info.Size()
	}
	logger.Info("Agent binary updated successfully",
		zap.String("sha256", sha256Hash),
		zap.Int64("size", size),
	)

	// fsync the directory so the rename is durable on power loss.
	// Return ErrDirFsync if any step fails; the binary IS installed — callers
	// should log a warning and proceed rather than treating this as fatal.
	// Open/close failures are surfaced too, otherwise the durability guarantee
	// would be lost silently.
	dir, err := os.Open(filepath.Dir(execPath))
	if err != nil {
		return size, fmt.Errorf("%w: open dir: %v", ErrDirFsync, err)
	}
	syncErr := dir.Sync()
	closeErr := dir.Close()
	if syncErr != nil {
		return size, fmt.Errorf("%w: %v", ErrDirFsync, syncErr)
	}
	if closeErr != nil {
		return size, fmt.Errorf("%w: close dir: %v", ErrDirFsync, closeErr)
	}

	return size, nil
}
