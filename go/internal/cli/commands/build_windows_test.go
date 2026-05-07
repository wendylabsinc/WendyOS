//go:build windows

package commands

import (
	"context"
	"strings"
	"testing"
)

// TestBuildProject_SwiftRejectedOnWindows verifies that `wendy build` for a
// Swift project fails fast on Windows with an actionable message instead of
// shelling out to a non-existent `swift` binary.
func TestBuildProject_SwiftRejectedOnWindows(t *testing.T) {
	err := buildProject(context.Background(), t.TempDir(), &BuildOption{Type: "swift"}, "test-app", "linux/arm64")
	if err == nil {
		t.Fatal("buildProject(swift) on Windows: error = nil, want non-nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "not supported") {
		t.Errorf("error message missing 'not supported': %q", msg)
	}
	if !strings.Contains(msg, "Dockerfile") {
		t.Errorf("error message should suggest a Dockerfile alternative: %q", msg)
	}
}
