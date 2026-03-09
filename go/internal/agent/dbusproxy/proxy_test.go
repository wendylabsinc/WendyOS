package dbusproxy

import (
	"os"
	"path/filepath"
	"testing"
)

func TestIsAvailable(t *testing.T) {
	// Just verify it doesn't panic. The result depends on the host.
	_ = IsAvailable()
}

func TestSocketDir(t *testing.T) {
	dir := SocketDir("my-app")
	expected := "/run/wendy/dbus-proxy/my-app"
	if dir != expected {
		t.Errorf("SocketDir() = %q, want %q", dir, expected)
	}
}

func TestNewManager(t *testing.T) {
	m := NewManager(nil)
	if m == nil {
		t.Fatal("NewManager returned nil")
	}
	if m.processes == nil {
		t.Fatal("processes map not initialized")
	}
}

func TestStopNonExistent(t *testing.T) {
	m := NewManager(nil)
	err := m.Stop("does-not-exist")
	if err != nil {
		t.Errorf("Stop non-existent should return nil, got %v", err)
	}
}

func TestStopAll_Empty(t *testing.T) {
	m := NewManager(nil)
	// Should not panic with no processes.
	m.StopAll()
}

func TestSocketDirContainsAppID(t *testing.T) {
	tests := []struct {
		appID string
		want  string
	}{
		{"simple", filepath.Join(baseDir, "simple")},
		{"my-bt-app", filepath.Join(baseDir, "my-bt-app")},
		{"app_with_underscores", filepath.Join(baseDir, "app_with_underscores")},
	}

	for _, tt := range tests {
		t.Run(tt.appID, func(t *testing.T) {
			got := SocketDir(tt.appID)
			if got != tt.want {
				t.Errorf("SocketDir(%q) = %q, want %q", tt.appID, got, tt.want)
			}
		})
	}
}

func TestStartFailsWithoutProxy(t *testing.T) {
	if IsAvailable() {
		t.Skip("xdg-dbus-proxy is available, skipping negative test")
	}

	// Verify that Start returns an error when xdg-dbus-proxy isn't installed.
	// We need to ensure the socket dir doesn't leak.
	m := NewManager(nil)
	_, err := m.Start(t.Context(), "test-fail")
	if err == nil {
		t.Error("Start should fail when xdg-dbus-proxy is not available")
		// Clean up just in case.
		_ = m.Stop("test-fail")
	}

	// Socket dir should not exist after failure.
	socketDir := SocketDir("test-fail")
	if _, statErr := os.Stat(socketDir); statErr == nil {
		t.Errorf("socket dir %q should not exist after failed start", socketDir)
		os.RemoveAll(socketDir)
	}
}
