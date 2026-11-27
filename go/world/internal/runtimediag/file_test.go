package runtimediag

import (
	"context"
	"encoding/json"
	"github.com/wendylabsinc/wendy/go/world/internal/runtimeobs"
	"golang.org/x/sys/unix"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestAtomicSnapshotsAndFinalFlush(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metrics.json")
	m := runtimeobs.New()
	w, err := Open(path, m)
	if err != nil {
		t.Fatal(err)
	}
	initial, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var first struct {
		InstanceID string `json:"instance_id"`
	}
	if err = json.Unmarshal(initial, &first); err != nil || len(first.InstanceID) != 32 {
		t.Fatal("missing instance identity", err)
	}
	var wg sync.WaitGroup
	work := func() {
		defer wg.Done()
		for j := 0; j < 100; j++ {
			m.Increment(runtimeobs.HTTPFailures)
			if err := w.Flush(); err != nil {
				t.Error(err)
			}
		}
	}
	wg.Add(2)
	go work()
	go work()
	wg.Wait()
	m.Observe(runtimeobs.HTTPChallenge, 25*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err = w.Run(ctx); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		runtimeobs.Snapshot
		CapturedAtUnixMS int64  `json:"captured_at_unix_ms"`
		UptimeNS         int64  `json:"uptime_ns"`
		InstanceID       string `json:"instance_id"`
	}
	if err = json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if out.Schema != "wendy.world.runtimeobs.v2" || out.CapturedAtUnixMS <= 0 || out.UptimeNS < 0 || out.InstanceID != first.InstanceID {
		t.Fatal("missing diagnostic timing")
	}
	for _, c := range out.Counters {
		if c.Name == "http.failures" && c.Value != 200 {
			t.Fatal(c)
		}
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("unsafe snapshot permissions", err)
	}
	files, err := filepath.Glob(filepath.Join(filepath.Dir(path), ".world-metrics-*"))
	if err != nil || len(files) != 0 {
		t.Fatal("temporary output retained", files, err)
	}
	secondPath := filepath.Join(filepath.Dir(path), "second.json")
	if _, err = Open(secondPath, m); err != nil {
		t.Fatal(err)
	}
	second, err := os.ReadFile(secondPath)
	if err != nil {
		t.Fatal(err)
	}
	var other struct {
		InstanceID string `json:"instance_id"`
	}
	if err = json.Unmarshal(second, &other); err != nil || other.InstanceID == first.InstanceID {
		t.Fatal("different writer reused instance identity", err)
	}
}

func TestDiagnosticsNeverOverwriteExistingInputs(t *testing.T) {
	dir := t.TempDir()
	original := filepath.Join(dir, "original")
	if err := os.WriteFile(original, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(original, link); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(dir, "fifo")
	if err := unix.Mkfifo(fifo, 0600); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{original, link, fifo, dir, ""} {
		if _, err := Open(p, runtimeobs.New()); err == nil {
			t.Fatal("accepted existing or invalid path", p)
		}
	}
	b, err := os.ReadFile(original)
	if err != nil || string(b) != "keep" {
		t.Fatal("overwrote existing file", err)
	}
	if _, err := Open(filepath.Join(dir, "nil"), nil); err == nil {
		t.Fatal("nil collector enabled output")
	}
	var w *Writer
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
}
