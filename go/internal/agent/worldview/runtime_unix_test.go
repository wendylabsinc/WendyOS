//go:build linux || darwin

package worldview

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func alive(pid int) bool { return !errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) }

func TestCloseTerminatesWorkerProcessGroup(t *testing.T) {
	session, dir := startFake(t, "hold")
	raw, err := os.ReadFile(filepath.Join(dir, "pids"))
	if err != nil {
		t.Fatal(err)
	}
	var worker, grandchild int
	if _, err := fmt.Sscanf(string(raw), "%d %d", &worker, &grandchild); err != nil {
		t.Fatal(err)
	}
	if !alive(worker) || !alive(grandchild) {
		t.Fatal("fake worker processes are not running before Close")
	}
	closed := make(chan struct{})
	go func() { _ = session.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(10 * time.Second):
		t.Fatal("Close did not return")
	}
	deadline := time.Now().Add(5 * time.Second)
	for alive(worker) || alive(grandchild) {
		if time.Now().After(deadline) {
			t.Fatalf("processes survived Close: worker %v, grandchild %v", alive(worker), alive(grandchild))
		}
		time.Sleep(20 * time.Millisecond)
	}
}
