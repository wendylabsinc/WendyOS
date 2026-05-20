//go:build windows

package commands

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/internal/shared/models"
)

// TestRunWithProvider_SwiftRejectedOnWindows verifies that `wendy run` for a
// Swift project fails fast on Windows with an actionable message instead of
// shelling out to a non-existent `swift` binary.
func TestRunWithProvider_SwiftRejectedOnWindows(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Package.swift"), []byte("// swift-tools-version:5.9\n"), 0o644); err != nil {
		t.Fatalf("creating Package.swift: %v", err)
	}

	err := runWithProvider(context.Background(), nil, models.ExternalDevice{}, dir, "", nil, runOptions{})
	if err == nil {
		t.Fatal("runWithProvider(swift) on Windows: error = nil, want non-nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "not supported") {
		t.Errorf("error message missing 'not supported': %q", msg)
	}
	if !strings.Contains(msg, "Dockerfile") {
		t.Errorf("error message should suggest a Dockerfile alternative: %q", msg)
	}
}
