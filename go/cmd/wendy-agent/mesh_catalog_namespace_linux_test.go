//go:build linux

package main

import (
	"os"
	"path/filepath"
	"testing"

	"go.uber.org/zap"
)

func TestCatalogAppPinsNamespaceUntilStop(t *testing.T) {
	manager := newMeshCatalogManager(t.TempDir(), nil, zap.NewNop())
	path := filepath.Join(t.TempDir(), "task-namespace")
	if err := os.Symlink("/proc/self/ns/net", path); err != nil {
		t.Fatal(err)
	}
	want, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.StartMeshApp("app", "app", "10.42.2.2", "wendy0", path, "10.42.2.1", nil); err != nil {
		t.Fatal(err)
	}
	app := manager.apps["app"]
	t.Cleanup(func() { manager.StopMeshApp("app") })
	// Model a task PID disappearing and being reused by another namespace.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/proc/self/ns/mnt", path); err != nil {
		t.Fatal(err)
	}
	got, err := os.Stat(app.netnsPath)
	if err != nil || !os.SameFile(want, got) {
		t.Fatalf("retained namespace changed: %v", err)
	}
	// A live bridge owns a separate descriptor while an app stop or
	// catalogue deactivation releases the registration's descriptor.
	borrowed, err := os.Open(app.netnsPath)
	if err != nil {
		t.Fatal(err)
	}
	defer borrowed.Close()
	manager.StopMeshApp("app")
	if _, err := os.Stat(app.netnsPath); !os.IsNotExist(err) {
		t.Fatalf("namespace descriptor survived stop: %v", err)
	}
	if got, err := borrowed.Stat(); err != nil || !os.SameFile(want, got) {
		t.Fatalf("bridge lost its namespace during stop: %v", err)
	}
	if err := manager.StartMeshApp("app", "app", "10.42.2.2", "wendy0", "/missing-task-namespace", "10.42.2.1", nil); err == nil {
		t.Fatal("missing task namespace accepted")
	}
}
