package services

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.uber.org/zap"
)

func TestCleanStaleTempFiles(t *testing.T) {
	dir := t.TempDir()

	stale := []string{
		filepath.Join(dir, ".agent-update-abc"),
		filepath.Join(dir, ".agent-update-xyz123"),
	}
	for _, p := range stale {
		if err := os.WriteFile(p, []byte("stale"), 0o600); err != nil {
			t.Fatalf("create stale file: %v", err)
		}
	}
	keep := filepath.Join(dir, "agent")
	if err := os.WriteFile(keep, []byte("keep"), 0o755); err != nil {
		t.Fatalf("create keep file: %v", err)
	}

	cleanStaleTempFiles(dir)

	for _, p := range stale {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("stale file %s should have been removed", filepath.Base(p))
		}
	}
	if _, err := os.Stat(keep); err != nil {
		t.Errorf("non-stale file unexpectedly removed: %v", err)
	}
}

func TestCreateUpdateTempFile_CreatesInBinaryDir(t *testing.T) {
	dir := t.TempDir()
	execPath := filepath.Join(dir, "agent")
	if err := os.WriteFile(execPath, []byte("bin"), 0o755); err != nil {
		t.Fatalf("setup: %v", err)
	}

	tmpFile, tmpPath, cleanup, err := createUpdateTempFile(execPath)
	if err != nil {
		t.Fatalf("createUpdateTempFile: %v", err)
	}
	defer cleanup()

	if filepath.Dir(tmpPath) != dir {
		t.Errorf("temp file not in binary dir: got %s, want %s", filepath.Dir(tmpPath), dir)
	}
	if !strings.HasPrefix(filepath.Base(tmpPath), ".agent-update-") {
		t.Errorf("unexpected temp file name: %s", filepath.Base(tmpPath))
	}
	if info, err := tmpFile.Stat(); err != nil {
		t.Fatalf("stat temp file: %v", err)
	} else if info.Mode()&0o177 != 0 {
		t.Errorf("temp file should be mode 0600, got %o", info.Mode()&0o777)
	}
}

func TestCreateUpdateTempFile_CleanupRemovesFile(t *testing.T) {
	dir := t.TempDir()
	execPath := filepath.Join(dir, "agent")
	if err := os.WriteFile(execPath, []byte("bin"), 0o755); err != nil {
		t.Fatalf("setup: %v", err)
	}

	_, tmpPath, cleanup, err := createUpdateTempFile(execPath)
	if err != nil {
		t.Fatalf("createUpdateTempFile: %v", err)
	}
	if _, err := os.Stat(tmpPath); err != nil {
		t.Fatalf("temp file should exist before cleanup: %v", err)
	}

	cleanup()

	if _, err := os.Stat(tmpPath); !os.IsNotExist(err) {
		t.Error("temp file should be removed after cleanup")
	}
}

func TestCommitBinaryUpdate_Success(t *testing.T) {
	dir := t.TempDir()
	execPath := filepath.Join(dir, "agent")
	if err := os.WriteFile(execPath, []byte("old"), 0o755); err != nil {
		t.Fatalf("setup: %v", err)
	}

	tmpFile, tmpPath, cleanup, err := createUpdateTempFile(execPath)
	if err != nil {
		t.Fatalf("createUpdateTempFile: %v", err)
	}
	defer cleanup()

	content := []byte("updated binary content")
	if _, err := tmpFile.Write(content); err != nil {
		t.Fatalf("write content: %v", err)
	}

	size, err := commitBinaryUpdate(tmpFile, tmpPath, execPath, "abc123", 0o755, zap.NewNop())
	if err != nil && !errors.Is(err, ErrDirFsync) {
		t.Fatalf("commitBinaryUpdate: %v", err)
	}
	if size != int64(len(content)) {
		t.Errorf("size: got %d, want %d", size, int64(len(content)))
	}

	installed, err := os.ReadFile(execPath)
	if err != nil {
		t.Fatalf("read installed binary: %v", err)
	}
	if string(installed) != string(content) {
		t.Errorf("content mismatch: got %q, want %q", installed, content)
	}

	info, err := os.Stat(execPath)
	if err != nil {
		t.Fatalf("stat installed binary: %v", err)
	}
	if info.Mode()&0o777 != 0o755 {
		t.Errorf("permissions: got %o, want %o", info.Mode()&0o777, 0o755)
	}
}

func TestCommitBinaryUpdate_PermissionsPreserved(t *testing.T) {
	dir := t.TempDir()
	execPath := filepath.Join(dir, "agent")
	if err := os.WriteFile(execPath, []byte("old"), 0o750); err != nil {
		t.Fatalf("setup: %v", err)
	}

	tmpFile, tmpPath, cleanup, err := createUpdateTempFile(execPath)
	if err != nil {
		t.Fatalf("createUpdateTempFile: %v", err)
	}
	defer cleanup()

	if _, err := tmpFile.Write([]byte("new")); err != nil {
		t.Fatalf("write: %v", err)
	}

	if _, err := commitBinaryUpdate(tmpFile, tmpPath, execPath, "hash", 0o750, zap.NewNop()); err != nil && !errors.Is(err, ErrDirFsync) {
		t.Fatalf("commitBinaryUpdate: %v", err)
	}

	info, err := os.Stat(execPath)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode()&0o777 != 0o750 {
		t.Errorf("permissions: got %o, want %o", info.Mode()&0o777, 0o750)
	}
}

func TestCommitBinaryUpdate_OldBinaryReplacedAtomically(t *testing.T) {
	dir := t.TempDir()
	execPath := filepath.Join(dir, "agent")
	if err := os.WriteFile(execPath, []byte("original"), 0o755); err != nil {
		t.Fatalf("setup: %v", err)
	}

	tmpFile, tmpPath, cleanup, err := createUpdateTempFile(execPath)
	if err != nil {
		t.Fatalf("createUpdateTempFile: %v", err)
	}
	defer cleanup()

	if _, err := tmpFile.Write([]byte("replacement")); err != nil {
		t.Fatalf("write: %v", err)
	}

	if _, err := commitBinaryUpdate(tmpFile, tmpPath, execPath, "hash", 0o755, zap.NewNop()); err != nil && !errors.Is(err, ErrDirFsync) {
		t.Fatalf("commitBinaryUpdate: %v", err)
	}

	// After commit the tmp path should no longer exist (it was renamed to execPath).
	if _, err := os.Stat(tmpPath); !os.IsNotExist(err) {
		t.Error("temp file should not exist after successful commit")
	}

	got, _ := os.ReadFile(execPath)
	if string(got) != "replacement" {
		t.Errorf("execPath has wrong content: %q", got)
	}
}
