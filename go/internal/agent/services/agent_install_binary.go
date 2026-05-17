package services

import (
	"os"
	"path/filepath"

	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

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

// createUpdateTempFile creates a uniquely-named temp file in the same directory
// as execPath and sets it to the given permissions. Returns the open file (ready
// for writing), its path, and a cleanup func that closes and removes it.
func createUpdateTempFile(execPath string, perm os.FileMode) (*os.File, string, func(), error) {
	tmpFile, err := os.CreateTemp(filepath.Dir(execPath), ".agent-update-*")
	if err != nil {
		return nil, "", nil, status.Errorf(codes.Internal, "failed to create update temp file: %v", err)
	}
	tmpPath := tmpFile.Name()
	cleanup := func() { tmpFile.Close(); os.Remove(tmpPath) }

	if err := tmpFile.Chmod(perm); err != nil {
		cleanup()
		return nil, "", nil, status.Errorf(codes.Internal, "failed to set update file permissions: %v", err)
	}
	return tmpFile, tmpPath, cleanup, nil
}

// commitBinaryUpdate syncs and closes tmpFile, then atomically installs it over
// execPath via a backup rename. On success it returns the installed binary's
// size. The caller is responsible for removing tmpPath if this returns an error.
func commitBinaryUpdate(tmpFile *os.File, tmpPath, execPath, sha256Hash string, logger *zap.Logger) (int64, error) {
	if err := tmpFile.Sync(); err != nil {
		return 0, status.Errorf(codes.Internal, "failed to sync update file: %v", err)
	}
	if err := tmpFile.Close(); err != nil {
		return 0, status.Errorf(codes.Internal, "failed to close update file: %v", err)
	}

	backupPath := execPath + ".backup"
	if err := os.Rename(execPath, backupPath); err != nil {
		return 0, status.Errorf(codes.Internal, "failed to create backup: %v", err)
	}

	if err := os.Rename(tmpPath, execPath); err != nil {
		if rbErr := os.Rename(backupPath, execPath); rbErr != nil {
			logger.Error("Failed to rollback from backup",
				zap.Error(rbErr),
				zap.String("backup_path", backupPath),
			)
		}
		return 0, status.Errorf(codes.Internal, "failed to install update: %v", err)
	}

	// fsync the directory so the rename is durable on power loss.
	if dir, err := os.Open(filepath.Dir(execPath)); err == nil {
		if syncErr := dir.Sync(); syncErr != nil {
			logger.Warn("Failed to fsync update directory; rename may not survive power loss",
				zap.Error(syncErr))
		}
		dir.Close()
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
	return size, nil
}
